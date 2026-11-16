package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// pairInvite mints a holaclaw invite onto id, as the owner, and returns its
// code.
func pairInvite(t *testing.T, s *Service, id event.ID, scope Scope) (string, Invite) {
	t.Helper()
	code, inv, err := s.CreateInvite(context.Background(), InviteSpec{
		Channel: event.ChannelHolaclaw, UserID: id, Scope: scope, CreatorRole: store.RoleOwner,
	})
	if err != nil {
		t.Fatal(err)
	}
	return code, inv
}

// A pairing creates a binding named after the device, with the platform, and
// its first token, which signs in as that binding. The events name the invite.
func TestPairCreatesABindingAndItsToken(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if _, err := initOwner(ctx, s); err != nil {
		t.Fatal(err)
	}
	ana := newUser(t, s, "ana")
	code, inv := pairInvite(t, s, ana.ID, "")
	rec := &recordSink{}
	s.Events = rec

	secret, tok, err := s.Pair(ctx, code, " iPhone\n15 ", "ios", "192.0.2.1")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if tok.Name != "iPhone 15" || tok.Scope != ScopeClient || tok.Origin != OriginPair || tok.OriginRef != inv.ID ||
		tok.IdentityID != ana.ID {
		t.Fatalf("token = %+v", tok)
	}
	p, err := s.Authenticate(ctx, secret)
	if err != nil || p.UserID != ana.ID || p.Binding != tok.Binding || p.Scope != ScopeClient || p.TokenID != tok.ID {
		t.Fatalf("principal = %+v, err %v", p, err)
	}
	devices, err := s.Devices(ctx, p)
	if err != nil || len(devices) != 1 {
		t.Fatalf("devices = %+v, err %v", devices, err)
	}
	if d := devices[0]; d.Alias != tok.Binding || d.Display != "iPhone 15" || d.Platform != "ios" || d.Tokens != 1 {
		t.Fatalf("device = %+v", d)
	}
	var bound, created bool
	for _, e := range rec.evts {
		var a map[string]string
		_ = json.Unmarshal(e.Attrs, &a) // every attribute here is a string
		switch e.Type {
		case event.IdentityBound:
			bound = a["invite"] == string(inv.ID) && a["token"] == string(tok.ID) && a["platform"] == "ios"
		case event.TokenCreated:
			created = a["origin"] == string(OriginPair) && a["invite"] == string(inv.ID)
		}
	}
	if !bound || !created {
		t.Fatalf("events = %+v, want identity.bound and token.created naming the invite", rec.evts)
	}
	list, err := s.ListInvites(ctx)
	if err != nil || len(list) != 1 || !list[0].Used() {
		t.Fatalf("invites = %+v, err %v; want the one used", list, err)
	}
}

// A code pairs once, and not after it expires; a failed attempt spends
// nothing.
func TestPairCodeIsSingleUseAndExpires(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	if _, err := initOwner(ctx, s); err != nil {
		t.Fatal(err)
	}
	ana := newUser(t, s, "ana")

	code, _ := pairInvite(t, s, ana.ID, "")
	if _, _, err := s.Pair(ctx, code, "Mac", "macos", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Pair(ctx, code, "iPad", "ipados", "192.0.2.2"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("second pair = %v, want ErrInviteInvalid", err)
	}

	late, _ := pairInvite(t, s, ana.ID, "")
	*clock = clock.Add(DefaultInviteTTL)
	if _, _, err := s.Pair(ctx, late, "Mac", "macos", "192.0.2.3"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("expired pair = %v, want ErrInviteInvalid", err)
	}

	// A DM code for another channel pairs nothing and stays usable there.
	tg, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: ana.ID, CreatorRole: store.RoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Pair(ctx, tg, "Mac", "macos", "192.0.2.4"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("telegram code = %v, want ErrInviteInvalid", err)
	}
	if _, err := s.RedeemInvite(ctx, tg, "tg", []string{"77"}, "Ana"); err != nil {
		t.Fatalf("the telegram code was spent by a pairing: %v", err)
	}
	// And a holaclaw code never redeems from a message.
	code, _ = pairInvite(t, s, ana.ID, "")
	if _, err := s.RedeemInvite(ctx, code, event.ChannelHolaclaw, []string{"x"}, ""); !errors.Is(err, ErrInviteChannel) {
		t.Fatalf("holaclaw redeem = %v, want ErrInviteChannel", err)
	}
}

// The token gets the invite's scope, never above what the role allows now.
func TestPairRespectsTheInviteScope(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if _, err := initOwner(ctx, s); err != nil {
		t.Fatal(err)
	}
	dan, err := s.CreateIdentity(ctx, "dan", store.RoleAdmin, EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := pairInvite(t, s, dan.ID, ScopeAdmin)
	if _, tok, err := s.Pair(ctx, code, "Mac", "macos", "a"); err != nil || tok.Scope != ScopeAdmin {
		t.Fatalf("admin invite: token = %+v, err %v", tok, err)
	}
	code, _ = pairInvite(t, s, dan.ID, "")
	if _, tok, err := s.Pair(ctx, code, "iPhone", "ios", "a"); err != nil || tok.Scope != ScopeClient {
		t.Fatalf("client invite: token = %+v, err %v", tok, err)
	}
	// Demoted after the invite was minted: the token is client.
	code, _ = pairInvite(t, s, dan.ID, ScopeAdmin)
	if _, err := s.Update(ctx, dan.ID, nil, ptr(store.RoleUser), "boss"); err != nil {
		t.Fatal(err)
	}
	if _, tok, err := s.Pair(ctx, code, "iPad", "ipados", "a"); err != nil || tok.Scope != ScopeClient {
		t.Fatalf("demoted: token = %+v, err %v", tok, err)
	}
}

// An identity disabled since the invite was minted pairs nothing, and the code
// works again once it is enabled.
func TestPairWaitsForADisabledIdentity(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	owner, err := initOwner(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	ana := newUser(t, s, "ana")
	code, _ := pairInvite(t, s, ana.ID, "")
	if _, err := s.Disable(ctx, ana.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Pair(ctx, code, "Mac", "macos", "a"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("disabled: %v, want ErrInviteInvalid", err)
	}
	if _, err := s.Enable(ctx, ana.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Pair(ctx, code, "Mac", "macos", "a"); err != nil {
		t.Fatalf("enabled: %v", err)
	}
}

// Failed codes are limited per source address: the limit holds even for a
// good code, spends nothing, leaves other addresses alone and lifts after the
// window. A malformed request is no attempt.
func TestPairRateLimitPerSource(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	if _, err := initOwner(ctx, s); err != nil {
		t.Fatal(err)
	}
	ana := newUser(t, s, "ana")
	code, _ := pairInvite(t, s, ana.ID, "")

	for range redeemFailures * 2 {
		if _, _, err := s.Pair(ctx, code, "", "ios", "198.51.100.7"); !errors.Is(err, ErrInvalidDevice) {
			t.Fatalf("no device = %v, want ErrInvalidDevice", err)
		}
	}
	for i := range redeemFailures {
		if _, _, err := s.Pair(ctx, "WRONGCODE0", "Mac", "macos", "198.51.100.7"); !errors.Is(err, ErrInviteInvalid) {
			t.Fatalf("attempt %d = %v, want ErrInviteInvalid", i, err)
		}
	}
	if _, _, err := s.Pair(ctx, code, "Mac", "macos", "198.51.100.7"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("after %d failures = %v, want ErrRateLimited", redeemFailures, err)
	}
	*clock = clock.Add(redeemWindow)
	if _, _, err := s.Pair(ctx, code, "Mac", "macos", "198.51.100.7"); err != nil {
		t.Fatalf("after the window: %v", err)
	}

	code, _ = pairInvite(t, s, ana.ID, "")
	for range redeemFailures {
		_, _, _ = s.Pair(ctx, "WRONGCODE0", "Mac", "macos", "198.51.100.8")
	}
	if _, _, err := s.Pair(ctx, code, "iPhone", "ios", "198.51.100.9"); err != nil {
		t.Fatalf("another address: %v", err)
	}
}

func TestPairRefusesAMalformedDevice(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if _, err := initOwner(ctx, s); err != nil {
		t.Fatal(err)
	}
	code, _ := pairInvite(t, s, newUser(t, s, "ana").ID, "")
	for _, c := range []struct{ device, platform string }{
		{"", "ios"}, {"\u200b\n", "ios"}, {"Mac", ""}, {"Mac", "macOS"}, {"Mac", "mac os"},
		{"Mac", "a-very-long-platform-name-that-goes-on"},
	} {
		if _, _, err := s.Pair(ctx, code, c.device, c.platform, "a"); !errors.Is(err, ErrInvalidDevice) {
			t.Errorf("Pair(%q, %q) = %v, want ErrInvalidDevice", c.device, c.platform, err)
		}
	}
	if _, _, err := s.Pair(ctx, code, "Mac", "macos", "a"); err != nil {
		t.Fatalf("a refused device spent the code: %v", err)
	}
}

// A signed-in client mints pairing links for its own identity, never above
// its own scope.
func TestInviteDevice(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if _, err := initOwner(ctx, s); err != nil {
		t.Fatal(err)
	}
	dan, err := s.CreateIdentity(ctx, "dan", store.RoleAdmin, EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	client := Principal{UserID: dan.ID, Role: store.RoleAdmin, Scope: ScopeClient}
	if _, _, err := s.InviteDevice(ctx, client, ScopeAdmin); !errors.Is(err, ErrScopeAboveCaller) {
		t.Fatalf("client asking for admin = %v, want ErrScopeAboveCaller", err)
	}
	if _, _, err := s.InviteDevice(ctx, client, "root"); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("bad scope = %v, want ErrInvalidScope", err)
	}
	code, inv, err := s.InviteDevice(ctx, client, "")
	if err != nil || inv.UserID != dan.ID || inv.Scope != ScopeClient || inv.Channel != event.ChannelHolaclaw ||
		inv.CreatedBy != dan.ID || inv.ExpiresAt.Sub(inv.CreatedAt) != DefaultInviteTTL {
		t.Fatalf("invite = %+v, err %v", inv, err)
	}
	if _, tok, err := s.Pair(ctx, code, "iPad", "ipados", "a"); err != nil || tok.IdentityID != dan.ID {
		t.Fatalf("pair = %+v, %v", tok, err)
	}
	admin := Principal{UserID: dan.ID, Role: store.RoleAdmin, Scope: ScopeAdmin}
	if _, inv, err := s.InviteDevice(ctx, admin, ScopeAdmin); err != nil || inv.Scope != ScopeAdmin {
		t.Fatalf("admin invite = %+v, err %v", inv, err)
	}
	// A pending identity's client pairs nothing: an admin confirms it first.
	guest, err := s.EnsureGuest(ctx, "tg", []string{"9"}, "Guest")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.InviteDevice(ctx, Principal{UserID: guest.ID, Role: store.RoleGuest, Scope: ScopeClient}, ""); !errors.Is(err, ErrPending) {
		t.Fatalf("pending = %v, want ErrPending", err)
	}
}

// Devices and UnbindDevice act only on the caller's own holaclaw bindings.
func TestDevicesAreTheCallersOwn(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	owner, err := initOwner(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	ana := newUser(t, s, "ana")
	anaSecret, anaTok, err := s.CreateToken(ctx, "laptop", ScopeClient, ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := pairInvite(t, s, ana.ID, "")
	phoneSecret, phone, err := s.Pair(ctx, code, "Phone", "ios", "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemInvite(ctx, mustInvite(t, s, ana.ID), "tg", []string{"5"}, "Ana"); err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(ctx, anaSecret)
	if err != nil {
		t.Fatal(err)
	}
	devices, err := s.Devices(ctx, p)
	if err != nil || len(devices) != 2 || devices[0].Alias != anaTok.Binding || devices[1].Alias != phone.Binding {
		t.Fatalf("devices = %+v, err %v; want the laptop and the phone, not telegram", devices, err)
	}

	ownerBindings, err := s.Bindings(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range ownerBindings {
		if _, err := s.UnbindDevice(ctx, p, b.Alias); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("ana unbinding the owner's %s = %v, want ErrNotFound", b.Alias, err)
		}
	}
	if _, err := s.UnbindDevice(ctx, p, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown = %v, want ErrNotFound", err)
	}
	if after, _ := s.Bindings(ctx, owner.ID); len(after) != len(ownerBindings) {
		t.Fatalf("the owner's bindings changed: %+v", after)
	}

	u, err := s.UnbindDevice(ctx, p, phone.Binding)
	if err != nil || u.Tokens != 1 || u.IdentityID != ana.ID {
		t.Fatalf("unbind phone = %+v, %v", u, err)
	}
	if _, err := s.Authenticate(ctx, phoneSecret); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("phone token after unbind = %v, want ErrTokenInvalid", err)
	}
	// The binding a request arrives from can go too: signing out.
	if _, err := s.UnbindDevice(ctx, p, p.Binding); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, anaSecret); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("laptop token after unbinding itself = %v, want ErrTokenInvalid", err)
	}

	socket, err := s.SocketPrincipal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UnbindDevice(ctx, socket, SocketAlias); !errors.Is(err, ErrSocketBinding) {
		t.Fatalf("unbind holaclaw:cli = %v, want ErrSocketBinding", err)
	}
	if _, err := s.UnbindDevice(ctx, Principal{}, phone.Binding); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no caller = %v, want ErrNotFound", err)
	}
}

// A burst of concurrent wrong codes from one address consults the database at
// most redeemFailures times: an attempt counts from the moment it starts.
func TestPairRateLimitHoldsUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if _, err := initOwner(ctx, s); err != nil {
		t.Fatal(err)
	}
	const n = 50
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		limited int
	)
	for range n {
		wg.Go(func() {
			_, _, err := s.Pair(ctx, "WRONGCODE0", "Mac", "macos", "198.51.100.7")
			switch {
			case errors.Is(err, ErrRateLimited):
				mu.Lock()
				limited++
				mu.Unlock()
			case !errors.Is(err, ErrInviteInvalid):
				t.Errorf("Pair = %v", err)
			}
		})
	}
	wg.Wait()
	if limited != n-redeemFailures {
		t.Fatalf("%d of %d attempts limited, want %d", limited, n, n-redeemFailures)
	}
}

// A good pairing takes its attempt back, so one address pairs many devices.
func TestPairSuccessDoesNotCount(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if _, err := initOwner(ctx, s); err != nil {
		t.Fatal(err)
	}
	ana := newUser(t, s, "ana")
	for i := range redeemFailures * 2 {
		code, _ := pairInvite(t, s, ana.ID, "")
		if _, _, err := s.Pair(ctx, code, "Mac", "macos", "a"); err != nil {
			t.Fatalf("pairing %d: %v", i, err)
		}
	}
}

// Keys an unauthenticated caller chooses do not pile up: past rlSweepAt keys,
// a new attempt drops every key whose window has passed.
func TestRateLimitMapIsSwept(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	if _, err := initOwner(ctx, s); err != nil {
		t.Fatal(err)
	}
	for i := range rlSweepAt + 10 {
		_, _, _ = s.Pair(ctx, "WRONGCODE0", "Mac", "macos", fmt.Sprintf("2001:db8:%x::/64", i))
	}
	*clock = clock.Add(redeemWindow)
	_, _, _ = s.Pair(ctx, "WRONGCODE0", "Mac", "macos", "198.51.100.7")
	s.rlMu.Lock()
	n := len(s.rl)
	s.rlMu.Unlock()
	if n != 1 {
		t.Fatalf("%d keys after the window, want 1", n)
	}
}

// mustInvite mints a telegram DM invite onto id.
func mustInvite(t *testing.T, s *Service, id event.ID) string {
	t.Helper()
	code, _, err := s.CreateInvite(context.Background(), InviteSpec{Channel: "tg", UserID: id, CreatorRole: store.RoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// `holaclaw init` without --token-file mints nothing to sign in with: the
// owner holds holaclaw:cli alone, and pairs its first device with an invite
// minted over the socket.
func TestCreateOwnerWithoutToken(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	b, err := s.CreateOwner(ctx, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Secret != "" || b.Token.ID != "" || b.Owner.Role != store.RoleOwner {
		t.Fatalf("bootstrapped = %+v, want the owner and no token", b)
	}
	if toks, err := s.ListTokens(ctx, TokenFilter{All: true}); err != nil || len(toks) != 0 {
		t.Fatalf("tokens = %+v, err %v; want none", toks, err)
	}
	if invs, err := s.ListInvites(ctx); err != nil || len(invs) != 0 {
		t.Fatalf("invites = %+v, err %v; want none", invs, err)
	}
	bindings, err := s.Bindings(ctx, b.Owner.ID)
	if err != nil || len(bindings) != 1 || bindings[0].Alias != SocketAlias {
		t.Fatalf("bindings = %+v, err %v; want holaclaw:cli alone", bindings, err)
	}
	socket, err := s.SocketPrincipal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := s.InviteDevice(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	secret, tok, err := s.Pair(ctx, code, "Mac", "macos", "a")
	if err != nil || tok.Scope != ScopeClient || tok.IdentityID != b.Owner.ID {
		t.Fatalf("pair = %+v, %v", tok, err)
	}
	if p, err := s.Authenticate(ctx, secret); err != nil || p.Role != store.RoleOwner || p.Scope != ScopeClient {
		t.Fatalf("principal = %+v, %v", p, err)
	}
}
