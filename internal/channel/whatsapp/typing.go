package whatsapp

import (
	"context"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"

	"holaclaw/internal/event"
)

// Typing marks the messages that started this turn read, then shows
// "composing" every 8 s until stop, which sends "paused". Messages that were
// dropped, or that started no turn, stay unread, so whoever holds the phone
// still sees them (§5.1). A failure ends the indicator: it is decoration, not
// delivery.
func (a *Adapter) Typing(ctx context.Context, target event.Origin) func() {
	jid, err := types.ParseJID(target.Conversation)
	cli := a.currentClient()
	if err != nil || cli == nil {
		return func() {}
	}
	tctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var composed bool
	go func() {
		defer close(done)
		a.markRead(tctx, cli, target.Conversation)
		for {
			if err := cli.SendChatPresence(tctx, jid, types.ChatPresenceComposing, types.ChatPresenceMediaText); err != nil {
				if tctx.Err() == nil {
					a.log.Warn("whatsapp typing indicator failed", "channel", a.name, "error", err)
				}
				return
			}
			composed = true
			select {
			case <-tctx.Done():
				return
			case <-a.after(typingInterval):
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
			if !composed {
				return
			}
			pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer pcancel()
			if err := cli.SendChatPresence(pctx, jid, types.ChatPresencePaused, types.ChatPresenceMediaText); err != nil {
				a.log.Warn("whatsapp typing indicator failed", "channel", a.name, "error", err)
			}
		})
	}
}

// markRead sends read receipts for the stored messages of conv that may have
// started this turn, one receipt per sender as WhatsApp requires.
func (a *Adapter) markRead(ctx context.Context, cli waClient, conv string) {
	refs := a.takeUnread(conv)
	for len(refs) > 0 {
		head := refs[0]
		var ids []types.MessageID
		var rest []readRef
		for _, r := range refs {
			if r.sender == head.sender && r.chat == head.chat {
				ids = append(ids, r.id)
			} else {
				rest = append(rest, r)
			}
		}
		// A turn started for this sender, so the gate accepted them: a later
		// call from the same id gets more than a silent reject (§7).
		a.rememberPeer(ctx, head.sender, head.alt)
		if err := cli.MarkRead(ctx, ids, a.now(), head.chat, head.sender); err != nil && ctx.Err() == nil {
			a.log.Warn("whatsapp read receipt failed", "channel", a.name, "error", err)
		}
		refs = rest
	}
}
