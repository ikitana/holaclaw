package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"holaclaw/internal/access"
	"holaclaw/internal/agent"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/approval"
	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/channel"
	"holaclaw/internal/channel/emoji"
	"holaclaw/internal/channel/googlechat"
	"holaclaw/internal/channel/slack"
	"holaclaw/internal/channel/telegram"
	"holaclaw/internal/channel/whatsapp"
	"holaclaw/internal/config"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
)

// channels owns the adapter instances: it starts them, tracks their
// lifecycle, delivers replies through the instance a conversation lives on,
// and shows approval prompts where the platform has buttons.
type channels struct {
	d      *Daemon
	gate   *access.Gate
	mu     sync.Mutex
	byName map[string]channel.Channel
	status map[string]gen.ChannelStatus
}

// setupChannels builds the access gate and one adapter per configured
// channel instance. Adapters start in Run.
func (d *Daemon) setupChannels() error {
	a := d.App
	cs := &channels{d: d, byName: map[string]channel.Channel{}, status: map[string]gen.ChannelStatus{}}
	cs.gate = access.New(access.Deps{
		Config: func(name string) (config.Access, bool) {
			c, ok := a.Config.Channels[name]
			return c.Access, ok
		},
		Auth: d.Auth, Store: a.Store, Dedupe: a.Store,
		Route: d.Router.Route, Agent: d.agentFor, Events: a.Events,
		Notify: func(ctx context.Context, target event.Origin, text string) {
			if err := cs.send(ctx, target, "", "", event.Message{Schema: event.SchemaVersion, ID: event.NewID(), Role: event.RoleAssistant, Text: text, CreatedAt: time.Now().UTC()}); err != nil {
				d.log.Warn("notice not delivered", "channel", target.Channel, "err", err)
			}
		},
	})
	d.Router.Sent = a.Store
	d.Router.OnApprovalReaction = cs.answerByReaction
	a.Reacter = cs

	for name, cc := range a.Config.Channels {
		ch, err := cs.build(name, cc)
		if err != nil {
			return err
		}
		cs.byName[name] = ch
		cs.status[name] = gen.ChannelStatus{Name: name, Type: cc.Type, Agent: cc.Agent, State: gen.ChannelStatusState(channel.StateUnlinked)}
		d.SetDeliverer(name, cs.deliver)
	}
	d.channels = cs
	d.Backend.Channels = cs.list
	d.Backend.ChannelLinker = cs.linker
	return nil
}

// linker is the adapter of a channel instance as a Linker, or nil when it has
// no interactive login (only WhatsApp has one).
func (cs *channels) linker(name string) channel.Linker {
	l, _ := cs.byName[name].(channel.Linker)
	return l
}

func (cs *channels) build(name string, cc config.Channel) (channel.Channel, error) {
	a := cs.d.App
	state := channel.SQLState{DB: a.Store.DB(), Channel: name}
	put := func(ctx context.Context, r io.Reader, mime string) (string, int64, error) {
		b, err := a.Store.PutBlob(ctx, r, mime)
		return b.ID, b.Size, err
	}
	onApproval := func(ctx context.Context, id event.ID, choice string, from []string) {
		cs.resolveApproval(ctx, name, id, choice, from)
	}
	// Google Chat rewrites its card only when the press resolved the approval.
	onApprovalAccepted := func(ctx context.Context, id event.ID, choice string, from []string) bool {
		return cs.resolveApproval(ctx, name, id, choice, from)
	}
	// The bot was added to a room: the gate decides whether that approves it.
	onJoined := func(ctx context.Context, conversation string, by []string) {
		cs.gate.OnJoined(ctx, event.Origin{Channel: name, ChannelType: cc.Type, Conversation: conversation}, by)
	}
	switch cc.Type {
	case "telegram":
		return telegram.New(telegram.Options{
			Name: name, Token: a.Secrets.Lookup(secrets.Channels, name, "bot_token"),
			BaseURL: cs.d.opts.ChannelBaseURL[name],
			State:   state, Blobs: blobOpener{a.Store}, PutBlob: put, OnApproval: onApproval, OnJoined: onJoined,
			Logger: cs.d.log,
		}), nil
	case "slack":
		return slack.New(slack.Options{
			Name:       name,
			BotToken:   a.Secrets.Lookup(secrets.Channels, name, "bot_token"),
			AppToken:   a.Secrets.Lookup(secrets.Channels, name, "app_token"),
			APIBaseURL: cs.d.opts.ChannelBaseURL[name],
			State:      state, Blobs: blobOpener{a.Store}, PutBlob: put, OnApproval: onApproval, Logger: cs.d.log,
		}), nil
	case "googlechat":
		return googlechat.New(googlechat.Options{
			Name: name, Key: a.Secrets.Lookup(secrets.Channels, name, "service_account"),
			BaseURL: cs.d.opts.ChannelBaseURL[name],
			State:   state, Blobs: blobOpener{a.Store}, PutBlob: put, OnApproval: onApprovalAccepted, OnJoined: onJoined,
			Logger: cs.d.log,
		}), nil
	case "whatsapp":
		// The session database is the credential, one file per instance
		// (docs/channels/whatsapp.md §5.2), so the name must stay a file name.
		if name == "." || name == ".." || filepath.Base(name) != name {
			return nil, fmt.Errorf("channel %s: the name must not hold a path separator", name)
		}
		return whatsapp.New(whatsapp.Options{
			Name: name, SessionPath: filepath.Join(a.DataDir, "whatsapp", name+".db"),
			State: state, Blobs: blobOpener{a.Store}, PutBlob: put, OnJoined: onJoined, Logger: cs.d.log,
		}), nil
	}
	return nil, fmt.Errorf("channel %s: unknown type %q", name, cc.Type)
}

// run starts every adapter and blocks until ctx ends. One failing instance
// (bad token) never stops the others or the daemon.
func (cs *channels) run(ctx context.Context) {
	var wg sync.WaitGroup
	for name, ch := range cs.byName {
		wg.Add(1)
		go func(name string, ch channel.Channel) {
			defer wg.Done()
			err := ch.Run(ctx, cs.gate.Inbound, func(s channel.State, detail string) { cs.setStatus(ctx, name, s, detail) })
			if err != nil && ctx.Err() == nil {
				cs.d.log.Error("channel stopped", "channel", name, "err", err)
			}
		}(name, ch)
	}
	wg.Wait()
}

func (cs *channels) setStatus(ctx context.Context, name string, s channel.State, detail string) {
	cs.mu.Lock()
	st := cs.status[name]
	st.State = gen.ChannelStatusState(s)
	st.Detail = nil
	if detail != "" {
		st.Detail = &detail
	}
	cs.status[name] = st
	cs.mu.Unlock()
	cs.d.App.Events.Emit(ctx, event.New(event.ChannelStatus, event.NewID(), "", "", map[string]any{
		"channel": name, "state": string(s), "detail": detail,
	}))
}

func (cs *channels) list() []gen.ChannelStatus {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]gen.ChannelStatus, 0, len(cs.status))
	for _, s := range cs.status {
		out = append(out, s)
	}
	return out
}

// deliver sends a reply through the adapter the conversation lives on, with a
// typing indicator handled by the caller's turn. The target is the session
// origin — never anything the model produced — with PlatformMsgID set to the
// message the turn answers when reply_to asks for a native reply.
func (cs *channels) deliver(ctx context.Context, sess store.Session, traceID event.ID, reply event.Message) error {
	return cs.send(ctx, sess.Origin, cs.replyTo(ctx, sess.Origin, traceID), traceID, reply)
}

// replyTo is the platform id of the message that started the turn, or "" for
// a plain reply.
func (cs *channels) replyTo(ctx context.Context, o event.Origin, traceID event.ID) string {
	switch cs.d.App.Config.Channels[o.Channel].Access.ReplyTo {
	case "off":
		return ""
	case "always":
	default: // "rooms"
		if o.ConversationKind != event.ConversationRoom {
			return ""
		}
	}
	return cs.inbound(ctx, o, traceID)
}

// inbound is the platform id of the message that started the turn, or "".
// The session row keeps the origin of its first message, so the id comes
// from the channel.received event of the turn's trace: always the triggering
// envelope. A trace without one (a task result) has none.
func (cs *channels) inbound(ctx context.Context, o event.Origin, traceID event.ID) string {
	if traceID == "" {
		return ""
	}
	evs, err := cs.d.App.Store.ListEvents(ctx, traceID)
	if err != nil {
		cs.d.log.Warn("inbound message lookup failed", "channel", o.Channel, "err", err)
		return ""
	}
	for _, e := range evs {
		if e.Type != event.ChannelReceived {
			continue
		}
		var a struct {
			Channel       string `json:"channel"`
			Conversation  string `json:"conversation"`
			PlatformMsgID string `json:"platform_msg_id"`
		}
		if json.Unmarshal(e.Attrs, &a) == nil && a.Channel == o.Channel && a.Conversation == o.Conversation {
			return a.PlatformMsgID
		}
	}
	return ""
}

// send delivers msg to target, as a native reply to the platform message
// replyTo when it is set; only deliver passes one. target.PlatformMsgID is
// always replaced: a session origin holds its first message's id, which a
// plain send must never quote.
func (cs *channels) send(ctx context.Context, target event.Origin, replyTo string, traceID event.ID, msg event.Message) error {
	return cs.sendPrompt(ctx, target, replyTo, traceID, msg, "")
}

// sendPrompt is send for a message that may be an approval prompt. The
// platform ids of a message sent in a session are kept, so a reaction to one
// can be routed by its target: approvalID marks them as that approval's
// prompt, otherwise they point at msg.
func (cs *channels) sendPrompt(ctx context.Context, target event.Origin, replyTo string, traceID event.ID, msg event.Message, approvalID event.ID) error {
	target.PlatformMsgID = replyTo
	ch, ok := cs.byName[target.Channel]
	if !ok {
		return fmt.Errorf("channel %q is not configured", target.Channel)
	}
	start := time.Now()
	rcpt, err := ch.Send(ctx, channel.Outbound{TraceID: traceID, Target: target, Message: msg})
	if err != nil {
		return err
	}
	// The rows go in before channel.sent: a client that reacts to what the
	// event names must find them.
	if msg.SessionID != "" && (msg.ID != "" || approvalID != "") && len(rcpt.PlatformMsgIDs) > 0 {
		rows := make([]store.Sent, 0, len(rcpt.PlatformMsgIDs))
		for _, id := range rcpt.PlatformMsgIDs {
			r := store.Sent{Channel: target.Channel, Conversation: target.Conversation, PlatformMsgID: id,
				SessionID: msg.SessionID, ApprovalID: approvalID}
			if approvalID == "" {
				r.MessageID = msg.ID
			}
			rows = append(rows, r)
		}
		if err := cs.d.App.Store.PutSent(ctx, rows); err != nil {
			cs.d.log.Warn("sent message ids not kept: reactions to them are dropped", "channel", target.Channel, "err", err)
		}
	}
	ev := event.New(event.ChannelSent, traceID, msg.SessionID, "", map[string]any{
		"channel": target.Channel, "platform_msg_ids": rcpt.PlatformMsgIDs,
	})
	ev.Duration, ev.MessageID = time.Since(start), msg.ID
	cs.d.App.Events.Emit(ctx, ev)
	for _, reason := range rcpt.Degraded {
		cs.d.App.Events.Emit(ctx, event.New(event.DeliveryDegraded, traceID, msg.SessionID, "", map[string]any{
			"channel": target.Channel, "reason": reason,
		}))
	}
	return nil
}

// typing shows a typing indicator for a conversation when the platform has one.
func (cs *channels) typing(ctx context.Context, target event.Origin) (stop func()) {
	if t, ok := cs.byName[target.Channel].(channel.Typer); ok {
		return t.Typing(ctx, target)
	}
	return func() {}
}

// Emit watches for tool approvals raised in a channel conversation and shows
// the buttons there when the person talking is an owner or admin. Everyone
// else's approvals go to the API (GUI / CLI) only.
func (cs *channels) Emit(ctx context.Context, e event.Event) {
	if e.SessionID == "" {
		return
	}
	switch e.Type {
	case event.ApprovalRequested:
		go cs.prompt(context.WithoutCancel(ctx), e)
	case event.BrowserTakeoverRequest:
		// A browser waiting for a person is shown here as text plus a
		// screenshot; the driving itself needs the GUI (docs/browser.md).
		go cs.takeoverNotice(context.WithoutCancel(ctx), e)
	}
}

func (cs *channels) prompt(ctx context.Context, e event.Event) {
	var attrs struct {
		ID event.ID `json:"id"`
	}
	if json.Unmarshal(e.Attrs, &attrs) != nil || attrs.ID == "" {
		return
	}
	sess, err := cs.d.App.Store.GetSession(ctx, e.SessionID)
	if err != nil {
		return
	}
	ch := cs.byName[sess.Origin.Channel]
	p, buttons := ch.(channel.ApprovalPrompter)
	_, reacts := ch.(channel.Reactor)
	if !buttons && !reacts || !cs.turnIsAdmin(ctx, sess, e.TraceID) {
		return
	}
	a, err := cs.d.Approvals.Get(ctx, attrs.ID)
	if err != nil || a.Status != tool.ApprovalPending {
		return
	}
	target := sess.Origin
	target.PlatformMsgID = "" // a prompt is never a reply
	text := "Allow: " + a.Summary + "?"
	if buttons {
		err = p.PromptApproval(ctx, target, a.ID, text, []string{"once", "session", "always", "deny"})
	} else {
		// No buttons: the prompt is text, answered by a reaction to it.
		err = cs.sendPrompt(ctx, target, "", e.TraceID, event.Message{
			// No ID: the prompt is never in the transcript.
			Schema: event.SchemaVersion, SessionID: sess.ID, TraceID: e.TraceID,
			Role: event.RoleAssistant, Text: text + "\n\n" + reactToAnswer, CreatedAt: time.Now().UTC(),
		}, a.ID)
	}
	if err != nil {
		cs.d.log.Warn("approval prompt failed", "channel", sess.Origin.Channel, "err", err)
	}
}

// reactToAnswer closes an approval prompt on a channel without buttons.
const reactToAnswer = "React to this message to answer: 👍 allow once · 🔁 allow for this session · ✅ always allow · 👎 deny"

// reactionChoice maps a reaction to an approval choice, or "" for an emoji
// that answers nothing. It compares emoji.Base, so 👍🏽 is 👍.
func reactionChoice(e string) string {
	switch emoji.Base(e) {
	case "👍":
		return "once"
	case "🔁":
		return "session"
	case "✅":
		return "always"
	case "👎":
		return "deny"
	}
	return ""
}

// answerByReaction resolves an approval from a reaction to its prompt
// (session.Router.OnApprovalReaction). The reactor is the envelope's sender,
// checked like a button press; the first accepted reaction wins, so a later
// change or a removal finds the approval settled and does nothing. The answer
// is a reply to the prompt naming who gave it; a refused reaction gets none.
func (cs *channels) answerByReaction(ctx context.Context, env event.Envelope, sent store.Sent) {
	if env.Reaction.Removed {
		return
	}
	choice := reactionChoice(env.Reaction.Emoji)
	if choice == "" {
		return
	}
	i, ok := cs.approve(ctx, env.Origin.Channel, sent.ApprovalID, choice, env.Sender.Aliases)
	if !ok {
		return
	}
	sess, err := cs.d.App.Store.GetSession(ctx, sent.SessionID)
	if err != nil {
		cs.d.log.Warn("approval answer not sent", "approval", sent.ApprovalID, "err", err)
		return
	}
	// An identity name is admin-set, but cleaned like any label shown back.
	name := "an admin"
	if strings.TrimSpace(i.Name) != "" {
		name = agent.SanitizeSender(i.Name)
	}
	text := "Denied by " + name
	if choice != "deny" {
		text = "Approved (" + map[string]string{"once": "once", "session": "for this session", "always": "always"}[choice] + ") by " + name
	}
	msg := event.Message{
		// No ID: like the prompt, the answer is never in the transcript, and
		// a reaction to it has nothing to point at.
		Schema: event.SchemaVersion, SessionID: sess.ID, TraceID: env.TraceID,
		Role: event.RoleAssistant, Text: text, CreatedAt: time.Now().UTC(),
	}
	if err := cs.send(ctx, sess.Origin, sent.PlatformMsgID, env.TraceID, msg); err != nil {
		cs.d.log.Warn("approval answer not sent", "approval", sent.ApprovalID, "err", err)
	}
}

// canReact reports whether a channel instance takes reactions.
func (cs *channels) canReact(name string) bool {
	if cs == nil {
		return false
	}
	_, ok := cs.byName[name].(channel.Reactor)
	return ok
}

// React puts emoji e on the turn's inbound message (tools.Reacter). The
// target is the message that started the turn, found like replyTo's, in the
// session's conversation: never anything the model chose. Errors are for the
// model to read.
func (cs *channels) React(ctx context.Context, c tool.Call, e string) error {
	ch := cs.byName[c.Origin.Channel]
	r, ok := ch.(channel.Reactor)
	if !ok {
		return errors.New("this conversation does not take reactions; answer in text")
	}
	// Models write ❤️ where a list may hold ❤: skin tones and the
	// presentation selector are ignored in the comparison, and the list's own
	// form is what is sent.
	if allowed := ch.Limits().Reactions; allowed != nil {
		i := slices.IndexFunc(allowed, func(a string) bool { return emoji.Base(a) == emoji.Base(e) })
		if i < 0 {
			return fmt.Errorf("this channel takes only these emoji: %s", strings.Join(allowed, " "))
		}
		e = allowed[i]
	}
	id := cs.inbound(ctx, c.Origin, c.TraceID)
	if id == "" {
		return errors.New("there is no message to react to; answer in text")
	}
	target := c.Origin
	target.PlatformMsgID = id
	if err := r.React(ctx, target, e); err != nil {
		return fmt.Errorf("the reaction was not sent: %v", err)
	}
	// The client shows it under the stored message the turn answers.
	ev := event.New(event.ChannelReacted, c.TraceID, c.SessionID, "", map[string]any{
		"channel": c.Origin.Channel, "platform_msg_id": id, "emoji": e,
	})
	if ms, err := cs.d.App.Store.ListTraceMessages(ctx, c.TraceID); err == nil {
		for _, m := range ms {
			if m.Role == event.RoleUser {
				ev.MessageID = m.ID
				break
			}
		}
	}
	cs.d.App.Events.Emit(ctx, ev)
	return nil
}

// turnIsAdmin reports whether the turn behind a trace acts as an owner or admin,
// so its approvals and takeovers may be shown in its conversation. A room
// holds no role, so in a room's session the answer comes from who the turn
// answers: the router.resolved events of its trace, each of them an owner or
// admin, the strictest sender of a coalesced turn included. No such event means
// no.
func (cs *channels) turnIsAdmin(ctx context.Context, sess store.Session, traceID event.ID) bool {
	if sess.Kind != store.SessionChat || sess.Origin.ConversationKind != event.ConversationRoom {
		return cs.isAdmin(ctx, sess.UserID)
	}
	evs, err := cs.d.App.Store.ListEvents(ctx, traceID)
	if err != nil {
		cs.d.log.Warn("turn sender lookup failed", "session", sess.ID, "err", err)
		return false
	}
	found := false
	for _, e := range evs {
		if e.Type != event.RouterResolved {
			continue
		}
		var a struct {
			User event.ID `json:"user"`
		}
		if json.Unmarshal(e.Attrs, &a) != nil || !cs.isAdmin(ctx, a.User) {
			return false
		}
		found = true
	}
	return found
}

func (cs *channels) isAdmin(ctx context.Context, id event.ID) bool {
	if id == "" {
		return false
	}
	i, err := cs.d.App.Store.GetIdentity(ctx, id)
	return err == nil && !i.Disabled() && (i.Role == store.RoleOwner || i.Role == store.RoleAdmin)
}

// resolveApproval handles a button press. The presser is identified from the
// platform callback, never from the button payload, and must be owner or admin.
// It reports whether the press resolved the approval.
func (cs *channels) resolveApproval(ctx context.Context, channelName string, id event.ID, choice string, from []string) bool {
	_, ok := cs.approve(ctx, channelName, id, choice, from)
	return ok
}

// approve is resolveApproval that also names who answered.
func (cs *channels) approve(ctx context.Context, channelName string, id event.ID, choice string, from []string) (store.Identity, bool) {
	i, err := cs.d.Auth.Resolve(ctx, channelName, from)
	if err != nil || !cs.isAdmin(ctx, i.ID) {
		// Anyone in a room can react to a prompt: routine, not a warning.
		cs.d.log.Debug("approval answer ignored: not an owner or admin", "channel", channelName)
		return store.Identity{}, false
	}
	var approve bool
	scope := tool.GrantOnce
	switch choice {
	case "once", "session", "always":
		approve, scope = true, tool.GrantScope(choice)
	case "deny":
	default:
		cs.d.log.Warn("approval press ignored: unknown choice", "channel", channelName)
		return store.Identity{}, false
	}
	if _, err := cs.d.Approvals.Resolve(ctx, id, approve, scope, i.ID); err != nil {
		if errors.Is(err, approval.ErrNotPending) {
			// Settled already: a second press or a later reaction.
			cs.d.log.Debug("approval already settled", "approval", id)
		} else {
			cs.d.log.Warn("approval resolve failed", "approval", id, "err", err)
		}
		return store.Identity{}, false
	}
	return i, true
}

// blobOpener lets adapters read attachment bytes for delivery.
type blobOpener struct{ st *store.SQLite }

func (b blobOpener) OpenAttachment(ctx context.Context, a event.Attachment) ([]byte, error) {
	rc, _, err := b.st.OpenBlob(ctx, a.BlobID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}
