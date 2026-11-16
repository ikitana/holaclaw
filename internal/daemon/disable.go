package daemon

import (
	"context"
	"encoding/json"

	"holaclaw/internal/event"
	"holaclaw/internal/session"
	"holaclaw/internal/store"
)

// Every check on a disabled identity runs where something new starts. What
// was already running when it was disabled is stopped here, on the
// identity.disabled event, which is emitted after the commit: a stream that
// reconnects or a turn that starts after it sees the new status.

// onDisabled is the event sink that reacts to identity.disabled. The fan-out
// holds its lock while a sink runs, and handing back the browser emits, so the
// work runs on its own goroutine.
func (d *Daemon) onDisabled(_ context.Context, e event.Event) {
	if e.Type != event.IdentityDisabled {
		return
	}
	var attrs struct {
		Identity event.ID `json:"identity"`
	}
	if json.Unmarshal(e.Attrs, &attrs) != nil || attrs.Identity == "" {
		return
	}
	go d.cutOff(context.Background(), attrs.Identity)
}

// cutOff closes what a disabled identity has open — its event streams and its
// live views, and the browser it holds — and aborts its running turns. Enabling
// restores none of it: clients reconnect and send again.
func (d *Daemon) cutOff(ctx context.Context, id event.ID) {
	streams := d.Hub.Disconnect(id)
	if d.browser != nil {
		d.browser.live.Disconnect(ctx, id)
	}
	turns := 0
	if d.Sessions != nil {
		turns = d.Sessions.AbortIdentity(id)
	}
	d.log.Info("cut off a disabled identity", "identity", id, "streams", streams, "turns", turns)
}

// turnDisabled reports whether a turn is for a disabled identity alone: its
// session's owner is disabled, or every sender is. Its messages were accepted
// before the disable, and the turn starts after it.
func (d *Daemon) turnDisabled(ctx context.Context, sess store.Session, pending []event.Message) bool {
	disabled := func(id event.ID) bool {
		i, err := d.App.Store.GetIdentity(ctx, id)
		return err == nil && i.Disabled()
	}
	if sess.UserID != "" && disabled(sess.UserID) {
		return true
	}
	senders := session.Senders(sess.UserID, pending)
	for _, id := range senders {
		if !disabled(id) {
			return false
		}
	}
	return len(senders) > 0
}
