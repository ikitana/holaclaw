package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/event"
	"holaclaw/internal/memory"
	"holaclaw/internal/store"
)

// --- H2: every token belongs to a binding; nothing defaults to the owner -----

func TestCreateTokenBindsToAUser(t *testing.T) {
	h := newHarness(t)

	// A token acts with its user's role: only the owner mints one for the
	// owner, in any scope.
	owner := string(h.owner.ID)
	for _, in := range []gen.TokenCreate{
		{Name: "cli", Scope: ptr(gen.TokenCreateScope("client")), UserId: ptr(owner)},
		{Name: "adm", Scope: ptr(gen.TokenCreateScope("admin")), UserId: ptr(owner)},
	} {
		errorBody(t, h.do(http.MethodPost, "/tokens", h.adminToken, in), http.StatusForbidden, CodeForbidden)
	}

	// No token is bound to nobody: a token names its user.
	for _, in := range []gen.TokenCreate{{Name: "cli", Scope: ptr(gen.TokenCreateScope("client"))}, {Name: "ops", Scope: ptr(gen.TokenCreateScope("admin"))}} {
		errorBody(t, h.do(http.MethodPost, "/tokens", h.ownerAdmin, in), http.StatusBadRequest, CodeBadRequest)
	}
	// A scope is admin or client.
	errorBody(t, h.do(http.MethodPost, "/tokens", h.ownerAdmin,
		gen.TokenCreate{Name: "x", Scope: ptr(gen.TokenCreateScope("channel")), UserId: ptr(owner)}), http.StatusBadRequest, CodeBadRequest)

	// An explicit user must exist.
	errorBody(t, h.do(http.MethodPost, "/tokens", h.adminToken,
		gen.TokenCreate{Name: "ghost", Scope: ptr(gen.TokenCreateScope("client")), UserId: ptr(string(event.NewID()))}),
		http.StatusBadRequest, CodeBadRequest)

	// A named user gets a new holaclaw binding, named after the token.
	bound := decode[gen.TokenCreated](t,
		h.do(http.MethodPost, "/tokens", h.adminToken,
			gen.TokenCreate{Name: "nadia's phone", Scope: ptr(gen.TokenCreateScope("client")), UserId: ptr(string(h.other.ID))}),
		http.StatusCreated)
	if bound.Token.UserId == nil || *bound.Token.UserId != string(h.other.ID) ||
		!strings.HasPrefix(bound.Token.Binding, "holaclaw:") {
		t.Fatalf("token = %+v, want a holaclaw binding of %s", bound.Token, h.other.ID)
	}
	u := decode[gen.User](t, h.do(http.MethodGet, "/identities/"+string(h.other.ID), h.adminToken, nil), http.StatusOK)
	found := false
	for _, b := range *u.Identities {
		if "holaclaw:"+b.Alias == bound.Token.Binding {
			found = b.Display != nil && *b.Display == "nadia's phone" && b.Tokens != nil && *b.Tokens == 1
		}
	}
	if !found {
		t.Fatalf("bindings = %+v, want %s named after the token with 1 active token", *u.Identities, bound.Token.Binding)
	}
	// And the secret it handed out really is isolated to that user.
	mine := h.newSession(h.ownerToken, gen.SessionCreate{Conversation: ptr("not-yours")})
	errorBody(t, h.do(http.MethodGet, "/sessions/"+mine.Id, bound.Secret, nil), http.StatusNotFound, CodeNotFound)
}

// identity show lists the holaclaw bindings with their active tokens:
// holaclaw:cli for the socket has none.
func TestIdentityShowsHolaclawBindings(t *testing.T) {
	h := newHarness(t)
	u := decode[gen.User](t, h.do(http.MethodGet, "/identities/"+string(h.owner.ID), h.ownerAdmin, nil), http.StatusOK)
	tokens := map[string]int{}
	for _, b := range *u.Identities {
		if b.Channel != "holaclaw" || b.Tokens == nil {
			t.Fatalf("binding %+v: want a holaclaw binding with a token count", b)
		}
		tokens[b.Alias] = *b.Tokens
	}
	// cli, the init binding from CreateOwner, and the harness's two.
	if len(tokens) != 4 || tokens["cli"] != 0 {
		t.Fatalf("owner holaclaw bindings = %v", tokens)
	}
}

// Unbinding a holaclaw binding revokes every token on it: the next request is
// refused, and the binding is gone from identity show.
func TestUnbindHolaclawRevokesTokens(t *testing.T) {
	h := newHarness(t)
	created := decode[gen.TokenCreated](t, h.do(http.MethodPost, "/tokens", h.adminToken,
		gen.TokenCreate{Name: "ci", Scope: ptr(gen.TokenCreateScope("client")), UserId: ptr(string(h.other.ID))}), http.StatusCreated)
	if rec := h.do(http.MethodGet, "/sessions", created.Secret, nil); rec.StatusCode != http.StatusOK {
		t.Fatalf("before unbind: status %d", rec.StatusCode)
	}
	ref := created.Token.Binding
	out := decode[gen.BindingUnbound](t, h.do(http.MethodDelete, "/bindings/"+ref, h.adminToken, nil), http.StatusOK)
	if out.TokensRevoked != 1 || out.Identity != string(h.other.ID) || out.PendingDeleted {
		t.Fatalf("unbind = %+v", out)
	}
	errorBody(t, h.do(http.MethodGet, "/sessions", created.Secret, nil), http.StatusUnauthorized, CodeUnauthorized)

	// The token is kept, revoked, and no longer names an identity.
	list := decode[struct {
		Tokens []gen.Token `json:"tokens"`
	}](t, h.do(http.MethodGet, "/tokens", h.adminToken, nil), http.StatusOK)
	for _, tok := range list.Tokens {
		if tok.Id == created.Token.Id && (tok.RevokedAt == nil || tok.UserId != nil || tok.Binding != ref) {
			t.Fatalf("token after unbind = %+v", tok)
		}
	}

	// holaclaw:cli is the socket's way in, and stays.
	errorBody(t, h.do(http.MethodDelete, "/bindings/holaclaw:cli", h.ownerAdmin, nil), http.StatusBadRequest, CodeBadRequest)
	if rec := h.socket(http.MethodGet, "/identities", nil); rec.Code != http.StatusOK {
		t.Fatalf("socket after the refused unbind: status %d", rec.Code)
	}
}

// A message sent through a token arrives from its binding and resolves to the
// binding's identity: the turn runs as that identity.
func TestMessageArrivesFromTheBinding(t *testing.T) {
	h := newHarness(t)
	sess := h.newSession(h.otherToken, gen.SessionCreate{Conversation: ptr("from-binding")})
	acc := decode[gen.MessageAccepted](t, h.do(http.MethodPost, "/sessions/"+sess.Id+"/messages", h.otherToken,
		gen.MessageSend{Id: string(event.NewID()), Text: ptr("hi")}), http.StatusAccepted)
	if acc.TraceId == "" {
		t.Fatalf("accepted = %+v", acc)
	}
	// The session actor stores the message, so it lands a moment later.
	deadline := time.Now().Add(5 * time.Second)
	for {
		msgs, err := h.st.ListMessages(context.Background(), event.ID(sess.Id), "", 10)
		if err == nil && len(msgs) > 0 {
			if msgs[0].UserID != h.other.ID {
				t.Fatalf("first message from %q, want %s", msgs[0].UserID, h.other.ID)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no message stored: err %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- M3: blobs and traces are authorized per user -----------------------------

// putBlob uploads bytes with one token and returns the blob id.
func (h *harness) putBlob(token, body string) string {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+BaseURL+"/blobs", strings.NewReader(body))
	if err != nil {
		h.t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("put blob: %v", err)
	}
	return decode[gen.Blob](h.t, resp, http.StatusCreated).Id
}

func TestBlobIsReadableOnlyByItsOwnUser(t *testing.T) {
	h := newHarness(t)
	id := h.putBlob(h.ownerToken, "the quarterly numbers")

	// The uploader reads back its own fresh upload; nobody else does.
	if resp := h.do(http.MethodGet, "/blobs/"+id, h.ownerToken, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("uploader read its own upload: status %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
	errorBody(t, h.do(http.MethodGet, "/blobs/"+id, h.otherToken, nil), http.StatusNotFound, CodeNotFound)

	// Attaching it to the owner's session does not open it to the other user.
	sess := h.newSession(h.ownerToken, gen.SessionCreate{Conversation: ptr("m3-blob")})
	decode[gen.MessageAccepted](t, h.do(http.MethodPost, "/sessions/"+sess.Id+"/messages", h.ownerToken,
		gen.MessageSend{
			Id: string(event.NewID()), Text: ptr("see attached"),
			Attachments: &[]gen.AttachmentRef{{BlobId: id, Name: ptr("q.txt")}},
		}), http.StatusAccepted)

	errorBody(t, h.do(http.MethodGet, "/blobs/"+id, h.otherToken, nil), http.StatusNotFound, CodeNotFound)

	// A principal with no fresh-upload memory of it still reads it once a
	// message of one of its own sessions carries it: the owner through a
	// second token, admin scope included. Another identity's admin token
	// does not; the socket reads everything.
	second := h.token("cli-owner-2", auth.ScopeClient, h.owner.ID)
	for name, token := range map[string]string{"owner's other token": second, "owner's admin token": h.ownerAdmin} {
		resp := h.do(http.MethodGet, "/blobs/"+id, token, nil)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", name, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	errorBody(t, h.do(http.MethodGet, "/blobs/"+id, h.adminToken, nil), http.StatusNotFound, CodeNotFound)
	if rec := h.socket(http.MethodGet, "/blobs/"+id, nil); rec.Code != http.StatusOK {
		t.Errorf("socket: status = %d, want 200", rec.Code)
	}

	// An unknown blob is the same answer as a forbidden one.
	errorBody(t, h.do(http.MethodGet, "/blobs/"+string(event.NewID()), h.ownerToken, nil),
		http.StatusNotFound, CodeNotFound)
}

func TestTraceIsReadableOnlyByItsOwnUser(t *testing.T) {
	h := newHarness(t)
	sess := h.newSession(h.ownerToken, gen.SessionCreate{Conversation: ptr("m3-trace")})
	acc := decode[gen.MessageAccepted](t, h.do(http.MethodPost, "/sessions/"+sess.Id+"/messages", h.ownerToken,
		gen.MessageSend{Id: string(event.NewID()), Text: ptr("how is the build")}), http.StatusAccepted)

	trace := decode[gen.Trace](t, h.do(http.MethodGet, "/traces/"+acc.TraceId, h.ownerToken, nil), http.StatusOK)
	if trace.TraceId != acc.TraceId {
		t.Fatalf("trace = %+v", trace)
	}
	errorBody(t, h.do(http.MethodGet, "/traces/"+acc.TraceId, h.otherToken, nil), http.StatusNotFound, CodeNotFound)

	// Admin scope still reads every trace, another identity's included: it is
	// how admins inspect other identities' conversations. The socket too.
	decode[gen.Trace](t, h.do(http.MethodGet, "/traces/"+acc.TraceId, h.adminToken, nil), http.StatusOK)
	if rec := h.socket(http.MethodGet, "/traces/"+acc.TraceId, nil); rec.Code != http.StatusOK {
		t.Fatalf("socket trace: status %d", rec.Code)
	}
}

// --- M7: a note is origin-scoped unless it says otherwise ---------------------

// recordingMemory captures what WriteMemory asked the backend for.
type recordingMemory struct{ last memory.Item }

func (m *recordingMemory) Write(_ context.Context, it memory.Item) (event.ID, error) {
	m.last = it
	return event.NewID(), nil
}

func (m *recordingMemory) Search(context.Context, memory.Query) ([]memory.Hit, error) {
	return nil, nil
}

func (m *recordingMemory) Get(context.Context, string, string, event.ID) (memory.Item, error) {
	return memory.Item{}, memory.ErrNotFound
}

func (m *recordingMemory) Forget(context.Context, string, string, event.ID) error { return nil }
func (m *recordingMemory) Reindex(context.Context, string) error                  { return nil }

func (m *recordingMemory) Status(context.Context, string) (memory.Status, error) {
	return memory.Status{}, nil
}

func TestWriteMemoryDefaultsToOriginVisibility(t *testing.T) {
	h := newHarness(t)
	mem := &recordingMemory{}
	h.b.Memory = mem

	body := gen.MemoryWrite{Agent: "main", Partition: "cli/one", Text: "angel prefers tabs"}
	item := decode[gen.MemoryItem](t, h.do(http.MethodPost, "/memory", h.adminToken, body), http.StatusCreated)
	if mem.last.Visibility != memory.VisOrigin {
		t.Fatalf("stored visibility = %q, want %q", mem.last.Visibility, memory.VisOrigin)
	}
	if item.Visibility != gen.MemoryItemVisibility(memory.VisOrigin) {
		t.Fatalf("returned visibility = %q, want origin", item.Visibility)
	}

	// `shared` still works, but only when it is asked for.
	body.Visibility = ptr(gen.MemoryWriteVisibility(memory.VisShared))
	decode[gen.MemoryItem](t, h.do(http.MethodPost, "/memory", h.adminToken, body), http.StatusCreated)
	if mem.last.Visibility != memory.VisShared {
		t.Fatalf("stored visibility = %q, want %q", mem.last.Visibility, memory.VisShared)
	}
}

// visible is the rule behind every one of the tests above; this pins it
// directly, including the token that isolates against nobody.
func TestVisible(t *testing.T) {
	dm := store.Session{UserID: "U1", Origin: event.Origin{ConversationKind: event.ConversationDM}}
	room := store.Session{UserID: "U1", Origin: event.Origin{ConversationKind: event.ConversationRoom}}
	for _, tc := range []struct {
		name string
		p    auth.Principal
		s    store.Session
		want bool
	}{
		{"socket", auth.Principal{Scope: auth.ScopeAdmin, UserID: "O", Binding: auth.SocketAlias}, dm, true},
		{"socket, room", auth.Principal{Scope: auth.ScopeAdmin, UserID: "O", Binding: auth.SocketAlias}, room, true},
		{"token on the socket's alias", auth.Principal{TokenID: "T", Scope: auth.ScopeAdmin, UserID: "O", Binding: auth.SocketAlias}, dm, false},
		{"own admin", auth.Principal{TokenID: "T", Scope: auth.ScopeAdmin, UserID: "U1"}, dm, true},
		{"other admin", auth.Principal{TokenID: "T", Scope: auth.ScopeAdmin, UserID: "U2"}, dm, false},
		{"admin with no identity", auth.Principal{TokenID: "T", Scope: auth.ScopeAdmin}, dm, false},
		{"own client", auth.Principal{TokenID: "T", Scope: auth.ScopeClient, UserID: "U1"}, dm, true},
		{"own client, room", auth.Principal{TokenID: "T", Scope: auth.ScopeClient, UserID: "U1"}, room, false},
		{"other client", auth.Principal{TokenID: "T", Scope: auth.ScopeClient, UserID: "U2"}, dm, false},
		{"client with no identity", auth.Principal{TokenID: "T", Scope: auth.ScopeClient}, dm, false},
	} {
		if got := visible(tc.p, tc.s); got != tc.want {
			t.Errorf("%s: visible = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Deleting a session first drops what was derived from it; if that fails the
// session must survive (contract review A4-1).
func TestDeleteSessionRunsTheCascadeFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	mk := func() gen.Session {
		return decode[gen.Session](t, h.do(http.MethodPost, "/sessions", h.ownerToken, gen.SessionCreate{}), http.StatusCreated)
	}
	var seen []event.ID
	fail := true
	h.b.BeforeDeleteSession = func(_ context.Context, id event.ID) error {
		seen = append(seen, id)
		if fail {
			return errors.New("memory is down")
		}
		return nil
	}
	s := mk()
	if resp := h.do(http.MethodDelete, "/sessions/"+s.Id, h.ownerToken, nil); resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when the cascade fails", resp.StatusCode)
	}
	if _, err := h.st.GetSession(context.Background(), event.ID(s.Id)); err != nil {
		t.Fatalf("session must survive a failed cascade: %v", err)
	}
	fail = false
	if resp := h.do(http.MethodDelete, "/sessions/"+s.Id, h.ownerToken, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(seen) != 2 || seen[1] != event.ID(s.Id) {
		t.Fatalf("hook calls = %v", seen)
	}
}
