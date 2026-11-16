package session

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"holaclaw/internal/agent"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/store/storetest"
)

// routerHarness is a router on a real store with one linked user.
type routerHarness struct {
	*harness
	r        *Router
	identity store.Identity
}

func newRouterHarness(t *testing.T) *routerHarness {
	t.Helper()
	var h *harness
	h = newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		return h.reply(ctx, in, "ok"), nil
	})
	ctx := context.Background()
	i := store.Identity{ID: event.NewID(), Name: "angel", Role: store.RoleOwner, CreatedAt: time.Now()}
	if err := h.st.CreateIdentity(ctx, i); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	storetest.Bind(t, h.st, "tg-main", []string{"11", "22"}, i.ID, "Angel")
	return &routerHarness{harness: h, identity: i, r: &Router{
		Store: h.st, Manager: h.m, Events: h.ev,
		AgentFor: func(string) string { return "main" },
	}}
}

func envelope(kind event.ConversationKind, alias, text string) event.Envelope {
	return event.Envelope{
		Origin: event.Origin{
			Channel: "tg-main", ChannelType: "telegram", Conversation: "c9", ConversationKind: kind,
		},
		Sender:  event.Sender{Aliases: []string{alias}, DisplayName: "Angel <admin>"},
		Message: event.Message{Text: text},
	}
}

func attrs(t *testing.T, e event.Event) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Attrs, &m); err != nil {
		t.Fatalf("attrs: %v", err)
	}
	return m
}

func TestRouteResolvesAndSubmits(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	acc, err := rh.r.Route(context.Background(), envelope(event.ConversationDM, "22", "hi"))
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if acc.TraceID == "" {
		t.Fatal("trace id was not stamped")
	}
	want := Key("main", event.Origin{
		Channel: "tg-main", ConversationKind: event.ConversationDM, Conversation: "c9",
	}, rh.identity.ID)
	if acc.Key != want {
		t.Fatalf("key = %q, want %q", acc.Key, want)
	}
	if acc.User.ID != rh.identity.ID || acc.Agent != "main" || acc.Duplicate {
		t.Fatalf("accepted = %+v", acc)
	}
	sess, err := rh.st.GetSession(context.Background(), acc.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if sess.Key != want || sess.UserID != rh.identity.ID || sess.Kind != store.SessionChat ||
		sess.Origin.Channel != "tg-main" {
		t.Fatalf("session = %+v", sess)
	}
	waitFor(t, "reply", func() bool { return len(rh.deliveries()) == 1 })

	// channel.received carries the trace, router.resolved trace and session.
	rec := rh.ev.of(event.ChannelReceived)
	if len(rec) != 1 || rec[0].TraceID != acc.TraceID {
		t.Fatalf("channel.received = %+v", rec)
	}
	res := rh.ev.of(event.RouterResolved)
	if len(res) != 1 || res[0].TraceID != acc.TraceID || res[0].SessionID != acc.SessionID {
		t.Fatalf("router.resolved = %+v", res)
	}
	a := attrs(t, res[0])
	if a["user"] != string(rh.identity.ID) || a["role"] != "owner" || a["agent"] != "main" || a["key"] != want {
		t.Fatalf("router.resolved attrs = %v", a)
	}
}

// A second alias of the same user lands in the same session.
func TestRouteLinkedAliasSharesSession(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	one, err := rh.r.Route(context.Background(), envelope(event.ConversationDM, "11", "a"))
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	two, err := rh.r.Route(context.Background(), envelope(event.ConversationDM, "22", "b"))
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if one.SessionID != two.SessionID {
		t.Fatalf("aliases got different sessions: %s vs %s", one.SessionID, two.SessionID)
	}
	if one.TraceID == two.TraceID {
		t.Fatal("two messages share a trace id")
	}
}

func TestRouteUnknownSender(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	env := envelope(event.ConversationDM, "999", "hi")
	_, err := rh.r.Route(context.Background(), env)
	var unknown *ErrUnknownSender
	if !errors.As(err, &unknown) {
		t.Fatalf("err = %v, want ErrUnknownSender", err)
	}
	if unknown.Envelope.Origin.Conversation != "c9" || unknown.Envelope.TraceID == "" {
		t.Fatalf("envelope not carried: %+v", unknown.Envelope)
	}
	dropped := rh.ev.of(event.RouterDropped)
	if len(dropped) != 1 || attrs(t, dropped[0])["reason"] != "unknown_sender" {
		t.Fatalf("router.dropped = %+v", dropped)
	}
	if n, err := rh.st.SessionsWithPending(context.Background()); err != nil || len(n) != 0 {
		t.Fatalf("unknown sender created work: %v (%v)", n, err)
	}
}

// A resolver failure that is not "unknown" surfaces as an ordinary error.
func TestRouteResolveError(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	rh.r.Resolve = func(context.Context, event.Origin, event.Sender) (store.Resolution, error) {
		return store.Resolution{}, errTest
	}
	_, err := rh.r.Route(context.Background(), envelope(event.ConversationDM, "11", "hi"))
	var unknown *ErrUnknownSender
	if err == nil || errors.As(err, &unknown) {
		t.Fatalf("err = %v, want a plain error", err)
	}
	if attrs(t, rh.ev.of(event.RouterDropped)[0])["reason"] != "resolve_error" {
		t.Fatal("wrong drop reason")
	}
}

// The same platform message id is accepted once.
func TestRouteDedupesPlatformMessage(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	env := envelope(event.ConversationDM, "11", "hi")
	env.Origin.PlatformMsgID = "tg:4242"
	first, err := rh.r.Route(context.Background(), env)
	if err != nil || first.Duplicate {
		t.Fatalf("first route: %+v (%v)", first, err)
	}
	// A re-delivery: new trace id, same platform id.
	env.TraceID = ""
	env.Message.ID = ""
	second, err := rh.r.Route(context.Background(), env)
	if err != nil {
		t.Fatalf("second route: %v", err)
	}
	if !second.Duplicate {
		t.Fatal("re-delivery was not reported as a duplicate")
	}
	waitFor(t, "one reply", func() bool { return len(rh.deliveries()) == 1 })
	ms, err := rh.st.ListMessages(context.Background(), first.SessionID, "", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var users int
	for _, m := range ms {
		if m.Role == event.RoleUser {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("stored %d user messages, want 1", users)
	}
	// Outside the TTL the same id is accepted again.
	rh.r.DedupeTTL = time.Nanosecond
	time.Sleep(time.Millisecond)
	env.TraceID, env.Message.ID = "", ""
	third, err := rh.r.Route(context.Background(), env)
	if err != nil || third.Duplicate {
		t.Fatalf("expired dedupe entry still blocked: %+v (%v)", third, err)
	}
}

// The sender label travels only for room conversations, and raw: the context
// builder sanitises it.
func TestRouteRoomSenderLabel(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	g, err := rh.r.Route(context.Background(), envelope(event.ConversationRoom, "11", "in room"))
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	m, err := rh.st.GetMessage(context.Background(), g.MessageID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if m.Sender != "Angel <admin>" {
		t.Fatalf("room sender = %q, want the raw display name", m.Sender)
	}
	if m.Role != event.RoleUser || m.TraceID != g.TraceID || m.SessionID != g.SessionID {
		t.Fatalf("message = %+v", m)
	}

	d, err := rh.r.Route(context.Background(), envelope(event.ConversationDM, "11", "in dm"))
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	dm, err := rh.st.GetMessage(context.Background(), d.MessageID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if dm.Sender != "" {
		t.Fatalf("dm carried a sender label: %q", dm.Sender)
	}
	if d.SessionID == g.SessionID {
		t.Fatal("room and dm shared a session")
	}
}

// A trace id supplied by the caller is kept.
func TestRouteKeepsTraceID(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	env := envelope(event.ConversationDM, "11", "hi")
	env.TraceID = event.NewID()
	acc, err := rh.r.Route(context.Background(), env)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if acc.TraceID != env.TraceID {
		t.Fatalf("trace id = %s, want %s", acc.TraceID, env.TraceID)
	}
}

// Route falls back to the store's identity lookup and the default agent.
func TestRouteDefaults(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	rh.r.AgentFor = nil
	rh.r.Resolve = nil
	rh.r.Events = nil
	acc, err := rh.r.Route(context.Background(), envelope(event.ConversationDM, "11", "hi"))
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if acc.Agent != DefaultAgent {
		t.Fatalf("agent = %q, want %q", acc.Agent, DefaultAgent)
	}
}

// Every inbound message carries its resolved sender: a room session is shared
// by the whole room, so the turn's policy role comes from the message, never
// from the session row (docs/sessions.md, docs/security.md).
func TestRouteStampsUserIDOnTheMessage(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	ctx := context.Background()

	// A second, less privileged member of the same room.
	guest := store.Identity{ID: event.NewID(), Name: "mallory", Status: store.StatusPending, CreatedAt: time.Now()}
	if err := rh.st.CreateIdentity(ctx, guest); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	storetest.Bind(t, rh.st, "tg-main", []string{"99"}, guest.ID, "Mallory")

	owner, err := rh.r.Route(ctx, envelope(event.ConversationRoom, "11", "hello room"))
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	intruder, err := rh.r.Route(ctx, envelope(event.ConversationRoom, "99", "and me"))
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if owner.SessionID != intruder.SessionID {
		t.Fatal("the room members did not share a session")
	}
	for _, tc := range []struct {
		what string
		id   event.ID
		want event.ID
	}{
		{"owner", owner.MessageID, rh.identity.ID},
		{"guest", intruder.MessageID, guest.ID},
	} {
		m, err := rh.st.GetMessage(ctx, tc.id)
		if err != nil {
			t.Fatalf("get %s message: %v", tc.what, err)
		}
		if m.UserID != tc.want {
			t.Errorf("%s message user = %q, want %q", tc.what, m.UserID, tc.want)
		}
	}

	// The session row names the room, which holds no role: a turn's role can
	// only come from its messages.
	sess, err := rh.st.GetSession(ctx, owner.SessionID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	room, err := rh.st.GetRoom(ctx, "tg-main", "c9")
	if err != nil {
		t.Fatalf("get room: %v", err)
	}
	if sess.UserID != room.ID || room.Kind != store.KindRoom || !room.Pending() {
		t.Fatalf("session user = %q, room = %+v; want the session owned by the pending room", sess.UserID, room)
	}
}

// The first message creates the room for a caller that reaches the router
// directly, and a second sender changes nothing about who owns the session.
func TestRouteRoomOwnsTheSession(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	ctx := context.Background()
	if _, err := rh.st.GetRoom(ctx, "tg-main", "c9"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("room before the first message: %v", err)
	}
	first, err := rh.r.Route(ctx, envelope(event.ConversationRoom, "11", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	room, err := rh.st.GetRoom(ctx, "tg-main", "c9")
	if err != nil {
		t.Fatal(err)
	}
	second := store.Identity{ID: event.NewID(), Name: "bob", Role: store.RoleUser, CreatedAt: time.Now()}
	if err := rh.st.CreateIdentity(ctx, second); err != nil {
		t.Fatal(err)
	}
	storetest.Bind(t, rh.st, "tg-main", []string{"33"}, second.ID, "Bob")
	again, err := rh.r.Route(ctx, envelope(event.ConversationRoom, "33", "me too"))
	if err != nil {
		t.Fatal(err)
	}
	if again.SessionID != first.SessionID || again.User.ID != second.ID {
		t.Fatalf("second = %+v, want the same session under its own sender", again)
	}
	if evs := rh.ev.of(event.IdentityCreated); len(evs) != 1 || attrs(t, evs[0])["identity"] != string(room.ID) {
		t.Fatalf("identity.created = %+v, want the room once", evs)
	}
	sess, err := rh.st.GetSession(ctx, first.SessionID)
	if err != nil || sess.UserID != room.ID {
		t.Fatalf("session user = %q (%v), want the room %q", sess.UserID, err, room.ID)
	}
	m, err := rh.st.GetMessage(ctx, again.MessageID)
	if err != nil || m.UserID != second.ID {
		t.Fatalf("message user = %q (%v), want the sender", m.UserID, err)
	}
}

// A disabled room, and a conversation whose binding a user holds, are dropped
// with a trace and start nothing.
func TestRouteDropsRefusedRooms(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		setup  func(t *testing.T, rh *routerHarness)
		reason string
	}{
		"disabled": {func(t *testing.T, rh *routerHarness) {
			room, _, err := rh.st.EnsureRoom(context.Background(), "tg-main", "c9", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rh.st.DB().Exec(`UPDATE identities SET status = 'disabled' WHERE id = ?`, string(room.ID)); err != nil {
				t.Fatal(err)
			}
		}, "room_disabled"},
		"binding held by a user": {func(t *testing.T, rh *routerHarness) {
			storetest.Bind(t, rh.st, "tg-main", []string{"c9"}, rh.identity.ID, "")
		}, "room_binding_held"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rh := newRouterHarness(t)
			tc.setup(t, rh)
			acc, err := rh.r.Route(context.Background(), envelope(event.ConversationRoom, "11", "hi"))
			if err != nil || acc.SessionID != "" {
				t.Fatalf("route = %+v, %v; want a quiet drop", acc, err)
			}
			if sessions, err := rh.st.ListSessions(context.Background()); err != nil || len(sessions) != 0 {
				t.Fatalf("sessions = %+v, %v", sessions, err)
			}
			evs := rh.ev.of(event.RouterDropped)
			if len(evs) != 1 || attrs(t, evs[0])["reason"] != tc.reason {
				t.Fatalf("dropped = %+v, want %s", evs, tc.reason)
			}
		})
	}
}

// failingRooms is a store whose EnsureRoom fails while fail is set.
type failingRooms struct {
	store.Store
	fail bool
}

func (f *failingRooms) EnsureRoom(ctx context.Context, channel, conversation string, now time.Time) (store.Identity, bool, error) {
	if f.fail {
		return store.Identity{}, false, errTest
	}
	return f.Store.EnsureRoom(ctx, channel, conversation, now)
}

// A store failure on the room is an error, starts no turn, and releases the
// dedupe: a retry of the same message is not swallowed as a duplicate.
func TestRouteRoomStoreFailureReleasesDedupe(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	fr := &failingRooms{Store: rh.st, fail: true}
	rh.r.Store = fr
	env := envelope(event.ConversationRoom, "11", "hi")
	env.Origin.PlatformMsgID = "tg:77"

	acc, err := rh.r.Route(context.Background(), env)
	if err == nil || !errors.Is(err, errTest) || acc.SessionID != "" || acc.Duplicate {
		t.Fatalf("route = %+v, %v; want a plain error and no session", acc, err)
	}
	if sessions, err := rh.st.ListSessions(context.Background()); err != nil || len(sessions) != 0 {
		t.Fatalf("sessions = %+v, %v", sessions, err)
	}
	if n, err := rh.st.SessionsWithPending(context.Background()); err != nil || len(n) != 0 {
		t.Fatalf("a failed route created work: %v (%v)", n, err)
	}

	fr.fail = false
	env.TraceID = ""
	acc, err = rh.r.Route(context.Background(), env)
	if err != nil || acc.Duplicate || acc.SessionID == "" {
		t.Fatalf("retry = %+v, %v; want accepted, not a duplicate", acc, err)
	}
}

// A reaction creates no room, even where a message would.
func TestRouteReactionCreatesNoRoom(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	env := envelope(event.ConversationRoom, "11", "")
	env.Reaction = &event.Reaction{TargetMsgID: "100", Emoji: "👍"}
	if _, err := rh.r.Route(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if _, err := rh.st.GetRoom(context.Background(), "tg-main", "c9"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("room = %v, want none", err)
	}
}

// Deleting a room leaves its sessions ownerless under the same key; the next
// message adopts them for the new room identity, and a session that has an owner
// keeps it.
func TestEnsureAdoptsAnOwnerlessRoomSession(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	ctx := context.Background()
	first, err := rh.r.Route(ctx, envelope(event.ConversationRoom, "11", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	old, err := rh.st.GetRoom(ctx, "tg-main", "c9")
	if err != nil {
		t.Fatal(err)
	}
	if err := rh.st.DeleteIdentity(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	if s, err := rh.st.GetSession(ctx, first.SessionID); err != nil || s.UserID != "" {
		t.Fatalf("after the delete = %+v, %v; want no owner", s, err)
	}
	second, err := rh.r.Route(ctx, envelope(event.ConversationRoom, "11", "back"))
	if err != nil {
		t.Fatal(err)
	}
	room, err := rh.st.GetRoom(ctx, "tg-main", "c9")
	if err != nil || room.ID == old.ID {
		t.Fatalf("room = %+v, %v; want a new one", room, err)
	}
	s, err := rh.st.GetSession(ctx, second.SessionID)
	if err != nil || second.SessionID != first.SessionID || s.UserID != room.ID {
		t.Fatalf("session = %+v, %v; want %s adopted by %s", s, err, first.SessionID, room.ID)
	}

	if got, err := rh.st.AdoptSession(ctx, s.ID, rh.identity.ID); err != nil || got.UserID != room.ID {
		t.Fatalf("adopting an owned session = %q, %v; want it kept", got.UserID, err)
	}
}

// A message into a held session stays there, whatever agent the channel
// would pick.
func TestRouteToKeepsTheSessionsAgent(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	ctx := context.Background()
	env := envelope(event.ConversationDM, "22", "hi")
	held, _, err := rh.st.EnsureSession(ctx, store.Session{
		Key: Key("codex", env.Origin, rh.identity.ID), Agent: "codex", Kind: store.SessionChat,
		UserID: rh.identity.ID, Origin: env.Origin,
	})
	if err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	acc, err := rh.r.RouteTo(ctx, env, held)
	if err != nil {
		t.Fatalf("route to: %v", err)
	}
	if acc.SessionID != held.ID || acc.Agent != "codex" || acc.Key != held.Key {
		t.Fatalf("accepted = %+v, want session %s under codex", acc, held.ID)
	}
	res := rh.ev.of(event.RouterResolved)
	if len(res) != 1 || res[0].SessionID != held.ID || attrs(t, res[0])["agent"] != "codex" {
		t.Fatalf("router.resolved = %+v", res)
	}
	all, err := rh.st.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("sessions = %+v, want only the held one", all)
	}
}

// A reply in a new thread starts from the exchange it hangs off: the root
// mention and its answer, copied into the thread session ahead of it. Tool
// traffic stays behind, and a thread whose root is unknown starts empty.
func TestRouteSeedsThreadFromItsRoot(t *testing.T) {
	h := newRouterHarness(t)
	h.r.Events = h.st // ReceivedTrace reads channel.received from the store
	ctx := context.Background()

	root := envelope(event.ConversationRoom, "11", "rain in Sevilla tomorrow?")
	root.Mentioned = true
	root.Origin.PlatformMsgID = "c9:100.1"
	acc, err := h.r.Route(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the root's answer", func() bool { return len(h.deliveries()) == 1 })
	// A tool exchange in the same trace is not copied.
	call := event.Message{SessionID: acc.SessionID, TraceID: acc.TraceID, Role: event.RoleAssistant,
		ToolCalls: []event.ToolCall{{ID: "c1", Name: "fetch"}}}
	result := event.Message{SessionID: acc.SessionID, TraceID: acc.TraceID, Role: event.RoleTool, ToolCallID: "c1", Text: "body"}
	for _, m := range []event.Message{call, result} {
		if _, err := h.st.AppendMessage(ctx, m, false); err != nil {
			t.Fatal(err)
		}
	}

	reply := envelope(event.ConversationRoom, "11", "and on Saturday?")
	reply.Mentioned = true
	reply.Origin.Thread, reply.Origin.ThreadRoot, reply.Origin.PlatformMsgID = "100.1", "c9:100.1", "c9:100.2"
	in, err := h.r.Route(ctx, reply)
	if err != nil {
		t.Fatal(err)
	}
	if in.SessionID == acc.SessionID {
		t.Fatal("the thread reply landed in the channel session")
	}
	waitFor(t, "the thread turn", func() bool { return h.turnCount() == 2 })
	hist := h.turnAt(1).History
	var got []string
	for _, m := range hist {
		got = append(got, string(m.Role)+":"+m.Text)
	}
	want := []string{"user:rain in Sevilla tomorrow?", "assistant:ok", "user:and on Saturday?"}
	if len(got) != len(want) {
		t.Fatalf("history = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("history = %q, want %q", got, want)
		}
	}
	if hist[0].ID == "" || hist[0].SessionID != in.SessionID || hist[0].TraceID != in.TraceID || hist[0].UserID != h.identity.ID {
		t.Fatalf("copied root = %+v", hist[0])
	}

	orphan := envelope(event.ConversationRoom, "11", "who?")
	orphan.Mentioned = true
	orphan.Origin.Thread, orphan.Origin.ThreadRoot, orphan.Origin.PlatformMsgID = "7.0", "c9:7.0", "c9:7.1"
	if _, err := h.r.Route(ctx, orphan); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the orphan thread turn", func() bool { return h.turnCount() == 3 })
	if hist := h.turnAt(2).History; len(hist) != 1 {
		t.Fatalf("orphan thread history = %+v, want only its own message", hist)
	}
}
