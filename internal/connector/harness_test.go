package connector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
	"holaclaw/internal/connector/mcp/mcptest"
	"holaclaw/internal/connector/oauth"
	"holaclaw/internal/connector/oauth/oauthtest"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

const (
	pinned  = "http://127.0.0.1:26502/callback"
	other   = "http://127.0.0.1:9911/cb"
	other2  = "http://127.0.0.1:9912/cb"
	fakeHit = "FAKE" // every token, code and secret of the fakes starts with it
)

// guard sits in front of the MCP server: it accepts the bearer tokens the
// fake authorization server issued, or everything when open, and answers
// 401 naming the https resource metadata (mcptest's own names http).
type guard struct {
	mu     sync.Mutex
	open   bool
	down   bool
	ok     map[string]bool
	header string
	// hold, when set, blocks every request until it is closed; each
	// blocked request is signalled on blocked.
	hold    chan struct{}
	blocked chan struct{}
}

func (g *guard) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		down := g.down
		allowed := g.open || g.ok[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		hdr := g.header
		hold, blocked := g.hold, g.blocked
		g.mu.Unlock()
		if hold != nil {
			select {
			case blocked <- struct{}{}:
			default:
			}
			<-hold
		}
		switch {
		case down:
			http.Error(w, "down", http.StatusInternalServerError)
		case !allowed:
			w.Header().Set("WWW-Authenticate", hdr)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (g *guard) set(f func(g *guard)) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f(g)
}

// block holds every request from now on; release lets them all through.
func (g *guard) block() (blocked <-chan struct{}, release func()) {
	hold, b := make(chan struct{}), make(chan struct{}, 1)
	g.set(func(g *guard) { g.hold, g.blocked = hold, b })
	var once sync.Once
	return b, func() {
		once.Do(func() {
			g.set(func(g *guard) { g.hold, g.blocked = nil, nil })
			close(hold)
		})
	}
}

// flakyStore fails PutConnector for the rows fail matches, and runs
// onDelete before each DeleteConnector.
type flakyStore struct {
	store.ConnectorStore
	mu       sync.Mutex
	fail     func(store.Connector) bool
	onDelete func(id string)
	delErr   error // DeleteConnector fails with it when set
	getErr   error // GetConnector fails with it when set
}

func (f *flakyStore) GetConnector(ctx context.Context, id string) (store.Connector, error) {
	f.mu.Lock()
	getErr := f.getErr
	f.mu.Unlock()
	if getErr != nil {
		return store.Connector{}, getErr
	}
	return f.ConnectorStore.GetConnector(ctx, id)
}

func (f *flakyStore) failGets(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr = err
}

func (f *flakyStore) DeleteConnector(ctx context.Context, id string) error {
	f.mu.Lock()
	hook, delErr := f.onDelete, f.delErr
	f.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	if delErr != nil {
		return delErr
	}
	return f.ConnectorStore.DeleteConnector(ctx, id)
}

func (f *flakyStore) failDeletes(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delErr = err
}

func (f *flakyStore) beforeDelete(fn func(id string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onDelete = fn
}

func (f *flakyStore) PutConnector(ctx context.Context, c store.Connector) error {
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if fail != nil && fail(c) {
		return errors.New("store: disk full")
	}
	return f.ConnectorStore.PutConnector(ctx, c)
}

func (f *flakyStore) failWhen(fn func(store.Connector) bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fn
}

// issuer is the fake's token endpoint: FAKEaccessN / FAKErefreshN, each
// access token accepted by the guard.
type issuer struct {
	mu        sync.Mutex
	g         *guard
	n         int
	expiresIn int
	fail      string // an OAuth error code to answer instead
	grants    []url.Values
}

func (is *issuer) serve(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	is.mu.Lock()
	defer is.mu.Unlock()
	is.grants = append(is.grants, r.PostForm)
	if is.fail != "" {
		oauthtest.JSONHandler(http.StatusBadRequest, `{"error":"`+is.fail+`","error_description":"SERVERTEXT"}`)(w, r)
		return
	}
	is.n++
	access := fmt.Sprintf("FAKEaccess%d", is.n)
	is.g.set(func(g *guard) { g.ok[access] = true })
	oauthtest.WriteJSON(w, map[string]any{
		"access_token": access, "token_type": "Bearer", "refresh_token": fmt.Sprintf("FAKErefresh%d", is.n),
		"expires_in": is.expiresIn, "scope": "repo",
	})
}

func (is *issuer) count(grantType string) int {
	is.mu.Lock()
	defer is.mu.Unlock()
	n := 0
	for _, g := range is.grants {
		if g.Get("grant_type") == grantType {
			n++
		}
	}
	return n
}

func (is *issuer) last() url.Values {
	is.mu.Lock()
	defer is.mu.Unlock()
	return is.grants[len(is.grants)-1]
}

func (is *issuer) setFail(code string) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.fail = code
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type sink struct {
	mu     sync.Mutex
	events []event.Event
	onEmit func(event.Event) // runs after each event is recorded
}

func (s *sink) Emit(_ context.Context, e event.Event) {
	s.mu.Lock()
	s.events = append(s.events, e)
	hook := s.onEmit
	s.mu.Unlock()
	if hook != nil {
		hook(e)
	}
}

func (s *sink) hook(fn func(event.Event)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onEmit = fn
}

// statuses is "state/detail" of every connector.status of id.
func (s *sink) statuses(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.events {
		if e.Type != event.ConnectorStatus {
			continue
		}
		var a struct{ ID, State, Detail string }
		_ = json.Unmarshal(e.Attrs, &a)
		if a.ID == id {
			out = append(out, a.State+"/"+a.Detail)
		}
	}
	return out
}

func (s *sink) last(id string) string {
	st := s.statuses(id)
	if len(st) == 0 {
		return ""
	}
	return st[len(st)-1]
}

// all is every event marshalled, for secret checks.
func (s *sink) all() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.Marshal(s.events)
	return string(b)
}

type harness struct {
	t      *testing.T
	ctx    context.Context
	as     *oauthtest.Server
	srv    *mcptest.Server
	g      *guard
	is     *issuer
	dir    string
	st     *store.SQLite
	fs     *flakyStore // st as the manager sees it
	sec    *secrets.File
	clock  *clock
	events *sink
	m      *Manager

	regMu sync.Mutex
	regs  int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, ctx: context.Background(), dir: t.TempDir(), events: &sink{}, clock: &clock{t: time.Now()}}
	h.as = oauthtest.New(t)
	h.g = &guard{ok: map[string]bool{}, header: h.as.Header()}
	h.srv = mcptest.New(t, mcptest.Options{
		Name: "fake", Version: "2.0",
		Tools: []mcptest.Tool{mcptest.TextTool("echo", "hello")},
		Wrap:  h.g.wrap,
	})
	h.as.PRM["resource"] = h.srv.URL
	h.is = &issuer{g: h.g, expiresIn: 3600}
	h.as.Handle(oauthtest.PathToken, h.is.serve)
	h.as.Handle(oauthtest.PathRegister, func(w http.ResponseWriter, _ *http.Request) {
		h.regMu.Lock()
		h.regs++
		id := fmt.Sprintf("dcr-client-%d", h.regs)
		h.regMu.Unlock()
		w.WriteHeader(http.StatusCreated)
		oauthtest.WriteJSON(w, map[string]any{"client_id": id})
	})
	st, err := store.Open(h.ctx, filepath.Join(h.dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h.st = st
	h.fs = &flakyStore{ConnectorStore: st}
	if h.sec, err = secrets.Open(filepath.Join(h.dir, "auth.toml")); err != nil {
		t.Fatal(err)
	}
	h.m = h.start(nil)
	return h
}

// start makes a manager over the harness's files, as at daemon start.
func (h *harness) start(conns map[string]config.Connector) *Manager {
	h.t.Helper()
	m, err := New(h.ctx, Options{
		Dir: h.dir, Connectors: conns, Store: h.fs, Secrets: h.sec, Events: h.events,
		HTTPClient: h.as.Client(), Now: h.clock.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = m.Close() })
	return m
}

// restart closes the manager and starts another from connectors.toml.
func (h *harness) restart() {
	h.t.Helper()
	_ = h.m.Close()
	h.m = h.start(h.fileConnectors())
}

func (h *harness) registrations() int {
	h.regMu.Lock()
	defer h.regMu.Unlock()
	return h.regs
}

// create opens an OAuth connector "gh" with redirect.
func (h *harness) create(id, redirect string) Started {
	h.t.Helper()
	s, err := h.m.Create(h.ctx, CreateRequest{ID: id, URL: h.srv.URL, Auth: "oauth", RedirectURI: redirect})
	if err != nil {
		h.t.Fatalf("create: %v", err)
	}
	if s.AuthorizationURL == "" || s.Connector.State != store.ConnectorAuthorizing {
		h.t.Fatalf("create: %+v", s)
	}
	return s
}

// approve is the person signing in: the redirect carries a code and the
// intent's state.
func approve(t *testing.T, s Started) oauth.Callback {
	t.Helper()
	u, err := url.Parse(s.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	return oauth.Callback{Code: "FAKEcode", State: u.Query().Get("state")}
}

func clientID(t *testing.T, s Started) string {
	t.Helper()
	u, _ := url.Parse(s.AuthorizationURL)
	return u.Query().Get("client_id")
}

// connect runs create and code.
func (h *harness) connect(id, redirect string) Connector {
	h.t.Helper()
	s := h.create(id, redirect)
	c, err := h.m.Code(h.ctx, id, approve(h.t, s))
	if err != nil {
		h.t.Fatalf("code: %v", err)
	}
	if c.State != store.ConnectorConnected {
		h.t.Fatalf("after code: %+v", c)
	}
	return c
}

func (h *harness) state(id string) (store.ConnectorState, string) {
	h.t.Helper()
	c, err := h.m.Get(h.ctx, id)
	if err != nil {
		h.t.Fatalf("get %s: %v", id, err)
	}
	return c.State, c.Detail
}

func (h *harness) secret(id, key string) string {
	v, _ := h.sec.Get(secrets.Connectors, id, key)
	return v
}

// fileConnectors parses connectors.toml as config does.
func (h *harness) fileConnectors() map[string]config.Connector {
	h.t.Helper()
	return parseConnectors(h.t, filepath.Join(h.dir, config.FileConnectors))
}

// writeConnectors writes connectors.toml by hand, holding cs.
func (h *harness) writeConnectors(cs ...config.Connector) {
	h.t.Helper()
	h.writeFile(connectorsTOML(h.t, cs...))
}

func (h *harness) writeFile(body string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, config.FileConnectors), []byte(body), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// gone asserts nothing of id is left anywhere.
func (h *harness) gone(id string) {
	h.t.Helper()
	if _, err := h.m.Get(h.ctx, id); err != ErrNotFound {
		h.t.Errorf("get after delete: %v", err)
	}
	if _, err := h.st.GetConnector(h.ctx, id); err != store.ErrNotFound {
		h.t.Errorf("row left: %v", err)
	}
	if keys := h.sec.Keys(secrets.Connectors, id); len(keys) != 0 {
		h.t.Errorf("auth.toml keys left: %v", keys)
	}
	if _, ok := h.fileConnectors()[id]; ok {
		h.t.Error("connectors.toml entry left")
	}
}

func failAll(store.Connector) bool { return true }

func wait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func basic(id, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(id+":"+secret))
}

func mustRow(t *testing.T, h *harness, id string) store.Connector {
	t.Helper()
	r, err := h.st.GetConnector(h.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
