package agent

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/store"
)

// cpBase is the clock every scripted transcript starts from.
var cpBase = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// cpStore mimics the store: ListMessages returns the rows with id strictly
// greater than after, ordered by id.
type cpStore struct {
	mu        sync.Mutex
	sess      store.Session
	msgs      []event.Message
	appendErr error
	updateErr error
	updates   []cpUpdate
	appends   int
}

type cpUpdate struct {
	prefixHash string
	summaryID  event.ID
	fails      int
}

func (s *cpStore) ListMessages(_ context.Context, sessionID, after event.ID, limit int) ([]event.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]event.Message, 0, len(s.msgs))
	for _, m := range s.msgs {
		if m.SessionID == sessionID && m.ID > after {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *cpStore) AppendMessage(_ context.Context, m event.Message, _ bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appendErr != nil {
		return false, s.appendErr
	}
	for _, e := range s.msgs {
		if e.ID == m.ID {
			return false, nil
		}
	}
	s.appends++
	s.msgs = append(s.msgs, m)
	return true, nil
}

func (s *cpStore) GetSession(_ context.Context, id event.ID) (store.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess.ID != id {
		return store.Session{}, store.ErrNotFound
	}
	return s.sess, nil
}

func (s *cpStore) UpdateSessionContext(_ context.Context, id event.ID, prefixHash string, summaryMsgID event.ID, fails int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateErr != nil {
		return s.updateErr
	}
	if s.sess.ID != id {
		return store.ErrNotFound
	}
	s.sess.PrefixHash, s.sess.SummaryMsgID, s.sess.CompactFails = prefixHash, summaryMsgID, fails
	s.updates = append(s.updates, cpUpdate{prefixHash, summaryMsgID, fails})
	return nil
}

// cpRig wires a Compactor to fakes: a scripted provider for the one summarize
// call, the in-memory store above and an event recorder.
type cpRig struct {
	st   *cpStore
	prov *fakeProvider
	ev   *recorder
	c    *Compactor
	in   CompactInput
}

const cpSession = event.ID("01HZZZZZZZZZZZZZZZZZZZZZZZ")

func newCompactRig(_ *testing.T, steps ...step) *cpRig {
	st := &cpStore{sess: store.Session{ID: cpSession, Key: "k", Agent: "test", PrefixHash: "pfx"}}
	prov := &fakeProvider{name: "sum", steps: steps}
	ev := &recorder{}
	clock := cpBase.Add(time.Hour)
	return &cpRig{
		st: st, prov: prov, ev: ev,
		c: &Compactor{
			Store:     st,
			Summarize: ModelRef{Model: provider.Model{Name: "sum", Provider: "fake", Context: 8192}, Provider: prov},
			Events:    ev,
			Now: func() time.Time {
				clock = clock.Add(time.Millisecond)
				return clock
			},
		},
		in: CompactInput{
			SessionID: cpSession,
			TraceID:   "T1",
			Def:       Definition{Name: "test"},
			Model:     provider.Model{Name: "chat", Context: 4096},
		},
	}
}

// add appends messages to the fake store, spacing their ids one millisecond
// apart so they sort in order and are never adjacent.
func (g *cpRig) add(msgs ...event.Message) {
	g.st.mu.Lock()
	defer g.st.mu.Unlock()
	g.st.msgs = append(g.st.msgs, msgs...)
}

// cpAt builds one transcript message with a ULID for the given offset.
func cpAt(offset int, role event.Role, text string) event.Message {
	t := cpBase.Add(time.Duration(offset) * time.Millisecond)
	return event.Message{
		Schema:    event.SchemaVersion,
		ID:        event.NewIDAt(t),
		SessionID: cpSession,
		TraceID:   "T0",
		Role:      role,
		Text:      text,
		CreatedAt: t,
	}
}

// cpFiller returns text costing about n tokens.
func cpFiller(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSpace(strings.Repeat("abcd ", n))
}

// cpChat builds an alternating user/assistant transcript of n messages, each
// about size tokens.
func cpChat(n, size int) []event.Message {
	out := make([]event.Message, 0, n)
	for i := 0; i < n; i++ {
		role := event.RoleUser
		if i%2 == 1 {
			role = event.RoleAssistant
		}
		out = append(out, cpAt(i, role, cpFiller(size)))
	}
	return out
}

// cpSummaryStep scripts the single summarize call.
func cpSummaryStep(text string) step {
	return step{resp: &provider.Response{Text: text, Finish: provider.FinishStop}}
}
