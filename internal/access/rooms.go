package access

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"holaclaw/internal/auth"
	"holaclaw/internal/config"
	"holaclaw/internal/event"
	"holaclaw/internal/session"
	"holaclaw/internal/store"
)

// RoomMessageNumberWindow caps how many stored room messages the next turn
// sees. A room transcript grows between mentions without any turn reading it,
// so the window bounds the prompt, not the transcript: rows stay immutable
// (docs/sessions.md).
const RoomMessageNumberWindow = 30

// Flood guards on the room path. A room is a place strangers can reach
// without being invited, so the same rate limits the DM path applies to an
// unknown sender apply here, and the transcript a room fills between mentions
// is capped too (docs/identity.md).
const (
	// roomStoreCap is how many unmentioned messages one conversation may
	// append per roomStoreWindow. Beyond it they are dropped, with one
	// router.dropped per conversation and window.
	roomStoreCap    = 120
	roomStoreWindow = time.Hour
	// roomBurst is how many rooms one alias may bring into being per
	// roomWindow under rooms = "open": a stranger adding the bot to many rooms
	// must not fill the identities table.
	roomBurst  = 10
	roomWindow = time.Hour
)

// room handles a message in a room: it resolves twice, the room first and then
// the sender (docs/identity.md §1), and only then applies the reply knob. A rule
// on who may talk in a room goes between the two steps.
func (g *Gate) room(ctx context.Context, env event.Envelope, acc config.Access) error {
	room, ok, err := g.resolveRoom(ctx, env, acc)
	if !ok || err != nil {
		return err
	}
	identity, ok, err := g.resolveRoomSender(ctx, env)
	if !ok || err != nil {
		return err
	}
	if acc.Reply == "always" || env.Mentioned {
		return g.route(ctx, env)
	}
	// Unmentioned messages start no turn but do grow the transcript, so a
	// busy room is capped per conversation rather than per sender.
	key := env.Origin.Channel + "\x00" + env.Origin.Conversation
	if !g.stored.allow([]string{key}, g.now(), roomStoreCap, roomStoreWindow) {
		g.dropRoom(ctx, env, "room_flood")
		return nil
	}
	return g.store(ctx, env, room, identity)
}

// resolveRoom is the room step: it finds the room identity, or makes it when the
// message is the one that brings the room in, and reports whether the message
// goes on. A room's state decides, never its sender's role: the sender is only
// asked here whether they approve it, and a disabled one brings in or approves
// nothing. Anything else under "approved" creates nothing, so a room's chatter
// can't fill the identities table, and a room that came in under "open" isn't
// approved when the knob flips: it fails closed.
func (g *Gate) resolveRoom(ctx context.Context, env event.Envelope, acc config.Access) (store.Identity, bool, error) {
	room, ok, err := g.lookupRoom(ctx, env, acc)
	if !ok || err != nil {
		return store.Identity{}, false, err
	}
	if room.Approved() || (acc.Rooms == "open" && room.ID != "") {
		return room, true, nil
	}
	// What comes next brings the room in or approves it, and a disabled sender
	// can do neither, with a code or without.
	switch i, err := g.d.Auth.Resolve(ctx, env.Origin.Channel, env.Sender.Aliases); {
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return store.Identity{}, false, fmt.Errorf("access: resolve room sender: %w", err)
	case err == nil && i.Disabled():
		g.emitDrop(ctx, env, "identity_disabled")
		return store.Identity{}, false, nil
	}
	if acc.Rooms == "open" {
		if !g.roomCreates.allow(aliasKeys(env), g.now(), roomBurst, roomWindow) {
			g.emitDrop(ctx, env, "rate_limited")
			return store.Identity{}, false, nil
		}
		room, created, err := g.d.Store.EnsureRoom(ctx, env.Origin.Channel, env.Origin.Conversation, g.now())
		if err != nil {
			if g.refuseRoom(ctx, env, err) {
				return store.Identity{}, false, nil
			}
			return store.Identity{}, false, fmt.Errorf("access: ensure room: %w", err)
		}
		if created {
			g.emitRoomCreated(ctx, env, room)
		}
		return room, true, nil
	}
	if code, found := roomInviteCode(env); found {
		return store.Identity{}, false, g.redeemRoom(ctx, env, code)
	}
	room, ok, err = g.autoApprove(ctx, env)
	if err != nil {
		return store.Identity{}, false, err
	}
	if !ok {
		g.dropRoom(ctx, env, "room_not_approved")
		return store.Identity{}, false, nil
	}
	return room, true, nil
}

// lookupRoom reads the room of an envelope, zero when it has none yet, and drops
// what no knob can let in: rooms off, a conversation whose binding a user
// holds, and a disabled room, before any invite or auto-approve gets to it.
func (g *Gate) lookupRoom(ctx context.Context, env event.Envelope, acc config.Access) (store.Identity, bool, error) {
	if acc.Rooms != "open" && acc.Rooms != "approved" {
		g.dropRoom(ctx, env, "rooms_disabled")
		return store.Identity{}, false, nil
	}
	room, err := g.d.Store.GetRoom(ctx, env.Origin.Channel, env.Origin.Conversation)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return store.Identity{}, true, nil
	case err != nil:
		if g.refuseRoom(ctx, env, err) {
			return store.Identity{}, false, nil
		}
		return store.Identity{}, false, fmt.Errorf("access: get room: %w", err)
	case room.Disabled():
		g.dropRoom(ctx, env, "room_disabled")
		return store.Identity{}, false, nil
	}
	return room, true, nil
}

// refuseRoom drops the envelope when err is the store refusing the room, and
// reports whether it did. A user holding the conversation's binding is a
// conflict for an admin: the binding is never adopted.
func (g *Gate) refuseRoom(ctx context.Context, env event.Envelope, err error) bool {
	switch {
	case errors.Is(err, store.ErrAliasLinked):
		if g.dropRoom(ctx, env, "room_binding_held") {
			slog.Warn("room dropped: a user identity holds its binding",
				"trace", env.TraceID, "channel", env.Origin.Channel, "conversation", env.Origin.Conversation)
		}
		return true
	case errors.Is(err, store.ErrRoomDisabled):
		g.dropRoom(ctx, env, "room_disabled")
		return true
	}
	return false
}

// resolveRoomSender is the sender step, the usual resolution: a linked user talks
// as themselves, anyone else as a pending identity, in approved rooms too.
// Approving a room is a decision about the room, not about every member of it,
// and a pending identity carries the narrow guest role (session-only context,
// safe tools). A disabled sender is dropped: its bindings are still held, so the
// message never creates a pending identity.
//
// Either way a guest is rate-limited, and an unknown sender is limited before
// EnsureGuest runs: the identities table is not a stranger's to fill.
func (g *Gate) resolveRoomSender(ctx context.Context, env event.Envelope) (store.Identity, bool, error) {
	identity, err := g.d.Auth.Resolve(ctx, env.Origin.Channel, env.Sender.Aliases)
	switch {
	case err == nil:
		if identity.Disabled() {
			g.emitDrop(ctx, env, "identity_disabled")
			return store.Identity{}, false, nil
		}
		if identity.Pending() && !g.allowRoomGuest(ctx, env) {
			return store.Identity{}, false, nil
		}
	case errors.Is(err, store.ErrNotFound):
		if !g.allowRoomGuest(ctx, env) {
			return store.Identity{}, false, nil
		}
		identity, err = g.d.Auth.EnsureGuest(ctx, env.Origin.Channel, env.Sender.Aliases, env.Sender.DisplayName)
		if errors.Is(err, store.ErrAliasLinked) {
			// Every alias is held by identities the match did not trust.
			g.emitDrop(ctx, env, "unknown_sender")
			return store.Identity{}, false, nil
		}
		if err != nil {
			return store.Identity{}, false, fmt.Errorf("access: resolve room sender: %w", err)
		}
	default:
		return store.Identity{}, false, fmt.Errorf("access: resolve room sender: %w", err)
	}
	return identity, true, nil
}

// allowRoomGuest applies the guest message limit inside a room. Unlike the DM
// path it sends no notice: a room is not the place to answer a stranger, and a
// bot that talks back on every throttled message is a flood of its own.
func (g *Gate) allowRoomGuest(ctx context.Context, env event.Envelope) bool {
	if g.guests.allow(aliasKeys(env), g.now(), guestBurst, guestWindow) {
		return true
	}
	g.emitDrop(ctx, env, "rate_limited")
	return false
}

// approvesRooms reports whether a sender's word approves a room: a known owner or
// admin who isn't switched off.
func approvesRooms(i store.Identity) bool {
	return !i.Disabled() && (i.Role == store.RoleOwner || i.Role == store.RoleAdmin)
}

// autoApprove approves a room when a known owner or admin mentions the bot in it
// (docs/identity.md), and reports whether it is approved.
func (g *Gate) autoApprove(ctx context.Context, env event.Envelope) (store.Identity, bool, error) {
	if !env.Mentioned {
		return store.Identity{}, false, nil
	}
	i, err := g.d.Auth.Resolve(ctx, env.Origin.Channel, env.Sender.Aliases)
	if errors.Is(err, store.ErrNotFound) {
		return store.Identity{}, false, nil
	}
	if err != nil {
		return store.Identity{}, false, fmt.Errorf("access: resolve room sender: %w", err)
	}
	if !approvesRooms(i) {
		return store.Identity{}, false, nil
	}
	return g.approveRoom(ctx, env, i.ID)
}

// approveRoom approves the room of an envelope, creating it pending when it has
// none, and reports whether it is approved: a room the store refuses is dropped
// here. Approving never revives a disabled room. by is the approver, kept on the
// room and in the access.granted event.
func (g *Gate) approveRoom(ctx context.Context, env event.Envelope, by event.ID) (store.Identity, bool, error) {
	room, change, err := g.d.Store.ApproveRoom(ctx, env.Origin.Channel, env.Origin.Conversation, by, g.now())
	if err != nil {
		if g.refuseRoom(ctx, env, err) {
			return store.Identity{}, false, nil
		}
		return store.Identity{}, false, fmt.Errorf("access: approve room: %w", err)
	}
	if change.Created {
		g.emitRoomCreated(ctx, env, room)
	}
	if change.Approved {
		g.emitGranted(ctx, env, by, "")
	}
	return room, true, nil
}

// emitRoomCreated records a room identity coming into being, after the commit
// that made it (docs/identity.md §8).
func (g *Gate) emitRoomCreated(ctx context.Context, env event.Envelope, room store.Identity) {
	g.d.Events.Emit(ctx, event.New(event.IdentityCreated, env.TraceID, "", "", map[string]any{
		"identity": room.ID, "kind": room.Kind, "status": room.Status,
		"bindings": []string{env.Origin.Channel + ":" + env.Origin.Conversation},
	}))
}

// emitGranted records a room being approved; invite is set when a code did it.
func (g *Gate) emitGranted(ctx context.Context, env event.Envelope, by, invite event.ID) {
	attrs := map[string]any{
		"channel":           env.Origin.Channel,
		"conversation":      env.Origin.Conversation,
		"conversation_kind": string(event.ConversationRoom),
		"approved_by":       string(by),
		"sender_aliases":    env.Sender.Aliases,
	}
	if invite != "" {
		attrs["invite_id"] = string(invite)
	}
	g.d.Events.Emit(ctx, event.New(event.AccessGranted, env.TraceID, "", "", attrs))
}

// roomInviteCode extracts a room invite from a message in a room that is not
// approved yet: a bare "/start <code>", which the Telegram startgroup link
// sends, or any other code form addressed to the bot. "/start@otherbot <code>"
// and an unaddressed bare code are room chatter.
func roomInviteCode(env event.Envelope) (string, bool) {
	code, ok := inviteCode(env.Message.Text)
	if !ok {
		return "", false
	}
	if !env.Mentioned && !strings.EqualFold(strings.Fields(env.Message.Text)[0], "/start") {
		return "", false
	}
	return code, true
}

// redeemRoom consumes a room invite and approves the room in the same
// transaction, creating it when it has none, with the invite's creator as the
// approver. Like a DM code, the message is a credential and is never routed or
// stored, and the sender stays whoever they were. A wrong code gets no answer:
// the room is not approved, and anyone can add the bot to a room of their own to
// make it talk.
func (g *Gate) redeemRoom(ctx context.Context, env event.Envelope, code string) error {
	var (
		approver event.ID
		room     store.Identity
		change   store.RoomChange
	)
	id, err := g.d.Auth.RedeemRoomInvite(ctx, code, env.Origin.Channel, env.Origin.Conversation,
		env.Sender.Aliases, func(tx *sql.Tx, by event.ID) error {
			approver = by
			var err error
			room, change, err = store.ApproveRoomIn(ctx, tx, env.Origin.Channel, env.Origin.Conversation, by, g.now())
			return err
		})
	switch {
	case err == nil:
		if change.Created {
			g.emitRoomCreated(ctx, env, room)
		}
		// The invite row records when it was spent, not where or by whom.
		g.emitGranted(ctx, env, approver, id)
		g.notify(ctx, env.Origin, noticeRoomOn)
		return nil
	case errors.Is(err, auth.ErrRateLimited):
		g.emitDrop(ctx, env, "rate_limited")
		return nil
	case errors.Is(err, auth.ErrInviteInvalid), errors.Is(err, auth.ErrInviteChannel):
		g.emitDrop(ctx, env, "invite_invalid")
		return nil
	case g.refuseRoom(ctx, env, err):
		return nil
	default:
		return fmt.Errorf("access: redeem room invite: %w", err)
	}
}

// store appends an unmentioned room message to the transcript without
// starting a turn: the next mention answers with the room's context. The
// session is the room's, whoever spoke; the message keeps its sender, whose
// label is stored raw for the context builder to sanitise.
func (g *Gate) store(ctx context.Context, env event.Envelope, room, identity store.Identity) error {
	name := session.DefaultAgent
	if g.d.Agent != nil {
		if n := g.d.Agent(env.Origin.Channel); n != "" {
			name = n
		}
	}
	sess, err := session.Ensure(ctx, g.d.Store, name, env.Origin, room.ID, env.TraceID)
	if err != nil {
		return fmt.Errorf("access: %w", err)
	}
	m := env.Message
	m.Schema = event.SchemaVersion
	m.ID = event.NewID()
	m.SessionID = sess.ID
	m.TraceID = env.TraceID
	m.Role = event.RoleUser
	m.UserID = identity.ID
	m.Sender = env.Sender.DisplayName
	if m.CreatedAt.IsZero() {
		m.CreatedAt = env.Received
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = g.now()
	}
	if _, err := g.d.Store.AppendMessage(ctx, m, false); err != nil {
		return fmt.Errorf("access: store room message: %w", err)
	}
	g.d.Events.Emit(ctx, event.New(event.ChannelReceived, env.TraceID, sess.ID, "", map[string]any{
		"channel":           env.Origin.Channel,
		"conversation":      env.Origin.Conversation,
		"conversation_kind": string(env.Origin.ConversationKind),
		"stored_only":       true,
	}))
	return nil
}

// dropRoom traces a dropped room at most once an hour, per conversation and
// reason: a busy disabled room must not fill the event log. It reports whether
// the drop was traced.
func (g *Gate) dropRoom(ctx context.Context, env event.Envelope, reason string) bool {
	key := env.Origin.Channel + "\x00" + env.Origin.Conversation + "\x00" + reason
	if !g.roomDrops.allow([]string{key}, g.now(), 1, roomDropEvery) {
		return false
	}
	g.d.Events.Emit(ctx, event.New(event.RouterDropped, env.TraceID, "", "", map[string]any{
		"reason":            reason,
		"channel":           env.Origin.Channel,
		"conversation":      env.Origin.Conversation,
		"conversation_kind": string(env.Origin.ConversationKind),
	}))
	return true
}

// WindowStart returns the history cursor for a room session: the id before
// the RoomMessageNumberWindow-th newest message since the last assistant
// reply, or "" when the backlog is short enough to be read whole. The daemon
// passes it to ListMessages, so a room that talked a thousand times between
// mentions costs the next turn a bounded prompt.
func WindowStart(ctx context.Context, st store.Store, sessionID event.ID) (event.ID, error) {
	msgs, err := st.ListMessages(ctx, sessionID, "", 0)
	if err != nil {
		return "", fmt.Errorf("access: window start: %w", err)
	}
	last := -1
	for i, m := range msgs {
		if m.Role == event.RoleAssistant {
			last = i
		}
	}
	if len(msgs)-(last+1) <= RoomMessageNumberWindow {
		return "", nil
	}
	return msgs[len(msgs)-RoomMessageNumberWindow-1].ID, nil
}
