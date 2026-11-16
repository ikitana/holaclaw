package telegram

import (
	"context"
	"sync"
	"time"

	"holaclaw/internal/event"
)

// Typing shows the typing indicator and refreshes it every 4 s until stop is
// called. A failure puts the indicator on a cooldown rather than retrying:
// it is decoration, not delivery (docs/channels).
func (a *Adapter) Typing(ctx context.Context, target event.Origin) func() {
	tctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(typingInterval)
		defer t.Stop()
		for {
			if !a.chatAction(tctx, target) {
				return
			}
			select {
			case <-tctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// chatAction sends one sendChatAction. It reports false when the loop should
// stop: the context ended, or the call failed and a cooldown started.
func (a *Adapter) chatAction(ctx context.Context, target event.Origin) bool {
	if ctx.Err() != nil {
		return false
	}
	a.mu.Lock()
	cooling := time.Now().Before(a.coolDown)
	a.mu.Unlock()
	if cooling {
		return false
	}
	body := map[string]any{"chat_id": target.Conversation, "action": "typing"}
	if target.Thread != "" {
		body["message_thread_id"] = target.Thread
	}
	if err := a.call(ctx, "sendChatAction", body, nil); err != nil {
		if ctx.Err() == nil {
			a.mu.Lock()
			a.coolDown = time.Now().Add(typingCooldown)
			a.mu.Unlock()
			a.log.Warn("telegram typing indicator disabled", "channel", a.name, "error", describe(err))
		}
		return false
	}
	return true
}
