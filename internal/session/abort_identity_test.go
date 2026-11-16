package session

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/agent"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// AbortIdentity stops the turns that are the identity's alone: its own
// session's, and a room turn it is the only sender of. A room turn that also
// answers somebody else, and everyone else's turns, keep going.
func TestAbortIdentityStopsOnlyItsOwnTurns(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var (
		mu      sync.Mutex
		started = map[event.ID]bool{}
	)
	var h *harness
	h = newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		mu.Lock()
		started[in.SessionID] = true
		mu.Unlock()
		select {
		case <-ctx.Done():
			return agent.TurnResult{Stop: agent.StopAborted}, ctx.Err()
		case <-release:
			return h.reply(ctx, in, "answered"), nil
		}
	}, func(d *Deps) { d.Debounce = 60 * time.Millisecond })
	ctx := context.Background()
	alice, bob := event.NewID(), event.NewID()
	if err := h.st.CreateIdentity(ctx, store.Identity{ID: alice, Name: "alice", Role: store.RoleUser, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create identity: %v", err)
	}

	room := func(conversation string) store.Session {
		s, _, err := h.st.EnsureSession(ctx, store.Session{
			Key: "main/tg/room/" + conversation, Agent: "main", Kind: store.SessionChat,
			Origin: event.Origin{Channel: "tg", ConversationKind: event.ConversationRoom, Conversation: conversation},
		})
		if err != nil {
			t.Fatalf("ensure session: %v", err)
		}
		return s
	}
	say := func(sess store.Session, from event.ID, text string) {
		m := event.Message{
			ID: event.NewID(), TraceID: event.NewID(), Role: event.RoleUser, Text: text,
			UserID: from, Mentioned: true, CreatedAt: time.Now(),
		}
		if _, err := h.m.Submit(ctx, sess, m); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}

	alone := room("alone")
	say(alone, alice, "only me")
	shared := room("shared")
	// Unmentioned chatter coalesces into the next mention's turn.
	m := event.Message{
		ID: event.NewID(), TraceID: event.NewID(), Role: event.RoleUser, Text: "by the way",
		UserID: bob, CreatedAt: time.Now(),
	}
	if _, err := h.m.Submit(ctx, shared, m); err != nil {
		t.Fatalf("submit: %v", err)
	}
	say(shared, alice, "and me")
	bobs := h.session("main/tg/dm/bob")
	say(bobs, bob, "bob's own")
	// A task run: its message carries no sender, its session names alice.
	owned, _, err := h.st.EnsureSession(ctx, store.Session{
		Key: "main/task/1", Agent: "main", Kind: store.SessionTask, UserID: alice,
		Origin: event.Origin{Channel: "task", ConversationKind: event.ConversationDM, Conversation: "1"},
	})
	if err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	h.submit(owned, "the task's prompt")

	all := []store.Session{alone, shared, bobs, owned}
	waitFor(t, "four running turns", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(started) == len(all)
	})
	if n := h.m.AbortIdentity(alice); n != 2 {
		t.Fatalf("AbortIdentity stopped %d turns, want 2", n)
	}
	waitFor(t, "the aborted turns to end", func() bool { return !h.m.Busy(alone.ID) && !h.m.Busy(owned.ID) })
	for _, s := range []store.Session{shared, bobs} {
		if !h.m.Busy(s.ID) {
			t.Fatalf("session %s stopped, want it still running", s.Key)
		}
	}
	if n := h.m.AbortIdentity(""); n != 0 {
		t.Fatalf("AbortIdentity(\"\") stopped %d turns", n)
	}

	close(release)
	waitFor(t, "the two others to answer", func() bool { return len(h.deliveries()) == 2 })
	got := map[event.ID]bool{}
	for _, d := range h.deliveries() {
		got[d.SessionID] = true
	}
	if !got[shared.ID] || !got[bobs.ID] {
		t.Fatalf("delivered to %v, want the shared room and bob's DM", got)
	}
}

// A turn whose Prepare reports its senders disabled runs nothing and answers
// nothing, not even the apology: it is evented as aborted.
func TestDisabledSendersTurnIsDropped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		return agent.TurnResult{Stop: agent.StopFinal}, nil
	}, func(d *Deps) {
		d.Prepare = func(context.Context, store.Session, []event.Message) (agent.TurnInput, error) {
			return agent.TurnInput{}, fmt.Errorf("wrapped: %w", ErrIdentityDisabled)
		}
	})
	sess := h.session("main/tg/dm/u1")
	sent := h.submit(sess, "queued before the disable")
	waitFor(t, "run.aborted", func() bool { return len(h.ev.of(event.RunAborted)) == 1 })
	ab := h.ev.of(event.RunAborted)[0]
	if ab.TraceID != sent.TraceID || string(ab.Attrs) != `{"reason":"identity_disabled"}` {
		t.Fatalf("run.aborted = %+v %s", ab, ab.Attrs)
	}
	if h.turnCount() != 0 || len(h.deliveries()) != 0 {
		t.Fatalf("turns %d, deliveries %v: want neither", h.turnCount(), h.deliveries())
	}
	// The input was consumed, not put back for a turn after the enable.
	if left, err := h.st.SessionsWithPending(context.Background()); err != nil || len(left) != 0 {
		t.Fatalf("pending: %v, %v", left, err)
	}
}

func TestSenders(t *testing.T) {
	t.Parallel()
	a, b, owner := event.NewID(), event.NewID(), event.NewID()
	for _, tc := range []struct {
		name  string
		owner event.ID
		msgs  []event.Message
		want  []event.ID
	}{
		{"distinct, in order", owner, []event.Message{{UserID: a}, {UserID: b}, {UserID: a}}, []event.ID{a, b}},
		{"none carried: the owner", owner, []event.Message{{}}, []event.ID{owner}},
		{"none known", "", []event.Message{{}}, nil},
		{"the owner is not a sender", owner, []event.Message{{UserID: a}, {}}, []event.ID{a}},
	} {
		if got := Senders(tc.owner, tc.msgs); !slices.Equal(got, tc.want) {
			t.Errorf("%s: Senders = %v, want %v", tc.name, got, tc.want)
		}
	}
}
