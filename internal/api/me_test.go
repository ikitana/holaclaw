package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/store"
)

// me asks GET /v1/me with token.
func (h *harness) me(t *testing.T, token string) gen.Me {
	t.Helper()
	return decode[gen.Me](t, h.do(http.MethodGet, "/me", token, nil), http.StatusOK)
}

// GET /v1/me answers with the identity, the effective scope, the binding and
// the token, as the request resolved.
func TestGetMe(t *testing.T) {
	h := newHarness(t)

	got := h.me(t, h.otherToken)
	if got.Identity.Id != string(h.other.ID) || got.Identity.Name != "nadia" || got.Identity.Role != gen.MeIdentityRoleUser ||
		got.Scope != gen.MeScopeClient || got.Token == nil || got.Token.Name != "cli-other" ||
		got.Token.Binding != got.Binding || !strings.HasPrefix(got.Binding, "holaclaw:") || got.Token.LastUsedAt == nil {
		t.Fatalf("me = %+v", got)
	}
	if got := h.me(t, h.ownerAdmin); got.Identity.Role != gen.MeIdentityRoleOwner || got.Scope != gen.MeScopeAdmin {
		t.Fatalf("owner's admin token = %+v", got)
	}

	// On the socket: holaclaw:cli, the owner at admin scope, and no token.
	r := httptest.NewRequest(http.MethodGet, BaseURL+"/me", nil)
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, r.WithContext(markSocket(r.Context())))
	var sock gen.Me
	if err := json.Unmarshal(w.Body.Bytes(), &sock); err != nil || w.Code != http.StatusOK {
		t.Fatalf("socket: %d %s", w.Code, w.Body)
	}
	if sock.Binding != "holaclaw:"+auth.SocketAlias || sock.Token != nil || sock.Scope != gen.MeScopeAdmin ||
		sock.Identity.Id != string(h.owner.ID) || sock.Identity.Role != gen.MeIdentityRoleOwner {
		t.Fatalf("socket me = %+v", sock)
	}
}

// The effective scope follows a scope change and a demotion at once, while the
// token's prefix, fixed when it was minted, does not.
func TestGetMeFollowsTheEffectiveScope(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	dan, err := h.auth.CreateIdentity(ctx, "dan", store.RoleAdmin, h.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	secret, tok, err := h.auth.CreateToken(ctx, "ops", auth.ScopeAdmin, dan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.me(t, secret); got.Scope != gen.MeScopeAdmin || got.Identity.Role != gen.MeIdentityRoleAdmin {
		t.Fatalf("before = %+v", got)
	}

	decode[gen.Token](t, h.do(http.MethodPatch, "/tokens/"+string(tok.ID), h.ownerAdmin, gen.TokenUpdate{Scope: gen.TokenUpdateScopeClient}), http.StatusOK)
	got := h.me(t, secret)
	if got.Scope != gen.MeScopeClient || got.Token.Scope != gen.TokenScopeClient || !strings.HasPrefix(secret, "hb_a_") {
		t.Fatalf("after PATCH = %+v (prefix %s)", got, secret[:5])
	}

	decode[gen.Token](t, h.do(http.MethodPatch, "/tokens/"+string(tok.ID), h.ownerAdmin, gen.TokenUpdate{Scope: gen.TokenUpdateScopeAdmin}), http.StatusOK)
	if _, err := h.auth.Update(ctx, dan.ID, nil, ptr(store.RoleUser), h.owner.ID); err != nil {
		t.Fatal(err)
	}
	if got := h.me(t, secret); got.Scope != gen.MeScopeClient || got.Identity.Role != gen.MeIdentityRoleUser {
		t.Fatalf("after demotion = %+v", got)
	}
}

// A device's last activity is the latest use of any of its binding's tokens,
// a revoked one included, and absent while none was used.
func TestDeviceLastUsedAt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	inv := pairingInvite(h, h.other.ID, "")
	phone := decode[gen.TokenCreated](t, pair(h, inv.Code, "Phone", "ios"), http.StatusCreated)
	alias := strings.TrimPrefix(phone.Token.Binding, "holaclaw:")

	device := func() gen.Device {
		t.Helper()
		list := decode[struct{ Devices []gen.Device }](t, h.do(http.MethodGet, "/me/devices", h.otherToken, nil), http.StatusOK)
		for _, d := range list.Devices {
			if d.Binding == phone.Token.Binding {
				return d
			}
		}
		t.Fatalf("no phone in %+v", list.Devices)
		return gen.Device{}
	}
	if d := device(); d.LastUsedAt != nil {
		t.Fatalf("unused device last_used_at = %v", d.LastUsedAt)
	}

	_, second, err := h.auth.AddToken(ctx, auth.Principal{UserID: h.owner.ID, Role: store.RoleOwner}, alias, "Phone", auth.ScopeClient)
	if err != nil {
		t.Fatal(err)
	}
	at := func(id string, when time.Time) {
		t.Helper()
		if _, err := h.st.DB().ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, when.UnixMilli(), id); err != nil {
			t.Fatal(err)
		}
	}
	early, late := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	at(phone.Token.Id, early)
	at(string(second.ID), late)
	if d := device(); d.LastUsedAt == nil || !d.LastUsedAt.Equal(late) || d.Tokens != 2 {
		t.Fatalf("device = %+v, want the later use and 2 tokens", d)
	}
	if err := h.auth.RevokeToken(ctx, auth.Principal{Role: store.RoleOwner}, second.ID); err != nil {
		t.Fatal(err)
	}
	if d := device(); d.LastUsedAt == nil || !d.LastUsedAt.Equal(late) || d.Tokens != 1 {
		t.Fatalf("device = %+v, want the revoked token's use kept and 1 active token", d)
	}
}

// A binding with no tokens, such as holaclaw:cli, counts none and was never
// used: the outer join's empty row is no token.
func TestDeviceWithoutTokens(t *testing.T) {
	h := newHarness(t)
	list := decode[struct{ Devices []gen.Device }](t, h.do(http.MethodGet, "/me/devices", h.ownerToken, nil), http.StatusOK)
	for _, d := range list.Devices {
		if d.Binding != "holaclaw:"+auth.SocketAlias {
			continue
		}
		if d.Tokens != 0 || d.LastUsedAt != nil {
			t.Fatalf("holaclaw:cli = %+v, want no tokens and no use", d)
		}
		return
	}
	t.Fatalf("no holaclaw:cli in %+v", list.Devices)
}
