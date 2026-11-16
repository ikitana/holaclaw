package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
)

func TestSessionFlowEndToEnd(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	sess := h.newSession(h.ownerToken, gen.SessionCreate{})
	if sess.Origin.Channel != event.ChannelHolaclaw || sess.Origin.ChannelType != event.ChannelHolaclaw {
		t.Fatalf("origin = %+v", sess.Origin)
	}
	if sess.Origin.ConversationKind != gen.OriginConversationKindDm || sess.Agent != "main" {
		t.Fatalf("session = %+v", sess)
	}
	if sess.UserId == nil || *sess.UserId != string(h.owner.ID) {
		t.Fatalf("user id = %v, want %s", sess.UserId, h.owner.ID)
	}

	// The same conversation again is the same session, and 200 rather than 201.
	again := decode[gen.Session](t, h.do(http.MethodPost, "/sessions", h.ownerToken,
		gen.SessionCreate{Conversation: ptr(sess.Origin.Conversation)}), http.StatusOK)
	if again.Id != sess.Id {
		t.Fatalf("second create made a new session: %s vs %s", again.Id, sess.Id)
	}

	stream := h.stream(h.ownerToken, sess.Id, "")

	msgID := string(event.NewID())
	acc := decode[gen.MessageAccepted](t, h.do(http.MethodPost, "/sessions/"+sess.Id+"/messages", h.ownerToken,
		gen.MessageSend{Id: msgID, Text: ptr("hola")}), http.StatusAccepted)
	if acc.Id != msgID || acc.TraceId == "" || acc.Duplicate {
		t.Fatalf("accepted = %+v", acc)
	}
	if acc.SessionId == nil || *acc.SessionId != sess.Id {
		t.Fatalf("session id = %v, want %s", acc.SessionId, sess.Id)
	}

	reply := waitFor(t, stream, string(event.Reply), 5*time.Second)
	var got gen.Event
	if err := json.Unmarshal(reply.Data, &got); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	if got.SessionId == nil || *got.SessionId != sess.Id {
		t.Fatalf("reply event = %+v", got)
	}
	if got.TraceId != acc.TraceId {
		t.Fatalf("reply trace = %s, want %s", got.TraceId, acc.TraceId)
	}

	// The transcript holds the question and the answer.
	hist := decode[struct {
		Messages []gen.Message `json:"messages"`
	}](t, h.do(http.MethodGet, "/sessions/"+sess.Id+"/messages", h.ownerToken, nil), http.StatusOK)
	if len(hist.Messages) != 2 {
		t.Fatalf("history = %d messages, want 2", len(hist.Messages))
	}
	if hist.Messages[0].Id != *acc.MessageId || hist.Messages[0].Role != gen.MessageRoleUser {
		t.Fatalf("first message = %+v", hist.Messages[0])
	}
	if hist.Messages[1].Role != gen.MessageRoleAssistant || *hist.Messages[1].Text != "echo: hola" {
		t.Fatalf("second message = %+v", hist.Messages[1])
	}

	// The trace joins events and messages.
	trace := decode[gen.Trace](t, h.do(http.MethodGet, "/traces/"+acc.TraceId, h.ownerToken, nil), http.StatusOK)
	if len(trace.Events) == 0 || len(trace.Messages) == 0 {
		t.Fatalf("trace = %d events, %d messages", len(trace.Events), len(trace.Messages))
	}
	errorBody(t, h.do(http.MethodGet, "/traces/"+string(event.NewID()), h.ownerToken, nil),
		http.StatusNotFound, CodeNotFound)

	// Aborting an idle session is accepted, and listing shows the session.
	if resp := h.do(http.MethodPost, "/sessions/"+sess.Id+"/abort", h.ownerToken, nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("abort status = %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
	list := decode[struct {
		Sessions []gen.Session `json:"sessions"`
	}](t, h.do(http.MethodGet, "/sessions", h.ownerToken, nil), http.StatusOK)
	if len(list.Sessions) != 1 || list.Sessions[0].Id != sess.Id {
		t.Fatalf("list = %+v", list.Sessions)
	}

	if resp := h.do(http.MethodDelete, "/sessions/"+sess.Id, h.ownerToken, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
	errorBody(t, h.do(http.MethodGet, "/sessions/"+sess.Id, h.ownerToken, nil), http.StatusNotFound, CodeNotFound)
}

// waitFor reads frames until one of the wanted type arrives.
func waitFor(t *testing.T, s *rawStream, typ string, timeout time.Duration) frame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		f, err := s.read(time.Until(deadline))
		if err != nil {
			break
		}
		if f.Type == typ {
			return f
		}
	}
	t.Fatalf("no %s event within %s", typ, timeout)
	return frame{}
}

func TestSendMessageIdempotentResend(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.newSession(h.ownerToken, gen.SessionCreate{})
	msg := gen.MessageSend{Id: string(event.NewID()), Text: ptr("once")}

	first := decode[gen.MessageAccepted](t, h.do(http.MethodPost, "/sessions/"+sess.Id+"/messages", h.ownerToken, msg), http.StatusAccepted)
	if first.Duplicate {
		t.Fatal("the first send was reported as a duplicate")
	}
	second := decode[gen.MessageAccepted](t, h.do(http.MethodPost, "/sessions/"+sess.Id+"/messages", h.ownerToken, msg), http.StatusAccepted)
	if !second.Duplicate {
		t.Fatal("the resend was not reported as a duplicate")
	}
}

func TestSendMessageRejectsInvalidID(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.newSession(h.ownerToken, gen.SessionCreate{})
	for _, id := range []string{"", "not-a-ulid", "0123"} {
		errorBody(t, h.do(http.MethodPost, "/sessions/"+sess.Id+"/messages", h.ownerToken,
			gen.MessageSend{Id: id, Text: ptr("hi")}), http.StatusBadRequest, CodeBadRequest)
	}
	// An empty message is refused too: a turn needs something to answer.
	errorBody(t, h.do(http.MethodPost, "/sessions/"+sess.Id+"/messages", h.ownerToken,
		gen.MessageSend{Id: string(event.NewID())}), http.StatusBadRequest, CodeBadRequest)
}

func TestSendMessageMultipart(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.newSession(h.ownerToken, gen.SessionCreate{})

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	msgID := string(event.NewID())
	spec, err := json.Marshal(gen.MessageSend{Id: msgID, Text: ptr("look at this")})
	if err != nil {
		t.Fatalf("encode message: %v", err)
	}
	if err := mw.WriteField("message", string(spec)); err != nil {
		t.Fatalf("write field: %v", err)
	}
	head := textproto.MIMEHeader{}
	// A traversal in the file name must not survive.
	head.Set("Content-Disposition", `form-data; name="file"; filename="../../etc/report.md"`)
	head.Set("Content-Type", "text/markdown")
	part, err := mw.CreatePart(head)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write([]byte("# notes\n")); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, h.srv.URL+BaseURL+"/sessions/"+sess.Id+"/messages", &body)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+h.ownerToken)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	acc := decode[gen.MessageAccepted](t, resp, http.StatusAccepted)
	if acc.Id != msgID {
		t.Fatalf("accepted id = %s, want %s", acc.Id, msgID)
	}

	stored, err := h.st.GetMessage(context.Background(), event.ID(*acc.MessageId))
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if len(stored.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(stored.Attachments))
	}
	a := stored.Attachments[0]
	if a.Name != "report.md" {
		t.Fatalf("name = %q, want the base name", a.Name)
	}
	if a.Kind != event.KindDocument || !strings.HasPrefix(a.MIME, "text/markdown") {
		t.Fatalf("attachment = %+v", a)
	}
	if a.Size != int64(len("# notes\n")) || a.SHA256 != a.BlobID {
		t.Fatalf("attachment = %+v", a)
	}
}

func TestBlobRoundTrip(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	req, err := http.NewRequest(http.MethodPost, h.srv.URL+BaseURL+"/blobs", strings.NewReader("hello blob"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer "+h.ownerToken)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	b := decode[gen.Blob](t, resp, http.StatusCreated)
	if b.Size != int64(len("hello blob")) || b.Mime != "text/plain" {
		t.Fatalf("blob = %+v", b)
	}

	get := h.do(http.MethodGet, "/blobs/"+b.Id, h.ownerToken, nil)
	defer func() { _ = get.Body.Close() }()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d", get.StatusCode)
	}
	if got := get.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff header = %q", got)
	}
	if got := get.Header.Get("Content-Disposition"); got != "attachment" {
		t.Fatalf("disposition = %q", got)
	}
	if got := get.Header.Get("Content-Type"); got != "text/plain" {
		t.Fatalf("content type = %q", got)
	}
	bytesBack, err := io.ReadAll(get.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(bytesBack) != "hello blob" {
		t.Fatalf("body = %q", bytesBack)
	}

	errorBody(t, h.do(http.MethodGet, "/blobs/deadbeef", h.ownerToken, nil), http.StatusNotFound, CodeNotFound)

	// A message may only reference a blob that exists.
	sess := h.newSession(h.ownerToken, gen.SessionCreate{})
	errorBody(t, h.do(http.MethodPost, "/sessions/"+sess.Id+"/messages", h.ownerToken, gen.MessageSend{
		Id:          string(event.NewID()),
		Attachments: &[]gen.AttachmentRef{{BlobId: "0000"}},
	}), http.StatusBadRequest, CodeBadRequest)

	// One that does becomes a full attachment, with kind derived from the mime.
	acc := decode[gen.MessageAccepted](t, h.do(http.MethodPost, "/sessions/"+sess.Id+"/messages", h.ownerToken, gen.MessageSend{
		Id:          string(event.NewID()),
		Attachments: &[]gen.AttachmentRef{{BlobId: b.Id, Name: ptr("notes.txt")}},
	}), http.StatusAccepted)
	stored, err := h.st.GetMessage(context.Background(), event.ID(*acc.MessageId))
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if len(stored.Attachments) != 1 || stored.Attachments[0].Kind != event.KindDocument {
		t.Fatalf("attachments = %+v", stored.Attachments)
	}
}

// TestBlobSniffsContentType covers the fallback when the client declares
// nothing useful.
func TestBlobSniffsContentType(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+BaseURL+"/blobs", strings.NewReader("<html><body>hi</body></html>"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.ownerToken)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	b := decode[gen.Blob](t, resp, http.StatusCreated)
	if !strings.HasPrefix(b.Mime, "text/html") {
		t.Fatalf("sniffed mime = %q", b.Mime)
	}
}

// TestUserIsolation is the client-scope rule: another user's session does not
// exist as far as this token is concerned — 404, never 403.
func TestUserIsolation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	mine := h.newSession(h.ownerToken, gen.SessionCreate{})

	for _, c := range []struct {
		method, path string
	}{
		{http.MethodGet, "/sessions/" + mine.Id},
		{http.MethodDelete, "/sessions/" + mine.Id},
		{http.MethodPost, "/sessions/" + mine.Id + "/abort"},
		{http.MethodGet, "/sessions/" + mine.Id + "/messages"},
	} {
		errorBody(t, h.do(c.method, c.path, h.otherToken, nil), http.StatusNotFound, CodeNotFound)
	}
	errorBody(t, h.do(http.MethodPost, "/sessions/"+mine.Id+"/messages", h.otherToken,
		gen.MessageSend{Id: string(event.NewID()), Text: ptr("peek")}), http.StatusNotFound, CodeNotFound)

	// Listing shows only one's own sessions, admin tokens included.
	theirs := h.newSession(h.otherToken, gen.SessionCreate{})
	if got := h.listIDs(h.otherToken); len(got) != 1 || got[0] != theirs.Id {
		t.Fatalf("other user sees %v", got)
	}
	if got := h.listIDs(h.adminToken); len(got) != 0 {
		t.Fatalf("admin sees %v, want none of the other identities' sessions", got)
	}

	// And the event stream of one user never carries the other's events.
	stream := h.stream(h.otherToken, "", "")
	h.emit(event.ID(mine.Id), event.RunStarted)
	stream.quiet(t, 300*time.Millisecond)
	h.emit(event.ID(theirs.Id), event.RunStarted)
	if f := stream.next(t, 2*time.Second); f.Type != string(event.RunStarted) {
		t.Fatalf("frame = %+v", f)
	}
}

func TestApprovalsResolveAndConflict(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := h.ask(tool.Approval{Agent: "main", Tool: "exec", Summary: "run ls", TraceID: event.NewID()})

	list := decode[struct {
		Approvals []gen.Approval `json:"approvals"`
	}](t, h.do(http.MethodGet, "/approvals", h.adminToken, nil), http.StatusOK)
	if len(list.Approvals) != 1 || list.Approvals[0].Id != string(a.ID) {
		t.Fatalf("pending = %+v", list.Approvals)
	}

	// Resolving is admin only.
	errorBody(t, h.do(http.MethodPost, "/approvals/"+string(a.ID), h.ownerToken,
		gen.ApprovalResolve{Approve: true}), http.StatusForbidden, CodeForbidden)

	resolved := decode[gen.Approval](t, h.do(http.MethodPost, "/approvals/"+string(a.ID), h.adminToken,
		gen.ApprovalResolve{Approve: true}), http.StatusOK)
	if resolved.Status != gen.ApprovalStatusApproved {
		t.Fatalf("status = %s", resolved.Status)
	}
	// The first decision stands.
	errorBody(t, h.do(http.MethodPost, "/approvals/"+string(a.ID), h.adminToken,
		gen.ApprovalResolve{Approve: false}), http.StatusConflict, CodeConflict)
	errorBody(t, h.do(http.MethodPost, "/approvals/"+string(event.NewID()), h.adminToken,
		gen.ApprovalResolve{Approve: true}), http.StatusNotFound, CodeNotFound)
}

// TestCredentialsAreWriteOnly proves the ARCHITECTURE.md §4 invariant: the API can write a
// credential and can never read it back, from any endpoint or any log line.
func TestCredentialsAreWriteOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	const secret = "sk-super-secret-value-0123456789"

	resp := h.do(http.MethodPut, "/credentials/ollama", h.adminToken, map[string]string{"api_key": secret})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if got, ok := h.b.Secrets.Get(secrets.Providers, "ollama", "api_key"); !ok || got != secret {
		t.Fatalf("the credential was not stored")
	}

	// There is no route that reads it back.
	get := h.do(http.MethodGet, "/credentials/ollama", h.adminToken, nil)
	defer func() { _ = get.Body.Close() }()
	if get.StatusCode == http.StatusOK {
		t.Fatal("GET /credentials/{provider} answered; credentials must be write-only")
	}
	body, err := io.ReadAll(get.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(body), secret) {
		t.Fatal("the credential appeared in a response body")
	}
	if strings.Contains(h.logs.String(), secret) {
		t.Fatal("the credential appeared in a log line")
	}

	// An unknown provider is a not_found, before anything is written.
	errorBody(t, h.do(http.MethodPut, "/credentials/nope", h.adminToken,
		map[string]string{"api_key": "x"}), http.StatusNotFound, CodeNotFound)
	if _, ok := h.b.Secrets.Get(secrets.Providers, "nope", "api_key"); ok {
		t.Fatal("a credential was written for an unknown provider")
	}
}

func TestBodyCapAndPanicAndNotImplemented(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	t.Run("body cap", func(t *testing.T) {
		big := `{"title":"` + strings.Repeat("a", 2<<20) + `"}`
		req, err := http.NewRequest(http.MethodPost, h.srv.URL+BaseURL+"/sessions", strings.NewReader(big))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+h.ownerToken)
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		errorBody(t, resp, http.StatusRequestEntityTooLarge, CodeTooLarge)
	})

	t.Run("not implemented", func(t *testing.T) {
		for _, path := range []string{"/memory?agent=main&partition=p", "/tasks", "/credentials/ollama/login"} {
			method := http.MethodGet
			resp := h.do(method, path, h.adminToken, nil)
			errorBody(t, resp, http.StatusNotImplemented, CodeNotImplemented)
		}
		// Channels answer an empty list rather than 501: the shape is stable.
		list := decode[struct {
			Channels []gen.ChannelStatus `json:"channels"`
		}](t, h.do(http.MethodGet, "/channels", h.adminToken, nil), http.StatusOK)
		if len(list.Channels) != 0 {
			t.Fatalf("channels = %+v", list.Channels)
		}
	})

	t.Run("panic", func(t *testing.T) {
		// A backend with no store panics on the first dereference past the
		// socket's binding; the client
		// gets a plain internal error, with nothing of the panic in it.
		broken := NewHandler(&Backend{Logger: h.b.Logger, Auth: h.auth})
		req := httptest.NewRequest(http.MethodGet, BaseURL+"/sessions", nil)
		req = req.WithContext(markSocket(req.Context()))
		rec := httptest.NewRecorder()
		broken.ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d", rec.Code)
		}
		var e gen.Error
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if e.Code != CodeInternal || strings.Contains(e.Message, "nil pointer") {
			t.Fatalf("error = %+v", e)
		}
	})
}

func TestSystemEndpoints(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	st := decode[gen.Status](t, h.do(http.MethodGet, "/status", h.ownerToken, nil), http.StatusOK)
	if st.Version != "test" || st.UptimeSeconds < 1 {
		t.Fatalf("status = %+v", st)
	}
	if st.PendingApprovals == nil || *st.PendingApprovals != 0 {
		t.Fatalf("pending approvals = %v", st.PendingApprovals)
	}

	agents := decode[struct {
		Agents []gen.AgentInfo `json:"agents"`
	}](t, h.do(http.MethodGet, "/agents", h.ownerToken, nil), http.StatusOK)
	if len(agents.Agents) != 1 || agents.Agents[0].Name != "main" {
		t.Fatalf("agents = %+v", agents.Agents)
	}
	if agents.Agents[0].Models["chat"] != "gemma" {
		t.Fatalf("models = %+v", agents.Agents[0].Models)
	}
	if _, ok := agents.Agents[0].Models["fallback"]; ok {
		t.Fatal("an unset role was reported")
	}

	models := decode[struct {
		Models []gen.ModelInfo `json:"models"`
	}](t, h.do(http.MethodGet, "/models", h.ownerToken, nil), http.StatusOK)
	if len(models.Models) != 1 || models.Models[0].Context != 8192 {
		t.Fatalf("models = %+v", models.Models)
	}
}

func TestTokensInvites(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// A token's secret is shown once and never listed.
	tc := decode[gen.TokenCreated](t, h.do(http.MethodPost, "/tokens", h.ownerAdmin,
		gen.TokenCreate{Name: "gui", Scope: ptr(gen.TokenCreateScopeClient), UserId: ptr(string(h.owner.ID))}), http.StatusCreated)
	if tc.Secret == "" || tc.Token.Scope != gen.TokenScopeClient {
		t.Fatalf("token = %+v", tc.Token)
	}
	listResp := h.do(http.MethodGet, "/tokens", h.adminToken, nil)
	raw, err := io.ReadAll(listResp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = listResp.Body.Close()
	if strings.Contains(string(raw), tc.Secret) {
		t.Fatal("the token secret appeared in the listing")
	}
	// The new token works, and stops working once revoked.
	if resp := h.do(http.MethodGet, "/status", tc.Secret, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("new token: status = %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
	if resp := h.do(http.MethodDelete, "/tokens/"+tc.Token.Id, h.ownerAdmin, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status = %d", resp.StatusCode)
	} else {
		_ = resp.Body.Close()
	}
	errorBody(t, h.do(http.MethodGet, "/status", tc.Secret, nil), http.StatusUnauthorized, CodeUnauthorized)

	configChannel(h, "tg-main", "telegram", "off")
	inv := decode[gen.Invite](t, h.do(http.MethodPost, "/invites", h.adminToken,
		gen.InviteCreate{Channel: "tg-main", UserId: ptr(string(h.other.ID))}), http.StatusCreated)
	if inv.Code == "" || inv.ExpiresAt.Before(time.Now()) {
		t.Fatalf("invite = %+v", inv)
	}
}

// TestSocketSessionsUseTheOwner covers the bootstrap path: the socket is the
// owner, so its sessions are keyed on the owner and route as such.
func TestSocketSessionsUseTheOwner(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rec := h.socket(http.MethodPost, "/sessions", gen.SessionCreate{})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	var sess gen.Session
	if err := json.Unmarshal(rec.Body.Bytes(), &sess); err != nil {
		t.Fatalf("decode: %v", err)
	}
	send := h.socket(http.MethodPost, "/sessions/"+sess.Id+"/messages",
		gen.MessageSend{Id: string(event.NewID()), Text: ptr("hola")})
	if send.Code != http.StatusAccepted {
		t.Fatalf("send status = %d, body %s", send.Code, send.Body)
	}
	var acc gen.MessageAccepted
	if err := json.Unmarshal(send.Body.Bytes(), &acc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	routed, err := h.st.GetSession(context.Background(), event.ID(*acc.SessionId))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if routed.UserID != h.owner.ID {
		t.Fatalf("routed to user %s, want the owner %s", routed.UserID, h.owner.ID)
	}
}

// A session created for an agent other than the channel's keeps it: the
// message lands in that session, not in a sibling under the default agent.
func TestSendMessageStaysInTheSessionsAgent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	sess := h.newSession(h.ownerToken, gen.SessionCreate{Agent: ptr("codex")})

	acc := decode[gen.MessageAccepted](t, h.do(http.MethodPost, "/sessions/"+sess.Id+"/messages", h.ownerToken,
		gen.MessageSend{Id: string(event.NewID()), Text: ptr("hola")}), http.StatusAccepted)
	if acc.SessionId == nil || *acc.SessionId != sess.Id {
		t.Fatalf("session id = %v, want %s", acc.SessionId, sess.Id)
	}
	list := decode[struct {
		Sessions []gen.Session `json:"sessions"`
	}](t, h.do(http.MethodGet, "/sessions", h.ownerToken, nil), http.StatusOK)
	if len(list.Sessions) != 1 || list.Sessions[0].Agent != "codex" {
		t.Fatalf("sessions = %+v, want only the codex one", list.Sessions)
	}
}

// A task session is visible to the task's creator, but it is not a
// conversation: a message posted to it is refused, nothing is stored and no
// sibling chat session is opened.
func TestSendMessageRefusesATaskSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	sess, _, err := h.st.EnsureSession(ctx, store.Session{
		Key: "main/task/x", Agent: "main", Kind: store.SessionTask, UserID: h.owner.ID,
	})
	if err != nil {
		t.Fatalf("ensure session: %v", err)
	}

	errorBody(t, h.do(http.MethodPost, "/sessions/"+string(sess.ID)+"/messages", h.ownerToken,
		gen.MessageSend{Id: string(event.NewID()), Text: ptr("hola")}), http.StatusConflict, CodeConflict)

	msgs, err := h.st.ListMessages(ctx, sess.ID, "", 10)
	if err != nil {
		t.Fatalf("list messages: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("messages = %+v, want none", msgs)
	}
	all, err := h.st.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("sessions = %+v, want only the task session", all)
	}
}
