package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go.mau.fi/whatsmeow/types"

	"holaclaw/internal/event"
)

// React puts emoji on the message target.PlatformMsgID ("<chat JID>/<id>"),
// or takes the reaction off when emoji is empty. WhatsApp wants the message's
// sender in a group, which the adapter remembers for every message it stored
// or sent; a message it does not know there cannot be reacted to (§5.1).
func (a *Adapter) React(ctx context.Context, target event.Origin, emoji string) error {
	cut := strings.LastIndexByte(target.PlatformMsgID, '/')
	if cut <= 0 || cut == len(target.PlatformMsgID)-1 || target.PlatformMsgID[:cut] != target.Conversation {
		return fmt.Errorf("whatsapp: bad reaction target %q", target.PlatformMsgID)
	}
	chat, err := types.ParseJID(target.Conversation)
	if err != nil || chat.User == "" ||
		chat.Server != types.DefaultUserServer && chat.Server != types.HiddenUserServer && chat.Server != types.GroupServer {
		return fmt.Errorf("whatsapp: bad target conversation %q", target.Conversation)
	}
	id := target.PlatformMsgID[cut+1:]
	cli := a.currentClient()
	if cli == nil {
		return errors.New("whatsapp: the channel is not running")
	}
	a.stateMu.Lock()
	author, known := a.authors[target.PlatformMsgID]
	a.stateMu.Unlock()
	if !known {
		if chat.Server == types.GroupServer {
			return fmt.Errorf("whatsapp: sender of %q unknown, cannot react in a group", target.PlatformMsgID)
		}
		author = chat // in a DM an unknown message is the peer's
	}
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if _, err := cli.SendMessage(ctx, chat, cli.BuildReaction(chat, author, id, emoji)); err != nil {
		return fmt.Errorf("whatsapp: react: %w", err)
	}
	return nil
}
