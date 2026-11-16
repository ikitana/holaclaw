package access

import (
	"context"
	"errors"
	"testing"
	"time"

	"holaclaw/internal/auth"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

func (h *harness) disableRoom(t *testing.T) store.Identity {
	t.Helper()
	room, _, err := h.st.EnsureRoom(context.Background(), "tg-main", "g1", h.now())
	if err != nil {
		t.Fatal(err)
	}
	h.setStatus(t, room.ID, store.StatusDisabled)
	return room
}

func (h *harness) setStatus(t *testing.T, id event.ID, status store.IdentityStatus) {
	t.Helper()
	if _, err := h.st.DB().Exec(`UPDATE identities SET status = ? WHERE id = ?`, string(status), string(id)); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) noRoom(t *testing.T) {
	t.Helper()
	if _, err := h.st.GetRoom(context.Background(), "tg-main", "g1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("room = %v, want none", err)
	}
}

func (h *harness) lastDrop(t *testing.T) string {
	t.Helper()
	d := h.dropped()
	if len(d) == 0 {
		t.Fatal("nothing dropped")
	}
	r, _ := d[len(d)-1]["reason"].(string)
	return r
}

// The first accepted message creates the pending room; it is the session's
// owner from then on, whoever speaks, and each message keeps its own sender.
func TestFirstMessageCreatesThePendingRoom(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, rooms("open", "mention"))
	h.noRoom(t)
	first := h.identity("1", store.RoleOwner)
	h.mustIn(roomMsg("1", "just chatting", false))
	room, err := h.st.GetRoom(ctx, "tg-main", "g1")
	if err != nil {
		t.Fatal(err)
	}
	if room.Kind != store.KindRoom || !room.Pending() || room.Role != "" || room.Approved() {
		t.Fatalf("room = %+v", room)
	}

	h.mustIn(roomMsg("stranger", "me too", false))
	second, err := h.au.Resolve(ctx, "tg-main", []string{"stranger"})
	if err != nil || second.Kind != store.KindUser {
		t.Fatalf("second sender = %+v, %v", second, err)
	}

	sessions, err := h.st.ListSessions(ctx)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions = %+v, %v", sessions, err)
	}
	if sessions[0].UserID != room.ID {
		t.Fatalf("session owner = %q, want the room %q", sessions[0].UserID, room.ID)
	}
	msgs, err := h.st.ListMessages(ctx, sessions[0].ID, "", 0)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("messages = %+v, %v", msgs, err)
	}
	if msgs[0].UserID != first.ID || msgs[1].UserID != second.ID {
		t.Fatalf("senders = %q %q, want %q %q", msgs[0].UserID, msgs[1].UserID, first.ID, second.ID)
	}
}

// Stored messages use the same adoption as routed ones: a room deleted and
// back takes over its old session.
func TestStoredMessageAdoptsTheRoomSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, rooms("open", "mention"))
	h.mustIn(roomMsg("stranger", "before", false))
	old, err := h.st.GetRoom(ctx, "tg-main", "g1")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.DeleteIdentity(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	h.mustIn(roomMsg("stranger", "after", false))
	room, err := h.st.GetRoom(ctx, "tg-main", "g1")
	if err != nil || room.ID == old.ID {
		t.Fatalf("room = %+v, %v; want a new one", room, err)
	}
	sessions, err := h.st.ListSessions(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].UserID != room.ID {
		t.Fatalf("sessions = %+v, %v; want one, adopted by %s", sessions, err, room.ID)
	}
}

// Under approved, chatter in a room nobody approved creates nothing at all.
func TestApprovedChatterCreatesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, rooms("approved", "always"))
	h.identity("7", store.RoleUser)
	h.mustIn(roomMsg("stranger", "hello?", false))
	h.mustIn(roomMsg("7", "hi @bot", true))
	h.noRoom(t)
	if h.lastDrop(t) != "room_not_approved" {
		t.Fatalf("dropped = %v", h.dropped())
	}
	list, err := h.st.ListIdentities(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("identities = %+v, %v; want only the user", list, err)
	}
}

// A room created under open is not approved: when the knob flips to approved it
// fails closed until someone approves it, keeping its identity.
func TestOpenToApprovedFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, rooms("open", "always"))
	h.identity("7", store.RoleUser)
	admin := h.identity("5", store.RoleAdmin)
	h.mustIn(roomMsg("7", "hi", false))
	room, err := h.st.GetRoom(ctx, "tg-main", "g1")
	if err != nil {
		t.Fatal(err)
	}
	if routed, _ := h.counts(); routed != 1 {
		t.Fatal("open did not route")
	}

	h.mu.Lock()
	h.acc = rooms("approved", "always")
	h.mu.Unlock()
	h.mustIn(roomMsg("7", "still here", false))
	if routed, _ := h.counts(); routed != 1 {
		t.Fatal("a room created under open answered under approved")
	}
	if h.lastDrop(t) != "room_not_approved" {
		t.Fatalf("dropped = %v", h.dropped())
	}

	// An admin's mention approves that same room, with an approver on the trace.
	h.mustIn(roomMsg("5", "@bot hi", true))
	if routed, _ := h.counts(); routed != 2 {
		t.Fatal("the admin's mention did not route")
	}
	got, err := h.st.GetRoom(ctx, "tg-main", "g1")
	if err != nil || got.ID != room.ID || !got.Approved() || !got.Pending() {
		t.Fatalf("room = %+v, %v", got, err)
	}
	if by := h.approvedBy(t); by != string(admin.ID) {
		t.Fatalf("approved_by = %q", by)
	}

	// Approved once, granted once.
	h.mustIn(roomMsg("5", "@bot again", true))
	if g := h.granted(); len(g) != 1 {
		t.Fatalf("granted = %v", g)
	}
}

// An invite redeemed in a pending room approves that room rather than making
// another.
func TestRoomInviteApprovesAPendingRoom(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, rooms("open", "mention"))
	h.mustIn(roomMsg("stranger", "chat", false))
	room, err := h.st.GetRoom(ctx, "tg-main", "g1")
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.acc = rooms("approved", "mention")
	h.mu.Unlock()
	code, owner := h.roomCode(t)
	h.mustIn(roomMsg("stranger", "/start "+code, false))
	got, err := h.st.GetRoom(ctx, "tg-main", "g1")
	if err != nil || got.ID != room.ID || !got.Approved() {
		t.Fatalf("room = %+v, %v", got, err)
	}
	if by := h.approvedBy(t); by != string(owner.ID) {
		t.Fatalf("approved_by = %q", by)
	}
}

// A disabled room is dropped under any knob before an invite or an auto-approve
// gets near it, and its messages make nobody pending.
func TestDisabledRoomDropsEverything(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, mode := range []string{"open", "approved"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, rooms(mode, "always"))
			code, _ := h.roomCode(t)
			room := h.disableRoom(t)

			h.mustIn(roomMsg("stranger", "hello", true))
			h.mustIn(roomMsg("1", "@bot on please", true)) // the owner
			h.mustIn(roomMsg("stranger", "/start "+code, false))
			if routed, notices := h.counts(); routed != 0 || len(notices) != 0 {
				t.Fatalf("routed %d, notices %v", routed, notices)
			}
			if d := h.dropped(); len(d) != 1 || d[0]["reason"] != "room_disabled" {
				t.Fatalf("dropped = %v, want one, capped per hour", d)
			}
			// No identity for the stranger, the code unspent, the room still off.
			if _, err := h.au.Resolve(ctx, "tg-main", []string{"stranger"}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("a disabled room made a pending identity: %v", err)
			}
			invites, err := h.au.ListInvites(ctx)
			if err != nil || len(invites) != 1 || invites[0].Used() {
				t.Fatalf("invites = %+v, %v", invites, err)
			}
			got, err := h.st.GetRoom(ctx, "tg-main", "g1")
			if err != nil || !got.Disabled() || got.Approved() || got.ID != room.ID {
				t.Fatalf("room = %+v, %v", got, err)
			}
			if len(h.granted()) != 0 {
				t.Fatalf("granted = %v", h.granted())
			}

			// Adding the bot doesn't revive it either, nor does a reaction pass.
			h.identity("5", store.RoleAdmin)
			h.join("5")
			h.mustIn(reacted(roomMsg("5", "", false)))
			if got, _ := h.st.GetRoom(ctx, "tg-main", "g1"); !got.Disabled() || got.Approved() {
				t.Fatalf("room after a join = %+v", got)
			}
			if routed, _ := h.counts(); routed != 0 {
				t.Fatalf("routed %d", routed)
			}
		})
	}
}

// rooms = off drops before anything is read.
func TestRoomsOffCreatesNoRoom(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("off", "always"))
	h.identity("1", store.RoleOwner)
	h.mustIn(roomMsg("1", "@bot hi", true))
	h.noRoom(t)
	if h.lastDrop(t) != "rooms_disabled" {
		t.Fatalf("dropped = %v", h.dropped())
	}
}

// A disabled sender is dropped in a room and can't approve one, whatever its role.
func TestDisabledSenderInARoom(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("approved", "always"))
	admin := h.identity("5", store.RoleAdmin)
	h.setStatus(t, admin.ID, store.StatusDisabled)
	h.mustIn(roomMsg("5", "@bot hi", true))
	h.join("5")
	h.noRoom(t)

	if err := h.approveRoom(context.Background(), "tg-main", "g1", "someone"); err != nil {
		t.Fatal(err)
	}
	h.mustIn(roomMsg("5", "@bot hi", true))
	h.mustIn(reacted(roomMsg("5", "", false)))
	if routed, _ := h.counts(); routed != 0 {
		t.Fatalf("a disabled sender routed %d", routed)
	}
	if h.lastDrop(t) != "identity_disabled" {
		t.Fatalf("dropped = %v", h.dropped())
	}
}

// A conversation whose binding a user holds is dropped, never adopted, under
// every way in.
func TestRoomBindingHeldByAUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, mode := range []string{"open", "approved"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, rooms(mode, "always"))
			holder := h.identity("g1", store.RoleOwner) // alias equal to the conversation id
			h.identity("5", store.RoleAdmin)
			h.mustIn(roomMsg("5", "@bot hi", true))
			if routed, _ := h.counts(); routed != 0 {
				t.Fatal("routed")
			}
			if h.lastDrop(t) != "room_binding_held" {
				t.Fatalf("dropped = %v", h.dropped())
			}
			if i, err := h.au.Resolve(ctx, "tg-main", []string{"g1"}); err != nil || i.ID != holder.ID {
				t.Fatalf("binding moved: %+v, %v", i, err)
			}
		})
	}
}

// A room's binding is never a sender, in a DM either.
func TestRoomAliasDoesNotResolveAsASender(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("open"))
	if _, _, err := h.st.EnsureRoom(context.Background(), "tg-main", "g1", h.now()); err != nil {
		t.Fatal(err)
	}
	h.mustIn(dm("g1", "i am the room"))
	if routed, _ := h.counts(); routed != 0 {
		t.Fatal("a room's alias routed as a sender")
	}
	if h.lastDrop(t) != "unknown_sender" {
		t.Fatalf("dropped = %v", h.dropped())
	}
	// Spent on nothing: a DM code doesn't take it either.
	h2 := newHarness(t, invite("invite"))
	if _, _, err := h2.st.EnsureRoom(context.Background(), "tg-main", "g1", h2.now()); err != nil {
		t.Fatal(err)
	}
	h2.mustIn(dm("g1", "/start "+h2.code(t, "tg-main")))
	if _, notices := h2.counts(); len(notices) != 1 || notices[0] != noticeBadCode {
		t.Fatalf("notices = %v", notices)
	}
}

// One alias can only bring so many rooms in under open; others are unaffected,
// and rooms that exist take no further hits.
func TestRoomCreationIsRateLimitedPerAlias(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, rooms("open", "always"))
	inRoom := func(alias, conversation string) event.Envelope {
		env := roomMsg(alias, "hi", false)
		env.Origin.Conversation = conversation
		return env
	}
	count := func() int {
		list, err := h.st.ListIdentities(ctx)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, i := range list {
			if i.Kind == store.KindRoom {
				n++
			}
		}
		return n
	}
	for i := range roomBurst + 3 {
		h.mustIn(inRoom("stranger", "r"+string(rune('a'+i))))
	}
	if n := count(); n != roomBurst {
		t.Fatalf("rooms = %d, want the limit %d", n, roomBurst)
	}
	if h.lastDrop(t) != "rate_limited" {
		t.Fatalf("dropped = %v", h.dropped())
	}
	// The limit is on creating, not on talking where the room exists.
	h.mustIn(inRoom("stranger", "ra"))
	if routed, _ := h.counts(); routed != roomBurst+1 {
		t.Fatalf("routed %d, want %d", routed, roomBurst+1)
	}
	h.mustIn(inRoom("other", "other-room"))
	if n := count(); n != roomBurst+1 {
		t.Fatalf("another alias: rooms = %d", n)
	}
	h.advance(roomWindow + time.Minute)
	h.mustIn(inRoom("stranger", "later"))
	if n := count(); n != roomBurst+2 {
		t.Fatalf("after the window: rooms = %d", n)
	}
}

// A reaction creates no room under any knob.
func TestReactionCreatesNoRoom(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"open", "approved", "off"} {
		h := newHarness(t, rooms(mode, "always"))
		h.identity("7", store.RoleOwner)
		h.mustIn(reacted(roomMsg("7", "", true)))
		h.noRoom(t)
		if mode == "open" {
			if routed, _ := h.counts(); routed != 1 {
				t.Fatalf("open: routed %d", routed)
			}
		}
	}
}

// The kind on the invite row is the new one.
func TestRoomInviteKindIsRoom(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("approved", "mention"))
	h.roomCode(t)
	list, err := h.au.ListInvites(context.Background())
	if err != nil || len(list) != 1 || list[0].Kind != auth.InviteRoom || string(list[0].Kind) != "room" {
		t.Fatalf("invites = %+v, %v", list, err)
	}
}
