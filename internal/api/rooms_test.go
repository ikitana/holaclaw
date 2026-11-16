package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// roleOf reads the optional role of a user on the wire: "" when absent.
func roleOf(r *gen.UserRole) gen.UserRole {
	if r == nil {
		return ""
	}
	return *r
}

// pendingRoom is the pending identity the first accepted room message creates.
func (h *harness) pendingRoom(conversation string) store.Identity {
	h.t.Helper()
	i, _, err := h.st.EnsureRoom(context.Background(), "telegram", conversation, time.Now())
	if err != nil {
		h.t.Fatalf("ensure room: %v", err)
	}
	return i
}

// room makes a confirmed, approved room.
func (h *harness) room(conversation string) store.Identity {
	h.t.Helper()
	i, err := h.auth.Confirm(context.Background(), h.pendingRoom(conversation).ID, "Ops", "", h.owner.ID)
	if err != nil {
		h.t.Fatalf("confirm room: %v", err)
	}
	return i
}

func TestIdentityKindAndStatusOnTheWire(t *testing.T) {
	h := newHarness(t)
	g := h.guest("111")
	pend := h.pendingRoom("-100")
	room := h.room("-200")

	u := decode[gen.User](t, h.do(http.MethodGet, "/identities/"+string(g.ID), h.adminToken, nil), http.StatusOK)
	if u.Kind != gen.UserKindUser || u.Status != gen.UserStatusPending || roleOf(u.Role) != gen.UserRoleGuest || u.ApprovedAt != nil || u.ApprovedBy != nil {
		t.Fatalf("pending user = %+v", u)
	}
	p := decode[gen.User](t, h.do(http.MethodGet, "/identities/"+string(pend.ID), h.adminToken, nil), http.StatusOK)
	if p.Kind != gen.UserKindRoom || p.Status != gen.UserStatusPending || p.Role != nil || p.ApprovedAt != nil || p.ApprovedBy != nil {
		t.Fatalf("pending room = %+v", p)
	}

	resp := h.do(http.MethodGet, "/identities/"+string(room.ID), h.adminToken, nil)
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if _, has := raw["role"]; has {
		t.Fatalf("a confirmed room has a role key: %v", raw)
	}
	if raw["kind"] != "room" || raw["status"] != "confirmed" || raw["approved_at"] == nil || raw["approved_by"] != string(h.owner.ID) {
		t.Fatalf("room = %v", raw)
	}
}

// A disabled user keeps its role on the wire; a disabled room has none.
func TestDisabledIdentityRoleOnTheWire(t *testing.T) {
	h := newHarness(t)
	user, err := h.auth.Confirm(context.Background(), h.guest("111").ID, "Bob", "user", h.owner.ID)
	if err != nil {
		t.Fatalf("confirm user: %v", err)
	}
	room := h.room("-200")
	for _, id := range []string{string(user.ID), string(room.ID)} {
		if _, err := h.st.DB().Exec(`UPDATE identities SET status = 'disabled' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}

	get := func(id string) map[string]any {
		resp := h.do(http.MethodGet, "/identities/"+id, h.adminToken, nil)
		var raw map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return raw
	}
	if raw := get(string(user.ID)); raw["status"] != "disabled" || raw["role"] != "user" {
		t.Fatalf("disabled user = %v, want status disabled and its role", raw)
	}
	raw := get(string(room.ID))
	if _, has := raw["role"]; has || raw["status"] != "disabled" {
		t.Fatalf("disabled room = %v, want status disabled and no role key", raw)
	}
}

func TestConfirmPendingRoom(t *testing.T) {
	h := newHarness(t)
	pend := h.pendingRoom("-100")

	errorBody(t, h.patchIdentity(string(pend.ID), gen.IdentityUpdate{Name: ptr("Ops"), Role: ptr(gen.IdentityUpdateRoleUser)}),
		http.StatusBadRequest, CodeBadRequest)
	errorBody(t, h.patchIdentity(string(pend.ID), gen.IdentityUpdate{}), http.StatusBadRequest, CodeBadRequest)

	u := decode[gen.User](t, h.patchIdentity(string(pend.ID), gen.IdentityUpdate{Name: ptr("Ops")}), http.StatusOK)
	if u.Name != "Ops" || u.Status != gen.UserStatusConfirmed || u.Role != nil || u.ApprovedAt == nil ||
		u.ApprovedBy == nil || *u.ApprovedBy != string(h.admin.ID) {
		t.Fatalf("confirmed room = %+v, want approved by the admin", u)
	}
}

// A token tied to nobody approves without an approver; one tied to a user
// names that user.
func TestConfirmRoomRecordsTheApprover(t *testing.T) {
	h := newHarness(t)
	pend := h.pendingRoom("-100")
	resp := h.do(http.MethodPatch, "/identities/"+string(pend.ID), h.ownerAdmin, gen.IdentityUpdate{Name: ptr("Ops")})
	u := decode[gen.User](t, resp, http.StatusOK)
	if u.ApprovedBy == nil || *u.ApprovedBy != string(h.owner.ID) {
		t.Fatalf("approved_by = %v, want %q", u.ApprovedBy, h.owner.ID)
	}
}

func TestRoomIdentityRefusals(t *testing.T) {
	h := newHarness(t)
	room := h.room("-200")
	pend := h.pendingRoom("-100")
	g := h.guest("111")
	id := string(room.ID)

	cases := map[string]*http.Response{
		"assign onto a room": h.do(http.MethodPost, "/identities/"+string(g.ID)+"/assign", h.adminToken, gen.IdentityAssign{To: id}),
		"assign a room":      h.do(http.MethodPost, "/identities/"+id+"/assign", h.adminToken, gen.IdentityAssign{To: string(h.other.ID)}),
		"unbind":             h.unbind("telegram:-200", h.adminToken),
		"unbind a pending":   h.unbind("telegram:-100", h.adminToken),
		"invite":             h.do(http.MethodPost, "/invites", h.adminToken, gen.InviteCreate{Channel: "holaclaw", UserId: &id}),
		"token":              h.do(http.MethodPost, "/tokens", h.adminToken, gen.TokenCreate{Name: "x", Scope: ptr(gen.TokenCreateScopeClient), UserId: ptr(id)}),
	}
	for name, resp := range cases {
		t.Run(name, func(t *testing.T) {
			errorBody(t, resp, http.StatusBadRequest, CodeBadRequest)
		})
	}
	if got, err := h.st.GetIdentity(context.Background(), pend.ID); err != nil || !got.Pending() {
		t.Fatalf("pending room after the unbind = %+v, %v", got, err)
	}
}

// A room is disabled pending or confirmed and comes back pending, with no role;
// a pending user is refused.
func TestDisableRoomOnTheWire(t *testing.T) {
	h := newHarness(t)
	for name, id := range map[string]event.ID{"pending": h.pendingRoom("-100").ID, "confirmed": h.room("-200").ID} {
		t.Run(name, func(t *testing.T) {
			u := decode[gen.User](t, h.switchIdentity(id, "disable", h.adminToken), http.StatusOK)
			if u.Status != gen.UserStatusDisabled || u.Role != nil {
				t.Fatalf("disabled = %+v", u)
			}
			u = decode[gen.User](t, h.switchIdentity(id, "enable", h.adminToken), http.StatusOK)
			if u.Status != gen.UserStatusPending || u.Role != nil {
				t.Fatalf("enabled = %+v", u)
			}
		})
	}
	errorBody(t, h.switchIdentity(h.guest("111").ID, "disable", h.adminToken), http.StatusConflict, CodeConflict)
}

func TestCreateInviteRefusesTheOldGroupKind(t *testing.T) {
	h := newHarness(t)
	configChannel(h, "tg-main", "telegram", "approved")
	body := map[string]any{"kind": "group", "channel": "tg-main"}
	errorBody(t, h.do(http.MethodPost, "/invites", h.adminToken, body), http.StatusBadRequest, CodeBadRequest)
}
