package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/browser"
	"holaclaw/internal/browser/client"
	"holaclaw/internal/browser/proto"
	"holaclaw/internal/event"
)

// --- fakes ---------------------------------------------------------------------

// fakeSidecar is the sidecar the service talks to: a session list and one
// stream per Stream call.
type fakeSidecar struct {
	mu       sync.Mutex
	sessions []proto.Session
	err      error
	streams  []*fakeStream
}

func (f *fakeSidecar) Sessions(context.Context) ([]proto.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions, f.err
}

func (f *fakeSidecar) Stream(context.Context, string) (Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	s := &fakeStream{
		frames: make(chan client.Frame, 32),
		sent:   make(chan proto.StreamMessage, 32),
		acks:   make(chan struct{}, 64),
		closed: make(chan struct{}),
	}
	f.streams = append(f.streams, s)
	return s, nil
}

func (f *fakeSidecar) opened() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.streams)
}

func (f *fakeSidecar) stream(i int) *fakeStream {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.streams[i]
}

type fakeStream struct {
	frames chan client.Frame
	sent   chan proto.StreamMessage
	acks   chan struct{}
	closed chan struct{}
	once   sync.Once
}

func (s *fakeStream) Frames() <-chan client.Frame { return s.frames }

func (s *fakeStream) Send(_ context.Context, m proto.StreamMessage) error {
	select {
	case s.sent <- m:
	default:
	}
	return nil
}

func (s *fakeStream) Ack(context.Context) error {
	select {
	case s.acks <- struct{}{}:
	default:
	}
	return nil
}

func (s *fakeStream) Close() error {
	s.once.Do(func() { close(s.closed); close(s.frames) })
	return nil
}

func (s *fakeStream) isClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

// fakeViewer is one live-view connection.
type fakeViewer struct {
	frames chan []byte
	texts  chan []byte
	in     chan []byte
	// hold, when set, blocks every write until it is closed: a viewer that
	// cannot keep up.
	hold chan struct{}
}

func newViewer() *fakeViewer {
	return &fakeViewer{
		frames: make(chan []byte, 64),
		texts:  make(chan []byte, 64),
		in:     make(chan []byte, 8),
	}
}

func (v *fakeViewer) WriteFrame(ctx context.Context, jpeg []byte) error {
	if v.hold != nil {
		select {
		case <-v.hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case v.frames <- jpeg:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (v *fakeViewer) WriteText(ctx context.Context, text []byte) error {
	select {
	case v.texts <- text:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (v *fakeViewer) Read(ctx context.Context) ([]byte, error) {
	select {
	case b := <-v.in:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (v *fakeViewer) send(t testing.TB, m proto.StreamMessage) {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	v.in <- raw
}

// recorder captures every event the service emits.
type recorder struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *recorder) Emit(_ context.Context, e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) byType(t event.Type) []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []event.Event
	for _, e := range r.events {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func (r *recorder) dump() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, e := range r.events {
		b.WriteString(string(e.Type))
		b.Write(e.Attrs)
	}
	return b.String()
}

// harness is a service over a fake sidecar with one running browser.
type harness struct {
	svc  *Service
	side *fakeSidecar
	evs  *recorder
	logs *bytes.Buffer
	mu   sync.Mutex
}

func newHarness(t testing.TB, opts ...func(*Options)) *harness {
	t.Helper()
	h := &harness{
		side: &fakeSidecar{sessions: []proto.Session{{
			Agent:   "main",
			Started: time.Now().UTC(),
			Tabs:    []proto.Tab{{ID: "t1", URL: "https://example.test", Title: "Example", Active: true}},
		}}},
		evs:  &recorder{},
		logs: &bytes.Buffer{},
	}
	o := Options{
		Sidecar:         h.side,
		Events:          h.evs,
		Logger:          slog.New(slog.NewTextHandler(&lockedWriter{mu: &h.mu, w: h.logs}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		TicketTTL:       time.Minute,
		TakeoverTimeout: 5 * time.Second,
	}
	for _, f := range opts {
		f(&o)
	}
	h.svc = New(o)
	return h
}

func (h *harness) logged() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logs.String()
}

// lockedWriter shares the harness mutex, so logged() never reads mid-write.
type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// watch starts one watcher and returns a stop function. It returns once the
// watcher has joined the relay, so a test can count on the order it started
// them in.
func (h *harness) watch(t testing.TB, agent string, user event.ID, v *fakeViewer) func() {
	t.Helper()
	before := h.watchers(agent)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := h.svc.Watch(ctx, agent, user, v); err != nil {
			t.Errorf("watch: %v", err)
		}
	}()
	waitFor(t, func() bool { return h.watchers(agent) == before+1 })
	return func() {
		cancel()
		<-done
	}
}

// watchers is how many live views the service reports for an agent.
func (h *harness) watchers(agent string) int {
	list, err := h.svc.Sessions(context.Background())
	if err != nil {
		return 0
	}
	for _, s := range list {
		if s.Agent == agent {
			return s.Watchers
		}
	}
	return 0
}

func waitFor(t testing.TB, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never became true")
		}
		time.Sleep(time.Millisecond)
	}
}

// --- tickets ---------------------------------------------------------------------

func TestTicketSingleUse(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	tk, err := h.svc.IssueTicket("main", "u1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if len(tk.Value) < 40 || tk.ExpiresAt.IsZero() {
		t.Fatalf("ticket = %+v", tk)
	}
	agent, user, ok := h.svc.RedeemTicket(tk.Value)
	if !ok || agent != "main" || user != "u1" {
		t.Fatalf("redeem = %q, %q, %v", agent, user, ok)
	}
	if _, _, ok := h.svc.RedeemTicket(tk.Value); ok {
		t.Fatal("a ticket is single use")
	}
	if _, _, ok := h.svc.RedeemTicket("not-a-ticket"); ok {
		t.Fatal("unknown ticket accepted")
	}
	if _, _, ok := h.svc.RedeemTicket(""); ok {
		t.Fatal("empty ticket accepted")
	}
}

func TestTicketExpiry(t *testing.T) {
	t.Parallel()
	now := time.Now()
	var mu sync.Mutex
	h := newHarness(t, func(o *Options) {
		o.TicketTTL = time.Minute
		o.Now = func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		}
	})
	tk, err := h.svc.IssueTicket("main", "u1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	mu.Lock()
	now = now.Add(61 * time.Second)
	mu.Unlock()
	if _, _, ok := h.svc.RedeemTicket(tk.Value); ok {
		t.Fatal("an expired ticket was accepted")
	}
}

// TestTicketConstantTime pins the comparison: every live ticket is compared,
// so a redeem says nothing about which one nearly matched.
func TestTicketConstantTime(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a, _ := h.svc.IssueTicket("main", "u1")
	b, _ := h.svc.IssueTicket("main", "u2")
	// A value one byte off the first ticket must not match either.
	near := a.Value[:len(a.Value)-1] + "x"
	if _, _, ok := h.svc.RedeemTicket(near); ok {
		t.Fatal("a near miss was accepted")
	}
	if _, user, ok := h.svc.RedeemTicket(b.Value); !ok || user != "u2" {
		t.Fatalf("redeem b = %q, %v", user, ok)
	}
	if _, user, ok := h.svc.RedeemTicket(a.Value); !ok || user != "u1" {
		t.Fatalf("redeem a = %q, %v", user, ok)
	}
}

// --- sessions and the lock ----------------------------------------------------------

func TestSessions(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	if err := h.svc.Takeover(ctx, "main", "u1"); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	list, err := h.svc.Sessions(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("sessions = %+v, %v", list, err)
	}
	s := list[0]
	if s.Agent != "main" || s.Holder != "u1" || s.Watchers != 0 || len(s.Tabs) != 1 || s.Tabs[0].Title != "Example" {
		t.Fatalf("status = %+v", s)
	}
}

func TestLock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	if err := h.svc.Takeover(ctx, "other", "u1"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("unknown agent: err = %v", err)
	}
	if err := h.svc.Takeover(ctx, "main", "u1"); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if !h.svc.Held("main") || h.svc.Holder("main") != "u1" {
		t.Fatalf("holder = %q", h.svc.Holder("main"))
	}
	if err := h.svc.Takeover(ctx, "main", "u2"); !errors.Is(err, ErrHeld) {
		t.Fatalf("second user: err = %v", err)
	}
	if err := h.svc.Takeover(ctx, "main", "u1"); err != nil {
		t.Fatalf("same user again: %v", err)
	}
	if err := h.svc.Release(ctx, "main", "u2"); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("release by another: err = %v", err)
	}
	if err := h.svc.Release(ctx, "main", "u1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if h.svc.Held("main") {
		t.Fatal("still held after release")
	}
	if err := h.svc.Release(ctx, "main", "u1"); err != nil {
		t.Fatalf("release is idempotent for the holder: %v", err)
	}

	started, ended := h.evs.byType(event.BrowserTakeoverStarted), h.evs.byType(event.BrowserTakeoverEnded)
	if len(started) != 1 || len(ended) != 1 {
		t.Fatalf("events: %d started, %d ended", len(started), len(ended))
	}
	var attrs map[string]any
	if err := json.Unmarshal(started[0].Attrs, &attrs); err != nil {
		t.Fatalf("attrs: %v", err)
	}
	if attrs["agent"] != "main" || attrs["user_id"] != "u1" || len(attrs) != 2 {
		t.Fatalf("attrs = %v", attrs)
	}
}

func TestWaitReleased(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	if err := h.svc.WaitReleased(ctx, "main"); err != nil {
		t.Fatalf("nobody holds it: %v", err)
	}
	if err := h.svc.Takeover(ctx, "main", "u1"); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- h.svc.WaitReleased(ctx, "main") }()
	select {
	case err := <-done:
		t.Fatalf("returned while held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := h.svc.Release(ctx, "main", "u1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("wait: %v", err)
	}

	// A cancelled context ends the wait too.
	if err := h.svc.Takeover(ctx, "main", "u1"); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := h.svc.WaitReleased(cctx, "main"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: err = %v", err)
	}
}

// --- takeover requests ---------------------------------------------------------------

func TestRequestTakeoverHandedBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	done := make(chan bool, 1)
	go func() {
		ok, err := h.svc.RequestTakeover(ctx, "main", browser.BlockLogin, "trace1", "sess1")
		if err != nil {
			t.Errorf("request: %v", err)
		}
		done <- ok
	}()

	// While it waits, the status says what it is waiting for.
	waitFor(t, func() bool {
		list, err := h.svc.Sessions(ctx)
		return err == nil && len(list) == 1 && list[0].TakeoverRequested == string(browser.BlockLogin)
	})
	if err := h.svc.Takeover(ctx, "main", "u1"); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	if err := h.svc.Release(ctx, "main", "u1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ok := <-done; !ok {
		t.Fatal("a completed takeover is a hand back")
	}

	reqs := h.evs.byType(event.BrowserTakeoverRequest)
	if len(reqs) != 1 {
		t.Fatalf("%d request events", len(reqs))
	}
	if reqs[0].TraceID != "trace1" || reqs[0].SessionID != "sess1" {
		t.Fatalf("event = %+v", reqs[0])
	}
	var attrs map[string]any
	if err := json.Unmarshal(reqs[0].Attrs, &attrs); err != nil {
		t.Fatalf("attrs: %v", err)
	}
	if attrs["agent"] != "main" || attrs["why"] != "login" || attrs["expires_at"] == nil || len(attrs) != 3 {
		t.Fatalf("attrs = %v", attrs)
	}
	// The pending marker is cleared once the request is answered.
	list, err := h.svc.Sessions(ctx)
	if err != nil || list[0].TakeoverRequested != "" {
		t.Fatalf("status = %+v, %v", list, err)
	}
}

// TestRequestTakeoverAlreadyHeld: a lock taken before the request counts once
// it is released.
func TestRequestTakeoverAlreadyHeld(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	if err := h.svc.Takeover(ctx, "main", "u1"); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	done := make(chan bool, 1)
	go func() {
		ok, _ := h.svc.RequestTakeover(ctx, "main", browser.Block2FA, "", "")
		done <- ok
	}()
	waitFor(t, func() bool {
		list, _ := h.svc.Sessions(ctx)
		return len(list) == 1 && list[0].TakeoverRequested == string(browser.Block2FA)
	})
	if err := h.svc.Release(ctx, "main", "u1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ok := <-done; !ok {
		t.Fatal("want true once the holder handed back")
	}
}

func TestRequestTakeoverTimeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t, func(o *Options) { o.TakeoverTimeout = 20 * time.Millisecond })
	ok, err := h.svc.RequestTakeover(context.Background(), "main", browser.BlockCaptcha, "", "")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if ok {
		t.Fatal("nobody took over: want false")
	}
}

func TestRequestTakeoverContext(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	ok, err := h.svc.RequestTakeover(ctx, "main", browser.BlockLogin, "", "")
	if ok || !errors.Is(err, context.Canceled) {
		t.Fatalf("ok = %v, err = %v", ok, err)
	}
}

// --- the relay ----------------------------------------------------------------------

func TestRelayFanOut(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	if err := h.svc.Takeover(ctx, "main", "u1"); err != nil {
		t.Fatalf("takeover: %v", err)
	}

	v1, v2 := newViewer(), newViewer()
	stop1 := h.watch(t, "main", "u1", v1)
	defer stop1()
	stop2 := h.watch(t, "main", "u2", v2)
	defer stop2()
	waitFor(t, func() bool {
		list, _ := h.svc.Sessions(ctx)
		return len(list) == 1 && list[0].Watchers == 2
	})
	if h.side.opened() != 1 {
		t.Fatalf("%d upstream streams for one agent", h.side.opened())
	}

	up := h.side.stream(0)
	up.frames <- client.Frame{Meta: &proto.StreamMessage{Type: proto.StreamMeta, Width: 800, Height: 600}}
	up.frames <- client.Frame{JPEG: []byte("frame-1")}

	for i, v := range []*fakeViewer{v1, v2} {
		var m proto.StreamMessage
		if err := json.Unmarshal(<-v.texts, &m); err != nil {
			t.Fatalf("viewer %d meta: %v", i, err)
		}
		// The holder is what the daemon adds to a sidecar meta.
		if m.Width != 800 || m.Holder != "u1" {
			t.Fatalf("viewer %d meta = %+v", i, m)
		}
		if got := string(<-v.frames); got != "frame-1" {
			t.Fatalf("viewer %d frame = %q", i, got)
		}
	}
	// One frame, one ack upstream.
	waitFor(t, func() bool { return len(up.acks) == 1 })
}

func TestRelayInputOnlyFromHolder(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	if err := h.svc.Takeover(ctx, "main", "u1"); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	holder, bystander := newViewer(), newViewer()
	defer h.watch(t, "main", "u1", holder)()
	defer h.watch(t, "main", "u2", bystander)()
	up := h.side.stream(0)

	bystander.send(t, proto.StreamMessage{Type: proto.StreamKey, Event: "down", Key: "x"})
	bystander.send(t, proto.StreamMessage{Type: proto.StreamQuality, JPEGQuality: 40})
	holder.send(t, proto.StreamMessage{Type: proto.StreamQuality, JPEGQuality: 45})
	holder.send(t, proto.StreamMessage{Type: proto.StreamPointer, Event: "down", X: 4, Y: 5})

	// The holder's pointer is dispatched and so is the holder's quality hint;
	// nothing of the bystander's leaves the daemon — not the key, and not the
	// quality either, which would restart the screencast under the person
	// working in it. The two watchers are read on goroutines of their own, so
	// only the set is fixed, not the order.
	kinds := map[proto.StreamType]proto.StreamMessage{}
	for i := 0; i < 2; i++ {
		m := <-up.sent
		kinds[m.Type] = m
	}
	if q, ok := kinds[proto.StreamQuality]; !ok || q.JPEGQuality != 45 {
		t.Fatalf("quality = %+v, %v", q, ok)
	}
	if p, ok := kinds[proto.StreamPointer]; !ok || p.X != 4 {
		t.Fatalf("pointer = %+v, %v", p, ok)
	}
	select {
	case m := <-up.sent:
		t.Fatalf("a non-holder's input reached the sidecar: %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

// A quality hint restarts the screencast, so it is the lock holder's to send
// while a lock is held — and anyone's while the browser is nobody's.
func TestRelayQualityFollowsTheLock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	watcher := newViewer()
	defer h.watch(t, "main", "u2", watcher)()
	up := h.side.stream(0)

	// Nobody holds the browser: a watcher may ask for a cheaper picture.
	watcher.send(t, proto.StreamMessage{Type: proto.StreamQuality, JPEGQuality: 35})
	select {
	case m := <-up.sent:
		if m.Type != proto.StreamQuality || m.JPEGQuality != 35 {
			t.Fatalf("unheld quality = %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a quality hint was dropped while nobody held the browser")
	}

	// Once somebody else holds it, the same watcher's hint is dropped.
	if err := h.svc.Takeover(ctx, "main", "u1"); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	watcher.send(t, proto.StreamMessage{Type: proto.StreamQuality, JPEGQuality: 80})
	select {
	case m := <-up.sent:
		t.Fatalf("a bystander restarted the screencast: %+v", m)
	case <-time.After(100 * time.Millisecond):
	}

	// And it is theirs again when the lock is handed back.
	if err := h.svc.Release(ctx, "main", "u1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	watcher.send(t, proto.StreamMessage{Type: proto.StreamQuality, JPEGQuality: 50})
	select {
	case m := <-up.sent:
		if m.Type != proto.StreamQuality || m.JPEGQuality != 50 {
			t.Fatalf("released quality = %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a quality hint was dropped after the lock was released")
	}
}

// TestRelayBackpressure: a watcher that never acks holds at most one frame,
// the sidecar is acked anyway, and the other watcher keeps up.
func TestRelayBackpressure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	slow, fast := newViewer(), newViewer()
	slow.hold = make(chan struct{})
	defer h.watch(t, "main", "u1", slow)()
	defer h.watch(t, "main", "u2", fast)()
	up := h.side.stream(0)

	for i := 0; i < 5; i++ {
		up.frames <- client.Frame{JPEG: []byte{byte(i)}}
	}
	// Every frame is acknowledged upstream whatever the viewers do: the
	// screencast never waits for a window.
	waitFor(t, func() bool { return len(up.acks) == 5 })

	// Neither watcher acked, so each holds exactly one frame: the fast one
	// has it in hand, the slow one's write is still blocked.
	waitFor(t, func() bool { return len(fast.frames) == 1 })
	<-fast.frames
	if len(fast.frames) != 0 {
		t.Fatalf("an unacked watcher got %d frames", len(fast.frames)+1)
	}
	close(slow.hold)
	waitFor(t, func() bool { return len(slow.frames) == 1 })
	<-slow.frames
	if len(slow.frames) != 0 {
		t.Fatalf("the slow watcher got %d frames", len(slow.frames)+1)
	}

	// An ack frees the slot again. The ack is read on the relay's own
	// goroutine, so the frame that gets through may not be the very next one.
	fast.send(t, proto.StreamMessage{Type: proto.StreamAck})
	got := false
	for i := 0; i < 100 && !got; i++ {
		up.frames <- client.Frame{JPEG: []byte("after-ack")}
		select {
		case <-fast.frames:
			got = true
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !got {
		t.Fatal("no frame reached the watcher after its ack")
	}
}

func TestRelayLastWatcherCloses(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	v1, v2 := newViewer(), newViewer()
	stop1 := h.watch(t, "main", "u1", v1)
	stop2 := h.watch(t, "main", "u2", v2)
	up := h.side.stream(0)

	stop1()
	if up.isClosed() {
		t.Fatal("the stream closed while a watcher was still there")
	}
	stop2()
	waitFor(t, up.isClosed)
	if h.side.opened() != 1 {
		t.Fatalf("%d streams opened", h.side.opened())
	}

	// A new watcher opens a fresh stream.
	v3 := newViewer()
	stop3 := h.watch(t, "main", "u3", v3)
	defer stop3()
	waitFor(t, func() bool { return h.side.opened() == 2 })
}

// TestRelayUpstreamEnds: when the sidecar stream ends, every Watch returns.
func TestRelayUpstreamEnds(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	v := newViewer()
	done := make(chan error, 1)
	go func() { done <- h.svc.Watch(context.Background(), "main", "u1", v) }()
	waitFor(t, func() bool { return h.side.opened() == 1 })
	_ = h.side.stream(0).Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch did not return when the stream ended")
	}
}

// TestDisconnect: what the daemon does on identity.disabled. Every live view
// of that identity ends and the browser it held is handed back; another
// user's view goes on.
func TestDisconnect(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()
	mine, theirs := newViewer(), newViewer()
	done := make(chan error, 1)
	go func() { done <- h.svc.Watch(ctx, "main", "u1", mine) }()
	waitFor(t, func() bool { return h.watchers("main") == 1 })
	stop := h.watch(t, "main", "u2", theirs)
	defer stop()
	if err := h.svc.Takeover(ctx, "main", "u1"); err != nil {
		t.Fatalf("takeover: %v", err)
	}

	h.svc.Disconnect(ctx, "u1")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the disabled user's Watch did not return")
	}
	if h.svc.Held("main") {
		t.Fatalf("still held by %q", h.svc.Holder("main"))
	}
	if n := len(h.evs.byType(event.BrowserTakeoverEnded)); n != 1 {
		t.Fatalf("%d takeover.ended events, want 1", n)
	}
	if n := h.watchers("main"); n != 1 {
		t.Fatalf("watchers = %d, want the other user's", n)
	}
	if h.side.stream(0).isClosed() {
		t.Fatal("the upstream closed under the remaining watcher")
	}
	// Nothing held and nobody watching: a no-op.
	h.svc.Disconnect(ctx, "u3")
	h.svc.Disconnect(ctx, "")
	if n := h.watchers("main"); n != 1 {
		t.Fatalf("watchers = %d after no-op disconnects", n)
	}
}

func TestWatchNoStream(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.side.mu.Lock()
	h.side.err = ErrNoSession
	h.side.mu.Unlock()
	if err := h.svc.Watch(context.Background(), "main", "u1", newViewer()); !errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v", err)
	}
}

// TestNothingFromAViewerIsRecorded is the rule of docs/browser.md as a test: a marker
// typed through a watcher reaches neither the log nor an event.
func TestNothingFromAViewerIsRecorded(t *testing.T) {
	t.Parallel()
	const marker = "hunter2-correct-horse"
	h := newHarness(t)
	ctx := context.Background()
	if err := h.svc.Takeover(ctx, "main", "u1"); err != nil {
		t.Fatalf("takeover: %v", err)
	}
	holder, bystander := newViewer(), newViewer()
	defer h.watch(t, "main", "u1", holder)()
	defer h.watch(t, "main", "u2", bystander)()
	up := h.side.stream(0)

	holder.send(t, proto.StreamMessage{Type: proto.StreamText, Text: marker})
	holder.send(t, proto.StreamMessage{Type: proto.StreamKey, Event: "down", Key: marker})
	// A non-holder's input and a malformed message are dropped, and dropping
	// is silent too.
	bystander.send(t, proto.StreamMessage{Type: proto.StreamText, Text: marker})
	bystander.in <- []byte(`{"type":"text","text":"` + marker + `"`) // truncated JSON

	if m := <-up.sent; m.Text != marker {
		t.Fatalf("the holder's text did not reach the sidecar: %+v", m)
	}
	if err := h.svc.Release(ctx, "main", "u1"); err != nil {
		t.Fatalf("release: %v", err)
	}

	if logs := h.logged(); strings.Contains(logs, marker) {
		t.Fatalf("a marker typed by a person reached the log:\n%s", logs)
	}
	if evs := h.evs.dump(); strings.Contains(evs, marker) {
		t.Fatalf("a marker typed by a person reached an event:\n%s", evs)
	}
}
