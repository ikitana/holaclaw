package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// fakeBrowser is a BrowserBackend with no sidecar behind it: the handlers are
// what is under test, not the live view.
type fakeBrowser struct {
	mu       sync.Mutex
	sessions []BrowserStatus
	tickets  map[string]BrowserStatus // value -> agent/holder of the issue
	issued   int
	takeover error
	release  error
	held     []string // "agent/user" of every successful takeover
	// watch stands in for the relay; nil answers at once.
	watch func(ctx context.Context, agent string, identityID event.ID, v BrowserViewer) error
}

func newFakeBrowser() *fakeBrowser {
	return &fakeBrowser{
		tickets: map[string]BrowserStatus{},
		sessions: []BrowserStatus{{
			Agent:    "main",
			Started:  time.Now().UTC(),
			Tabs:     []BrowserTab{{ID: "t1", URL: "https://example.test", Title: "Example", Active: true}},
			Watchers: 1,
		}},
	}
}

func (f *fakeBrowser) Sessions(context.Context) ([]BrowserStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions, nil
}

func (f *fakeBrowser) IssueTicket(agent string, identityID event.ID) (BrowserTicket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issued++
	value := fmt.Sprintf("ticket-%d", f.issued)
	f.tickets[value] = BrowserStatus{Agent: agent, Holder: identityID}
	return BrowserTicket{Value: value, ExpiresAt: time.Now().Add(time.Minute).UTC()}, nil
}

func (f *fakeBrowser) RedeemTicket(value string) (string, event.ID, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.tickets[value]
	if !ok {
		return "", "", false
	}
	delete(f.tickets, value)
	return t.Agent, t.Holder, true
}

func (f *fakeBrowser) Takeover(_ context.Context, agent string, identityID event.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.takeover != nil {
		return f.takeover
	}
	f.held = append(f.held, agent+"/"+string(identityID))
	for i := range f.sessions {
		if f.sessions[i].Agent == agent {
			f.sessions[i].Holder = identityID
		}
	}
	return nil
}

func (f *fakeBrowser) Release(_ context.Context, agent string, identityID event.ID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.release != nil {
		return f.release
	}
	for i := range f.sessions {
		if f.sessions[i].Agent == agent && f.sessions[i].Holder == identityID {
			f.sessions[i].Holder = ""
		}
	}
	return nil
}

func (f *fakeBrowser) Watch(ctx context.Context, agent string, identityID event.ID, v BrowserViewer) error {
	if f.watch != nil {
		return f.watch(ctx, agent, identityID, v)
	}
	return nil
}

// browserHarness is the API with a browser backend and one guest.
type browserHarness struct {
	*harness
	fake       *fakeBrowser
	guest      store.Identity
	guestToken string
}

func newBrowserHarness(t *testing.T) *browserHarness {
	t.Helper()
	h := newHarness(t)
	fake := newFakeBrowser()
	h.b.Browser = fake
	guest, err := h.auth.EnsureGuest(context.Background(), "telegram", []string{"gus"}, "gus")
	if err != nil {
		t.Fatalf("create guest: %v", err)
	}
	return &browserHarness{
		harness:    h,
		fake:       fake,
		guest:      guest,
		guestToken: h.token("cli-guest", auth.ScopeClient, guest.ID),
	}
}

// TestBrowserNotImplemented pins the shape of /v1 in a build with no browser:
// every operation answers 501, none of them 404.
func TestBrowserNotImplemented(t *testing.T) {
	t.Parallel()
	h := newHarness(t) // no Browser backend
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/browser/sessions"},
		{http.MethodPost, "/browser/sessions/main/view"},
		{http.MethodGet, "/browser/sessions/main/stream?ticket=t"},
		{http.MethodPost, "/browser/sessions/main/takeover"},
		{http.MethodPost, "/browser/sessions/main/release"},
		{http.MethodGet, "/browser/viewer"},
	} {
		errorBody(t, h.do(c.method, c.path, h.adminToken, nil), http.StatusNotImplemented, CodeNotImplemented)
	}
}

func TestListBrowserSessions(t *testing.T) {
	t.Parallel()
	h := newBrowserHarness(t)
	// A second browser for an agent this build does not configure.
	h.fake.sessions = append(h.fake.sessions, BrowserStatus{Agent: "ghost", Started: time.Now().UTC()})

	type list struct {
		Sessions []gen.BrowserSession `json:"sessions"`
	}
	t.Run("a user sees the agents they may use", func(t *testing.T) {
		got := decode[list](t, h.do(http.MethodGet, "/browser/sessions", h.ownerToken, nil), http.StatusOK)
		if len(got.Sessions) != 1 || got.Sessions[0].Agent != "main" {
			t.Fatalf("sessions = %+v", got.Sessions)
		}
		if got.Sessions[0].Watchers != 1 || got.Sessions[0].Tabs[0].Title != "Example" {
			t.Fatalf("session = %+v", got.Sessions[0])
		}
	})
	t.Run("admin sees every session", func(t *testing.T) {
		got := decode[list](t, h.do(http.MethodGet, "/browser/sessions", h.adminToken, nil), http.StatusOK)
		if len(got.Sessions) != 2 {
			t.Fatalf("sessions = %+v", got.Sessions)
		}
	})
	t.Run("a guest sees none", func(t *testing.T) {
		got := decode[list](t, h.do(http.MethodGet, "/browser/sessions", h.guestToken, nil), http.StatusOK)
		if len(got.Sessions) != 0 {
			t.Fatalf("sessions = %+v", got.Sessions)
		}
	})
	t.Run("the blocker a run waits on is on the wire", func(t *testing.T) {
		h.fake.mu.Lock()
		h.fake.sessions[0].TakeoverRequested = "login"
		h.fake.mu.Unlock()
		got := decode[list](t, h.do(http.MethodGet, "/browser/sessions", h.adminToken, nil), http.StatusOK)
		if got.Sessions[0].TakeoverRequested == nil || *got.Sessions[0].TakeoverRequested != gen.Login {
			t.Fatalf("takeover_requested = %v", got.Sessions[0].TakeoverRequested)
		}
		h.fake.mu.Lock()
		h.fake.sessions[0].TakeoverRequested = ""
		h.fake.mu.Unlock()
	})
}

// TestViewBrowserAuthz is the watching rule of docs/browser.md: users of the agent yes,
// guests no, unknown agents not found.
func TestViewBrowserAuthz(t *testing.T) {
	t.Parallel()
	h := newBrowserHarness(t)

	t.Run("a user gets a ticket", func(t *testing.T) {
		tk := decode[gen.BrowserTicket](t, h.do(http.MethodPost, "/browser/sessions/main/view", h.ownerToken, nil), http.StatusOK)
		if tk.Ticket == "" || tk.ExpiresAt.IsZero() {
			t.Fatalf("ticket = %+v", tk)
		}
		if want := "/browser/sessions/main/stream?ticket=" + tk.Ticket; tk.Path != want {
			t.Fatalf("path = %q, want %q", tk.Path, want)
		}
	})
	t.Run("a guest is refused", func(t *testing.T) {
		errorBody(t, h.do(http.MethodPost, "/browser/sessions/main/view", h.guestToken, nil), http.StatusForbidden, CodeForbidden)
	})
	t.Run("an unknown agent is not found", func(t *testing.T) {
		errorBody(t, h.do(http.MethodPost, "/browser/sessions/ghost/view", h.ownerToken, nil), http.StatusNotFound, CodeNotFound)
	})
	t.Run("the socket acts as the owner", func(t *testing.T) {
		rec := h.socket(http.MethodPost, "/browser/sessions/main/view", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body)
		}
		var tk gen.BrowserTicket
		if err := json.Unmarshal(rec.Body.Bytes(), &tk); err != nil {
			t.Fatalf("decode: %v", err)
		}
		h.fake.mu.Lock()
		defer h.fake.mu.Unlock()
		if got := h.fake.tickets[tk.Ticket].Holder; got != h.owner.ID {
			t.Fatalf("ticket user = %q, want the owner %q", got, h.owner.ID)
		}
	})
}

// TestTakeoverAndRelease covers the lock endpoints and the error mapping the
// spec asks for: conflict, not_found, forbidden.
func TestTakeoverAndRelease(t *testing.T) {
	t.Parallel()
	h := newBrowserHarness(t)

	got := decode[gen.BrowserSession](t, h.do(http.MethodPost, "/browser/sessions/main/takeover", h.adminToken, nil), http.StatusOK)
	if got.Holder == nil || *got.Holder != string(h.admin.ID) {
		t.Fatalf("holder = %v, want the admin the token belongs to", got.Holder)
	}
	if resp := h.do(http.MethodPost, "/browser/sessions/main/release", h.adminToken, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("release status = %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}

	h.fake.mu.Lock()
	h.fake.takeover = ErrBrowserHeld
	h.fake.mu.Unlock()
	errorBody(t, h.do(http.MethodPost, "/browser/sessions/main/takeover", h.adminToken, nil), http.StatusConflict, CodeConflict)

	h.fake.mu.Lock()
	h.fake.takeover = ErrBrowserNoSession
	h.fake.mu.Unlock()
	errorBody(t, h.do(http.MethodPost, "/browser/sessions/main/takeover", h.adminToken, nil), http.StatusNotFound, CodeNotFound)

	h.fake.mu.Lock()
	h.fake.release = ErrBrowserNotHolder
	h.fake.mu.Unlock()
	errorBody(t, h.do(http.MethodPost, "/browser/sessions/main/release", h.adminToken, nil), http.StatusForbidden, CodeForbidden)

	// A client token never gets that far: takeover is admin scope.
	errorBody(t, h.do(http.MethodPost, "/browser/sessions/main/takeover", h.ownerToken, nil), http.StatusForbidden, CodeForbidden)
}

// TestStreamTicket is the whole authentication of the WebSocket: no bearer
// token, one ticket, consumed once.
func TestStreamTicket(t *testing.T) {
	t.Parallel()
	h := newBrowserHarness(t)

	t.Run("an unknown ticket is unauthorized", func(t *testing.T) {
		errorBody(t, h.do(http.MethodGet, "/browser/sessions/main/stream?ticket=nope", "", nil),
			http.StatusUnauthorized, CodeUnauthorized)
	})
	t.Run("a ticket for another agent is unauthorized", func(t *testing.T) {
		tk := decode[gen.BrowserTicket](t, h.do(http.MethodPost, "/browser/sessions/main/view", h.ownerToken, nil), http.StatusOK)
		errorBody(t, h.do(http.MethodGet, "/browser/sessions/other/stream?ticket="+tk.Ticket, "", nil),
			http.StatusUnauthorized, CodeUnauthorized)
	})
	t.Run("a used ticket is unauthorized", func(t *testing.T) {
		tk := decode[gen.BrowserTicket](t, h.do(http.MethodPost, "/browser/sessions/main/view", h.ownerToken, nil), http.StatusOK)
		h.fake.RedeemTicket(tk.Ticket)
		errorBody(t, h.do(http.MethodGet, "/browser/sessions/main/stream?ticket="+tk.Ticket, "", nil),
			http.StatusUnauthorized, CodeUnauthorized)
	})
}

// A ticket minted just before its identity was disabled opens nothing, and
// after the enable a new one works again.
func TestStreamTicketOfADisabledIdentity(t *testing.T) {
	t.Parallel()
	h := newBrowserHarness(t)
	view := func() gen.BrowserTicket {
		return decode[gen.BrowserTicket](t, h.do(http.MethodPost, "/browser/sessions/main/view", h.otherToken, nil), http.StatusOK)
	}
	tk := view()
	decode[gen.User](t, h.switchIdentity(h.other.ID, "disable", h.adminToken), http.StatusOK)
	errorBody(t, h.do(http.MethodGet, "/browser/sessions/main/stream?ticket="+tk.Ticket, "", nil),
		http.StatusForbidden, CodeIdentityDisabled)
	errorBody(t, h.do(http.MethodPost, "/browser/sessions/main/view", h.otherToken, nil),
		http.StatusForbidden, CodeIdentityDisabled)

	decode[gen.User](t, h.switchIdentity(h.other.ID, "enable", h.adminToken), http.StatusOK)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(h.srv.URL, "http") + BaseURL + view().Path
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: h.srv.Client()})
	if err != nil {
		t.Fatalf("dial after the enable: %v", err)
	}
	_ = conn.CloseNow()
}

// The bearer bypass is the live view's route and nothing else: an encoded
// slash decodes to a path the router sends elsewhere, so it must not match.
// Every one of these carries a ticket the service would honour, so only the
// path decides.
func TestStreamBypassIsExactOnTheEscapedPath(t *testing.T) {
	t.Parallel()
	h := newBrowserHarness(t)
	ticket := func() string {
		t.Helper()
		tk := decode[gen.BrowserTicket](t, h.do(http.MethodPost, "/browser/sessions/main/view", h.ownerToken, nil), http.StatusOK)
		return tk.Ticket
	}
	for _, path := range []string{
		"/browser/sessions/x%2Fstream",
		"/browser/sessions%2Fx%2Fstream",
		"/browser%2Fsessions%2Fx%2Fstream",
		"/browser/sessions/main%2Fstream",
	} {
		t.Run(path, func(t *testing.T) {
			errorBody(t, h.do(http.MethodGet, path+"?ticket="+ticket(), "", nil),
				http.StatusUnauthorized, CodeUnauthorized)
		})
	}
	// The real route still reaches the handler, which redeems the ticket and
	// then fails the upgrade for a request that is not a WebSocket — anything
	// but the 401 an unauthenticated request gets.
	t.Run("the real route", func(t *testing.T) {
		resp := h.do(http.MethodGet, "/browser/sessions/main/stream?ticket="+ticket(), "", nil)
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode == http.StatusUnauthorized {
			t.Fatalf("a valid ticket on the real route = %d", resp.StatusCode)
		}
	})
}

// TestStreamWebSocket runs the live view end to end over httptest: a frame out,
// an input message in, and the user the ticket was issued to.
func TestStreamWebSocket(t *testing.T) {
	t.Parallel()
	h := newBrowserHarness(t)
	type seen struct {
		agent string
		user  event.ID
		text  []byte
	}
	got := make(chan seen, 1)
	h.fake.watch = func(ctx context.Context, agent string, identityID event.ID, v BrowserViewer) error {
		if err := v.WriteText(ctx, []byte(`{"type":"meta","width":800,"height":600}`)); err != nil {
			return err
		}
		if err := v.WriteFrame(ctx, []byte("jpeg")); err != nil {
			return err
		}
		raw, err := v.Read(ctx)
		if err != nil {
			return err
		}
		got <- seen{agent: agent, user: identityID, text: raw}
		return nil
	}

	tk := decode[gen.BrowserTicket](t, h.do(http.MethodPost, "/browser/sessions/main/view", h.ownerToken, nil), http.StatusOK)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(h.srv.URL, "http") + BaseURL + tk.Path
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: h.srv.Client()})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	typ, meta, err := conn.Read(ctx)
	if err != nil || typ != websocket.MessageText || !strings.Contains(string(meta), `"width":800`) {
		t.Fatalf("meta = %q, %v, %v", meta, typ, err)
	}
	typ, frame, err := conn.Read(ctx)
	if err != nil || typ != websocket.MessageBinary || string(frame) != "jpeg" {
		t.Fatalf("frame = %q, %v, %v", frame, typ, err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"ack"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	s := <-got
	if s.agent != "main" || s.user != h.owner.ID || string(s.text) != `{"type":"ack"}` {
		t.Fatalf("relay saw %+v", s)
	}
}

// TestBrowserViewerPage pins the page and, above all, its policy: it may load
// nothing and talk to nobody but its own origin.
func TestBrowserViewerPage(t *testing.T) {
	t.Parallel()
	h := newBrowserHarness(t)
	resp := h.do(http.MethodGet, "/browser/viewer", h.ownerToken, nil)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content type = %q", ct)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if csp != browserCSP {
		t.Fatalf("csp = %q", csp)
	}
	// A live view of somebody's logged-in browser is never framed: without
	// this a page anywhere could put the viewer in an iframe and watch it.
	for _, want := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("csp = %q, want %q in it", csp, want)
		}
	}
	if !strings.Contains(string(viewerHTML), "<canvas") {
		t.Fatal("the viewer page has no canvas")
	}
}
