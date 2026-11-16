package api

import (
	"context"
	_ "embed"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/config"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// Browser endpoints (docs/browser.md): the live view of an agent's browser
// and the takeover lock. The handlers hold no state — internal/browser/live
// owns tickets, the lock and the stream relay and implements BrowserBackend —
// and they never touch what flows over the stream: input is the one thing in
// the API that is neither logged nor traced.
//
// They answer 501 while the daemon has no browser backend, like every other
// optional surface.

// BrowserBackend is the live-view service as the handlers see it. It is
// declared here, and internal/browser/live implements it, so the API does not
// depend on the browser half (the TaskBackend pattern).
type BrowserBackend interface {
	// Sessions lists every running agent browser with its lock state. The
	// handler filters it to what the caller may see.
	Sessions(ctx context.Context) ([]BrowserStatus, error)
	// IssueTicket mints a single-use, short-lived ticket for one agent's live
	// view, bound to the user who asked for it.
	IssueTicket(agent string, identityID event.ID) (BrowserTicket, error)
	// RedeemTicket consumes a ticket and returns who it was issued to. A
	// wrong, used or expired value is simply not ok.
	RedeemTicket(value string) (agent string, identityID event.ID, ok bool)
	// Takeover takes the lock for identityID: ErrBrowserHeld when somebody else
	// holds it, ErrBrowserNoSession when the agent has no browser.
	Takeover(ctx context.Context, agent string, identityID event.ID) error
	// Release hands the browser back. ErrBrowserNotHolder for anyone else.
	Release(ctx context.Context, agent string, identityID event.ID) error
	// Watch relays the agent's live view to v until ctx ends.
	Watch(ctx context.Context, agent string, identityID event.ID, v BrowserViewer) error
}

// BrowserStatus is one running agent browser: the sidecar's session plus what
// only the daemon knows — who holds the lock, who is watching, and whether the
// run is waiting for a person.
type BrowserStatus struct {
	Agent    string
	Started  time.Time
	Tabs     []BrowserTab
	Holder   event.ID
	Watchers int
	// TakeoverRequested is the blocker a run is waiting on ("login",
	// "captcha", "2fa"), else empty.
	TakeoverRequested string
}

// BrowserTab is one page target. Title comes from the page: untrusted.
type BrowserTab struct {
	ID     string
	URL    string
	Title  string
	Active bool
}

// BrowserTicket is what a browser can carry on a WebSocket URL instead of an
// Authorization header.
type BrowserTicket struct {
	Value     string
	ExpiresAt time.Time
}

// BrowserViewer is one live-view connection as the relay writes to it: JPEG
// frames out, control messages both ways. Nothing read here is ever logged.
type BrowserViewer interface {
	WriteFrame(ctx context.Context, jpeg []byte) error
	WriteText(ctx context.Context, text []byte) error
	Read(ctx context.Context) ([]byte, error)
}

// The errors a BrowserBackend returns. They are declared here because the
// handlers map them onto status codes, and the backend lives elsewhere.
var (
	// ErrBrowserHeld means somebody else holds the takeover lock.
	ErrBrowserHeld = errors.New("browser: held by another user")
	// ErrBrowserNoSession means the agent has no running browser.
	ErrBrowserNoSession = errors.New("browser: no session")
	// ErrBrowserNotHolder means the caller is not the lock holder.
	ErrBrowserNotHolder = errors.New("browser: not the holder")
)

// maxBrowserStreamMessage caps one message read from a viewer: pointer and key
// events are tiny, a paste is the only thing with any size to it.
const maxBrowserStreamMessage = 64 << 10

//go:embed viewer.html
var viewerHTML []byte

// browserCSP is the viewer page's policy: it loads nothing at all, draws
// frames from blobs, talks to its own origin, and is never framed — a live
// view of somebody's logged-in browser is not a page to put in an iframe.
const browserCSP = "default-src 'none'; script-src 'unsafe-inline'; img-src blob: data:; " +
	"style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'"

// ListBrowserSessions lists the running browsers the caller may see: their own
// agents for a client token, every one for admin and the socket.
func (h *handlers) ListBrowserSessions(w http.ResponseWriter, r *http.Request) {
	if h.b.Browser == nil {
		notImplemented(w, "browser")
		return
	}
	p, _ := PrincipalFrom(r.Context())
	all, err := h.b.Browser.Sessions(r.Context())
	if err != nil {
		h.b.fail(w, "list browser sessions", err)
		return
	}
	out := make([]gen.BrowserSession, 0, len(all))
	for _, s := range all {
		if mayUseAgent(p, h.b.Config, s.Agent) {
			out = append(out, browserSession(s))
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Sessions []gen.BrowserSession `json:"sessions"`
	}{out})
}

// ViewBrowser issues the ticket the stream URL carries. Watching is for users
// of the agent; a guest is refused, because every user of an agent shares that
// agent's logins (docs/browser.md).
func (h *handlers) ViewBrowser(w http.ResponseWriter, r *http.Request, agent gen.Agent) {
	if h.b.Browser == nil {
		notImplemented(w, "browser")
		return
	}
	p, _ := PrincipalFrom(r.Context())
	identityID := p.UserID
	if !knownAgent(h.b.Config, agent) {
		notFound(w)
		return
	}
	if !mayUseAgent(p, h.b.Config, agent) {
		writeError(w, http.StatusForbidden, CodeForbidden, "this user may not watch that agent's browser")
		return
	}
	ticket, err := h.b.Browser.IssueTicket(agent, identityID)
	if err != nil {
		h.b.fail(w, "issue browser ticket", err)
		return
	}
	writeJSON(w, http.StatusOK, gen.BrowserTicket{
		Ticket:    ticket.Value,
		ExpiresAt: ticket.ExpiresAt,
		Path:      "/browser/sessions/" + agent + "/stream?ticket=" + ticket.Value,
	})
}

// StreamBrowser is the one WebSocket of the API. It carries no bearer token
// (the spec says `security: []`): the ticket issued by ViewBrowser is the
// whole authentication, and it is consumed here.
func (h *handlers) StreamBrowser(w http.ResponseWriter, r *http.Request, agent gen.Agent, params gen.StreamBrowserParams) {
	if h.b.Browser == nil {
		notImplemented(w, "browser")
		return
	}
	ticketAgent, identityID, ok := h.b.Browser.RedeemTicket(params.Ticket)
	if !ok || ticketAgent != agent {
		// A ticket for another agent is as good as no ticket: one answer for
		// every failure, like every other auth failure in the API.
		unauthorized(w)
		return
	}
	// A ticket minted just before its identity was disabled is still unused:
	// it must not open what the token no longer can.
	if h.b.Store != nil {
		if i, err := h.b.Store.GetIdentity(r.Context(), identityID); err == nil && i.Disabled() {
			writeError(w, http.StatusForbidden, CodeIdentityDisabled, "the identity is disabled")
			return
		}
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return // Accept has already answered
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(maxBrowserStreamMessage)

	// The error is not logged with the request: it can only say how the live
	// view ended, and the relay keeps its content out of it.
	if err := h.b.Browser.Watch(r.Context(), agent, identityID, &wsViewer{conn: conn}); err != nil {
		_ = conn.Close(websocket.StatusInternalError, "stream ended")
		return
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// TakeoverBrowser takes the lock; the running task suspends until it is
// handed back. Admin scope is enforced by the scope middleware.
func (h *handlers) TakeoverBrowser(w http.ResponseWriter, r *http.Request, agent gen.Agent) {
	if h.b.Browser == nil {
		notImplemented(w, "browser")
		return
	}
	p, _ := PrincipalFrom(r.Context())
	identityID := p.UserID
	if err := h.b.Browser.Takeover(r.Context(), agent, identityID); err != nil {
		h.failBrowser(w, "takeover browser", err)
		return
	}
	all, err := h.b.Browser.Sessions(r.Context())
	if err != nil {
		h.b.fail(w, "takeover browser", err)
		return
	}
	for _, s := range all {
		if s.Agent == agent {
			writeJSON(w, http.StatusOK, browserSession(s))
			return
		}
	}
	notFound(w)
}

// ReleaseBrowser hands the browser back. Idempotent for the holder.
func (h *handlers) ReleaseBrowser(w http.ResponseWriter, r *http.Request, agent gen.Agent) {
	if h.b.Browser == nil {
		notImplemented(w, "browser")
		return
	}
	p, _ := PrincipalFrom(r.Context())
	identityID := p.UserID
	if err := h.b.Browser.Release(r.Context(), agent, identityID); err != nil {
		h.failBrowser(w, "release browser", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetBrowserViewer serves the built-in viewer page. It is a manual-testing
// tool: the real GUI is HolaClaw.
func (h *handlers) GetBrowserViewer(w http.ResponseWriter, _ *http.Request) {
	if h.b.Browser == nil {
		notImplemented(w, "browser")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", browserCSP)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(viewerHTML)
}

// --- plumbing ----------------------------------------------------------------

// failBrowser maps the lock errors onto the stable codes; anything else takes
// the ordinary path.
func (h *handlers) failBrowser(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, ErrBrowserHeld):
		writeError(w, http.StatusConflict, CodeConflict, "another user holds the browser")
	case errors.Is(err, ErrBrowserNoSession):
		writeError(w, http.StatusNotFound, CodeNotFound, "not found")
	case errors.Is(err, ErrBrowserNotHolder):
		writeError(w, http.StatusForbidden, CodeForbidden, "another user holds the browser")
	default:
		h.b.fail(w, what, err)
	}
}

// knownAgent reports whether the agent is configured. An agent that is not is
// a not_found, whoever asks.
func knownAgent(cfg *config.Config, agent string) bool {
	if cfg == nil {
		return false
	}
	_, ok := cfg.Agents[agent]
	return ok
}

// mayUseAgent is the live-view authorisation rule, and it is the one the rest
// of the API already uses for agents: every user of an agent shares that
// agent's browser profile and logins (docs/browser.md, docs/security.md N8), so any user
// bound to a token may watch any configured agent — and a guest may not watch
// at all. Admin and the socket see every agent, configured or not, because
// they manage the agents.
func mayUseAgent(p auth.Principal, cfg *config.Config, agent string) bool {
	if p.Scope == auth.ScopeAdmin {
		return true
	}
	if p.UserID == "" || p.Role == store.RoleGuest {
		return false
	}
	return knownAgent(cfg, agent)
}

// browserSession renders one status on the wire.
func browserSession(s BrowserStatus) gen.BrowserSession {
	out := gen.BrowserSession{
		Agent:    s.Agent,
		Started:  s.Started,
		Watchers: s.Watchers,
		Holder:   optID(s.Holder),
	}
	out.Tabs = make([]struct {
		Active bool   `json:"active"`
		Id     string `json:"id"`
		Title  string `json:"title"`
		Url    string `json:"url"`
	}, 0, len(s.Tabs))
	for _, t := range s.Tabs {
		out.Tabs = append(out.Tabs, struct {
			Active bool   `json:"active"`
			Id     string `json:"id"`
			Title  string `json:"title"`
			Url    string `json:"url"`
		}{Active: t.Active, Id: t.ID, Title: t.Title, Url: t.URL})
	}
	if b := gen.BrowserSessionTakeoverRequested(s.TakeoverRequested); b.Valid() {
		out.TakeoverRequested = &b
	}
	return out
}

// wsViewer is the BrowserViewer over one accepted WebSocket. Reads that are
// not text are dropped: a viewer sends no binary frames.
type wsViewer struct{ conn *websocket.Conn }

var _ BrowserViewer = (*wsViewer)(nil)

func (v *wsViewer) WriteFrame(ctx context.Context, jpeg []byte) error {
	return v.conn.Write(ctx, websocket.MessageBinary, jpeg)
}

func (v *wsViewer) WriteText(ctx context.Context, text []byte) error {
	return v.conn.Write(ctx, websocket.MessageText, text)
}

func (v *wsViewer) Read(ctx context.Context) ([]byte, error) {
	for {
		typ, data, err := v.conn.Read(ctx)
		if err != nil {
			return nil, err
		}
		if typ == websocket.MessageText {
			return data, nil
		}
	}
}
