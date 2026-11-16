package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/agent"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

var errTest = errors.New("no route to platform")

// ---- fakes -----------------------------------------------------------------

// sink collects emitted events.
type sink struct {
	mu sync.Mutex
	ev []event.Event
}

func (s *sink) Emit(_ context.Context, e event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ev = append(s.ev, e)
}

func (s *sink) of(t event.Type) []event.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []event.Event
	for _, e := range s.ev {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

// turnFunc is the scripted body of a fake Turner.
type turnFunc func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error)

func (f turnFunc) Turn(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
	return f(ctx, in)
}

// harness wires a manager onto a real SQLite store with a scripted Turner.
type harness struct {
	t  *testing.T
	st *store.SQLite
	m  *Manager
	ev *sink

	mu         sync.Mutex
	turns      []agent.TurnInput
	delivered  []event.Message
	deliverErr error
}

func newHarness(t *testing.T, turn turnFunc, opts ...func(*Deps)) *harness {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &harness{t: t, st: st, ev: &sink{}}
	d := Deps{
		Store:    st,
		Events:   h.ev,
		Debounce: -1, // no coalescing window unless a test asks for one
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Prepare: func(_ context.Context, _ store.Session, _ []event.Message) (agent.TurnInput, error) {
			return agent.TurnInput{}, nil
		},
		Runner: func(store.Session) (Turner, error) {
			return turnFunc(func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
				h.mu.Lock()
				h.turns = append(h.turns, in)
				h.mu.Unlock()
				return turn(ctx, in)
			}), nil
		},
		Deliver: func(_ context.Context, _ store.Session, _ event.ID, reply event.Message) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.deliverErr != nil {
				return h.deliverErr
			}
			h.delivered = append(h.delivered, reply)
			return nil
		},
	}
	for _, o := range opts {
		o(&d)
	}
	h.m = New(d)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.m.Shutdown(ctx)
	})
	return h
}

func (h *harness) session(key string) store.Session {
	h.t.Helper()
	s, _, err := h.st.EnsureSession(context.Background(), store.Session{
		Key: key, Agent: "main", Kind: store.SessionChat,
		Origin: event.Origin{Channel: "tg", ConversationKind: event.ConversationDM, Conversation: "c1"},
	})
	if err != nil {
		h.t.Fatalf("ensure session: %v", err)
	}
	return s
}

func (h *harness) taskSession(key string) store.Session {
	h.t.Helper()
	s, _, err := h.st.EnsureSession(context.Background(), store.Session{
		Key: key, Agent: "main", Kind: store.SessionTask,
	})
	if err != nil {
		h.t.Fatalf("ensure session: %v", err)
	}
	return s
}

func (h *harness) submit(sess store.Session, text string) event.Message {
	h.t.Helper()
	m := event.Message{
		ID: event.NewID(), TraceID: event.NewID(), Role: event.RoleUser, Text: text,
		CreatedAt: time.Now(),
	}
	if _, err := h.m.Submit(context.Background(), sess, m); err != nil {
		h.t.Fatalf("submit: %v", err)
	}
	return m
}

func (h *harness) turnCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.turns)
}

func (h *harness) turnAt(i int) agent.TurnInput {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.turns[i]
}

func (h *harness) deliveries() []event.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]event.Message(nil), h.delivered...)
}

// reply stores an assistant message and returns it as the turn's reply.
func (h *harness) reply(ctx context.Context, in agent.TurnInput, text string) agent.TurnResult {
	m := event.Message{
		Schema: event.SchemaVersion, ID: event.NewID(), SessionID: in.SessionID, TraceID: in.TraceID,
		Role: event.RoleAssistant, Text: text, CreatedAt: time.Now(),
	}
	// A turn cut short (an abort, or the harness shutting down under a test
	// that did not wait for it) fails here by design, not by mistake.
	if _, err := h.st.AppendMessage(ctx, m, false); err != nil && ctx.Err() == nil {
		h.t.Errorf("append reply: %v", err)
	}
	return agent.TurnResult{Reply: m, New: []event.Message{m}, Stop: agent.StopFinal}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// ---- keys ------------------------------------------------------------------

func TestKey(t *testing.T) {
	t.Parallel()
	dm := event.Origin{Channel: "tg-main", ConversationKind: event.ConversationDM, Conversation: "42"}
	room := event.Origin{Channel: "tg-main", ConversationKind: event.ConversationRoom, Conversation: "-100"}
	thread := room
	thread.Thread = "7"
	tests := []struct {
		name   string
		agent  string
		origin event.Origin
		user   event.ID
		want   string
	}{
		{"dm with user", "main", dm, "U1", "main/tg-main/dm/U1"},
		{"dm without user", "main", dm, "", "main/tg-main/dm/42"},
		{"holaclaw", "main", event.Origin{
			Channel: event.ChannelHolaclaw, ChannelType: event.ChannelHolaclaw, ConversationKind: event.ConversationDM, Conversation: "c/1",
		}, "U1", "main/holaclaw/dm/U1/c%2F1"},
		{"room", "main", room, "U1", "main/tg-main/room/-100"},
		{"room thread", "main", thread, "U1", "main/tg-main/room/-100/7"},
		{"escapes separator", "main", event.Origin{
			Channel: "tg/x", ConversationKind: event.ConversationRoom, Conversation: "a/b", Thread: "t",
		}, "U1", "main/tg%2Fx/room/a%2Fb/t"},
		{"escapes escape char", "ma%in", dm, "u%2Fx", "ma%25in/tg-main/dm/u%252Fx"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Key(tc.agent, tc.origin, tc.user); got != tc.want {
				t.Fatalf("Key = %q, want %q", got, tc.want)
			}
		})
	}
}

// A forged id must not be able to produce the key of another conversation.
func TestKeyNoForging(t *testing.T) {
	t.Parallel()
	honest := Key("main", event.Origin{
		Channel: "tg", ConversationKind: event.ConversationRoom, Conversation: "g1", Thread: "t1",
	}, "")
	forged := Key("main", event.Origin{
		Channel: "tg", ConversationKind: event.ConversationRoom, Conversation: "g1/t1",
	}, "")
	if honest == forged {
		t.Fatalf("forged key collides: %q", honest)
	}
	// A dm key can likewise not be forged out of a channel name.
	if Key("main", event.Origin{Channel: "tg/dm", ConversationKind: event.ConversationDM}, "u") ==
		Key("main", event.Origin{Channel: "tg", ConversationKind: event.ConversationDM}, "u") {
		t.Fatal("channel name forged a dm key")
	}
}

// Two aliases of the same user in one channel instance share a session: the
// identity link is resolved before key derivation.
func TestKeyIdentityLinkShares(t *testing.T) {
	t.Parallel()
	a := event.Origin{Channel: "wa", ConversationKind: event.ConversationDM, Conversation: "jid@s.whatsapp.net"}
	b := event.Origin{Channel: "wa", ConversationKind: event.ConversationDM, Conversation: "lid@lid"}
	if Key("main", a, "U9") != Key("main", b, "U9") {
		t.Fatal("linked aliases got different session keys")
	}
	if Key("main", a, "U9") == Key("main", a, "U8") {
		t.Fatal("different users share a dm session")
	}
}

func TestTaskKey(t *testing.T) {
	t.Parallel()
	if got, want := TaskKey("main", "T1"), "main/task/T1"; got != want {
		t.Fatalf("TaskKey = %q, want %q", got, want)
	}
	// The reserved namespace cannot be reached by a chat key.
	if TaskKey("main", "T1") == Key("main", event.Origin{
		Channel: "task", ConversationKind: event.ConversationDM,
	}, "T1") {
		t.Fatal("chat key reached the task namespace")
	}
}

// ---- actor -----------------------------------------------------------------

// One actor, every message processed exactly once, in order.
func TestConcurrentSubmitsOneActorNoLoss(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		return h0(ctx, in), nil
	})
	sess := h.session("main/tg/dm/u1")
	const n = 100
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := event.Message{ID: event.NewID(), TraceID: event.NewID(), Role: event.RoleUser, Text: "hi"}
			if _, err := h.m.Submit(context.Background(), sess, m); err != nil {
				t.Errorf("submit: %v", err)
			}
		}()
	}
	wg.Wait()

	waitFor(t, "mailbox drained", func() bool {
		ms, err := h.st.ListMessages(context.Background(), sess.ID, "", 0)
		return err == nil && len(ms) == n && !h.m.Busy(sess.ID)
	})
	h.m.mu.Lock()
	actors := len(h.m.actors)
	h.m.mu.Unlock()
	if actors != 1 {
		t.Fatalf("actors = %d, want 1", actors)
	}
	// Every message was seen exactly once, in transcript order: each turn's
	// history is a prefix ending in that turn's new messages.
	if h.turnCount() == 0 {
		t.Fatal("no turns ran")
	}
	last := h.turnAt(h.turnCount() - 1)
	if len(last.History) != n {
		t.Fatalf("final history = %d messages, want %d", len(last.History), n)
	}
	for i := 1; i < len(last.History); i++ {
		if last.History[i-1].Pos >= last.History[i].Pos {
			t.Fatalf("history out of order at %d", i)
		}
	}
	for i := 1; i < h.turnCount(); i++ {
		if len(h.turnAt(i).History) <= len(h.turnAt(i-1).History) {
			t.Fatalf("turn %d did not grow the history", i)
		}
	}
}

func h0(context.Context, agent.TurnInput) agent.TurnResult {
	return agent.TurnResult{Stop: agent.StopFinal}
}

// Turns of different sessions overlap.
func TestParallelAcrossSessions(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	inflight, peak := 0, 0
	release := make(chan struct{})
	h := newHarness(t, func(ctx context.Context, _ agent.TurnInput) (agent.TurnResult, error) {
		mu.Lock()
		inflight++
		if inflight > peak {
			peak = inflight
		}
		mu.Unlock()
		<-release
		mu.Lock()
		inflight--
		mu.Unlock()
		return agent.TurnResult{Stop: agent.StopFinal}, nil
	})
	a, b := h.session("main/tg/dm/a"), h.session("main/tg/dm/b")
	h.submit(a, "one")
	h.submit(b, "two")
	waitFor(t, "both turns running", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return peak == 2
	})
	if h.m.Active() != 2 {
		t.Fatalf("Active = %d, want 2", h.m.Active())
	}
	close(release)
}

// Rapid messages coalesce into one turn.
func TestCoalesce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		return agent.TurnResult{Stop: agent.StopFinal}, nil
	}, func(d *Deps) { d.Debounce = 60 * time.Millisecond })
	sess := h.session("main/tg/dm/u1")
	h.submit(sess, "a")
	h.submit(sess, "b")
	h.submit(sess, "c")
	waitFor(t, "one turn", func() bool { return h.turnCount() == 1 })
	in := h.turnAt(0)
	if len(in.History) != 3 {
		t.Fatalf("history = %d messages, want the 3 coalesced ones", len(in.History))
	}
	if in.History[0].Text != "a" || in.History[2].Text != "c" {
		t.Fatalf("coalesced messages out of order: %+v", in.History)
	}
	time.Sleep(200 * time.Millisecond)
	if n := h.turnCount(); n != 1 {
		t.Fatalf("turns = %d, want 1", n)
	}
}

// In a room each mention is its own turn, answering under its own trace (so
// reply_to quotes it). With reply = "always", unmentioned messages before a
// mention ride along in its turn; with "mention" they never reach the mailbox.
func TestRoomTurnPerMention(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		return agent.TurnResult{Stop: agent.StopFinal}, nil
	}, func(d *Deps) { d.Debounce = 60 * time.Millisecond })
	sess, _, err := h.st.EnsureSession(context.Background(), store.Session{
		Key: "main/tg/room/-100", Agent: "main", Kind: store.SessionChat,
		Origin: event.Origin{Channel: "tg", ConversationKind: event.ConversationRoom, Conversation: "-100"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var sent []event.Message
	for _, in := range []struct {
		text      string
		mentioned bool
	}{{"by the way", false}, {"rain in Sevilla?", true}, {"USD-EUR?", true}} {
		m := event.Message{
			ID: event.NewID(), TraceID: event.NewID(), Role: event.RoleUser, Text: in.text,
			Mentioned: in.mentioned, CreatedAt: time.Now(),
		}
		if _, err := h.m.Submit(context.Background(), sess, m); err != nil {
			t.Fatal(err)
		}
		sent = append(sent, m)
	}
	waitFor(t, "two turns", func() bool { return h.turnCount() == 2 })
	first, second := h.turnAt(0), h.turnAt(1)
	if len(first.History) != 2 || first.TraceID != sent[1].TraceID {
		t.Fatalf("first turn: history %d, trace %s", len(first.History), first.TraceID)
	}
	if len(second.History) != 3 || second.TraceID != sent[2].TraceID || second.History[2].Text != "USD-EUR?" {
		t.Fatalf("second turn: history %+v, trace %s", second.History, second.TraceID)
	}
	// Nothing is left for a third turn.
	if left, err := h.st.SessionsWithPending(context.Background()); err != nil || len(left) != 0 {
		t.Fatalf("pending after two turns: %v, %v", left, err)
	}
}

// A room answer of NO_REPLY is stored and not delivered; in a DM it is an
// ordinary answer.
func TestRoomPassIsNotDelivered(t *testing.T) {
	t.Parallel()
	var h *harness
	h = newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		return h.reply(ctx, in, " "+agent.NoReply+"\n"), nil
	})
	room, _, err := h.st.EnsureSession(context.Background(), store.Session{
		Key: "main/tg/room/-100", Agent: "main", Kind: store.SessionChat,
		Origin: event.Origin{Channel: "tg", ConversationKind: event.ConversationRoom, Conversation: "-100"},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.submit(room, "morning all")
	waitFor(t, "the room turn", func() bool { return h.turnCount() == 1 })
	waitFor(t, "the pass stored", func() bool {
		ms, _ := h.st.ListMessages(context.Background(), room.ID, "", 0)
		return len(ms) == 2 && ms[1].Role == event.RoleAssistant
	})

	h.submit(h.session("main/tg/dm/u1"), "say nothing")
	waitFor(t, "the dm delivery", func() bool { return len(h.deliveries()) == 1 })
	if got := h.deliveries(); len(got) != 1 || !agent.Passed(got[0]) {
		t.Fatalf("deliveries = %+v, want only the dm one", got)
	}
}

// The debounce window is capped, so a steady trickle still runs.
func TestCoalesceCapped(t *testing.T) {
	t.Parallel()
	const d = 40 * time.Millisecond
	h := newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		return agent.TurnResult{Stop: agent.StopFinal}, nil
	}, func(dep *Deps) { dep.Debounce = d })
	sess := h.session("main/tg/dm/u1")
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			m := event.Message{ID: event.NewID(), TraceID: event.NewID(), Role: event.RoleUser}
			_, _ = h.m.Submit(context.Background(), sess, m)
			time.Sleep(d / 3)
		}
	}()
	waitFor(t, "a turn despite the trickle", func() bool { return h.turnCount() > 0 })
	close(stop)
}

// A message arriving mid-run waits for the next turn and never interrupts.
func TestMessageMidRun(t *testing.T) {
	t.Parallel()
	started, release := make(chan struct{}, 1), make(chan struct{})
	h := newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		if len(in.History) == 1 {
			started <- struct{}{}
			<-release
		}
		return agent.TurnResult{Stop: agent.StopFinal}, nil
	})
	sess := h.session("main/tg/dm/u1")
	h.submit(sess, "first")
	<-started
	h.submit(sess, "second")
	time.Sleep(20 * time.Millisecond)
	if n := h.turnCount(); n != 1 {
		t.Fatalf("turns = %d while one was running, want 1", n)
	}
	close(release)
	waitFor(t, "second turn", func() bool { return h.turnCount() == 2 })
	first, second := h.turnAt(0), h.turnAt(1)
	if len(first.History) != 1 || first.History[0].Text != "first" {
		t.Fatalf("first turn history = %+v", first.History)
	}
	if len(second.History) != 2 || second.History[1].Text != "second" {
		t.Fatalf("second turn history = %+v", second.History)
	}
	if second.TraceID != second.History[1].TraceID {
		t.Fatal("turn trace id is not the trace of the last pending message")
	}
	if second.SessionID != sess.ID {
		t.Fatal("session id not stamped on the turn input")
	}
}

// Abort cancels the running turn only; pending messages run next.
func TestAbort(t *testing.T) {
	t.Parallel()
	started := make(chan struct{}, 1)
	var h *harness
	h = newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		if len(in.History) == 1 {
			started <- struct{}{}
			<-ctx.Done()
			return agent.TurnResult{Stop: agent.StopAborted}, ctx.Err()
		}
		return h.reply(ctx, in, "done"), nil
	})
	sess := h.session("main/tg/dm/u1")
	h.submit(sess, "slow")
	<-started
	h.submit(sess, "next")
	if !h.m.Abort(sess.ID) {
		t.Fatal("Abort reported no running turn")
	}
	waitFor(t, "second turn", func() bool { return h.turnCount() == 2 })
	waitFor(t, "reply delivered", func() bool { return len(h.deliveries()) == 1 })
	if got := h.deliveries()[0].Text; got != "done" {
		t.Fatalf("delivered %q, want the second turn's reply", got)
	}
	if h.m.Abort("nope") {
		t.Fatal("Abort on an unknown session reported true")
	}
}

// A panicking turn is recovered: run.aborted is evented and the actor lives.
func TestPanicRecovery(t *testing.T) {
	t.Parallel()
	var h *harness
	h = newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		if in.History[len(in.History)-1].Text == "boom" {
			panic("boom")
		}
		return h.reply(ctx, in, "fine"), nil
	})
	sess := h.session("main/tg/dm/u1")
	h.submit(sess, "boom")
	waitFor(t, "run.aborted", func() bool { return len(h.ev.of(event.RunAborted)) == 1 })
	ab := h.ev.of(event.RunAborted)[0]
	if ab.SessionID != sess.ID || ab.TraceID == "" {
		t.Fatalf("run.aborted missing ids: %+v", ab)
	}
	if string(ab.Attrs) != `{"reason":"panic"}` {
		t.Fatalf("run.aborted attrs = %s", ab.Attrs)
	}
	// The turn row was released and the actor still works.
	h.submit(sess, "again")
	waitFor(t, "next reply", func() bool { return len(h.deliveries()) == 1 })
	// Delivery happens inside the turn; the token is released after it.
	waitFor(t, "turn token cleared", func() bool {
		s, err := h.st.GetSession(context.Background(), sess.ID)
		return err == nil && s.TurnToken == ""
	})
}

// Submitting the same message id twice stores one row and runs one turn.
func TestDuplicateSubmit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		return agent.TurnResult{Stop: agent.StopFinal}, nil
	})
	sess := h.session("main/tg/dm/u1")
	m := event.Message{ID: event.NewID(), TraceID: event.NewID(), Role: event.RoleUser, Text: "hi"}
	dup, err := h.m.Submit(context.Background(), sess, m)
	if err != nil || dup {
		t.Fatalf("first submit: dup=%v err=%v", dup, err)
	}
	waitFor(t, "first turn", func() bool { return h.turnCount() == 1 })
	dup, err = h.m.Submit(context.Background(), sess, m)
	if err != nil || !dup {
		t.Fatalf("second submit: dup=%v err=%v, want duplicate", dup, err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := h.turnCount(); n != 1 {
		t.Fatalf("turns = %d after a duplicate, want 1", n)
	}
	ms, err := h.st.ListMessages(context.Background(), sess.ID, "", 0)
	if err != nil || len(ms) != 1 {
		t.Fatalf("messages = %d (%v), want 1", len(ms), err)
	}
}

// A restart replays the mailbox instead of losing input.
func TestRestartReplay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	blocked := make(chan struct{}, 1)
	var first *Manager
	// The runner blocks until the manager is shut down, so the message is
	// still pending when the process "dies".
	first = New(Deps{
		Store: st, Debounce: -1, Logger: quiet,
		Runner: func(store.Session) (Turner, error) {
			blocked <- struct{}{}
			<-first.ctx.Done()
			return nil, first.ctx.Err()
		},
	})
	sess, _, err := st.EnsureSession(context.Background(), store.Session{Key: "main/tg/dm/u1", Agent: "main"})
	if err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	msg := event.Message{ID: event.NewID(), TraceID: event.NewID(), Role: event.RoleUser, Text: "replay me"}
	if _, err := first.Submit(context.Background(), sess, msg); err != nil {
		t.Fatalf("submit: %v", err)
	}
	<-blocked
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := first.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if _, err := first.Submit(context.Background(), sess, event.Message{ID: event.NewID()}); err == nil {
		t.Fatal("Submit after Shutdown was accepted")
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Fresh process: same store, same pending row.
	st2, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = st2.Close() }()
	seen := make(chan agent.TurnInput, 1)
	second := New(Deps{
		Store: st2, Debounce: -1, Logger: quiet,
		Runner: func(store.Session) (Turner, error) {
			return turnFunc(func(_ context.Context, in agent.TurnInput) (agent.TurnResult, error) {
				seen <- in
				return agent.TurnResult{Stop: agent.StopFinal}, nil
			}), nil
		},
	})
	defer func() { _ = second.Shutdown(context.Background()) }()
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case in := <-seen:
		if len(in.History) != 1 || in.History[0].ID != msg.ID {
			t.Fatalf("replayed history = %+v", in.History)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending message was not replayed")
	}
}

// An idle actor leaves the registry, and a Submit racing that exit is never
// lost: the hook parks the actor inside the exit window.
func TestIdleExitAndLostWakeup(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		return agent.TurnResult{Stop: agent.StopFinal}, nil
	}, func(d *Deps) { d.IdleTimeout = 20 * time.Millisecond })
	sess := h.session("main/tg/dm/u1")

	// Plain idle exit.
	h.submit(sess, "one")
	waitFor(t, "first turn", func() bool { return h.turnCount() == 1 })
	waitFor(t, "actor exit", func() bool {
		h.m.mu.Lock()
		defer h.m.mu.Unlock()
		return len(h.m.actors) == 0
	})

	// Now the race: hold the actor just before it deletes itself, submit, let
	// it go. The message must still be processed.
	inWindow, proceed := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.m.hookIdleExit = func() {
		once.Do(func() {
			close(inWindow)
			<-proceed
		})
	}
	h.submit(sess, "two")
	waitFor(t, "second turn", func() bool { return h.turnCount() == 2 })
	<-inWindow
	h.submit(sess, "three")
	close(proceed)
	waitFor(t, "third turn", func() bool { return h.turnCount() == 3 })
	if got := h.turnAt(2).History; got[len(got)-1].Text != "three" {
		t.Fatalf("lost wake-up: last turn history = %+v", got)
	}
}

// Task turns never consume interactive slots.
func TestLanesAreSeparate(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	var mu sync.Mutex
	running := map[string]bool{}
	h := newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		mu.Lock()
		running[string(in.SessionID)] = true
		mu.Unlock()
		<-release
		return agent.TurnResult{Stop: agent.StopFinal}, nil
	}, func(d *Deps) {
		d.LaneCaps = map[Lane]int{LaneTask: 1, LaneInteractive: 2}
		d.ProviderOf = func(store.Session) string { return "p1" }
	})
	t1, t2 := h.taskSession("main/task/T1"), h.taskSession("main/task/T2")
	chat := h.session("main/tg/dm/u1")
	h.submit(t1, "a")
	h.submit(t2, "b")
	h.submit(chat, "c")
	waitFor(t, "one task and the chat turn running", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return running[string(chat.ID)] && (running[string(t1.ID)] || running[string(t2.ID)])
	})
	mu.Lock()
	both := running[string(t1.ID)] && running[string(t2.ID)]
	mu.Unlock()
	if both {
		t.Fatal("task lane cap of 1 was exceeded")
	}
	close(release)
	waitFor(t, "all turns", func() bool { return h.turnCount() == 3 })
}

// A delivery failure is logged and evented, and the actor keeps working.
func TestDeliveryError(t *testing.T) {
	t.Parallel()
	var h *harness
	h = newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		return h.reply(ctx, in, "hello"), nil
	})
	h.mu.Lock()
	h.deliverErr = errTest
	h.mu.Unlock()
	sess := h.session("main/tg/dm/u1")
	h.submit(sess, "hi")
	waitFor(t, "delivery.degraded", func() bool { return len(h.ev.of(event.DeliveryDegraded)) == 1 })
	e := h.ev.of(event.DeliveryDegraded)[0]
	if e.SessionID != sess.ID || e.TraceID == "" || e.MessageID == "" {
		t.Fatalf("delivery.degraded missing ids: %+v", e)
	}
	h.mu.Lock()
	h.deliverErr = nil
	h.mu.Unlock()
	h.submit(sess, "again")
	waitFor(t, "recovered delivery", func() bool { return len(h.deliveries()) == 1 })
}

// A failed turn delivers a short apology, stored as an assistant message.
func TestTurnErrorApology(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		return agent.TurnResult{Stop: agent.StopError}, errTest
	})
	sess := h.session("main/tg/dm/u1")
	h.submit(sess, "hi")
	waitFor(t, "apology delivered", func() bool { return len(h.deliveries()) == 1 })
	got := h.deliveries()[0]
	if got.Text != apologyText || got.Role != event.RoleAssistant {
		t.Fatalf("delivered %+v, want the apology", got)
	}
	ms, err := h.st.ListMessages(context.Background(), sess.ID, "", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ms) != 2 || ms[1].Text != apologyText {
		t.Fatalf("transcript = %+v, want the apology appended", ms)
	}
	if len(h.ev.of(event.ModelError)) != 0 {
		t.Fatal("manager duplicated model.error")
	}
}

// A crash leftover is healed and the turn retried once.
func TestBeginTurnHeals(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		return agent.TurnResult{Stop: agent.StopFinal}, nil
	})
	sess := h.session("main/tg/dm/u1")
	if _, _, err := h.st.BeginTurn(context.Background(), sess.ID); err != nil {
		t.Fatalf("begin turn: %v", err)
	}
	h.submit(sess, "hi")
	waitFor(t, "turn after healing", func() bool { return h.turnCount() == 1 })
}

// Shutdown cancels the running turn and waits for the actor.
func TestShutdownWaits(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	done := make(chan struct{})
	h := newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		close(started)
		<-ctx.Done()
		time.Sleep(20 * time.Millisecond)
		close(done)
		return agent.TurnResult{Stop: agent.StopAborted}, ctx.Err()
	})
	sess := h.session("main/tg/dm/u1")
	h.submit(sess, "hi")
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.m.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case <-done:
	default:
		t.Fatal("Shutdown returned before the turn finished")
	}
	if h.m.Active() != 0 {
		t.Fatalf("Active = %d after shutdown", h.m.Active())
	}
	if len(h.deliveries()) != 0 {
		t.Fatal("an aborted turn delivered something")
	}
}

func TestKeyClientConversationsAreParallel(t *testing.T) {
	a := event.Origin{Channel: event.ChannelHolaclaw, ChannelType: event.ChannelHolaclaw, Conversation: "c1", ConversationKind: event.ConversationDM}
	b := a
	b.Conversation = "c2"
	if Key("main", a, "U1") == Key("main", b, "U1") {
		t.Fatal("two holaclaw conversations of one user share a session")
	}
	if Key("main", a, "U1") == Key("main", a, "U2") {
		t.Fatal("two users share a session by choosing the same conversation id")
	}
}
