package session

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"holaclaw/internal/agent"
	"holaclaw/internal/channel/emoji"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// DefaultDedupeTTL is how long a platform message id is remembered, so a
// re-delivery by the platform does not start a second turn.
const DefaultDedupeTTL = 10 * time.Minute

// DefaultAgent is used when AgentFor names none.
const DefaultAgent = "main"

// ErrUnknownSender reports a sender with no linked user. It carries the
// envelope so the access layer (M3) can decide what to do with it.
type ErrUnknownSender struct {
	Envelope event.Envelope
}

func (e *ErrUnknownSender) Error() string {
	return fmt.Sprintf("session: unknown sender on channel %q", e.Envelope.Origin.Channel)
}

// ErrIdentityDisabled reports a sender whose identity is switched off. The
// access gate has dropped these already; API messages skip the gate, and this
// is their last line. Deps.Prepare returns it, wrapped, for a turn whose
// senders were all disabled after their messages were accepted.
var ErrIdentityDisabled = errors.New("session: the identity is disabled")

// MessageAccepted is what routing an envelope produced.
type MessageAccepted struct {
	TraceID   event.ID
	SessionID event.ID
	MessageID event.ID
	Key       string
	Agent     string
	User      store.Identity
	// Duplicate reports a message already seen (platform re-delivery or a
	// repeated message id); no turn was started for it.
	Duplicate bool
}

// Router turns an inbound Envelope into a session submission: trace id,
// access lookup, agent, session key, mailbox (ARCHITECTURE.md §2, docs/sessions.md).
type Router struct {
	// Resolve maps the envelope's origin plus its sender to a user, linking
	// the sender's free aliases to it like store.ResolveAndLink: the router
	// reports Resolution.Free as linked, so a resolver that links nothing
	// leaves it empty. Nil uses store.ResolveAndLink with no namespaces.
	Resolve  func(ctx context.Context, origin event.Origin, sender event.Sender) (store.Resolution, error)
	AgentFor func(channel string) string
	Store    store.Store
	Manager  *Manager
	Events   event.Sink
	// Logger receives alias conflicts. Nil uses slog.Default.
	Logger *slog.Logger
	// DedupeTTL overrides DefaultDedupeTTL.
	DedupeTTL time.Duration
	Now       func() time.Time
	// Sent finds the bot message a reaction targets. Nil drops reactions.
	Sent store.SentStore
	// OnApprovalReaction answers a reaction to an approval prompt. The router
	// has only resolved the sender; the callee checks the role and the emoji.
	// Nil drops those reactions.
	OnApprovalReaction func(ctx context.Context, env event.Envelope, sent store.Sent)

	mu   sync.Mutex
	seen map[string]time.Time
}

func (r *Router) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Router) emit(ctx context.Context, e event.Event) {
	if r.Events == nil {
		return
	}
	r.Events.Emit(ctx, e)
}

// Route resolves the envelope and hands the message to the session actor.
func (r *Router) Route(ctx context.Context, env event.Envelope) (MessageAccepted, error) {
	return r.route(ctx, env, nil)
}

// RouteTo is Route into a session the client already holds. The session's
// own agent answers: re-deriving it from the channel would open a sibling
// session under another agent.
func (r *Router) RouteTo(ctx context.Context, env event.Envelope, sess store.Session) (MessageAccepted, error) {
	return r.route(ctx, env, &sess)
}

func (r *Router) route(ctx context.Context, env event.Envelope, target *store.Session) (MessageAccepted, error) {
	if env.TraceID == "" {
		env.TraceID = event.NewID()
	}
	o := env.Origin
	acc := MessageAccepted{TraceID: env.TraceID}
	recv := map[string]any{
		"channel":           o.Channel,
		"channel_type":      o.ChannelType,
		"conversation":      o.Conversation,
		"conversation_kind": string(o.ConversationKind),
		"thread":            o.Thread,
		"platform_msg_id":   o.PlatformMsgID,
		"mentioned":         env.Mentioned,
	}
	if env.Reaction != nil {
		recv["reaction"] = true
	}
	if env.Sender.Token != "" {
		recv["token"] = string(env.Sender.Token)
	}
	r.emit(ctx, event.New(event.ChannelReceived, env.TraceID, "", "", recv))

	// A client-supplied message id is an idempotency key only. The stored id
	// is always server-generated: transcripts are ordered by id, and a client
	// with a skewed clock must not be able to reorder a conversation.
	do := o
	if do.PlatformMsgID == "" && env.Message.ID != "" {
		do.PlatformMsgID = "client:" + string(env.Message.ID)
	}
	dedupeKey, fresh := r.mark(do)
	if !fresh {
		acc.Duplicate = true
		r.emit(ctx, event.New(event.RouterDropped, env.TraceID, "", "", map[string]any{
			"reason":  "duplicate",
			"channel": o.Channel,
		}))
		return acc, nil
	}
	// A failed route must not swallow a retry of the same message.
	ok := false
	defer func() {
		if !ok {
			r.unmark(dedupeKey)
		}
	}()

	res, err := r.resolve(ctx, o, env.Sender)
	if err == nil && res.Identity.Disabled() {
		r.emit(ctx, event.New(event.RouterDropped, env.TraceID, "", "", map[string]any{
			"reason":  "identity_disabled",
			"channel": o.Channel,
		}))
		return acc, ErrIdentityDisabled
	}
	r.reportLinks(ctx, env, res, err)
	identity := res.Identity
	if err != nil {
		reason := "resolve_error"
		if errors.Is(err, store.ErrNotFound) {
			reason = "unknown_sender"
		}
		r.emit(ctx, event.New(event.RouterDropped, env.TraceID, "", "", map[string]any{
			"reason":  reason,
			"channel": o.Channel,
		}))
		if reason == "unknown_sender" {
			return acc, &ErrUnknownSender{Envelope: env}
		}
		return acc, fmt.Errorf("session: resolve sender: %w", err)
	}
	acc.User = identity
	if env.Reaction != nil {
		if err := r.reaction(ctx, env, identity, &acc); err != nil {
			return acc, err
		}
		ok = true
		return acc, nil
	}

	var sess store.Session
	if target != nil {
		sess = *target
	} else {
		agentName := DefaultAgent
		if r.AgentFor != nil {
			if n := r.AgentFor(o.Channel); n != "" {
				agentName = n
			}
		}
		// A room's session belongs to the room identity, whoever spoke; only
		// the message carries the sender.
		owner := identity.ID
		if o.ConversationKind == event.ConversationRoom {
			room, dropped, err := r.room(ctx, env)
			if err != nil {
				return acc, err
			}
			if dropped {
				ok = true
				return acc, nil
			}
			owner = room.ID
		}
		sess, err = Ensure(ctx, r.Store, agentName, o, owner, env.TraceID)
		if err != nil {
			return acc, err
		}
	}
	key, agentName := sess.Key, sess.Agent
	acc.SessionID, acc.Key, acc.Agent, acc.User = sess.ID, key, agentName, identity

	msg := r.message(env, sess, identity)
	acc.MessageID = msg.ID
	r.emit(ctx, event.New(event.RouterResolved, env.TraceID, sess.ID, "", map[string]any{
		"user":    string(identity.ID),
		"role":    roleLabel(identity),
		"agent":   agentName,
		"session": string(sess.ID),
		"key":     key,
	}))

	dup, err := r.Manager.Submit(ctx, sess, msg)
	if err != nil {
		return acc, err
	}
	acc.Duplicate = dup
	ok = true
	return acc, nil
}

// Ensure returns the session of a conversation, the one way a session comes
// to exist: seeded when it is a new thread, and owned by owner, a DM's user or a
// room's identity. A room identity that was deleted left its sessions ownerless
// under the same keys, so the room's next identity adopts them.
func Ensure(ctx context.Context, st store.Store, agentName string, o event.Origin, owner, traceID event.ID) (store.Session, error) {
	sess, created, err := st.EnsureSession(ctx, store.Session{
		Key: Key(agentName, o, owner), Agent: agentName, Kind: store.SessionChat, UserID: owner, Origin: o,
	})
	if err != nil {
		return store.Session{}, fmt.Errorf("session: ensure session: %w", err)
	}
	if created {
		if err := SeedThread(ctx, st, sess, o, traceID); err != nil {
			return store.Session{}, err
		}
	}
	if sess.UserID == "" && o.ConversationKind == event.ConversationRoom {
		if sess, err = st.AdoptSession(ctx, sess.ID, owner); err != nil {
			return store.Session{}, fmt.Errorf("session: adopt session: %w", err)
		}
	}
	return sess, nil
}

// room finds the room identity of an envelope, and reports a drop for a room
// disabled since the gate let it in and for a conversation a user's binding
// holds, which is never adopted.
func (r *Router) room(ctx context.Context, env event.Envelope) (store.Identity, bool, error) {
	o := env.Origin
	room, created, err := r.Store.EnsureRoom(ctx, o.Channel, o.Conversation, r.now())
	reason := ""
	switch {
	case errors.Is(err, store.ErrAliasLinked):
		reason = "room_binding_held"
		log := r.Logger
		if log == nil {
			log = slog.Default()
		}
		log.Warn("room dropped: a user identity holds its binding",
			"trace", env.TraceID, "channel", o.Channel, "conversation", o.Conversation)
	case err != nil:
		return store.Identity{}, false, fmt.Errorf("session: room: %w", err)
	case room.Disabled():
		reason = "room_disabled"
	default:
		if created {
			r.emit(ctx, event.New(event.IdentityCreated, env.TraceID, "", "", map[string]any{
				"identity": room.ID, "kind": room.Kind, "status": room.Status,
				"bindings": []string{o.Channel + ":" + o.Conversation},
			}))
		}
		return room, false, nil
	}
	r.emit(ctx, event.New(event.RouterDropped, env.TraceID, "", "", map[string]any{
		"reason": reason, "channel": o.Channel, "conversation": o.Conversation,
	}))
	return store.Identity{}, true, nil
}

// message builds the transcript entry. The sender label is carried only for
// room conversations, raw: the context builder sanitises it, so a display
// name cannot forge a prompt section (docs/sessions.md).
//
// UserID is the resolved sender, stamped on every inbound message: a room's
// session is shared by everyone in it and owned by the room, so the turn's
// policy role comes from the message, not from the session row (docs/sessions.md, docs/security.md).
func (r *Router) message(env event.Envelope, sess store.Session, identity store.Identity) event.Message {
	m := env.Message
	m.Schema = event.SchemaVersion
	m.ID = event.NewID()
	m.SessionID = sess.ID
	m.TraceID = env.TraceID
	m.Role = event.RoleUser
	m.UserID = identity.ID
	m.Mentioned = env.Mentioned
	m.Sender = ""
	if env.Origin.ConversationKind == event.ConversationRoom {
		m.Sender = env.Sender.DisplayName
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = env.Received
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = r.now()
	}
	return m
}

// reactionPreview is how much of the bot message a reaction line quotes.
const reactionPreview = 80

// reaction routes a resolved sender's reaction by its target: an approval
// prompt goes to OnApprovalReaction; another bot message gets a line in its
// session, "[👍 on: <the message's start>]", which starts no turn, so the
// agent does not answer every thumbs-up and the next turn sees it. Removing
// a reaction from a bot message is dropped: the line already told the story.
// Anything else is dropped.
func (r *Router) reaction(ctx context.Context, env event.Envelope, identity store.Identity, acc *MessageAccepted) error {
	o, re := env.Origin, env.Reaction
	if r.Sent == nil || re.TargetMsgID == "" {
		r.dropReaction(ctx, env, "reaction_target")
		return nil
	}
	// By the target id alone, whole: adapter ids carry their conversation,
	// and a reaction may carry no thread (Telegram topics).
	sent, err := r.Sent.GetSent(ctx, o.Channel, re.TargetMsgID)
	if errors.Is(err, store.ErrNotFound) {
		r.dropReaction(ctx, env, "reaction_target")
		return nil
	}
	if err != nil {
		return fmt.Errorf("session: reaction target: %w", err)
	}
	// Defence in depth against an adapter whose ids are not unique: the
	// message must have been sent to the conversation the reaction is in.
	// The thread is not compared.
	if sent.Conversation != o.Conversation {
		r.dropReaction(ctx, env, "reaction_target")
		return nil
	}
	acc.SessionID = sent.SessionID
	if sent.ApprovalID != "" {
		if r.OnApprovalReaction != nil {
			r.OnApprovalReaction(ctx, env, sent)
		}
		return nil
	}
	if re.Removed {
		r.dropReaction(ctx, env, "reaction_removed")
		return nil
	}
	// The emoji is a sender's text going into the context.
	e := emoji.Clean(re.Emoji)
	if e == "" {
		r.dropReaction(ctx, env, "reaction_emoji")
		return nil
	}
	target, err := r.Store.GetMessage(ctx, sent.MessageID)
	if errors.Is(err, store.ErrNotFound) {
		r.dropReaction(ctx, env, "reaction_target")
		return nil
	}
	if err != nil {
		return fmt.Errorf("session: reaction target: %w", err)
	}
	quoted := strings.Join(strings.Fields(target.Text), " ")
	if q := []rune(quoted); len(q) > reactionPreview {
		quoted = string(q[:reactionPreview])
	}
	if quoted == "" {
		quoted = "(attachment)"
	}
	m := event.Message{
		Schema: event.SchemaVersion, ID: event.NewID(), SessionID: sent.SessionID, TraceID: env.TraceID,
		Role: event.RoleUser, UserID: identity.ID, Text: "[" + e + " on: " + quoted + "]",
		CreatedAt: env.Received,
	}
	if o.ConversationKind == event.ConversationRoom {
		m.Sender = env.Sender.DisplayName
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = r.now()
	}
	if _, err := r.Store.AppendMessage(ctx, m, false); err != nil {
		return fmt.Errorf("session: store reaction: %w", err)
	}
	acc.MessageID = m.ID
	r.emit(ctx, event.New(event.RouterResolved, env.TraceID, sent.SessionID, "", map[string]any{
		"user": string(identity.ID), "role": roleLabel(identity), "session": string(sent.SessionID), "stored_only": true,
	}))
	return nil
}

func (r *Router) dropReaction(ctx context.Context, env event.Envelope, reason string) {
	r.emit(ctx, event.New(event.RouterDropped, env.TraceID, "", "", map[string]any{
		"reason": reason, "channel": env.Origin.Channel,
	}))
}

func (r *Router) resolve(ctx context.Context, origin event.Origin, sender event.Sender) (store.Resolution, error) {
	if r.Resolve != nil {
		return r.Resolve(ctx, origin, sender)
	}
	return r.Store.ResolveAndLink(ctx, origin.Channel, sender.Aliases, agent.SanitizeDisplay(sender.DisplayName), nil)
}

// reportLinks records what resolving linked, one identity.bound per alias, and
// logs the aliases left alone. Ids only: no display name, no text.
func (r *Router) reportLinks(ctx context.Context, env event.Envelope, res store.Resolution, err error) {
	log := r.Logger
	if log == nil {
		log = slog.Default()
	}
	ch := env.Origin.Channel
	for _, c := range res.Conflicts {
		if c.Held != "" {
			log.Warn("alias match not trusted: the identity holds another id in its namespace",
				"trace", env.TraceID, "channel", ch, "user", c.IdentityID, "alias", c.Alias, "held", c.Held)
			continue
		}
		log.Warn("alias left alone: linked to another identity",
			"trace", env.TraceID, "channel", ch, "user", c.IdentityID, "alias", c.Alias, "resolved", res.Identity.ID)
	}
	if err != nil || res.Identity.ID == "" {
		return
	}
	for _, a := range res.Free {
		r.emit(ctx, event.New(event.IdentityBound, env.TraceID, "", "", map[string]any{
			"identity": string(res.Identity.ID), "binding": ch + ":" + a, "matched": res.Matched,
		}))
	}
}

// mark records a platform message id and reports whether it is new. Messages
// without one are never deduped here; Submit still dedupes on message id.
func (r *Router) mark(o event.Origin) (string, bool) {
	if o.PlatformMsgID == "" {
		return "", true
	}
	ttl := r.DedupeTTL
	if ttl <= 0 {
		ttl = DefaultDedupeTTL
	}
	key := o.Channel + "\x00" + o.PlatformMsgID
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = map[string]time.Time{}
	}
	for k, t := range r.seen {
		if now.Sub(t) > ttl {
			delete(r.seen, k)
		}
	}
	if t, ok := r.seen[key]; ok && now.Sub(t) <= ttl {
		return key, false
	}
	r.seen[key] = now
	return key, true
}

func (r *Router) unmark(key string) {
	if key == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.seen, key)
}

// roleLabel is the role the router.resolved event reports: a pending identity
// has none and runs as a guest.
func roleLabel(i store.Identity) string {
	if i.Pending() {
		return string(store.RoleGuest)
	}
	return string(i.Role)
}
