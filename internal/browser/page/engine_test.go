package page

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"holaclaw/internal/browser/cdp"
	"holaclaw/internal/browser/chromium"
	"holaclaw/internal/browser/proto"
)

// syncBuffer is a log sink the tests can read while a handler writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestNewRequiresAStateDir(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New with no state directory succeeded")
	}
	dir := t.TempDir()
	e, err := New(Options{StateDir: dir})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = e.Shutdown(t.Context()) }()
	fi, err := os.Stat(filepath.Join(dir, "downloads"))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("quarantine root: %v, %v", fi, err)
	}
	if e.pace != DefaultPacing {
		t.Fatalf("pacing = %v, want the default", e.pace)
	}
	zero := 0.0
	e2, err := New(Options{StateDir: t.TempDir(), Pacing: &zero})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e2.Shutdown(t.Context()) }()
	if e2.pace != 0 {
		t.Fatalf("pacing = %v, want it switched off", e2.pace)
	}
	// Pacing 0 means no delay at all: the pause returns immediately.
	start := time.Now()
	e2.pause(t.Context(), time.Second)
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("pause slept with pacing 0")
	}
}

// Without a session every call is no_session, and nothing starts a browser.
func TestCallsWithoutASessionAreRefused(t *testing.T) {
	e := testEngineNoBrowser(t)
	if _, err := e.Observe(t.Context(), "ghost", proto.ObserveRequest{}); !isCode(err, proto.ErrNoSession) {
		t.Errorf("Observe = %v", err)
	}
	res, err := e.Act(t.Context(), "ghost", proto.Action{Kind: proto.ActClick, Ref: "r1"})
	if !isCode(err, proto.ErrNoSession) || res.Code != proto.ErrNoSession || res.OK {
		t.Errorf("Act = %+v, %v", res, err)
	}
	if _, err := e.Screenshot(t.Context(), "ghost"); !isCode(err, proto.ErrNoSession) {
		t.Errorf("Screenshot = %v", err)
	}
	if err := e.Close(t.Context(), "ghost"); !isCode(err, proto.ErrNoSession) {
		t.Errorf("Close = %v", err)
	}
	if _, _, err := e.Download(t.Context(), "ghost", "01J"); !isCode(err, proto.ErrNoSession) {
		t.Errorf("Download = %v", err)
	}
	if err := e.Stream(t.Context(), "ghost", nil, nil); !isCode(err, proto.ErrNoSession) {
		t.Errorf("Stream = %v", err)
	}
	if got := e.Sessions(t.Context()); len(got) != 0 {
		t.Errorf("Sessions = %+v", got)
	}
}

// One live view per agent: the second is told a person already holds it.
func TestSecondStreamIsHeld(t *testing.T) {
	e := testEngineNoBrowser(t)
	s := &session{e: e, agent: "alpha"}
	s.streaming.Store(true)
	e.sessions["alpha"] = s
	if err := e.Stream(t.Context(), "alpha", nil, nil); !isCode(err, proto.ErrHeld) {
		t.Fatalf("second Stream = %v, want held", err)
	}
}

// Health never starts a browser and says, in one fixed sentence, why one
// cannot run here.
func TestHealth(t *testing.T) {
	e := testEngineNoBrowser(t)
	h := e.Health(t.Context())
	if h.Problem == "" || !strings.Contains(h.Problem, "browser") {
		t.Fatalf("health with no binary = %+v", h)
	}

	e.opts.Binary = chromium.Binary{Path: "/usr/bin/chromium", Version: "100.0.0.0", Major: 100}
	if h := e.Health(t.Context()); !strings.Contains(h.Problem, "major") {
		t.Fatalf("health with an old browser = %+v", h)
	}
	e.opts.Binary = chromium.Binary{Path: "/usr/bin/chromium", Version: "153.0.0.0", Major: 153}
	e.opts.Headless = true
	h = e.Health(t.Context())
	if h.Problem != "" || h.Browser != "153.0.0.0" || !h.Headless {
		t.Fatalf("health = %+v", h)
	}
}

func TestRefsAreStaleAfterTheNextObservation(t *testing.T) {
	at := &attachment{refs: map[string]elemRef{"r1": {backend: 7, session: "S1"}}}
	got, err := at.resolve("r1")
	if err != nil || got.backend != 7 {
		t.Fatalf("resolve = %+v, %v", got, err)
	}
	if _, err := at.resolve("r9"); !isCode(err, proto.ErrStaleRef) {
		t.Fatalf("unknown ref = %v, want stale_ref", err)
	}
	if _, err := at.resolve(""); !isCode(err, proto.ErrBadAction) {
		t.Fatalf("empty ref = %v, want bad_action", err)
	}
	// A new observation replaces the map wholesale.
	at.mu.Lock()
	at.refs = map[string]elemRef{"r1": {backend: 99}}
	at.mu.Unlock()
	if got, _ := at.resolve("r1"); got.backend != 99 {
		t.Fatal("the ref map was not replaced")
	}
}

// TestARefNeverMeansADifferentElement is the rule behind the generation in a
// ref. Positional names alone are not enough: open a select and twenty options
// enter document order, so the name the search button had now belongs to an
// option, and a queued move clicks the wrong thing with no error anywhere. A
// ref names the observation that minted it, so the old one is simply not
// found — which is the one outcome the executor can recover from.
func TestARefNeverMeansADifferentElement(t *testing.T) {
	at := &attachment{tabs: map[string]*tab{}, refs: map[string]elemRef{}}

	mint := func(gen uint64, backends ...int64) {
		refs := map[string]elemRef{}
		for i, b := range backends {
			refs[refName(gen, i+1)] = elemRef{backend: b, session: "S1"}
		}
		at.mu.Lock()
		at.gen, at.refs = gen, refs
		at.mu.Unlock()
	}

	// First observation: the search button is the second element.
	mint(1, 11, 22)
	button := refName(1, 2)
	got, err := at.resolve(button)
	if err != nil || got.backend != 22 {
		t.Fatalf("resolve = %+v, %v", got, err)
	}

	// The select opens: the same position is now an option of the dropdown.
	mint(2, 11, 33, 44, 22)
	if _, err := at.resolve(button); !isCode(err, proto.ErrStaleRef) {
		t.Fatalf("a ref of the previous observation resolved to %v, want stale_ref", err)
	}
	if got, err := at.resolve(refName(2, 4)); err != nil || got.backend != 22 {
		t.Fatalf("the button of this observation = %+v, %v", got, err)
	}
}

// The rate limit is wall clock and per agent, whatever the pacing says.
func TestRateLimit(t *testing.T) {
	s := &session{}
	s.rateLimit(t.Context())
	start := time.Now()
	s.rateLimit(t.Context())
	if d := time.Since(start); d < actInterval/2 {
		t.Fatalf("two actions %v apart, want at least ~%v", d, actInterval)
	}
}

func TestTabBookkeeping(t *testing.T) {
	at := &attachment{tabs: map[string]*tab{}, refs: map[string]elemRef{}}
	for _, id := range []string{"T1", "T2"} {
		at.tabs[id] = &tab{targetID: id, url: "about:blank", frames: map[string]string{}}
		at.order = append(at.order, id)
		at.active = id
	}
	tabs := at.tabList()
	if len(tabs) != 2 || !tabs[1].Active || tabs[0].Active {
		t.Fatalf("tabs = %+v", tabs)
	}
	at.onDestroyed("T2")
	tabs = at.tabList()
	if len(tabs) != 1 || tabs[0].ID != "T1" || !tabs[0].Active {
		t.Fatalf("after destroy: %+v", tabs)
	}
	at.onDestroyed("T1")
	if _, err := at.activeTab(); !isCode(err, proto.ErrNoSession) {
		t.Fatalf("with no tabs = %v", err)
	}
}

func TestSettleReturnsOnIdle(t *testing.T) {
	at := &attachment{tabs: map[string]*tab{}, refs: map[string]elemRef{}}
	t1 := &tab{targetID: "T1", frames: map[string]string{}}
	at.tabs["T1"] = t1
	lifecycle, stop := at.watch(t1)
	defer stop()

	go func() {
		at.onLifecycle(t1, cdpLifecycle("load"))
		at.onLifecycle(t1, cdpLifecycle("networkIdle"))
	}()
	start := time.Now()
	at.settle(t.Context(), lifecycle, true)
	if time.Since(start) > 2*time.Second {
		t.Fatal("settle did not return on networkIdle")
	}
	at.mu.Lock()
	loading := t1.loading
	at.mu.Unlock()
	if loading {
		t.Error("the tab is still marked loading after load")
	}
	// Without a navigation the settle window bounds the wait.
	start = time.Now()
	at.settle(t.Context(), lifecycle, false)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("settle without a navigation took %v", d)
	}
}

// cdpLifecycle is one lifecycle event of the main frame.
func cdpLifecycle(name string) cdp.LifecycleEvent {
	return cdp.LifecycleEvent{FrameID: "F1", Name: name}
}

// A quality hint restarts the screencast, which costs the viewer frames. One
// change is applied; a second one straight after is ignored, whoever sends it
// and however the numbers move.
func TestQualityChangesAreDebounced(t *testing.T) {
	f := newFakeCDP(t)
	st := &stream{
		e:           testEngineNoBrowser(t),
		sess:        f.conn.Session("S1"),
		quality:     jpegQuality,
		everyNth:    3,
		lastQuality: time.Now().Add(-2 * qualityInterval),
	}

	st.requality(t.Context(), proto.StreamMessage{Type: proto.StreamQuality, JPEGQuality: 40})
	if st.quality != 40 {
		t.Fatalf("quality = %d, want the first hint applied", st.quality)
	}
	// Everything after it, inside the window, is dropped: the settings stand
	// and the screencast is not restarted again.
	for _, q := range []int{80, 30, 75, 31} {
		st.requality(t.Context(), proto.StreamMessage{Type: proto.StreamQuality, JPEGQuality: q})
	}
	st.requality(t.Context(), proto.StreamMessage{Type: proto.StreamQuality, MaxFPS: 1})
	if st.quality != 40 || st.everyNth != 3 {
		t.Fatalf("quality/everyNth = %d/%d, want the debounced values 40/3", st.quality, st.everyNth)
	}

	waitUntil(t, func() bool { return countMethod(f, "Page.startScreencast") >= 1 })
	if n := countMethod(f, "Page.startScreencast"); n != 1 {
		t.Fatalf("the screencast was restarted %d times, want once", n)
	}
	if n := countMethod(f, "Page.stopScreencast"); n != 1 {
		t.Fatalf("the screencast was stopped %d times, want once", n)
	}

	// Once the window has passed, a hint takes effect again.
	st.lastQuality = time.Now().Add(-2 * qualityInterval)
	st.requality(t.Context(), proto.StreamMessage{Type: proto.StreamQuality, JPEGQuality: 30})
	if st.quality != 30 {
		t.Fatalf("quality = %d, want the hint after the window", st.quality)
	}
	waitUntil(t, func() bool { return countMethod(f, "Page.startScreencast") == 2 })
}

// countMethod is how many times the fake browser saw a method.
func countMethod(f *fakeCDP, method string) int {
	n := 0
	for _, m := range f.methods() {
		if m == method {
			n++
		}
	}
	return n
}

// waitUntil polls until cond holds or the test gives up.
func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition never held")
}

// The subscription cancels are appended from the cdp handler goroutines — a
// new tab, a new out-of-process iframe — while detach takes the list away, so
// they have a lock of their own. Whatever the interleaving, every cancel runs
// exactly once: the ones registered before detach from the list it took, the
// ones that arrive after it straight away.
func TestAttachmentCancelsAreGuarded(t *testing.T) {
	const n = 256
	at := &attachment{}
	var ran atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})

	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			at.addCancels(func() { ran.Add(1) })
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for _, cancel := range at.takeCancels() {
			cancel()
		}
	}()
	close(start)
	wg.Wait()

	// Whatever detach did not see is in the list it left behind.
	for _, cancel := range at.takeCancels() {
		cancel()
	}
	if got := ran.Load(); got != n {
		t.Fatalf("%d of %d subscriptions were cancelled", got, n)
	}
	// A subscription that arrives after detach is cancelled where it stands,
	// not kept in a list nobody reads again.
	at.addCancels(func() { ran.Add(1) })
	if got := ran.Load(); got != n+1 {
		t.Fatalf("a cancel registered after detach did not run: %d", got)
	}
	if left := at.takeCancels(); len(left) != 0 {
		t.Fatalf("%d cancels were kept after detach", len(left))
	}
}

// The wait ActWait makes (quiet.go). What these hold is that nothing in it is
// a fixed sleep: a tab that is not loading waits for the lifecycle not at all,
// a tab that is loading waits exactly until Chromium says it has loaded, and
// the quiet window ends as soon as two readings of the document agree.
func TestWaitLoadedIsFreeOnATabThatIsNotLoading(t *testing.T) {
	at := &attachment{tabs: map[string]*tab{}, refs: map[string]elemRef{}}
	t1 := &tab{targetID: "T1", frames: map[string]string{}}
	at.tabs["T1"] = t1
	lifecycle, stop := at.watch(t1)
	defer stop()

	start := time.Now()
	if !at.waitLoaded(t.Context(), t1, lifecycle) {
		t.Fatal("a tab that is not loading did not pass the lifecycle stage")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("it waited %v for a page that had already arrived", d)
	}
}

func TestWaitLoadedEndsOnTheLifecycleEvent(t *testing.T) {
	at := &attachment{tabs: map[string]*tab{}, refs: map[string]elemRef{}}
	t1 := &tab{targetID: "T1", frames: map[string]string{}, loading: true}
	at.tabs["T1"] = t1
	lifecycle, stop := at.watch(t1)
	defer stop()

	go func() {
		time.Sleep(20 * time.Millisecond)
		at.onLifecycle(t1, cdpLifecycle("load"))
	}()
	if !at.waitLoaded(t.Context(), t1, lifecycle) {
		t.Fatal("the wait did not end on the load event")
	}

	// And a tab that never finishes costs the action's timeout, not the run.
	t1.loading = true
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if at.waitLoaded(ctx, t1, lifecycle) {
		t.Error("a tab that never loaded was reported as loaded")
	}
}

func TestWaitQuietEndsWhenTheDocumentStopsChanging(t *testing.T) {
	f := newFakeCDP(t)
	e := testEngineNoBrowser(t)
	at := &attachment{conn: f.conn, tabs: map[string]*tab{}, refs: map[string]elemRef{}}
	t1 := &tab{targetID: "T1", sessionID: "S1", frames: map[string]string{}}
	at.tabs["T1"] = t1
	lifecycle, stop := at.watch(t1)
	defer stop()

	f.result("Page.getFrameTree", map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "F1"}}})
	f.result("Page.createIsolatedWorld", map[string]any{"executionContextId": 7})
	// The same reading twice: a document that has stopped arriving.
	f.result("Runtime.evaluate", map[string]any{"result": map[string]any{"value": map[string]any{
		"ready": "complete", "resources": 12, "text": 900, "elements": 140,
	}}})

	start := time.Now()
	if !e.waitQuiet(t.Context(), at, t1, lifecycle, proto.WaitQuiet) {
		t.Fatal("a page that had stopped was not reported quiet")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the quiet window took %v", d)
	}
	// It read the page in an isolated world, and never enabled Runtime.
	for _, m := range f.methods() {
		if m == "Runtime.enable" {
			t.Fatal("the wait enabled the Runtime domain")
		}
	}
	if _, ok := f.sent("Page.createIsolatedWorld"); !ok {
		t.Error("the reading was not made in an isolated world")
	}
}

func TestWaitQuietGivesUpOnAPageThatNeverStops(t *testing.T) {
	f := newFakeCDP(t)
	e := testEngineNoBrowser(t)
	at := &attachment{conn: f.conn, tabs: map[string]*tab{}, refs: map[string]elemRef{}}
	t1 := &tab{targetID: "T1", sessionID: "S1", frames: map[string]string{}}
	at.tabs["T1"] = t1
	lifecycle, stop := at.watch(t1)
	defer stop()

	f.result("Page.getFrameTree", map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "F1"}}})
	f.result("Page.createIsolatedWorld", map[string]any{"executionContextId": 7})
	// Still loading, whatever the readings say: readyState never completes.
	f.result("Runtime.evaluate", map[string]any{"result": map[string]any{"value": map[string]any{
		"ready": "loading", "resources": 3, "text": 10, "elements": 20,
	}}})

	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	if e.waitQuiet(ctx, at, t1, lifecycle, proto.WaitQuiet) {
		t.Fatal("a page that never finished was reported quiet")
	}
}

// TestAShortBoundStillConfirmsQuiet: a caller in a hurry — the 250 ms a move
// gets to settle — must still be able to come back early. The window between
// the two readings shrinks to fit the bound instead of swallowing it whole.
func TestAShortBoundStillConfirmsQuiet(t *testing.T) {
	f := newFakeCDP(t)
	e := testEngineNoBrowser(t)
	at := &attachment{conn: f.conn, tabs: map[string]*tab{}, refs: map[string]elemRef{}}
	t1 := &tab{targetID: "T1", sessionID: "S1", frames: map[string]string{}}
	at.tabs["T1"] = t1
	lifecycle, stop := at.watch(t1)
	defer stop()

	f.result("Page.getFrameTree", map[string]any{"frameTree": map[string]any{"frame": map[string]any{"id": "F1"}}})
	f.result("Page.createIsolatedWorld", map[string]any{"executionContextId": 7})
	f.result("Runtime.evaluate", map[string]any{"result": map[string]any{"value": map[string]any{
		"ready": "complete", "resources": 12, "text": 900, "elements": 140,
	}}})

	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	if !e.waitQuiet(ctx, at, t1, lifecycle, proto.WaitQuiet) {
		t.Fatal("a settled page was not confirmed within the bound")
	}
	if d := time.Since(start); d >= 250*time.Millisecond {
		t.Errorf("the wait took %v: it spent its whole bound instead of returning early", d)
	}
}

// freshTab attachment: two tabs, the second active, on a fake browser whose
// Target.createTarget answers T3.
func freshTabRig(t *testing.T) (*fakeCDP, *attachment) {
	t.Helper()
	f := newFakeCDP(t)
	f.result("Target.createTarget", map[string]any{"targetId": "T3"})
	s := &session{e: testEngineNoBrowser(t), agent: "muse"}
	at := &attachment{s: s, conn: f.conn, bs: f.conn.Session(""), tabs: map[string]*tab{}, refs: map[string]elemRef{}}
	s.at = at
	for _, id := range []string{"T1", "T2"} {
		at.tabs[id] = &tab{targetID: id, url: "https://news.example/", frames: map[string]string{}}
		at.order = append(at.order, id)
		at.active = id
	}
	at.dialog = "beforeunload"
	return f, at
}

// OpenRequest.Fresh (issue #136): a new blank tab becomes the active one once
// the browser has attached it, and every tab before it is closed — the page a
// previous run left, its dialog and its popup with it.
func TestFreshTabReplacesEveryTab(t *testing.T) {
	f, at := freshTabRig(t)
	go func() {
		// What the browser-level auto-attach does with the new target.
		for countMethod(f, "Target.createTarget") == 0 {
			time.Sleep(time.Millisecond)
		}
		at.mu.Lock()
		at.tabs["T3"] = &tab{targetID: "T3", url: "about:blank", frames: map[string]string{}}
		at.order = append(at.order, "T3")
		at.wakeTabsLocked()
		at.mu.Unlock()
	}()

	if err := at.freshTab(t.Context()); err != nil {
		t.Fatalf("freshTab: %v", err)
	}
	tabs := at.tabList()
	if len(tabs) != 1 || tabs[0].ID != "T3" || !tabs[0].Active || tabs[0].URL != "about:blank" {
		t.Fatalf("tabs = %+v, want the new blank tab alone", tabs)
	}
	if params, _ := f.sent("Target.createTarget"); !strings.Contains(string(params), `"about:blank"`) {
		t.Errorf("createTarget params %s", params)
	}
	var closed []string
	for _, c := range f.commands() {
		if c.Method == "Target.closeTarget" {
			var p struct {
				TargetID string `json:"targetId"`
			}
			_ = json.Unmarshal(c.Params, &p)
			closed = append(closed, p.TargetID)
		}
	}
	if strings.Join(closed, ",") != "T1,T2" {
		t.Errorf("closed %v, want T1 and T2", closed)
	}
	if d := at.dialogKind(); d != "" {
		t.Errorf("the old page's dialog %q outlived it", d)
	}
}

// A new tab the browser never attaches is a failure, not a run on the old
// page: nothing is closed, and the caller is told.
func TestFreshTabThatNeverArrivesClosesNothing(t *testing.T) {
	f, at := freshTabRig(t)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err := at.freshTab(ctx)
	if !isCode(err, proto.ErrTimeout) {
		t.Fatalf("freshTab = %v, want a timeout", err)
	}
	if n := countMethod(f, "Target.closeTarget"); n != 0 {
		t.Errorf("%d tabs closed for a new one that never came", n)
	}
	if tabs := at.tabList(); len(tabs) != 2 {
		t.Errorf("tabs = %+v, want both kept", tabs)
	}
}

// The live view follows the active tab: after a fresh tab replaced the one it
// was showing, the picture moves to the new one rather than staying on a page
// that is gone.
func TestStreamFollowsTheActiveTab(t *testing.T) {
	f, at := freshTabRig(t)
	at.tabs["T1"].sessionID, at.tabs["T2"].sessionID = "S1", "S2"
	st := &stream{e: at.s.e, at: at, quality: jpegQuality, everyNth: 3, v: &recordViewer{}}
	if err := st.attach(t.Context(), at.tabs["T1"]); err != nil {
		t.Fatalf("attach: %v", err)
	}
	st.follow(t.Context())
	if st.tab != at.tabs["T2"] {
		t.Fatalf("the live view stayed on %s", st.tab.targetID)
	}
	if n := countMethod(f, "Page.startScreencast"); n != 2 {
		t.Errorf("%d screencasts started, want the first tab's and the active one's", n)
	}
	// Already on the active tab: nothing to restart.
	st.follow(t.Context())
	if n := countMethod(f, "Page.startScreencast"); n != 2 {
		t.Errorf("following the tab already shown restarted the screencast (%d)", n)
	}
}
