// Package mcp is a thin MCP client for remote servers over Streamable HTTP,
// tools only (docs/connectors.md §5, §7, §12). It knows nothing about
// OAuth, config or the daemon: the caller supplies the bearer token and
// decides what a 401 means.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// CallTimeout bounds every request end to end: waiting for a slot, a
	// reconnect inside it and the retry.
	CallTimeout = 60 * time.Second
	// MaxInFlight is the number of requests one Client runs at once. The
	// SDK's own long-lived streams (legacy GET, modern subscriptions/listen)
	// are not counted.
	MaxInFlight = 4

	// protocolVersion is offered first. The SDK tries the stateless
	// server/discover and falls back to initialize with 2025-11-25 (which
	// may negotiate older) when the server does not speak it.
	protocolVersion = "2026-07-28"

	clientName    = "holaclaw"
	clientVersion = "1"

	// ListTools bounds. A tool over a per-tool cap is skipped; a listing
	// over MaxListBytes (all tools seen, skipped ones included) or
	// maxPages is an error.
	maxPages       = 20
	maxTools       = 1000
	MaxListBytes   = 4 << 20
	maxNameBytes   = 128
	maxDescBytes   = 16 << 10
	maxSchemaBytes = 64 << 10
)

// Options configure Dial.
type Options struct {
	URL string
	// Token returns the bearer token, called once per HTTP request, with a
	// deadline. An empty token sends no Authorization header. Nil means none.
	Token func(ctx context.Context) (string, error)
	// HTTPClient is copied, never modified. Nil means a zero http.Client.
	HTTPClient *http.Client
	// OnToolsChanged follows notifications/tools/list_changed (legacy GET
	// stream or modern subscriptions/listen). Calls are coalesced, made from
	// one goroutine, and never after Close returns. It must not call Close.
	OnToolsChanged func()
	// Insecure allows plain http to any host and an https certificate that
	// does not verify: a connector's allow_insecure_transport.
	Insecure bool
}

// ServerInfo is the server's self-description from the handshake.
type ServerInfo struct {
	Name    string
	Version string
}

// Tool is one tool definition as the server declared it. The hints are
// data only: DestructiveHint nil means unset (the spec defaults it to true).
type Tool struct {
	Name            string          `json:"name"`
	Description     string          `json:"description,omitempty"`
	InputSchema     json.RawMessage `json:"input_schema"`
	ReadOnlyHint    bool            `json:"read_only_hint,omitempty"`
	DestructiveHint *bool           `json:"destructive_hint,omitempty"`
}

// Result is a tools/call result flattened to text.
type Result struct {
	Text    string
	IsError bool
}

// Client is one MCP connection, replaced when the server loses it. Safe for
// concurrent use.
type Client struct {
	opts       Options
	sdkClient  *sdk.Client
	httpClient *http.Client
	transport  *transport    // httpClient.Transport
	slots      chan struct{} // MaxInFlight places
	timeout    time.Duration // CallTimeout; tests shorten it

	closeCtx    context.Context // cancelled by Close; interrupts every call
	closeCancel context.CancelFunc
	changed     chan struct{} // coalesced tools/list_changed
	workerDone  chan struct{}

	mu         sync.Mutex
	live       *conn
	connecting chan struct{} // closed when the running handshake ends
	active     int           // calls inside call()
	closed     bool
}

// Dial connects (server/discover, or initialize on a legacy server) and
// returns a Client. Plain http is refused unless the host is loopback or
// o.Insecure.
func Dial(ctx context.Context, o Options) (*Client, error) {
	u, err := url.Parse(o.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && (u.Scheme != "http" || !o.Insecure && !isLoopback(u.Hostname()))) {
		return nil, errors.New("mcp: url must be https, or http on a loopback host")
	}
	c := &Client{
		opts:    o,
		slots:   make(chan struct{}, MaxInFlight),
		timeout: CallTimeout,
		changed: make(chan struct{}, 1),
	}
	c.httpClient = newHTTPClient(o.HTTPClient, u, o.Token, o.Insecure)
	c.transport = c.httpClient.Transport.(*transport)
	c.closeCtx, c.closeCancel = context.WithCancel(context.Background())
	copts := &sdk.ClientOptions{Capabilities: &sdk.ClientCapabilities{}}
	if o.OnToolsChanged != nil {
		// Also what makes the SDK open subscriptions/listen on a modern server.
		copts.ToolListChangedHandler = c.toolsChanged
	}
	c.sdkClient = sdk.NewClient(&sdk.Implementation{Name: clientName, Version: clientVersion}, copts)
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cn, err := c.connect(ctx)
	if err != nil {
		c.closeCancel()
		return nil, err
	}
	c.live = cn
	if o.OnToolsChanged != nil {
		c.workerDone = make(chan struct{})
		go c.notifyLoop()
	}
	return c, nil
}

// SessionID is the current Mcp-Session-Id. It is empty on a modern
// (stateless) server, while reconnecting, and after Close.
func (c *Client) SessionID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live == nil || c.live.modern {
		return ""
	}
	return c.live.cs.ID()
}

// ServerInfo is what the current connection's handshake returned.
func (c *Client) ServerInfo() ServerInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live == nil || c.live.cs.InitializeResult() == nil || c.live.cs.InitializeResult().ServerInfo == nil {
		return ServerInfo{}
	}
	si := c.live.cs.InitializeResult().ServerInfo
	return ServerInfo{Name: si.Name, Version: si.Version}
}

// ListTools returns every tool, following pagination. If the connection
// changes between pages the listing starts over, once. Tools over the
// per-tool caps are skipped.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	c.retireOverCached()
	var (
		out      []Tool
		cursor   string
		on       *conn
		budget   = MaxListBytes
		restarts int
	)
	for page := 0; ; page++ {
		if page == maxPages {
			return nil, fmt.Errorf("mcp: tools/list: more than %d pages", maxPages)
		}
		var (
			res  *sdk.ListToolsResult
			used *conn
		)
		err := c.call(ctx, "tools/list", func(ctx context.Context, cn *conn) error {
			used = cn
			var err error
			res, err = cn.cs.ListTools(ctx, &sdk.ListToolsParams{Cursor: cursor})
			if err == nil && stateFrom(ctx).get().sent {
				// Fetched, not a cache hit: the SDK cached it on a modern conn.
				if b, merr := json.Marshal(res); merr == nil {
					cn.noteCachedPage(len(b))
				}
			}
			return err
		})
		if err != nil {
			return nil, err
		}
		if on != nil && used != on {
			if restarts++; restarts > 1 {
				return nil, fmt.Errorf("mcp: tools/list: %w", ErrSessionLost)
			}
			out, cursor, on, budget, page = nil, "", nil, MaxListBytes, -1
			continue
		}
		on = used
		for _, t := range res.Tools {
			tl := convertTool(t)
			n := len(tl.Name) + len(tl.Description) + len(tl.InputSchema)
			if budget -= n; budget < 0 {
				return nil, fmt.Errorf("%w: over %d bytes", ErrListTooLarge, MaxListBytes)
			}
			if len(tl.Name) > maxNameBytes || len(tl.Description) > maxDescBytes || len(tl.InputSchema) > maxSchemaBytes {
				continue
			}
			if len(out) == maxTools {
				return nil, fmt.Errorf("mcp: tools/list: more than %d tools", maxTools)
			}
			out = append(out, tl)
		}
		if res.NextCursor == "" {
			return out, nil
		}
		cursor = res.NextCursor
	}
}

// CallTool runs tools/call. Args is a JSON object; nil sends {}. A server
// isError is Result.IsError, not an error.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (Result, error) {
	p := &sdk.CallToolParams{Name: name}
	if len(args) > 0 {
		if !json.Valid(args) {
			return Result{}, errors.New("mcp: tools/call: args are not valid JSON")
		}
		p.Arguments = args
	}
	var res *sdk.CallToolResult
	err := c.call(ctx, "tools/call", func(ctx context.Context, cn *conn) error {
		var err error
		res, err = cn.cs.CallTool(ctx, p)
		return err
	})
	if err != nil {
		return Result{}, err
	}
	return flatten(res), nil
}

// Ping checks the server answers: ping on a legacy server, server/discover
// on a modern one (2026-07-28 removed ping). On a modern server with
// OnToolsChanged it reconnects instead: a dropped or refused
// subscriptions/listen is invisible otherwise, so the caller's health
// check bounds how long list_changed can go missing. The reconnect also
// empties the SDK's tools/list cache.
func (c *Client) Ping(ctx context.Context) error {
	c.mu.Lock()
	current := c.live
	c.mu.Unlock()
	if current != nil && current.modern && c.opts.OnToolsChanged != nil {
		c.drop(current)
		return c.call(ctx, "ping", func(context.Context, *conn) error { return nil })
	}
	return c.call(ctx, "ping", func(ctx context.Context, cn *conn) error {
		if cn.modern {
			_, err := sdk.CallCustomMethod[*sdk.DiscoverParams, *sdk.DiscoverResult](ctx, cn.cs, "server/discover", &sdk.DiscoverParams{})
			return err
		}
		return cn.cs.Ping(ctx, nil)
	})
}

// Close interrupts every call (they return ErrClosed), stops the
// OnToolsChanged goroutine and ends the connection (DELETE when the server
// gave a session id). When calls were in flight the DELETE is skipped: the
// server would hold it until their handlers end, stalling Close and them.
// A modern connection is closed in the background: the SDK's best-effort
// cancel of subscriptions/listen can take up to 5 s.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	cn, busy := c.live, c.active > 0
	c.live, c.closed = nil, true
	c.mu.Unlock()
	if busy {
		c.transport.noDelete.Store(true)
	}
	c.closeCancel()
	if c.workerDone != nil {
		<-c.workerDone
	}
	if cn == nil {
		return nil
	}
	if cn.modern {
		go func() { _ = cn.cs.Close() }()
		return nil
	}
	if err := cn.cs.Close(); err != nil && !busy && !errors.Is(err, sdk.ErrConnectionClosed) {
		return fmt.Errorf("mcp: close: %w", err)
	}
	return nil
}
