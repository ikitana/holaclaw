package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/agent"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/approval"
	"holaclaw/internal/auth"
	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
	"holaclaw/internal/event"
	"holaclaw/internal/session"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
)

// turnFunc is a fake session.Turner.
type turnFunc func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error)

func (f turnFunc) Turn(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
	return f(ctx, in)
}

// logSink captures every log line so a test can prove a secret never reached
// one.
type logSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// harness is the whole API on real services: a real SQLite store, the real
// auth, approval and session layers, and an echoing turner.
type harness struct {
	t    testing.TB
	st   *store.SQLite
	auth *auth.Service
	appr *approval.Service
	hub  *Hub
	mgr  *session.Manager
	b    *Backend
	h    http.Handler
	srv  *httptest.Server
	logs *logSink
	sink event.Sink

	owner store.Identity
	other store.Identity

	admin store.Identity

	ownerToken string // client scope, on a binding of the owner
	otherToken string // client scope, on a binding of the second user
	adminToken string // admin scope, on a binding of an admin
	ownerAdmin string // admin scope, on a binding of the owner, as `holaclaw init` mints it
	addrs      map[string]net.Addr
}

func newHarness(t testing.TB) *harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	logs := &logSink{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	h := &harness{t: t, st: st, logs: logs}
	h.hub = NewHub(st, logger)
	h.sink = event.Multi(st, h.hub)
	h.auth = auth.New(st)
	h.auth.Events = h.sink
	h.appr = approval.New(st.DB(), h.sink)

	h.mgr = session.New(session.Deps{
		Store:    st,
		Events:   h.sink,
		Debounce: -1,
		Logger:   logger,
		Prepare: func(context.Context, store.Session, []event.Message) (agent.TurnInput, error) {
			return agent.TurnInput{}, nil
		},
		Runner: func(store.Session) (session.Turner, error) { return turnFunc(h.echo), nil },
	})
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.mgr.Shutdown(stop)
	})

	if _, err = h.auth.CreateOwner(ctx, true, nil); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h.owner, err = h.auth.Owner(ctx)
	if err != nil {
		t.Fatalf("ensure owner: %v", err)
	}
	if h.other, err = h.auth.CreateIdentity(ctx, "nadia", store.RoleUser, h.owner.ID); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	if h.admin, err = h.auth.CreateIdentity(ctx, "ada", store.RoleAdmin, h.owner.ID); err != nil {
		t.Fatalf("create identity: %v", err)
	}

	h.b = &Backend{
		Config:    testConfig(),
		Store:     st,
		Auth:      h.auth,
		Approvals: h.appr,
		Sessions:  h.mgr,
		Hub:       h.hub,
		Secrets:   openSecrets(t, dir),
		Version:   "test",
		Started:   time.Now().Add(-time.Minute),
		Logger:    logger,
	}
	h.b.Router = &session.Router{
		Store:    st,
		Manager:  h.mgr,
		Events:   h.sink,
		Resolve:  h.resolve,
		AgentFor: func(string) string { return "main" },
	}
	h.h = NewHandler(h.b)
	h.srv = httptest.NewServer(h.h)
	t.Cleanup(h.srv.Close)

	h.ownerToken = h.token("cli-owner", auth.ScopeClient, h.owner.ID)
	h.otherToken = h.token("cli-other", auth.ScopeClient, h.other.ID)
	h.adminToken = h.token("admin", auth.ScopeAdmin, h.admin.ID)
	h.ownerAdmin = h.token("init", auth.ScopeAdmin, h.owner.ID)
	return h
}

// resolve is what the daemon installs: holaclaw clients arrive from their
// binding, linking nothing.
func (h *harness) resolve(ctx context.Context, origin event.Origin, sender event.Sender) (store.Resolution, error) {
	channel := origin.Channel
	if channel == event.ChannelHolaclaw {
		i, err := h.resolveAlias(ctx, channel, sender.Aliases)
		return store.Resolution{Identity: i}, err
	}
	return h.st.ResolveAndLink(ctx, channel, sender.Aliases, sender.DisplayName, nil)
}

// echo is the fake turn: it stores an assistant message and emits the reply
// event the API streams, the way the real loop does.
func (h *harness) echo(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
	text := "echo"
	for i := len(in.History) - 1; i >= 0; i-- {
		if in.History[i].Role == event.RoleUser {
			text = "echo: " + in.History[i].Text
			break
		}
	}
	msg := event.Message{
		Schema: event.SchemaVersion, ID: event.NewID(), SessionID: in.SessionID, TraceID: in.TraceID,
		Role: event.RoleAssistant, Text: text, CreatedAt: time.Now().UTC(),
	}
	if _, err := h.st.AppendMessage(ctx, msg, false); err != nil {
		return agent.TurnResult{}, err
	}
	ev := event.New(event.Reply, in.TraceID, in.SessionID, "", map[string]any{"text": text})
	ev.MessageID = msg.ID
	h.sink.Emit(ctx, ev)
	return agent.TurnResult{Reply: msg, New: []event.Message{msg}}, nil
}

func (h *harness) token(name string, scope auth.Scope, user event.ID) string {
	h.t.Helper()
	secret, _, err := h.auth.CreateToken(context.Background(), name, scope, user)
	if err != nil {
		h.t.Fatalf("create token: %v", err)
	}
	return secret
}

func testConfig() *config.Config {
	return &config.Config{
		Agents: map[string]config.Agent{"main": {
			Name:   "main",
			Tools:  []string{"memory"},
			Models: config.Roles{Chat: "gemma"},
		}},
		Models: map[string]config.Model{"gemma": {
			Name: "gemma", Provider: "ollama", ID: "gemma4:e2b", Context: 8192, Tools: true,
		}},
		Providers: map[string]config.Provider{"ollama": {Name: "ollama", Type: "openai-compatible"}},
	}
}

func openSecrets(t testing.TB, dir string) *secrets.File {
	t.Helper()
	f, err := secrets.Open(filepath.Join(dir, "auth.toml"))
	if err != nil {
		t.Fatalf("open secrets: %v", err)
	}
	return f
}

// --- request helpers ----------------------------------------------------------

// do sends a request with a bearer token through the httptest server.
func (h *harness) do(method, path, token string, body any) *http.Response {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encode body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+BaseURL+path, rdr)
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("do: %v", err)
	}
	return resp
}

// socket runs one request in process, marked as arriving on the unix socket.
func (h *harness) socket(method, path string, body any) *httptest.ResponseRecorder {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("encode body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, BaseURL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req = req.WithContext(markSocket(req.Context()))
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	return rec
}

// decode reads a JSON response and asserts its status.
func decode[T any](t testing.TB, resp *http.Response, want int) T {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, want, body)
	}
	var out T
	if len(body) > 0 {
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
	}
	return out
}

func errorBody(t testing.TB, resp *http.Response, wantStatus int, wantCode string) {
	t.Helper()
	e := decode[gen.Error](t, resp, wantStatus)
	if e.Code != wantCode {
		t.Fatalf("code = %q, want %q (message %q)", e.Code, wantCode, e.Message)
	}
}

// newSession creates a session for a token and returns it.
func (h *harness) newSession(token string, in gen.SessionCreate) gen.Session {
	h.t.Helper()
	return decode[gen.Session](h.t, h.do(http.MethodPost, "/sessions", token, in), http.StatusCreated)
}

// --- SSE helpers --------------------------------------------------------------

// frame is one server-sent event as it came off the wire.
type frame struct {
	ID      string
	Type    string
	Data    []byte
	Comment string
}

// rawStream reads frames off an SSE response without interpreting them, so a
// test can assert on the exact payload the server produced. Exactly one
// goroutine reads the connection and hands frames over a channel: a test that
// times out must not leave a reader behind that swallows the next frame.
type rawStream struct {
	resp   *http.Response
	r      *bufio.Reader
	frames chan frame
}

func (h *harness) stream(token, focus, lastID string) *rawStream {
	h.t.Helper()
	url := h.srv.URL + BaseURL + "/events"
	if focus != "" {
		url += "?focus=" + focus
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("stream status = %d", resp.StatusCode)
	}
	s := &rawStream{resp: resp, r: bufio.NewReader(resp.Body), frames: make(chan frame, 64)}
	go s.pump()
	h.t.Cleanup(s.close)
	return s
}

func (s *rawStream) pump() {
	defer close(s.frames)
	for {
		f, err := s.readBlocking()
		if err != nil {
			return
		}
		s.frames <- f
	}
}

func (s *rawStream) close() { _ = s.resp.Body.Close() }

// next reads the next frame, failing the test on timeout.
func (s *rawStream) next(t testing.TB, timeout time.Duration) frame {
	t.Helper()
	f, err := s.read(timeout)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return f
}

// quiet asserts that nothing else arrives within the window.
func (s *rawStream) quiet(t testing.TB, window time.Duration) {
	t.Helper()
	for {
		f, err := s.read(window)
		if err != nil {
			return // a timeout is the expected outcome
		}
		if f.Comment != "" {
			continue // heartbeats do not count as traffic
		}
		t.Fatalf("unexpected event %s %s", f.ID, f.Type)
	}
}

func (s *rawStream) read(timeout time.Duration) (frame, error) {
	select {
	case f, ok := <-s.frames:
		if !ok {
			return frame{}, io.EOF
		}
		return f, nil
	case <-time.After(timeout):
		return frame{}, context.DeadlineExceeded
	}
}

func (s *rawStream) readBlocking() (frame, error) {
	var f frame
	for {
		line, err := s.r.ReadString('\n')
		if err != nil {
			return frame{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if f.Comment != "" || len(f.Data) > 0 {
				return f, nil
			}
		case strings.HasPrefix(line, ":"):
			f.Comment = strings.TrimSpace(strings.TrimPrefix(line, ":"))
			return f, nil
		case strings.HasPrefix(line, "id: "):
			f.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			f.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			f.Data = append(f.Data, strings.TrimPrefix(line, "data: ")...)
		}
	}
}

// emit stores and fans out one event for a session, returning its id.
func (h *harness) emit(sessionID event.ID, typ event.Type) event.ID {
	h.t.Helper()
	e := event.New(typ, event.NewID(), sessionID, "", map[string]any{"n": 1})
	h.sink.Emit(context.Background(), e)
	return e.ID
}

// resolveAlias is the identity holding one of the aliases, or
// store.ErrNotFound; it links nothing.
func (h *harness) resolveAlias(ctx context.Context, channel string, aliases []string) (store.Identity, error) {
	r, err := store.ResolveAliases(ctx, h.st.DB(), channel, aliases, nil)
	return r.Identity, err
}

// ask raises a tool approval the way a suspended tool call does, and returns it
// once it is pending. The call gives up when the test ends.
func (h *harness) ask(a tool.Approval) tool.Approval {
	h.t.Helper()
	a.ID = event.NewID()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = h.appr.Request(ctx, a)
	}()
	h.t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := h.appr.Get(context.Background(), a.ID)
		if err == nil {
			return got
		}
		if !errors.Is(err, approval.ErrNotFound) || time.Now().After(deadline) {
			h.t.Fatalf("approval %s never pending: %v", a.ID, err)
		}
		time.Sleep(time.Millisecond)
	}
}
