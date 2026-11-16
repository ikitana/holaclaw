package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/session"
	"holaclaw/internal/store"
)

// defaultMessageLimit is the page size of GET /sessions/{id}/messages;
// maxMessageLimit is the cap the spec declares.
const (
	defaultMessageLimit = 100
	maxMessageLimit     = 500
)

// CreateSession returns the session of a conversation, creating it on first
// use: 201 when it was new, 200 when it already existed.
func (h *handlers) CreateSession(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	var in gen.SessionCreate
	if !decodeJSON(w, r, &in) {
		return
	}
	agent := session.DefaultAgent
	if in.Agent != nil && *in.Agent != "" {
		agent = *in.Agent
	}
	conversation := string(event.NewID())
	if in.Conversation != nil && *in.Conversation != "" {
		conversation = *in.Conversation
	}
	// A conversation belongs to the identity, not to the binding it was opened
	// from, so every client of the identity sees it (docs/identity.md §4).
	origin := event.Origin{
		Channel:          event.ChannelHolaclaw,
		ChannelType:      event.ChannelHolaclaw,
		Conversation:     conversation,
		ConversationKind: event.ConversationDM,
	}
	want := store.Session{
		Key:    session.Key(agent, origin, p.UserID),
		Agent:  agent,
		Kind:   store.SessionChat,
		UserID: p.UserID,
		Origin: origin,
	}
	if in.Title != nil {
		want.Title = *in.Title
	}
	sess, created, err := h.b.Store.EnsureSession(r.Context(), want)
	if err != nil {
		h.b.fail(w, "create session", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	// No totals here: a failure to sum must not turn a session that was just
	// created into an error, and a new one has none.
	writeJSON(w, status, h.session(sess))
}

// ListSessions returns the sessions the caller may see: its identity's for a
// token, all of them for the socket.
func (h *handlers) ListSessions(w http.ResponseWriter, r *http.Request) {
	p, _ := PrincipalFrom(r.Context())
	all, err := h.b.Store.ListSessions(r.Context())
	if err != nil {
		h.b.fail(w, "list sessions", err)
		return
	}
	// The totals of every session in one grouped pass, not a query per row.
	var usage map[event.ID]event.Usage
	if ul, ok := h.b.Store.(store.UsageLister); ok {
		if usage, err = ul.SessionUsage(r.Context()); err != nil {
			h.b.fail(w, "list sessions", err)
			return
		}
	}
	out := make([]gen.Session, 0, len(all))
	for _, s := range all {
		if visible(p, s) {
			out = append(out, withUsage(h.session(s), usage, s.ID))
		}
	}
	writeJSON(w, http.StatusOK, struct {
		Sessions []gen.Session `json:"sessions"`
	}{out})
}

func (h *handlers) GetSession(w http.ResponseWriter, r *http.Request, id gen.Id) {
	sess, ok := h.load(w, r, id)
	if !ok {
		return
	}
	h.writeSession(w, r, http.StatusOK, sess)
}

func (h *handlers) DeleteSession(w http.ResponseWriter, r *http.Request, id gen.Id) {
	p, _ := PrincipalFrom(r.Context())
	sess, ok := h.load(w, r, id)
	if !ok || !writable(w, p, sess) {
		return
	}
	// What was derived from the conversation goes first (recall memory): if
	// that fails the session stays, rather than leaving a searchable summary
	// of a conversation the user believes deleted (docs/traceability.md).
	if h.b.BeforeDeleteSession != nil {
		if err := h.b.BeforeDeleteSession(r.Context(), sess.ID); err != nil {
			h.b.fail(w, "delete session", err)
			return
		}
	}
	if h.b.Sessions != nil {
		h.b.Sessions.Abort(sess.ID)
	}
	if err := h.b.Store.DeleteSession(r.Context(), sess.ID); err != nil {
		h.b.fail(w, "delete session", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AbortSession cancels the running turn. Aborting an idle session is not an
// error: the client asked for a state, and that state holds. A task session
// is read-only, but its identity, the task's creator, may still stop a run.
func (h *handlers) AbortSession(w http.ResponseWriter, r *http.Request, id gen.Id) {
	p, _ := PrincipalFrom(r.Context())
	sess, ok := h.load(w, r, id)
	if !ok || (sess.Kind == store.SessionChat && !writable(w, p, sess)) {
		return
	}
	if h.b.Sessions != nil {
		h.b.Sessions.Abort(sess.ID)
	}
	w.WriteHeader(http.StatusAccepted)
}

// ListMessages pages the transcript of a session.
func (h *handlers) ListMessages(w http.ResponseWriter, r *http.Request, id gen.Id, params gen.ListMessagesParams) {
	sess, ok := h.load(w, r, id)
	if !ok {
		return
	}
	limit := defaultMessageLimit
	if params.Limit != nil {
		limit = *params.Limit
	}
	if limit < 1 || limit > maxMessageLimit {
		badRequest(w, fmt.Sprintf("limit must be between 1 and %d", maxMessageLimit))
		return
	}
	var after event.ID
	if params.After != nil {
		after = event.ID(*params.After)
	}
	msgs, err := h.b.Store.ListMessages(r.Context(), sess.ID, after, limit)
	if err != nil {
		h.b.fail(w, "list messages", err)
		return
	}
	out := make([]gen.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toGenMessage(m))
	}
	writeJSON(w, http.StatusOK, struct {
		Messages []gen.Message `json:"messages"`
	}{out})
}

// ListTraces summarises the traces of one session, newest first: how a client
// finds a trace id without having kept the one a turn printed. Enumerating
// them is optional for a store, so one that cannot answers not_implemented
// rather than pretending the session was never run.
func (h *handlers) ListTraces(w http.ResponseWriter, r *http.Request, id gen.Id) {
	lister, ok := h.b.Store.(store.TraceLister)
	if !ok {
		notImplemented(w, "trace listing")
		return
	}
	// The visibility rule is getSession's: a session of another user is a
	// not_found, and so are its traces (docs/identity.md, docs/traceability.md).
	sess, ok := h.load(w, r, id)
	if !ok {
		return
	}
	traces, err := lister.ListTraces(r.Context(), sess.ID)
	if err != nil {
		h.b.fail(w, "list traces", err)
		return
	}
	out := make([]gen.TraceSummary, 0, len(traces))
	for _, t := range traces {
		out = append(out, gen.TraceSummary{
			TraceId:   string(t.TraceID),
			SessionId: string(t.SessionID),
			StartedAt: t.StartedAt,
			EndedAt:   t.EndedAt,
			Events:    t.Events,
			ToolCalls: t.ToolCalls,
			LastType:  optString(string(t.LastType)),
			Preview:   optString(t.Preview),

			PromptTokens:     t.Usage.PromptTokens,
			CompletionTokens: t.Usage.CompletionTokens,
			Cost:             usd(t.Usage.Cost),
			UnpricedCalls:    t.Usage.UnpricedCalls,
			CachedTokens:     t.Usage.CachedTokens,
			ReasoningTokens:  t.Usage.ReasoningTokens,
		})
	}
	writeJSON(w, http.StatusOK, struct {
		Traces []gen.TraceSummary `json:"traces"`
	}{out})
}

// SendMessage stores the message and queues a turn. The client-generated ULID
// is the idempotency key: a resend is accepted and reported as a duplicate
// without starting a second turn (docs/reference/api.md).
func (h *handlers) SendMessage(w http.ResponseWriter, r *http.Request, id gen.Id) {
	p, _ := PrincipalFrom(r.Context())
	sess, ok := h.load(w, r, id)
	if !ok {
		return
	}
	// A task session runs the task's stored prompt under the task's grants,
	// and its runs close the task and reach its delivery target: a typed
	// message has no place in it.
	if sess.Kind != store.SessionChat {
		writeError(w, http.StatusConflict, CodeConflict, "only a chat session takes messages")
		return
	}
	if !writable(w, p, sess) {
		return
	}

	var (
		in          gen.MessageSend
		attachments []event.Attachment
		err         error
	)
	if isMultipart(r) {
		in, attachments, err = h.multipart(r)
	} else {
		if !decodeJSON(w, r, &in) {
			return
		}
	}
	if err != nil {
		if tooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, CodeTooLarge, "payload too large")
			return
		}
		badRequest(w, err.Error())
		return
	}
	msg, ok2 := h.message(r.Context(), w, in, attachments)
	if !ok2 {
		return
	}
	env := event.Envelope{
		Schema: event.SchemaVersion,
		Origin: sess.Origin,
		// The message arrives from the caller's holaclaw binding, and the
		// router resolves it like any other channel's (docs/identity.md §1).
		Sender:   event.Sender{Aliases: []string{p.Binding}, Token: p.TokenID},
		Message:  msg,
		Received: time.Now().UTC(),
	}
	h.route(w, r, env, sess)
}

// route hands an envelope to the router for target and renders the acceptance.
func (h *handlers) route(w http.ResponseWriter, r *http.Request, env event.Envelope, target store.Session) {
	if h.b.Router == nil {
		notImplemented(w, "message routing")
		return
	}
	acc, err := h.b.Router.RouteTo(r.Context(), env, target)
	var unknown *session.ErrUnknownSender
	switch {
	case errors.As(err, &unknown):
		// The caller's binding went between authenticating and routing.
		writeError(w, http.StatusForbidden, CodeForbidden, "unknown sender")
		return
	case errors.Is(err, session.ErrIdentityDisabled):
		writeError(w, http.StatusForbidden, CodeIdentityDisabled, "the identity is disabled")
		return
	case err != nil:
		h.b.fail(w, "route message", err)
		return
	}
	writeJSON(w, http.StatusAccepted, gen.MessageAccepted{
		Id:        string(env.Message.ID),
		MessageId: optID(acc.MessageID),
		TraceId:   string(acc.TraceID),
		SessionId: optID(acc.SessionID),
		Duplicate: acc.Duplicate,
	})
}

// message turns the wire message into a transcript entry: the id must be a
// ULID (it is the idempotency key) and every attachment must reference a blob
// that exists.
func (h *handlers) message(ctx context.Context, w http.ResponseWriter, in gen.MessageSend, extra []event.Attachment) (event.Message, bool) {
	id := event.ID(in.Id)
	if !id.Valid() {
		badRequest(w, "message id must be a ULID")
		return event.Message{}, false
	}
	m := event.Message{Schema: event.SchemaVersion, ID: id, Role: event.RoleUser}
	if in.Text != nil {
		m.Text = *in.Text
	}
	if in.ReplyTo != nil {
		m.ReplyTo = event.ID(*in.ReplyTo)
	}
	if in.Attachments != nil {
		for _, ref := range *in.Attachments {
			a, err := h.attachment(ctx, ref)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					badRequest(w, "attachment references an unknown blob")
					return event.Message{}, false
				}
				h.b.fail(w, "resolve attachment", err)
				return event.Message{}, false
			}
			m.Attachments = append(m.Attachments, a)
		}
	}
	m.Attachments = append(m.Attachments, extra...)
	if m.Text == "" && len(m.Attachments) == 0 {
		badRequest(w, "a message needs text or an attachment")
		return event.Message{}, false
	}
	return m, true
}

// attachment resolves a blob reference into a full attachment. Size, sha256
// and kind come from the store and the mime type, never from the client
// (docs/sessions.md).
func (h *handlers) attachment(ctx context.Context, ref gen.AttachmentRef) (event.Attachment, error) {
	rc, b, err := h.b.Store.OpenBlob(ctx, ref.BlobId)
	if err != nil {
		return event.Attachment{}, err
	}
	_ = rc.Close()
	mime := b.MIME
	if ref.Mime != nil && *ref.Mime != "" {
		mime = *ref.Mime
	}
	if mime == "" {
		mime = defaultMIME
	}
	name := ""
	if ref.Name != nil {
		name = *ref.Name
	}
	return event.Attachment{
		ID:     event.NewID(),
		BlobID: b.ID,
		MIME:   mime,
		Name:   safeName(name, b.ID),
		Size:   b.Size,
		SHA256: b.ID,
		Kind:   event.KindFromMIME(mime),
	}, nil
}

// multipart reads the `message` part and streams every `file` part into the
// blob store, in the order they arrive.
func (h *handlers) multipart(r *http.Request) (gen.MessageSend, []event.Attachment, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return gen.MessageSend{}, nil, fmt.Errorf("invalid multipart body")
	}
	var (
		in     gen.MessageSend
		seen   bool
		attach []event.Attachment
	)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return gen.MessageSend{}, nil, wrapBody(err, "invalid multipart body")
		}
		switch part.FormName() {
		case "message":
			body, err := io.ReadAll(io.LimitReader(part, MaxJSONBody))
			if err != nil {
				return gen.MessageSend{}, nil, wrapBody(err, "invalid multipart body")
			}
			if err := unmarshalStrict(body, &in); err != nil {
				return gen.MessageSend{}, nil, fmt.Errorf("invalid message part: %w", err)
			}
			seen = true
		case "file":
			a, err := h.storePart(r.Context(), part)
			if err != nil {
				return gen.MessageSend{}, nil, err
			}
			attach = append(attach, a)
		}
		_ = part.Close()
	}
	if !seen {
		return gen.MessageSend{}, nil, fmt.Errorf("the multipart body needs a `message` part")
	}
	return in, attach, nil
}

// storePart stores one uploaded file and describes it.
func (h *handlers) storePart(ctx context.Context, part *multipart.Part) (event.Attachment, error) {
	body, mime, err := sniff(part, part.Header.Get("Content-Type"))
	if err != nil {
		return event.Attachment{}, wrapBody(err, "cannot read uploaded file")
	}
	b, err := h.b.Store.PutBlob(ctx, body, mime)
	if err != nil {
		return event.Attachment{}, err
	}
	return event.Attachment{
		ID:     event.NewID(),
		BlobID: b.ID,
		MIME:   mime,
		Name:   safeName(part.FileName(), b.ID),
		Size:   b.Size,
		SHA256: b.ID,
		Kind:   event.KindFromMIME(mime),
	}, nil
}

// safeName reduces a client-supplied file name to its base name: a path, a
// traversal or a control character never reaches the store or a channel.
func safeName(name, fallback string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." || name == "/" {
		return fallback
	}
	if len(name) > 255 {
		name = name[:255]
	}
	return name
}

// --- helpers ------------------------------------------------------------------

// load reads a session the caller may see. A session of another identity is a
// not_found, not a forbidden: existence is not the caller's business.
func (h *handlers) load(w http.ResponseWriter, r *http.Request, id gen.Id) (store.Session, bool) {
	p, _ := PrincipalFrom(r.Context())
	sess, err := h.b.Store.GetSession(r.Context(), event.ID(id))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			notFound(w)
			return store.Session{}, false
		}
		h.b.fail(w, "get session", err)
		return store.Session{}, false
	}
	if !visible(p, sess) {
		notFound(w)
		return store.Session{}, false
	}
	return sess, true
}

// visible is the isolation rule: every token sees only the sessions of its
// binding's identity, admins included, and no room session, which belongs to
// the room rather than to whoever spoke first. The socket sees everything.
// Every token has an identity through its binding; one without would match
// the sessions of nobody, so it sees nothing (docs/identity.md §4).
func visible(p auth.Principal, s store.Session) bool {
	if isSocket(p) {
		return true
	}
	if p.UserID == "" || s.UserID != p.UserID {
		return false
	}
	return s.Origin.ConversationKind != event.ConversationRoom
}

// isSocket reports whether p is the unix socket's holaclaw:cli, the owner on
// the host. No token carries its binding.
func isSocket(p auth.Principal) bool {
	return p.TokenID == "" && p.Binding == auth.SocketAlias
}

// readOnly reports whether a visible session takes no writes from a token:
// only the identity's holaclaw conversations do. A DM through another of its
// bindings continues in that channel, and a task session runs the task's
// stored prompt.
func readOnly(s store.Session) bool {
	return s.Kind != store.SessionChat || s.Origin.Channel != event.ChannelHolaclaw
}

// writable answers a write to a read-only session with a conflict naming
// where the conversation continues. The socket writes to any session.
func writable(w http.ResponseWriter, p auth.Principal, s store.Session) bool {
	if isSocket(p) || !readOnly(s) {
		return true
	}
	msg := "a task session is read-only"
	if s.Kind == store.SessionChat {
		msg = fmt.Sprintf("this conversation is read-only here: continue it in %s", s.Origin.Channel)
	}
	writeError(w, http.StatusConflict, CodeConflict, msg)
	return false
}

// anyVisible reports whether at least one of the session ids exists and is
// visible to p. Ids repeat and most calls carry one, so the lookup is deduped
// and stops at the first hit.
func (h *handlers) anyVisible(ctx context.Context, p auth.Principal, ids []event.ID) bool {
	seen := make(map[event.ID]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		sess, err := h.b.Store.GetSession(ctx, id)
		if err != nil {
			continue
		}
		if visible(p, sess) {
			return true
		}
	}
	return false
}

func (h *handlers) session(s store.Session) gen.Session {
	out := gen.Session{
		Id:        string(s.ID),
		Key:       s.Key,
		Agent:     s.Agent,
		Kind:      gen.SessionKind(s.Kind),
		UserId:    optID(s.UserID),
		Origin:    toGenOrigin(s.Origin),
		Title:     optString(s.Title),
		ReadOnly:  readOnly(s),
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
	}
	if h.b.Sessions != nil {
		out.Busy = ptr(h.b.Sessions.Busy(s.ID))
	}
	return out
}

// writeSession answers with one session and its totals, asked for that
// session alone.
func (h *handlers) writeSession(w http.ResponseWriter, r *http.Request, status int, s store.Session) {
	var usage map[event.ID]event.Usage
	if ul, ok := h.b.Store.(store.UsageLister); ok {
		var err error
		if usage, err = ul.SessionUsage(r.Context(), s.ID); err != nil {
			h.b.fail(w, "get session", err)
			return
		}
	}
	writeJSON(w, status, withUsage(h.session(s), usage, s.ID))
}

// withUsage fills a session's totals from what the store summed. A nil map
// is a store that cannot total (the fields stay absent); a session missing
// from a map had no model call, which is a zero. Cost and the token details
// stay absent when no call reported them.
func withUsage(out gen.Session, usage map[event.ID]event.Usage, id event.ID) gen.Session {
	if usage == nil {
		return out
	}
	u := usage[id]
	out.PromptTokens, out.CompletionTokens = ptr(u.PromptTokens), ptr(u.CompletionTokens)
	out.UnpricedCalls = ptr(u.UnpricedCalls)
	out.Cost, out.CachedTokens, out.ReasoningTokens = usd(u.Cost), u.CachedTokens, u.ReasoningTokens
	return out
}

// usd is a summed cost in USD, exact, for the API's decimal string; nil
// stays absent.
func usd(n *provider.NanoUSD) *decimal.Decimal {
	if n == nil {
		return nil
	}
	return ptr(n.Decimal())
}

func toGenOrigin(o event.Origin) gen.Origin {
	return gen.Origin{
		Channel:          o.Channel,
		ChannelType:      o.ChannelType,
		Conversation:     o.Conversation,
		ConversationKind: gen.OriginConversationKind(o.ConversationKind),
		Thread:           optString(o.Thread),
		PlatformMsgId:    optString(o.PlatformMsgID),
	}
}

func toGenMessage(m event.Message) gen.Message {
	out := gen.Message{
		Schema:     m.Schema,
		Id:         string(m.ID),
		SessionId:  string(m.SessionID),
		TraceId:    string(m.TraceID),
		Role:       gen.MessageRole(m.Role),
		Text:       optString(m.Text),
		ReplyTo:    optID(m.ReplyTo),
		ToolCallId: optString(m.ToolCallID),
		ToolName:   optString(m.ToolName),
		Sender:     optString(m.Sender),
		CreatedAt:  m.CreatedAt,
	}
	if m.Tainted {
		out.Tainted = ptr(true)
	}
	// Tainted says where the text came from; the score says whether it tries
	// to steer (docs/security.md). Nothing scored is not a zero score, so an
	// unscored message carries no field rather than a confident 0.
	if m.TaintScore != 0 {
		out.TaintScore = ptr(float32(m.TaintScore))
	}
	if len(m.Attachments) > 0 {
		list := make([]gen.Attachment, 0, len(m.Attachments))
		for _, a := range m.Attachments {
			list = append(list, toGenAttachment(a))
		}
		out.Attachments = &list
	}
	if len(m.ToolCalls) > 0 {
		list := make([]gen.ToolCall, 0, len(m.ToolCalls))
		for _, c := range m.ToolCalls {
			list = append(list, gen.ToolCall{Id: c.ID, Name: c.Name, Arguments: c.Arguments})
		}
		out.ToolCalls = &list
	}
	return out
}

func toGenAttachment(a event.Attachment) gen.Attachment {
	out := gen.Attachment{
		Id:     string(a.ID),
		BlobId: a.BlobID,
		Mime:   a.MIME,
		Name:   a.Name,
		Size:   a.Size,
		Sha256: a.SHA256,
		Kind:   gen.AttachmentKind(a.Kind),
	}
	if a.Derived != nil {
		out.Derived = &struct {
			By   gen.AttachmentDerivedBy `json:"by"`
			Text string                  `json:"text"`
		}{By: gen.AttachmentDerivedBy(a.Derived.By), Text: a.Derived.Text}
	}
	return out
}
