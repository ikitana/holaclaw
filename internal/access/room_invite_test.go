package access

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"holaclaw/internal/auth"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// roomCode mints a room invite for tg-main, issued by the owner.
func (h *harness) roomCode(t *testing.T) (string, store.Identity) {
	t.Helper()
	owner := h.identity("1", store.RoleOwner)
	code, _, err := h.au.CreateInvite(context.Background(),
		auth.InviteSpec{Kind: auth.InviteRoom, Channel: "tg-main", CreatedBy: owner.ID})
	if err != nil {
		t.Fatalf("create room invite: %v", err)
	}
	return code, owner
}

func (h *harness) approvedBy(t *testing.T) string {
	t.Helper()
	g := h.granted()
	if len(g) == 0 {
		t.Fatal("no access.granted event")
	}
	by, _ := g[0]["approved_by"].(string)
	// The room itself remembers the approver the trace names.
	room, err := h.st.GetRoom(context.Background(), "tg-main", "g1")
	if err != nil || string(room.ApprovedBy) != by {
		t.Fatalf("room approved_by = %q, %v; want %q", room.ApprovedBy, err, by)
	}
	return by
}

func (h *harness) granted() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, e := range h.events {
		if e.Type == event.AccessGranted {
			var m map[string]any
			if err := json.Unmarshal(e.Attrs, &m); err != nil {
				h.t.Fatalf("attrs: %v", err)
			}
			out = append(out, m)
		}
	}
	return out
}

func TestRoomInviteApprovesTheRoom(t *testing.T) {
	t.Parallel()
	forms := map[string]struct {
		text      string
		mentioned bool
	}{
		// What the startgroup deep link sends, with and without the bot name.
		"start":        {"/start CODE", false},
		"start at bot": {"/start@holaclaw CODE", true},
		// Typed by hand after adding the bot, addressed to it.
		"mentioned bare": {"CODE", true},
	}
	for name, f := range forms {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, rooms("approved", "mention"))
			code, owner := h.roomCode(t)
			// The sender is a stranger: who taps the link does not matter.
			h.mustIn(roomMsg("stranger", strings.Replace(f.text, "CODE", code, 1), f.mentioned))

			routed, notices := h.counts()
			if routed != 0 {
				t.Fatal("the code message was routed")
			}
			if len(notices) != 1 || notices[0] != noticeRoomOn {
				t.Fatalf("notices = %v", notices)
			}
			if by := h.approvedBy(t); by != string(owner.ID) {
				t.Fatalf("approved_by = %q, want the invite's creator %q", by, owner.ID)
			}
			// The redemption is traced: which invite, which room, who sent it.
			list, err := h.au.ListInvites(context.Background())
			if err != nil || len(list) != 1 {
				t.Fatalf("invites = %+v, err %v", list, err)
			}
			g := h.granted()
			if len(g) != 1 || g[0]["invite_id"] != string(list[0].ID) || g[0]["conversation"] != "g1" ||
				g[0]["channel"] != "tg-main" {
				t.Fatalf("access.granted = %v", g)
			}
			if a, _ := g[0]["sender_aliases"].([]any); len(a) != 1 || a[0] != "stranger" {
				t.Fatalf("sender_aliases = %v", g[0]["sender_aliases"])
			}
			// The redeemer was not linked; they talk as a guest from now on.
			if _, err := h.au.Resolve(context.Background(), "tg-main", []string{"stranger"}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("resolve redeemer err = %v, want ErrNotFound", err)
			}
			h.mustIn(roomMsg("stranger", "hi @bot", true))
			if routed, _ := h.counts(); routed != 1 {
				t.Fatal("the approved room did not route")
			}
		})
	}
}

// A bare code nobody addressed to the bot is room chatter, not a credential.
func TestRoomInviteUnaddressedBareCodeIsIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("approved", "mention"))
	code, _ := h.roomCode(t)
	h.mustIn(roomMsg("stranger", code, false))
	if ok, _ := h.roomApproved(context.Background(), "tg-main", "g1"); ok {
		t.Fatal("an unaddressed code approved the room")
	}
	if _, notices := h.counts(); len(notices) != 0 {
		t.Fatalf("notices = %v", notices)
	}
	// Still unused.
	h.mustIn(roomMsg("stranger", "/start "+code, false))
	if ok, _ := h.roomApproved(context.Background(), "tg-main", "g1"); !ok {
		t.Fatal("the code was spent by the chatter")
	}
}

// "/start@otherbot <code>" is addressed to another bot: not redeemed, not
// counted as a guess, and the code stays usable.
func TestRoomInviteForAnotherBotIsIgnored(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("approved", "mention"))
	code, _ := h.roomCode(t)
	for i := 0; i < 6; i++ {
		h.mustIn(roomMsg("stranger", "/start@otherbot "+code, false))
	}
	if ok, _ := h.roomApproved(context.Background(), "tg-main", "g1"); ok {
		t.Fatal("a command for another bot approved the room")
	}
	if _, notices := h.counts(); len(notices) != 0 {
		t.Fatalf("notices = %v", notices)
	}
	for _, d := range h.dropped() {
		if d["reason"] != "room_not_approved" {
			t.Fatalf("dropped = %v", d)
		}
	}
	h.mustIn(roomMsg("stranger", "/start "+code, false))
	if ok, _ := h.roomApproved(context.Background(), "tg-main", "g1"); !ok {
		t.Fatal("the code did not survive")
	}
}

// A DM code sent in a room is refused and stays usable in a DM; a room code
// sent in a DM links nobody.
func TestInviteKindsStayOnTheirPath(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("approved", "mention"))
	dmCode := h.code(t, "tg-main")
	h.mustIn(roomMsg("stranger", "/start "+dmCode, false))
	if ok, _ := h.roomApproved(context.Background(), "tg-main", "g1"); ok {
		t.Fatal("a DM code approved a room")
	}
	if _, notices := h.counts(); len(notices) != 0 {
		t.Fatalf("notices = %v", notices)
	}
	h.mustIn(dm("9", "/start "+dmCode))
	if _, notices := h.counts(); notices[len(notices)-1] != noticeLinked {
		t.Fatalf("the DM code did not survive: %v", notices)
	}

	roomCode, _ := h.roomCode(t)
	h.mustIn(dm("8", "/start "+roomCode))
	if _, notices := h.counts(); notices[len(notices)-1] != noticeBadCode {
		t.Fatalf("a room code in a DM: %v", notices)
	}
	if _, err := h.au.Resolve(context.Background(), "tg-main", []string{"8"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("room code linked a DM sender: %v", err)
	}
}

// A wrong code in a room nobody approved is traced, never answered: anyone can
// add the bot to a room of their own.
func TestRoomInviteInvalidAndRateLimited(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("approved", "mention"))
	for i := 0; i < 5; i++ {
		h.mustIn(roomMsg("stranger", "/start ZZZZZZZZZZ", false))
	}
	if _, notices := h.counts(); len(notices) != 0 {
		t.Fatalf("notices = %v", notices)
	}
	d := h.dropped()
	if d[0]["reason"] != "invite_invalid" {
		t.Fatalf("dropped = %v", d)
	}
	// The room has burnt its guesses, even for another member.
	h.mustIn(roomMsg("other", "/start ZZZZZZZZZZ", false))
	if _, notices := h.counts(); len(notices) != 0 {
		t.Fatalf("rate-limited attempt answered: %v", notices)
	}
	if d := h.dropped(); d[len(d)-1]["reason"] != "rate_limited" {
		t.Fatalf("last drop = %v", d[len(d)-1])
	}
}

// With rooms off the gate never reads a code: the knob comes first.
func TestRoomInviteNeedsApprovedRooms(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("off", "mention"))
	code, _ := h.roomCode(t)
	h.mustIn(roomMsg("stranger", "/start "+code, false))
	if ok, _ := h.roomApproved(context.Background(), "tg-main", "g1"); ok {
		t.Fatal("rooms = off approved a room")
	}
	if d := h.dropped(); len(d) != 1 || d[0]["reason"] != "rooms_disabled" {
		t.Fatalf("dropped = %v", d)
	}
}

// Once the room is approved, messages take the ordinary path.
func TestRoomInviteInAnApprovedRoomIsOrdinary(t *testing.T) {
	t.Parallel()
	h := newHarness(t, rooms("approved", "always"))
	code, owner := h.roomCode(t)
	if err := h.approveRoom(context.Background(), "tg-main", "g1", owner.ID); err != nil {
		t.Fatal(err)
	}
	h.mustIn(roomMsg("stranger", "/start "+code, false))
	if routed, _ := h.counts(); routed != 1 {
		t.Fatal("an approved room did not route")
	}
}
