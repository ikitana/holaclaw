// Package live is the daemon's live view of an agent's browser
// (docs/browser.md): the tickets a WebSocket URL carries instead of a bearer
// token, the takeover lock a person holds while the run suspends, and the
// relay that fans the sidecar's screencast out to watchers and forwards only
// the holder's input back.
//
// One rule outranks the rest: **nothing read from a viewer is logged, traced
// or put in an error**. What a person types through takeover — passwords,
// one-time codes — goes client → daemon → sidecar and nowhere else, so the
// relay drops a malformed message rather than quoting it, and every event it
// emits carries an agent and a user id and nothing more.
package live

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"holaclaw/internal/api"
	"holaclaw/internal/browser"
	"holaclaw/internal/browser/client"
	"holaclaw/internal/browser/proto"
	"holaclaw/internal/event"
)

// Defaults of the service; Options overrides them.
const (
	// DefaultTicketTTL is how long an unused live-view ticket stands. It only
	// has to survive the round trip from the POST to the WebSocket.
	DefaultTicketTTL = 60 * time.Second
	// DefaultTakeoverTimeout is how long a blocked run waits for a person
	// before it gives up (docs/browser.md: timeout = the run fails).
	DefaultTakeoverTimeout = 5 * time.Minute
	// ticketBytes is the entropy of a ticket value.
	ticketBytes = 32
)

// The types and errors the API declares for its browser backend. They are
// aliased here so the service satisfies api.BrowserBackend directly and the
// API needs no import of this package (the internal/task pattern).
type (
	// Status is one running agent browser plus the lock state.
	Status = api.BrowserStatus
	// Ticket is a single-use live-view ticket.
	Ticket = api.BrowserTicket
	// Viewer is one live-view connection as the relay writes to it.
	Viewer = api.BrowserViewer
)

var (
	// ErrHeld means somebody else holds the takeover lock.
	ErrHeld = api.ErrBrowserHeld
	// ErrNoSession means the agent has no running browser.
	ErrNoSession = api.ErrBrowserNoSession
	// ErrNotHolder means the caller is not the lock holder.
	ErrNotHolder = api.ErrBrowserNotHolder
)

// Sidecar is the part of the sidecar client the live view uses. The real
// client is wrapped by FromClient; tests use a fake.
type Sidecar interface {
	Sessions(ctx context.Context) ([]proto.Session, error)
	Stream(ctx context.Context, agent string) (Stream, error)
}

// Stream is one live view on the sidecar side.
type Stream interface {
	Frames() <-chan client.Frame
	Send(ctx context.Context, m proto.StreamMessage) error
	Ack(ctx context.Context) error
	Close() error
}

// FromClient adapts the sidecar client to Sidecar.
func FromClient(c *client.Client) Sidecar { return clientSidecar{c} }

type clientSidecar struct{ c *client.Client }

func (s clientSidecar) Sessions(ctx context.Context) ([]proto.Session, error) {
	return s.c.Sessions(ctx)
}

func (s clientSidecar) Stream(ctx context.Context, agent string) (Stream, error) {
	st, err := s.c.Stream(ctx, agent)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// Options build a Service; every field but Sidecar has a working default.
type Options struct {
	Sidecar Sidecar
	Events  event.Sink
	Logger  *slog.Logger
	// Now is the clock used for ticket expiry; tests replace it.
	Now func() time.Time
	// TicketTTL overrides DefaultTicketTTL.
	TicketTTL time.Duration
	// TakeoverTimeout overrides DefaultTakeoverTimeout.
	TakeoverTimeout time.Duration
}

// Service owns the tickets, the takeover lock and the stream relays. It is
// safe for concurrent use.
type Service struct {
	sidecar         Sidecar
	events          event.Sink
	log             *slog.Logger
	now             func() time.Time
	ticketTTL       time.Duration
	takeoverTimeout time.Duration

	mu      sync.Mutex
	tickets map[string]ticket
	agents  map[string]*agentState
	relays  map[string]*relay
}

var _ api.BrowserBackend = (*Service)(nil)

// New returns a service over a sidecar.
func New(o Options) *Service {
	s := &Service{
		sidecar:         o.Sidecar,
		events:          o.Events,
		log:             o.Logger,
		now:             o.Now,
		ticketTTL:       o.TicketTTL,
		takeoverTimeout: o.TakeoverTimeout,
		tickets:         map[string]ticket{},
		agents:          map[string]*agentState{},
		relays:          map[string]*relay{},
	}
	if s.events == nil {
		s.events = event.Discard
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.ticketTTL <= 0 {
		s.ticketTTL = DefaultTicketTTL
	}
	if s.takeoverTimeout <= 0 {
		s.takeoverTimeout = DefaultTakeoverTimeout
	}
	return s
}

// --- tickets ------------------------------------------------------------------

type ticket struct {
	agent      string
	identityID event.ID
	expires    time.Time
}

// IssueTicket mints a single-use ticket for one agent's live view. The value
// is 32 random bytes: it is a credential, and it travels in a URL.
func (s *Service) IssueTicket(agent string, identityID event.ID) (Ticket, error) {
	raw := make([]byte, ticketBytes)
	if _, err := rand.Read(raw); err != nil {
		return Ticket{}, fmt.Errorf("browser live: ticket: %w", err)
	}
	value := base64.RawURLEncoding.EncodeToString(raw)
	expires := s.now().Add(s.ticketTTL).UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepTickets()
	s.tickets[value] = ticket{agent: agent, identityID: identityID, expires: expires}
	return Ticket{Value: value, ExpiresAt: expires}, nil
}

// RedeemTicket consumes a ticket. Unknown, used and expired values are one
// answer, and the comparison runs over every live ticket so the time it takes
// says nothing about which one matched.
func (s *Service) RedeemTicket(value string) (string, event.ID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepTickets()
	var (
		found string
		hit   ticket
		ok    bool
	)
	want := []byte(value)
	for k, t := range s.tickets {
		if subtle.ConstantTimeCompare([]byte(k), want) == 1 {
			found, hit, ok = k, t, true
		}
	}
	if !ok || value == "" {
		return "", "", false
	}
	delete(s.tickets, found)
	return hit.agent, hit.identityID, true
}

// sweepTickets drops what has expired. Callers hold mu.
func (s *Service) sweepTickets() {
	now := s.now()
	for k, t := range s.tickets {
		if !now.Before(t.expires) {
			delete(s.tickets, k)
		}
	}
}

// --- sessions -------------------------------------------------------------------

// Sessions lists the running browsers with what only the daemon knows: the
// holder, the watchers and a pending takeover request.
func (s *Service) Sessions(ctx context.Context) ([]Status, error) {
	sessions, err := s.sidecar.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(sessions))
	for _, sess := range sessions {
		st := Status{Agent: sess.Agent, Started: sess.Started}
		for _, t := range sess.Tabs {
			st.Tabs = append(st.Tabs, api.BrowserTab{ID: t.ID, URL: t.URL, Title: t.Title, Active: t.Active})
		}
		s.mu.Lock()
		if a, ok := s.agents[sess.Agent]; ok {
			st.Holder, st.TakeoverRequested = a.holder, string(a.requested)
		}
		if r, ok := s.relays[sess.Agent]; ok {
			st.Watchers = r.count()
		}
		s.mu.Unlock()
		out = append(out, st)
	}
	return out, nil
}

// --- the takeover lock ------------------------------------------------------------

// agentState is one agent's lock. changed is closed and replaced on every
// change, so waiters can watch without a condition variable.
type agentState struct {
	holder     event.ID
	lastHolder event.ID
	// handbacks counts completed takeover cycles: a person took the lock and
	// gave it back. RequestTakeover waits for this to move.
	handbacks int
	requested browser.Blocker
	changed   chan struct{}
}

// bump wakes everyone waiting on a state change. Callers hold mu.
func (a *agentState) bump() {
	close(a.changed)
	a.changed = make(chan struct{})
}

// state returns the agent's lock, creating it on first use. Callers hold mu.
func (s *Service) state(agent string) *agentState {
	a, ok := s.agents[agent]
	if !ok {
		a = &agentState{changed: make(chan struct{})}
		s.agents[agent] = a
	}
	return a
}

// Takeover takes the lock for identityID. The executor suspends the run while it
// is held; only this user's input is dispatched.
func (s *Service) Takeover(ctx context.Context, agent string, identityID event.ID) error {
	if err := s.requireSession(ctx, agent); err != nil {
		return err
	}
	s.mu.Lock()
	a := s.state(agent)
	if a.holder != "" && a.holder != identityID {
		s.mu.Unlock()
		return ErrHeld
	}
	taken := a.holder == ""
	a.holder, a.lastHolder = identityID, identityID
	a.bump()
	s.mu.Unlock()
	if !taken {
		return nil // already this user's: taking twice is not an event
	}
	s.emit(ctx, event.BrowserTakeoverStarted, "", "", map[string]any{
		"agent": agent, "user_id": string(identityID),
	})
	s.holderChanged(agent)
	return nil
}

// Release hands the browser back. It is idempotent for the holder and refused
// for anyone else.
func (s *Service) Release(ctx context.Context, agent string, identityID event.ID) error {
	s.mu.Lock()
	a := s.state(agent)
	switch {
	case a.holder == identityID && identityID != "":
	case a.holder == "" && a.lastHolder == identityID && identityID != "":
		s.mu.Unlock()
		return nil // already handed back by this user
	default:
		s.mu.Unlock()
		return ErrNotHolder
	}
	a.holder = ""
	a.handbacks++
	a.bump()
	s.mu.Unlock()
	s.emit(ctx, event.BrowserTakeoverEnded, "", "", map[string]any{
		"agent": agent, "user_id": string(identityID),
	})
	s.holderChanged(agent)
	return nil
}

// Holder is who holds the lock, or "".
func (s *Service) Holder(agent string) event.ID {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.agents[agent]; ok {
		return a.holder
	}
	return ""
}

// Held reports whether a person holds the browser. The executor checks it
// before every step.
func (s *Service) Held(agent string) bool { return s.Holder(agent) != "" }

// WaitReleased blocks while a person holds the browser, so a running task
// pauses instead of fighting them for the page.
func (s *Service) WaitReleased(ctx context.Context, agent string) error {
	for {
		s.mu.Lock()
		a := s.state(agent)
		held, changed := a.holder != "", a.changed
		s.mu.Unlock()
		if !held {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// RequestTakeover asks a person to get past a blocker and hand the browser
// back; it is the building block of browser.Humans.Takeover. It returns true
// once a takeover cycle has completed — taken and released — and false when
// nobody did within TakeoverTimeout. A lock already held when the request is
// made counts as soon as it is released.
func (s *Service) RequestTakeover(ctx context.Context, agent string, why browser.Blocker, traceID, sessionID event.ID) (bool, error) {
	expires := s.now().Add(s.takeoverTimeout).UTC()
	s.mu.Lock()
	a := s.state(agent)
	base := a.handbacks
	a.requested = why
	a.bump()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if a.requested == why {
			a.requested = ""
			a.bump()
		}
		s.mu.Unlock()
	}()

	// Shown to owner and admins like an approval; `why` is a closed set and
	// nothing about the page goes with it.
	s.emit(ctx, event.BrowserTakeoverRequest, traceID, sessionID, map[string]any{
		"agent": agent, "why": string(why), "expires_at": expires,
	})

	timer := time.NewTimer(s.takeoverTimeout)
	defer timer.Stop()
	for {
		s.mu.Lock()
		done, changed := a.handbacks > base, a.changed
		s.mu.Unlock()
		if done {
			return true, nil
		}
		select {
		case <-changed:
		case <-timer.C:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// requireSession refuses a lock on an agent that has no browser running.
func (s *Service) requireSession(ctx context.Context, agent string) error {
	sessions, err := s.sidecar.Sessions(ctx)
	if err != nil {
		return err
	}
	for _, sess := range sessions {
		if sess.Agent == agent {
			return nil
		}
	}
	return ErrNoSession
}

// emit sends one trace event. Attrs are fixed words, an agent and a user id:
// never page text, never input.
func (s *Service) emit(ctx context.Context, typ event.Type, traceID, sessionID event.ID, attrs map[string]any) {
	s.events.Emit(ctx, event.New(typ, traceID, sessionID, "", attrs))
}

// Watchers is how many live-view streams are open on the agent's browser:
// zero means nobody could take over right now.
func (s *Service) Watchers(agent string) int {
	s.mu.Lock()
	r, ok := s.relays[agent]
	s.mu.Unlock()
	if !ok {
		return 0
	}
	return r.count()
}
