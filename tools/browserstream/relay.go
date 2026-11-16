package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"holaclaw/internal/browser/client"
	"holaclaw/internal/browser/proto"
)

// retryInterval is how often an agent with no running browser (or whose
// stream just ended) is retried.
const retryInterval = time.Second

// frameBuffer is how many watcher goroutines may have a frame in flight; a
// watcher that cannot keep up simply misses frames, same as the daemon's own
// relay (internal/browser/live/relay.go), whose backpressure rule this
// mirrors: one frame in flight per watcher, everything else dropped.
const frameBuffer = 1

// textBuffer is how many control messages — meta, status, the sidecar's
// activity, the daemon's events — may queue for one viewer. The timeline is a
// stream of its own, so it is deeper than the single frame slot; a viewer that
// falls even further behind drops entries rather than stalling the relay.
const textBuffer = 256

// sidecar is the slice of the sidecar client this tool uses.
type sidecarAPI interface {
	Sessions(ctx context.Context) ([]proto.Session, error)
	Stream(ctx context.Context, agent string) (*client.Stream, error)
}

// manager fans one sidecar stream per agent out to every dev-viewer watching
// it. Unlike the daemon's live.Service there is no takeover lock, no ticket
// and no input forwarding but page-tab switches: this tool is for watching.
type manager struct {
	sidecar sidecarAPI
	log     *slog.Logger
	// events is the daemon's event feed, nil when -no-events or when no
	// daemon address could be worked out.
	events *eventHub

	mu     sync.Mutex
	relays map[string]*relay
}

var _ sidecarAPI = (*client.Client)(nil)

func newManager(c sidecarAPI, log *slog.Logger) *manager {
	return &manager{sidecar: c, log: log, relays: map[string]*relay{}}
}

// relay is one agent's upstream stream, reopened for as long as somebody is
// watching, fanned out to every watcher currently connected.
type relay struct {
	mgr    *manager
	agent  string
	cancel context.CancelFunc

	mu       sync.Mutex
	up       *client.Stream // the currently open upstream stream, or nil while waiting
	watchers map[*watcher]struct{}
	lastMeta []byte
}

// watcher is one browser tab's WebSocket connection to this tool.
type watcher struct {
	conn   *websocket.Conn
	frames chan []byte
	texts  chan []byte
	stop   context.CancelFunc
}

// handleStream upgrades to a WebSocket and streams one agent's browser to it
// until the client disconnects. No origin check: this tool has no notion of
// same-origin, being reachable from any device on the network by design.
func (m *manager) handleStream(w http.ResponseWriter, r *http.Request) {
	agent := r.URL.Query().Get("agent")
	if agent == "" {
		http.Error(w, "missing ?agent=", http.StatusBadRequest)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		return // Accept already answered
	}
	defer func() { _ = conn.CloseNow() }()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	w2 := &watcher{conn: conn, frames: make(chan []byte, frameBuffer), texts: make(chan []byte, textBuffer), stop: cancel}

	rl := m.join(agent, w2)
	defer m.leave(rl, w2)

	// The agent's own account of the run comes from the daemon, not the
	// sidecar, and is not per agent: every viewer sees it.
	if m.events != nil {
		m.events.add(w2)
		defer m.events.remove(w2)
	}

	go w2.write(ctx)
	readTabs(ctx, conn, rl)
}

// join adds w to the agent's relay, starting it when it is the first watcher.
func (m *manager) join(agent string, w *watcher) *relay {
	m.mu.Lock()
	defer m.mu.Unlock()
	rl, ok := m.relays[agent]
	if !ok {
		runCtx, cancel := context.WithCancel(context.Background())
		rl = &relay{mgr: m, agent: agent, cancel: cancel, watchers: map[*watcher]struct{}{}}
		m.relays[agent] = rl
		go rl.run(runCtx)
	}
	rl.add(w)
	return rl
}

// leave removes w, stopping the relay once nobody is left watching.
func (m *manager) leave(rl *relay, w *watcher) {
	m.mu.Lock()
	rl.mu.Lock()
	delete(rl.watchers, w)
	last := len(rl.watchers) == 0
	rl.mu.Unlock()
	if last && m.relays[rl.agent] == rl {
		delete(m.relays, rl.agent)
	}
	m.mu.Unlock()
	if last {
		rl.cancel()
	}
}

func (rl *relay) add(w *watcher) {
	rl.mu.Lock()
	rl.watchers[w] = struct{}{}
	meta := rl.lastMeta
	rl.mu.Unlock()
	if meta != nil {
		w.sendText(meta)
	}
}

// run keeps one agent's upstream stream open for as long as the relay lives,
// reopening it on a timer whenever the agent has no browser yet or the
// stream just ended — the "poll waiting for them" behaviour.
func (rl *relay) run(ctx context.Context) {
	for {
		rl.broadcastStatus("waiting for " + rl.agent + "'s browser…")
		up := rl.dial(ctx)
		if up == nil {
			return // ctx ended while waiting
		}
		rl.mu.Lock()
		rl.up = up
		rl.mu.Unlock()
		rl.broadcastStatus("watching " + rl.agent)
		rl.pump(ctx, up)
		rl.mu.Lock()
		rl.up = nil
		rl.mu.Unlock()
		_ = up.Close()
		if ctx.Err() != nil {
			return
		}
	}
}

// dial retries Stream until it succeeds or ctx ends.
func (rl *relay) dial(ctx context.Context) *client.Stream {
	t := time.NewTicker(retryInterval)
	defer t.Stop()
	for {
		if up, err := rl.mgr.sidecar.Stream(ctx, rl.agent); err == nil {
			return up
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return nil
		}
	}
}

// pump fans one open stream out to the current watchers until it ends.
func (rl *relay) pump(ctx context.Context, up *client.Stream) {
	for f := range up.Frames() {
		switch {
		case f.Meta != nil:
			rl.broadcastText(*f.Meta)
		case f.JPEG != nil:
			rl.broadcastFrame(f.JPEG)
			_ = up.Ack(ctx) // acked immediately: a slow tab drops frames, not the screencast
		}
	}
}

// broadcastText fans one of the sidecar's text messages out. Only meta is the
// state of the view and worth replaying to a watcher that joins later; the
// activity entries are news, and the sidecar replays its own ring to every
// new stream anyway.
func (rl *relay) broadcastText(m proto.StreamMessage) {
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	rl.mu.Lock()
	if m.Type == proto.StreamMeta {
		rl.lastMeta = raw
	}
	watchers := rl.snapshot()
	rl.mu.Unlock()
	for _, w := range watchers {
		w.sendText(raw)
	}
}

func (rl *relay) broadcastStatus(text string) {
	raw, err := json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{"status", text})
	if err != nil {
		return
	}
	rl.mu.Lock()
	watchers := rl.snapshot()
	rl.mu.Unlock()
	for _, w := range watchers {
		w.sendText(raw)
	}
}

func (rl *relay) broadcastFrame(jpeg []byte) {
	rl.mu.Lock()
	watchers := rl.snapshot()
	rl.mu.Unlock()
	for _, w := range watchers {
		w.offer(jpeg)
	}
}

func (rl *relay) snapshot() []*watcher {
	out := make([]*watcher, 0, len(rl.watchers))
	for w := range rl.watchers {
		out = append(out, w)
	}
	return out
}

// offer drops the frame if the watcher's single slot is still full: a stuck
// browser tab loses frames, never the screencast.
func (w *watcher) offer(jpeg []byte) {
	select {
	case w.frames <- jpeg:
	default:
		select {
		case <-w.frames:
		default:
		}
		select {
		case w.frames <- jpeg:
		default:
		}
	}
}

func (w *watcher) sendText(raw []byte) {
	select {
	case w.texts <- raw:
	default:
	}
}

// write is the only goroutine that writes to the viewer's connection.
func (w *watcher) write(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case jpeg := <-w.frames:
			if err := w.conn.Write(ctx, websocket.MessageBinary, jpeg); err != nil {
				w.stop()
				return
			}
		case raw := <-w.texts:
			if err := w.conn.Write(ctx, websocket.MessageText, raw); err != nil {
				w.stop()
				return
			}
		}
	}
}

// readTabs is the viewer's only input: which page to look at. Nothing else
// (pointer, key, text) is accepted — this tool watches, it does not drive.
func readTabs(ctx context.Context, conn *websocket.Conn, rl *relay) {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		var m proto.StreamMessage
		if json.Unmarshal(data, &m) != nil || m.Type != proto.StreamTab {
			continue
		}
		rl.mu.Lock()
		up := rl.up
		rl.mu.Unlock()
		if up != nil {
			_ = up.Send(ctx, m)
		}
	}
}
