package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
)

func (h *harness) assign(id, target, token string) *http.Response {
	h.t.Helper()
	return h.do(http.MethodPost, "/identities/"+id+"/assign", token, gen.IdentityAssign{To: target})
}

func TestAssignIdentity(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")

	out := decode[gen.IdentityAssignment](t, h.assign(string(g.ID), string(h.other.ID), h.adminToken), http.StatusOK)
	if out.Identity.Id != string(h.other.ID) || out.Bindings != 1 {
		t.Fatalf("assignment = %+v", out)
	}
	var moved bool
	for _, id := range *out.Identity.Identities {
		moved = moved || (id.Channel == "telegram" && id.Alias == "111")
	}
	if !moved {
		t.Fatalf("telegram/111 not on the target: %+v", out.Identity.Identities)
	}
	errorBody(t, h.do(http.MethodGet, "/identities/"+string(g.ID), h.adminToken, nil), http.StatusNotFound, CodeNotFound)

	evs, err := h.st.ListEventsAfter(context.Background(), "", "", 0)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var found bool
	for _, e := range evs {
		found = found || e.Type == event.IdentityAssigned
	}
	if !found {
		t.Fatal("no identity.assigned event")
	}
}

func TestAssignIdentityRefusals(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")
	g2 := h.guest("222")

	errorBody(t, h.assign(string(g.ID), string(g.ID), h.adminToken), http.StatusBadRequest, CodeBadRequest)
	errorBody(t, h.assign(string(g.ID), "", h.adminToken), http.StatusBadRequest, CodeBadRequest)
	errorBody(t, h.assign(string(h.other.ID), string(h.owner.ID), h.adminToken), http.StatusConflict, CodeConflict)
	errorBody(t, h.assign(string(g.ID), string(g2.ID), h.adminToken), http.StatusConflict, CodeConflict)
	errorBody(t, h.assign(string(g.ID), string(event.NewID()), h.adminToken), http.StatusNotFound, CodeNotFound)
	errorBody(t, h.assign(string(event.NewID()), string(h.owner.ID), h.adminToken), http.StatusNotFound, CodeNotFound)
	// A client token is not an admin, even the owner's.
	errorBody(t, h.assign(string(g.ID), string(h.owner.ID), h.ownerToken), http.StatusForbidden, CodeForbidden)
	// An admin is not the owner: only the owner links an account to the owner.
	errorBody(t, h.assign(string(g.ID), string(h.owner.ID), h.adminToken), http.StatusForbidden, CodeForbidden)

	if i, err := h.st.GetIdentity(context.Background(), g.ID); err != nil || !i.Pending() {
		t.Fatalf("a refused assign touched the guest: %+v, %v", i, err)
	}
}

func TestClientAssignIdentity(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")
	ctx := context.Background()
	c, err := Dial(h.srv.URL, h.ownerAdmin)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	out, err := c.AssignIdentity(ctx, string(g.ID), string(h.owner.ID))
	if err != nil || out.Identity.Id != string(h.owner.ID) {
		t.Fatalf("assign = %+v, %v", out, err)
	}
	_, err = c.AssignIdentity(ctx, string(h.other.ID), string(h.owner.ID))
	var ce *ClientError
	if !errors.As(err, &ce) || ce.Status != http.StatusConflict {
		t.Fatalf("assigning a confirmed user: err = %v, want 409", err)
	}
}
