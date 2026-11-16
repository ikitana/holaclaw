package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/store/storetest"
)

// eventAttrs returns the attrs of every stored event of type typ.
func (h *harness) eventAttrs(typ event.Type) []map[string]any {
	h.t.Helper()
	evs, err := h.st.ListEventsAfter(context.Background(), "", "", 0)
	if err != nil {
		h.t.Fatalf("list events: %v", err)
	}
	var out []map[string]any
	for _, e := range evs {
		if e.Type != typ {
			continue
		}
		var attrs map[string]any
		if err := json.Unmarshal(e.Attrs, &attrs); err != nil {
			h.t.Fatalf("attrs: %v", err)
		}
		out = append(out, attrs)
	}
	return out
}

// hasEvent reports whether an event of type typ carries every pair in want.
func (h *harness) hasEvent(typ event.Type, want map[string]any) bool {
	h.t.Helper()
	for _, attrs := range h.eventAttrs(typ) {
		ok := true
		for k, v := range want {
			ok = ok && attrs[k] == v
		}
		if ok {
			return true
		}
	}
	return false
}

func (h *harness) create(body any) *http.Response {
	h.t.Helper()
	return h.do(http.MethodPost, "/identities", h.adminToken, body)
}

func (h *harness) switchIdentity(id event.ID, verb, token string) *http.Response {
	h.t.Helper()
	return h.do(http.MethodPost, "/identities/"+string(id)+"/"+verb, token, nil)
}

func (h *harness) unbind(ref, token string) *http.Response {
	h.t.Helper()
	return h.do(http.MethodDelete, "/bindings/"+url.PathEscape(ref), token, nil)
}

func TestCreateIdentity(t *testing.T) {
	h := newHarness(t)

	u := decode[gen.User](t, h.create(gen.IdentityCreate{Name: "Rosa"}), http.StatusCreated)
	if u.Name != "Rosa" || roleOf(u.Role) != gen.UserRoleUser || u.Status != gen.UserStatusConfirmed ||
		u.Kind != gen.UserKindUser || u.Identities != nil {
		t.Fatalf("created = %+v, want a confirmed user with no bindings", u)
	}
	got := decode[gen.User](t, h.do(http.MethodGet, "/identities/"+u.Id, h.adminToken, nil), http.StatusOK)
	if got.Id != u.Id || got.Name != "Rosa" {
		t.Fatalf("get = %+v", got)
	}

	adm := decode[gen.User](t, h.create(gen.IdentityCreate{Name: "Ada", Role: ptr(gen.IdentityCreateRoleAdmin)}), http.StatusCreated)
	if roleOf(adm.Role) != gen.UserRoleAdmin {
		t.Fatalf("role = %q, want admin", roleOf(adm.Role))
	}

	// The creating admin is on the event, and the identity has no bindings.
	if !h.hasEvent(event.IdentityCreated, map[string]any{"identity": u.Id, "status": "confirmed"}) {
		t.Fatalf("no identity.created for %s: %v", u.Id, h.eventAttrs(event.IdentityCreated))
	}
	for _, attrs := range h.eventAttrs(event.IdentityCreated) {
		if attrs["identity"] == u.Id && (attrs["by"] == nil || attrs["bindings"] != nil) {
			t.Fatalf("identity.created attrs = %v, want by and no bindings", attrs)
		}
	}
}

func TestCreateIdentityRefusals(t *testing.T) {
	h := newHarness(t)

	for name, body := range map[string]any{
		"no name":    gen.IdentityCreate{},
		"blank name": gen.IdentityCreate{Name: "  "},
		"guest":      map[string]string{"name": "x", "role": "guest"},
		"owner":      map[string]string{"name": "x", "role": "owner"},
		"not json":   "nope",
	} {
		t.Run(name, func(t *testing.T) {
			errorBody(t, h.create(body), http.StatusBadRequest, CodeBadRequest)
		})
	}
	errorBody(t, h.do(http.MethodPost, "/identities", h.ownerToken, gen.IdentityCreate{Name: "x"}),
		http.StatusForbidden, CodeForbidden)
	all := decode[identityList](t, h.do(http.MethodGet, "/identities", h.adminToken, nil), http.StatusOK)
	if len(all.Identities) != 3 {
		t.Fatalf("identities = %d after refusals, want 3", len(all.Identities))
	}
}

func TestUnbind(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	g := h.guest("111")

	if _, err := h.st.DB().ExecContext(ctx, `INSERT INTO memory_items (id, agent, layer, text, visibility, "partition", user_id, created_at)
		VALUES ('m1', 'main', 'note', 'likes tea', 'origin', ?, ?, 1)`, "dm/"+string(g.ID), string(g.ID)); err != nil {
		t.Fatal(err)
	}
	out := decode[gen.BindingUnbound](t, h.unbind("telegram:111", h.adminToken), http.StatusOK)
	if out.Identity != string(g.ID) || !out.PendingDeleted || out.MemoryForgotten != 1 || out.TokensRevoked != 0 {
		t.Fatalf("unbound = %+v", out)
	}
	if _, err := h.resolveAlias(ctx, "telegram", []string{"111"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resolve unbound = %v, want ErrNotFound", err)
	}
	if !h.hasEvent(event.IdentityUnbound, map[string]any{"identity": string(g.ID), "binding": "telegram:111"}) {
		t.Fatalf("no identity.unbound: %v", h.eventAttrs(event.IdentityUnbound))
	}
	// The last binding of a pending identity took it along.
	errorBody(t, h.do(http.MethodGet, "/identities/"+string(g.ID), h.adminToken, nil), http.StatusNotFound, CodeNotFound)
	if !h.hasEvent(event.IdentityDeleted, map[string]any{"identity": string(g.ID), "reason": "pending_unbound"}) {
		t.Fatalf("no identity.deleted: %v", h.eventAttrs(event.IdentityDeleted))
	}
}

func TestUnbindKeepsAConfirmedIdentity(t *testing.T) {
	h := newHarness(t)
	storetest.Bind(t, h.st, "wa", []string{"lid:123"}, h.other.ID, "")

	// `wa:lid:123` is channel wa, alias lid:123.
	out := decode[gen.BindingUnbound](t, h.unbind("wa:lid:123", h.adminToken), http.StatusOK)
	if out.Identity != string(h.other.ID) || out.PendingDeleted || out.MemoryForgotten != 0 {
		t.Fatalf("unbound = %+v", out)
	}
	u := decode[gen.User](t, h.do(http.MethodGet, "/identities/"+string(h.other.ID), h.adminToken, nil), http.StatusOK)
	// What stays is the binding of nadia's token.
	if u.Id != string(h.other.ID) || u.Identities == nil || len(*u.Identities) != 1 || (*u.Identities)[0].Channel != event.ChannelHolaclaw {
		t.Fatalf("confirmed identity after its wa unbind = %+v", u)
	}
}

func TestUnbindRefused(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	g := h.guest("111")
	storetest.Bind(t, h.st, "tg", []string{"owner1"}, h.owner.ID, "")

	for _, c := range []struct {
		name, ref string
		status    int
		code      string
	}{
		{"unknown binding", "telegram:999", http.StatusNotFound, CodeNotFound},
		{"unknown channel", "nochannel:111", http.StatusNotFound, CodeNotFound},
		{"no colon", "telegram", http.StatusBadRequest, CodeBadRequest},
		{"empty alias", "telegram:", http.StatusBadRequest, CodeBadRequest},
		{"empty channel", ":111", http.StatusBadRequest, CodeBadRequest},
		{"the owner's, by an admin", "tg:owner1", http.StatusForbidden, CodeForbidden},
	} {
		t.Run(c.name, func(t *testing.T) {
			errorBody(t, h.unbind(c.ref, h.adminToken), c.status, c.code)
		})
	}
	for _, tok := range []string{h.ownerToken, h.otherToken} {
		errorBody(t, h.unbind("telegram:111", tok), http.StatusForbidden, CodeForbidden)
	}
	// The owner unbinds the owner.
	if out := decode[gen.BindingUnbound](t, h.unbind("tg:owner1", h.ownerAdmin), http.StatusOK); out.Identity != string(h.owner.ID) {
		t.Fatalf("owner unbind = %+v", out)
	}
	if i, err := h.resolveAlias(ctx, "telegram", []string{"111"}); err != nil || i.ID != g.ID {
		t.Fatalf("a refused unbind touched the binding: %+v, %v", i, err)
	}
}

func TestClientLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	c, err := Dial(h.srv.URL, h.adminToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	u, err := c.CreateIdentity(ctx, "Rosa", "")
	if err != nil || roleOf(u.Role) != gen.UserRoleUser {
		t.Fatalf("create = %+v, %v", u, err)
	}
	if _, err := c.CreateIdentity(ctx, "Bad", "guest"); err == nil {
		t.Fatal("creating a guest: no error")
	}

	// A Google Chat alias carries a slash, and a colon splits only once.
	storetest.Bind(t, h.st, "gchat", []string{"users/42", "a:b"}, h.other.ID, "")
	for _, alias := range []string{"users/42", "a:b"} {
		if out, err := c.Unbind(ctx, "gchat", alias); err != nil || out.Identity != string(h.other.ID) || out.PendingDeleted {
			t.Fatalf("unbind %s = %+v, %v", alias, out, err)
		}
		if _, err := h.resolveAlias(ctx, "gchat", []string{alias}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("resolve %s after unbind = %v, want ErrNotFound", alias, err)
		}
	}
	var ce *ClientError
	if _, err := c.Unbind(ctx, "gchat", "users/42"); !errors.As(err, &ce) || ce.Status != http.StatusNotFound {
		t.Fatalf("second unbind err = %v, want 404", err)
	}

	d, err := c.DisableIdentity(ctx, string(u.Id))
	if err != nil || d.Status != gen.UserStatusDisabled {
		t.Fatalf("disable = %+v, %v", d, err)
	}
	e, err := c.EnableIdentity(ctx, string(u.Id))
	if err != nil || e.Status != gen.UserStatusConfirmed {
		t.Fatalf("enable = %+v, %v", e, err)
	}
	if _, err := c.DisableIdentity(ctx, string(h.owner.ID)); !errors.As(err, &ce) || ce.Status != http.StatusConflict {
		t.Fatalf("disable owner err = %v, want 409", err)
	}
}

func TestAssignOntoDisabledIsRefused(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")
	resp := h.switchIdentity(h.other.ID, "disable", h.adminToken)
	_ = resp.Body.Close()

	errorBody(t, h.assign(string(g.ID), string(h.other.ID), h.adminToken), http.StatusConflict, CodeConflict)
	if i, err := h.st.GetIdentity(context.Background(), g.ID); err != nil || !i.Pending() {
		t.Fatalf("a refused assign touched the pending identity: %+v, %v", i, err)
	}
}

func TestAssignNeedsTo(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")

	// The old field name is no longer read.
	errorBody(t, h.do(http.MethodPost, "/identities/"+string(g.ID)+"/assign", h.adminToken,
		map[string]string{"target": string(h.other.ID)}), http.StatusBadRequest, CodeBadRequest)
	out := decode[gen.IdentityAssignment](t, h.assign(string(g.ID), string(h.other.ID), h.adminToken), http.StatusOK)
	if out.Bindings != 1 {
		t.Fatalf("bindings = %d, want 1", out.Bindings)
	}
}

func TestDisableAndEnable(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")

	u := decode[gen.User](t, h.switchIdentity(h.other.ID, "disable", h.adminToken), http.StatusOK)
	if u.Status != gen.UserStatusDisabled || roleOf(u.Role) != gen.UserRoleUser {
		t.Fatalf("disabled = %+v", u)
	}
	// Disabling twice changes nothing and reports nothing.
	decode[gen.User](t, h.switchIdentity(h.other.ID, "disable", h.adminToken), http.StatusOK)
	if n := len(h.eventAttrs(event.IdentityDisabled)); n != 1 {
		t.Fatalf("identity.disabled events = %d, want 1", n)
	}
	if !h.hasEvent(event.IdentityDisabled, map[string]any{"identity": string(h.other.ID)}) {
		t.Fatal("no identity.disabled for the identity")
	}

	// Its token is refused with its own code, not a 401, and works again after enable.
	errorBody(t, h.do(http.MethodGet, "/sessions", h.otherToken, nil), http.StatusForbidden, CodeIdentityDisabled)

	u = decode[gen.User](t, h.switchIdentity(h.other.ID, "enable", h.adminToken), http.StatusOK)
	if u.Status != gen.UserStatusConfirmed {
		t.Fatalf("enabled = %+v", u)
	}
	decode[struct {
		Sessions []gen.Session `json:"sessions"`
	}](t, h.do(http.MethodGet, "/sessions", h.otherToken, nil), http.StatusOK)
	if !h.hasEvent(event.IdentityEnabled, map[string]any{"identity": string(h.other.ID)}) {
		t.Fatal("no identity.enabled")
	}

	for _, verb := range []string{"disable", "enable"} {
		errorBody(t, h.switchIdentity(h.owner.ID, verb, h.adminToken), http.StatusConflict, CodeConflict)
		errorBody(t, h.switchIdentity(g.ID, verb, h.adminToken), http.StatusConflict, CodeConflict)
		errorBody(t, h.switchIdentity(event.NewID(), verb, h.adminToken), http.StatusNotFound, CodeNotFound)
		errorBody(t, h.switchIdentity(h.other.ID, verb, h.ownerToken), http.StatusForbidden, CodeForbidden)
	}
	if i, _ := h.st.GetIdentity(context.Background(), h.owner.ID); i.Disabled() {
		t.Fatal("the owner was disabled")
	}
}

// A disabled identity cannot be invited to, and the reason reaches the caller.
func TestInviteForDisabledIdentity(t *testing.T) {
	h := newHarness(t)
	resp := h.switchIdentity(h.other.ID, "disable", h.adminToken)
	_ = resp.Body.Close()

	id := string(h.other.ID)
	errorBody(t, h.do(http.MethodPost, "/invites", h.adminToken, gen.InviteCreate{Channel: "holaclaw", UserId: &id}),
		http.StatusBadRequest, CodeBadRequest)
}

// No token is minted for a disabled identity, as nothing is assigned onto one.
func TestTokenForDisabledIdentity(t *testing.T) {
	h := newHarness(t)
	resp := h.switchIdentity(h.other.ID, "disable", h.adminToken)
	_ = resp.Body.Close()

	errorBody(t, h.do(http.MethodPost, "/tokens", h.adminToken,
		gen.TokenCreate{Name: "t", Scope: ptr(gen.TokenCreateScope("client")), UserId: ptr(string(h.other.ID))}),
		http.StatusConflict, CodeConflict)
}

func TestWireCarriesStatusKindAndBindingDates(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")

	u := decode[gen.User](t, h.do(http.MethodGet, "/identities/"+string(g.ID), h.adminToken, nil), http.StatusOK)
	if u.Status != gen.UserStatusPending || roleOf(u.Role) != gen.UserRoleGuest || u.Kind != gen.UserKindUser {
		t.Fatalf("pending = %+v", u)
	}
	if u.Identities == nil || (*u.Identities)[0].CreatedAt.IsZero() {
		t.Fatalf("binding without a date: %+v", u.Identities)
	}
}

func TestDeleteIdentityReportsTokens(t *testing.T) {
	h := newHarness(t)
	got := decode[gen.IdentityDeleted](t, h.do(http.MethodDelete, "/identities/"+string(h.other.ID), h.adminToken, nil), http.StatusOK)
	if got.TokensRevoked != 1 {
		t.Fatalf("tokens_revoked = %d, want 1 (the other user's token)", got.TokensRevoked)
	}
}
