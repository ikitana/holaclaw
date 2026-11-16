package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// listIDs lists the session ids a token sees.
func (h *harness) listIDs(token string) []string {
	h.t.Helper()
	list := decode[struct {
		Sessions []gen.Session `json:"sessions"`
	}](h.t, h.do(http.MethodGet, "/sessions", token, nil), http.StatusOK)
	ids := make([]string, 0, len(list.Sessions))
	for _, s := range list.Sessions {
		ids = append(ids, s.Id)
	}
	return ids
}

// roomSession creates a room session whose first sender was user.
func (h *harness) roomSession(t *testing.T, user event.ID) store.Session {
	t.Helper()
	conv := string(event.NewID())
	s, _, err := h.st.EnsureSession(context.Background(), store.Session{
		Key: "main/tg/room/" + conv, Agent: "main", Kind: store.SessionChat, UserID: user,
		Origin: event.Origin{
			Channel: "tg", ChannelType: "telegram", Conversation: conv,
			ConversationKind: event.ConversationRoom,
		},
	})
	if err != nil {
		t.Fatalf("ensure room session: %v", err)
	}
	return s
}

// An admin token sees its own identity's sessions, and another identity's do
// not exist for it.
func TestAdminTokenSeesOnlyItsIdentity(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	owners := h.newSession(h.ownerToken, gen.SessionCreate{})
	admins := h.newSession(h.adminToken, gen.SessionCreate{})

	if got := h.listIDs(h.adminToken); !slices.Equal(got, []string{admins.Id}) {
		t.Fatalf("admin sees %v, want only %s", got, admins.Id)
	}
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/sessions/" + owners.Id},
		{http.MethodGet, "/sessions/" + owners.Id + "/messages"},
		{http.MethodPost, "/sessions/" + owners.Id + "/abort"},
		{http.MethodDelete, "/sessions/" + owners.Id},
	} {
		errorBody(t, h.do(c.method, c.path, h.adminToken, nil), http.StatusNotFound, CodeNotFound)
	}
	errorBody(t, h.do(http.MethodPost, "/sessions/"+owners.Id+"/messages", h.adminToken,
		gen.MessageSend{Id: string(event.NewID()), Text: ptr("peek")}), http.StatusNotFound, CodeNotFound)

	// The owner's admin token is the owner's identity like any other.
	if got := h.listIDs(h.ownerAdmin); !slices.Equal(got, []string{owners.Id}) {
		t.Fatalf("owner's admin token sees %v, want only %s", got, owners.Id)
	}
}

// A holaclaw conversation belongs to the identity: a second client of it
// lists, reads and continues what the first started.
func TestClientsOfOneIdentityShareHolaclawConversations(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	phone := h.ownerToken
	laptop := h.token("laptop", auth.ScopeClient, h.owner.ID)

	sess := h.newSession(phone, gen.SessionCreate{Conversation: ptr("shared")})
	if sess.ReadOnly {
		t.Fatalf("a holaclaw conversation is read-only: %+v", sess)
	}
	decode[gen.MessageAccepted](t, h.do(http.MethodPost, "/sessions/"+sess.Id+"/messages", phone,
		gen.MessageSend{Id: string(event.NewID()), Text: ptr("from the phone")}), http.StatusAccepted)

	if got := h.listIDs(laptop); !slices.Equal(got, []string{sess.Id}) {
		t.Fatalf("laptop sees %v, want %s", got, sess.Id)
	}
	got := decode[gen.Session](t, h.do(http.MethodGet, "/sessions/"+sess.Id, laptop, nil), http.StatusOK)
	if got.ReadOnly {
		t.Fatalf("read_only for the identity's other client: %+v", got)
	}
	acc := decode[gen.MessageAccepted](t, h.do(http.MethodPost, "/sessions/"+sess.Id+"/messages", laptop,
		gen.MessageSend{Id: string(event.NewID()), Text: ptr("from the laptop")}), http.StatusAccepted)
	if acc.SessionId == nil || *acc.SessionId != sess.Id {
		t.Fatalf("laptop's message went to %v, want %s", acc.SessionId, sess.Id)
	}
	// Opening the same conversation from the laptop is the same session.
	again := decode[gen.Session](t, h.do(http.MethodPost, "/sessions", laptop,
		gen.SessionCreate{Conversation: ptr("shared")}), http.StatusOK)
	if again.Id != sess.Id {
		t.Fatalf("laptop opened %s, want %s", again.Id, sess.Id)
	}
}

// A DM through another of the identity's bindings is listed and readable, but
// every write is refused with the channel to continue in.
func TestDMThroughAnotherBindingIsReadOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	dm := h.session(t, "tg", h.owner.ID)

	list := decode[struct {
		Sessions []gen.Session `json:"sessions"`
	}](t, h.do(http.MethodGet, "/sessions", h.ownerToken, nil), http.StatusOK)
	if len(list.Sessions) != 1 || list.Sessions[0].Id != string(dm.ID) || !list.Sessions[0].ReadOnly {
		t.Fatalf("sessions = %+v, want the tg DM, read-only", list.Sessions)
	}
	decode[struct {
		Messages []gen.Message `json:"messages"`
	}](t, h.do(http.MethodGet, "/sessions/"+string(dm.ID)+"/messages", h.ownerToken, nil), http.StatusOK)

	refused := func(resp *http.Response) {
		t.Helper()
		e := decode[gen.Error](t, resp, http.StatusConflict)
		if e.Code != CodeConflict || !strings.Contains(e.Message, "continue it in tg") {
			t.Fatalf("error = %+v, want a conflict naming tg", e)
		}
	}
	refused(h.do(http.MethodPost, "/sessions/"+string(dm.ID)+"/messages", h.ownerToken,
		gen.MessageSend{Id: string(event.NewID()), Text: ptr("hola")}))
	refused(h.do(http.MethodPost, "/sessions/"+string(dm.ID)+"/abort", h.ownerToken, nil))
	refused(h.do(http.MethodDelete, "/sessions/"+string(dm.ID), h.ownerToken, nil))

	msgs, err := h.st.ListMessages(context.Background(), dm.ID, "", 10)
	if err != nil || len(msgs) != 0 {
		t.Fatalf("messages = %+v, %v; want none stored", msgs, err)
	}
	if _, err := h.st.GetSession(context.Background(), dm.ID); err != nil {
		t.Fatalf("the DM is gone after a refused delete: %v", err)
	}
	// The socket still writes to it.
	if rec := h.socket(http.MethodDelete, "/sessions/"+string(dm.ID), nil); rec.Code != http.StatusNoContent {
		t.Fatalf("socket delete: status %d, body %s", rec.Code, rec.Body)
	}
}

// A task session is listed to its creator, read-only, but the creator may
// still abort its run. Another identity's token can't reach it at all.
func TestTaskSessionIsReadOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess, _, err := h.st.EnsureSession(context.Background(), store.Session{
		Key: "main/task/x", Agent: "main", Kind: store.SessionTask, UserID: h.owner.ID,
		Origin: event.Origin{Channel: "task", ChannelType: "task", Conversation: "x", ConversationKind: event.ConversationDM},
	})
	if err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	got := decode[gen.Session](t, h.do(http.MethodGet, "/sessions/"+string(sess.ID), h.ownerToken, nil), http.StatusOK)
	if !got.ReadOnly {
		t.Fatalf("task session is writable: %+v", got)
	}
	errorBody(t, h.do(http.MethodDelete, "/sessions/"+string(sess.ID), h.ownerToken, nil), http.StatusConflict, CodeConflict)
	resp := h.do(http.MethodPost, "/sessions/"+string(sess.ID)+"/abort", h.ownerToken, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("creator abort: status %d, want 202", resp.StatusCode)
	}
	_ = resp.Body.Close()
	for _, token := range []string{h.otherToken, h.adminToken} {
		errorBody(t, h.do(http.MethodPost, "/sessions/"+string(sess.ID)+"/abort", token, nil), http.StatusNotFound, CodeNotFound)
	}
}

// A room session belongs to the room, not to whoever spoke first: no token
// lists or opens it, nor follows its events.
func TestRoomSessionsAreNotListed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	room := h.roomSession(t, h.owner.ID)

	for _, token := range []string{h.ownerToken, h.ownerAdmin} {
		if got := h.listIDs(token); len(got) != 0 {
			t.Fatalf("token sees %v, want no room", got)
		}
		errorBody(t, h.do(http.MethodGet, "/sessions/"+string(room.ID), token, nil), http.StatusNotFound, CodeNotFound)
		errorBody(t, h.do(http.MethodGet, "/sessions/"+string(room.ID)+"/messages", token, nil), http.StatusNotFound, CodeNotFound)
	}

	s := h.stream(h.ownerAdmin, "", "")
	h.emit(room.ID, event.RunStarted)
	s.quiet(t, 300*time.Millisecond)
}

// The socket, the owner on the host, sees every session: other identities',
// rooms and DMs alike.
func TestSocketSeesEverySession(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	want := []string{
		h.newSession(h.ownerToken, gen.SessionCreate{}).Id,
		h.newSession(h.otherToken, gen.SessionCreate{}).Id,
		h.newSession(h.adminToken, gen.SessionCreate{}).Id,
		string(h.roomSession(t, h.other.ID).ID),
		string(h.session(t, "tg", h.other.ID).ID),
	}
	rec := h.socket(http.MethodGet, "/sessions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var list struct {
		Sessions []gen.Session `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := make([]string, 0, len(list.Sessions))
	for _, s := range list.Sessions {
		got = append(got, s.Id)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("socket sees %v, want %v", got, want)
	}
	for _, id := range want {
		if rec := h.socket(http.MethodGet, "/sessions/"+id, nil); rec.Code != http.StatusOK {
			t.Fatalf("socket get %s: status %d", id, rec.Code)
		}
	}
}

// The event stream follows the same rule: an admin token gets its identity's
// sessions' events, not another identity's, and still the events of no
// session.
func TestStreamFollowsSessionVisibility(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	theirs := h.session(t, event.ChannelHolaclaw, h.owner.ID)
	mine := h.session(t, event.ChannelHolaclaw, h.admin.ID)

	s := h.stream(h.adminToken, "", "")
	h.emit(theirs.ID, event.RunStarted)
	s.quiet(t, 300*time.Millisecond)
	want := h.emit(mine.ID, event.RunStarted)
	if f := s.next(t, 2*time.Second); f.ID != string(want) {
		t.Fatalf("frame = %+v, want %s", f, want)
	}
	none := h.emit("", event.RunStarted)
	if f := s.next(t, 2*time.Second); f.ID != string(none) {
		t.Fatalf("frame = %+v, want the session-less %s", f, none)
	}
}
