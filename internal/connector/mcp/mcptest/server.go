// Package mcptest is a fake MCP server over Streamable HTTP for tests,
// built on the SDK's server side and httptest. It records what it saw and
// can lose sessions, require a bearer token and change its tool list.
package mcptest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Handler runs one tools/call. Args is the raw arguments object.
type Handler func(ctx context.Context, args json.RawMessage) (*sdk.CallToolResult, error)

// Tool is one tool the server offers.
type Tool struct {
	Name        string
	Description string
	// InputSchema defaults to {"type":"object"}.
	InputSchema json.RawMessage
	Annotations *sdk.ToolAnnotations
	Handler     Handler
}

// Options configure New.
type Options struct {
	Tools []Tool
	// Token, when set, is required as "Authorization: Bearer <Token>"; a
	// missing or wrong one gets 401 with a WWW-Authenticate header. Empty
	// means open.
	Token string
	// PageSize for tools/list; 0 is the SDK default.
	PageSize int
	// Name and Version are the serverInfo; defaults "mcptest" and "1.0.0".
	Name, Version string
	// Wrap, when set, wraps the whole server (outside the token check and
	// the recording), e.g. to serve /.well-known/... or inject statuses.
	Wrap func(http.Handler) http.Handler
	// Legacy runs the SDK's stateful handler pinned to 2025-11-25 and
	// older: it rejects server/discover, so clients fall back to
	// initialize, Mcp-Session-Id and the standalone GET stream. The session
	// controls (ExpireSessions, LoseSessionOn) only act on a Legacy server.
	// The default is the SDK's stateless handler, which speaks 2026-07-28
	// (server/discover, no session ids, subscriptions/listen).
	Legacy bool
}

// LegacyVersion is the newest protocol a Legacy server speaks.
const LegacyVersion = "2025-11-25"

// Request is one HTTP request as the server saw it.
type Request struct {
	HTTPMethod      string // POST, GET, DELETE
	Path            string
	RPCMethod       string // JSON-RPC method of a POST, "" for responses
	SessionID       string // Mcp-Session-Id header
	Authorization   string // Authorization header
	ProtocolVersion string // Mcp-Protocol-Version header
}

// Server is a running fake. Close it with t.Cleanup (New does).
type Server struct {
	*httptest.Server
	srv   *sdk.Server
	stop  context.Context // cancelled by Close; cancels every handler
	halt  context.CancelFunc
	token string

	mu          sync.Mutex
	requests    []Request
	initializes int
	inFlight    int
	maxInFlight int
	dead        map[string]bool
	loseOn      map[string]int
	tools       []string
}

// ResourceMetadataPath is advertised in the 401 WWW-Authenticate header.
const ResourceMetadataPath = "/.well-known/oauth-protected-resource"

// New starts a server and registers its Close with t.Cleanup.
func New(t testing.TB, o Options) *Server {
	t.Helper()
	name, version := o.Name, o.Version
	if name == "" {
		name = "mcptest"
	}
	if version == "" {
		version = "1.0.0"
	}
	s := &Server{token: o.Token, dead: map[string]bool{}, loseOn: map[string]int{}}
	s.stop, s.halt = context.WithCancel(context.Background())
	so := &sdk.ServerOptions{
		PageSize:     o.PageSize,
		Capabilities: &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{ListChanged: true}},
	}
	if o.Legacy {
		for _, v := range sdk.SupportedProtocolVersions() {
			if v <= LegacyVersion {
				so.SupportedProtocolVersions = append(so.SupportedProtocolVersions, v)
			}
		}
	}
	s.srv = sdk.NewServer(&sdk.Implementation{Name: name, Version: version}, so)
	s.SetTools(o.Tools...)
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s.srv },
		&sdk.StreamableHTTPOptions{Stateless: !o.Legacy})
	root := s.middleware(h)
	if o.Wrap != nil {
		root = o.Wrap(root)
	}
	s.Server = httptest.NewServer(root)
	t.Cleanup(s.Close)
	return s
}

// Close drops open streams and stops the server.
func (s *Server) Close() {
	s.halt()
	s.CloseClientConnections()
	s.Server.Close()
}

// SetTools replaces the tool list; connected clients get
// notifications/tools/list_changed.
func (s *Server) SetTools(tools ...Tool) {
	s.mu.Lock()
	old := s.tools
	s.tools = nil
	for _, t := range tools {
		s.tools = append(s.tools, t.Name)
	}
	s.mu.Unlock()
	if len(old) > 0 {
		s.srv.RemoveTools(old...)
	}
	for _, t := range tools {
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		h := t.Handler
		s.srv.AddTool(&sdk.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
			Annotations: t.Annotations,
		}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			defer context.AfterFunc(s.stop, cancel)()
			s.enter()
			defer s.leave()
			if h == nil {
				return Text(""), nil
			}
			return h(ctx, req.Params.Arguments)
		})
	}
}

// ExpireSessions makes every session seen so far unknown: its next request
// gets 404.
func (s *Server) ExpireSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.requests {
		if r.SessionID != "" {
			s.dead[r.SessionID] = true
		}
	}
}

// LoseSessionOn answers the next n requests for rpcMethod (e.g.
// "tools/call") with 404 and forgets their session: every later request
// with that session id, the client's GET stream and DELETE included, also
// gets 404.
func (s *Server) LoseSessionOn(rpcMethod string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loseOn[rpcMethod] = n
}

// SetToken changes the required bearer token; "" makes the server open.
func (s *Server) SetToken(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = tok
}

// Requests returns every request seen, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Initializes counts initialize requests that reached the SDK server.
func (s *Server) Initializes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initializes
}

// MaxConcurrentCalls is the most tool handlers seen running at once.
func (s *Server) MaxConcurrentCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxInFlight
}

func (s *Server) enter() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight++
	s.maxInFlight = max(s.maxInFlight, s.inFlight)
}

func (s *Server) leave() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight--
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := Request{
			HTTPMethod:      r.Method,
			Path:            r.URL.Path,
			SessionID:       r.Header.Get("Mcp-Session-Id"),
			Authorization:   r.Header.Get("Authorization"),
			ProtocolVersion: r.Header.Get("Mcp-Protocol-Version"),
		}
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
			if err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			var m struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &m)
			rec.RPCMethod = m.Method
			r.Body = io.NopCloser(bytes.NewReader(body))
		}

		s.mu.Lock()
		s.requests = append(s.requests, rec)
		token := s.token
		authorized := token == "" || rec.Authorization == "Bearer "+token
		lose := false
		if authorized && rec.SessionID != "" {
			if s.dead[rec.SessionID] {
				lose = true
			} else if n := s.loseOn[rec.RPCMethod]; n > 0 && rec.RPCMethod != "" {
				s.loseOn[rec.RPCMethod] = n - 1
				s.dead[rec.SessionID] = true
				lose = true
			}
		}
		if authorized && !lose && rec.RPCMethod == "initialize" {
			s.initializes++
		}
		s.mu.Unlock()

		switch {
		case !authorized:
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+s.URL+ResourceMetadataPath+`"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		case lose:
			http.Error(w, "session not found", http.StatusNotFound)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

// Text is a result with one text part.
func Text(s string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: s}}}
}

// Error is an isError result with one text part.
func Error(s string) *sdk.CallToolResult {
	r := Text(s)
	r.IsError = true
	return r
}

// TextTool returns a tool that always answers text.
func TextTool(name, text string) Tool {
	return Tool{Name: name, Description: "returns " + strings.ToLower(name), Handler: func(context.Context, json.RawMessage) (*sdk.CallToolResult, error) {
		return Text(text), nil
	}}
}

// BlockingTool returns a tool that answers "done" once release is closed,
// or fails when the call is cancelled. A nil release blocks until then:
// use it as the slow tool for timeout tests.
func BlockingTool(name string, release <-chan struct{}) Tool {
	return Tool{Name: name, Description: "blocks", Handler: func(ctx context.Context, _ json.RawMessage) (*sdk.CallToolResult, error) {
		select {
		case <-release:
			return Text("done"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
}
