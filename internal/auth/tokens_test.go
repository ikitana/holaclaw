package auth

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// tokenEnv is an initialised service with an admin and a user identity.
type tokenEnv struct {
	s           *Service
	rec         *recordSink
	owner       store.Identity
	ada, nadia  store.Identity
	adaP, ownP  Principal
	ownerAlias  string
	ownerSecret string
}

func newTokenEnv(t *testing.T) tokenEnv {
	t.Helper()
	ctx := context.Background()
	s, _ := newService(t)
	boot, err := s.CreateOwner(ctx, true, nil)
	secret, tok := boot.Secret, boot.Token
	if err != nil {
		t.Fatal(err)
	}
	owner, err := s.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ada, err := s.CreateIdentity(ctx, "ada", store.RoleAdmin, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	nadia, err := s.CreateIdentity(ctx, "nadia", store.RoleUser, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordSink{}
	s.Events = rec
	return tokenEnv{
		s: s, rec: rec, owner: owner, ada: ada, nadia: nadia,
		adaP:       Principal{UserID: ada.ID, Role: store.RoleAdmin, Scope: ScopeAdmin},
		ownP:       Principal{UserID: owner.ID, Role: store.RoleOwner, Scope: ScopeAdmin},
		ownerAlias: tok.Binding, ownerSecret: secret,
	}
}

// eventsOf returns the attrs of every recorded event of type typ.
func (e tokenEnv) eventsOf(t *testing.T, typ event.Type) []map[string]any {
	t.Helper()
	e.rec.mu.Lock()
	defer e.rec.mu.Unlock()
	var out []map[string]any
	for _, ev := range e.rec.evts {
		if ev.Type != typ {
			continue
		}
		var attrs map[string]any
		if err := json.Unmarshal(ev.Attrs, &attrs); err != nil {
			t.Fatal(err)
		}
		out = append(out, attrs)
	}
	return out
}

func TestMintTokenScope(t *testing.T) {
	ctx := context.Background()
	e := newTokenEnv(t)

	// client by default, recorded as a token create by its caller.
	_, tok, err := e.s.MintToken(ctx, e.adaP, "phone", "", e.nadia.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Scope != ScopeClient || tok.Origin != OriginTokenCreate || tok.CreatedBy != e.ada.ID {
		t.Fatalf("token = %+v", tok)
	}
	stored, err := getTokenIn(ctx, e.s.db, tok.ID)
	if err != nil || stored.Origin != OriginTokenCreate || stored.CreatedBy != e.ada.ID || stored.OriginRef != "" {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	if got := e.eventsOf(t, event.TokenCreated); len(got) != 1 || got[0]["token"] != string(tok.ID) ||
		got[0]["scope"] != "client" || got[0]["origin"] != "token_create" || got[0]["by"] != string(e.ada.ID) {
		t.Fatalf("token.created = %v", got)
	}
	if got := e.eventsOf(t, event.IdentityBound); len(got) != 1 || got[0]["token"] != string(tok.ID) {
		t.Fatalf("identity.bound = %v", got)
	}

	// admin is refused for a user, and nothing is created.
	if _, _, err := e.s.MintToken(ctx, e.adaP, "ops", ScopeAdmin, e.nadia.ID); !errors.Is(err, ErrScopeAboveRole) {
		t.Fatalf("admin for a user: err = %v, want ErrScopeAboveRole", err)
	}
	if b := holaclawBindings(t, e.s, e.nadia.ID); len(b) != 1 {
		t.Fatalf("bindings after a refusal = %v, want only the phone's", b)
	}
	// and allowed for an admin.
	if _, tok, err := e.s.MintToken(ctx, e.adaP, "ops", ScopeAdmin, e.ada.ID); err != nil || tok.Scope != ScopeAdmin {
		t.Fatalf("admin for an admin: %+v, %v", tok, err)
	}
	if _, _, err := e.s.MintToken(ctx, e.adaP, "x", "root", e.ada.ID); !errors.Is(err, ErrInvalidScope) {
		t.Fatalf("bad scope: err = %v", err)
	}
}

func TestInitTokenOrigin(t *testing.T) {
	ctx := context.Background()
	e := newTokenEnv(t)
	p, err := e.s.Authenticate(ctx, e.ownerSecret)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := getTokenIn(ctx, e.s.db, p.TokenID)
	if err != nil || tok.Origin != OriginInit || tok.Scope != ScopeAdmin {
		t.Fatalf("init token = %+v, %v", tok, err)
	}
}

func TestAddToken(t *testing.T) {
	ctx := context.Background()
	e := newTokenEnv(t)
	_, ci, err := e.s.MintToken(ctx, e.adaP, "automations", "", e.nadia.ID)
	if err != nil {
		t.Fatal(err)
	}
	secret, backup, err := e.s.AddToken(ctx, e.adaP, ci.Binding, "backup", "")
	if err != nil {
		t.Fatal(err)
	}
	if backup.Binding != ci.Binding || backup.IdentityID != e.nadia.ID || backup.Scope != ScopeClient {
		t.Fatalf("added = %+v", backup)
	}
	if p, err := e.s.Authenticate(ctx, secret); err != nil || p.Binding != ci.Binding || p.TokenID != backup.ID {
		t.Fatalf("added token signs in as %+v, %v", p, err)
	}
	// No new binding came with it.
	if b := holaclawBindings(t, e.s, e.nadia.ID); len(b) != 1 {
		t.Fatalf("bindings = %v", b)
	}

	for _, tc := range []struct {
		name  string
		by    Principal
		alias string
		scope Scope
		want  error
	}{
		{"admin for a user", e.adaP, ci.Binding, ScopeAdmin, ErrScopeAboveRole},
		{"unknown binding", e.adaP, "nope", "", store.ErrNotFound},
		{"the socket", e.ownP, SocketAlias, "", ErrSocketBinding},
		{"the owner's, by an admin", e.adaP, e.ownerAlias, "", ErrOwnerOnly},
	} {
		if _, _, err := e.s.AddToken(ctx, tc.by, tc.alias, "x", tc.scope); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if _, _, err := e.s.AddToken(ctx, e.ownP, e.ownerAlias, "x", ScopeAdmin); err != nil {
		t.Errorf("the owner's, by the owner: %v", err)
	}
	if _, err := e.s.Disable(ctx, e.nadia.ID, e.ada.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.AddToken(ctx, e.adaP, ci.Binding, "x", ""); !errors.Is(err, ErrDisabled) {
		t.Errorf("disabled: err = %v", err)
	}
}

// A demotion caps a token on the next request; a promotion raises nothing
// until the token is set.
func TestScopeFollowsRole(t *testing.T) {
	ctx := context.Background()
	e := newTokenEnv(t)
	secret, tok, err := e.s.MintToken(ctx, e.ownP, "phone", ScopeClient, e.nadia.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetTokenScope(ctx, e.adaP, tok.ID, ScopeAdmin); !errors.Is(err, ErrScopeAboveRole) {
		t.Fatalf("set admin for a user: err = %v, want ErrScopeAboveRole", err)
	}
	if _, err := e.s.Update(ctx, e.nadia.ID, nil, ptr(store.RoleAdmin), EmptyEventID); err != nil {
		t.Fatal(err)
	}
	if p, err := e.s.Authenticate(ctx, secret); err != nil || p.Scope != ScopeClient {
		t.Fatalf("promoted: principal = %+v, %v; want client until set", p, err)
	}
	set, err := e.s.SetTokenScope(ctx, e.adaP, tok.ID, ScopeAdmin)
	if err != nil || set.Scope != ScopeAdmin {
		t.Fatalf("set admin: %+v, %v", set, err)
	}
	if p, err := e.s.Authenticate(ctx, secret); err != nil || p.Scope != ScopeAdmin {
		t.Fatalf("raised: principal = %+v, %v", p, err)
	}
	if got := e.eventsOf(t, event.TokenUpdated); len(got) != 1 || got[0]["old_scope"] != "client" ||
		got[0]["scope"] != "admin" || got[0]["by"] != string(e.ada.ID) {
		t.Fatalf("token.updated = %v", got)
	}
	// Setting the scope it has records nothing.
	if _, err := e.s.SetTokenScope(ctx, e.adaP, tok.ID, ScopeAdmin); err != nil {
		t.Fatal(err)
	}
	if got := e.eventsOf(t, event.TokenUpdated); len(got) != 1 {
		t.Fatalf("a no-op set emitted %v", got)
	}

	if _, err := e.s.Update(ctx, e.nadia.ID, nil, ptr(store.RoleUser), EmptyEventID); err != nil {
		t.Fatal(err)
	}
	if p, err := e.s.Authenticate(ctx, secret); err != nil || p.Scope != ScopeClient {
		t.Fatalf("demoted: principal = %+v, %v; want capped at client", p, err)
	}
	// Lowering is always allowed.
	if set, err := e.s.SetTokenScope(ctx, e.adaP, tok.ID, ScopeClient); err != nil || set.Scope != ScopeClient {
		t.Fatalf("lower: %+v, %v", set, err)
	}
}

func TestSetTokenScopeRefusals(t *testing.T) {
	ctx := context.Background()
	e := newTokenEnv(t)
	ownerTok, err := e.s.Authenticate(ctx, e.ownerSecret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetTokenScope(ctx, e.adaP, ownerTok.TokenID, ScopeClient); !errors.Is(err, ErrOwnerOnly) {
		t.Errorf("owner's token by an admin: err = %v", err)
	}
	if _, err := e.s.SetTokenScope(ctx, e.adaP, "nope", ScopeClient); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown: err = %v", err)
	}
	if _, err := e.s.SetTokenScope(ctx, e.adaP, ownerTok.TokenID, "root"); !errors.Is(err, ErrInvalidScope) {
		t.Errorf("bad scope: err = %v", err)
	}
	_, tok, err := e.s.MintToken(ctx, e.adaP, "phone", "", e.ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.RevokeToken(ctx, e.adaP, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetTokenScope(ctx, e.adaP, tok.ID, ScopeAdmin); !errors.Is(err, ErrRevoked) {
		t.Errorf("revoked: err = %v", err)
	}
	// No token of a disabled identity changes.
	_, live, err := e.s.MintToken(ctx, e.adaP, "laptop", "", e.ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Disable(ctx, e.ada.ID, e.owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.SetTokenScope(ctx, e.ownP, live.ID, ScopeAdmin); !errors.Is(err, ErrDisabled) {
		t.Errorf("disabled: err = %v", err)
	}
}

// Revoking a token a rotation replaced names the active token it led to,
// through every rotation; once that one is revoked too, revoking is a no-op.
func TestRevokeRotatedToken(t *testing.T) {
	ctx := context.Background()
	e := newTokenEnv(t)
	_, a, err := e.s.MintToken(ctx, e.adaP, "ci", "", e.nadia.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, b, err := e.s.RotateToken(ctx, e.adaP, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, c, err := e.s.RotateToken(ctx, e.adaP, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	var rotated *RotatedError
	if err := e.s.RevokeToken(ctx, e.adaP, a.ID); !errors.As(err, &rotated) || rotated.Into != c.ID || !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoke a rotated token: err = %v, want RotatedError into %s", err, c.ID)
	}
	if err := e.s.RevokeToken(ctx, e.adaP, c.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.s.RevokeToken(ctx, e.adaP, a.ID); err != nil {
		t.Fatalf("revoke with the chain all revoked: err = %v, want nil", err)
	}
}

// Rotation keeps the binding, name and scope, and revokes the old token in the
// same transaction.
func TestRotateToken(t *testing.T) {
	ctx := context.Background()
	e := newTokenEnv(t)
	oldSecret, old, err := e.s.MintToken(ctx, e.adaP, "ci", ScopeAdmin, e.ada.ID)
	if err != nil {
		t.Fatal(err)
	}
	newSecret, tok, err := e.s.RotateToken(ctx, e.ownP, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if tok.ID == old.ID || tok.Binding != old.Binding || tok.Name != "ci" || tok.Scope != ScopeAdmin ||
		tok.Origin != OriginRotate || tok.OriginRef != old.ID || tok.CreatedBy != e.owner.ID || newSecret == oldSecret {
		t.Fatalf("rotated = %+v", tok)
	}
	if _, err := e.s.Authenticate(ctx, oldSecret); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("old secret: err = %v, want ErrTokenInvalid", err)
	}
	if p, err := e.s.Authenticate(ctx, newSecret); err != nil || p.TokenID != tok.ID || p.Binding != old.Binding {
		t.Fatalf("new secret: %+v, %v", p, err)
	}
	stored, err := getTokenIn(ctx, e.s.db, old.ID)
	if err != nil || !stored.Revoked() || stored.RevokedBy != e.owner.ID || !stored.RevokedAt.Equal(tok.CreatedAt) {
		t.Fatalf("old token = %+v, %v", stored, err)
	}
	created := e.eventsOf(t, event.TokenCreated)
	if last := created[len(created)-1]; last["origin"] != "rotate" || last["replaces"] != string(old.ID) {
		t.Fatalf("token.created = %v", last)
	}
	if got := e.eventsOf(t, event.TokenRevoked); len(got) != 1 || got[0]["token"] != string(old.ID) ||
		got[0]["reason"] != "rotated" || got[0]["replaced_by"] != string(tok.ID) {
		t.Fatalf("token.revoked = %v", got)
	}

	// The old one can't be rotated again.
	if _, _, err := e.s.RotateToken(ctx, e.ownP, old.ID); !errors.Is(err, ErrRevoked) {
		t.Fatalf("rotate a revoked token: err = %v", err)
	}
	if _, _, err := e.s.RotateToken(ctx, e.ownP, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rotate unknown: err = %v", err)
	}
}

func TestRotateTokenRefusals(t *testing.T) {
	ctx := context.Background()
	e := newTokenEnv(t)
	ownerP, err := e.s.Authenticate(ctx, e.ownerSecret)
	if err != nil {
		t.Fatal(err)
	}
	// An admin rotating the owner's token would hold an owner secret.
	if _, _, err := e.s.RotateToken(ctx, e.adaP, ownerP.TokenID); !errors.Is(err, ErrOwnerOnly) {
		t.Fatalf("owner's token by an admin: err = %v", err)
	}
	if _, err := e.s.Authenticate(ctx, e.ownerSecret); err != nil {
		t.Fatalf("a refused rotation revoked the token: %v", err)
	}
	_, tok, err := e.s.MintToken(ctx, e.adaP, "phone", "", e.nadia.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Disable(ctx, e.nadia.ID, e.ada.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.RotateToken(ctx, e.adaP, tok.ID); !errors.Is(err, ErrDisabled) {
		t.Fatalf("disabled: err = %v", err)
	}
	if stored, err := getTokenIn(ctx, e.s.db, tok.ID); err != nil || stored.Revoked() {
		t.Fatalf("a refused rotation revoked the token: %+v, %v", stored, err)
	}
}

// Self-rotation replaces only the calling token: another on the same binding
// keeps working.
func TestRotateOwnToken(t *testing.T) {
	ctx := context.Background()
	e := newTokenEnv(t)
	ciSecret, ci, err := e.s.MintToken(ctx, e.adaP, "automations", "", e.nadia.ID)
	if err != nil {
		t.Fatal(err)
	}
	backupSecret, _, err := e.s.AddToken(ctx, e.adaP, ci.Binding, "backup", "")
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.s.Authenticate(ctx, ciSecret)
	if err != nil {
		t.Fatal(err)
	}
	newSecret, tok, err := e.s.RotateOwnToken(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if tok.OriginRef != ci.ID || tok.CreatedBy != e.nadia.ID || tok.Name != "automations" {
		t.Fatalf("rotated = %+v", tok)
	}
	if _, err := e.s.Authenticate(ctx, ciSecret); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("calling token: err = %v", err)
	}
	for _, secret := range []string{backupSecret, newSecret} {
		if _, err := e.s.Authenticate(ctx, secret); err != nil {
			t.Fatalf("other token: %v", err)
		}
	}
	// The socket has no token to rotate.
	sock, err := e.s.SocketPrincipal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.RotateOwnToken(ctx, sock); !errors.Is(err, ErrNoToken) {
		t.Fatalf("socket: err = %v, want ErrNoToken", err)
	}
}

func TestRevokeTokenRecords(t *testing.T) {
	ctx := context.Background()
	e := newTokenEnv(t)
	_, tok, err := e.s.MintToken(ctx, e.adaP, "phone", "", e.nadia.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.RevokeToken(ctx, e.adaP, tok.ID); err != nil {
		t.Fatal(err)
	}
	// Revoking twice keeps the first revoker and records nothing new.
	if err := e.s.RevokeToken(ctx, e.ownP, tok.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := getTokenIn(ctx, e.s.db, tok.ID)
	if err != nil || stored.RevokedBy != e.ada.ID || stored.IdentityID != e.nadia.ID {
		t.Fatalf("revoked = %+v, %v", stored, err)
	}
	if got := e.eventsOf(t, event.TokenRevoked); len(got) != 1 || got[0]["by"] != string(e.ada.ID) {
		t.Fatalf("token.revoked = %v", got)
	}
	ownerP, err := e.s.Authenticate(ctx, e.ownerSecret)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.RevokeToken(ctx, e.adaP, ownerP.TokenID); !errors.Is(err, ErrOwnerOnly) {
		t.Fatalf("owner's token by an admin: err = %v", err)
	}
}

// Unbinding revokes the binding's tokens, each with its event.
func TestUnbindRevokesTokensWithEvents(t *testing.T) {
	ctx := context.Background()
	e := newTokenEnv(t)
	_, a, err := e.s.MintToken(ctx, e.adaP, "automations", "", e.nadia.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, b, err := e.s.AddToken(ctx, e.adaP, a.Binding, "backup", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Unbind(ctx, event.ChannelHolaclaw, a.Binding, e.adaP); err != nil {
		t.Fatal(err)
	}
	got := e.eventsOf(t, event.TokenRevoked)
	if len(got) != 2 {
		t.Fatalf("token.revoked = %v, want two", got)
	}
	for i, want := range []event.ID{a.ID, b.ID} {
		if got[i]["token"] != string(want) || got[i]["reason"] != "unbound" || got[i]["by"] != string(e.ada.ID) ||
			got[i]["identity"] != string(e.nadia.ID) {
			t.Errorf("token.revoked[%d] = %v", i, got[i])
		}
	}
	if stored, err := getTokenIn(ctx, e.s.db, b.ID); err != nil || stored.RevokedBy != e.ada.ID {
		t.Fatalf("revoked = %+v, %v", stored, err)
	}

	// Deleting an identity revokes its tokens the same way.
	_, c, err := e.s.MintToken(ctx, e.adaP, "laptop", "", e.nadia.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.DeleteIdentity(ctx, e.nadia.ID, true, e.ada.ID); err != nil {
		t.Fatal(err)
	}
	got = e.eventsOf(t, event.TokenRevoked)
	if last := got[len(got)-1]; len(got) != 3 || last["token"] != string(c.ID) || last["reason"] != "identity_deleted" {
		t.Fatalf("token.revoked = %v", got)
	}
}

func TestListTokensFilter(t *testing.T) {
	ctx := context.Background()
	e := newTokenEnv(t)
	_, phone, err := e.s.MintToken(ctx, e.adaP, "phone", "", e.nadia.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, ci, err := e.s.MintToken(ctx, e.adaP, "ci", "", e.nadia.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.s.RotateToken(ctx, e.adaP, ci.ID); err != nil {
		t.Fatal(err)
	}
	ids := func(f TokenFilter) []event.ID {
		t.Helper()
		list, err := e.s.ListTokens(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]event.ID, 0, len(list))
		for _, tok := range list {
			out = append(out, tok.ID)
		}
		return out
	}
	if got := ids(TokenFilter{IdentityID: e.nadia.ID}); len(got) != 2 || got[0] != phone.ID {
		t.Fatalf("nadia's active = %v", got)
	}
	if got := ids(TokenFilter{IdentityID: e.nadia.ID, All: true}); len(got) != 3 {
		t.Fatalf("nadia's, all = %v", got)
	}
	if got := ids(TokenFilter{Binding: ci.Binding, All: true}); len(got) != 2 || got[0] != ci.ID {
		t.Fatalf("ci's binding, all = %v", got)
	}
	if got := ids(TokenFilter{Binding: ci.Binding}); len(got) != 1 || got[0] == ci.ID {
		t.Fatalf("ci's binding, active = %v", got)
	}
	// The init token is the owner's only one.
	if got := ids(TokenFilter{}); len(got) != 3 {
		t.Fatalf("every active token = %v", got)
	}
}
