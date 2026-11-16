package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
)

// The second feed of the timeline: the daemon's own trace of what the browser
// agent is doing — the moves it decided on, the steps it took, what it was
// blocked by, how a task ended. The sidecar knows none of that, and the
// daemon knows nothing of the pixels; the viewer is where the two meet.
//
// The events are read from /v1/events (SSE) over the daemon's unix socket,
// which needs no token, and everything that is not a browser.* event is
// dropped here rather than shipped to a browser tab. A daemon that is not
// running is not an error: the screencast is the point, this is the
// commentary.

const (
	// eventRing is how many events a viewer that joins later is caught up
	// with.
	eventRing = 200
	// eventRetry is how long to wait before dialling the daemon again.
	eventRetry = 5 * time.Second
	// eventPrefix is the family of events this tool shows; approvalPrefix is
	// the one other family worth a line, because a run that stops for an
	// approval nobody noticed looks exactly like a frozen browser.
	eventPrefix    = "browser."
	approvalPrefix = "approval."
)

// eventSource is the slice of the daemon API client the hub uses.
type eventSource interface {
	Events(ctx context.Context, focus, lastID string) (*api.EventStream, error)
}

var _ eventSource = (*api.Client)(nil)

// eventHub tails the daemon's event stream and fans the browser events out to
// every connected viewer, keeping the last eventRing of them for the next one.
type eventHub struct {
	src   eventSource
	log   *slog.Logger
	retry time.Duration

	mu       sync.Mutex
	ring     [][]byte
	watchers map[*watcher]struct{}
}

func newEventHub(src eventSource, log *slog.Logger) *eventHub {
	return &eventHub{src: src, log: log, retry: eventRetry, watchers: map[*watcher]struct{}{}}
}

// add registers a viewer and catches it up with the events so far.
func (h *eventHub) add(w *watcher) {
	h.mu.Lock()
	h.watchers[w] = struct{}{}
	ring := append([][]byte(nil), h.ring...)
	h.mu.Unlock()
	for _, raw := range ring {
		w.sendText(raw)
	}
}

// size is how many events are kept for the next viewer.
func (h *eventHub) size() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.ring)
}

func (h *eventHub) remove(w *watcher) {
	h.mu.Lock()
	delete(h.watchers, w)
	h.mu.Unlock()
}

// run tails the daemon for as long as ctx lives, reconnecting on the retry
// interval. It says once that the daemon is not there, not once every five
// seconds: this tool runs for hours on a developer's terminal.
func (h *eventHub) run(ctx context.Context) {
	lastID, complained := "", false
	for {
		stream, err := h.src.Events(ctx, "", lastID)
		if err != nil {
			if !complained {
				h.log.Warn("browserstream: no daemon events; retrying", "err", err, "every", h.retry)
				complained = true
			}
		} else {
			complained = false
			lastID = h.tail(stream)
			_ = stream.Close()
		}
		select {
		case <-time.After(h.retry):
		case <-ctx.Done():
			return
		}
	}
}

// tail reads one connection to the end and returns the id to resume from.
func (h *eventHub) tail(stream *api.EventStream) string {
	for {
		ev, err := stream.Next()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				h.log.Debug("browserstream: event stream ended", "err", err)
			}
			return stream.LastID()
		}
		if !strings.HasPrefix(ev.Type, eventPrefix) && !strings.HasPrefix(ev.Type, approvalPrefix) {
			continue
		}
		raw, err := json.Marshal(struct {
			Type  string    `json:"type"`
			Event gen.Event `json:"event"`
		}{"event", ev})
		if err != nil {
			continue
		}
		h.publish(raw)
	}
}

// publish records one rendered event and offers it to every viewer.
func (h *eventHub) publish(raw []byte) {
	h.mu.Lock()
	h.ring = append(h.ring, raw)
	if len(h.ring) > eventRing {
		h.ring = append(h.ring[:0], h.ring[len(h.ring)-eventRing:]...)
	}
	watchers := make([]*watcher, 0, len(h.watchers))
	for w := range h.watchers {
		watchers = append(watchers, w)
	}
	h.mu.Unlock()
	for _, w := range watchers {
		w.sendText(raw)
	}
}
