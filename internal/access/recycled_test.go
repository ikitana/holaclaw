package access

import (
	"context"
	"errors"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/store/storetest"
)

// recycledHarness declares the WhatsApp namespaces on tg-main and links an
// old account holding a phone number and its LID.
func recycledHarness(t *testing.T, dmMode string) (*harness, store.Identity) {
	t.Helper()
	h := newHarness(t, invite(dmMode))
	h.au.Namespaces = func(string) []string { return []string{"wa:", "wa:lid:"} }
	old := h.identity("wa:+1", store.RoleUser)
	storetest.Bind(t, h.st, "tg-main", []string{"wa:lid:9"}, old.ID, "")
	return h, old
}

func dmAliases(text string, aliases ...string) event.Envelope {
	env := dm(aliases[0], text)
	env.Sender.Aliases = aliases
	return env
}

func owner(t *testing.T, h *harness, alias string) event.ID {
	t.Helper()
	r, err := store.ResolveAliases(context.Background(), h.st.DB(), "tg-main", []string{alias}, nil)
	if errors.Is(err, store.ErrNotFound) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return r.Identity.ID
}

// A recycled number in open mode: the new person becomes a guest of their own
// with the LID only; the number stays with the old identity.
func TestRecycledNumberOpenModeMakesANewGuest(t *testing.T) {
	t.Parallel()
	h, old := recycledHarness(t, "open")
	env := dmAliases("hello", "wa:+1", "wa:lid:5")
	h.mustIn(env)
	if routed, _ := h.counts(); routed != 1 {
		t.Fatal("not routed")
	}
	i, err := h.au.Resolve(context.Background(), "tg-main", env.Sender.Aliases)
	if err != nil {
		t.Fatal(err)
	}
	if i.ID == old.ID || !i.Pending() {
		t.Fatalf("resolved to %+v, want a new guest", i)
	}
	if got := owner(t, h, "wa:lid:5"); got != i.ID {
		t.Fatalf("wa:lid:5 owner = %q, want the guest", got)
	}
	if got := owner(t, h, "wa:+1"); got != old.ID {
		t.Fatalf("number moved to %q", got)
	}
	// The next message from the same person finds the same guest.
	h.mustIn(env)
	if again, _ := h.au.Resolve(context.Background(), "tg-main", env.Sender.Aliases); again.ID != i.ID {
		t.Fatalf("second message resolved to %q, want %q", again.ID, i.ID)
	}
}

// In invite mode the recycled sender is a stranger like any other.
func TestRecycledNumberInviteModeIsDropped(t *testing.T) {
	t.Parallel()
	h, _ := recycledHarness(t, "invite")
	h.mustIn(dmAliases("hello", "wa:+1", "wa:lid:5"))
	routed, notices := h.counts()
	if routed != 0 || len(notices) != 1 || notices[0] != noticeInviteOnly {
		t.Fatalf("routed %d, notices %v", routed, notices)
	}
	if d := h.dropped(); len(d) != 1 || d[0]["reason"] != "unknown_sender" {
		t.Fatalf("dropped = %v", d)
	}
	if got := owner(t, h, "wa:lid:5"); got != "" {
		t.Fatalf("wa:lid:5 linked to %q", got)
	}
}

// When every alias sits on an identity the match does not trust, there is
// nothing to give a guest: the message is dropped, not retried.
func TestRecycledNumberWithNoFreeAliasIsDropped(t *testing.T) {
	t.Parallel()
	h, _ := recycledHarness(t, "open")
	other := h.identity("wa:lid:5", store.RoleUser)
	storetest.Bind(t, h.st, "tg-main", []string{"wa:+2"}, other.ID, "")
	if err := h.in(dmAliases("hello", "wa:+1", "wa:lid:5")); err != nil {
		t.Fatalf("inbound: %v", err)
	}
	if routed, _ := h.counts(); routed != 0 {
		t.Fatal("routed")
	}
	if d := h.dropped(); len(d) != 1 || d[0]["reason"] != "unknown_sender" {
		t.Fatalf("dropped = %v", d)
	}
}

// The same holds in an open room.
func TestRecycledNumberInRoomMakesANewGuest(t *testing.T) {
	t.Parallel()
	h, old := recycledHarness(t, "invite")
	h.acc.Rooms = "open"
	env := roomMsg("wa:+1", "hello", true)
	env.Sender.Aliases = []string{"wa:+1", "wa:lid:5"}
	h.mustIn(env)
	if routed, _ := h.counts(); routed != 1 {
		t.Fatal("not routed")
	}
	if got := owner(t, h, "wa:lid:5"); got == "" || got == old.ID {
		t.Fatalf("wa:lid:5 owner = %q, want a new guest", got)
	}
}
