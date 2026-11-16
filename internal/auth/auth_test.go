package auth

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// ownerP is a caller acting as the owner, for the calls that check who acts.
var ownerP = Principal{Role: store.RoleOwner}

// newService opens a real SQLite store in a temp dir with a frozen clock.
func newService(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	clock := &now
	s := New(st)
	var mu sync.Mutex
	s.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return *clock
	}
	return s, clock
}

func TestCreateAuthenticateRevokeToken(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	owner, err := initOwner(ctx, s)
	if err != nil {
		t.Fatal(err)
	}

	plain, tok, err := s.CreateToken(ctx, "cli", ScopeAdmin, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "hb_a_") || len(plain) != len("hb_a_")+2*tokenBytes {
		t.Fatalf("plaintext %q has the wrong shape", plain)
	}
	if tok.ID == "" || tok.Scope != ScopeAdmin || tok.IdentityID != owner.ID || tok.Binding == "" {
		t.Fatalf("token = %+v", tok)
	}

	p, err := s.Authenticate(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	if p.TokenID != tok.ID || p.Scope != ScopeAdmin || p.UserID != owner.ID || p.Role != store.RoleOwner || p.Binding != tok.Binding {
		t.Fatalf("principal = %+v", p)
	}

	// The stored hash is never handed back. The other token is init's.
	list, err := s.ListTokens(ctx, TokenFilter{All: true})
	if err != nil || len(list) != 2 || list[1].ID != tok.ID || list[1].IdentityID != owner.ID {
		t.Fatalf("list = %v, err %v", list, err)
	}
	if list[1].LastUsedAt == nil {
		t.Fatal("last_used_at not set after authenticate")
	}

	if err := s.RevokeToken(ctx, ownerP, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, plain); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("revoked token err = %v, want ErrTokenInvalid", err)
	}
	// Revoking a token leaves its binding.
	if b := holaclawBindings(t, s, owner.ID); !slices.Contains(b, tok.Binding) {
		t.Fatalf("bindings after revoke = %v, want %s kept", b, tok.Binding)
	}
	if err := s.RevokeToken(ctx, ownerP, "nosuchtoken"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoke unknown err = %v, want ErrNotFound", err)
	}
}

// holaclawBindings lists the aliases of an identity's holaclaw bindings.
func holaclawBindings(t *testing.T, s *Service, id event.ID) []string {
	t.Helper()
	all, err := s.Bindings(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, b := range all {
		if b.Channel == event.ChannelHolaclaw {
			out = append(out, b.Alias)
		}
	}
	return out
}

// Each token gets its own binding, named after it, and resolves token →
// binding → identity.
func TestTokenResolvesThroughItsBinding(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if _, err := initOwner(ctx, s); err != nil {
		t.Fatal(err)
	}
	carla, err := s.CreateIdentity(ctx, "carla", store.RoleUser, EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	plainA, a, err := s.CreateToken(ctx, "iPhone 15", ScopeClient, carla.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, b, err := s.CreateToken(ctx, "automations", ScopeClient, carla.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.Binding == b.Binding {
		t.Fatalf("two tokens share binding %s", a.Binding)
	}
	bindings, err := s.Bindings(ctx, carla.ID)
	if err != nil || len(bindings) != 2 || bindings[0].Alias != a.Binding || bindings[0].Display != "iPhone 15" {
		t.Fatalf("bindings = %+v, err %v", bindings, err)
	}
	counts, err := s.ActiveTokens(ctx, carla.ID)
	if err != nil || counts[a.Binding] != 1 || counts[b.Binding] != 1 {
		t.Fatalf("active tokens = %v, err %v", counts, err)
	}
	p, err := s.Authenticate(ctx, plainA)
	if err != nil || p.UserID != carla.ID || p.Binding != a.Binding || p.Role != store.RoleUser {
		t.Fatalf("principal = %+v, err %v", p, err)
	}

	// The binding is what carries the identity: moved, the token follows it.
	other, err := s.CreateIdentity(ctx, "dana", store.RoleAdmin, EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE bindings SET identity_id = ? WHERE channel = 'holaclaw' AND alias = ?`, string(other.ID), a.Binding); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Authenticate(ctx, plainA); err != nil || p.UserID != other.ID || p.Role != store.RoleAdmin {
		t.Fatalf("principal after the move = %+v, err %v", p, err)
	}
}

// No token authenticates without a binding: none is minted without an
// identity, and one whose binding is gone is invalid.
func TestNoTokenWithoutABinding(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if _, _, err := s.CreateToken(ctx, "x", ScopeClient, ""); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("client token with no identity: err = %v, want ErrInvalidIdentity", err)
	}
	if _, _, err := s.CreateToken(ctx, "x", ScopeAdmin, ""); !errors.Is(err, ErrInvalidIdentity) {
		t.Fatalf("admin token with no identity: err = %v, want ErrInvalidIdentity", err)
	}
	if _, _, err := s.CreateToken(ctx, "x", ScopeClient, "nosuchuser"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("token for a missing identity: err = %v, want ErrNotFound", err)
	}
	// Only its own messages bind a pending identity.
	guest, err := s.EnsureGuest(ctx, "tg", []string{"42"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MintToken(ctx, Principal{Role: store.RoleAdmin}, "x", ScopeClient, guest.ID); !errors.Is(err, ErrPending) {
		t.Fatalf("token for a pending identity: err = %v, want ErrPending", err)
	}
	ana, err := s.CreateIdentity(ctx, "ana", store.RoleUser, EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	plain, tok, err := s.CreateToken(ctx, "ci", ScopeClient, ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A binding removed behind the service's back, without revoking.
	if _, err := s.db.Exec(`DELETE FROM bindings WHERE channel = 'holaclaw' AND alias = ?`, tok.Binding); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, plain); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("token with no binding: err = %v, want ErrTokenInvalid", err)
	}
	// The schema refuses a holaclaw token row with no binding at all.
	if _, err := s.db.Exec(`INSERT INTO api_tokens (id, name, scope, hash, binding_id, created_at) VALUES ('t1', 'x', 'client', 'h1', NULL, 0)`); err == nil {
		t.Fatal("a client token with no binding_id was stored")
	}
}

func TestAuthenticateRejects(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	ana, err := s.CreateIdentity(ctx, "ana", store.RoleUser, EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := s.CreateToken(ctx, "client", ScopeClient, ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"garbage", "\x00\x01not a token"},
		{"wrong prefix", "xx_" + plain[3:]},
		{"unknown", "hb_a_" + strings.Repeat("0", 2*tokenBytes)},
		{"truncated", plain[:len(plain)-1]},
		{"sql-ish", "hb_a_' OR 1=1 --"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Authenticate(ctx, tc.input); !errors.Is(err, ErrTokenInvalid) {
				t.Fatalf("err = %v, want ErrTokenInvalid", err)
			}
		})
	}
	if _, _, err := s.CreateToken(ctx, "x", Scope("root"), ana.ID); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("err = %v, want ErrInvalidScope", err)
	}
}

// A demotion lowers an admin token to client at once, and a promotion raises
// it back only through SetTokenScope.
func TestScopeCappedByRole(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if _, err := initOwner(ctx, s); err != nil {
		t.Fatal(err)
	}
	rec := &recordSink{}
	s.Events = rec
	dan, err := s.CreateIdentity(ctx, "dan", store.RoleAdmin, EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	plain, tok, err := s.CreateToken(ctx, "ops", ScopeAdmin, dan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := s.Authenticate(ctx, plain); err != nil || p.Scope != ScopeAdmin {
		t.Fatalf("admin: principal = %+v, err %v", p, err)
	}
	if _, err := s.Update(ctx, dan.ID, nil, ptr(store.RoleUser), "boss"); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Authenticate(ctx, plain); err != nil || p.Scope != ScopeClient || p.Role != store.RoleUser {
		t.Fatalf("demoted: principal = %+v, err %v", p, err)
	}
	if stored, err := getTokenIn(ctx, s.db, tok.ID); err != nil || stored.Scope != ScopeClient {
		t.Fatalf("demoted: stored = %+v, %v; want lowered to client", stored, err)
	}
	if e := rec.last(); e.Type != event.TokenUpdated ||
		!strings.Contains(string(e.Attrs), `"reason":"demoted"`) || !strings.Contains(string(e.Attrs), `"by":"boss"`) {
		t.Fatalf("last event = %s %s, want token.updated for the demotion", e.Type, e.Attrs)
	}
	if _, err := s.Update(ctx, dan.ID, nil, ptr(store.RoleAdmin), EmptyEventID); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Authenticate(ctx, plain); err != nil || p.Scope != ScopeClient {
		t.Fatalf("promoted again: principal = %+v, err %v; want client until set", p, err)
	}
	if _, err := s.SetTokenScope(ctx, ownerP, tok.ID, ScopeAdmin); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Authenticate(ctx, plain); err != nil || p.Scope != ScopeAdmin {
		t.Fatalf("raised: principal = %+v, err %v", p, err)
	}
	// A client token stays client whatever the role.
	plain, _, err = s.CreateToken(ctx, "phone", ScopeClient, dan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := s.Authenticate(ctx, plain); err != nil || p.Scope != ScopeClient {
		t.Fatalf("client token of an admin: principal = %+v, err %v", p, err)
	}
}

func ptr[T any](v T) *T { return &v }

func TestScopeMatrix(t *testing.T) {
	tests := []struct {
		have, required Scope
		want           bool
	}{
		{ScopeAdmin, ScopeAdmin, true},
		{ScopeAdmin, ScopeClient, true},
		{ScopeClient, ScopeAdmin, false},
		{ScopeClient, ScopeClient, true},
		{Scope("channel"), ScopeClient, false},
		{ScopeAdmin, Scope("channel"), false},
		{Scope(""), ScopeClient, false},
		{ScopeAdmin, Scope("root"), false},
	}
	for _, tc := range tests {
		if got := tc.have.Allows(tc.required); got != tc.want {
			t.Errorf("%q.Allows(%q) = %v, want %v", tc.have, tc.required, got, tc.want)
		}
	}
}

// The unix socket arrives as holaclaw:cli, a binding of the owner with no
// tokens, and resolves through it.
func TestSocketResolvesToHolaclawCLI(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if _, err := s.SocketPrincipal(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("socket before init: err = %v, want ErrNotFound", err)
	}
	owner, err := initOwner(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.SocketPrincipal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.Binding != SocketAlias || p.UserID != owner.ID || p.Role != store.RoleOwner || p.Scope != ScopeAdmin || p.TokenID != "" {
		t.Fatalf("socket principal = %+v", p)
	}
	if counts, err := s.ActiveTokens(ctx, owner.ID); err != nil || counts[SocketAlias] != 0 {
		t.Fatalf("holaclaw:cli tokens = %v, err %v", counts, err)
	}
}

func TestLastUsedThrottled(t *testing.T) {
	ctx := context.Background()
	s, clock := newService(t)
	ana, err := s.CreateIdentity(ctx, "ana", store.RoleUser, EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	plain, tok, err := s.CreateToken(ctx, "cli", ScopeClient, ana.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, plain); err != nil {
		t.Fatal(err)
	}
	first := mustToken(t, s, tok.ID).LastUsedAt
	if first == nil {
		t.Fatal("last_used_at not set")
	}
	// Inside the throttle window: no write.
	*clock = clock.Add(30 * time.Second)
	if _, err := s.Authenticate(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if got := mustToken(t, s, tok.ID).LastUsedAt; !got.Equal(*first) {
		t.Fatalf("last_used_at = %v, want unchanged %v", got, first)
	}
	// Past it: refreshed.
	*clock = clock.Add(2 * time.Minute)
	if _, err := s.Authenticate(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if got := mustToken(t, s, tok.ID).LastUsedAt; !got.After(*first) {
		t.Fatalf("last_used_at = %v, want after %v", got, first)
	}
}

func mustToken(t *testing.T, s *Service, id event.ID) Token {
	t.Helper()
	tok, err := getTokenIn(context.Background(), s.db, id)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestOwnerNeverDeleted(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	owner, err := initOwner(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteIdentity(ctx, owner.ID, false, EmptyEventID); !errors.Is(err, ErrOwner) {
		t.Fatalf("delete err = %v, want ErrOwner", err)
	}
	// A second owner changes nothing: deleting an owner is never allowed.
	second, err := s.CreateIdentity(ctx, "ana", store.RoleAdmin, EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	setRole(t, s, second.ID, store.RoleOwner)
	if _, err := s.DeleteIdentity(ctx, owner.ID, false, EmptyEventID); !errors.Is(err, ErrOwner) {
		t.Fatalf("delete err = %v, want ErrOwner", err)
	}
	if _, err := s.DeleteIdentity(ctx, "nosuchuser", false, EmptyEventID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateIdentity(ctx, "eve", store.RoleOwner, EmptyEventID); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("err = %v, want ErrInvalidRole", err)
	}
}

// setRole changes a confirmed identity's role behind the service's back.
func setRole(t *testing.T, s *Service, id event.ID, role store.Role) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE identities SET role = ? WHERE id = ?`, string(role), string(id)); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteUserRunsTheHookFirst(t *testing.T) {
	s, _ := newService(t)
	ctx := context.Background()
	i, err := s.CreateIdentity(ctx, "ana", store.RoleUser, EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	var seen event.ID
	s.BeforeDeleteUser = func(_ context.Context, id event.ID) error { seen = id; return errors.New("nope") }
	if _, err := s.DeleteIdentity(ctx, i.ID, false, EmptyEventID); err == nil {
		t.Fatal("a failing hook must abort the deletion")
	}
	if seen != i.ID {
		t.Fatalf("hook saw %q", seen)
	}
	s.BeforeDeleteUser = func(context.Context, event.ID) error { return nil }
	s.ForgetUserMemory = func(context.Context, event.ID) (int64, error) { return 0, errors.New("disk") }
	if _, err := s.DeleteIdentity(ctx, i.ID, false, EmptyEventID); err == nil {
		t.Fatal("a failing forget must abort the deletion")
	}
	if _, err := s.st.GetIdentity(ctx, i.ID); err != nil {
		t.Fatalf("the user must survive an aborted deletion: %v", err)
	}
	s.ForgetUserMemory = nil
	if _, err := s.DeleteIdentity(ctx, i.ID, false, EmptyEventID); err != nil {
		t.Fatal(err)
	}
}
