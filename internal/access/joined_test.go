package access

import (
	"context"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// joined is the origin the daemon builds for an adapter's join hook: the
// Telegram chat id of a supergroup, as in the telegram package's fixture.
func joined() event.Origin {
	return event.Origin{Channel: "tg-main", ChannelType: "telegram", Conversation: "g1"}
}

func (h *harness) join(by ...string) {
	h.g.OnJoined(context.Background(), joined(), by)
}

func (h *harness) approved(t *testing.T) bool {
	t.Helper()
	ok, err := h.roomApproved(context.Background(), "tg-main", "g1")
	if err != nil {
		t.Fatalf("room approved: %v", err)
	}
	return ok
}

func TestAdminAddingTheBotApprovesTheRoom(t *testing.T) {
	t.Parallel()
	for _, role := range []store.Role{store.RoleOwner, store.RoleAdmin} {
		t.Run(string(role), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, rooms("approved", "mention"))
			// Google Chat's fixture: the adder's user name and email.
			admin := h.identity("users/111", role)
			h.join("users/111", "alice@example.com")

			if !h.approved(t) {
				t.Fatal("an admin adding the bot did not approve the room")
			}
			if by := h.approvedBy(t); by != string(admin.ID) {
				t.Fatalf("approved_by = %q, want %q", by, admin.ID)
			}
			if _, notices := h.counts(); len(notices) != 1 || notices[0] != noticeRoomOn {
				t.Fatalf("notices = %v", notices)
			}
			g := h.granted()
			if len(g) != 1 || g[0]["conversation"] != "g1" || g[0]["approved_by"] != string(admin.ID) ||
				g[0]["conversation_kind"] != string(event.ConversationRoom) {
				t.Fatalf("granted = %v", g)
			}
			if len(h.dropped()) != 0 {
				t.Fatalf("dropped = %v", h.dropped())
			}

			// The room is on: a stranger's mention routes.
			h.mustIn(roomMsg("stranger", "hi @bot", true))
			if routed, _ := h.counts(); routed != 1 {
				t.Fatal("a mention in the approved room did not route")
			}
		})
	}
}

func TestOthersAddingTheBotApproveNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("approved", "mention"))
	h.identity("7", store.RoleUser)
	h.identity("g", store.RoleGuest)

	for _, by := range [][]string{{"7"}, {"g"}, {"stranger"}, nil} {
		h.join(by...)
		if h.approved(t) {
			t.Fatalf("added by %v approved the room", by)
		}
	}
	if _, notices := h.counts(); len(notices) != 0 {
		t.Fatalf("a room nobody approved was told %v", notices)
	}
	if len(h.granted()) != 0 {
		t.Fatalf("granted = %v", h.granted())
	}
	// Traced once per conversation and hour, like a disabled room's messages.
	d := h.dropped()
	if len(d) != 1 || d[0]["reason"] != "room_not_approved" || d[0]["conversation"] != "g1" {
		t.Fatalf("dropped = %v", d)
	}
	// The stranger's join created no identity.
	if _, err := h.au.Resolve(context.Background(), "tg-main", []string{"stranger"}); err == nil {
		t.Fatal("a join created an identity")
	}

	// Still off: the stranger's mention is dropped, an admin's still approves it.
	h.mustIn(roomMsg("stranger", "hi @bot", true))
	if routed, _ := h.counts(); routed != 0 {
		t.Fatal("the room routed before anyone approved it")
	}
	h.identity("5", store.RoleAdmin)
	h.mustIn(roomMsg("5", "@bot join us", true))
	if !h.approved(t) {
		t.Fatal("an admin mention after a stranger's join did not approve the room")
	}
}

func TestStrangerJoinTracesAreCappedPerAlias(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("approved", "mention"))
	for i := 0; i < dropEvents+5; i++ {
		o := joined()
		o.Conversation = "g" + string(rune('a'+i))
		h.g.OnJoined(context.Background(), o, []string{"stranger"})
	}
	if n := len(h.dropped()); n != dropEvents {
		t.Fatalf("dropped %d, want %d", n, dropEvents)
	}
}

func TestAddedToAnApprovedRoomIsQuiet(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("approved", "mention"))
	owner := h.identity("1", store.RoleOwner)
	if err := h.approveRoom(context.Background(), "tg-main", "g1", owner.ID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	h.identity("5", store.RoleAdmin)
	h.join("5")
	h.join("stranger")
	if _, notices := h.counts(); len(notices) != 0 {
		t.Fatalf("notices = %v", notices)
	}
	// The one grant is the approve above.
	if len(h.granted()) != 1 || len(h.dropped()) != 0 {
		t.Fatalf("granted = %v dropped = %v", h.granted(), h.dropped())
	}
	if by := h.approvedBy(t); by != string(owner.ID) {
		t.Fatalf("approved_by = %q, want the first approver", by)
	}
}

func TestJoinUnderOtherRoomKnobs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("off", "mention"))
	h.identity("1", store.RoleOwner)
	h.join("1")
	if h.approved(t) {
		t.Fatal("rooms = off approved a room")
	}
	if d := h.dropped(); len(d) != 1 || d[0]["reason"] != "rooms_disabled" {
		t.Fatalf("dropped = %v", d)
	}
	h.advance(time.Minute)

	h.mu.Lock()
	h.acc = rooms("open", "mention")
	h.mu.Unlock()
	h.join("1")
	h.join("stranger")
	if h.approved(t) {
		t.Fatal("rooms = open needs no row")
	}
	if _, notices := h.counts(); len(notices) != 0 || len(h.granted()) != 0 || len(h.dropped()) != 1 {
		t.Fatalf("open: notices = %v granted = %v dropped = %v", notices, h.granted(), h.dropped())
	}
}

func TestJoinOnUnknownChannelIsDropped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("approved", "mention"))
	h.identity("1", store.RoleOwner)
	o := joined()
	o.Channel = "nope"
	h.g.OnJoined(context.Background(), o, []string{"1"})
	if d := h.dropped(); len(d) != 1 || d[0]["reason"] != "unknown_channel" {
		t.Fatalf("dropped = %v", d)
	}
	if h.approved(t) {
		t.Fatal("an unknown channel approved a room")
	}
}
