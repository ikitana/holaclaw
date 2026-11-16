package auth

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/store/storetest"
)

// Resolve, ResolveAndLink and EnsureGuest apply the channel's namespaces: a
// match through a recycled number is no match, and the guest made for the new
// person takes only the aliases nobody holds.
func TestRecycledNumberGuard(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	s.Namespaces = func(ch string) []string {
		if ch == "wa" {
			return []string{"wa:", "wa:lid:"}
		}
		return nil
	}
	old, err := s.CreateIdentity(ctx, "old", store.RoleUser, EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	storetest.Bind(t, s.st, "wa", []string{"wa:+1", "wa:lid:9"}, old.ID, "")
	recycled := []string{"wa:+1", "wa:lid:5"}

	if _, err := s.Resolve(ctx, "wa", recycled); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resolve: %v, want ErrNotFound", err)
	}
	if r, err := s.ResolveAndLink(ctx, "wa", event.Sender{Aliases: recycled}); !errors.Is(err, store.ErrNotFound) || len(r.Conflicts) != 1 {
		t.Fatalf("resolve and link: %+v, %v", r, err)
	}
	g, err := s.EnsureGuest(ctx, "wa", recycled, "")
	if err != nil {
		t.Fatal(err)
	}
	if g.ID == old.ID || !g.Pending() || g.Name != placeholderName(g.ID) {
		t.Fatalf("guest = %+v", g)
	}
	ids, err := s.Bindings(ctx, g.ID)
	if err != nil || len(ids) != 1 || ids[0].Alias != "wa:lid:5" {
		t.Fatalf("guest identities = %+v, %v", ids, err)
	}
	if again, err := s.EnsureGuest(ctx, "wa", recycled, ""); err != nil || again.ID != g.ID {
		t.Fatalf("second ensure = %+v, %v", again, err)
	}
	// The old person still resolves, and on a channel with no namespaces the
	// same aliases are no conflict at all.
	if i, err := s.Resolve(ctx, "wa", []string{"wa:+1", "wa:lid:9"}); err != nil || i.ID != old.ID {
		t.Fatalf("old resolve = %+v, %v", i, err)
	}
	storetest.Bind(t, s.st, "tg", []string{"wa:+1", "wa:lid:9"}, old.ID, "")
	r, err := s.ResolveAndLink(ctx, "tg", event.Sender{Aliases: recycled})
	if err != nil || r.Identity.ID != old.ID || len(r.Free) != 1 {
		t.Fatalf("no namespaces: %+v, %v", r, err)
	}
}

// An invite redeemed through a recycled number must not carry the old pending
// identity along: the redeem is refused and nothing moves. The old person
// redeeming with their own ids still takes their pending identity along.
func TestRedeemRefusesUntrustedPendingIdentity(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	s.Namespaces = func(string) []string { return []string{"wa:", "wa:lid:"} }
	owner, err := initOwner(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	old, err := s.EnsureGuest(ctx, "wa", []string{"wa:+1", "wa:lid:9"}, "")
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "wa", UserID: newUser(t, s, "Old").ID, CreatedBy: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemInvite(ctx, code, "wa", []string{"wa:+1", "wa:lid:5"}, "Mallory"); !errors.Is(err, store.ErrAliasLinked) {
		t.Fatalf("err = %v, want ErrAliasLinked", err)
	}
	ids, err := s.Bindings(ctx, old.ID)
	if err != nil || len(ids) != 2 {
		t.Fatalf("old identities = %+v, %v", ids, err)
	}
	if _, err := s.Resolve(ctx, "wa", []string{"wa:lid:5"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("wa:lid:5 linked: %v", err)
	}
	// The refused redeem did not spend the code.
	i, err := s.RedeemInvite(ctx, code, "wa", []string{"wa:+1", "wa:lid:9"}, "Old")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Resolve(ctx, "wa", []string{"wa:lid:9"}); err != nil || got.ID != i.ID {
		t.Fatalf("old ids resolve to %+v, %v; want the invited user", got, err)
	}
}

// With every alias held by identities the match does not trust, there is no
// guest to make.
func TestEnsureGuestNoFreeAlias(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	s.Namespaces = func(string) []string { return []string{"wa:", "wa:lid:"} }
	a, _ := s.CreateIdentity(ctx, "a", store.RoleUser, EmptyEventID)
	b, _ := s.CreateIdentity(ctx, "b", store.RoleUser, EmptyEventID)
	storetest.Bind(t, s.st, "wa", []string{"wa:+1", "wa:lid:9"}, a.ID, "")
	storetest.Bind(t, s.st, "wa", []string{"wa:+2", "wa:lid:5"}, b.ID, "")
	if _, err := s.EnsureGuest(ctx, "wa", []string{"wa:+1", "wa:lid:5"}, ""); !errors.Is(err, store.ErrAliasLinked) {
		t.Fatalf("err = %v, want ErrAliasLinked", err)
	}
}

// link runs linkIn in its own transaction, the way a caller's write would.
func link(s *Service, aliases []string, id event.ID, display string) error {
	ctx := context.Background()
	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := linkIn(ctx, tx, "tg", aliases, id, display, s.now())
		return err
	})
}

// A room keeps its one binding: nothing links an alias onto it.
func TestLinkRefusesARoom(t *testing.T) {
	s, _ := newService(t)
	room := newRoom(t, s, "-100")
	if err := link(s, []string{"7"}, room.ID, ""); !errors.Is(err, ErrRoomIdentity) {
		t.Fatalf("err = %v, want ErrRoomIdentity", err)
	}
	if _, err := s.Resolve(context.Background(), "tg", []string{"7"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resolve = %v, want the alias still free", err)
	}
}

// A conflict on one alias rolls back the aliases linked before it, and the
// display is stored sanitized, empty when nothing is left of it.
func TestLinkIsAtomicAndSanitizes(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	a, _ := s.CreateIdentity(ctx, "a", store.RoleUser, EmptyEventID)
	b, _ := s.CreateIdentity(ctx, "b", store.RoleUser, EmptyEventID)
	storetest.Bind(t, s.st, "tg", []string{"held"}, b.ID, "")
	if err := link(s, []string{"1", "2", "held"}, a.ID, "Ana"); !errors.Is(err, store.ErrAliasLinked) {
		t.Fatalf("err = %v, want ErrAliasLinked", err)
	}
	if held, err := s.Bindings(ctx, a.ID); err != nil || len(held) != 0 {
		t.Fatalf("bindings after the conflict = %+v, %v", held, err)
	}
	if err := link(s, []string{"1"}, a.ID, "An"+string(rune(0x202e))+"a]"); err != nil {
		t.Fatal(err)
	}
	if err := link(s, []string{"2"}, a.ID, " "+string(rune(0x200b))+" "); err != nil {
		t.Fatal(err)
	}
	held, err := s.Bindings(ctx, a.ID)
	if err != nil || len(held) != 2 || held[0].Display != "Ana" || held[1].Display != "" {
		t.Fatalf("bindings = %+v, %v", held, err)
	}
}
