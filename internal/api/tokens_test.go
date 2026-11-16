package api

import (
	"context"
	"net/http"
	"testing"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/event"
)

func TestCreateTokenOnBinding(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	other := string(h.other.ID)

	// The scope defaults to client, and the origin and creator are recorded.
	first := decode[gen.TokenCreated](t, h.do(http.MethodPost, "/tokens", h.adminToken,
		gen.TokenCreate{Name: "ci", UserId: &other}), http.StatusCreated)
	if first.Token.Scope != gen.TokenScopeClient || first.Token.Origin != gen.TokenOriginTokenCreate ||
		first.Token.CreatedBy == nil || *first.Token.CreatedBy != string(h.admin.ID) {
		t.Fatalf("token = %+v", first.Token)
	}
	// A second token joins the same binding.
	second := decode[gen.TokenCreated](t, h.do(http.MethodPost, "/tokens", h.adminToken,
		gen.TokenCreate{Name: "backup", Binding: &first.Token.Binding}), http.StatusCreated)
	if second.Token.Binding != first.Token.Binding || second.Secret == "" {
		t.Fatalf("token = %+v", second.Token)
	}

	socket := "holaclaw:cli"
	telegram := "telegram:1"
	missing := "holaclaw:" + string(event.NewID())
	for name, in := range map[string]gen.TokenCreate{
		"neither":         {Name: "x"},
		"both":            {Name: "x", UserId: &other, Binding: &first.Token.Binding},
		"admin for user":  {Name: "x", UserId: &other, Scope: ptr(gen.TokenCreateScopeAdmin)},
		"admin on a user": {Name: "x", Binding: &first.Token.Binding, Scope: ptr(gen.TokenCreateScopeAdmin)},
		"socket":          {Name: "x", Binding: &socket},
		"other channel":   {Name: "x", Binding: &telegram},
		"missing binding": {Name: "x", Binding: &missing},
	} {
		t.Run(name, func(t *testing.T) {
			errorBody(t, h.do(http.MethodPost, "/tokens", h.ownerAdmin, in), http.StatusBadRequest, CodeBadRequest)
		})
	}
}

func TestUpdateToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	other := string(h.other.ID)
	tc := decode[gen.TokenCreated](t, h.do(http.MethodPost, "/tokens", h.adminToken,
		gen.TokenCreate{Name: "ci", UserId: &other}), http.StatusCreated)
	path := "/tokens/" + tc.Token.Id

	// admin is refused while the role is user.
	errorBody(t, h.do(http.MethodPatch, path, h.adminToken, gen.TokenUpdate{Scope: gen.TokenUpdateScopeAdmin}),
		http.StatusBadRequest, CodeBadRequest)
	errorBody(t, h.do(http.MethodPatch, path, h.adminToken, gen.TokenUpdate{Scope: "root"}),
		http.StatusBadRequest, CodeBadRequest)
	errorBody(t, h.do(http.MethodPatch, "/tokens/"+string(event.NewID()), h.adminToken,
		gen.TokenUpdate{Scope: gen.TokenUpdateScopeClient}), http.StatusNotFound, CodeNotFound)
	// The owner's tokens are the owner's to change.
	ownerTok := decode[gen.TokenCreated](t, h.do(http.MethodPost, "/tokens", h.ownerAdmin,
		gen.TokenCreate{Name: "laptop", UserId: ptr(string(h.owner.ID))}), http.StatusCreated)
	errorBody(t, h.do(http.MethodPatch, "/tokens/"+ownerTok.Token.Id, h.adminToken,
		gen.TokenUpdate{Scope: gen.TokenUpdateScopeAdmin}), http.StatusForbidden, CodeForbidden)

	// After a promotion it goes through, and the token acts as admin.
	resp := h.do(http.MethodPatch, "/identities/"+other, h.adminToken, gen.IdentityUpdate{Role: ptr(gen.IdentityUpdateRoleAdmin)})
	_ = resp.Body.Close()
	if code := h.status(t, probe{method: http.MethodGet, path: "/tokens"}, tc.Secret); code != http.StatusForbidden {
		t.Fatalf("promoted, not raised: status %d, want 403", code)
	}
	got := decode[gen.Token](t, h.do(http.MethodPatch, path, h.adminToken, gen.TokenUpdate{Scope: gen.TokenUpdateScopeAdmin}),
		http.StatusOK)
	if got.Scope != gen.TokenScopeAdmin {
		t.Fatalf("token = %+v", got)
	}
	if code := h.status(t, probe{method: http.MethodGet, path: "/tokens"}, tc.Secret); code != http.StatusOK {
		t.Fatalf("raised: status %d, want 200", code)
	}

	// A revoked token stays revoked.
	resp = h.do(http.MethodDelete, path, h.adminToken, nil)
	_ = resp.Body.Close()
	errorBody(t, h.do(http.MethodPatch, path, h.adminToken, gen.TokenUpdate{Scope: gen.TokenUpdateScopeClient}),
		http.StatusConflict, CodeConflict)
}

func TestRotateTokenAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	other := string(h.other.ID)
	tc := decode[gen.TokenCreated](t, h.do(http.MethodPost, "/tokens", h.adminToken,
		gen.TokenCreate{Name: "ci", UserId: &other}), http.StatusCreated)

	rot := decode[gen.TokenCreated](t, h.do(http.MethodPost, "/tokens/"+tc.Token.Id+"/rotate", h.adminToken, nil),
		http.StatusCreated)
	if rot.Token.Binding != tc.Token.Binding || rot.Token.Origin != gen.TokenOriginRotate ||
		rot.Token.OriginRef == nil || *rot.Token.OriginRef != tc.Token.Id {
		t.Fatalf("rotated = %+v", rot.Token)
	}
	errorBody(t, h.do(http.MethodGet, "/status", tc.Secret, nil), http.StatusUnauthorized, CodeUnauthorized)
	if code := h.status(t, probe{method: http.MethodGet, path: "/status"}, rot.Secret); code != http.StatusOK {
		t.Fatalf("new secret: status %d", code)
	}
	errorBody(t, h.do(http.MethodPost, "/tokens/"+tc.Token.Id+"/rotate", h.adminToken, nil), http.StatusConflict, CodeConflict)
	// Revoking the replaced token says the replacement still works.
	errorBody(t, h.do(http.MethodDelete, "/tokens/"+tc.Token.Id, h.adminToken, nil), http.StatusConflict, CodeConflict)
	errorBody(t, h.do(http.MethodPost, "/tokens/"+string(event.NewID())+"/rotate", h.adminToken, nil),
		http.StatusNotFound, CodeNotFound)

	// An admin can't rotate the owner's token into a secret of its own.
	ownerTok := decode[gen.TokenCreated](t, h.do(http.MethodPost, "/tokens", h.ownerAdmin,
		gen.TokenCreate{Name: "laptop", UserId: ptr(string(h.owner.ID))}), http.StatusCreated)
	errorBody(t, h.do(http.MethodPost, "/tokens/"+ownerTok.Token.Id+"/rotate", h.adminToken, nil),
		http.StatusForbidden, CodeForbidden)
	errorBody(t, h.do(http.MethodDelete, "/tokens/"+ownerTok.Token.Id, h.adminToken, nil),
		http.StatusForbidden, CodeForbidden)

	// Revoked tokens are listed only with all, with who revoked them.
	list := decode[struct{ Tokens []gen.Token }](t,
		h.do(http.MethodGet, "/tokens?binding="+tc.Token.Binding, h.adminToken, nil), http.StatusOK)
	if len(list.Tokens) != 1 || list.Tokens[0].Id != rot.Token.Id {
		t.Fatalf("active = %+v", list.Tokens)
	}
	list = decode[struct{ Tokens []gen.Token }](t,
		h.do(http.MethodGet, "/tokens?all=true&identity="+other+"&binding="+tc.Token.Binding, h.adminToken, nil), http.StatusOK)
	if len(list.Tokens) != 2 || list.Tokens[0].Id != tc.Token.Id || list.Tokens[0].RevokedBy == nil ||
		*list.Tokens[0].RevokedBy != string(h.admin.ID) {
		t.Fatalf("all = %+v", list.Tokens)
	}
	errorBody(t, h.do(http.MethodGet, "/tokens?binding=telegram:1", h.adminToken, nil), http.StatusBadRequest, CodeBadRequest)
}

// A client rotates the token it signs in with, and only that one.
func TestRotateOwnTokenAPI(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	secret, tok, err := h.auth.CreateToken(ctx, "ci", auth.ScopeClient, h.other.ID)
	if err != nil {
		t.Fatal(err)
	}
	sibling, _, err := h.auth.AddToken(ctx, auth.Principal{Role: "owner"}, tok.Binding, "backup", "")
	if err != nil {
		t.Fatal(err)
	}
	rot := decode[gen.TokenCreated](t, h.do(http.MethodPost, "/me/token/rotate", secret, nil), http.StatusCreated)
	if rot.Token.OriginRef == nil || *rot.Token.OriginRef != string(tok.ID) || rot.Token.Name != "ci" {
		t.Fatalf("rotated = %+v", rot.Token)
	}
	errorBody(t, h.do(http.MethodGet, "/status", secret, nil), http.StatusUnauthorized, CodeUnauthorized)
	for _, s := range []string{rot.Secret, sibling, h.otherToken} {
		if code := h.status(t, probe{method: http.MethodGet, path: "/status"}, s); code != http.StatusOK {
			t.Fatalf("another token stopped working: status %d", code)
		}
	}
	// The socket has no token of its own.
	if rec := h.socket(http.MethodPost, "/me/token/rotate", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("socket: status %d, want 400", rec.Code)
	}
}

// A message's trace carries the token that sent it.
func TestTraceCarriesToken(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	p, err := h.auth.Authenticate(context.Background(), h.ownerToken)
	if err != nil {
		t.Fatal(err)
	}
	sess := h.newSession(h.ownerToken, gen.SessionCreate{Conversation: ptr("traced")})
	acc := decode[gen.MessageAccepted](t, h.do(http.MethodPost, "/sessions/"+sess.Id+"/messages", h.ownerToken,
		gen.MessageSend{Id: string(event.NewID()), Text: ptr("hi")}), http.StatusAccepted)
	trace := decode[gen.Trace](t, h.do(http.MethodGet, "/traces/"+acc.TraceId, h.ownerToken, nil), http.StatusOK)
	for _, e := range trace.Events {
		if e.Type != string(event.ChannelReceived) {
			continue
		}
		if e.Attrs == nil || (*e.Attrs)["token"] != string(p.TokenID) {
			t.Fatalf("channel.received attrs = %v, want token %s", e.Attrs, p.TokenID)
		}
		return
	}
	t.Fatalf("no channel.received in %+v", trace.Events)
}
