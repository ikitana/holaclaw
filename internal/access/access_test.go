package access

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/auth"
	"holaclaw/internal/config"
	"holaclaw/internal/event"
	"holaclaw/internal/session"
	"holaclaw/internal/store"
	"holaclaw/internal/store/storetest"
)

var errTest = errors.New("boom")

// harness is a gate over a real store, real auth, with the
// router and the notifier recorded.
type harness struct {
	t  *testing.T
	st *store.SQLite
	au *auth.Service
	g  *Gate

	mu       sync.Mutex
	clock    time.Time
	acc      config.Access
	routeErr error
	routed   []event.Envelope
	notices  []string
	events   []event.Event
}

func newHarness(t *testing.T, acc config.Access) *harness {
	t.Helper()
	return newHarnessIn(t, t.TempDir(), acc)
}

// newHarnessIn opens the store in dir, so a second harness on the same dir is
// the daemon after a restart.
func newHarnessIn(t *testing.T, dir string, acc config.Access) *harness {
	t.Helper()
	st, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &harness{t: t, st: st, clock: time.Unix(1700000000, 0).UTC(), acc: acc}
	h.au = auth.New(st)
	h.au.Now = h.now
	h.g = New(Deps{
		Config: func(ch string) (config.Access, bool) {
			if ch != "tg-main" {
				return config.Access{}, false
			}
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.acc, true
		},
		Auth: h.au,
		Route: func(_ context.Context, env event.Envelope) (session.MessageAccepted, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.routeErr != nil {
				return session.MessageAccepted{}, h.routeErr
			}
			h.routed = append(h.routed, env)
			return session.MessageAccepted{TraceID: env.TraceID}, nil
		},
		Store:  st,
		Dedupe: st,
		Notify: func(_ context.Context, _ event.Origin, text string) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.notices = append(h.notices, text)
		},
		Events: event.SinkFunc(func(_ context.Context, e event.Event) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.events = append(h.events, e)
		}),
		Now: h.now,
	})
	return h
}

// roomApproved reports whether a conversation has a room identity that is on.
func (h *harness) roomApproved(ctx context.Context, channel, conversation string) (bool, error) {
	room, err := h.st.GetRoom(ctx, channel, conversation)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return room.Approved(), err
}

// approveRoom approves a room the way an admin's word does.
func (h *harness) approveRoom(ctx context.Context, channel, conversation string, by event.ID) error {
	env := event.Envelope{Origin: event.Origin{Channel: channel, Conversation: conversation, ConversationKind: event.ConversationRoom}}
	_, _, err := h.g.approveRoom(ctx, env, by)
	return err
}

func (h *harness) now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clock
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clock = h.clock.Add(d)
}

func (h *harness) counts() (routed int, notices []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.routed), append([]string(nil), h.notices...)
}

func (h *harness) dropped() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, e := range h.events {
		if e.Type == event.RouterDropped {
			var m map[string]any
			if err := json.Unmarshal(e.Attrs, &m); err != nil {
				h.t.Fatalf("attrs: %v", err)
			}
			out = append(out, m)
		}
	}
	return out
}

func (h *harness) received() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, e := range h.events {
		if e.Type == event.ChannelReceived {
			var m map[string]any
			if err := json.Unmarshal(e.Attrs, &m); err != nil {
				h.t.Fatalf("attrs: %v", err)
			}
			out = append(out, m)
		}
	}
	return out
}

// user links an alias to a user of the given role; RoleGuest makes a pending
// identity.
func (h *harness) identity(alias string, role store.Role) store.Identity {
	h.t.Helper()
	ctx := context.Background()
	i := store.Identity{ID: event.NewID(), Name: "u" + alias, Role: role, CreatedAt: h.now()}
	if role == store.RoleGuest {
		i.Role, i.Status = "", store.StatusPending
	}
	if err := h.st.CreateIdentity(ctx, i); err != nil {
		h.t.Fatalf("create identity: %v", err)
	}
	storetest.Bind(h.t, h.st, "tg-main", []string{alias}, i.ID, "U "+alias)
	return i
}

func dm(alias, text string) event.Envelope {
	return event.Envelope{
		Origin: event.Origin{
			Channel: "tg-main", ChannelType: "telegram",
			Conversation: "c-" + alias, ConversationKind: event.ConversationDM,
		},
		Sender:  event.Sender{Aliases: []string{alias}, DisplayName: "Nina <b>"},
		Message: event.Message{Text: text},
	}
}

func roomMsg(alias, text string, mentioned bool) event.Envelope {
	return event.Envelope{
		Origin: event.Origin{
			Channel: "tg-main", ChannelType: "telegram",
			Conversation: "g1", ConversationKind: event.ConversationRoom,
		},
		Sender:    event.Sender{Aliases: []string{alias}, DisplayName: "Nina <b>"},
		Mentioned: mentioned,
		Message:   event.Message{Text: text},
	}
}

func invite(dmMode string) config.Access {
	return config.Access{DM: dmMode, Rooms: "off", Reply: "mention"}
}

func (h *harness) in(env event.Envelope) error {
	return h.g.Inbound(context.Background(), env)
}

func (h *harness) mustIn(env event.Envelope) {
	h.t.Helper()
	if err := h.in(env); err != nil {
		h.t.Fatalf("inbound: %v", err)
	}
}

// --- basics -----------------------------------------------------------------

func TestFromSelfIsDroppedSilently(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	env := dm("1", "hi")
	env.Sender.FromSelf = true
	h.mustIn(env)
	routed, notices := h.counts()
	if routed != 0 || len(notices) != 0 || len(h.dropped()) != 0 {
		t.Fatalf("self message produced work: %d routed, %v, %v", routed, notices, h.dropped())
	}
}

func TestUnknownChannelIsDropped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	env := dm("1", "hi")
	env.Origin.Channel = "nope"
	h.mustIn(env)
	if routed, _ := h.counts(); routed != 0 {
		t.Fatal("unknown channel was routed")
	}
	d := h.dropped()
	if len(d) != 1 || d[0]["reason"] != "unknown_channel" {
		t.Fatalf("dropped = %v", d)
	}
}

func TestKnownSenderRoutesInEveryMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"invite", "open"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, invite(mode))
			h.identity("7", store.RoleUser)
			h.mustIn(dm("7", "hello"))
			if routed, _ := h.counts(); routed != 1 {
				t.Fatalf("%s: routed %d, want 1", mode, routed)
			}
		})
	}
}

func TestRouteErrorPropagates(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	h.identity("7", store.RoleUser)
	h.mu.Lock()
	h.routeErr = errTest
	h.mu.Unlock()
	if err := h.in(dm("7", "hi")); !errors.Is(err, errTest) {
		t.Fatalf("err = %v, want the transient error", err)
	}
}

// An identity that vanished between the lookup and the route is a drop, not a
// redelivery.
func TestRouteUnknownSenderIsDropped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	h.identity("7", store.RoleUser)
	h.mu.Lock()
	h.routeErr = &session.ErrUnknownSender{}
	h.mu.Unlock()
	if err := h.in(dm("7", "hi")); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	d := h.dropped()
	if len(d) != 1 || d[0]["reason"] != "unknown_sender" {
		t.Fatalf("dropped = %v", d)
	}
}

// --- dm: invite -------------------------------------------------------------

func TestInviteModeDropsStrangerAndNotifiesOncePerDay(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	h.mustIn(dm("9", "hello?"))
	h.mustIn(dm("9", "anyone there"))
	routed, notices := h.counts()
	if routed != 0 {
		t.Fatal("stranger was routed")
	}
	if len(notices) != 1 || notices[0] != noticeInviteOnly {
		t.Fatalf("notices = %v", notices)
	}
	d := h.dropped()
	if len(d) != 2 || d[0]["reason"] != "unknown_sender" {
		t.Fatalf("dropped = %v", d)
	}
	if _, ok := d[0]["text"]; ok {
		t.Fatal("drop event carried message text")
	}

	h.advance(23 * time.Hour)
	h.mustIn(dm("9", "still here"))
	if _, notices = h.counts(); len(notices) != 1 {
		t.Fatalf("notified again inside the day: %v", notices)
	}
	h.advance(2 * time.Hour)
	h.mustIn(dm("9", "hello again"))
	if _, notices = h.counts(); len(notices) != 2 {
		t.Fatalf("no notice after a day: %v", notices)
	}
}

// A flood from one alias must not fill the event table.
func TestDropEventsAreCappedPerAlias(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	for i := 0; i < 100; i++ {
		h.mustIn(dm("9", "spam"))
	}
	if n := len(h.dropped()); n != dropEvents {
		t.Fatalf("emitted %d drop events, want %d", n, dropEvents)
	}
	h.advance(dropWindow + time.Minute)
	h.mustIn(dm("9", "spam"))
	if n := len(h.dropped()); n != dropEvents+1 {
		t.Fatalf("window did not reopen: %d events", n)
	}
}

// code mints a DM invite on channel for a new user identity with no bindings.
func (h *harness) code(t *testing.T, channel string) string {
	t.Helper()
	i, err := h.au.CreateIdentity(context.Background(), "Invited", store.RoleUser, "")
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}
	code, _, err := h.au.CreateInvite(context.Background(), auth.InviteSpec{Channel: channel, UserID: i.ID})
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	return code
}

func TestInviteCodeLinksSender(t *testing.T) {
	t.Parallel()
	forms := map[string]string{
		"start":   "/start CODE",
		"bare":    "CODE",
		"at_bot":  "/start@holaclaw CODE",
		"padded":  "  CODE  ",
		"shouted": "/START CODE",
	}
	for name, form := range forms {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, invite("invite"))
			code := h.code(t, "tg-main")
			text := strings.Replace(form, "CODE", strings.ToLower(code[:5])+"-"+code[5:], 1)
			h.mustIn(dm("9", text))
			routed, notices := h.counts()
			if routed != 0 {
				t.Fatal("the /start message was routed")
			}
			if len(notices) != 1 || notices[0] != noticeLinked {
				t.Fatalf("notices = %v", notices)
			}
			// Linked: the next message is an ordinary one.
			h.mustIn(dm("9", "hi"))
			if routed, _ = h.counts(); routed != 1 {
				t.Fatalf("linked sender not routed: %d", routed)
			}
		})
	}
}

func TestInviteCodeInvalid(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	h.mustIn(dm("9", "/start ZZZZZZZZZZ"))
	_, notices := h.counts()
	if len(notices) != 1 || notices[0] != noticeBadCode {
		t.Fatalf("notices = %v", notices)
	}
	d := h.dropped()
	if len(d) != 1 || d[0]["reason"] != "invite_invalid" {
		t.Fatalf("dropped = %v", d)
	}
}

func TestInviteCodeForAnotherChannel(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	code := h.code(t, "tg-other")
	h.mustIn(dm("9", "/start "+code))
	_, notices := h.counts()
	if len(notices) != 1 || notices[0] != noticeBadCode {
		t.Fatalf("notices = %v", notices)
	}
	if routed, _ := h.counts(); routed != 0 {
		t.Fatal("a wrong-channel code routed")
	}
}

// After too many wrong codes auth rate-limits: the gate answers nothing.
func TestInviteRateLimitedIsSilent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	for i := 0; i < 5; i++ {
		h.mustIn(dm("9", "/start ZZZZZZZZZZ"))
	}
	if _, notices := h.counts(); len(notices) != 5 {
		t.Fatalf("notices = %v", notices)
	}
	h.mustIn(dm("9", "/start ZZZZZZZZZZ"))
	_, notices := h.counts()
	if len(notices) != 5 {
		t.Fatalf("rate-limited attempt answered: %v", notices)
	}
	d := h.dropped()
	if d[len(d)-1]["reason"] != "rate_limited" {
		t.Fatalf("last drop = %v", d[len(d)-1])
	}
}

// A message that is not a code at all never reaches the redeem path.
func TestNonCodeTextIsNotACode(t *testing.T) {
	t.Parallel()
	for _, text := range []string{"hello", "/start", "one two three", "0123456789012"} {
		if code, ok := inviteCode(text); ok {
			t.Fatalf("%q read as code %q", text, code)
		}
	}
	if code, ok := inviteCode("/start abcd-lfghjk"); !ok || code != "ABCD1FGHJK" {
		t.Fatalf("code = %q, %v", code, ok)
	}
}

// --- dm: open ---------------------------------------------------------------

func TestOpenModeCreatesGuestAndRoutes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("open"))
	h.mustIn(dm("9", "hello"))
	if routed, _ := h.counts(); routed != 1 {
		t.Fatal("open mode did not route")
	}
	i, err := h.au.Resolve(context.Background(), "tg-main", []string{"9"})
	if err != nil {
		t.Fatalf("resolve guest: %v", err)
	}
	if !i.Pending() {
		t.Fatalf("role = %q, want guest", i.Role)
	}
}

func TestOpenModeRateLimitsGuests(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("open"))
	for i := 0; i < guestBurst+5; i++ {
		h.mustIn(dm("9", "hi"))
	}
	routed, notices := h.counts()
	if routed != guestBurst {
		t.Fatalf("routed %d, want %d", routed, guestBurst)
	}
	if len(notices) != 1 || notices[0] != noticeTooFast {
		t.Fatalf("notices = %v", notices)
	}
	h.advance(guestWindow + time.Minute)
	h.mustIn(dm("9", "hi"))
	if routed, _ = h.counts(); routed != guestBurst+1 {
		t.Fatalf("window did not reopen: %d routed", routed)
	}
	// A linked user is never throttled as a guest.
	h.identity("8", store.RoleUser)
	for i := 0; i < guestBurst+5; i++ {
		h.mustIn(dm("8", "hi"))
	}
	if routed, _ = h.counts(); routed != guestBurst+1+guestBurst+5 {
		t.Fatalf("a known user was throttled: %d routed", routed)
	}
}

// --- rooms -----------------------------------------------------------------

func rooms(mode, reply string) config.Access {
	return config.Access{DM: "invite", Rooms: mode, Reply: reply}
}

func TestRoomsOffDropsAndTracesHourly(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("off", "mention"))
	h.identity("7", store.RoleOwner)
	for i := 0; i < 5; i++ {
		h.mustIn(roomMsg("7", "hi @bot", true))
	}
	if routed, _ := h.counts(); routed != 0 {
		t.Fatal("a room message passed a disabled rooms knob")
	}
	d := h.dropped()
	if len(d) != 1 || d[0]["reason"] != "rooms_disabled" {
		t.Fatalf("dropped = %v", d)
	}
	h.advance(roomDropEvery + time.Minute)
	h.mustIn(roomMsg("7", "hi @bot", true))
	if len(h.dropped()) != 2 {
		t.Fatalf("hourly window did not reopen: %v", h.dropped())
	}
}

func TestApprovedRoomsNeedApproval(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("approved", "always"))
	h.identity("7", store.RoleUser) // a plain user, not an admin
	h.mustIn(roomMsg("7", "hi @bot", true))
	if routed, _ := h.counts(); routed != 0 {
		t.Fatal("an unapproved room routed")
	}
	ok, err := h.roomApproved(context.Background(), "tg-main", "g1")
	if err != nil || ok {
		t.Fatalf("approved = %v (%v)", ok, err)
	}
	d := h.dropped()
	if len(d) != 1 || d[0]["reason"] != "room_not_approved" {
		t.Fatalf("dropped = %v", d)
	}

	owner := h.identity("1", store.RoleOwner)
	if err := h.approveRoom(context.Background(), "tg-main", "g1", owner.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	h.mustIn(roomMsg("7", "hi again", false))
	if routed, _ := h.counts(); routed != 1 {
		t.Fatal("an approved room did not route")
	}
}

func TestAdminMentionAutoApprovesRoom(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("approved", "mention"))
	admin := h.identity("5", store.RoleAdmin)

	// A stranger's mention approves nothing.
	h.mustIn(roomMsg("stranger", "hey @bot", true))
	if ok, _ := h.roomApproved(context.Background(), "tg-main", "g1"); ok {
		t.Fatal("a stranger approved the room")
	}
	// An unmentioned admin message approves nothing either.
	h.mustIn(roomMsg("5", "chatting", false))
	if ok, _ := h.roomApproved(context.Background(), "tg-main", "g1"); ok {
		t.Fatal("an unmentioned admin approved the room")
	}
	if routed, _ := h.counts(); routed != 0 {
		t.Fatalf("routed %d before approval", routed)
	}

	h.mustIn(roomMsg("5", "@bot join us", true))
	ok, err := h.roomApproved(context.Background(), "tg-main", "g1")
	if err != nil || !ok {
		t.Fatalf("admin mention did not approve: %v (%v)", ok, err)
	}
	if routed, _ := h.counts(); routed != 1 {
		t.Fatal("the approving message was not routed")
	}
	// The auto-approve is traced like every other way in, with its approver.
	if by := h.approvedBy(t); by != string(admin.ID) {
		t.Fatalf("approved_by = %q, want %q", by, admin.ID)
	}
}

func TestOpenRoomMakesUnknownSendersGuests(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("open", "always"))
	h.mustIn(roomMsg("stranger", "hello room", false))
	if routed, _ := h.counts(); routed != 1 {
		t.Fatal("open room did not route")
	}
	i, err := h.au.Resolve(context.Background(), "tg-main", []string{"stranger"})
	if err != nil || !i.Pending() {
		t.Fatalf("guest = %+v (%v)", i, err)
	}
}

// Members of an approved room also talk as guests unless linked.
func TestApprovedRoomMakesUnknownSendersGuests(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("approved", "always"))
	if err := h.approveRoom(context.Background(), "tg-main", "g1", "someone"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	h.mustIn(roomMsg("stranger", "hello room", false))
	if routed, _ := h.counts(); routed != 1 {
		t.Fatal("approved room did not route")
	}
	i, err := h.au.Resolve(context.Background(), "tg-main", []string{"stranger"})
	if err != nil || !i.Pending() {
		t.Fatalf("guest = %+v (%v)", i, err)
	}
}

func TestUnmentionedRoomMessageIsStoredNotRouted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("open", "mention"))
	h.mustIn(roomMsg("stranger", "just chatting", false))
	if routed, _ := h.counts(); routed != 0 {
		t.Fatal("an unmentioned message started a turn")
	}
	rec := h.received()
	if len(rec) != 1 || rec[0]["stored_only"] != true {
		t.Fatalf("channel.received = %v", rec)
	}

	ctx := context.Background()
	sessions, err := h.st.ListSessions(ctx)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions = %v (%v)", sessions, err)
	}
	msgs, err := h.st.ListMessages(ctx, sessions[0].ID, "", 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("stored %d messages, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Role != event.RoleUser || m.Text != "just chatting" || m.TraceID == "" {
		t.Fatalf("message = %+v", m)
	}
	if m.Sender != "Nina <b>" {
		t.Fatalf("sender = %q, want the raw display name", m.Sender)
	}
	if pending, err := h.st.SessionsWithPending(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("stored message was pending: %v (%v)", pending, err)
	}

	// The mention that follows lands in the same session, through the router.
	h.mustIn(roomMsg("stranger", "@bot what do you think", true))
	if routed, _ := h.counts(); routed != 1 {
		t.Fatal("the mention was not routed")
	}
}

func TestReplyAlwaysRoutesEverything(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("open", "always"))
	h.mustIn(roomMsg("stranger", "just chatting", false))
	if routed, _ := h.counts(); routed != 1 {
		t.Fatal("reply=always did not route an unmentioned message")
	}
	sessions, _ := h.st.ListSessions(context.Background())
	if len(sessions) != 0 {
		t.Fatalf("the gate stored a routed message: %v", sessions)
	}
}

// --- window -----------------------------------------------------------------

func TestWindowStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	sess, _, err := st.EnsureSession(ctx, store.Session{Key: "main/tg/room/g1", Agent: "main", Kind: store.SessionChat})
	if err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	add := func(role event.Role, text string) event.ID {
		t.Helper()
		m := event.Message{
			Schema: event.SchemaVersion, ID: event.NewID(), SessionID: sess.ID,
			TraceID: event.NewID(), Role: role, Text: text, CreatedAt: time.Now(),
		}
		if _, err := st.AppendMessage(ctx, m, false); err != nil {
			t.Fatalf("append: %v", err)
		}
		return m.ID
	}

	// A short backlog needs no cursor.
	for i := 0; i < 5; i++ {
		add(event.RoleUser, "chatter")
	}
	after, err := WindowStart(ctx, st, sess.ID)
	if err != nil || after != "" {
		t.Fatalf("after = %q (%v), want empty", after, err)
	}

	add(event.RoleAssistant, "hello all")
	for i := 0; i < RoomMessageNumberWindow; i++ {
		add(event.RoleUser, "chatter")
	}
	if after, err = WindowStart(ctx, st, sess.ID); err != nil || after != "" {
		t.Fatalf("exactly the window needs no cursor: %q (%v)", after, err)
	}

	var ids []event.ID
	for i := 0; i < 70; i++ {
		ids = append(ids, add(event.RoleUser, "chatter"))
	}
	after, err = WindowStart(ctx, st, sess.ID)
	if err != nil {
		t.Fatalf("window start: %v", err)
	}
	if after != ids[len(ids)-RoomMessageNumberWindow-1] {
		t.Fatalf("cursor = %q, want the id before the %dth newest", after, RoomMessageNumberWindow)
	}
	rest, err := st.ListMessages(ctx, sess.ID, after, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rest) != RoomMessageNumberWindow {
		t.Fatalf("cursor exposes %d messages, want %d", len(rest), RoomMessageNumberWindow)
	}

	// A fresh assistant reply resets the backlog.
	add(event.RoleAssistant, "answer")
	if after, err = WindowStart(ctx, st, sess.ID); err != nil || after != "" {
		t.Fatalf("after an answer: %q (%v)", after, err)
	}
}

// --- misc -------------------------------------------------------------------

// Notify is optional: a gate without one still decides.
func TestNotifyIsOptional(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("invite"))
	g := New(Deps{
		Config: func(string) (config.Access, bool) { return invite("invite"), true },
		Auth:   h.au,
		Route: func(context.Context, event.Envelope) (session.MessageAccepted, error) {
			return session.MessageAccepted{}, nil
		},
		Store: h.st,
	})
	if err := g.Inbound(context.Background(), dm("9", "hello")); err != nil {
		t.Fatalf("inbound: %v", err)
	}
}

// --- room flood guards (M4) ---------------------------------------------------

// A stranger in an open room is rate-limited exactly like one in a DM: past
// the burst nothing is written, no guest is created and no turn starts.
func TestRoomGuestBurstIsLimited(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("open", "always"))
	for i := 0; i < guestBurst; i++ {
		h.mustIn(roomMsg("flooder", "hi @bot", true))
	}
	routed, _ := h.counts()
	if routed != guestBurst {
		t.Fatalf("routed %d of the first %d messages", routed, guestBurst)
	}
	for i := 0; i < 5; i++ {
		h.mustIn(roomMsg("flooder", "hi @bot", true))
	}
	if routed, _ = h.counts(); routed != guestBurst {
		t.Fatalf("routed %d, want the burst to hold at %d", routed, guestBurst)
	}
	d := h.dropped()
	if len(d) == 0 || d[len(d)-1]["reason"] != "rate_limited" {
		t.Fatalf("dropped = %v", d)
	}

	// The window reopens.
	h.advance(guestWindow + time.Minute)
	h.mustIn(roomMsg("flooder", "hi @bot", true))
	if routed, _ = h.counts(); routed != guestBurst+1 {
		t.Fatalf("routed %d after the window reopened", routed)
	}
}

// The same limit guards the storage path: a throttled guest's unmentioned
// messages are not appended either, and the guest is created exactly once.
func TestRoomGuestBurstAlsoLimitsStoredMessages(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("open", "mention"))
	ctx := context.Background()
	for i := 0; i < guestBurst+8; i++ {
		h.mustIn(roomMsg("flooder", "just chatting", false))
	}
	sessions, err := h.st.ListSessions(ctx)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions = %v (%v)", sessions, err)
	}
	msgs, err := h.st.ListMessages(ctx, sessions[0].ID, "", 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) != guestBurst {
		t.Fatalf("stored %d messages, want the guest burst %d", len(msgs), guestBurst)
	}
	identities, err := h.st.ListIdentities(ctx)
	if err != nil {
		t.Fatalf("list identities: %v", err)
	}
	// The room, and one guest however many messages it sent.
	if len(identities) != 2 || !identities[0].Pending() || !identities[1].Pending() {
		t.Fatalf("identities = %+v, want the room and one guest", identities)
	}
}

// The unmentioned transcript of one room is capped per hour, with exactly one
// drop event per window.
func TestUnmentionedRoomStorageIsCapped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("open", "mention"))
	ctx := context.Background()
	h.identity("member", store.RoleUser) // a known sender: no guest limit in the way

	for i := 0; i < roomStoreCap+10; i++ {
		h.mustIn(roomMsg("member", "chatter", false))
	}
	sessions, err := h.st.ListSessions(ctx)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions = %v (%v)", sessions, err)
	}
	msgs, err := h.st.ListMessages(ctx, sessions[0].ID, "", 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) != roomStoreCap {
		t.Fatalf("stored %d messages, want the cap %d", len(msgs), roomStoreCap)
	}
	d := h.dropped()
	floods := 0
	for _, m := range d {
		if m["reason"] == "room_flood" {
			floods++
			if m["conversation"] != "g1" {
				t.Errorf("drop event = %v", m)
			}
		}
	}
	if floods != 1 {
		t.Fatalf("%d room_flood events, want exactly 1: %v", floods, d)
	}

	// A mention is never dropped by this cap: it starts a turn instead.
	h.mustIn(roomMsg("member", "hi @bot", true))
	if routed, _ := h.counts(); routed != 1 {
		t.Fatalf("a mention was swallowed by the storage cap")
	}

	// The window reopens: storage resumes.
	before := len(msgs)
	h.advance(roomStoreWindow + time.Minute)
	h.mustIn(roomMsg("member", "chatter again", false))
	msgs, err = h.st.ListMessages(ctx, sessions[0].ID, "", 0)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) != before+1 {
		t.Fatalf("stored %d messages after the window reopened, want %d", len(msgs), before+1)
	}
}

// --- a guest redeeming an invite ---------------------------------------------

func TestGuestRedeemsInvite(t *testing.T) {
	t.Parallel()
	for name, form := range map[string]string{"start": "/start CODE", "bare": "CODE"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, invite("open"))
			h.mustIn(dm("9", "hi"))
			guest, err := h.au.Resolve(context.Background(), "tg-main", []string{"9"})
			if err != nil || !guest.Pending() {
				t.Fatalf("sender = %+v, %v, want a guest", guest, err)
			}
			code := h.code(t, "tg-main")
			h.mustIn(dm("9", strings.Replace(form, "CODE", code, 1)))
			routed, notices := h.counts()
			if routed != 1 || len(notices) != 1 || notices[0] != noticeLinked {
				t.Fatalf("routed %d, notices %v; want the code kept from the router", routed, notices)
			}
			i, err := h.au.Resolve(context.Background(), "tg-main", []string{"9"})
			if err != nil || i.ID == guest.ID || i.Role != store.RoleUser {
				t.Fatalf("sender = %+v, %v, want the invited user", i, err)
			}
		})
	}
}

func TestGuestCodeShapedWordRoutes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("open"))
	h.mustIn(dm("9", "hi"))
	// Ten letters of Crockford's alphabet: shaped like a code, but a word.
	h.mustIn(dm("9", "everything"))
	routed, notices := h.counts()
	if routed != 2 || len(notices) != 0 {
		t.Fatalf("routed %d, notices %v; want the word routed quietly", routed, notices)
	}
}

func TestGuestRateLimitedCodeIsNotRouted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("open"))
	h.mustIn(dm("9", "hi"))
	for i := 0; i < 5; i++ {
		h.mustIn(dm("9", "/start ZZZZZZZZZZ"))
	}
	// The limiter answers before the code is checked: it may be live, so it
	// must not reach the transcript.
	h.mustIn(dm("9", h.code(t, "tg-main")))
	if routed, _ := h.counts(); routed != 1 {
		t.Fatalf("routed %d, want only the first hi", routed)
	}
}

func TestGuestBadStartCodeIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("open"))
	h.mustIn(dm("9", "hi"))
	h.mustIn(dm("9", "/start ZZZZZZZZZZ"))
	routed, notices := h.counts()
	if routed != 1 || len(notices) != 1 || notices[0] != noticeBadCode {
		t.Fatalf("routed %d, notices %v", routed, notices)
	}
}

// A live code from a confirmed user is refused and kept out of the transcript,
// and stays good for whoever it was meant for. A code-shaped word that is no
// code is an ordinary message.
func TestConfirmedUserCodeIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, invite("invite"))
	carla := h.identity("9", store.RoleUser)
	for stranger, form := range map[string]string{"10": "%s", "11": "/start %s"} {
		code := h.code(t, "tg-main")
		h.mustIn(dm("9", strings.Replace(form, "%s", code, 1)))
		routed, notices := h.counts()
		if routed != 0 || len(notices) == 0 || notices[len(notices)-1] != noticeBadCode {
			t.Fatalf("%q: routed %d, notices %v; want refused", form, routed, notices)
		}
		if b, err := h.au.Bindings(ctx, carla.ID); err != nil || len(b) != 1 {
			t.Fatalf("%q: carla's bindings = %v, %v", form, b, err)
		}
		h.mustIn(dm(stranger, "/start "+code))
		if _, notices := h.counts(); notices[len(notices)-1] != noticeLinked {
			t.Fatalf("%q: the code was spent by the refusal: %v", form, notices)
		}
	}
	h.mustIn(dm("9", "ZZZZZZZZZZ"))
	if routed, _ := h.counts(); routed != 1 {
		t.Fatalf("routed %d, want the code-shaped word", routed)
	}
}
