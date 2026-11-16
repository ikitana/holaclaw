package access

import (
	"context"
	"errors"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// OnJoined handles the bot being added to a room; the adapters raise it from a
// platform event that carries no message. by are the aliases of whoever added
// the bot, taken from the platform event the way a sender's are.
//
// With rooms = "approved", an owner or admin linked on this channel instance
// approves the room by adding the bot: the add is the same decision a mention
// from them would be, and the room is told it is on. Anyone else adding the
// bot approves nothing, creates nothing and gets no reply: the room stays off
// until an admin mentions the bot there or a room invite is redeemed in it, and
// the join is traced as a router.dropped. An already approved room, and the open
// and off knobs, need no decision: off is traced, the rest is quiet. A disabled
// room stays off, whoever adds the bot.
func (g *Gate) OnJoined(ctx context.Context, origin event.Origin, by []string) {
	origin.ConversationKind = event.ConversationRoom
	env := event.Envelope{TraceID: event.NewID(), Origin: origin, Sender: event.Sender{Aliases: by}}
	acc, ok := g.d.Config(origin.Channel)
	if !ok {
		g.emitDrop(ctx, env, "unknown_channel")
		return
	}
	if acc.Rooms == "open" {
		return
	}
	room, ok, err := g.lookupRoom(ctx, env, acc)
	if err != nil {
		g.dropRoom(ctx, env, "join_failed")
		return
	}
	if !ok || room.Approved() {
		return
	}

	i, err := g.d.Auth.Resolve(ctx, origin.Channel, by)
	switch {
	case errors.Is(err, store.ErrNotFound) || (err == nil && !approvesRooms(i)):
		// Per alias first, so one stranger adding the bot to many rooms is
		// capped like any other unknown sender.
		if g.drops.allow(aliasKeys(env), g.now(), dropEvents, dropWindow) {
			g.dropRoom(ctx, env, "room_not_approved")
		}
		return
	case err != nil:
		g.dropRoom(ctx, env, "join_failed")
		return
	}
	if _, ok, err := g.approveRoom(ctx, env, i.ID); err != nil {
		g.dropRoom(ctx, env, "join_failed")
		return
	} else if !ok {
		return
	}
	g.notify(ctx, origin, noticeRoomOn)
}
