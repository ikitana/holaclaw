package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// guest makes a pending identity the way an open DM does.
func (h *harness) guest(alias string) store.Identity {
	h.t.Helper()
	i, err := h.auth.EnsureGuest(context.Background(), "telegram", []string{alias}, "Guest "+alias)
	if err != nil {
		h.t.Fatalf("ensure guest: %v", err)
	}
	return i
}

func (h *harness) patchIdentity(id string, body any) *http.Response {
	h.t.Helper()
	return h.do(http.MethodPatch, "/identities/"+id, h.adminToken, body)
}

type identityList struct {
	Identities []gen.User `json:"identities"`
}

func TestListIdentities(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")

	all := decode[identityList](t, h.do(http.MethodGet, "/identities", h.adminToken, nil), http.StatusOK)
	// The owner, nadia, ada and the guest.
	if len(all.Identities) != 4 {
		t.Fatalf("identities = %d, want 4", len(all.Identities))
	}

	pending := decode[identityList](t, h.do(http.MethodGet, "/identities?pending=true", h.adminToken, nil), http.StatusOK)
	if len(pending.Identities) != 1 || pending.Identities[0].Id != string(g.ID) {
		t.Fatalf("pending = %+v, want only the guest", pending.Identities)
	}
	got := pending.Identities[0]
	if roleOf(got.Role) != gen.UserRoleGuest || got.Identities == nil || (*got.Identities)[0].Alias != "111" {
		t.Fatalf("pending guest = %+v", got)
	}
}

func TestGetIdentity(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")

	u := decode[gen.User](t, h.do(http.MethodGet, "/identities/"+string(g.ID), h.adminToken, nil), http.StatusOK)
	if u.Id != string(g.ID) || u.Identities == nil || len(*u.Identities) != 1 {
		t.Fatalf("identity = %+v", u)
	}
	errorBody(t, h.do(http.MethodGet, "/identities/"+string(event.NewID()), h.adminToken, nil),
		http.StatusNotFound, CodeNotFound)
}

func TestConfirmPendingIdentity(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")

	u := decode[gen.User](t, h.patchIdentity(string(g.ID), gen.IdentityUpdate{Name: ptr("Rosa")}), http.StatusOK)
	if u.Name != "Rosa" || roleOf(u.Role) != gen.UserRoleUser {
		t.Fatalf("confirmed = %+v, want Rosa as user", u)
	}
	if u.Identities == nil || len(*u.Identities) != 1 || (*u.Identities)[0].Alias != "111" {
		t.Fatalf("the IDs were not kept: %+v", u.Identities)
	}

	evs, err := h.st.ListEventsAfter(context.Background(), "", "", 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var found bool
	for _, e := range evs {
		if e.Type != event.IdentityConfirmed {
			continue
		}
		var attrs map[string]any
		if err := json.Unmarshal(e.Attrs, &attrs); err != nil {
			t.Fatalf("attrs: %v", err)
		}
		if attrs["identity"] == string(g.ID) {
			found = true
		}
	}
	if !found {
		t.Fatal("no identity.confirmed event")
	}

	pending := decode[identityList](t, h.do(http.MethodGet, "/identities?pending=true", h.adminToken, nil), http.StatusOK)
	if len(pending.Identities) != 0 {
		t.Fatalf("pending after confirm = %+v", pending.Identities)
	}
}

func TestConfirmPendingIdentityAsAdmin(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")
	role := gen.IdentityUpdateRoleAdmin

	u := decode[gen.User](t, h.patchIdentity(string(g.ID), gen.IdentityUpdate{Name: ptr("Rosa"), Role: &role}), http.StatusOK)
	if roleOf(u.Role) != gen.UserRoleAdmin {
		t.Fatalf("role = %q, want admin", roleOf(u.Role))
	}
}

func TestConfirmPendingIdentityNeedsAName(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")

	errorBody(t, h.patchIdentity(string(g.ID), gen.IdentityUpdate{}), http.StatusBadRequest, CodeBadRequest)
	errorBody(t, h.patchIdentity(string(g.ID), gen.IdentityUpdate{Name: ptr("")}), http.StatusBadRequest, CodeBadRequest)
	if i, _ := h.st.GetIdentity(context.Background(), g.ID); !i.Pending() {
		t.Fatalf("role = %q, want still guest", i.Role)
	}
}

func TestUpdateIdentity(t *testing.T) {
	h := newHarness(t)
	admin := gen.IdentityUpdateRoleAdmin

	u := decode[gen.User](t, h.patchIdentity(string(h.other.ID), gen.IdentityUpdate{Role: &admin}), http.StatusOK)
	if roleOf(u.Role) != gen.UserRoleAdmin || u.Name != "nadia" {
		t.Fatalf("updated = %+v", u)
	}

	// The owner can be renamed, but their role cannot change.
	u = decode[gen.User](t, h.patchIdentity(string(h.owner.ID), gen.IdentityUpdate{Name: ptr("Angel")}), http.StatusOK)
	if u.Name != "Angel" || roleOf(u.Role) != gen.UserRoleOwner {
		t.Fatalf("owner = %+v", u)
	}
	errorBody(t, h.patchIdentity(string(h.owner.ID), gen.IdentityUpdate{Role: &admin}), http.StatusConflict, CodeConflict)

	// Nobody can be made owner.
	errorBody(t, h.patchIdentity(string(h.other.ID), map[string]string{"role": "owner"}), http.StatusBadRequest, CodeBadRequest)
	errorBody(t, h.patchIdentity(string(h.other.ID), map[string]string{"role": "guest"}), http.StatusBadRequest, CodeBadRequest)

	// An empty update changes nothing and says so.
	errorBody(t, h.patchIdentity(string(h.other.ID), gen.IdentityUpdate{}), http.StatusBadRequest, CodeBadRequest)

	errorBody(t, h.patchIdentity(string(event.NewID()), gen.IdentityUpdate{Name: ptr("x")}), http.StatusNotFound, CodeNotFound)
	errorBody(t, h.do(http.MethodPatch, "/identities/"+string(h.other.ID), h.adminToken, "not an object"),
		http.StatusBadRequest, CodeBadRequest)
}

func TestIdentitiesNeedAdmin(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")

	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/identities"},
		{http.MethodGet, "/identities/" + string(g.ID)},
		{http.MethodPatch, "/identities/" + string(g.ID)},
		{http.MethodDelete, "/identities/" + string(g.ID)},
	} {
		var body any
		if c.method == http.MethodPatch {
			body = gen.IdentityUpdate{Name: ptr("Rosa")}
		}
		errorBody(t, h.do(c.method, c.path, h.ownerToken, body), http.StatusForbidden, CodeForbidden)
	}
	if i, _ := h.st.GetIdentity(context.Background(), g.ID); !i.Pending() {
		t.Fatalf("a client token confirmed the guest: %+v", i)
	}
}

func TestDeleteIdentity(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")

	got := decode[gen.IdentityDeleted](t, h.do(http.MethodDelete, "/identities/"+string(g.ID), h.adminToken, nil), http.StatusOK)
	if got.Id != string(g.ID) || got.Role == nil || *got.Role != gen.IdentityDeletedRoleGuest {
		t.Fatalf("deleted = %+v", got)
	}
	if len(got.Aliases) != 1 || got.Aliases[0] != "telegram:111" {
		t.Fatalf("aliases = %+v, want [telegram:111]", got.Aliases)
	}
	if got.MemoryKept {
		t.Fatalf("memory_kept = true, want false (default)")
	}

	errorBody(t, h.do(http.MethodGet, "/identities/"+string(g.ID), h.adminToken, nil), http.StatusNotFound, CodeNotFound)
}

func TestDeleteIdentityKeepMemory(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")

	got := decode[gen.IdentityDeleted](t,
		h.do(http.MethodDelete, "/identities/"+string(g.ID)+"?keep_memory=true", h.adminToken, nil), http.StatusOK)
	if !got.MemoryKept {
		t.Fatalf("memory_kept = false, want true")
	}
	if got.MemoryForgotten != 0 {
		t.Fatalf("memory_forgotten = %d, want 0 when kept", got.MemoryForgotten)
	}
}

func TestDeleteIdentityRefusesOwner(t *testing.T) {
	h := newHarness(t)

	errorBody(t, h.do(http.MethodDelete, "/identities/"+string(h.owner.ID), h.adminToken, nil),
		http.StatusConflict, CodeConflict)
}

func TestDeleteIdentityUnknown(t *testing.T) {
	h := newHarness(t)

	errorBody(t, h.do(http.MethodDelete, "/identities/"+string(event.NewID()), h.adminToken, nil),
		http.StatusNotFound, CodeNotFound)
}

func TestClientDeleteIdentity(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")
	ctx := context.Background()
	c, err := Dial(h.srv.URL, h.adminToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	deleted, err := c.DeleteIdentity(ctx, string(g.ID), false)
	if err != nil || deleted.Id != string(g.ID) || deleted.MemoryKept {
		t.Fatalf("delete = %+v, %v", deleted, err)
	}
	if _, err := c.GetIdentity(ctx, string(g.ID)); err == nil {
		t.Fatal("deleted identity still found")
	}

	_, err = c.DeleteIdentity(ctx, string(h.owner.ID), false)
	var ce *ClientError
	if !errors.As(err, &ce) || ce.Status != http.StatusConflict {
		t.Fatalf("delete owner err = %v, want 409", err)
	}
}

func TestClientIdentities(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")
	ctx := context.Background()
	c, err := Dial(h.srv.URL, h.adminToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	pending, err := c.ListIdentities(ctx, true)
	if err != nil || len(pending) != 1 {
		t.Fatalf("list pending = %+v, %v", pending, err)
	}
	all, err := c.ListIdentities(ctx, false)
	if err != nil || len(all) != 4 {
		t.Fatalf("list all = %d, %v", len(all), err)
	}
	if _, err := c.GetIdentity(ctx, string(g.ID)); err != nil {
		t.Fatalf("get: %v", err)
	}
	u, err := c.UpdateIdentity(ctx, string(g.ID), gen.IdentityUpdate{Name: ptr("Rosa")})
	if err != nil || roleOf(u.Role) != gen.UserRoleUser {
		t.Fatalf("update = %+v, %v", u, err)
	}

	_, err = c.UpdateIdentity(ctx, string(h.owner.ID), gen.IdentityUpdate{Role: ptr(gen.IdentityUpdateRoleUser)})
	var ce *ClientError
	if !errors.As(err, &ce) || ce.Status != http.StatusConflict {
		t.Fatalf("owner role change err = %v, want 409", err)
	}
	if _, err := c.GetIdentity(ctx, string(event.NewID())); err == nil {
		t.Fatal("unknown id: no error")
	}
}
