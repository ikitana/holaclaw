package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestCallReply(t *testing.T) {
	t.Parallel()
	f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
		f.reply(cmd.ID, map[string]any{"product": "Chromium/153"})
	})

	var v Version
	if err := f.conn.Call(ctxT(t), "", "Browser.getVersion", nil, &v); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if v.Product != "Chromium/153" {
		t.Fatalf("product = %q", v.Product)
	}
	got := f.commands()
	if len(got) != 1 || got[0].Method != "Browser.getVersion" || got[0].ID != 1 {
		t.Fatalf("commands = %+v", got)
	}
	if got[0].SessionID != "" {
		t.Fatalf("browser-level command carried a sessionId: %q", got[0].SessionID)
	}
	// A nil result is allowed and the reply is discarded.
	if err := f.conn.Call(ctxT(t), "", "Browser.getVersion", nil, nil); err != nil {
		t.Fatalf("Call with nil result: %v", err)
	}
}

func TestCallCarriesSessionAndParams(t *testing.T) {
	t.Parallel()
	f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
		f.reply(cmd.ID, map[string]any{"frameId": "F1"})
	})

	s := f.conn.Session("S1")
	frameID, errText, err := s.Navigate(ctxT(t), "https://example.test/")
	if err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	if frameID != "F1" || errText != "" {
		t.Fatalf("frame=%q errText=%q", frameID, errText)
	}
	cmd := f.commands()[0]
	if cmd.SessionID != "S1" {
		t.Fatalf("sessionId = %q", cmd.SessionID)
	}
	if !strings.Contains(string(cmd.Params), "https://example.test/") {
		t.Fatalf("params = %s", cmd.Params)
	}
}

func TestCallConcurrent(t *testing.T) {
	t.Parallel()
	// Replies come back out of order on purpose: matching is by id, never by
	// arrival.
	f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
		go f.reply(cmd.ID, map[string]any{"n": cmd.ID})
	})

	const n = 100
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var r struct {
				N int64 `json:"n"`
			}
			if err := f.conn.Call(ctxT(t), "", "Test.echo", map[string]any{"x": 1}, &r); err != nil {
				errs <- err
				return
			}
			if r.N == 0 {
				errs <- errors.New("zero id in reply")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Call: %v", err)
	}

	seen := map[int64]bool{}
	for _, c := range f.commands() {
		if seen[c.ID] {
			t.Fatalf("duplicate id %d", c.ID)
		}
		seen[c.ID] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d ids, want %d", len(seen), n)
	}
}

func TestCallProtocolError(t *testing.T) {
	t.Parallel()
	f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
		f.replyError(cmd.ID, -32601, "'Runtime.nope' wasn't found")
	})

	err := f.conn.Call(ctxT(t), "", "Runtime.nope", nil, nil)
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want *Error", err, err)
	}
	if pe.Code != -32601 || !strings.Contains(pe.Message, "wasn't found") {
		t.Fatalf("error = %+v", pe)
	}
	if !strings.Contains(pe.Error(), "-32601") {
		t.Fatalf("Error() = %q", pe.Error())
	}
}

func TestCallContextCancelled(t *testing.T) {
	t.Parallel()
	f := newFake(t, Options{}, nil) // never replies

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := f.conn.Call(ctx, "", "Test.hang", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	// The pending entry is gone: a late reply must not be delivered anywhere,
	// and the map must not grow.
	f.conn.mu.Lock()
	n := len(f.conn.pending)
	f.conn.mu.Unlock()
	if n != 0 {
		t.Fatalf("pending = %d, want 0", n)
	}
	f.reply(1, map[string]any{"late": true}) // must not panic
	time.Sleep(20 * time.Millisecond)
}

func TestCloseFailsPendingCalls(t *testing.T) {
	t.Parallel()
	f := newFake(t, Options{}, nil)

	done := make(chan error, 1)
	go func() {
		done <- f.conn.Call(context.Background(), "", "Test.hang", nil, nil)
	}()
	time.Sleep(20 * time.Millisecond)
	if err := f.conn.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("pending call err = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending call did not fail after Close")
	}

	select {
	case <-f.conn.Done():
	default:
		t.Fatal("Done not closed")
	}
	if !errors.Is(f.conn.Err(), ErrClosed) {
		t.Fatalf("Err = %v", f.conn.Err())
	}
	// Close is idempotent, and a later Call fails at once.
	if err := f.conn.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := f.conn.Call(ctxT(t), "", "Test.after", nil, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Call after Close = %v", err)
	}
	// Subscribing after Close yields a no-op cancel rather than a panic.
	f.conn.Subscribe("", "", func(json.RawMessage) {})()
}

func TestBrowserDeathEndsPendingCalls(t *testing.T) {
	t.Parallel()
	f := newFake(t, Options{}, nil)

	done := make(chan error, 1)
	go func() {
		done <- f.conn.Call(context.Background(), "", "Test.hang", nil, nil)
	}()
	time.Sleep(20 * time.Millisecond)
	_ = f.evW.Close() // the browser's fd 4 goes away

	select {
	case err := <-done:
		if err == nil || errors.Is(err, ErrClosed) {
			t.Fatalf("err = %v, want the read error", err)
		}
		if !strings.Contains(err.Error(), "read") {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending call did not fail when the pipe closed")
	}
	<-f.conn.Done()
}

func TestOversizeMessageClosesConn(t *testing.T) {
	t.Parallel()
	f := newFake(t, Options{MaxMessageBytes: 1 << 10}, nil)

	f.sendRaw(fmt.Appendf(nil, `{"id":1,"result":{"pad":%q}}`, strings.Repeat("x", 4<<10)))

	select {
	case <-f.conn.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("oversize message did not end the connection")
	}
	if !errors.Is(f.conn.Err(), ErrTooLarge) {
		t.Fatalf("Err = %v, want ErrTooLarge", f.conn.Err())
	}
}

func TestOversizeAcrossBufferRefills(t *testing.T) {
	t.Parallel()
	// Over bufio's 64 KiB buffer, so the ErrBufferFull path decides.
	f := newFake(t, Options{MaxMessageBytes: 1 << 10}, nil)
	f.sendRaw(fmt.Appendf(nil, `{"id":1,"result":{"pad":%q}}`, strings.Repeat("x", 300<<10)))

	select {
	case <-f.conn.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("oversize message did not end the connection")
	}
	if !errors.Is(f.conn.Err(), ErrTooLarge) {
		t.Fatalf("Err = %v, want ErrTooLarge", f.conn.Err())
	}
}

func TestLargeMessageReassembled(t *testing.T) {
	t.Parallel()
	// A screenshot is far bigger than the read buffer: the frame has to be
	// reassembled across refills, unchanged.
	pad := strings.Repeat("abcdefgh", 40<<10) // 320 KiB
	f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
		f.reply(cmd.ID, map[string]any{"data": pad})
	})

	var r struct {
		Data string `json:"data"`
	}
	if err := f.conn.Call(ctxT(t), "", "Page.captureScreenshot", nil, &r); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if r.Data != pad {
		t.Fatalf("payload corrupted: %d bytes, want %d", len(r.Data), len(pad))
	}
}

func TestMalformedFramesAreSkipped(t *testing.T) {
	t.Parallel()
	f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
		f.sendRaw([]byte(`{"id":`))             // truncated JSON
		f.sendRaw([]byte(`{"params":{"a":1}}`)) // neither id nor method
		f.sendRaw([]byte(``))                   // empty frame
		f.sendRaw([]byte(`not json at all`))
		f.sendRaw([]byte(`{"id":99999,"result":{}}`))                         // reply to nothing
		f.sendRaw([]byte(`{"method":"Page.lifecycleEvent","params":"nope"}`)) // params of the wrong shape
		f.reply(cmd.ID, map[string]any{"ok": true})
	})

	typed := make(chan struct{}, 4)
	defer On(f.conn.Session(""), EventLifecycleEvent, func(LifecycleEvent) {
		typed <- struct{}{}
	})()

	var r struct {
		OK bool `json:"ok"`
	}
	if err := f.conn.Call(ctxT(t), "", "Test.ping", nil, &r); err != nil {
		t.Fatalf("Call after malformed frames: %v", err)
	}
	if !r.OK {
		t.Fatal("reply lost")
	}
	if f.conn.Err() != nil {
		t.Fatalf("connection died on a malformed frame: %v", f.conn.Err())
	}
	select {
	case <-typed:
		t.Fatal("event params that do not decode were handed to the handler")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestEventsRoutedBySessionAndMethod(t *testing.T) {
	t.Parallel()
	f := newFake(t, Options{}, nil)

	type hit struct{ where, params string }
	hits := make(chan hit, 32)
	record := func(where string) func(json.RawMessage) {
		return func(p json.RawMessage) { hits <- hit{where, string(p)} }
	}

	defer f.conn.Subscribe("", EventTargetCreated, record("browser/targetCreated"))()
	defer f.conn.Subscribe("S1", EventLifecycleEvent, record("S1/lifecycle"))()
	defer f.conn.Subscribe("S1", "", record("S1/all"))()
	defer f.conn.Subscribe("S2", EventLifecycleEvent, record("S2/lifecycle"))()

	f.event("", EventTargetCreated, map[string]any{"targetInfo": map[string]any{"targetId": "T"}})
	f.event("S1", EventLifecycleEvent, map[string]any{"name": "load"})
	f.event("S1", EventFrameNavigated, map[string]any{"frame": map[string]any{"id": "F"}})
	f.event("S3", EventLifecycleEvent, map[string]any{"name": "load"}) // nobody

	want := map[string]int{
		"browser/targetCreated": 1,
		"S1/lifecycle":          1,
		"S1/all":                2,
	}
	got := map[string]int{}
	deadline := time.After(2 * time.Second)
	for total := 0; total < 4; total++ {
		select {
		case h := <-hits:
			got[h.where]++
		case <-deadline:
			t.Fatalf("timed out; got %v", got)
		}
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
	select {
	case h := <-hits:
		t.Fatalf("unexpected extra delivery: %+v", h)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestEventOrderPreservedPerSubscription(t *testing.T) {
	t.Parallel()
	f := newFake(t, Options{}, nil)

	const n = 200
	seen := make(chan int, n)
	defer On(f.conn.Session("S1"), EventLifecycleEvent, func(ev LifecycleEvent) {
		seen <- int(ev.Timestamp)
	})()

	for i := range n {
		f.event("S1", EventLifecycleEvent, map[string]any{"name": "load", "timestamp": i})
	}
	for i := range n {
		select {
		case got := <-seen:
			if got != i {
				t.Fatalf("event %d arrived as %d: order lost", i, got)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d events arrived", i, n)
		}
	}
}

func TestSubscribeCancel(t *testing.T) {
	t.Parallel()
	f := newFake(t, Options{}, nil)

	hits := make(chan struct{}, 8)
	cancel := f.conn.Subscribe("S1", EventLifecycleEvent, func(json.RawMessage) { hits <- struct{}{} })

	f.event("S1", EventLifecycleEvent, map[string]any{"name": "load"})
	select {
	case <-hits:
	case <-time.After(2 * time.Second):
		t.Fatal("no event before cancel")
	}

	cancel()
	cancel() // idempotent
	f.event("S1", EventLifecycleEvent, map[string]any{"name": "load"})
	select {
	case <-hits:
		t.Fatal("event delivered after cancel")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestHandlerMayCallBackIntoConn(t *testing.T) {
	t.Parallel()
	// The reason handlers do not run on the read loop: this would deadlock.
	f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
		f.reply(cmd.ID, map[string]any{"ok": true})
	})

	done := make(chan error, 1)
	var cancel func()
	cancel = f.conn.Subscribe("S1", EventLifecycleEvent, func(json.RawMessage) {
		err := f.conn.Call(ctxT(t), "S1", "Page.bringToFront", nil, nil)
		cancel() // cancelling from inside the handler is allowed
		done <- err
	})
	defer cancel()

	f.event("S1", EventLifecycleEvent, map[string]any{"name": "load"})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Call from handler: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler deadlocked the connection")
	}
}

func TestEventQueueOverflowDropsOldest(t *testing.T) {
	t.Parallel()
	f := newFake(t, Options{EventQueue: 4}, nil)

	release := make(chan struct{})
	got := make(chan int, 64)
	var once sync.Once
	defer On(f.conn.Session("S1"), EventLifecycleEvent, func(ev LifecycleEvent) {
		once.Do(func() { <-release }) // stall while the queue fills
		got <- int(ev.Timestamp)
	})()

	const n = 20
	for i := range n {
		f.event("S1", EventLifecycleEvent, map[string]any{"name": "load", "timestamp": i})
	}
	close(release)

	// The read loop never blocked, so the connection is alive, and what does
	// arrive is still in order — just with a hole where the drops were.
	var seen []int
	deadline := time.After(time.Second)
collect:
	for {
		select {
		case v := <-got:
			seen = append(seen, v)
		case <-deadline:
			break collect
		case <-time.After(200 * time.Millisecond):
			break collect
		}
	}
	if len(seen) == 0 {
		t.Fatal("nothing delivered")
	}
	if len(seen) >= n {
		t.Fatalf("nothing was dropped (%d events with a queue of 4) — the test no longer covers overflow", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("order lost across a drop: %v", seen)
		}
	}
	if seen[len(seen)-1] != n-1 {
		t.Fatalf("the newest event was dropped instead of the oldest: %v", seen)
	}
	if f.conn.Err() != nil {
		t.Fatalf("connection died: %v", f.conn.Err())
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	// Not parallel: it counts goroutines.
	settle := func() int {
		var n int
		for range 50 {
			runtime.GC()
			time.Sleep(10 * time.Millisecond)
			n = runtime.NumGoroutine()
		}
		return n
	}
	before := settle()

	for range 5 {
		func() {
			f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
				f.reply(cmd.ID, map[string]any{"ok": true})
			})
			cancels := make([]func(), 0, 4)
			for i := range 4 {
				cancels = append(cancels, f.conn.Subscribe(fmt.Sprintf("S%d", i), "", func(json.RawMessage) {}))
			}
			f.event("S1", EventLifecycleEvent, map[string]any{"name": "load"})
			if err := f.conn.Call(ctxT(t), "", "Test.ping", nil, nil); err != nil {
				t.Fatalf("Call: %v", err)
			}
			// Half are cancelled explicitly, half are left to Close.
			cancels[0]()
			cancels[1]()
			if err := f.conn.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			cancels[2]() // after Close, still fine
		}()
	}

	after := settle()
	if after > before+2 {
		t.Fatalf("goroutines: %d before, %d after", before, after)
	}
}

func TestSessionHelpers(t *testing.T) {
	t.Parallel()
	f := newFake(t, Options{}, func(f *fakeBrowser, cmd fakeCommand) {
		switch cmd.Method {
		case "Target.attachToTarget":
			f.reply(cmd.ID, map[string]any{"sessionId": "SID"})
		case "Target.getTargets":
			f.reply(cmd.ID, map[string]any{"targetInfos": []any{
				map[string]any{"targetId": "T1", "type": "page", "url": "about:blank"},
			}})
		case "DOM.getBoxModel":
			f.reply(cmd.ID, map[string]any{"model": map[string]any{
				"content": []float64{10, 20, 30, 20, 30, 40, 10, 40},
				"width":   20, "height": 20,
			}})
		default:
			f.reply(cmd.ID, map[string]any{})
		}
	})

	browser := f.conn.Session("")
	if _, err := browser.CreateTarget(ctxT(t), "about:blank"); err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	sid, err := browser.AttachToTarget(ctxT(t), "T1")
	if err != nil || sid != "SID" {
		t.Fatalf("AttachToTarget = %q, %v", sid, err)
	}
	targets, err := browser.GetTargets(ctxT(t))
	if err != nil || len(targets) != 1 || targets[0].TargetID != "T1" {
		t.Fatalf("GetTargets = %+v, %v", targets, err)
	}

	page := f.conn.Session(sid)
	if page.ID() != "SID" || page.Conn() != f.conn {
		t.Fatal("Session accessors")
	}
	box, err := page.GetBoxModel(ctxT(t), NodeRef{BackendNodeID: 7})
	if err != nil {
		t.Fatalf("GetBoxModel: %v", err)
	}
	if x, y := box.Content.Center(); x != 20 || y != 30 {
		t.Fatalf("centre = %v,%v want 20,30", x, y)
	}
	if x, y := (Quad{1, 2}).Center(); x != 0 || y != 0 {
		t.Fatal("short quad should not panic")
	}

	// attachToTarget is always flat: the whole package assumes flat sessions.
	for _, c := range f.commands() {
		if c.Method == "Target.attachToTarget" && !strings.Contains(string(c.Params), `"flatten":true`) {
			t.Fatalf("attachToTarget not flattened: %s", c.Params)
		}
	}
}

func TestNodeAndSnapshotAccessors(t *testing.T) {
	t.Parallel()
	n := &Node{Attributes: []string{"id", "go", "class", "btn"}}
	if v, ok := n.Attr("class"); !ok || v != "btn" {
		t.Fatalf("Attr = %q, %v", v, ok)
	}
	if _, ok := n.Attr("missing"); ok {
		t.Fatal("Attr found a missing attribute")
	}

	snap := &Snapshot{Strings: []string{"a", "b"}}
	if snap.Str(1) != "b" || snap.Str(-1) != "" || snap.Str(9) != "" {
		t.Fatal("Str")
	}
	rs := RareStringData{Index: []int{3}, Value: []StrIdx{1}}
	if v, ok := rs.Get(3); !ok || v != 1 {
		t.Fatalf("RareStringData.Get = %v %v", v, ok)
	}
	if _, ok := rs.Get(4); ok {
		t.Fatal("RareStringData.Get found a missing node")
	}
	ri := RareIntegerData{Index: []int{2}, Value: []int{5}}
	if v, ok := ri.Get(2); !ok || v != 5 {
		t.Fatalf("RareIntegerData.Get = %v %v", v, ok)
	}
	if _, ok := ri.Get(0); ok {
		t.Fatal("RareIntegerData.Get found a missing node")
	}
	rb := RareBooleanData{Index: []int{1, 4}}
	if !rb.Has(4) || rb.Has(2) {
		t.Fatal("RareBooleanData.Has")
	}

	ax := &AXValue{Type: "string", Value: "Submit"}
	if ax.Text() != "Submit" {
		t.Fatal("AXValue.Text")
	}
	var nilAX *AXValue
	if nilAX.Text() != "" || nilAX.Bool() {
		t.Fatal("nil AXValue accessors must be safe")
	}
	if (&AXValue{Type: "booleanOrUndefined", Value: true}).Bool() != true {
		t.Fatal("AXValue.Bool")
	}
}
