package connector

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/connector/mcp"
	"holaclaw/internal/connector/mcp/mcptest"
	"holaclaw/internal/connector/oauth"
	"holaclaw/internal/connector/oauth/oauthtest"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

func (is *issuer) setExpiresIn(s int) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.expiresIn = s
}

func (s *sink) count(t event.Type) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.events {
		if e.Type == t {
			n++
		}
	}
	return n
}

// eventually polls cond for up to 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A call refreshes first when the token expires within 60 s.
func TestRefreshBeforeCall(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	h.clock.advance(59*time.Minute + 30*time.Second) // 30 s left
	h.is.setExpiresIn(7200)                          // the next token outlives the clock's jump
	before := h.is.count("refresh_token")
	if res, err := h.m.CallTool(h.ctx, "gh", "echo", nil); err != nil || res.Text != "hello" {
		t.Fatalf("call: %v", err)
	}
	if h.is.count("refresh_token") != before+1 {
		t.Errorf("refreshes = %d, want %d", h.is.count("refresh_token"), before+1)
	}
	if g := h.is.last(); g.Get("refresh_token") != "FAKErefresh1" || g.Get("client_id") != oauth.CIMDClientID || g.Get("resource") != h.srv.URL {
		t.Errorf("refresh request = %v", g)
	}
	if h.secret("gh", secrets.ConnectorAccessToken) != "FAKEaccess2" || h.secret("gh", secrets.ConnectorRefreshToken) != "FAKErefresh2" {
		t.Error("refreshed tokens not stored")
	}
	if h.events.count(event.AuthRefreshed) != 1 {
		t.Errorf("auth.refreshed events = %d", h.events.count(event.AuthRefreshed))
	}
	// Not due: no refresh.
	if _, err := h.m.CallTool(h.ctx, "gh", "echo", nil); err != nil || h.is.count("refresh_token") != before+1 {
		t.Errorf("second call refreshed: %v", err)
	}
	if !strings.Contains(h.events.all(), `"kind":"connector"`) {
		t.Error("auth.refreshed without kind")
	}
}

// A 401 on a call refreshes once and retries once.
func TestRefreshAfter401(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	h.g.set(func(g *guard) { delete(g.ok, "FAKEaccess1") }) // revoked server side
	if res, err := h.m.CallTool(h.ctx, "gh", "echo", nil); err != nil || res.Text != "hello" {
		t.Fatalf("call after revoke: %v", err)
	}
	if h.is.count("refresh_token") != 1 {
		t.Errorf("refreshes = %d", h.is.count("refresh_token"))
	}
	// A server that refuses every token: one refresh, one retry, an error.
	h.g.set(func(g *guard) { g.ok = map[string]bool{}; g.down = false })
	h.is.mu.Lock()
	h.is.g = &guard{ok: map[string]bool{}} // new tokens are not accepted either
	h.is.mu.Unlock()
	_, err := h.m.CallTool(h.ctx, "gh", "echo", nil)
	if !errors.Is(err, mcp.ErrUnauthorized) || h.is.count("refresh_token") != 2 {
		t.Errorf("err = %v, refreshes %d", err, h.is.count("refresh_token"))
	}
	// A rejected refresh on a call: expired.
	h.is.setFail("invalid_grant")
	if _, err := h.m.CallTool(h.ctx, "gh", "echo", nil); !errors.Is(err, ErrNeedsReconnect) {
		t.Errorf("rejected on call: %v", err)
	}
	if st, d := h.state("gh"); st != store.ConnectorExpired || d != DetailRefreshRejected {
		t.Errorf("state = %s/%s", st, d)
	}
}

// A 401 at dial refreshes once and dials again, with or without an expiry.
func TestRefreshOnDial401(t *testing.T) {
	h := newHarness(t)
	h.is.setExpiresIn(0) // no lifetime: TokenExpiry stays nil
	h.connect("gh", pinned)
	if c, _ := h.m.Get(h.ctx, "gh"); c.TokenExpiry != nil {
		t.Fatalf("expiry = %v", c.TokenExpiry)
	}
	h.restart()
	h.g.set(func(g *guard) { delete(g.ok, "FAKEaccess1") })
	if res, err := h.m.CallTool(h.ctx, "gh", "echo", nil); err != nil || res.Text != "hello" {
		t.Fatalf("call: %v", err)
	}
	if n := h.is.count("refresh_token"); n != 1 {
		t.Errorf("refreshes = %d", n)
	}
}

// Two calls that both meet a 401 refresh once.
func TestConcurrent401OneRefresh(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	h.g.set(func(g *guard) { delete(g.ok, "FAKEaccess1") })
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = h.m.CallTool(h.ctx, "gh", "echo", nil)
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || h.is.count("refresh_token") != 1 {
		t.Errorf("errs %v, refreshes %d", errs, h.is.count("refresh_token"))
	}
}

// A rejected refresh is expired at once; calls answer a readable error.
func TestRefreshRejected(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	h.is.setFail("invalid_grant")
	h.clock.advance(55 * time.Minute) // within one check of the expiry
	h.m.Check(h.ctx)
	if st, d := h.state("gh"); st != store.ConnectorExpired || d != DetailRefreshRejected {
		t.Fatalf("after invalid_grant: %s/%s", st, d)
	}
	_, err := h.m.CallTool(h.ctx, "gh", "echo", nil)
	var se *StateError
	if !errors.As(err, &se) || !errors.Is(err, ErrNeedsReconnect) || err.Error() != "connector gh needs to be reconnected by an admin" {
		t.Errorf("call when expired: %v", err)
	}
	if tools, err := h.m.Tools("gh"); err != nil || len(tools) != 1 {
		t.Errorf("tools when expired: %v %v", tools, err)
	}
	// Expired connectors are not checked.
	n := h.is.count("refresh_token")
	h.m.Check(h.ctx)
	if h.is.count("refresh_token") != n {
		t.Error("checked an expired connector")
	}
}

// A refresh before a call that is rejected expires the connector.
func TestTokenSourceRefreshRejected(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	h.clock.advance(59*time.Minute + 30*time.Second)
	h.is.setFail("invalid_grant")
	if _, err := h.m.CallTool(h.ctx, "gh", "echo", nil); !errors.Is(err, ErrNeedsReconnect) {
		t.Errorf("call = %v", err)
	}
	if st, d := h.state("gh"); st != store.ConnectorExpired || d != DetailRefreshRejected {
		t.Errorf("state = %s/%s", st, d)
	}
}

// A transient refresh failure is not retried before every call.
func TestRefreshCooldown(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	h.clock.advance(59*time.Minute + 30*time.Second)
	h.is.setFail("server_error")
	if _, err := h.m.CallTool(h.ctx, "gh", "echo", nil); err != nil {
		t.Fatalf("call with a still valid token: %v", err)
	}
	if n := h.is.count("refresh_token"); n != 1 {
		t.Fatalf("refreshes = %d", n)
	}
	if _, err := h.m.CallTool(h.ctx, "gh", "echo", nil); err != nil || h.is.count("refresh_token") != 1 {
		t.Errorf("retried inside the cooldown: %v, %d", err, h.is.count("refresh_token"))
	}
	h.clock.advance(refreshCooldown + time.Second)
	_, _ = h.m.CallTool(h.ctx, "gh", "echo", nil)
	if h.is.count("refresh_token") < 2 {
		t.Error("not retried after the cooldown")
	}
}

// A refresh finishes and is stored although the caller's ctx is cancelled
// while the token request runs.
func TestRefreshSurvivesCancel(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	h.clock.advance(59*time.Minute + 30*time.Second)
	h.is.setExpiresIn(7200) // the next token outlives the clock's jump
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	h.as.Handle(oauthtest.PathToken, func(w http.ResponseWriter, r *http.Request) {
		cancel()
		time.Sleep(50 * time.Millisecond)
		h.is.serve(w, r)
	})
	_, _ = h.m.CallTool(ctx, "gh", "echo", nil)
	if h.secret("gh", secrets.ConnectorAccessToken) != "FAKEaccess2" || h.secret("gh", secrets.ConnectorRefreshToken) != "FAKErefresh2" {
		t.Error("refreshed tokens lost")
	}
	if c, _ := h.m.Get(h.ctx, "gh"); c.TokenExpiry == nil || c.TokenExpiry.Sub(h.clock.Now()) < 10*time.Minute {
		t.Errorf("expiry not stored: %v", c.TokenExpiry)
	}
}

// A 401 on a ping whose refresh fails is a refresh failure.
func TestPing401RefreshFailed(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	h.g.set(func(g *guard) { delete(g.ok, "FAKEaccess1") })
	h.is.setFail("server_error")
	for range maxFailures {
		h.m.Check(h.ctx)
	}
	if st, d := h.state("gh"); st != store.ConnectorDegraded || d != DetailRefreshFailed {
		t.Errorf("state = %s/%s", st, d)
	}
}

// Three failed checks degrade, one success recovers.
func TestDegradedAndRecovery(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	h.g.set(func(g *guard) { g.down = true })
	for i := 1; i <= 2; i++ {
		h.m.Check(h.ctx)
		c, _ := h.m.Get(h.ctx, "gh")
		if c.State != store.ConnectorConnected || c.Failures != i {
			t.Fatalf("check %d: %s failures %d", i, c.State, c.Failures)
		}
	}
	h.m.Check(h.ctx)
	if st, d := h.state("gh"); st != store.ConnectorDegraded || d != DetailPingFailed {
		t.Fatalf("after 3: %s/%s", st, d)
	}
	// Calls still try.
	if _, err := h.m.CallTool(h.ctx, "gh", "echo", nil); err == nil || errors.Is(err, ErrNeedsReconnect) || strings.Contains(err.Error(), "down") {
		t.Errorf("call when degraded and down: %v", err)
	}
	h.g.set(func(g *guard) { g.down = false })
	h.m.Check(h.ctx)
	c, _ := h.m.Get(h.ctx, "gh")
	if c.State != store.ConnectorConnected || c.Detail != "" || c.Failures != 0 || c.LastCheck == nil {
		t.Errorf("after recovery: %+v", c)
	}
	want := []string{"pending/", "authorizing/", "connected/", "degraded/" + DetailPingFailed, "connected/"}
	if got := h.events.statuses("gh"); !slices.Equal(got, want) {
		t.Errorf("events = %v", got)
	}

	// A refresh that fails (not rejected) counts as a failure.
	h.is.setFail("server_error")
	h.clock.advance(55 * time.Minute)
	h.m.Check(h.ctx)
	if c, _ := h.m.Get(h.ctx, "gh"); c.Failures != 1 || c.State != store.ConnectorConnected {
		t.Errorf("after refresh failure: %+v", c)
	}
}

// tools/list_changed refreshes the cached tool list.
func TestToolsListChanged(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	h.srv.SetTools(mcptest.TextTool("echo", "hello"), mcptest.TextTool("second", "two"))
	eventually(t, "the new tool list", func() bool {
		tools, _ := h.m.Tools("gh")
		return len(tools) == 2
	})
	if r := mustRow(t, h, "gh"); !strings.Contains(string(r.Tools), "second") {
		t.Errorf("row tools = %s", r.Tools)
	}
	// And every check.
	h.srv.SetTools(mcptest.TextTool("third", "3"))
	h.m.Check(h.ctx)
	if tools, _ := h.m.Tools("gh"); len(tools) != 1 || tools[0].Name != "third" {
		t.Errorf("after check: %+v", tools)
	}
	if _, err := h.m.Tools("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("tools of unknown: %v", err)
	}
	h.create("new", pinned)
	if _, err := h.m.Tools("new"); !errors.Is(err, ErrNotConnected) {
		t.Errorf("tools while creating: %v", err)
	}
	if _, err := h.m.CallTool(h.ctx, "new", "echo", nil); !errors.Is(err, ErrNotConnected) {
		t.Errorf("call while creating: %v", err)
	}
}

// list_changed refreshes at most once per interval, with a trailing one.
func TestListChangedThrottle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.m.listInterval = 2 * time.Second
	h.connect("gh", pinned)
	h.srv.SetTools(mcptest.TextTool("echo", "hello"), mcptest.TextTool("two", "2"))
	eventually(t, "first refresh", func() bool { tools, _ := h.m.Tools("gh"); return len(tools) == 2 })
	h.srv.SetTools(mcptest.TextTool("three", "3"))
	// While the trailing refresh is due, the list is the first one.
	c := h.conn("gh")
	eventually(t, "a trailing refresh due", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.listTimer != nil && len(c.tools) != 2 {
			t.Errorf("refreshed inside the interval: %+v", c.tools)
		}
		return c.listTimer != nil
	})
	eventually(t, "trailing refresh", func() bool { tools, _ := h.m.Tools("gh"); return len(tools) == 1 && tools[0].Name == "three" })
}

// An open server connected by create follows list_changed: the probe's
// client it adopts listens too.
func TestOpenServerListChanged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.g.set(func(g *guard) { g.open = true })
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "homes", URL: h.srv.URL}); err != nil {
		t.Fatal(err)
	}
	h.srv.SetTools(mcptest.TextTool("echo", "hello"), mcptest.TextTool("second", "two"))
	eventually(t, "the new tool list", func() bool { tools, _ := h.m.Tools("homes"); return len(tools) == 2 })
}

// A redial while a trailing refresh is due: the change seen by the new
// client is listed from it once the interval ends, and the old client is
// never asked. A client that is not the connector's arms nothing.
func TestListChangedAcrossRedial(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	logs := &syncBuffer{}
	h.m.log = slog.New(slog.NewTextHandler(logs, nil))
	h.m.listInterval = 2 * time.Second
	h.g.set(func(g *guard) { g.open = true })
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "homes", URL: h.srv.URL}); err != nil {
		t.Fatal(err)
	}
	h.srv.SetTools(mcptest.TextTool("echo", "hello"), mcptest.TextTool("two", "2"))
	eventually(t, "first refresh", func() bool { tools, _ := h.m.Tools("homes"); return len(tools) == 2 })
	c := h.conn("homes")
	c.mu.Lock()
	old := c.client
	c.mu.Unlock()
	h.srv.SetTools(mcptest.TextTool("three", "3"))
	eventually(t, "a trailing refresh due", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.listTimer != nil })

	if _, err := h.m.Reconnect(h.ctx, "homes", ReconnectRequest{}); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	timer := c.listTimer
	c.mu.Unlock()
	if timer == nil {
		t.Fatal("the trailing refresh was dropped by the redial")
	}
	h.srv.SetTools(mcptest.TextTool("four", "4"))
	eventually(t, "the change on the new client", func() bool {
		tools, _ := h.m.Tools("homes")
		return len(tools) == 1 && tools[0].Name == "four"
	})
	if strings.Contains(logs.String(), "not refreshed") {
		t.Errorf("warning from the old client: %s", logs.String())
	}

	// The old client's list_changed is ignored.
	c.mu.Lock()
	last := c.lastList
	c.mu.Unlock()
	h.m.toolsChanged(c, old)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.listTimer != nil || !c.lastList.Equal(last) {
		t.Errorf("old client: timer armed %v, lastList moved %v", c.listTimer != nil, !c.lastList.Equal(last))
	}
}

// conn is the manager's conn of id.
func (h *harness) conn(id string) *conn {
	h.t.Helper()
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	c := h.m.conns[id]
	if c == nil {
		h.t.Fatalf("no conn %s", id)
	}
	return c
}

// syncBuffer is a bytes.Buffer safe for a logger and a test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Tool names that cannot follow "<id>." are dropped, so are duplicates;
// an unchanged list is not written again.
func TestToolNamesAndCaps(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	h.srv.SetTools(mcptest.TextTool("bad.name", "x"), mcptest.TextTool("good", "y"),
		mcptest.TextTool(strings.Repeat("a", 65), "z"), mcptest.TextTool("sp ace", "w"))
	h.m.Check(h.ctx)
	if tools, _ := h.m.Tools("gh"); len(tools) != 1 || tools[0].Name != "good" {
		t.Errorf("tools = %+v", tools)
	}
	got, _, err := h.m.cacheTools(&conn{id: "x"}, []mcp.Tool{{Name: "a"}, {Name: "a", Description: "dup"}, {Name: "b"}})
	if err != nil || len(got) != 2 || got[0].Description != "" {
		t.Errorf("duplicates: %+v", got)
	}

	var puts int
	var mu sync.Mutex
	h.fs.failWhen(func(store.Connector) bool { mu.Lock(); puts++; mu.Unlock(); return false })
	h.m.Check(h.ctx)
	if puts != 1 { // the check itself, not the unchanged list
		t.Errorf("puts = %d", puts)
	}

	if s := capInfo(strings.Repeat("é", 200)); len(s) > maxServerInfo || !strings.HasPrefix(s, "é") {
		t.Errorf("capInfo = %d bytes", len(s))
	}
	if capSession(strings.Repeat("s", maxSessionID+1)) != "" || capSession("abc") != "abc" {
		t.Error("capSession")
	}
}

func TestDeleteDuringCheck(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	blocked, release := h.g.block()
	defer release()
	done := make(chan struct{})
	go func() {
		h.m.Check(h.ctx)
		close(done)
	}()
	wait(t, blocked, "the check's ping")
	if err := h.m.Delete(h.ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	release()
	wait(t, done, "the check")
	h.gone("gh")
}

// A check cancelled mid-flight (the daemon stopping) returns at once and is
// not a failure. The guard holds what the check sends first: on this modern
// server Ping reconnects, so that is the new handshake.
func TestCancelledCheckRecordsNothing(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	before := mustRow(t, h, "gh")
	events := len(h.events.statuses("gh"))
	blocked, release := h.g.block()
	defer release()
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() {
		h.m.Check(ctx)
		close(done)
	}()
	wait(t, blocked, "the check's ping")
	cancel()
	wait(t, done, "the check")
	release()
	after := mustRow(t, h, "gh")
	if after.Failures != before.Failures || after.State != store.ConnectorConnected || after.Detail != "" {
		t.Errorf("after a cancelled check: failures %d → %d, %s/%s", before.Failures, after.Failures, after.State, after.Detail)
	}
	if n := len(h.events.statuses("gh")); n != events {
		t.Errorf("a cancelled check emitted %d events", n-events)
	}
}

func TestDeleteDuringCall(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	h.srv.SetTools(mcptest.TextTool("echo", "hello"), mcptest.BlockingTool("slow", nil))
	done := make(chan error, 1)
	go func() {
		_, err := h.m.CallTool(h.ctx, "gh", "slow", nil)
		done <- err
	}()
	eventually(t, "the call to run", func() bool { return h.srv.MaxConcurrentCalls() >= 1 })
	if err := h.m.Delete(h.ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("call succeeded after delete")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call not interrupted by delete")
	}
	h.gone("gh")
}

// Close: no dial afterwards, every client closed.
func TestClose(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	if err := h.m.Close(); err != nil {
		t.Fatal(err)
	}
	// CallTool and Check return after any request they send; the client's
	// own shutdown sends no request of theirs.
	calls := func() int {
		n := 0
		for _, r := range h.srv.Requests() {
			if r.HTTPMethod == http.MethodPost && r.RPCMethod != "" && !strings.HasPrefix(r.RPCMethod, "notifications/") {
				n++
			}
		}
		return n
	}
	n := calls()
	if _, err := h.m.CallTool(h.ctx, "gh", "echo", nil); !errors.Is(err, ErrClosed) {
		t.Errorf("call after close: %v", err)
	}
	h.m.Check(h.ctx)
	if got := calls(); got != n {
		t.Errorf("requests after close: %d new", got-n)
	}
	h.m.mu.Lock()
	for _, c := range h.m.conns {
		if c.client != nil {
			t.Errorf("%s keeps a client", c.id)
		}
	}
	h.m.mu.Unlock()
}

// No token, code, verifier, state or secret in any event or error.
func TestNoSecretsInEvents(t *testing.T) {
	h := newHarness(t)
	h.is.setExpiresIn(30)
	s := h.create("gh", pinned)
	cb := approve(t, s)
	if _, err := h.m.Code(h.ctx, "gh", cb); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.CallTool(h.ctx, "gh", "echo", nil); err != nil {
		t.Fatal(err)
	}
	h.is.setFail("invalid_grant")
	h.m.Check(h.ctx)
	_, err := h.m.CallTool(h.ctx, "gh", "echo", nil)
	all := h.events.all()
	for _, leak := range []string{fakeHit, cb.State, "SERVERTEXT", "code_challenge"} {
		if strings.Contains(all, leak) {
			t.Errorf("events carry %q", leak)
		}
		if err != nil && strings.Contains(err.Error(), leak) {
			t.Errorf("error carries %q", leak)
		}
	}
	if h.events.count(event.ConnectorStatus) == 0 || h.events.count(event.AuthRefreshed) == 0 {
		t.Error("expected both event types")
	}
	if v, err := h.m.Get(h.ctx, "gh"); err != nil || v.Detail != DetailRefreshRejected {
		t.Errorf("view: %+v", v)
	}
}

func TestStateErrorText(t *testing.T) {
	if s := (&StateError{ID: "x", State: store.ConnectorPending}).Error(); s != "connector x is not connected yet" {
		t.Error(s)
	}
	e := callError("x", errors.New("SERVERTEXT"))
	if e.Error() != "connector x: request failed" {
		t.Error(e)
	}
}
