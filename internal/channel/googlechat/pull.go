package googlechat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"time"

	"holaclaw/internal/channel"
)

// pulledMessage is one Pub/Sub delivery.
type pulledMessage struct {
	AckID   string `json:"ackId"`
	Message struct {
		Data      string `json:"data"`
		MessageID string `json:"messageId"`
	} `json:"message"`
}

// Run pulls events until ctx ends (docs/channels/googlechat.md §5). It never
// gives up: a missing or refused key is reported and retried slowly, because
// the key is read per request and `auth set` can fix it without a restart.
func (a *Adapter) Run(ctx context.Context, in channel.Inbound, status func(channel.State, string)) error {
	if status == nil {
		status = func(channel.State, string) {}
	}
	var last channel.State
	var lastDetail string
	report := func(s channel.State, detail string) {
		if s != last || detail != lastDetail {
			last, lastDetail = s, detail
			status(s, detail)
		}
	}
	var bo backoff
	// failed reports a failed request and waits before the next attempt. It
	// returns false once ctx has ended.
	failed := func(op string, err error) bool {
		if ctx.Err() != nil {
			return false
		}
		var e *Error
		errors.As(err, &e)
		a.log.Warn("googlechat "+op+" failed", "error", describe(err))
		switch kindOfErr(err) {
		case KindAuth, KindForbidden, KindBadKey:
			report(channel.StateLoggedOut, describe(err))
			return sleepCtx(ctx, loggedOutWait)
		case KindRateLimited:
			report(channel.StateReconnecting, "rate limited by Google")
			if e.RetryAfter > 0 {
				return sleepCtx(ctx, min(e.RetryAfter, backoffMax))
			}
		default:
			report(channel.StateReconnecting, describe(err))
		}
		return bo.wait(ctx)
	}
	// checked is whether the subscription check passed since the last
	// failure. A pull only returns once an event arrives or Pub/Sub's wait
	// ends, so the quick check is what reports connected without that delay.
	checked := false
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if strings.TrimSpace(a.opt.Key()) == "" {
			checked = false
			report(channel.StateUnlinked, "no service_account key is set")
			if !sleepCtx(ctx, loggedOutWait) {
				return ctx.Err()
			}
			continue
		}
		if !checked {
			project, err := a.checkSubscription(ctx)
			if err != nil {
				if !failed("subscription check", err) {
					return ctx.Err()
				}
				continue
			}
			checked = true
			report(channel.StateConnected, subscriptionPath(project))
		}
		msgs, project, err := a.pull(ctx)
		if err != nil {
			checked = false
			if !failed("pull", err) {
				return ctx.Err()
			}
			continue
		}
		report(channel.StateConnected, subscriptionPath(project))
		if !a.handleBatch(ctx, in, msgs) {
			// The nacked events come back at once; a rejection that persists
			// must not turn into a hot loop of pulls.
			if !bo.wait(ctx) {
				return ctx.Err()
			}
			continue
		}
		bo.reset()
	}
}

// pull is one long poll. A poll that outlives pullTimeout is an empty batch,
// not a failure.
func (a *Adapter) pull(ctx context.Context) ([]pulledMessage, string, error) {
	var out struct {
		ReceivedMessages []pulledMessage `json:"receivedMessages"`
	}
	project := ""
	err := a.call(ctx, request{
		op:     "pull",
		method: http.MethodPost,
		url: func(p string) string {
			project = p
			return a.pubsub + "/v1/" + subscriptionPath(p) + ":pull"
		},
		body:    map[string]any{"maxMessages": maxPullBatch, "returnImmediately": false},
		timeout: pullTimeout,
	}, &out)
	if err != nil {
		var e *Error
		if ctx.Err() == nil && errors.As(err, &e) && e.Kind == KindNetwork && errors.Is(err, context.DeadlineExceeded) {
			return nil, project, nil
		}
		return nil, project, err
	}
	return out.ReceivedMessages, project, nil
}

// pending is one decoded delivery waiting to be handled.
type pending struct {
	ackID string
	ev    *wireEvent
	at    time.Time
}

// handleBatch handles one pulled batch in createTime order: store, then ack.
// When Inbound fails, that event and every later one in the batch get their
// ack deadline set to 0, so Pub/Sub redelivers them in a batch that keeps the
// order. It reports false when Inbound refused an event.
func (a *Adapter) handleBatch(ctx context.Context, in channel.Inbound, msgs []pulledMessage) bool {
	var batch []pending
	var junk []string
	for _, m := range msgs {
		ev, reason := decodeEvent(m.Message.Data)
		if ev == nil {
			// Acked, or it would come back forever. Only the reason is logged,
			// never the content.
			a.log.Warn("googlechat event dropped", "reason", reason, "pubsub_message", m.Message.MessageID)
			junk = append(junk, m.AckID)
			continue
		}
		batch = append(batch, pending{ackID: m.AckID, ev: ev, at: ev.sortTime()})
	}
	a.ack(ctx, junk)
	sort.SliceStable(batch, func(i, j int) bool { return batch[i].at.Before(batch[j].at) })

	for i, p := range batch {
		if err := a.handleEvent(ctx, in, p.ev); err != nil {
			a.log.Warn("googlechat inbound rejected", "type", p.ev.Type, "error", a.scrub(describe(err)))
			ids := make([]string, 0, len(batch)-i)
			for _, rest := range batch[i:] {
				ids = append(ids, rest.ackID)
			}
			a.nack(ctx, ids)
			return false
		}
		a.ack(ctx, []string{p.ackID})
	}
	return true
}

// decodeEvent unwraps a Pub/Sub data field into a Chat event. A nil event comes
// with the reason it was refused.
func decodeEvent(data string) (*wireEvent, string) {
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(data); err != nil {
			return nil, "data is not base64"
		}
	}
	var ev wireEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, "data is not a JSON chat event"
	}
	switch ev.Type {
	case evMessage:
		if ev.Message == nil || ev.Space == nil {
			return nil, "MESSAGE without message or space"
		}
	case evAdded, evRemoved:
		if ev.Space == nil {
			return nil, ev.Type + " without space"
		}
	case evCardClicked:
	case "":
		return nil, "event has no type"
	default:
		return nil, "unhandled event type " + truncate(ev.Type, 40)
	}
	return &ev, ""
}

// ack acknowledges delivered messages. It outlives a cancelled poll: the
// envelopes are already stored.
func (a *Adapter) ack(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ackTimeout)
	defer cancel()
	err := a.call(actx, request{
		op: "acknowledge", method: http.MethodPost,
		url:     func(p string) string { return a.pubsub + "/v1/" + subscriptionPath(p) + ":acknowledge" },
		body:    map[string]any{"ackIds": ids},
		timeout: ackTimeout,
	}, nil)
	if err != nil {
		// Pub/Sub redelivers; the dedupe window and the router absorb it.
		a.log.Warn("googlechat acknowledge failed", "error", describe(err))
	}
}

// nack sets the ack deadline to 0 so Pub/Sub redelivers at once.
func (a *Adapter) nack(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ackTimeout)
	defer cancel()
	err := a.call(actx, request{
		op: "modifyAckDeadline", method: http.MethodPost,
		url:     func(p string) string { return a.pubsub + "/v1/" + subscriptionPath(p) + ":modifyAckDeadline" },
		body:    map[string]any{"ackIds": ids, "ackDeadlineSeconds": 0},
		timeout: ackTimeout,
	}, nil)
	if err != nil {
		// The deadline lapses on its own; redelivery only comes later.
		a.log.Warn("googlechat modifyAckDeadline failed", "error", describe(err))
	}
}

// backoff is exponential with jitter, backoffBase → backoffMax.
type backoff struct{ n int }

func (b *backoff) reset() { b.n = 0 }

// wait sleeps for the next interval. It reports false when ctx ended first.
func (b *backoff) wait(ctx context.Context) bool {
	d := backoffBase << min(b.n, 16)
	if d > backoffMax || d <= 0 {
		d = backoffMax
	}
	b.n++
	d = d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
	return sleepCtx(ctx, d)
}
