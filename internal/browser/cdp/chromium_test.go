//go:build linux

package cdp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Integration tests against a real Chromium, over a real --remote-debugging-pipe.
// They are what the rules of docs/browser.md are actually held by; the unit tests only hold
// the client. They skip when no chromium is on PATH or -short is given.
//
// One browser is shared (each test gets its own tab), plus one short-lived
// browser for the launch-flag test. The process group is killed on the way out,
// so nothing outlives the run.

// pageHTML reports what the page sees into its own title: that is how the
// tests read the page's state without ever running script in the main world.
const pageHTML = `<!doctype html>
<html><head><title>start</title></head>
<body style="margin:0">
<h1>HolaClaw test page</h1>
<button id="go" style="position:absolute;left:40px;top:120px;width:180px;height:40px">Order coffee</button>
<label for="q">Search query</label>
<input id="q" type="text" style="position:absolute;left:40px;top:200px;width:220px;height:30px">
<button id="pop" style="position:absolute;left:40px;top:260px;width:180px;height:40px">Open popup</button>
<div id="tick" style="position:absolute;left:40px;top:320px;font:24px monospace">0</div>
<script>
var st = {clicks:0, trusted:"none", text:"", wd:String(navigator.webdriver), n:0};
function render() {
  var iso = (typeof window.__probe === "undefined") ? "none" : "seen";
  st.n++;
  // The tick keeps the page repainting and its title changing, which is what
  // the screencast and targetInfoChanged tests need.
  document.getElementById("tick").textContent = String(st.n);
  document.title = "c=" + st.clicks + " trusted=" + st.trusted +
    " wd=" + st.wd + " iso=" + iso + " text=" + st.text + " n=" + st.n;
}
render();
setInterval(render, 50);
document.getElementById("go").addEventListener("click", function (e) {
  st.clicks++; st.trusted = String(e.isTrusted); render();
});
var q = document.getElementById("q");
q.addEventListener("input", function (e) { st.text = q.value; render(); });
document.getElementById("pop").addEventListener("click", function (e) {
  st.clicks++; st.trusted = String(e.isTrusted); render();
  window.open(location.pathname + "?popup=1", "_blank");
});
</script>
</body></html>`

const popupHTML = `<!doctype html><html><head><title>popup</title></head><body>popup</body></html>`

var (
	sharedOnce sync.Once
	shared     *fixture
	sharedErr  error

	srvOnce sync.Once
	srv     *httptest.Server
)

// fixture is one launched browser and its connection.
type fixture struct {
	cmd *exec.Cmd
	c   *Conn
	dir string
}

func TestMain(m *testing.M) {
	code := m.Run()
	if shared != nil {
		shared.stop()
	}
	if srv != nil {
		srv.Close()
	}
	os.Exit(code)
}

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	srvOnce.Do(func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/page", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if r.URL.Query().Get("popup") != "" {
				_, _ = fmt.Fprint(w, popupHTML)
				return
			}
			_, _ = fmt.Fprint(w, pageHTML)
		})
		srv = httptest.NewServer(mux)
	})
	return srv
}

// launch starts one Chromium wired to a pipe. extraFlags are appended after
// the defaults, which never include --no-sandbox (docs/browser.md).
func launch(t *testing.T, extraFlags ...string) (*fixture, error) {
	t.Helper()
	bin, err := exec.LookPath("chromium")
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "cdp-profile-")
	if err != nil {
		return nil, err
	}

	// fd 3 is what Chromium reads commands from, fd 4 what it writes to.
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}

	args := append([]string{
		"--headless=new",
		"--remote-debugging-pipe",
		"--user-data-dir=" + dir,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-blink-features=AutomationControlled",
	}, extraFlags...)
	args = append(args, "about:blank")

	cmd := exec.Command(bin, args...)
	cmd.ExtraFiles = []*os.File{inR, outW}
	cmd.Env = append(os.Environ(), "HOME="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if testing.Verbose() {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// The child owns its ends now.
	_ = inR.Close()
	_ = outW.Close()

	f := &fixture{cmd: cmd, dir: dir}
	f.c = New(outR, inW, Options{})
	return f, nil
}

// stop closes the connection and kills the whole process group: a renderer or
// zygote must not outlive the test run.
func (f *fixture) stop() {
	if f == nil {
		return
	}
	if f.c != nil {
		_ = f.c.Close()
	}
	if f.cmd != nil && f.cmd.Process != nil {
		pid := f.cmd.Process.Pid
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		done := make(chan struct{})
		go func() { _, _ = f.cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	if f.dir != "" {
		_ = os.RemoveAll(f.dir)
	}
}

// browser returns the shared Chromium, skipping the test when there is none.
func browser(t *testing.T) *Conn {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping: -short")
	}
	if _, err := exec.LookPath("chromium"); err != nil {
		t.Skip("skipping: no chromium on PATH")
	}
	sharedOnce.Do(func() {
		shared, sharedErr = launch(t)
		if sharedErr != nil {
			return
		}
		// A first command proves the pipe and the sandbox: Chromium exits
		// rather than run without one, and then this never answers.
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var v *Version
		v, sharedErr = shared.c.Session("").GetVersion(ctx)
		if sharedErr == nil {
			t.Logf("browser: %s (%s)", v.Product, v.Revision)
			t.Logf("user agent: %s", v.UserAgent)
		}
	})
	if sharedErr != nil {
		t.Fatalf("chromium did not start over a pipe (no --no-sandbox was used): %v", sharedErr)
	}
	return shared.c
}

// page opens a tab on the test server, waits for load, and returns its
// session, target id and main frame id.
func page(t *testing.T, c *Conn, query string) (*Session, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	browserSess := c.Session("")
	targetID, err := browserSess.CreateTarget(ctx, "about:blank")
	if err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = browserSess.CloseTarget(ctx, targetID)
	})

	sid, err := browserSess.AttachToTarget(ctx, targetID)
	if err != nil {
		t.Fatalf("AttachToTarget: %v", err)
	}
	s := c.Session(sid)
	if err := s.PageEnable(ctx); err != nil {
		t.Fatalf("Page.enable: %v", err)
	}
	if err := s.SetLifecycleEventsEnabled(ctx, true); err != nil {
		t.Fatalf("Page.setLifecycleEventsEnabled: %v", err)
	}

	loaded := make(chan struct{}, 1)
	cancelSub := On(s, EventLifecycleEvent, func(ev LifecycleEvent) {
		if ev.Name == "load" {
			select {
			case loaded <- struct{}{}:
			default:
			}
		}
	})
	defer cancelSub()

	url := testServer(t).URL + "/page" + query
	frameID, errText, err := s.Navigate(ctx, url)
	if err != nil {
		t.Fatalf("Page.navigate: %v", err)
	}
	if errText != "" {
		t.Fatalf("Page.navigate: %s", errText)
	}
	select {
	case <-loaded:
	case <-time.After(20 * time.Second):
		t.Fatal("no load lifecycle event")
	}
	return s, targetID, frameID
}

// title polls the page's title through Target.getTargetInfo — no main-world
// script anywhere in the loop.
func title(t *testing.T, s *Session, targetID string, want func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		info, err := s.GetTargetInfo(ctx, targetID)
		cancel()
		if err != nil {
			t.Fatalf("Target.getTargetInfo: %v", err)
		}
		last = info.Title
		if want == nil || want(last) {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last
}

func TestChromiumNavigateAndLifecycle(t *testing.T) {
	c := browser(t)
	s, targetID, frameID := page(t, c, "")
	ctx := ctxT(t)

	if frameID == "" {
		t.Fatal("no frame id from navigate")
	}
	tree, err := s.GetFrameTree(ctx)
	if err != nil {
		t.Fatalf("Page.getFrameTree: %v", err)
	}
	if tree.Frame.ID != frameID {
		t.Fatalf("frame tree root = %q, navigate said %q", tree.Frame.ID, frameID)
	}
	if !strings.Contains(tree.Frame.URL, "/page") {
		t.Fatalf("frame url = %q", tree.Frame.URL)
	}

	info, err := s.GetTargetInfo(ctx, targetID)
	if err != nil {
		t.Fatalf("Target.getTargetInfo: %v", err)
	}
	if info.Type != "page" || !strings.Contains(info.URL, "/page") {
		t.Fatalf("target info = %+v", info)
	}

	// The launch flag does its job: the page sees no webdriver.
	got := title(t, s, targetID, func(s string) bool { return strings.Contains(s, "wd=") })
	if !strings.Contains(got, "wd=false") {
		t.Fatalf("navigator.webdriver leaked: title = %q", got)
	}

	// History is readable and navigating back works.
	idx, entries, err := s.GetNavigationHistory(ctx)
	if err != nil {
		t.Fatalf("Page.getNavigationHistory: %v", err)
	}
	if idx < 1 || len(entries) < 2 {
		t.Fatalf("history idx=%d entries=%d", idx, len(entries))
	}
	if err := s.NavigateToHistoryEntry(ctx, entries[idx-1].ID); err != nil {
		t.Fatalf("Page.navigateToHistoryEntry: %v", err)
	}
}

func TestChromiumAccessibilityTree(t *testing.T) {
	c := browser(t)
	s, _, _ := page(t, c, "")
	ctx := ctxT(t)

	// Deliberately without Accessibility.enable: the question is whether the
	// hot path needs it.
	nodes, err := s.GetFullAXTree(ctx, 0, "")
	if err != nil {
		t.Fatalf("Accessibility.getFullAXTree without enable: %v", err)
	}
	if len(nodes) == 0 {
		t.Fatal("empty accessibility tree")
	}

	find := func(role, name string) *AXNode {
		for i := range nodes {
			n := &nodes[i]
			if n.Role.Text() == role && n.Name.Text() == name {
				return n
			}
		}
		return nil
	}
	button := find("button", "Order coffee")
	if button == nil {
		t.Fatalf("button not in the tree (%d nodes)", len(nodes))
	}
	if button.BackendDOMNodeID == 0 {
		t.Fatal("button has no backendDOMNodeId to act on")
	}
	if find("textbox", "Search query") == nil {
		t.Fatal("labelled textbox not in the tree")
	}

	// And it still works after enable, so the sidecar may do either.
	if err := s.AccessibilityEnable(ctx); err != nil {
		t.Fatalf("Accessibility.enable: %v", err)
	}
	after, err := s.GetFullAXTree(ctx, 0, "")
	if err != nil || len(after) == 0 {
		t.Fatalf("getFullAXTree after enable: %d nodes, %v", len(after), err)
	}

	// queryAXTree finds the same button from the document root.
	doc, err := s.GetDocument(ctx, 1, false)
	if err != nil {
		t.Fatalf("DOM.getDocument: %v", err)
	}
	hits, err := s.QueryAXTree(ctx, NodeRef{NodeID: doc.NodeID}, "Order coffee", "")
	if err != nil {
		t.Fatalf("Accessibility.queryAXTree: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("queryAXTree found nothing")
	}
}

func TestChromiumDOMSnapshot(t *testing.T) {
	c := browser(t)
	s, _, _ := page(t, c, "")
	ctx := ctxT(t)

	snap, err := s.CaptureSnapshot(ctx, SnapshotOptions{ComputedStyles: []string{"display", "visibility"}})
	if err != nil {
		t.Fatalf("DOMSnapshot.captureSnapshot: %v", err)
	}
	if len(snap.Documents) == 0 || len(snap.Strings) == 0 {
		t.Fatalf("snapshot: %d documents, %d strings", len(snap.Documents), len(snap.Strings))
	}
	doc := snap.Documents[0]
	if !strings.Contains(snap.Str(doc.DocumentURL), "/page") {
		t.Fatalf("document url = %q", snap.Str(doc.DocumentURL))
	}
	n := doc.Nodes
	if len(n.NodeName) == 0 || len(n.ParentIndex) != len(n.NodeName) || len(n.BackendNodeID) != len(n.NodeName) {
		t.Fatalf("parallel arrays disagree: names=%d parents=%d backend=%d",
			len(n.NodeName), len(n.ParentIndex), len(n.BackendNodeID))
	}

	var buttons, clickable int
	for i := range n.NodeName {
		if snap.Str(n.NodeName[i]) == "BUTTON" {
			buttons++
			if n.IsClickable.Has(i) {
				clickable++
			}
		}
	}
	if buttons != 2 {
		t.Fatalf("found %d buttons, want 2", buttons)
	}
	if clickable == 0 {
		t.Fatal("no button was marked clickable")
	}
	if len(doc.Layout.NodeIndex) == 0 || len(doc.Layout.Bounds) != len(doc.Layout.NodeIndex) {
		t.Fatalf("layout: %d indexes, %d bounds", len(doc.Layout.NodeIndex), len(doc.Layout.Bounds))
	}

	// The input's value is in the sparse column once it has one.
	if err := typeInto(ctx, s, "espresso"); err != nil {
		t.Fatalf("typing: %v", err)
	}
	snap2, err := s.CaptureSnapshot(ctx, SnapshotOptions{ComputedStyles: []string{}})
	if err != nil {
		t.Fatalf("second captureSnapshot: %v", err)
	}
	d2 := snap2.Documents[0]
	var found bool
	for i := range d2.Nodes.NodeName {
		if snap2.Str(d2.Nodes.NodeName[i]) != "INPUT" {
			continue
		}
		if v, ok := d2.Nodes.InputValue.Get(i); ok && strings.Contains(snap2.Str(v), "espresso") {
			found = true
		}
	}
	if !found {
		t.Fatal("inputValue not in the snapshot")
	}
}

func TestChromiumInputIsTrusted(t *testing.T) {
	c := browser(t)
	s, targetID, _ := page(t, c, "")
	ctx := ctxT(t)

	// Find the button through the accessibility tree, aim at its box-model
	// centre, and click it with real input events.
	nodes, err := s.GetFullAXTree(ctx, 0, "")
	if err != nil {
		t.Fatalf("getFullAXTree: %v", err)
	}
	var backendID int64
	for i := range nodes {
		if nodes[i].Role.Text() == "button" && nodes[i].Name.Text() == "Order coffee" {
			backendID = nodes[i].BackendDOMNodeID
		}
	}
	if backendID == 0 {
		t.Fatal("button not found")
	}
	ref := NodeRef{BackendNodeID: backendID}
	if err := s.ScrollIntoViewIfNeeded(ctx, ref); err != nil {
		t.Fatalf("DOM.scrollIntoViewIfNeeded: %v", err)
	}
	box, err := s.GetBoxModel(ctx, ref)
	if err != nil {
		t.Fatalf("DOM.getBoxModel: %v", err)
	}
	x, y := box.Content.Center()
	if x <= 0 || y <= 0 {
		t.Fatalf("centre = %v,%v", x, y)
	}

	// Hit-testing the centre must land on the button itself.
	hit, _, _, err := s.GetNodeForLocation(ctx, int(x), int(y))
	if err != nil {
		t.Fatalf("DOM.getNodeForLocation: %v", err)
	}
	if hit != backendID {
		t.Fatalf("hit test = %d, want the button %d", hit, backendID)
	}
	if quads, err := s.GetContentQuads(ctx, ref); err != nil || len(quads) == 0 {
		t.Fatalf("DOM.getContentQuads = %d quads, %v", len(quads), err)
	}

	for _, ev := range []MouseEvent{
		{Type: "mouseMoved", X: x, Y: y, Button: "none"},
		{Type: "mousePressed", X: x, Y: y, Button: "left", Buttons: 1, ClickCount: 1},
		{Type: "mouseReleased", X: x, Y: y, Button: "left", Buttons: 0, ClickCount: 1},
	} {
		if err := s.DispatchMouseEvent(ctx, ev); err != nil {
			t.Fatalf("Input.dispatchMouseEvent %s: %v", ev.Type, err)
		}
	}
	got := title(t, s, targetID, func(s string) bool { return strings.Contains(s, "c=1") })
	if !strings.Contains(got, "c=1") {
		t.Fatalf("click not seen: title = %q", got)
	}
	if !strings.Contains(got, "trusted=true") {
		t.Fatalf("dispatched click was not isTrusted: title = %q", got)
	}

	// Wheel events go through the same call.
	err = s.DispatchMouseEvent(ctx, MouseEvent{Type: "mouseWheel", X: x, Y: y, DeltaX: 0, DeltaY: 100})
	if err != nil {
		t.Fatalf("mouseWheel: %v", err)
	}

	// Type into the textbox: per-key events, then insertText for the rest.
	if err := typeInto(ctx, s, "flat"); err != nil {
		t.Fatalf("typing: %v", err)
	}
	got = title(t, s, targetID, func(s string) bool { return strings.Contains(s, "text=aflat") })
	if !strings.Contains(got, "text=aflat") {
		t.Fatalf("typing not seen: title = %q", got)
	}
}

// typeInto focuses the test page's input, sends one real key and then
// insertText. It lives here because it is test scaffolding, not API.
func typeInto(ctx context.Context, s *Session, text string) error {
	doc, err := s.GetDocument(ctx, -1, false)
	if err != nil {
		return err
	}
	var input *Node
	var walk func(*Node)
	walk = func(n *Node) {
		if n.NodeName == "INPUT" && input == nil {
			input = n
		}
		for i := range n.Children {
			walk(&n.Children[i])
		}
	}
	walk(doc)
	if input == nil {
		return errors.New("no input in the document")
	}
	if err := s.Focus(ctx, NodeRef{NodeID: input.NodeID}); err != nil {
		return err
	}
	key := KeyEvent{
		Type: "keyDown", Key: "a", Code: "KeyA", Text: "a", UnmodifiedText: "a",
		WindowsVirtualKeyCode: 65, NativeVirtualKeyCode: 65,
	}
	if err := s.DispatchKeyEvent(ctx, key); err != nil {
		return err
	}
	key.Type = "keyUp"
	key.Text = ""
	if err := s.DispatchKeyEvent(ctx, key); err != nil {
		return err
	}
	return s.InsertText(ctx, text)
}

func TestChromiumScreenshotAndScreencast(t *testing.T) {
	c := browser(t)
	s, _, _ := page(t, c, "")
	ctx := ctxT(t)

	img, err := s.CaptureScreenshot(ctx, "jpeg", 60)
	if err != nil {
		t.Fatalf("Page.captureScreenshot: %v", err)
	}
	if len(img) < 1024 || img[0] != 0xFF || img[1] != 0xD8 {
		t.Fatalf("not a JPEG: %d bytes, prefix %x", len(img), img[:min(4, len(img))])
	}

	frames := make(chan ScreencastFrameEvent, 16)
	cancel := On(s, EventScreencastFrame, func(ev ScreencastFrameEvent) {
		select {
		case frames <- ev:
		default:
		}
	})
	defer cancel()

	if err := s.StartScreencast(ctx, "jpeg", 60, 640, 400, 1); err != nil {
		t.Fatalf("Page.startScreencast: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.StopScreencast(ctx)
	}()

	// The page repaints its own title on a timer, so frames keep coming.
	var acked int
	deadline := time.After(15 * time.Second)
	for acked < 3 {
		select {
		case ev := <-frames:
			if len(ev.Data) == 0 {
				t.Fatal("empty screencast frame")
			}
			if ev.Metadata.DeviceWidth <= 0 {
				t.Fatalf("frame metadata = %+v", ev.Metadata)
			}
			if err := s.ScreencastFrameAck(ctx, ev.SessionID); err != nil {
				t.Fatalf("Page.screencastFrameAck: %v", err)
			}
			acked++
		case <-deadline:
			t.Fatalf("only %d screencast frames arrived in headless mode", acked)
		}
	}
}

func TestChromiumEvalIsolated(t *testing.T) {
	c := browser(t)
	s, targetID, frameID := page(t, c, "")
	ctx := ctxT(t)

	var sum int
	if err := s.EvalIsolated(ctx, frameID, "1 + 2", &sum); err != nil {
		t.Fatalf("EvalIsolated: %v", err)
	}
	if sum != 3 {
		t.Fatalf("sum = %d", sum)
	}

	// The isolated world shares the DOM but not the script world.
	var heading string
	err := s.EvalIsolated(ctx, frameID, "document.querySelector('h1').textContent", &heading)
	if err != nil {
		t.Fatalf("EvalIsolated on the DOM: %v", err)
	}
	if heading != "HolaClaw test page" {
		t.Fatalf("heading = %q", heading)
	}

	var wd bool
	if err := s.EvalIsolated(ctx, frameID, "navigator.webdriver === true", &wd); err != nil {
		t.Fatalf("EvalIsolated webdriver: %v", err)
	}
	if wd {
		t.Fatal("navigator.webdriver is true")
	}

	// A variable set in the isolated world is invisible to the page.
	var set string
	err = s.EvalIsolated(ctx, frameID, "window.__probe = 'x'; window.__probe", &set)
	if err != nil {
		t.Fatalf("EvalIsolated set: %v", err)
	}
	if set != "x" {
		t.Fatalf("set = %q", set)
	}
	// The page retitles itself every 50 ms, so one settle is enough for it to
	// have reported what it can see — and it must still be "none".
	time.Sleep(300 * time.Millisecond)
	got := title(t, s, targetID, nil)
	if !strings.Contains(got, "iso=none") {
		t.Fatalf("the main world can see the isolated world's variable: title = %q", got)
	}

	// And the denylist still refuses the unsanctioned route against a real
	// browser: nothing is written to the pipe.
	if err := s.Call(ctx, "Runtime.enable", nil, nil); !errors.Is(err, ErrDenied) {
		t.Fatalf("Runtime.enable = %v, want ErrDenied", err)
	}
	if err := s.Call(ctx, "Runtime.evaluate", map[string]any{"expression": "1"}, nil); !errors.Is(err, ErrDenied) {
		t.Fatalf("main-world evaluate = %v, want ErrDenied", err)
	}
	// The connection is unharmed.
	if _, err := s.GetTargetInfo(ctx, targetID); err != nil {
		t.Fatalf("after denials: %v", err)
	}
}

// TestChromiumPopupAutoAttach holds the rule that a window the page opens
// cannot escape the sidecar: browser-level auto-attach hands us a flat session
// for it before it has navigated, which is where the allow-list will be
// applied.
func TestChromiumPopupAutoAttach(t *testing.T) {
	c := browser(t)
	ctx := ctxT(t)
	bs := c.Session("")

	attached := make(chan AttachedToTargetEvent, 16)
	defer On(bs, EventAttachedToTarget, func(ev AttachedToTargetEvent) {
		// A target that starts paused must be released, or the opener's input
		// event never gets acknowledged. This runs off the read loop, so it
		// can call back into the connection while a Call is in flight.
		if ev.WaitingForDebugger && ev.SessionID != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := c.Session(ev.SessionID).RunIfWaitingForDebugger(ctx); err != nil {
				t.Logf("runIfWaitingForDebugger: %v", err)
			}
		}
		select {
		case attached <- ev:
		default:
		}
	})()

	// Auto-attach is set on the browser session before the tab exists: that is
	// the order the sidecar will use, and the only one that catches a popup.
	if err := bs.SetAutoAttach(ctx, true, true); err != nil {
		t.Fatalf("Target.setAutoAttach: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = bs.SetAutoAttach(ctx, false, false)
	})

	targetID, err := bs.CreateTarget(ctx, "about:blank")
	if err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = bs.CloseTarget(ctx, targetID)
	})

	// The tab arrives as an auto-attach rather than an explicit attachToTarget.
	var sid string
	for sid == "" {
		select {
		case ev := <-attached:
			if ev.TargetInfo.TargetID == targetID {
				sid = ev.SessionID
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the new tab was not auto-attached")
		}
	}
	s := c.Session(sid)

	if err := s.PageEnable(ctx); err != nil {
		t.Fatalf("Page.enable: %v", err)
	}
	if err := s.SetLifecycleEventsEnabled(ctx, true); err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
	loaded := make(chan struct{}, 1)
	defer On(s, EventLifecycleEvent, func(ev LifecycleEvent) {
		if ev.Name == "load" {
			select {
			case loaded <- struct{}{}:
			default:
			}
		}
	})()
	if _, errText, err := s.Navigate(ctx, testServer(t).URL+"/page"); err != nil || errText != "" {
		t.Fatalf("navigate: %v %s", err, errText)
	}
	select {
	case <-loaded:
	case <-time.After(20 * time.Second):
		t.Fatal("no load")
	}

	// Click "Open popup" with a real, trusted gesture so the popup blocker
	// lets it through.
	nodes, err := s.GetFullAXTree(ctx, 0, "")
	if err != nil {
		t.Fatalf("getFullAXTree: %v", err)
	}
	var backendID int64
	for i := range nodes {
		// Role matters: StaticText and InlineTextBox carry the same name and
		// no backend node id.
		if nodes[i].Role.Text() == "button" && nodes[i].Name.Text() == "Open popup" {
			backendID = nodes[i].BackendDOMNodeID
		}
	}
	if backendID == 0 {
		t.Fatalf("popup button not found among %d nodes", len(nodes))
	}
	box, err := s.GetBoxModel(ctx, NodeRef{BackendNodeID: backendID})
	if err != nil {
		t.Fatalf("getBoxModel: %v", err)
	}
	x, y := box.Content.Center()
	for _, ev := range []MouseEvent{
		{Type: "mousePressed", X: x, Y: y, Button: "left", Buttons: 1, ClickCount: 1},
		{Type: "mouseReleased", X: x, Y: y, Button: "left", Buttons: 0, ClickCount: 1},
	} {
		// The release runs the page's handler, which calls window.open. With
		// browser-level auto-attach Chromium does not acknowledge that input
		// event until the new target has been released, so this only returns
		// because the subscription handler above does that — off the read loop.
		if err := s.DispatchMouseEvent(ctx, ev); err != nil {
			t.Fatalf("dispatchMouseEvent %s: %v", ev.Type, err)
		}
	}
	if got := title(t, s, targetID, func(s string) bool { return strings.Contains(s, "c=1") }); !strings.Contains(got, "c=1") {
		t.Fatalf("the click did not land: title = %q", got)
	}

	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev := <-attached:
			// A popup is auto-attached as soon as it exists, before it has
			// navigated: the URL on the event is still about:blank.
			if ev.TargetInfo.Type != "page" || ev.TargetInfo.TargetID == targetID {
				continue
			}
			if ev.SessionID == "" {
				t.Fatal("auto-attached without a flat session id")
			}
			popupTarget := ev.TargetInfo.TargetID
			popup := c.Session(ev.SessionID)
			t.Logf("popup attached: %+v waitingForDebugger=%v", ev.TargetInfo, ev.WaitingForDebugger)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = bs.CloseTarget(ctx, popupTarget)
			})
			if ev.TargetInfo.OpenerID != targetID {
				t.Logf("popup openerId = %q (tab is %q)", ev.TargetInfo.OpenerID, targetID)
			}
			// The flat session works: commands answer on it.
			got := ""
			for until := time.Now().Add(10 * time.Second); time.Now().Before(until); {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				info, err := popup.GetTargetInfo(ctx, "")
				cancel()
				if err != nil {
					t.Fatalf("popup getTargetInfo: %v", err)
				}
				got = info.URL
				if strings.Contains(got, "popup=1") {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatalf("popup url = %q, want the popup page", got)
		case <-deadline:
			t.Fatal("the popup was not auto-attached")
		}
	}
}

func TestChromiumTargetsAndDiscovery(t *testing.T) {
	c := browser(t)
	s, targetID, _ := page(t, c, "")
	ctx := ctxT(t)

	bs := c.Session("")
	changed := make(chan TargetInfoChangedEvent, 32)
	defer On(bs, EventTargetInfoChanged, func(ev TargetInfoChangedEvent) {
		select {
		case changed <- ev:
		default:
		}
	})()
	if err := bs.SetDiscoverTargets(ctx, true); err != nil {
		t.Fatalf("Target.setDiscoverTargets: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = bs.SetDiscoverTargets(ctx, false)
	}()

	targets, err := bs.GetTargets(ctx)
	if err != nil {
		t.Fatalf("Target.getTargets: %v", err)
	}
	var found bool
	for _, ti := range targets {
		if ti.TargetID == targetID {
			found = true
		}
	}
	if !found {
		t.Fatalf("our tab is not among %d targets", len(targets))
	}
	if err := bs.ActivateTarget(ctx, targetID); err != nil {
		t.Fatalf("Target.activateTarget: %v", err)
	}
	if err := s.BringToFront(ctx); err != nil {
		t.Fatalf("Page.bringToFront: %v", err)
	}

	// A navigation changes the target's URL and title. (A page's own timers
	// will not do: Chromium freezes them in a tab that was never activated,
	// which is worth knowing for the sidecar's idle handling.)
	if _, _, err := s.Navigate(ctx, testServer(t).URL+"/page?again=1"); err != nil {
		t.Fatalf("Page.navigate: %v", err)
	}
	deadline := time.After(15 * time.Second)
	for {
		select {
		case ev := <-changed:
			if ev.TargetInfo.TargetID == targetID && strings.Contains(ev.TargetInfo.URL, "again=1") {
				return
			}
		case <-deadline:
			t.Fatal("no targetInfoChanged after a navigation")
		}
	}
}

// TestChromiumWebdriverWithoutFlag launches a second browser without
// --disable-blink-features=AutomationControlled to record what pipe mode does
// on its own. It is the measurement docs/browser.md rests on.
func TestChromiumWebdriverWithoutFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: -short")
	}
	if _, err := exec.LookPath("chromium"); err != nil {
		t.Skip("skipping: no chromium on PATH")
	}
	f, err := launchBare(t)
	if err != nil {
		t.Fatalf("launch bare: %v", err)
	}
	defer f.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	bs := f.c.Session("")
	targetID, err := bs.CreateTarget(ctx, "about:blank")
	if err != nil {
		t.Fatalf("CreateTarget: %v", err)
	}
	sid, err := bs.AttachToTarget(ctx, targetID)
	if err != nil {
		t.Fatalf("AttachToTarget: %v", err)
	}
	s := f.c.Session(sid)
	if err := s.PageEnable(ctx); err != nil {
		t.Fatalf("Page.enable: %v", err)
	}
	if err := s.SetLifecycleEventsEnabled(ctx, true); err != nil {
		t.Fatalf("lifecycle: %v", err)
	}
	loaded := make(chan struct{}, 1)
	defer On(s, EventLifecycleEvent, func(ev LifecycleEvent) {
		if ev.Name == "load" {
			select {
			case loaded <- struct{}{}:
			default:
			}
		}
	})()
	if _, _, err := s.Navigate(ctx, testServer(t).URL+"/page"); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	select {
	case <-loaded:
	case <-time.After(20 * time.Second):
		t.Fatal("no load")
	}

	got := title(t, s, targetID, func(s string) bool { return strings.Contains(s, "wd=") })
	switch {
	case strings.Contains(got, "wd=true"):
		t.Logf("MEASURED: --remote-debugging-pipe alone sets navigator.webdriver; "+
			"--disable-blink-features=AutomationControlled is required (title=%q)", got)
	case strings.Contains(got, "wd=false"):
		t.Logf("MEASURED: --remote-debugging-pipe alone leaves navigator.webdriver false (title=%q)", got)
	default:
		t.Fatalf("could not read navigator.webdriver: title = %q", got)
	}
}

// launchBare is launch without the stealth flag.
func launchBare(t *testing.T) (*fixture, error) {
	t.Helper()
	bin, err := exec.LookPath("chromium")
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "cdp-bare-")
	if err != nil {
		return nil, err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin,
		"--headless=new",
		"--remote-debugging-pipe",
		"--user-data-dir="+dir,
		"--no-first-run",
		"--no-default-browser-check",
		"about:blank",
	)
	cmd.ExtraFiles = []*os.File{inR, outW}
	cmd.Env = append(os.Environ(), "HOME="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if testing.Verbose() {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	_ = inR.Close()
	_ = outW.Close()
	f := &fixture{cmd: cmd, dir: dir}
	f.c = New(outR, inW, Options{})
	return f, nil
}

// TestZZChromiumLeavesNothingRunning runs last (tests run in file order within
// a package, and the name sorts late) and checks the shared browser is gone
// once it is stopped.
func TestZZChromiumLeavesNothingRunning(t *testing.T) {
	if shared == nil {
		t.Skip("no browser was started")
	}
	pid := shared.cmd.Process.Pid
	shared.stop()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pid, 0); err != nil {
			return // the process group is gone
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("chromium process group %d is still alive after stop", pid)
}
