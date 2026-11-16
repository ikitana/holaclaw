package auth

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/store/storetest"
)

// newUser creates a confirmed user identity with no bindings.
func newUser(t *testing.T, s *Service, name string) store.Identity {
	t.Helper()
	i, err := s.CreateIdentity(context.Background(), name, store.RoleUser, EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestInviteBindsTheInvitedIdentity(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	owner, err := initOwner(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	ana, err := s.CreateIdentity(ctx, "Ana", store.RoleAdmin, EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	code, inv, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: ana.ID, CreatedBy: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != inviteCodeLen {
		t.Fatalf("code %q has length %d, want %d", code, len(code), inviteCodeLen)
	}
	for _, r := range code {
		if !strings.ContainsRune(crockford, r) {
			t.Fatalf("code %q is not Crockford base32", code)
		}
	}
	if !inv.ExpiresAt.Equal(inv.CreatedAt.Add(DefaultInviteTTL)) || DefaultInviteTTL != 15*time.Minute {
		t.Fatalf("expiry = %v, want +15m from %v", inv.ExpiresAt, inv.CreatedAt)
	}
	if inv.Kind != InviteDM || inv.Channel != "tg" || inv.UserID != ana.ID || inv.Scope != "" {
		t.Fatalf("invite = %+v", inv)
	}

	i, err := s.RedeemInvite(ctx, code, "tg", []string{"1234", "@ana"}, "Ana R.")
	if err != nil {
		t.Fatal(err)
	}
	if i.ID != ana.ID || i.Role != store.RoleAdmin || i.Name != "Ana" {
		t.Fatalf("identity = %+v, want ana with her role and name", i)
	}
	ids, err := s.Bindings(ctx, i.ID)
	if err != nil || len(ids) != 2 || ids[0].Display != "Ana R." {
		t.Fatalf("bindings = %v, err %v", ids, err)
	}
	if got, err := s.Resolve(ctx, "tg", []string{"@ana"}); err != nil || got.ID != ana.ID {
		t.Fatalf("resolve = %+v, err %v", got, err)
	}
	identities, err := s.st.ListIdentities(ctx)
	if err != nil || len(identities) != 2 {
		t.Fatalf("identities = %v, err %v; a redeem creates none", identities, err)
	}
	// A code is one-shot.
	if _, err := s.RedeemInvite(ctx, code, "tg", []string{"5678"}, "Bo"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("reuse err = %v, want ErrInviteInvalid", err)
	}
	list, err := s.ListInvites(ctx)
	if err != nil || len(list) != 1 || !list[0].Used() || list[0].UserID != ana.ID {
		t.Fatalf("invites = %+v, err %v", list, err)
	}
	// Lower case and separators are accepted.
	code2, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: ana.ID, CreatedBy: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemInvite(ctx, strings.ToLower(code2[:5])+"-"+code2[5:], "tg", []string{"999"}, "Cy"); err != nil {
		t.Fatalf("normalised redeem: %v", err)
	}
}

// An invite names one channel and, unless it is a room's, one confirmed user
// identity.
func TestInviteSpecIsRequired(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	bo := newUser(t, s, "Bo")
	guest, err := s.EnsureGuest(ctx, "tg", []string{"7"}, "stranger")
	if err != nil {
		t.Fatal(err)
	}
	off := newUser(t, s, "Off")
	if _, err := s.Disable(ctx, off.ID, EmptyEventID); err != nil {
		t.Fatal(err)
	}
	room := newRoom(t, s, "-100")
	for name, tc := range map[string]struct {
		spec InviteSpec
		want error
	}{
		"no channel":           {InviteSpec{UserID: bo.ID}, ErrInvalidInvite},
		"no identity":          {InviteSpec{Channel: "tg"}, ErrInvalidInvite},
		"a malformed identity": {InviteSpec{Channel: "tg", UserID: "nosuchuser"}, ErrInvalidInvite},
		"an unknown identity":  {InviteSpec{Channel: "tg", UserID: event.NewID()}, store.ErrNotFound},
		"a pending identity":   {InviteSpec{Channel: "tg", UserID: guest.ID}, ErrPending},
		"a disabled identity":  {InviteSpec{Channel: "tg", UserID: off.ID}, ErrInvalidInvite},
		"a room identity":      {InviteSpec{Channel: "tg", UserID: room.ID}, ErrRoomIdentity},
		"an unknown kind":      {InviteSpec{Kind: "group", Channel: "tg"}, ErrInvalidInvite},
		"a room naming one":    {InviteSpec{Kind: InviteRoom, Channel: "tg", UserID: bo.ID}, ErrInvalidInvite},
		"a room, no channel":   {InviteSpec{Kind: InviteRoom}, ErrInvalidInvite},
	} {
		if _, _, err := s.CreateInvite(ctx, tc.spec); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
	if list, err := s.ListInvites(ctx); err != nil || len(list) != 0 {
		t.Fatalf("invites = %+v, %v; a refused spec stores nothing", list, err)
	}
}

// Only the owner mints an invite for the owner.
func TestInviteForTheOwner(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	owner, err := initOwner(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, by := range []store.Role{store.RoleAdmin, store.RoleUser, ""} {
		spec := InviteSpec{Channel: "tg", UserID: owner.ID, CreatorRole: by}
		if _, _, err := s.CreateInvite(ctx, spec); !errors.Is(err, ErrOwnerOnly) {
			t.Fatalf("creator role %q: err = %v, want ErrOwnerOnly", by, err)
		}
	}
	code, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: owner.ID, CreatedBy: owner.ID, CreatorRole: store.RoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	i, err := s.RedeemInvite(ctx, code, "tg", []string{"42"}, "Angel")
	if err != nil || i.ID != owner.ID || i.Role != store.RoleOwner {
		t.Fatalf("redeemed as %+v, %v; want the owner", i, err)
	}
}

// The TTL defaults to 15 minutes and is refused above an hour, or above a day
// onto an identity with no bindings yet.
func TestInviteTTLBounds(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	fresh := newUser(t, s, "Fresh")
	bound := newUser(t, s, "Bound")
	storetest.Bind(t, s.st, "tg", []string{"1"}, bound.ID, "")
	for name, tc := range map[string]struct {
		spec InviteSpec
		ttl  time.Duration // 0: refused
	}{
		"default":                {InviteSpec{UserID: bound.ID}, DefaultInviteTTL},
		"the cap, bound":         {InviteSpec{UserID: bound.ID, TTL: time.Hour}, time.Hour},
		"over the cap, bound":    {InviteSpec{UserID: bound.ID, TTL: time.Hour + time.Second}, 0},
		"a day, no bindings":     {InviteSpec{UserID: fresh.ID, TTL: 24 * time.Hour}, 24 * time.Hour},
		"over a day, no binding": {InviteSpec{UserID: fresh.ID, TTL: 24*time.Hour + time.Second}, 0},
		"negative":               {InviteSpec{UserID: fresh.ID, TTL: -time.Minute}, 0},
		"a room, default":        {InviteSpec{Kind: InviteRoom}, DefaultInviteTTL},
		"a room, the cap":        {InviteSpec{Kind: InviteRoom, TTL: time.Hour}, time.Hour},
		"a room, over the cap":   {InviteSpec{Kind: InviteRoom, TTL: 2 * time.Hour}, 0},
	} {
		tc.spec.Channel = "tg"
		_, inv, err := s.CreateInvite(ctx, tc.spec)
		switch {
		case tc.ttl == 0 && !errors.Is(err, ErrInvalidInvite):
			t.Errorf("%s: err = %v, want ErrInvalidInvite", name, err)
		case tc.ttl != 0 && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.ttl != 0 && inv.ExpiresAt.Sub(inv.CreatedAt) != tc.ttl:
			t.Errorf("%s: ttl = %v, want %v", name, inv.ExpiresAt.Sub(inv.CreatedAt), tc.ttl)
		}
	}
	_, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: bound.ID, TTL: 2 * time.Hour})
	if err == nil || !strings.Contains(err.Error(), "at most 1h while") {
		t.Errorf("err = %v, want the cap and why", err)
	}
}

// A holaclaw invite pairs a device: it carries a token scope, client unless
// asked, and admin only for an identity whose role allows it. No other invite
// takes one, and no message redeems it.
func TestInviteScope(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	user := newUser(t, s, "Bo")
	admin, err := s.CreateIdentity(ctx, "Ada", store.RoleAdmin, EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		spec InviteSpec
		want Scope
		err  error
	}{
		"default":              {InviteSpec{Channel: event.ChannelHolaclaw, UserID: user.ID}, ScopeClient, nil},
		"client":               {InviteSpec{Channel: event.ChannelHolaclaw, UserID: user.ID, Scope: ScopeClient}, ScopeClient, nil},
		"admin for an admin":   {InviteSpec{Channel: event.ChannelHolaclaw, UserID: admin.ID, Scope: ScopeAdmin}, ScopeAdmin, nil},
		"admin for a user":     {InviteSpec{Channel: event.ChannelHolaclaw, UserID: user.ID, Scope: ScopeAdmin}, "", ErrInvalidScope},
		"channel scope":        {InviteSpec{Channel: event.ChannelHolaclaw, UserID: admin.ID, Scope: Scope("channel")}, "", ErrInvalidScope},
		"another channel":      {InviteSpec{Channel: "tg", UserID: admin.ID, Scope: ScopeClient}, "", ErrInvalidInvite},
		"a room":               {InviteSpec{Kind: InviteRoom, Channel: "tg", Scope: ScopeClient}, "", ErrInvalidInvite},
		"no scope off pairing": {InviteSpec{Channel: "tg", UserID: admin.ID}, "", nil},
	} {
		_, inv, err := s.CreateInvite(ctx, tc.spec)
		if !errors.Is(err, tc.err) || (err == nil && inv.Scope != tc.want) {
			t.Errorf("%s: scope = %q, err = %v; want %q, %v", name, inv.Scope, err, tc.want, tc.err)
		}
	}
	code, inv, err := s.CreateInvite(ctx, InviteSpec{Channel: event.ChannelHolaclaw, UserID: admin.ID, Scope: ScopeAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemInvite(ctx, code, event.ChannelHolaclaw, []string{"x"}, ""); !errors.Is(err, ErrInviteChannel) {
		t.Fatalf("redeem by message: err = %v, want ErrInviteChannel", err)
	}
	list, err := s.ListInvites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range list {
		if l.ID == inv.ID && (l.Used() || l.Scope != ScopeAdmin) {
			t.Fatalf("invite = %+v; want unused, scope admin", l)
		}
	}
}

func TestInviteExpiryRevokeAndChannel(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	bo := newUser(t, s, "Bo")
	spec := InviteSpec{Channel: "tg", UserID: bo.ID}

	expired, _, err := s.CreateInvite(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	long := spec
	long.TTL = 24 * time.Hour
	day, _, err := s.CreateInvite(ctx, long)
	if err != nil {
		t.Fatal(err)
	}
	scoped, _, err := s.CreateInvite(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	revoked, revInv, err := s.CreateInvite(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInvite(ctx, revInv.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInvite(ctx, "nosuchinvite"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}

	if _, err := s.RedeemInvite(ctx, scoped, "whatsapp", []string{"a"}, ""); !errors.Is(err, ErrInviteChannel) {
		t.Fatalf("wrong-channel err = %v, want ErrInviteChannel", err)
	}
	if _, err := s.RedeemInvite(ctx, revoked, "tg", []string{"b"}, ""); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("revoked err = %v, want ErrInviteInvalid", err)
	}
	if _, err := s.RedeemInvite(ctx, "0000000000", "tg", []string{"c"}, ""); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("unknown err = %v, want ErrInviteInvalid", err)
	}

	*clock = clock.Add(2 * time.Hour)
	if _, err := s.RedeemInvite(ctx, expired, "tg", []string{"d"}, ""); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("expired err = %v, want ErrInviteInvalid", err)
	}
	// The day-long one is still alive two hours in.
	if _, err := s.RedeemInvite(ctx, day, "tg", []string{"e"}, "E"); err != nil {
		t.Fatalf("day-long invite: %v", err)
	}
}

// A code sent on its own channel after a wrong-channel attempt still works.
func TestInviteWrongChannelKeepsInvite(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	bo := newUser(t, s, "Bo")
	code, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: bo.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemInvite(ctx, code, "tg-other", []string{"a"}, ""); !errors.Is(err, ErrInviteChannel) {
		t.Fatalf("err = %v, want ErrInviteChannel", err)
	}
	if _, err := s.Resolve(ctx, "tg-other", []string{"a"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bound on the wrong channel: %v", err)
	}
	if i, err := s.RedeemInvite(ctx, code, "tg", []string{"a"}, ""); err != nil || i.ID != bo.ID {
		t.Fatalf("redeem on its channel = %+v, %v", i, err)
	}
}

func TestRedeemConcurrent(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	bo := newUser(t, s, "Bo")
	code, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: bo.ID})
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	var ok atomic.Int64
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.RedeemInvite(ctx, code, "tg", []string{string(rune('a' + i))}, "")
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrInviteInvalid), errors.Is(err, ErrRateLimited):
			default:
				t.Errorf("unexpected err: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := ok.Load(); got != 1 {
		t.Fatalf("%d redeems succeeded, want exactly 1", got)
	}
	bindings, err := s.Bindings(ctx, bo.ID)
	if err != nil || len(bindings) != 1 {
		t.Fatalf("bindings = %v, err %v; want exactly one", bindings, err)
	}
}

func TestRedeemRateLimited(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	for i := range redeemFailures {
		if _, err := s.RedeemInvite(ctx, "ZZZZZZZZZZ", "tg", []string{"ana"}, ""); !errors.Is(err, ErrInviteInvalid) {
			t.Fatalf("attempt %d err = %v, want ErrInviteInvalid", i, err)
		}
	}
	if _, err := s.RedeemInvite(ctx, "ZZZZZZZZZZ", "tg", []string{"ana"}, ""); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	// Even a valid code is refused while the alias is blocked...
	code, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: newUser(t, s, "Bo").ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemInvite(ctx, code, "tg", []string{"ana"}, ""); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	// ...but another alias, and another channel, are unaffected.
	if _, err := s.RedeemInvite(ctx, code, "tg", []string{"bo"}, "Bo"); err != nil {
		t.Fatalf("other alias: %v", err)
	}
	// The window forgives.
	*clock = clock.Add(redeemWindow + time.Minute)
	if _, err := s.RedeemInvite(ctx, "ZZZZZZZZZZ", "tg", []string{"ana"}, ""); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("after the window err = %v, want ErrInviteInvalid", err)
	}
}

// A code sent from a binding of a confirmed identity is refused, the invited
// one's included, and consumes nothing.
func TestRedeemFromAConfirmedBindingKeepsInvite(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	first := newUser(t, s, "Ana")
	storetest.Bind(t, s.st, "tg", []string{"ana"}, first.ID, "Ana")
	bo := newUser(t, s, "Bo")
	storetest.Bind(t, s.st, "tg", []string{"bo-old"}, bo.ID, "Bo")
	code, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: bo.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, aliases := range [][]string{{"bo", "ana"}, {"bo-old"}, {"bo", "bo-old"}} {
		if _, err := s.RedeemInvite(ctx, code, "tg", aliases, "Bo"); !errors.Is(err, store.ErrAliasLinked) {
			t.Fatalf("%v: err = %v, want ErrAliasLinked", aliases, err)
		}
	}
	// Nothing was committed: the invite is unused and no binding added.
	list, err := s.ListInvites(ctx)
	if err != nil || len(list) != 1 || list[0].Used() {
		t.Fatalf("invite = %+v, err %v; want unused", list, err)
	}
	if _, err := s.Resolve(ctx, "tg", []string{"bo"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	// The same code still works for a free alias.
	i, err := s.RedeemInvite(ctx, code, "tg", []string{"bo"}, "Bo")
	if err != nil || i.ID != bo.ID {
		t.Fatalf("identity = %+v, %v", i, err)
	}
}

// A new guest is named from the random end of its id, lowercase, however the
// platform calls it; the display stays on the binding, empty when it is.
func TestGuestNameIsNotThePlatforms(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	shape := regexp.MustCompile(`^guest-[0-9a-z]{6}$`)
	for alias, display := range map[string]string{"1": "Angel (owner)", "2": ""} {
		i, err := s.EnsureGuest(ctx, "tg", []string{alias}, display)
		if err != nil {
			t.Fatal(err)
		}
		if !shape.MatchString(i.Name) || !strings.HasSuffix(strings.ToUpper(string(i.ID)), strings.ToUpper(i.Name[len("guest-"):])) {
			t.Fatalf("name = %q for id %s", i.Name, i.ID)
		}
		stored, err := s.st.GetIdentity(ctx, i.ID)
		if err != nil || stored.Name != i.Name {
			t.Fatalf("stored = %+v, %v", stored, err)
		}
		ids, err := s.Bindings(ctx, i.ID)
		if err != nil || len(ids) != 1 || ids[0].Display != display {
			t.Fatalf("bindings = %+v, %v; want display %q", ids, err, display)
		}
	}
}

func TestEnsureGuestIdempotent(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	i, err := s.EnsureGuest(ctx, "tg", []string{"77", "@zoe"}, "Zoe")
	if err != nil {
		t.Fatal(err)
	}
	if !i.Pending() || i.Name != placeholderName(i.ID) {
		t.Fatalf("guest = %+v", i)
	}
	again, err := s.EnsureGuest(ctx, "tg", []string{"77"}, "Zoe")
	if err != nil || again.ID != i.ID {
		t.Fatalf("guest = %+v, err %v; want %s", again, err, i.ID)
	}
	identities, err := s.st.ListIdentities(ctx)
	if err != nil || len(identities) != 1 {
		t.Fatalf("identities = %v, err %v", identities, err)
	}
	// Concurrent first contact still yields one guest.
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.EnsureGuest(ctx, "tg", []string{"88"}, "Bo"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	identities, err = s.st.ListIdentities(ctx)
	if err != nil || len(identities) != 2 {
		t.Fatalf("identities = %v, err %v; want 2", identities, err)
	}
	if _, err := s.EnsureGuest(ctx, "", nil, ""); err == nil {
		t.Fatal("want a refusal without channel and aliases")
	}
	if _, err := s.Resolve(ctx, "tg", []string{"nobody"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
