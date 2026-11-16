package page

import (
	"context"
	"encoding/base64"
	"math"
	"reflect"
	"sync"
	"time"

	"holaclaw/internal/browser/cdp"
	"holaclaw/internal/browser/chromium"
	"holaclaw/internal/browser/proto"
	"holaclaw/internal/browser/sidecar"
)

// The live view is a screencast out and input in (docs/browser.md). Two rules shape this
// file: a frame is acknowledged to Chromium only after the Viewer has taken it
// — that ack is the backpressure, so a slow link slows the browser instead of
// queueing frames — and **nothing that arrives on `in` is ever logged**, not
// its contents, not its shape, not at debug level. It is what a person is
// typing into their own accounts.

const (
	// jpegQuality is the measured sweet spot: q60 at 1280×800 was ~10 KB a
	// frame (docs/browser.md); the taller default window costs about half as much again.
	jpegQuality = 60
	// The screencast is bounded at the window's own size, so a frame is the
	// whole viewport and nothing is scaled: what the browser sees is what a
	// person watching it sees.
	maxStreamW   = chromium.DefaultWindowWidth
	maxStreamH   = chromium.DefaultWindowHeight
	minQuality   = 30
	maxQuality   = 80
	maxFPS       = 20
	sourceFPS    = 60 // Chromium's frame source, for the everyNthFrame sum
	metaInterval = 500 * time.Millisecond
	// qualityInterval is how often the screencast may be restarted for a new
	// quality hint. Anything faster is a viewer flapping the picture.
	qualityInterval = time.Second
)

// Stream runs the live view until ctx ends. One per agent: a second one is
// refused with ErrHeld rather than fighting over the screencast.
func (e *Engine) Stream(ctx context.Context, agent string, in <-chan proto.StreamMessage, v sidecar.Viewer) error {
	s, err := e.session(agent)
	if err != nil {
		return err
	}
	if !s.streaming.CompareAndSwap(false, true) {
		return errf(proto.ErrHeld, "stream")
	}
	defer s.streaming.Store(false)

	at, release, err := s.ensure(ctx)
	if err != nil {
		return err
	}
	defer release()

	t, err := at.activeTab()
	if err != nil {
		return err
	}
	st := &stream{e: e, at: at, v: v, quality: jpegQuality, everyNth: 3}
	if err := st.attach(ctx, t); err != nil {
		return err
	}
	defer st.stop()

	if err := st.meta(ctx); err != nil {
		return err
	}
	// What the engine has been asked to do is part of the live view: the ring
	// is replayed first, so a viewer that joined mid-task sees how the page
	// got where it is, then new entries as they are recorded.
	replay, acts, unsubscribe := s.subscribe()
	defer unsubscribe()
	for _, a := range replay {
		if err := st.activity(ctx, a); err != nil {
			return err
		}
	}

	ticker := time.NewTicker(metaInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case a := <-acts:
			if err := st.activity(ctx, a); err != nil {
				return err
			}
		case <-at.conn.Done():
			return errf(proto.ErrNoSession, "closed")
		case <-ticker.C:
			// The tab list and the viewport change on their own; the viewer is
			// told when they do, not on a schedule. The active tab changes on
			// its own too — a popup, a run that started on a fresh tab — and
			// the picture goes where it went.
			st.follow(ctx)
			if err := st.metaIfChanged(ctx); err != nil {
				return err
			}
		case msg, ok := <-in:
			if !ok {
				return nil
			}
			st.input(ctx, msg)
		}
	}
}

// stream is one live view: the tab it follows, the screencast settings, and
// the last meta sent.
type stream struct {
	e  *Engine
	at *attachment
	v  sidecar.Viewer

	quality  int
	everyNth int
	// lastQuality is when the screencast was last restarted for a hint.
	lastQuality time.Time

	tab    *tab
	sess   *cdp.Session
	cancel func()

	// metaMu guards the viewport and the last meta sent: frames update them on
	// the subscription goroutine while Stream's ticker reads them.
	metaMu        sync.Mutex
	width, height int
	lastMeta      proto.StreamMessage
}

// attach starts the screencast on a tab and subscribes to its frames.
func (st *stream) attach(ctx context.Context, t *tab) error {
	st.tab = t
	st.sess = st.at.conn.Session(t.sessionID)
	st.cancel = cdp.On(st.sess, cdp.EventScreencastFrame, func(ev cdp.ScreencastFrameEvent) {
		st.frame(ctx, ev)
	})
	err := st.sess.StartScreencast(ctx, "jpeg", st.quality, maxStreamW, maxStreamH, st.everyNth)
	if err != nil {
		st.cancel()
		st.cancel = nil
		return browserErr(ctx, err, "screencast")
	}
	return nil
}

// stop ends the screencast and drains it: the subscription is cancelled first,
// so no frame reaches the viewer after Stream returns.
func (st *stream) stop() {
	if st.cancel != nil {
		st.cancel()
		st.cancel = nil
	}
	if st.sess == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), attachTimeout)
	defer cancel()
	_ = st.sess.StopScreencast(ctx)
}

// frame hands one JPEG to the viewer and acknowledges it afterwards. It runs
// on the subscription's own goroutine, so blocking here bounds the frames in
// flight without stalling the connection.
func (st *stream) frame(ctx context.Context, ev cdp.ScreencastFrameEvent) {
	data, err := base64.StdEncoding.DecodeString(ev.Data)
	if err != nil {
		return
	}
	w, h := int(ev.Metadata.DeviceWidth), int(ev.Metadata.DeviceHeight)
	if err := st.v.Frame(ctx, data, w, h); err != nil {
		return // the viewer is gone; ctx ends the stream
	}
	// Only now: this is the backpressure.
	if err := st.sess.ScreencastFrameAck(ctx, ev.SessionID); err != nil {
		return
	}
	st.metaMu.Lock()
	resized := w != st.width || h != st.height
	st.width, st.height = w, h
	st.metaMu.Unlock()
	if resized {
		_ = st.metaIfChanged(ctx)
	}
}

// meta sends the current size and tab list.
func (st *stream) meta(ctx context.Context) error {
	st.metaMu.Lock()
	defer st.metaMu.Unlock()
	m := proto.StreamMessage{
		Type:   proto.StreamMeta,
		Width:  st.width,
		Height: st.height,
		Tabs:   st.at.tabList(),
	}
	st.lastMeta = m
	if err := st.v.Meta(ctx, m); err != nil {
		return err
	}
	return nil
}

// activity sends one entry of the engine's own account to the viewer. It
// travels on the same channel as meta — a StreamMessage of its own type — so
// no interface grows for a development view.
func (st *stream) activity(ctx context.Context, a proto.Activity) error {
	return st.v.Meta(ctx, proto.StreamMessage{Type: proto.StreamActivity, Activity: &a})
}

// metaIfChanged sends meta only when the tabs or the viewport moved.
func (st *stream) metaIfChanged(ctx context.Context) error {
	st.metaMu.Lock()
	defer st.metaMu.Unlock()
	m := proto.StreamMessage{
		Type:   proto.StreamMeta,
		Width:  st.width,
		Height: st.height,
		Tabs:   st.at.tabList(),
	}
	if reflect.DeepEqual(m, st.lastMeta) {
		return nil
	}
	st.lastMeta = m
	return st.v.Meta(ctx, m)
}

// input dispatches one viewer message. The daemon has already dropped
// everyone but the takeover lock holder, so whatever arrives is dispatched —
// and never logged, whatever happens.
func (st *stream) input(ctx context.Context, m proto.StreamMessage) {
	switch m.Type {
	case proto.StreamPointer:
		st.pointer(ctx, m)
	case proto.StreamWheel:
		_ = st.sess.DispatchMouseEvent(ctx, cdp.MouseEvent{
			Type: "mouseWheel", X: m.X, Y: m.Y, DeltaX: m.DeltaX, DeltaY: m.DeltaY,
			Modifiers: m.Modifiers,
		})
	case proto.StreamKey:
		_ = st.sess.DispatchKeyEvent(ctx, viewerKey(m.Event, m.Key, m.Code, m.Modifiers))
	case proto.StreamText:
		_ = st.sess.InsertText(ctx, m.Text)
	case proto.StreamTab:
		st.activate(ctx, m.Tab)
	case proto.StreamQuality:
		st.requality(ctx, m)
	case proto.StreamAck:
		// Acks are implicit: the frame is acknowledged when Viewer.Frame
		// returns, which is where the backpressure already lives.
	}
}

// pointer maps a viewer pointer event onto Input.dispatchMouseEvent.
func (st *stream) pointer(ctx context.Context, m proto.StreamMessage) {
	ev := cdp.MouseEvent{X: m.X, Y: m.Y, Modifiers: m.Modifiers, ClickCount: m.Clicks}
	switch m.Event {
	case "down":
		ev.Type, ev.Button, ev.Buttons = "mousePressed", button(m.Button), buttonMask(m.Button)
		if ev.ClickCount == 0 {
			ev.ClickCount = 1
		}
	case "up":
		ev.Type, ev.Button = "mouseReleased", button(m.Button)
		if ev.ClickCount == 0 {
			ev.ClickCount = 1
		}
	default:
		ev.Type, ev.Button = "mouseMoved", "none"
		if m.Button != "" {
			ev.Button, ev.Buttons = button(m.Button), buttonMask(m.Button)
		}
	}
	st.at.mu.Lock()
	st.at.px, st.at.py = m.X, m.Y
	st.at.mu.Unlock()
	_ = st.sess.DispatchMouseEvent(ctx, ev)
}

// activate moves the live view to another tab of the same browser.
func (st *stream) activate(ctx context.Context, targetID string) {
	st.at.mu.Lock()
	t := st.at.tabs[targetID]
	if t != nil {
		st.at.active = targetID
	}
	st.at.mu.Unlock()
	st.show(ctx, t)
}

// follow moves the live view to the active tab when it is no longer the one
// being shown.
func (st *stream) follow(ctx context.Context) {
	t, err := st.at.activeTab()
	if err != nil {
		return
	}
	st.show(ctx, t)
}

// show restarts the screencast on t, unless t is what is already shown.
func (st *stream) show(ctx context.Context, t *tab) {
	if t == nil || t == st.tab {
		return
	}
	st.stop()
	_ = st.at.bs.ActivateTarget(ctx, t.targetID)
	if err := st.attach(ctx, t); err != nil {
		return
	}
	_ = st.metaIfChanged(ctx)
}

// requality applies the viewer's hint, clamped: a viewer may ask for less
// bandwidth, not for more than the sidecar is willing to encode. Taking a new
// setting means stopping and starting the screencast, which costs frames, so a
// change that follows the last one too closely is ignored rather than served.
func (st *stream) requality(ctx context.Context, m proto.StreamMessage) {
	quality, everyNth := st.quality, st.everyNth
	if m.JPEGQuality > 0 {
		quality = clamp(m.JPEGQuality, minQuality, maxQuality)
	}
	if m.MaxFPS > 0 {
		fps := clamp(m.MaxFPS, 1, maxFPS)
		everyNth = clamp(int(math.Round(float64(sourceFPS)/float64(fps))), 1, sourceFPS)
	}
	if quality == st.quality && everyNth == st.everyNth {
		return
	}
	if !st.lastQuality.IsZero() && time.Since(st.lastQuality) < qualityInterval {
		return
	}
	st.lastQuality = time.Now()
	st.quality, st.everyNth = quality, everyNth
	// Restarting is how the screencast takes new settings.
	_ = st.sess.StopScreencast(ctx)
	_ = st.sess.StartScreencast(ctx, "jpeg", st.quality, maxStreamW, maxStreamH, st.everyNth)
}

// button maps the viewer's name onto CDP's.
func button(name string) string {
	switch name {
	case "middle", "right", "back", "forward":
		return name
	case "":
		return "none"
	default:
		return "left"
	}
}

// buttonMask is CDP's buttons bit field for a held button.
func buttonMask(name string) int {
	switch name {
	case "right":
		return 2
	case "middle":
		return 4
	case "":
		return 0
	default:
		return 1
	}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
