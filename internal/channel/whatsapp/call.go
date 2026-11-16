package whatsapp

import (
	"context"

	"go.mau.fi/whatsmeow/types"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

// callNotice is what a rejected caller hears instead of ringing silently,
// but only when they have already had a turn with the bot (§7).
const callNotice = "I can't take calls. Please write to me instead."

// peerKey is where a sender id who has had a turn with the bot is recorded
// in the instance state store, so a later call from the same id is more than
// a silent reject (§7).
func peerKey(j types.JID) string { return "peer:" + j.ToNonAD().String() }

// rememberPeer records sender, and alt when known, as ids that have had a
// turn with the bot: the gate already accepted them once. Called only when
// Typing marks a conversation's messages read, which is when a turn starts.
func (a *Adapter) rememberPeer(ctx context.Context, sender, alt types.JID) {
	if a.state == nil {
		return
	}
	for _, j := range []types.JID{sender, alt} {
		if j.User == "" {
			continue
		}
		if err := a.state.Set(ctx, peerKey(j), "1"); err != nil {
			a.log.Warn("whatsapp peer not remembered", "channel", a.name, "error", err)
		}
	}
}

// call rejects an incoming call offer or notice off the event-handler
// goroutine: RejectCall is a network round trip, and the handler must stay
// quick. A caller who already had a turn with the bot hears callNotice in
// the DM, with the conversation's remembered disappearing timer; a stranger,
// or a group call, gets the reject and nothing else, so a stranger the gate
// dropped still gets nothing (§7). Bounded by runCtx, the run's own context.
func (a *Adapter) call(runCtx context.Context, gen int, meta types.BasicCallMeta) {
	go func() {
		a.stateMu.Lock()
		cli, current := a.client, gen == a.clientGen
		a.stateMu.Unlock()
		if !current || cli == nil {
			return
		}
		from := meta.CallCreator
		if from.IsEmpty() {
			from = meta.From
		}
		_ = cli.RejectCall(runCtx, from.ToNonAD(), meta.CallID)
		a.log.Debug("whatsapp call rejected", "channel", a.name, "id", meta.CallID)
		if !meta.GroupJID.IsEmpty() {
			return // group calls: reject only, never a message
		}
		conv, ok := a.knownCaller(runCtx, meta)
		if !ok {
			return
		}
		if _, err := a.Send(runCtx, channel.Outbound{
			Target:  event.Origin{Conversation: conv},
			Message: event.Message{Text: callNotice},
		}); err != nil && runCtx.Err() == nil {
			a.log.Warn("whatsapp call notice not sent", "channel", a.name, "error", err)
		}
	}()
}

// knownCaller reports the DM to reply in when the call's own ids match a
// remembered peer, and whether one was found at all.
func (a *Adapter) knownCaller(ctx context.Context, meta types.BasicCallMeta) (conv string, ok bool) {
	if a.state == nil {
		return "", false
	}
	ids := []types.JID{meta.From, meta.CallCreator, meta.CallCreatorAlt}
	for _, j := range ids {
		if j.User == "" {
			continue
		}
		if _, found, err := a.state.Get(ctx, peerKey(j)); err == nil && found {
			return callConversation(ids), true
		}
	}
	return "", false
}

// callConversation is the DM to answer in: the phone-number id among a
// call's ids when one is known, as conversationOf prefers for a message
// (§4), else whichever id the call named first.
func callConversation(ids []types.JID) string {
	for _, j := range ids {
		if j.Server == types.DefaultUserServer && j.User != "" {
			return j.ToNonAD().String()
		}
	}
	for _, j := range ids {
		if j.User != "" {
			return j.ToNonAD().String()
		}
	}
	return ""
}
