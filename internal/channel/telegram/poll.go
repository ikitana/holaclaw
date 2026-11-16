package telegram

import (
	"context"
	"errors"
	"math/rand/v2"
	"strconv"
	"time"

	"holaclaw/internal/channel"
)

// allowedUpdates is the exact set holaclaw consumes. edited_message is not one
// of them: an edit is not a new turn.
var allowedUpdates = []string{"message", "callback_query", "my_chat_member", "message_reaction"}

// Run connects and polls until ctx ends (ARCHITECTURE.md §6, docs/channels).
func (a *Adapter) Run(ctx context.Context, in channel.Inbound, status func(channel.State, string)) error {
	if status == nil {
		status = func(channel.State, string) {}
	}
	var bo backoff
	for {
		err := a.connect(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var te *Error
		if errors.As(err, &te) && te.Kind == KindAuth {
			// A revoked token never recovers by retrying.
			status(channel.StateLoggedOut, te.Description)
			return err
		}
		status(channel.StateReconnecting, describe(err))
		if !bo.wait(ctx) {
			return ctx.Err()
		}
	}
	_, user := a.bot()
	status(channel.StateConnected, "@"+user)

	p := &poller{a: a, in: in}
	p.confirmed = a.loadOffset(ctx)
	p.next = p.confirmed
	return p.run(ctx, status)
}

// connect runs getMe and records the bot identity used for mention detection
// and from_self.
func (a *Adapter) connect(ctx context.Context) error {
	var me wireUser
	if err := a.call(ctx, "getMe", nil, &me); err != nil {
		return err
	}
	a.setBot(me.ID, me.Username)
	return nil
}

// album is a media group being debounced into one envelope (docs/channels).
type album struct {
	group    string
	msgs     []*wireMessage
	upTo     int64
	deadline time.Time
}

// poller owns the offset pair. confirmed is what is persisted and what the
// next poll acknowledges; next runs ahead of it only while an album is buffered
// in memory, so a crash replays the whole album.
type poller struct {
	a         *Adapter
	in        channel.Inbound
	confirmed int64
	next      int64
	alb       *album
}

func (p *poller) run(ctx context.Context, status func(channel.State, string)) error {
	var bo backoff
	healthy := true
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if p.alb != nil && !time.Now().Before(p.alb.deadline) {
			p.flush(ctx)
			continue
		}
		timeout := p.a.pollTimeout
		if p.alb != nil {
			timeout = 0 // a buffered album must not wait out a long poll
		}
		ups, err := p.a.getUpdates(ctx, p.next, timeout)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var te *Error
			switch {
			case errors.As(err, &te) && te.Kind == KindAuth:
				status(channel.StateLoggedOut, te.Description)
				return err
			case errors.As(err, &te) && te.Kind == KindRateLimit:
				status(channel.StateReconnecting, "rate limited by Telegram")
				healthy = false
				if !sleepCtx(ctx, te.RetryAfter) {
					return ctx.Err()
				}
				continue
			case errors.As(err, &te) && te.Kind == KindConflict:
				status(channel.StateReconnecting, "another poller is running getUpdates for this bot token")
			default:
				status(channel.StateReconnecting, describe(err))
			}
			healthy = false
			p.a.log.Warn("telegram poll failed", "channel", p.a.name, "error", describe(err))
			if !bo.wait(ctx) {
				return ctx.Err()
			}
			continue
		}
		bo.reset()
		if !healthy {
			status(channel.StateConnected, "polling resumed")
			healthy = true
		}
		if len(ups) == 0 {
			if p.alb != nil && !sleepCtx(ctx, time.Until(p.alb.deadline)) {
				return ctx.Err()
			}
			continue
		}
		p.feed(ctx, ups)
	}
}

// getUpdates is one long poll.
func (a *Adapter) getUpdates(ctx context.Context, offset int64, timeout time.Duration) ([]wireUpdate, error) {
	body := map[string]any{
		"offset":          offset,
		"timeout":         int(timeout / time.Second),
		"allowed_updates": allowedUpdates,
	}
	var ups []wireUpdate
	if err := a.call(ctx, "getUpdates", body, &ups); err != nil {
		return nil, err
	}
	return ups, nil
}

// feed processes one batch in order. It stops at the first rejected envelope so
// that update — and everything after it — is redelivered (at-least-once; the
// access gate records each message's platform id durably and answers a
// redelivered one with channel.ErrDuplicate).
func (p *poller) feed(ctx context.Context, ups []wireUpdate) {
	for _, up := range ups {
		if m := up.Message; m != nil && m.MediaGroupID != "" {
			if p.alb != nil && p.alb.group != m.MediaGroupID && !p.flush(ctx) {
				return
			}
			if p.alb == nil {
				p.alb = &album{group: m.MediaGroupID, deadline: time.Now().Add(p.a.albumWait)}
			}
			p.alb.msgs = append(p.alb.msgs, m)
			p.alb.upTo = up.UpdateID
			p.next = up.UpdateID + 1
			continue
		}
		if p.alb != nil && !p.flush(ctx) {
			return
		}
		if !p.one(ctx, up) {
			return
		}
	}
}

// one handles a single non-album update. It reports whether the offset may
// advance past it.
func (p *poller) one(ctx context.Context, up wireUpdate) bool {
	switch {
	case up.CallbackQuery != nil:
		p.a.handleCallback(ctx, up.CallbackQuery)
	case up.MyChatMember != nil:
		p.a.handleChatMember(ctx, up.MyChatMember)
	case up.MessageReaction != nil:
		if env, ok := p.a.reactionEnvelope(up.UpdateID, up.MessageReaction); ok {
			if err := p.in(ctx, env); err != nil {
				p.a.log.Warn("telegram reaction rejected", "channel", p.a.name, "error", describe(err))
				p.reject()
				return false
			}
		}
	case up.Message != nil:
		env, ok := p.a.envelope(ctx, []*wireMessage{up.Message})
		if ok {
			// A duplicate was handled before: the offset moves past it.
			if err := p.in(ctx, env); err != nil && !errors.Is(err, channel.ErrDuplicate) {
				p.a.log.Warn("telegram inbound rejected", "channel", p.a.name, "error", describe(err))
				p.reject()
				return false
			}
		}
	}
	p.accept(ctx, up.UpdateID)
	return true
}

// flush delivers the buffered media group as one envelope.
func (p *poller) flush(ctx context.Context) bool {
	alb := p.alb
	p.alb = nil
	if alb == nil || len(alb.msgs) == 0 {
		return true
	}
	env, ok := p.a.envelope(ctx, alb.msgs)
	if ok {
		if err := p.in(ctx, env); err != nil && !errors.Is(err, channel.ErrDuplicate) {
			p.a.log.Warn("telegram album rejected", "channel", p.a.name, "error", describe(err))
			p.reject()
			return false
		}
	}
	p.accept(ctx, alb.upTo)
	return true
}

// accept advances and persists the offset past an accepted update.
func (p *poller) accept(ctx context.Context, updateID int64) {
	p.confirmed = updateID + 1
	if p.next < p.confirmed {
		p.next = p.confirmed
	}
	p.a.saveOffset(ctx, p.confirmed)
}

// reject rewinds the in-memory offset so the rejected update is polled again.
func (p *poller) reject() {
	p.next = p.confirmed
	p.alb = nil
}

func (a *Adapter) loadOffset(ctx context.Context) int64 {
	if a.state == nil {
		return 0
	}
	v, ok, err := a.state.Get(ctx, offsetKey)
	if err != nil {
		a.log.Warn("telegram offset load failed", "channel", a.name, "error", err.Error())
		return 0
	}
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (a *Adapter) saveOffset(ctx context.Context, off int64) {
	if a.state == nil {
		return
	}
	// The offset must outlive a cancelled poll, so it is written with a
	// context that is not the poll's.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := a.state.Set(sctx, offsetKey, strconv.FormatInt(off, 10)); err != nil {
		a.log.Warn("telegram offset save failed", "channel", a.name, "error", err.Error())
	}
}

// backoff is exponential with jitter, 1s → 60s (docs/channels).
type backoff struct{ n int }

func (b *backoff) reset() { b.n = 0 }

// wait sleeps for the next interval. It reports false when ctx ended first.
func (b *backoff) wait(ctx context.Context) bool {
	d := backoffBase << min(b.n, 16)
	if d > backoffMax || d <= 0 {
		d = backoffMax
	}
	b.n++
	// Full jitter over the lower half keeps pollers from resyncing.
	d = d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
	return sleepCtx(ctx, d)
}

// sleepCtx sleeps for d, or returns false when ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// describe renders an error for a status detail or a log line. Errors from this
// package are already scrubbed of the token.
func describe(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
