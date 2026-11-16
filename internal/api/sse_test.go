package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// session creates a session directly in the store, for stream tests that do
// not care how it got there.
func (h *harness) session(t *testing.T, channel string, user event.ID) store.Session {
	t.Helper()
	origin := event.Origin{
		Channel: channel, ChannelType: channel, Conversation: string(event.NewID()),
		ConversationKind: event.ConversationDM,
	}
	s, _, err := h.st.EnsureSession(context.Background(), store.Session{
		Key: "main/" + channel + "/dm/" + origin.Conversation, Agent: "main",
		Kind: store.SessionChat, UserID: user, Origin: origin,
	})
	if err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	return s
}

// TestSSEResumeHasNoGapAndNoDuplicate: N events, disconnect, M more, reconnect
// with Last-Event-ID — exactly the M arrive, in order, once.
func TestSSEResumeHasNoGapAndNoDuplicate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.session(t, event.ChannelHolaclaw, h.admin.ID)

	live := h.stream(h.adminToken, "", "")
	var first []event.ID
	for i := 0; i < 3; i++ {
		first = append(first, h.emit(sess.ID, event.RunStarted))
	}
	for _, want := range first {
		if f := live.next(t, 2*time.Second); f.ID != string(want) {
			t.Fatalf("live id = %s, want %s", f.ID, want)
		}
	}
	live.close()

	// Nobody is listening for these.
	var missed []event.ID
	for i := 0; i < 2; i++ {
		missed = append(missed, h.emit(sess.ID, event.RunFinished))
	}

	resumed := h.stream(h.adminToken, "", string(first[len(first)-1]))
	for _, want := range missed {
		f := resumed.next(t, 2*time.Second)
		if f.ID != string(want) {
			t.Fatalf("replayed id = %s, want %s", f.ID, want)
		}
	}
	// Nothing is replayed twice and nothing from before the resume point.
	resumed.quiet(t, 300*time.Millisecond)

	// The stream continues live from there, still without duplicates.
	next := h.emit(sess.ID, event.RunFinished)
	if f := resumed.next(t, 2*time.Second); f.ID != string(next) {
		t.Fatalf("id after the switch to live = %s, want %s", f.ID, next)
	}
}

// TestSSEResumeAcrossManyPages exercises the paging loop of the replay.
func TestSSEResumeAcrossManyPages(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.session(t, event.ChannelHolaclaw, h.admin.ID)

	start := h.emit(sess.ID, event.RunStarted)
	total := replayPage + 5
	var ids []event.ID
	for i := 0; i < total; i++ {
		ids = append(ids, h.emit(sess.ID, event.ModelProgress))
	}
	resumed := h.stream(h.adminToken, "", string(start))
	for i, want := range ids {
		f := resumed.next(t, 5*time.Second)
		if f.ID != string(want) {
			t.Fatalf("event %d: id = %s, want %s", i, f.ID, want)
		}
	}
	resumed.quiet(t, 300*time.Millisecond)
}

// TestSSEFocusSlimsOtherSessions: full events for the focused session, a
// notification for the rest (docs/reference/api.md).
func TestSSEFocusSlimsOtherSessions(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	focused := h.session(t, event.ChannelHolaclaw, h.admin.ID)
	other := h.session(t, event.ChannelHolaclaw, h.admin.ID)

	s := h.stream(h.adminToken, string(focused.ID), "")

	h.emit(other.ID, event.RunStarted)
	slim := s.next(t, 2*time.Second)
	if keys := jsonKeys(t, slim.Data); !equal(keys, []string{"id", "session_id", "trace_id", "type"}) {
		t.Fatalf("slim event carries %v", keys)
	}

	h.emit(focused.ID, event.RunStarted)
	full := s.next(t, 2*time.Second)
	keys := jsonKeys(t, full.Data)
	for _, want := range []string{"schema", "span_id", "attrs"} {
		if !contains(keys, want) {
			t.Fatalf("focused event is missing %q: %v", want, keys)
		}
	}
}

func TestSSEHeartbeat(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.hub.Heartbeat = 40 * time.Millisecond
	s := h.stream(h.adminToken, "", "")
	f := s.next(t, 2*time.Second)
	if f.Comment != "hb" {
		t.Fatalf("expected a heartbeat comment, got %+v", f)
	}
}

// TestSSEEphemeralTokensCarryNoID keeps resume honest: a model.token is never
// stored, so it must not move the client's Last-Event-ID.
func TestSSEEphemeralTokensCarryNoID(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.session(t, event.ChannelHolaclaw, h.admin.ID)
	s := h.stream(h.adminToken, "", "")

	h.hub.PublishToken(sess.ID, event.NewID(), "hel")
	f := s.next(t, 2*time.Second)
	if f.ID != "" {
		t.Fatalf("an ephemeral event carried id %q", f.ID)
	}
	if f.Type != string(event.ModelToken) {
		t.Fatalf("type = %s", f.Type)
	}
	var got gen.Event
	if err := json.Unmarshal(f.Data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if (*got.Attrs)["delta"] != "hel" {
		t.Fatalf("attrs = %+v", *got.Attrs)
	}
}

// TestHubDropsSlowConsumerWithoutBlocking: the emitter is on the reply path,
// so a subscriber that cannot keep up is closed, never waited for.
func TestHubDropsSlowConsumerWithoutBlocking(t *testing.T) {
	t.Parallel()
	hub := NewHub(nil, nil)
	sub, _ := hub.subscribe("", "")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < subscriberBuffer*2; i++ {
			hub.Emit(context.Background(), event.New(event.ModelProgress, event.NewID(), "", "", nil))
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Emit blocked on a subscriber that does not read")
	}

	select {
	case <-sub.dropped:
	case <-time.After(time.Second):
		t.Fatal("the slow subscriber was not dropped")
	}
	if n := hub.Subscribers(); n != 0 {
		t.Fatalf("subscribers = %d, want 0", n)
	}

	// A fresh subscriber still works after the drop, and Close ends it.
	next, _ := hub.subscribe("", "")
	hub.Emit(context.Background(), event.New(event.RunStarted, event.NewID(), "", "", nil))
	select {
	case <-next.ch:
	case <-time.After(time.Second):
		t.Fatal("a fresh subscriber received nothing")
	}
	hub.Close()
	select {
	case <-next.dropped:
	case <-time.After(time.Second):
		t.Fatal("Close did not end the stream")
	}
}

// TestSSEStreamEndsWhenTheHubCloses covers the shutdown path over HTTP.
func TestSSEStreamEndsWhenTheHubCloses(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	s := h.stream(h.adminToken, "", "")
	// The stream is registered before the first event is emitted.
	deadline := time.Now().Add(2 * time.Second)
	for h.hub.Subscribers() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if h.hub.Subscribers() != 1 {
		t.Fatalf("subscribers = %d, want 1", h.hub.Subscribers())
	}
	h.hub.Close()
	if _, err := s.read(2 * time.Second); err == nil {
		t.Fatal("the stream did not end")
	}
}

// TestHubDisconnectEndsOnlyThatIdentitysStreams: what the daemon does on
// identity.disabled. The identity's streams end; everyone else's stay live.
func TestHubDisconnectEndsOnlyThatIdentitysStreams(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	mine := h.session(t, event.ChannelHolaclaw, h.owner.ID)
	owner := h.stream(h.ownerToken, "", "")
	other := h.stream(h.otherToken, "", "")
	admin := h.stream(h.ownerAdmin, "", "")
	deadline := time.Now().Add(2 * time.Second)
	for h.hub.Subscribers() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if n := h.hub.Disconnect(h.other.ID); n != 1 {
		t.Fatalf("Disconnect ended %d streams, want 1", n)
	}
	if _, err := other.read(2 * time.Second); err != io.EOF {
		t.Fatalf("the disconnected stream: err = %v, want it ended", err)
	}
	if n := h.hub.Disconnect(""); n != 0 {
		t.Fatalf("Disconnect(\"\") ended %d streams, want none", n)
	}
	id := h.emit(mine.ID, event.RunStarted)
	for _, s := range []*rawStream{owner, admin} {
		if f := s.next(t, 2*time.Second); f.ID != string(id) {
			t.Fatalf("a remaining stream got %q, want %s", f.ID, id)
		}
	}
	if n := h.hub.Subscribers(); n != 2 {
		t.Fatalf("subscribers = %d, want 2", n)
	}
}

// TestStreamEventsRequiresAToken guards the one endpoint a browser opens
// without the usual fetch plumbing.
func TestStreamEventsRequiresAToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	errorBody(t, h.do(http.MethodGet, "/events", "", nil), http.StatusUnauthorized, CodeUnauthorized)
}

// --- helpers ------------------------------------------------------------------

func jsonKeys(t *testing.T, data []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(ss []string, s string) bool {
	for _, e := range ss {
		if e == s {
			return true
		}
	}
	return false
}

// emitOn stores and fans out one event of a given trace.
func (h *harness) emitOn(sessionID, traceID event.ID, typ event.Type, attrs map[string]any) event.ID {
	h.t.Helper()
	e := event.New(typ, traceID, sessionID, "", attrs)
	h.sink.Emit(context.Background(), e)
	return e.ID
}

// partialFrame decodes a model.partial frame, failing on anything else.
func partialFrame(t *testing.T, f frame) gen.Event {
	t.Helper()
	if f.Type != string(event.ModelPartial) {
		t.Fatalf("type = %s, want %s", f.Type, event.ModelPartial)
	}
	if f.ID != "" {
		t.Fatalf("an ephemeral event carried id %q", f.ID)
	}
	var got gen.Event
	if err := json.Unmarshal(f.Data, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// TestSSEJoinMidTurnGetsTheTextSoFar: a stream opened while a turn streams
// starts from what was already said, then continues with the live deltas.
func TestSSEJoinMidTurnGetsTheTextSoFar(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.session(t, event.ChannelHolaclaw, h.admin.ID)
	trace := event.NewID()
	h.emitOn(sess.ID, trace, event.RunStarted, nil)
	h.hub.PublishToken(sess.ID, trace, "The capital ")
	h.hub.PublishToken(sess.ID, trace, "is Paris")

	s := h.stream(h.adminToken, string(sess.ID), "")
	got := partialFrame(t, s.next(t, 2*time.Second))
	if got.SessionId == nil || *got.SessionId != string(sess.ID) || got.TraceId != string(trace) {
		t.Fatalf("partial is for session %v trace %s", got.SessionId, got.TraceId)
	}
	if (*got.Attrs)["text"] != "The capital is Paris" {
		t.Fatalf("attrs = %+v", *got.Attrs)
	}

	h.hub.PublishToken(sess.ID, trace, ", which")
	f := s.next(t, 2*time.Second)
	if f.Type != string(event.ModelToken) {
		t.Fatalf("type after the partial = %s", f.Type)
	}
	var tok gen.Event
	if err := json.Unmarshal(f.Data, &tok); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if (*tok.Attrs)["delta"] != ", which" {
		t.Fatalf("attrs = %+v", *tok.Attrs)
	}
}

// TestSSEPartialEndsWithTheTurn: once the turn's reply or run end is out, a
// new stream has nothing in flight to catch up on.
func TestSSEPartialEndsWithTheTurn(t *testing.T) {
	t.Parallel()
	for _, end := range []event.Type{event.Reply, event.RunFinished, event.RunAborted} {
		t.Run(string(end), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			sess := h.session(t, event.ChannelHolaclaw, h.admin.ID)
			trace := event.NewID()
			h.hub.PublishToken(sess.ID, trace, "hola")
			h.emitOn(sess.ID, trace, end, nil)

			s := h.stream(h.adminToken, string(sess.ID), "")
			s.quiet(t, 300*time.Millisecond)
		})
	}
}

// TestSSEPartialSurvivesAReplyFromAnotherTrace: a task delivering into the
// main chat posts a reply on its own trace; the main chat's turn is still
// running and keeps its text.
func TestSSEPartialSurvivesAReplyFromAnotherTrace(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.session(t, event.ChannelHolaclaw, h.admin.ID)
	trace := event.NewID()
	h.hub.PublishToken(sess.ID, trace, "still ")
	h.emitOn(sess.ID, event.NewID(), event.Reply, map[string]any{"task": "t1"})
	h.hub.PublishToken(sess.ID, trace, "going")

	s := h.stream(h.adminToken, string(sess.ID), "")
	if got := partialFrame(t, s.next(t, 2*time.Second)); (*got.Attrs)["text"] != "still going" {
		t.Fatalf("attrs = %+v", *got.Attrs)
	}
}

// TestSSEPartialStartsOverOnANewTrace: the text of an earlier turn whose end
// never arrived is not prefixed to the next one.
func TestSSEPartialStartsOverOnANewTrace(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.session(t, event.ChannelHolaclaw, h.admin.ID)
	h.hub.PublishToken(sess.ID, event.NewID(), "old")
	next := event.NewID()
	h.hub.PublishToken(sess.ID, next, "new")

	s := h.stream(h.adminToken, string(sess.ID), "")
	got := partialFrame(t, s.next(t, 2*time.Second))
	if got.TraceId != string(next) || (*got.Attrs)["text"] != "new" {
		t.Fatalf("partial = trace %s %+v", got.TraceId, *got.Attrs)
	}
}

// TestSSEPartialFollowsTheFocus: a focused stream catches up on its own
// session only; an unfocused one sees every session in full, so every turn.
func TestSSEPartialFollowsTheFocus(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := h.session(t, event.ChannelHolaclaw, h.admin.ID)
	b := h.session(t, event.ChannelHolaclaw, h.admin.ID)
	h.hub.PublishToken(a.ID, event.NewID(), "from a")
	h.hub.PublishToken(b.ID, event.NewID(), "from b")

	focused := h.stream(h.adminToken, string(b.ID), "")
	got := partialFrame(t, focused.next(t, 2*time.Second))
	if *got.SessionId != string(b.ID) || (*got.Attrs)["text"] != "from b" {
		t.Fatalf("focused partial = %v %+v", *got.SessionId, *got.Attrs)
	}
	focused.quiet(t, 300*time.Millisecond)

	all := h.stream(h.adminToken, "", "")
	texts := map[string]any{}
	for i := 0; i < 2; i++ {
		got := partialFrame(t, all.next(t, 2*time.Second))
		texts[*got.SessionId] = (*got.Attrs)["text"]
	}
	if texts[string(a.ID)] != "from a" || texts[string(b.ID)] != "from b" {
		t.Fatalf("unfocused partials = %v", texts)
	}
}

// TestSSEPartialRespectsSessionOwnership: a client token never catches up on
// a turn in someone else's session.
func TestSSEPartialRespectsSessionOwnership(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.session(t, event.ChannelHolaclaw, h.other.ID)
	h.hub.PublishToken(sess.ID, event.NewID(), "private")

	s := h.stream(h.ownerToken, "", "")
	s.quiet(t, 300*time.Millisecond)
}

// TestSSEResumeThenPartialThenLive: a resume replays what the client missed up
// to the moment it joined, then the turn's text so far, then the live tail.
func TestSSEResumeThenPartialThenLive(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.session(t, event.ChannelHolaclaw, h.admin.ID)
	seen := h.emit(sess.ID, event.RunFinished)
	trace := event.NewID()
	started := h.emitOn(sess.ID, trace, event.RunStarted, nil)
	h.hub.PublishToken(sess.ID, trace, "so far")

	s := h.stream(h.adminToken, string(sess.ID), string(seen))
	if f := s.next(t, 2*time.Second); f.ID != string(started) {
		t.Fatalf("replayed id = %s, want %s", f.ID, started)
	}
	if got := partialFrame(t, s.next(t, 2*time.Second)); (*got.Attrs)["text"] != "so far" {
		t.Fatalf("attrs = %+v", *got.Attrs)
	}
	fin := h.emitOn(sess.ID, trace, event.RunFinished, nil)
	if f := s.next(t, 2*time.Second); f.ID != string(fin) {
		t.Fatalf("live id = %s, want %s", f.ID, fin)
	}
	s.quiet(t, 300*time.Millisecond)
}

// TestSSEPartialIsTheSessionsNotAClients: two clients switching between the
// same chats while a turn streams each catch up on the whole text on every
// switch; one client joining never moves what the other gets.
func TestSSEPartialIsTheSessionsNotAClients(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := h.session(t, event.ChannelHolaclaw, h.admin.ID)
	b := h.session(t, event.ChannelHolaclaw, h.admin.ID)
	trace := event.NewID()
	h.hub.PublishToken(a.ID, trace, "one ")

	desktop := h.stream(h.adminToken, string(a.ID), "")
	if got := partialFrame(t, desktop.next(t, 2*time.Second)); (*got.Attrs)["text"] != "one " {
		t.Fatalf("desktop joined at %+v", *got.Attrs)
	}
	desktop.close()
	h.stream(h.adminToken, string(b.ID), "").quiet(t, 100*time.Millisecond)

	h.hub.PublishToken(a.ID, trace, "two ")
	cli := h.stream(h.adminToken, string(a.ID), "")
	if got := partialFrame(t, cli.next(t, 2*time.Second)); (*got.Attrs)["text"] != "one two " {
		t.Fatalf("cli joined at %+v", *got.Attrs)
	}
	cli.close()

	h.hub.PublishToken(a.ID, trace, "three")
	for _, name := range []string{"desktop", "cli"} {
		back := h.stream(h.adminToken, string(a.ID), "")
		if got := partialFrame(t, back.next(t, 2*time.Second)); (*got.Attrs)["text"] != "one two three" {
			t.Fatalf("%s came back to %+v", name, *got.Attrs)
		}
		back.close()
	}
}
