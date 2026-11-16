package access

import (
	"context"
	"testing"

	"holaclaw/internal/config"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// reacted turns an envelope into a reaction to a bot message. Its Message
// holds an invite command to show that a reaction never redeems one.
func reacted(env event.Envelope) event.Envelope {
	env.Message = event.Message{Text: "/start ABCD-EFGH"}
	env.Reaction = &event.Reaction{TargetMsgID: "100", Emoji: "👍"}
	return env
}

// A stranger's reaction never introduces them: no invite redeemed, no guest,
// no notice, in any DM mode or an open room.
func TestStrangerReactionIsDroppedQuietly(t *testing.T) {
	t.Parallel()
	for _, acc := range []config.Access{invite("invite"), invite("open"), rooms("open", "always")} {
		h := newHarness(t, acc)
		env := reacted(dm("9", ""))
		if acc.Rooms == "open" {
			env = reacted(roomMsg("9", "", true))
		}
		h.mustIn(env)
		routed, notices := h.counts()
		if routed != 0 || len(notices) != 0 {
			t.Fatalf("%+v: routed %d, notices %v", acc, routed, notices)
		}
		if ids, _ := h.st.ListIdentities(context.Background()); len(ids) != 0 {
			t.Fatalf("%+v: a user was made: %+v", acc, ids)
		}
		if d := h.dropped(); len(d) != 1 || d[0]["reason"] != "unknown_sender" {
			t.Fatalf("%+v: dropped = %v", acc, d)
		}
	}
}

// A known sender's reaction routes in a DM and in an approved room, whatever
// the reply knob says: it is not a message to answer.
func TestKnownReactionRoutes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("open", "mention"))
	h.identity("7", store.RoleUser)
	h.mustIn(reacted(dm("7", "")))
	h.mustIn(reacted(roomMsg("7", "", false)))
	if routed, notices := h.counts(); routed != 2 || len(notices) != 0 {
		t.Fatalf("routed %d, notices %v", routed, notices)
	}
}

// A room that is not approved drops reactions like messages, and a reaction
// never auto-approves it, even from an owner.
func TestReactionInDisabledRoomIsDropped(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"off", "approved"} {
		h := newHarness(t, rooms(mode, "mention"))
		h.identity("7", store.RoleOwner)
		h.mustIn(reacted(roomMsg("7", "", true)))
		if routed, _ := h.counts(); routed != 0 {
			t.Fatalf("%s: routed", mode)
		}
		if ok, _ := h.roomApproved(context.Background(), "tg-main", "g1"); ok {
			t.Fatalf("%s: a reaction approved the room", mode)
		}
	}
}

// In an approved-rooms instance a reaction routes once the room is
// approved, and only then.
func TestReactionInApprovedRoom(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("approved", "mention"))
	owner := h.identity("7", store.RoleOwner)
	h.mustIn(reacted(roomMsg("7", "", false)))
	if routed, _ := h.counts(); routed != 0 {
		t.Fatal("routed before the room was approved")
	}
	if d := h.dropped(); len(d) != 1 || d[0]["reason"] != "room_not_approved" {
		t.Fatalf("dropped = %v", d)
	}
	if err := h.approveRoom(context.Background(), "tg-main", "g1", owner.ID); err != nil {
		t.Fatal(err)
	}
	h.mustIn(reacted(roomMsg("7", "", false)))
	if routed, notices := h.counts(); routed != 1 || len(notices) != 0 {
		t.Fatalf("approved: routed %d, notices %v", routed, notices)
	}
}

// A guest's reactions count against the guest limit, without a notice.
func TestGuestReactionsAreLimitedSilently(t *testing.T) {
	t.Parallel()
	h := newHarness(t, invite("open"))
	h.identity("5", store.RoleGuest)
	for range guestBurst + 3 {
		h.mustIn(reacted(dm("5", "")))
	}
	routed, notices := h.counts()
	if routed != guestBurst || len(notices) != 0 {
		t.Fatalf("routed %d, notices %v", routed, notices)
	}
}
