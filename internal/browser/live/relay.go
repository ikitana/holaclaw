package live

import (
	"context"
	"encoding/json"
	"sync"

	"holaclaw/internal/browser/proto"
	"holaclaw/internal/event"
)

// textBuffer is how many control messages may queue for one watcher. Meta is
// rare; a watcher that cannot keep up with it is not worth stalling the relay.
const textBuffer = 8

// Backpressure, chosen once and held to: the relay acknowledges every frame to
// the sidecar as soon as it has been offered to the watchers, and a watcher
// that has not acknowledged its last frame is skipped rather than queued. So
// exactly one frame is in flight per watcher, a slow watcher loses frames
// instead of slowing the screencast, and the sidecar's own ack timeout (it
// drops a stream that goes unacknowledged) is never tripped by a viewer.

// relay is one sidecar stream fanned out to the watchers of one agent. The
// first watcher opens it, the last one closes it.
type relay struct {
	svc   *Service
	agent string
	up    Stream
	// cancel ends the upstream stream; it is not tied to any one watcher's
	// request, so a watcher leaving cannot cut the others off.
	cancel context.CancelFunc

	mu       sync.Mutex
	watchers map[*watcher]struct{}
	lastMeta []byte
}

// watcher is one live-view connection.
type watcher struct {
	identityID event.ID
	view       Viewer
	frames     chan []byte // at most one frame waiting
	texts      chan []byte
	// stop ends this watcher's read and write loops; the relay calls it when
	// the upstream ends.
	stop context.CancelFunc

	mu      sync.Mutex
	waiting bool // a frame is out and not yet acknowledged
}

// Watch relays the agent's live view to conn until ctx ends or the viewer
// goes away. Input is forwarded to the sidecar only while identityID holds the
// takeover lock; everyone else's is dropped, silently and without a trace.
func (s *Service) Watch(ctx context.Context, agent string, identityID event.ID, conn Viewer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &watcher{
		identityID: identityID,
		view:       conn,
		frames:     make(chan []byte, 1),
		texts:      make(chan []byte, textBuffer),
		stop:       cancel,
	}
	r, err := s.join(ctx, agent, w)
	if err != nil {
		return err
	}
	defer s.leave(r, w)

	s.log.Info("browser: live view started", "agent", agent, "user", string(identityID))
	defer s.log.Info("browser: live view ended", "agent", agent, "user", string(identityID))

	go w.write(ctx)
	r.read(ctx, w)
	return nil
}

// Disconnect ends every live view identityID has open and hands back any
// browser it holds, so a run suspended for it goes on. The daemon calls it when
// the identity is disabled; a new ticket is refused at authentication.
func (s *Service) Disconnect(ctx context.Context, identityID event.ID) {
	if identityID == "" {
		return
	}
	s.mu.Lock()
	var held []string
	for agent, a := range s.agents {
		if a.holder == identityID {
			held = append(held, agent)
		}
	}
	relays := make([]*relay, 0, len(s.relays))
	for _, r := range s.relays {
		relays = append(relays, r)
	}
	s.mu.Unlock()
	for _, r := range relays {
		r.mu.Lock()
		for w := range r.watchers {
			if w.identityID == identityID {
				w.stop()
			}
		}
		r.mu.Unlock()
	}
	for _, agent := range held {
		// ErrNotHolder only when it was handed back meanwhile.
		_ = s.Release(ctx, agent, identityID)
	}
}

// join adds w to the agent's relay, opening the sidecar stream when it is the
// first watcher.
func (s *Service) join(ctx context.Context, agent string, w *watcher) (*relay, error) {
	s.mu.Lock()
	if r, ok := s.relays[agent]; ok {
		r.add(w)
		s.mu.Unlock()
		return r, nil
	}
	s.mu.Unlock()

	// The stream outlives the watcher that opened it, so it runs on a context
	// of its own.
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	up, err := s.sidecar.Stream(streamCtx, agent)
	if err != nil {
		cancel()
		return nil, err
	}
	r := &relay{svc: s, agent: agent, up: up, cancel: cancel, watchers: map[*watcher]struct{}{}}

	s.mu.Lock()
	if other, ok := s.relays[agent]; ok {
		// Another watcher won the race; keep its relay and drop ours.
		other.add(w)
		s.mu.Unlock()
		cancel()
		_ = up.Close()
		return other, nil
	}
	s.relays[agent] = r
	r.add(w)
	s.mu.Unlock()

	go r.pump(streamCtx)
	return r, nil
}

// leave removes w and closes the sidecar stream when it was the last watcher.
func (s *Service) leave(r *relay, w *watcher) {
	s.mu.Lock()
	r.mu.Lock()
	delete(r.watchers, w)
	last := len(r.watchers) == 0
	r.mu.Unlock()
	if last && s.relays[r.agent] == r {
		delete(s.relays, r.agent)
	}
	s.mu.Unlock()
	if last {
		r.cancel()
		_ = r.up.Close()
	}
}

func (r *relay) add(w *watcher) {
	r.mu.Lock()
	r.watchers[w] = struct{}{}
	meta := r.lastMeta
	r.mu.Unlock()
	if meta != nil {
		w.sendText(meta)
	}
}

func (r *relay) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.watchers)
}

// pump reads the sidecar stream and fans it out until the stream ends.
func (r *relay) pump(ctx context.Context) {
	for f := range r.up.Frames() {
		switch {
		case f.Meta != nil:
			r.broadcastMeta(*f.Meta)
		case f.JPEG != nil:
			r.broadcastFrame(f.JPEG)
			// Acknowledged straight away: the viewers' pace is their own
			// problem, the screencast's is not.
			_ = r.up.Ack(ctx)
		}
	}
	// The stream ended: let every watcher's Watch return.
	r.mu.Lock()
	for w := range r.watchers {
		w.stop()
	}
	r.mu.Unlock()
}

// broadcastMeta forwards a control message with the current lock holder in it,
// which is the one thing the sidecar cannot know.
//
// Only meta is the state of the view: the sidecar sends other text messages
// too (its activity account, docs/browser.md), and those are one-off news. They are
// forwarded as they came — no holder, and not kept as the message a joining
// watcher is caught up with, which would otherwise leave a new viewer with an
// old activity entry and no tab list at all.
func (r *relay) broadcastMeta(m proto.StreamMessage) {
	if m.Type != proto.StreamMeta {
		r.forward(m)
		return
	}
	m.Holder = string(r.svc.Holder(r.agent))
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	r.mu.Lock()
	r.lastMeta = raw
	watchers := r.snapshot()
	r.mu.Unlock()
	for _, w := range watchers {
		w.sendText(raw)
	}
}

// forward passes one non-meta text message on to the watchers unchanged.
func (r *relay) forward(m proto.StreamMessage) {
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	r.mu.Lock()
	watchers := r.snapshot()
	r.mu.Unlock()
	for _, w := range watchers {
		w.sendText(raw)
	}
}

// broadcastFrame offers a frame to every watcher that has acknowledged its
// last one.
func (r *relay) broadcastFrame(jpeg []byte) {
	r.mu.Lock()
	watchers := r.snapshot()
	r.mu.Unlock()
	for _, w := range watchers {
		w.offer(jpeg)
	}
}

// snapshot copies the watcher set. Callers hold r.mu.
func (r *relay) snapshot() []*watcher {
	out := make([]*watcher, 0, len(r.watchers))
	for w := range r.watchers {
		out = append(out, w)
	}
	return out
}

// holderChanged tells the watchers of an agent who holds the browser now,
// without waiting for the sidecar's next meta message.
func (s *Service) holderChanged(agent string) {
	s.mu.Lock()
	r, ok := s.relays[agent]
	s.mu.Unlock()
	if !ok {
		return
	}
	r.mu.Lock()
	last := r.lastMeta
	r.mu.Unlock()
	m := proto.StreamMessage{Type: proto.StreamMeta}
	if last != nil {
		if json.Unmarshal(last, &m) != nil {
			return
		}
	}
	r.broadcastMeta(m)
}

// read is the viewer's half of the relay: acks free the watcher's frame slot,
// input reaches the sidecar only from the lock holder, and everything else is
// dropped. Nothing read here is logged, traced or returned in an error — not
// even as "malformed".
func (r *relay) read(ctx context.Context, w *watcher) {
	for {
		raw, err := w.view.Read(ctx)
		if err != nil {
			return
		}
		var m proto.StreamMessage
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		switch m.Type {
		case proto.StreamAck:
			w.acked()
		case proto.StreamQuality:
			// A hint about the picture, not input — but the sidecar restarts
			// the screencast to take it, so while somebody holds the browser
			// it is theirs alone to send: another watcher must not be able to
			// interrupt the picture a person is working in.
			if holder := r.svc.Holder(r.agent); holder == "" || holder == w.identityID {
				_ = r.up.Send(ctx, m)
			}
		case proto.StreamPointer, proto.StreamWheel, proto.StreamKey,
			proto.StreamText, proto.StreamTab:
			if w.identityID != "" && r.svc.Holder(r.agent) == w.identityID {
				_ = r.up.Send(ctx, m)
			}
		default:
			// Unknown, or a server-to-viewer type: ignored.
		}
	}
}

// --- one watcher ----------------------------------------------------------------

// offer hands a frame over unless the watcher still owes an ack for the last
// one, in which case the frame is dropped.
func (w *watcher) offer(jpeg []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.waiting {
		return
	}
	select {
	case w.frames <- jpeg:
		w.waiting = true
	default:
	}
}

func (w *watcher) acked() {
	w.mu.Lock()
	w.waiting = false
	w.mu.Unlock()
}

// sendText queues a control message, dropping it when the watcher is that far
// behind: a stale meta is worth less than a working relay.
func (w *watcher) sendText(raw []byte) {
	select {
	case w.texts <- raw:
	default:
	}
}

// write is the only goroutine that writes to a viewer.
func (w *watcher) write(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case jpeg := <-w.frames:
			if err := w.view.WriteFrame(ctx, jpeg); err != nil {
				w.stop()
				return
			}
		case raw := <-w.texts:
			if err := w.view.WriteText(ctx, raw); err != nil {
				w.stop()
				return
			}
		}
	}
}
