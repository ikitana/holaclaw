package task

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/api"
	"holaclaw/internal/approval"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

var _ api.TaskBackend = (*Service)(nil)

// clock is the injectable time source: no test ever sleeps.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *clock { return &clock{t: t} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *clock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

// submitted is one recorded run start.
type submitted struct {
	Session store.Session
	Message event.Message
}

type recorder struct {
	mu   sync.Mutex
	runs []submitted
	err  error
}

func (r *recorder) submit(_ context.Context, sess store.Session, msg event.Message) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return false, r.err
	}
	r.runs = append(r.runs, submitted{Session: sess, Message: msg})
	return false, nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.runs)
}

func (r *recorder) all() []submitted {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]submitted(nil), r.runs...)
}

// sink records the trace events the service emits.
type sink struct {
	mu     sync.Mutex
	events []event.Event
}

func (s *sink) Emit(_ context.Context, e event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *sink) typed(t event.Type) []event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []event.Event
	for _, e := range s.events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func (s *sink) has(t event.Type) bool { return len(s.typed(t)) > 0 }

type fixture struct {
	svc      *Service
	store    *store.SQLite
	grants   *approval.Service
	runs     *recorder
	events   *sink
	clock    *clock
	busy     map[event.ID]bool
	busyMu   sync.Mutex
	tools    []string
	agents   []string
	location *time.Location
}

// baseTime is a fixed, DST-free instant every test starts from.
var baseTime = time.Date(2026, 5, 4, 8, 0, 0, 0, time.UTC)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := t.Context()
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{
		store: st, runs: &recorder{}, events: &sink{}, clock: newClock(baseTime),
		busy: map[event.ID]bool{}, tools: []string{"files.write", "memory.save", "fetch"},
		agents: []string{"main", "other"}, location: time.UTC,
	}
	f.grants = approval.New(st.DB(), f.events, approval.WithNow(f.clock.Now))
	svc, err := New(Deps{
		DB: st.DB(), Store: st, Submit: f.runs.submit, Grants: f.grants,
		Busy: func(id event.ID) bool {
			f.busyMu.Lock()
			defer f.busyMu.Unlock()
			return f.busy[id]
		},
		Agents: func(name string) bool {
			for _, a := range f.agents {
				if a == name {
					return true
				}
			}
			return false
		},
		KnownTools: func(string) []string { return f.tools },
		Events:     f.events, Now: f.clock.Now, Location: f.location,
		After: func(time.Duration) <-chan time.Time { return nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.svc = svc
	return f
}

// chatSession creates a chat session a task may deliver to.
// user creates a holaclaw user; sessions and tasks reference one.
func (f *fixture) user(t *testing.T, id event.ID) event.ID {
	t.Helper()
	err := f.store.CreateIdentity(t.Context(), store.Identity{
		ID: id, Name: string(id), Role: store.RoleUser, CreatedAt: f.clock.Now(),
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return id
}

func (f *fixture) chatSession(t *testing.T, user event.ID) store.Session {
	t.Helper()
	f.user(t, user)
	sess, _, err := f.store.EnsureSession(t.Context(), store.Session{
		Key: "main/tg-main/dm/" + string(user), Agent: "main", Kind: store.SessionChat, UserID: user,
		Origin: event.Origin{
			Channel: "tg-main", ChannelType: "telegram", Conversation: "c-42",
			ConversationKind: event.ConversationDM,
		},
		CreatedAt: f.clock.Now(),
	})
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	return sess
}

func (f *fixture) setBusy(id event.ID, on bool) {
	f.busyMu.Lock()
	defer f.busyMu.Unlock()
	f.busy[id] = on
}

func (f *fixture) get(t *testing.T, id event.ID) Task {
	t.Helper()
	got, err := f.svc.get(t.Context(), id)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	return got
}

func mustContain(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err, want)
	}
}

func ptr[T any](v T) *T { return &v }
