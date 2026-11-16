// Package access is the gate every channel adapter hands its envelopes to: it
// decides who may talk at all — DM knob, rooms knob, reply mode — and only
// then calls the router. It runs before any run exists (docs/identity.md, docs/security.md).
//
// The gate never blocks on a human: a stranger comes in with an invite, or as a
// pending identity where the knobs are open. Inbound returns nil for everything it handled or dropped, so an adapter advances its
// offset, channel.ErrDuplicate for a message it already handled, and any
// other error only for transient failures (database, routing) worth a
// redelivery.
package access

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"holaclaw/internal/auth"
	"holaclaw/internal/channel"
	"holaclaw/internal/config"
	"holaclaw/internal/event"
	"holaclaw/internal/session"
	"holaclaw/internal/store"
)

// Flood guards. Every one of them protects the database from an unknown
// sender: a stranger must not be able to fill the event log or the identity
// table by sending in a loop.
const (
	// noticeEvery is how often one alias may be told it is not welcome.
	noticeEvery = 24 * time.Hour
	// dropEvents is how many router.dropped events one alias may produce per
	// dropWindow; beyond that the message is dropped without a trace.
	dropEvents = 20
	dropWindow = time.Hour
	// guestBurst is how many messages one guest alias may send per
	// guestWindow in open mode.
	guestBurst  = 20
	guestWindow = 10 * time.Minute
	// roomDropEvery is how often a disabled or unapproved room produces a drop
	// event.
	roomDropEvery = time.Hour
)

// Notices the gate sends. Short, identical for every failure of a kind: a
// stranger learns nothing about who is known or which codes exist.
const (
	noticeLinked     = "You are linked. Say hi!"
	noticeBadCode    = "That code is not valid."
	noticeInviteOnly = "This assistant is invite-only."
	noticeTooFast    = "Too many messages. Try again later."
	noticeRoomOn     = "This room is approved. Say hi!"
)

// Deps are the collaborators of a Gate. Notify may be nil.
type Deps struct {
	// Config returns the access knobs of a channel instance; false means the
	// channel is unknown and its envelopes are dropped.
	Config func(channel string) (config.Access, bool)
	Auth   *auth.Service
	// Route is session.Router.Route.
	Route func(ctx context.Context, env event.Envelope) (session.MessageAccepted, error)
	// Store holds the room identities, and persists room messages that start no
	// turn.
	Store store.Store
	// Notify sends a short system reply to the sender; the daemon delivers it
	// through the adapter. Nil disables notices.
	Notify func(ctx context.Context, target event.Origin, text string)
	// Agent names the agent of a channel instance, for the session key of a
	// stored room message. Empty or nil means session.DefaultAgent.
	Agent func(channel string) string
	// Dedupe records the messages handled, so one that comes back is dropped
	// (#101). Nil means no durable dedupe.
	Dedupe store.InboundStore
	Events event.Sink
	Now    func() time.Time
}

// Gate implements channel.Inbound.
type Gate struct {
	d Deps

	notices *limiter // one notice per (channel, alias) per noticeEvery
	drops   *limiter // drop events per alias
	guests  *limiter // guest messages per alias
	stored  *limiter // unmentioned messages appended per room

	roomDrops   *limiter // drop events per room
	roomCreates *limiter // rooms created per alias
}

// New builds a Gate.
func New(d Deps) *Gate {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Events == nil {
		d.Events = event.Discard
	}
	return &Gate{
		d: d, notices: newLimiter(), drops: newLimiter(),
		guests: newLimiter(), stored: newLimiter(),
		roomDrops: newLimiter(), roomCreates: newLimiter(),
	}
}

func (g *Gate) now() time.Time { return g.d.Now() }

// Inbound decides whether an envelope may reach the router (channel.Inbound).
func (g *Gate) Inbound(ctx context.Context, env event.Envelope) error {
	// Our own messages come back on some platforms; they are not input.
	if env.Sender.FromSelf {
		return nil
	}
	if env.TraceID == "" {
		env.TraceID = event.NewID()
	}
	acc, ok := g.d.Config(env.Origin.Channel)
	if !ok {
		g.emitDrop(ctx, env, "unknown_channel")
		return nil
	}
	keys := dedupeKeys(env)
	if g.d.Dedupe != nil && len(keys) > 0 {
		fresh, err := g.d.Dedupe.ClaimInbound(ctx, env.Origin.Channel, keys, g.now())
		if err != nil {
			return fmt.Errorf("access: claim inbound: %w", err)
		}
		if !fresh {
			g.emitDrop(ctx, env, "duplicate")
			return channel.ErrDuplicate
		}
	}
	var err error
	switch {
	case env.Reaction != nil:
		err = g.reaction(ctx, env, acc)
	case env.Origin.ConversationKind == event.ConversationRoom:
		err = g.room(ctx, env, acc)
	default:
		err = g.dm(ctx, env, acc)
	}
	if err != nil && g.d.Dedupe != nil && len(keys) > 0 {
		// The adapter redelivers on an error, and the redelivery must not
		// look handled. A failed release only costs that redelivery; the
		// original error is the one worth returning.
		_ = g.d.Dedupe.ReleaseInbound(context.WithoutCancel(ctx), env.Origin.Channel, keys)
	}
	return err
}

// dedupeKeys are the keys that identify an envelope across resends: the
// adapter's, else the platform message id, else none.
func dedupeKeys(env event.Envelope) []string {
	if len(env.DedupeKeys) > 0 {
		return env.DedupeKeys
	}
	if env.Origin.PlatformMsgID != "" {
		return []string{env.Origin.PlatformMsgID}
	}
	return nil
}

// dm applies the dm knob. A known sender always routes, a disabled one never,
// and an invite code is never routed, whoever sends it; the knob only decides
// what happens to a stranger.
func (g *Gate) dm(ctx context.Context, env event.Envelope, acc config.Access) error {
	identity, err := g.d.Auth.Resolve(ctx, env.Origin.Channel, env.Sender.Aliases)
	switch {
	case err == nil:
		if identity.Disabled() {
			g.emitDrop(ctx, env, "identity_disabled")
			return nil
		}
		if done, err := g.redeemKnown(ctx, env); done || err != nil {
			return err
		}
		if acc.DM == "open" && identity.Pending() && !g.allowGuest(ctx, env) {
			return nil
		}
		return g.route(ctx, env)
	case !errors.Is(err, store.ErrNotFound):
		return fmt.Errorf("access: resolve sender: %w", err)
	}

	switch acc.DM {
	case "open":
		if !g.allowGuest(ctx, env) {
			return nil
		}
		if _, err := g.d.Auth.EnsureGuest(ctx, env.Origin.Channel,
			env.Sender.Aliases, env.Sender.DisplayName); errors.Is(err, store.ErrAliasLinked) {
			// Every alias is held by identities the match did not trust.
			g.dropUnknown(ctx, env, false)
			return nil
		} else if err != nil {
			return fmt.Errorf("access: ensure guest: %w", err)
		}
		return g.route(ctx, env)
	default: // invite
		if code, found := inviteCode(env.Message.Text); found {
			return g.redeem(ctx, env, code)
		}
		g.dropUnknown(ctx, env, true)
		return nil
	}
}

// reaction lets a known sender's reaction reach the router, in a DM or in an
// approved room. It never redeems, makes a guest, creates a room or
// sends a notice: a reaction is no way to introduce oneself, so a stranger's is
// dropped quietly, as is a disabled sender's. A guest's counts against the
// guest limit, silently.
func (g *Gate) reaction(ctx context.Context, env event.Envelope, acc config.Access) error {
	inRoom := env.Origin.ConversationKind == event.ConversationRoom
	if inRoom {
		room, ok, err := g.lookupRoom(ctx, env, acc)
		if !ok || err != nil {
			return err
		}
		if acc.Rooms == "approved" && !room.Approved() {
			g.dropRoom(ctx, env, "room_not_approved")
			return nil
		}
	}
	identity, err := g.d.Auth.Resolve(ctx, env.Origin.Channel, env.Sender.Aliases)
	switch {
	case errors.Is(err, store.ErrNotFound):
		g.emitDrop(ctx, env, "unknown_sender")
		return nil
	case err != nil:
		return fmt.Errorf("access: resolve reactor: %w", err)
	case identity.Disabled():
		g.emitDrop(ctx, env, "identity_disabled")
		return nil
	}
	if identity.Pending() && !g.allowRoomGuest(ctx, env) {
		return nil
	}
	return g.route(ctx, env)
}

// redeem consumes an invite code. The message carrying the code is never
// routed: it is a credential, not a turn.
func (g *Gate) redeem(ctx context.Context, env event.Envelope, code string) error {
	_, err := g.d.Auth.RedeemInvite(ctx, code, env.Origin.Channel, env.Sender.Aliases, env.Sender.DisplayName)
	return g.answerRedeem(ctx, env, err)
}

// answerRedeem tells the sender how their code went.
func (g *Gate) answerRedeem(ctx context.Context, env event.Envelope, err error) error {
	switch {
	case err == nil:
		g.notify(ctx, env.Origin, noticeLinked)
		return nil
	case errors.Is(err, auth.ErrRateLimited):
		// Too many wrong codes: silence, so guessing costs nothing to answer.
		g.emitDrop(ctx, env, "rate_limited")
		return nil
	case errors.Is(err, auth.ErrInviteInvalid), errors.Is(err, auth.ErrInviteChannel),
		errors.Is(err, store.ErrAliasLinked):
		g.emitDrop(ctx, env, "invite_invalid")
		g.notify(ctx, env.Origin, noticeBadCode)
		return nil
	default:
		return fmt.Errorf("access: redeem invite: %w", err)
	}
}

// redeemKnown handles a code from a sender who already has an identity. A
// pending one redeems it: auth moves the guest onto the invited identity. A
// confirmed one is refused (store.ErrAliasLinked), and the code is still kept
// out of the transcript. A known sender is already talking, so only "/start
// <code>" is surely a code; a bare code-shaped word that is no code is an
// ordinary message and routes (done is false). Codes for another channel are
// refused like a stranger's.
func (g *Gate) redeemKnown(ctx context.Context, env event.Envelope) (done bool, err error) {
	code, found := inviteCode(env.Message.Text)
	if !found {
		return false, nil
	}
	_, err = g.d.Auth.RedeemInvite(ctx, code, env.Origin.Channel, env.Sender.Aliases, env.Sender.DisplayName)
	bare := !isStart(strings.Fields(env.Message.Text)[0])
	// Rate limited, the code was never checked: it may be live, so it is not
	// routed into the transcript.
	if bare && errors.Is(err, auth.ErrInviteInvalid) {
		return false, nil
	}
	return true, g.answerRedeem(ctx, env, err)
}

// dropUnknown traces a dropped stranger and, in invite mode, tells them once a
// day that this is invite-only. Neither the event nor the notice carries the
// message text.
func (g *Gate) dropUnknown(ctx context.Context, env event.Envelope, notice bool) {
	g.emitDrop(ctx, env, "unknown_sender")
	if notice && g.notices.allow(aliasKeys(env), g.now(), 1, noticeEvery) {
		g.notify(ctx, env.Origin, noticeInviteOnly)
	}
}

// allowGuest applies the open-mode message limit, with one notice per window.
func (g *Gate) allowGuest(ctx context.Context, env event.Envelope) bool {
	if g.guests.allow(aliasKeys(env), g.now(), guestBurst, guestWindow) {
		return true
	}
	g.emitDrop(ctx, env, "rate_limited")
	if g.notices.allow(noticeKeys(env, "fast"), g.now(), 1, guestWindow) {
		g.notify(ctx, env.Origin, noticeTooFast)
	}
	return false
}

// route hands the envelope to the router. An unknown sender there means the
// identity vanished between our lookup and the route: drop, do not redeliver.
func (g *Gate) route(ctx context.Context, env event.Envelope) error {
	_, err := g.d.Route(ctx, env)
	if err == nil {
		return nil
	}
	var unknown *session.ErrUnknownSender
	if errors.As(err, &unknown) {
		g.emitDrop(ctx, env, "unknown_sender")
		return nil
	}
	if errors.Is(err, session.ErrIdentityDisabled) {
		// Disabled since our lookup; the router traced the drop.
		return nil
	}
	return err
}

func (g *Gate) notify(ctx context.Context, target event.Origin, text string) {
	if g.d.Notify == nil {
		return
	}
	g.d.Notify(ctx, target, text)
}

// emitDrop records a dropped envelope, capped per alias so a flood cannot fill
// the event table. Attributes never carry the message text.
func (g *Gate) emitDrop(ctx context.Context, env event.Envelope, reason string) {
	if !g.drops.allow(aliasKeys(env), g.now(), dropEvents, dropWindow) {
		return
	}
	g.d.Events.Emit(ctx, event.New(event.RouterDropped, env.TraceID, "", "", map[string]any{
		"reason":            reason,
		"channel":           env.Origin.Channel,
		"conversation_kind": string(env.Origin.ConversationKind),
	}))
}

// aliasKeys are the rate-limit keys of a sender: one per alias, per channel.
func aliasKeys(env event.Envelope) []string {
	return keysFor(env, "")
}

func noticeKeys(env event.Envelope, tag string) []string { return keysFor(env, tag) }

func keysFor(env event.Envelope, tag string) []string {
	if len(env.Sender.Aliases) == 0 {
		return []string{env.Origin.Channel + "\x00" + tag + "\x00" + env.Origin.Conversation}
	}
	out := make([]string, 0, len(env.Sender.Aliases))
	for _, a := range env.Sender.Aliases {
		out = append(out, env.Origin.Channel+"\x00"+tag+"\x00"+a)
	}
	return out
}

// inviteCode extracts a code from a first message: "/start <code>" (the
// Telegram deep link) or a bare code-shaped token. Anything else is not a
// credential.
func inviteCode(text string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(text))
	switch {
	case len(fields) == 2 && isStart(fields[0]):
		return codeShaped(fields[1])
	case len(fields) == 1 && !isStart(fields[0]):
		return codeShaped(fields[0])
	}
	return "", false
}

// isStart matches /start and the "/start@botname" form Telegram groups use.
func isStart(s string) bool {
	s = strings.ToLower(s)
	return s == "/start" || strings.HasPrefix(s, "/start@")
}

// crockford is the invite alphabet: no I, L, O or U (auth mints codes in it).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// codeShaped reports whether a token can be an invite code, normalising it the
// way auth does before hashing: case, separators and the letters Crockford
// maps onto digits.
func codeShaped(tok string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(tok) {
		switch r {
		case '-', '_':
		case 'I', 'L':
			b.WriteRune('1')
		case 'O':
			b.WriteRune('0')
		default:
			b.WriteRune(r)
		}
	}
	norm := b.String()
	if len(norm) != codeLen {
		return "", false
	}
	for _, r := range norm {
		if !strings.ContainsRune(crockford, r) {
			return "", false
		}
	}
	return norm, true
}

// codeLen mirrors the generated code length in auth.
const codeLen = 10
