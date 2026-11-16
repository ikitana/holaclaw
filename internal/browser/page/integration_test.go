//go:build linux

package page

import (
	"bytes"
	"context"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/browser/chromium"
	"holaclaw/internal/browser/proto"
)

// These tests drive a real Chromium on a real virtual display against local
// httptest servers — there is no network here, and no URL that is not
// 127.0.0.1. What they hold is what only a browser can prove: the join of the
// accessibility tree and the snapshot, trusted input, the allow-list on the
// network, downloads, dialogs, and the screencast's backpressure.
//
// One browser is shared by the tests that can share one (agent "alpha"); the
// allow-list and download tests take their own agent because they open with a
// different list. They skip without chromium, without Xvfb, or under -short.

var (
	srvOnce sync.Once
	srv     *httptest.Server

	engineOnce sync.Once
	engine     *Engine
	engineErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if engine != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = engine.Shutdown(ctx)
		cancel()
	}
	if srv != nil {
		srv.Close()
	}
	os.Exit(code)
}

// ── the pages ────────────────────────────────────────────────────────────────

// formHTML reports what it sees into its own title: that is how these tests
// read a page's state without running a script in its main world.
const formHTML = `<!doctype html><html><head><title>form</title></head>
<body style="margin:0">
<h1>Booking</h1>
<form action="/done" method="get">
<label for="dep">Departure airport</label>
<input id="dep" name="dep" type="text" style="position:absolute;left:40px;top:100px;width:220px;height:30px">
<button id="search" type="submit" style="position:absolute;left:40px;top:150px;width:180px;height:40px">Search flights</button>
</form>
<button id="order" style="position:absolute;left:40px;top:220px;width:180px;height:40px">Order coffee</button>
<a id="other" href="/other" style="position:absolute;left:40px;top:280px">Other page</a>
<button id="far" style="position:absolute;left:40px;top:3000px;width:180px;height:40px">Far away button</button>
<script>
var clicks = 0, trusted = "none";
function render() { document.title = "form c=" + clicks + " trusted=" + trusted; }
render();
document.getElementById("order").addEventListener("click", function (e) {
  clicks++; trusted = String(e.isTrusted); render();
});
</script>
</body></html>`

const richHTML = `<!doctype html><html><head><title>rich</title></head>
<body style="margin:0">
<iframe src="/frame" width="300" height="150" style="position:absolute;left:20px;top:20px"></iframe>
<div id="host" style="position:absolute;left:20px;top:200px"></div>
<script>
var r = document.getElementById("host").attachShadow({mode: "open"});
r.innerHTML = '<button id="sb" style="width:200px;height:40px">Shadow button</button>';
</script>
</body></html>`

// domHTML is what the accessibility tree alone does not report: a field named
// only by its placeholder, a contenteditable editor, a select, a div that is a
// button only because it has a handler, and a button the page hides from
// assistive technology.
const domHTML = `<!doctype html><html><head><title>dom</title></head>
<body style="margin:0">
<input id="q" type="text" placeholder="Search the site" style="position:absolute;left:20px;top:20px;width:220px;height:30px">
<div id="editor" contenteditable="true" style="position:absolute;left:20px;top:70px;width:220px;height:40px">Draft here</div>
<select id="country" aria-label="Country" style="position:absolute;left:20px;top:130px;width:220px;height:30px">
<option>Spain</option><option>Portugal</option></select>
<div id="fake" onclick="document.title = 'dom clicked'" style="cursor:pointer;position:absolute;left:20px;top:180px;width:220px;height:40px">Fake button</div>
<button id="quiet" aria-hidden="true" style="position:absolute;left:20px;top:240px;width:220px;height:40px">Quiet button</button>
</body></html>`

// statesHTML is a page that publishes what it is doing: a combobox that opens
// a list of options as it is typed in, and a form that rejects an empty
// required field into an alert. Nothing here is in the accessibility tree
// until the page puts it there.
const statesHTML = `<!doctype html><html><head><title>states</title></head>
<body style="margin:0">
<label for="city" style="position:absolute;left:20px;top:0px">City</label>
<input id="city" role="combobox" aria-autocomplete="list" aria-expanded="false" aria-controls="cities"
 style="position:absolute;left:20px;top:20px;width:220px;height:30px">
<ul id="cities" role="listbox" style="position:absolute;left:20px;top:60px;width:220px;margin:0;padding:0;list-style:none"></ul>
<form id="f" novalidate style="position:absolute;left:20px;top:220px">
<input id="email" name="email" type="text" required aria-label="Email address" style="width:220px;height:30px">
<button id="send" type="submit" style="width:180px;height:40px">Send</button>
</form>
<div id="err" role="alert" style="position:absolute;left:20px;top:320px;width:300px"></div>
<script>
var box = document.getElementById("city"), list = document.getElementById("cities");
box.addEventListener("input", function () {
  list.innerHTML = "";
  ["Madrid", "Murcia", "Marbella"].forEach(function (name) {
    var li = document.createElement("li");
    li.setAttribute("role", "option");
    li.style.height = "24px";
    li.textContent = name;
    list.appendChild(li);
  });
  box.setAttribute("aria-expanded", "true");
});
document.getElementById("f").addEventListener("submit", function (e) {
  e.preventDefault();
  var email = document.getElementById("email");
  if (email.value === "") {
    email.setAttribute("aria-invalid", "true");
    document.getElementById("err").textContent = "Enter your email address";
  }
});
</script>
</body></html>`

const frameHTML = `<!doctype html><html><head><title>frame</title></head>
<body style="margin:0"><button style="width:200px;height:40px">Frame button</button></body></html>`

const dialogHTML = `<!doctype html><html><head><title>dialog</title></head>
<body style="margin:0">
<button id="go" style="position:absolute;left:40px;top:40px;width:180px;height:40px">Show alert</button>
<script>
// On a timer, so the input event that opens the dialog is acknowledged first.
document.getElementById("go").addEventListener("click", function () {
  setTimeout(function () { alert("something"); }, 10);
});
</script>
</body></html>`

// tickHTML repaints for ever: a screencast only produces frames when the page
// composites one, so the live-view tests need a page that moves.
const tickHTML = `<!doctype html><html><head><title>tick</title></head>
<body style="margin:0">
<button id="order" style="position:absolute;left:40px;top:220px;width:180px;height:40px">Order coffee</button>
<div id="t" style="position:absolute;left:40px;top:300px;font:40px monospace">0</div>
<script>
var n = 0, clicks = 0, trusted = "none";
function render() { document.title = "tick n=" + n + " c=" + clicks + " trusted=" + trusted; }
setInterval(function () { n++; document.getElementById("t").textContent = String(n); render(); }, 50);
document.getElementById("order").addEventListener("click", function (e) {
  clicks++; trusted = String(e.isTrusted); render();
});
</script>
</body></html>`

// botWallHTML is a challenge with a plain control behind it: nothing on this
// page may be pressed, wall or not — this layer only ever detects and
// reports one.
const botWallHTML = `<!doctype html><html><head><title>Verify you are human</title></head>
<body style="margin:0">
<h1>Verify you are human</h1>
<div class="g-recaptcha" style="position:fixed;left:40px;top:80px;width:300px;height:80px;background:#ddd">
<button id="notrobot" style="width:200px;height:40px">I am not a robot</button>
</div>
<div id="cookie-banner" style="position:fixed;left:0;bottom:0;width:100%;height:80px;background:#eee">
<button id="reject" style="width:160px;height:40px">Reject all</button>
</div>
<script>
document.getElementById("notrobot").addEventListener("click", function () { document.title = "solved"; });
document.getElementById("reject").addEventListener("click", function () { document.title = "banner pressed"; });
</script>
</body></html>`

// widgetsHTML is a page built out of the three things a shortlist of roles and
// names cannot explain on its own: a combobox that is a div and opens a list
// it controls, a date field that opens a dialog when it takes focus, and a
// cookie wall that declares itself modal and stands over everything until it
// is answered.
const widgetsHTML = `<!doctype html><html><head><title>widgets</title></head>
<body style="margin:0">
<div id="trip" role="combobox" aria-haspopup="listbox" aria-expanded="false" aria-controls="tt"
 aria-label="Trip type" tabindex="0"
 style="position:absolute;left:20px;top:20px;width:220px;height:30px;border:1px solid #333">Round trip</div>
<ul id="tt" role="listbox" aria-label="Ticket type" hidden
 style="position:absolute;left:20px;top:60px;width:220px;margin:0;padding:0;list-style:none"></ul>
<input id="depart" aria-label="Departure date" style="position:absolute;left:20px;top:160px;width:220px;height:30px">
<div id="picker" role="dialog" aria-label="Choose date" hidden
 style="position:absolute;left:20px;top:200px;width:220px;height:120px;background:#fff;border:1px solid #333"></div>
<button id="search" style="position:absolute;left:20px;top:340px;width:180px;height:40px">Search</button>
<div id="cookies" role="dialog" aria-modal="true" aria-label="Cookies"
 style="position:fixed;left:0;top:0;width:100%;height:100%;background:#fff;z-index:9">
<button id="accept" style="position:absolute;left:20px;top:20px;width:120px;height:40px">Accept</button>
<button id="decline" style="position:absolute;left:160px;top:20px;width:120px;height:40px">Decline</button>
</div>
<script>
var trip = document.getElementById("trip"), list = document.getElementById("tt");
trip.addEventListener("click", function () {
  list.hidden = false;
  list.innerHTML = "";
  ["Round trip", "One way", "Multi-city"].forEach(function (name) {
    var li = document.createElement("li");
    li.setAttribute("role", "option");
    li.style.height = "24px";
    li.textContent = name;
    list.appendChild(li);
  });
  trip.setAttribute("aria-expanded", "true");
});
document.getElementById("depart").addEventListener("focus", function () {
  var p = document.getElementById("picker");
  p.hidden = false;
  p.innerHTML = "";
  ["1", "2", "3"].forEach(function (d) {
    var b = document.createElement("button");
    b.style.width = "40px";
    b.style.height = "30px";
    b.textContent = d;
    p.appendChild(b);
  });
});
document.getElementById("accept").addEventListener("click", function () {
  document.getElementById("cookies").remove();
});
</script>
</body></html>`

// datesHTML is the shape of a real booking form (renfe.com, M11): a date field
// the page will not let anybody type into, which says in a sentence of its own
// how it is worked, and which opens a calendar made of divs with no role, no
// handler of their own and no tabindex — thirty day cells whose only mark is
// the hand the page draws over them, and which rank below every field, button
// and link on the page, so a shortlist that did not know they had just arrived
// would cut them. Beside it, a live region the page keeps standing and a button whose
// only name is drawn inside it.
const datesHTML = `<!doctype html><html><head><title>dates</title></head>
<body style="margin:0">
<div id="tips" aria-live="polite"
 style="position:absolute;left:20px;top:0;width:600px;height:30px">We use cookies to make this site work</div>
<input id="depart" readonly aria-label="Fecha vuelta" aria-describedby="howto" value="mié., 23/09/26"
 style="position:absolute;left:20px;top:40px;width:220px;height:30px">
<div id="howto" style="position:absolute;left:260px;top:40px;width:600px;height:30px">Para cambiar fechas usar las flechas de dirección izquierda y derecha</div>
<button id="open" style="position:absolute;left:20px;top:80px;width:40px;height:30px">
<svg width="16" height="16" viewBox="0 0 16 16"><title>Abrir calendario</title><rect width="16" height="16"></rect></svg>
</button>
<input id="who" aria-label="Pasajeros" style="position:absolute;left:20px;top:120px;width:220px;height:30px">
<button id="search" style="position:absolute;left:20px;top:160px;width:180px;height:40px">Buscar</button>
<div id="cal" style="position:absolute;left:20px;top:220px;width:320px"></div>
<script>
document.getElementById("depart").addEventListener("click", function () {
  var cal = document.getElementById("cal");
  if (cal.childElementCount) { return; }
  for (var d = 1; d <= 30; d++) {
    var cell = document.createElement("div");
    cell.className = "lightpick__day";
    cell.style.cssText = "display:inline-block;width:40px;height:30px;cursor:pointer";
    cell.textContent = String(d);
    cal.appendChild(cell);
  }
});
// The click handler is the container's, as a real picker's is: the cells
// themselves have no handler, no role and no tabindex, and the hand drawn
// over them is the only sign that they can be clicked at all.
document.getElementById("cal").addEventListener("click", function (ev) {
  if (ev.target.className !== "lightpick__day") { return; }
  document.getElementById("depart").value = ev.target.textContent;
});
</script>
</body></html>`

// testServer serves every page of these tests on 127.0.0.1.
func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	srvOnce.Do(func() {
		mux := http.NewServeMux()
		html := func(w http.ResponseWriter, body string) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, body)
		}
		mux.HandleFunc("/form", func(w http.ResponseWriter, _ *http.Request) { html(w, formHTML) })
		mux.HandleFunc("/rich", func(w http.ResponseWriter, _ *http.Request) { html(w, richHTML) })
		mux.HandleFunc("/dom", func(w http.ResponseWriter, _ *http.Request) { html(w, domHTML) })
		mux.HandleFunc("/states", func(w http.ResponseWriter, _ *http.Request) { html(w, statesHTML) })
		mux.HandleFunc("/widgets", func(w http.ResponseWriter, _ *http.Request) { html(w, widgetsHTML) })
		mux.HandleFunc("/dates", func(w http.ResponseWriter, _ *http.Request) { html(w, datesHTML) })
		mux.HandleFunc("/frame", func(w http.ResponseWriter, _ *http.Request) { html(w, frameHTML) })
		mux.HandleFunc("/dialog", func(w http.ResponseWriter, _ *http.Request) { html(w, dialogHTML) })
		mux.HandleFunc("/tick", func(w http.ResponseWriter, _ *http.Request) { html(w, tickHTML) })
		mux.HandleFunc("/botwall", func(w http.ResponseWriter, _ *http.Request) { html(w, botWallHTML) })
		mux.HandleFunc("/other", func(w http.ResponseWriter, _ *http.Request) {
			html(w, `<!doctype html><html><head><title>other</title></head><body>other page</body></html>`)
		})
		mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
			html(w, `<!doctype html><html><head><title>done</title></head><body>`+
				`<p>booked `+r.URL.Query().Get("dep")+`</p></body></html>`)
		})
		// A page whose only job is to open a window at the URL it is given.
		mux.HandleFunc("/popup", func(w http.ResponseWriter, r *http.Request) {
			html(w, `<!doctype html><html><head><title>popup host</title></head><body style="margin:0">
<button id="pop" style="position:absolute;left:40px;top:40px;width:180px;height:40px">Open window</button>
<script>document.getElementById("pop").addEventListener("click", function () {
  window.open(`+strconv.Quote(r.URL.Query().Get("to"))+`, "_blank");
});</script></body></html>`)
		})
		mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, r.URL.Query().Get("to"), http.StatusFound)
		})
		// Two client-side redirects: the wire blocks both, but the tab
		// commits an error page at the URL it was refused.
		mux.HandleFunc("/meta", func(w http.ResponseWriter, r *http.Request) {
			html(w, `<!doctype html><html><head><title>meta</title>`+
				`<meta http-equiv="refresh" content="0; url=`+r.URL.Query().Get("to")+`">`+
				`</head><body>going</body></html>`)
		})
		mux.HandleFunc("/js", func(w http.ResponseWriter, r *http.Request) {
			html(w, `<!doctype html><html><head><title>js</title></head><body>going
<script>location.href = `+strconv.Quote(r.URL.Query().Get("to"))+`;</script>
</body></html>`)
		})
		mux.HandleFunc("/file", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="notes.txt"`)
			_, _ = io.WriteString(w, "quarantined bytes")
		})
		mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
			size := int64(proto.MaxDownloadBytes) + (1 << 20)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", `attachment; filename="big.bin"`)
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			_, _ = io.CopyN(w, zeroes{}, size)
		})
		// A page with a link to whatever URL it is given, so a click can
		// navigate or download.
		mux.HandleFunc("/link", func(w http.ResponseWriter, r *http.Request) {
			html(w, `<!doctype html><html><head><title>link</title></head><body style="margin:0">
<a id="a" href="`+r.URL.Query().Get("to")+`" style="position:absolute;left:40px;top:40px">Get the file</a>
</body></html>`)
		})
		srv = httptest.NewServer(mux)
	})
	return srv
}

// zeroes is an endless reader of one byte value.
type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'z'
	}
	return len(p), nil
}

// ── the engine under test ────────────────────────────────────────────────────

// realEngine returns the shared engine, skipping when this machine cannot run
// a browser.
func realEngine(t *testing.T) *Engine {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping: -short")
	}
	if _, err := exec.LookPath("Xvfb"); err != nil {
		t.Skip("skipping: no Xvfb")
	}
	bin, err := chromium.Find("")
	if err != nil {
		t.Skipf("skipping: no browser: %v", err)
	}
	if bin.TooOld() {
		t.Skipf("skipping: browser %s is older than %d", bin.Version, chromium.MinMajor)
	}
	engineOnce.Do(func() {
		zero := 0.0
		dir, err := os.MkdirTemp("", "page-state-")
		if err != nil {
			engineErr = err
			return
		}
		engine, engineErr = New(Options{
			Binary:   bin,
			StateDir: dir,
			Pacing:   &zero,
			Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	})
	if engineErr != nil {
		t.Fatalf("engine: %v", engineErr)
	}
	return engine
}

// open starts one agent's browser with an allow-list and returns the engine.
func open(t *testing.T, agent string, allow ...string) *Engine {
	t.Helper()
	e := realEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := e.Open(ctx, agent, proto.OpenRequest{Allow: allow}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	return e
}

// goTo navigates the agent to a path of the test server and fails on anything
// but a clean result.
func goTo(t *testing.T, e *Engine, agent, path string) proto.ActResult {
	t.Helper()
	res, err := e.Act(ctxT(t), agent, proto.Action{Kind: proto.ActNavigate, URL: testServer(t).URL + path})
	if err != nil {
		t.Fatalf("navigate %s: %v", path, err)
	}
	if !res.OK {
		t.Fatalf("navigate %s: %+v", path, res)
	}
	return res
}

func observe(t *testing.T, e *Engine, agent string, req proto.ObserveRequest) proto.Observation {
	t.Helper()
	obs, err := e.Observe(ctxT(t), agent, req)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	return obs
}

// refOf finds a candidate by role and name.
func refOf(t *testing.T, obs proto.Observation, role, name string) string {
	t.Helper()
	for _, c := range obs.Candidates {
		if c.Role == role && c.Name == name {
			return c.Ref
		}
	}
	t.Fatalf("no %s named %q among %+v", role, name, obs.Candidates)
	return ""
}

// candidateOf is the candidate behind a ref of this observation.
func candidateOf(obs proto.Observation, ref string) (proto.Candidate, bool) {
	for _, c := range obs.Candidates {
		if c.Ref == ref {
			return c, true
		}
	}
	return proto.Candidate{}, false
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// waitTitle polls the observation until the title matches.
func waitTitle(t *testing.T, e *Engine, agent string, want func(string) bool) string {
	t.Helper()
	var last string
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		obs := observe(t, e, agent, proto.ObserveRequest{})
		last = obs.Title
		if want(last) {
			return last
		}
		time.Sleep(100 * time.Millisecond)
	}
	return last
}

// ── the tests ────────────────────────────────────────────────────────────────

// Observation: roles and names from the accessibility tree, boxes from the
// snapshot, refs that mean something, and a shortlist the hints steer.
func TestIntegrationObserveForm(t *testing.T) {
	e := open(t, "alpha")
	goTo(t, e, "alpha", "/form")

	obs := observe(t, e, "alpha", proto.ObserveRequest{TextBytes: 1 << 10})
	if !strings.Contains(obs.URL, "/form") || obs.Title == "" {
		t.Fatalf("observation = %+v", obs)
	}
	if obs.Total < 4 {
		t.Fatalf("total = %d, want the form's elements", obs.Total)
	}
	if !strings.Contains(obs.Text, "Booking") {
		t.Fatalf("text = %q", obs.Text)
	}
	var textbox, far *proto.Candidate
	for i, c := range obs.Candidates {
		if c.Ref == "" {
			t.Fatalf("candidate without a ref: %+v", c)
		}
		switch c.Name {
		case "Departure airport":
			textbox = &obs.Candidates[i]
		case "Far away button":
			far = &obs.Candidates[i]
		}
	}
	if textbox == nil || textbox.Role != "textbox" {
		t.Fatalf("no departure textbox among %+v", obs.Candidates)
	}
	if far == nil || !far.Offscreen {
		t.Fatalf("the button 3000px down is %+v, want offscreen", far)
	}
	if refOf(t, obs, "button", "Search flights") == "" {
		t.Fatal("no search button")
	}

	// Hints move the match to the front without changing what the refs mean.
	hinted := observe(t, e, "alpha", proto.ObserveRequest{Hints: []string{"departure"}})
	if hinted.Candidates[0].Name != "Departure airport" {
		t.Fatalf("hinted shortlist = %+v", hinted.Candidates)
	}
	if capped := observe(t, e, "alpha", proto.ObserveRequest{MaxCandidates: 2}); len(capped.Candidates) != 2 {
		t.Fatalf("cap 2 gave %d candidates", len(capped.Candidates))
	}
}

// A click is real input: the page reports isTrusted = true. Typing, clearing
// and submitting go the same way, and the form arrives with the value.
func TestIntegrationClickTypeSubmit(t *testing.T) {
	e := open(t, "alpha")
	goTo(t, e, "alpha", "/form")

	obs := observe(t, e, "alpha", proto.ObserveRequest{})
	res, err := e.Act(ctxT(t), "alpha", proto.Action{Kind: proto.ActClick, Ref: refOf(t, obs, "button", "Order coffee")})
	if err != nil || !res.OK {
		t.Fatalf("click: %+v %v", res, err)
	}
	got := waitTitle(t, e, "alpha", func(s string) bool { return strings.Contains(s, "trusted=true") })
	if !strings.Contains(got, "c=1") || !strings.Contains(got, "trusted=true") {
		t.Fatalf("title after the click = %q, want a trusted click", got)
	}

	obs = observe(t, e, "alpha", proto.ObserveRequest{Hints: []string{"departure"}})
	ref := refOf(t, obs, "textbox", "Departure airport")
	res, err = e.Act(ctxT(t), "alpha", proto.Action{Kind: proto.ActType, Ref: ref, Text: "Bilbao", Clear: true})
	if err != nil || !res.OK {
		t.Fatalf("type: %+v %v", res, err)
	}
	// Refs belong to the observation that minted them: the box is found again
	// on the fresh page, and the old handle would be stale_ref.
	obs = observe(t, e, "alpha", proto.ObserveRequest{Hints: []string{"departure"}})
	ref = refOf(t, obs, "textbox", "Departure airport")
	if c, ok := candidateOf(obs, ref); !ok || c.Value != "Bilbao" {
		t.Fatalf("value after typing = %q (found %v)", c.Value, ok)
	}
	// Typing again with Clear replaces rather than appends.
	res, err = e.Act(ctxT(t), "alpha", proto.Action{
		Kind: proto.ActType, Ref: ref, Text: "Madrid", Clear: true, Submit: true,
	})
	if err != nil || !res.OK {
		t.Fatalf("submit: %+v %v", res, err)
	}
	if !res.Navigated || !strings.Contains(res.URL, "dep=Madrid") {
		t.Fatalf("after submit: %+v", res)
	}

	// Back returns to the form.
	res, err = e.Act(ctxT(t), "alpha", proto.Action{Kind: proto.ActBack})
	if err != nil || !res.OK || !strings.Contains(res.URL, "/form") {
		t.Fatalf("back: %+v %v", res, err)
	}
}

// A ref belongs to the observation it came from and to nothing else.
func TestIntegrationStaleRef(t *testing.T) {
	e := open(t, "alpha")
	goTo(t, e, "alpha", "/form")
	obs := observe(t, e, "alpha", proto.ObserveRequest{})
	last := obs.Candidates[len(obs.Candidates)-1].Ref

	goTo(t, e, "alpha", "/other") // a page with no interactive elements
	observe(t, e, "alpha", proto.ObserveRequest{})

	res, err := e.Act(ctxT(t), "alpha", proto.Action{Kind: proto.ActClick, Ref: last})
	if !isCode(err, proto.ErrStaleRef) || res.Code != proto.ErrStaleRef {
		t.Fatalf("stale click = %+v, %v", res, err)
	}
	// An unknown key is refused on the same closed-set principle.
	res, err = e.Act(ctxT(t), "alpha", proto.Action{Kind: proto.ActKey, Key: "F13"})
	if !isCode(err, proto.ErrBadAction) || res.Code != proto.ErrBadAction {
		t.Fatalf("unknown key = %+v, %v", res, err)
	}
}

// Scroll and wait: both are ordinary actions with a settled result.
func TestIntegrationScrollAndWait(t *testing.T) {
	e := open(t, "alpha")
	goTo(t, e, "alpha", "/form")
	res, err := e.Act(ctxT(t), "alpha", proto.Action{Kind: proto.ActScroll, DeltaY: 3000})
	if err != nil || !res.OK {
		t.Fatalf("scroll: %+v %v", res, err)
	}
	res, err = e.Act(ctxT(t), "alpha", proto.Action{Kind: proto.ActWait, TimeoutMS: 2000})
	if err != nil || !res.OK {
		t.Fatalf("wait: %+v %v", res, err)
	}
	// The far button is on screen now that the page scrolled.
	deadline := time.Now().Add(10 * time.Second)
	for {
		obs := observe(t, e, "alpha", proto.ObserveRequest{Hints: []string{"far"}})
		for _, c := range obs.Candidates {
			if c.Name == "Far away button" && !c.Offscreen {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("the far button is still offscreen after scrolling")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Candidates come from iframes and from shadow DOM, not only from the top
// document.
func TestIntegrationIframeAndShadowDOM(t *testing.T) {
	e := open(t, "alpha")
	goTo(t, e, "alpha", "/rich")
	names := map[string]bool{}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		obs := observe(t, e, "alpha", proto.ObserveRequest{})
		names = map[string]bool{}
		for _, c := range obs.Candidates {
			names[c.Name] = true
		}
		if names["Frame button"] && names["Shadow button"] {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("candidates = %v, want the iframe's and the shadow root's buttons", names)
}

// The allow-list is the network: a navigation outside it never leaves, a
// redirect out of it is failed, and a window opened outside it is closed.
func TestIntegrationAllowList(t *testing.T) {
	e := open(t, "beta", "127.0.0.1")
	s := testServer(t)
	_, port, _ := strings.Cut(strings.TrimPrefix(s.URL, "http://"), ":")
	outside := "http://localhost:" + port // the same server under a host that is not allowed

	// Refused before a request is even made.
	res, err := e.Act(ctxT(t), "beta", proto.Action{Kind: proto.ActNavigate, URL: outside + "/other"})
	if !isCode(err, proto.ErrNotAllowed) || res.Code != proto.ErrNotAllowed {
		t.Fatalf("navigate outside the list = %+v, %v", res, err)
	}

	// A redirect out of the list is stopped on the wire.
	res, err = e.Act(ctxT(t), "beta", proto.Action{
		Kind: proto.ActNavigate, URL: s.URL + "/redirect?to=" + outside + "/other", TimeoutMS: 10000,
	})
	if err == nil && res.OK && strings.Contains(res.URL, "localhost") {
		t.Fatalf("a redirect left the allow-list: %+v", res)
	}
	obs := observe(t, e, "beta", proto.ObserveRequest{})
	if strings.Contains(obs.URL, "localhost") {
		t.Fatalf("the browser ended up outside the list: %q", obs.URL)
	}

	// A window opened at an allowed host becomes a tab.
	goTo(t, e, "beta", "/popup?to="+s.URL+"/other")
	obs = observe(t, e, "beta", proto.ObserveRequest{})
	res, err = e.Act(ctxT(t), "beta", proto.Action{Kind: proto.ActClick, Ref: refOf(t, obs, "button", "Open window")})
	if err != nil || !res.OK {
		t.Fatalf("click: %+v %v", res, err)
	}
	if n := waitTabs(t, e, "beta", 2); n != 2 {
		t.Fatalf("an allowed popup did not become a tab: %d tabs", n)
	}

	// A window opened outside the list is closed again.
	goTo(t, e, "beta", "/popup?to="+outside+"/other")
	obs = observe(t, e, "beta", proto.ObserveRequest{})
	before := len(sessionOf(t, e, "beta").Tabs)
	res, err = e.Act(ctxT(t), "beta", proto.Action{Kind: proto.ActClick, Ref: refOf(t, obs, "button", "Open window")})
	if err != nil || !res.OK {
		t.Fatalf("click: %+v %v", res, err)
	}
	time.Sleep(2 * time.Second)
	tabs := sessionOf(t, e, "beta").Tabs
	for _, tab := range tabs {
		if strings.HasPrefix(tab.URL, outside) {
			t.Fatalf("a window outside the allow-list is open: %+v", tabs)
		}
	}
	if len(tabs) > before {
		t.Fatalf("the blocked window was left behind: %d tabs, was %d", len(tabs), before)
	}
	if err := e.Close(ctxT(t), "beta"); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// waitTabs waits for the agent's tab count to reach n.
func waitTabs(t *testing.T, e *Engine, agent string, n int) int {
	t.Helper()
	last := 0
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		last = len(sessionOf(t, e, agent).Tabs)
		if last >= n {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
	return last
}

func sessionOf(t *testing.T, e *Engine, agent string) proto.Session {
	t.Helper()
	for _, s := range e.Sessions(ctxT(t)) {
		if s.Agent == agent {
			return s
		}
	}
	t.Fatalf("no session for %s", agent)
	return proto.Session{}
}

// Downloads land in the quarantine under an opaque id, come back over the
// socket exactly once, and a file over the per-file cap is cancelled and
// deleted instead.
func TestIntegrationDownloadQuarantine(t *testing.T) {
	e := open(t, "gamma")
	s := testServer(t)
	goTo(t, e, "gamma", "/link?to="+s.URL+"/file")

	obs := observe(t, e, "gamma", proto.ObserveRequest{})
	got := clickAndCollect(t, e, "gamma", refOf(t, obs, "link", "Get the file"))
	if len(got) != 1 {
		t.Fatalf("downloads = %+v, want one", got)
	}
	if got[0].Name != "notes.txt" || got[0].Size != int64(len("quarantined bytes")) {
		t.Fatalf("download = %+v", got[0])
	}
	rc, meta, err := e.Download(ctxT(t), "gamma", got[0].ID)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if string(body) != "quarantined bytes" || meta.Name != "notes.txt" {
		t.Fatalf("body = %q, meta = %+v", body, meta)
	}

	// Over the per-file cap: cancelled, deleted, never reported.
	goTo(t, e, "gamma", "/link?to="+s.URL+"/big")
	obs = observe(t, e, "gamma", proto.ObserveRequest{})
	if got := clickAndCollect(t, e, "gamma", refOf(t, obs, "link", "Get the file")); len(got) != 0 {
		t.Fatalf("an oversize download was filed: %+v", got)
	}
	dir := filepath.Join(e.quarantine, "gamma")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, _ := entry.Info()
		t.Fatalf("the quarantine still holds %s (%d bytes)", entry.Name(), info.Size())
	}

	// Closing the session takes the quarantine with it.
	if err := e.Close(ctxT(t), "gamma"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the quarantine survived Close: %v", err)
	}
}

// clickAndCollect clicks a ref and gathers the downloads the click and the
// waits after it report.
func clickAndCollect(t *testing.T, e *Engine, agent, ref string) []proto.Download {
	t.Helper()
	res, err := e.Act(ctxT(t), agent, proto.Action{Kind: proto.ActClick, Ref: ref})
	if err != nil || !res.OK {
		t.Fatalf("click: %+v %v", res, err)
	}
	got := res.Downloads
	for i := 0; i < 6 && len(got) == 0; i++ {
		res, err := e.Act(ctxT(t), agent, proto.Action{Kind: proto.ActWait, TimeoutMS: 1000})
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
		got = append(got, res.Downloads...)
	}
	return got
}

// A dialog blocks the renderer: it is reported, then dismissed by the next
// action, and the page carries on.
func TestIntegrationDialogDismissal(t *testing.T) {
	e := open(t, "alpha")
	goTo(t, e, "alpha", "/dialog")
	obs := observe(t, e, "alpha", proto.ObserveRequest{})
	ref := refOf(t, obs, "button", "Show alert")
	if _, err := e.Act(ctxT(t), "alpha", proto.Action{Kind: proto.ActClick, Ref: ref}); err != nil {
		t.Fatalf("click: %v", err)
	}
	var seen proto.Observation
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		seen = observe(t, e, "alpha", proto.ObserveRequest{})
		if seen.Dialog != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if seen.Dialog != "alert" {
		t.Fatalf("dialog = %q, want alert", seen.Dialog)
	}
	// The next action dismisses it; the observation after that is clean.
	if _, err := e.Act(ctxT(t), "alpha", proto.Action{Kind: proto.ActWait, TimeoutMS: 2000}); err != nil {
		t.Fatalf("wait: %v", err)
	}
	after := observe(t, e, "alpha", proto.ObserveRequest{})
	if after.Dialog != "" {
		t.Fatalf("the dialog was not dismissed: %+v", after)
	}
	if len(after.Candidates) == 0 {
		t.Fatal("the page is not observable again after the dialog")
	}
}

func TestIntegrationScreenshot(t *testing.T) {
	e := open(t, "alpha")
	goTo(t, e, "alpha", "/form")
	img, err := e.Screenshot(ctxT(t), "alpha")
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	if len(img) < 1000 || img[0] != 0xFF || img[1] != 0xD8 {
		t.Fatalf("screenshot is %d bytes and does not start with a JPEG marker", len(img))
	}
}

// testViewer is a slow live-view client: it records what it was given and how
// many frames were ever in flight at once.
type testViewer struct {
	delay time.Duration

	mu       sync.Mutex
	frames   int
	inFlight int
	maxFly   int
	metas    []proto.StreamMessage
	sizes    [][2]int
	images   [][]byte
}

func (v *testViewer) Frame(ctx context.Context, data []byte, w, h int) error {
	v.mu.Lock()
	v.inFlight++
	if v.inFlight > v.maxFly {
		v.maxFly = v.inFlight
	}
	v.frames++
	v.sizes = append(v.sizes, [2]int{w, h})
	v.images = append(v.images, data)
	v.mu.Unlock()

	select {
	case <-time.After(v.delay):
	case <-ctx.Done():
	}

	v.mu.Lock()
	v.inFlight--
	v.mu.Unlock()
	return nil
}

func (v *testViewer) Meta(_ context.Context, m proto.StreamMessage) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.metas = append(v.metas, m)
	return nil
}

func (v *testViewer) count() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.frames
}

// The screencast runs at the viewer's speed: a frame is acknowledged only
// after the viewer took it, so a slow viewer bounds what is in flight.
func TestIntegrationScreencastBackpressure(t *testing.T) {
	e := open(t, "alpha")
	goTo(t, e, "alpha", "/tick")

	v := &testViewer{delay: 200 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	in := make(chan proto.StreamMessage)
	if err := e.Stream(ctx, "alpha", in, v); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.frames == 0 {
		t.Fatal("no frame reached the viewer")
	}
	if v.maxFly != 1 {
		t.Fatalf("%d frames were in flight at once; the ack is the backpressure", v.maxFly)
	}
	// A 200 ms viewer cannot have been handed 4 s of a 20 fps stream.
	if v.frames > 25 {
		t.Fatalf("%d frames in 4 s to a 200 ms viewer: the screencast is not paced by the ack", v.frames)
	}
	if len(v.metas) == 0 || len(v.metas[0].Tabs) == 0 {
		t.Fatalf("meta = %+v, want the tab list", v.metas)
	}
	// The JPEG itself is capped at 1280×800 (the metadata reports the page's
	// own device size, which is not the same number).
	for _, jpg := range v.images {
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(jpg))
		if err != nil {
			t.Fatalf("a frame is not a JPEG: %v", err)
		}
		if cfg.Width > maxStreamW || cfg.Height > maxStreamH {
			t.Fatalf("frame is %dx%d, over the %dx%d cap", cfg.Width, cfg.Height, maxStreamW, maxStreamH)
		}
	}
}

// Input from the live view is dispatched as real input: the page sees
// isTrusted = true, and a second viewer is told the view is held.
func TestIntegrationStreamInputRelay(t *testing.T) {
	e := open(t, "alpha")
	goTo(t, e, "alpha", "/tick")

	v := &testViewer{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	in := make(chan proto.StreamMessage, 8)

	done := make(chan error, 1)
	go func() { done <- e.Stream(ctx, "alpha", in, v) }()
	waitFor(t, "the first frame", func() bool { return v.count() > 0 })

	// A second live view of the same agent is refused.
	if err := e.Stream(ctxT(t), "alpha", nil, &testViewer{}); !isCode(err, proto.ErrHeld) {
		t.Fatalf("second Stream = %v, want held", err)
	}

	// The "Order coffee" button is at (40,220), 180×40: its centre is (130,240).
	in <- proto.StreamMessage{Type: proto.StreamPointer, Event: "move", X: 130, Y: 240}
	in <- proto.StreamMessage{Type: proto.StreamPointer, Event: "down", X: 130, Y: 240, Button: "left", Clicks: 1}
	in <- proto.StreamMessage{Type: proto.StreamPointer, Event: "up", X: 130, Y: 240, Button: "left", Clicks: 1}

	got := waitTitle(t, e, "alpha", func(s string) bool { return strings.Contains(s, "trusted=true") })
	if !strings.Contains(got, "trusted=true") {
		t.Fatalf("title after relayed input = %q, want a trusted click", got)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Stream returned %v", err)
	}
	// The screencast is stopped and drained: nothing arrives after Stream.
	before := v.count()
	time.Sleep(500 * time.Millisecond)
	if after := v.count(); after != before {
		t.Fatalf("%d frames arrived after Stream returned", after-before)
	}
}

// The browser the Manager restarts is re-attached transparently: the next call
// answers, with new refs.
func TestIntegrationReattachAfterStop(t *testing.T) {
	e := open(t, "delta")
	goTo(t, e, "delta", "/form")
	obs := observe(t, e, "delta", proto.ObserveRequest{})
	if len(obs.Candidates) == 0 {
		t.Fatal("no candidates before the restart")
	}
	// An idle stop is what the Manager does on its own; Stop is the same thing
	// on demand.
	e.mgr.Stop(ctxT(t), "delta")

	obs = observe(t, e, "delta", proto.ObserveRequest{})
	if obs.URL == "" {
		t.Fatalf("the re-attached browser gave %+v", obs)
	}
	res, err := e.Act(ctxT(t), "delta", proto.Action{Kind: proto.ActNavigate, URL: testServer(t).URL + "/form"})
	if err != nil || !res.OK {
		t.Fatalf("after the restart: %+v %v", res, err)
	}
	if err := e.Close(ctxT(t), "delta"); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// waitFor polls until cond or fails.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// L8: the wire blocks a client-side redirect out of the allow-list, but the
// tab still commits Chromium's error page and keeps the URL it was refused.
// The action must say so — not_allowed, never OK with an off-list URL — and
// leave the tab on a page the list allows.
func TestIntegrationClientSideRedirectOffTheList(t *testing.T) {
	e := open(t, "epsilon", "127.0.0.1")
	s := testServer(t)

	// A second server, reached under a host the list does not carry.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><html><head><title>elsewhere</title></head><body>elsewhere</body></html>`)
	}))
	defer other.Close()
	_, port, _ := strings.Cut(strings.TrimPrefix(other.URL, "http://"), ":")
	outside := "http://localhost:" + port

	for _, page := range []string{"/meta?to=", "/js?to="} {
		t.Run(page, func(t *testing.T) {
			// Start from an allowed page, so there is somewhere to go back to.
			goTo(t, e, "epsilon", "/form")

			res, err := e.Act(ctxT(t), "epsilon", proto.Action{
				Kind: proto.ActNavigate, URL: s.URL + page + outside + "/", TimeoutMS: 15000,
			})
			if res.OK {
				t.Fatalf("a redirect off the list was reported OK: %+v", res)
			}
			if !isCode(err, proto.ErrNotAllowed) || res.Code != proto.ErrNotAllowed {
				t.Fatalf("result = %+v, err = %v, want not_allowed", res, err)
			}
			if strings.Contains(res.URL, "localhost") {
				t.Fatalf("the result URL is off the list: %+v", res)
			}
			// And the executor's next observation is not of an error page.
			obs := observe(t, e, "epsilon", proto.ObserveRequest{})
			if strings.Contains(obs.URL, "localhost") {
				t.Fatalf("the tab was left outside the list: %q", obs.URL)
			}
		})
	}
	if err := e.Close(ctxT(t), "epsilon"); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// M7: the per-tab and per-iframe subscriptions are cancelled when the session
// is closed, whichever goroutine registered them. A page with frames is
// attached and closed repeatedly; the goroutines must come back down.
func TestIntegrationCloseLeavesNoSubscriptionGoroutines(t *testing.T) {
	e := open(t, "zeta", "127.0.0.1")
	// One round first, so the browser process and its own goroutines are up
	// and out of the count.
	goTo(t, e, "zeta", "/rich")
	if err := e.Close(ctxT(t), "zeta"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	settleGoroutines()
	before := runtime.NumGoroutine()

	for range 3 {
		open(t, "zeta", "127.0.0.1")
		goTo(t, e, "zeta", "/rich")
		observe(t, e, "zeta", proto.ObserveRequest{})
		if err := e.Close(ctxT(t), "zeta"); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	settleGoroutines()
	// Each round subscribes a handful of times per tab and per frame; a lost
	// cancel would show as a count that climbs with the rounds.
	if after := runtime.NumGoroutine(); after > before+5 {
		t.Fatalf("goroutines %d -> %d over three attach/close rounds", before, after)
	}
}

// settleGoroutines waits for the goroutine count to stop moving: a closed
// connection's readers and subscriptions end on their own time.
func settleGoroutines() {
	last := -1
	for range 40 {
		time.Sleep(100 * time.Millisecond)
		n := runtime.NumGoroutine()
		if n == last {
			return
		}
		last = n
	}
}

// The candidate set is the DOM merged with the tree: a field the tree names
// only by its placeholder, an editor, a select, a handler-only div and a button
// the page hid from assistive technology are all there, each named by what a
// person reads on it, and none of them twice.
func TestIntegrationObserveDOMSources(t *testing.T) {
	e := open(t, "alpha")
	goTo(t, e, "alpha", "/dom")

	obs := observe(t, e, "alpha", proto.ObserveRequest{MaxCandidates: proto.MaxCandidates})
	for _, want := range []struct{ role, name string }{
		{"textbox", "Search the site"},
		{"textbox", "Draft here"},
		{"combobox", "Country"},
		{"button", "Fake button"},
		{"button", "Quiet button"},
	} {
		n := 0
		for _, c := range obs.Candidates {
			if c.Role == want.role && c.Name == want.name {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s %q appears %d times among %+v", want.role, want.name, n, obs.Candidates)
		}
	}
	// A click still lands on the element the DOM supplied.
	res, err := e.Act(ctxT(t), "alpha", proto.Action{
		Kind: proto.ActClick,
		Ref:  refOf(t, obs, "button", "Fake button"),
	})
	if err != nil || !res.OK {
		t.Fatalf("click: %v %+v", err, res)
	}
	if got := waitTitle(t, e, "alpha", func(s string) bool { return s == "dom clicked" }); got != "dom clicked" {
		t.Fatalf("title after the click = %q", got)
	}
}

// A bot wall is recognised and nothing on it is touched — not the challenge,
// not the plain control behind it. Solving one is the executor's blocker
// classification to report, never this layer's click.
func TestIntegrationBotWallIsNeverClicked(t *testing.T) {
	e := open(t, "theta", "127.0.0.1")
	t.Cleanup(func() { _ = e.Close(ctxT(t), "theta") })

	goTo(t, e, "theta", "/botwall")
	obs := observe(t, e, "theta", proto.ObserveRequest{})
	if got := e.LastBotWall("theta"); got != BotWall {
		t.Fatalf("LastBotWall = %q, want %q", got, BotWall)
	}
	if obs.Title != "Verify you are human" {
		t.Fatalf("title = %q: something on the bot wall was pressed", obs.Title)
	}
}

// A page with no bot wall is left exactly as it is, and costs no click.
func TestIntegrationNoBotWallOnAPlainPage(t *testing.T) {
	e := open(t, "iota", "127.0.0.1")
	t.Cleanup(func() { _ = e.Close(ctxT(t), "iota") })

	goTo(t, e, "iota", "/form")
	obs := observe(t, e, "iota", proto.ObserveRequest{})
	if got := e.LastBotWall("iota"); got != "" {
		t.Fatalf("LastBotWall on a plain page = %q", got)
	}
	if !strings.HasPrefix(obs.Title, "form c=0") {
		t.Fatalf("title = %q: a plain page was clicked", obs.Title)
	}
}

// What a page says about its own state, read off a real browser: a combobox
// that opens a list as it is typed in, and a form that rejects an empty
// required field. Both are things only the running page knows — the markup
// that arrives over the wire says neither.
func TestIntegrationObserveStates(t *testing.T) {
	e := open(t, "alpha")
	goTo(t, e, "alpha", "/states")

	obs := observe(t, e, "alpha", proto.ObserveRequest{MaxCandidates: proto.MaxCandidates})
	city := candidateNamed(t, obs, "combobox", "City")
	if city.Expanded == nil || *city.Expanded {
		t.Fatalf("the closed combobox = %+v, want an expanded state of false", city)
	}
	if city.HasPopup != "listbox" {
		t.Fatalf("aria-autocomplete=list gave HasPopup %q", city.HasPopup)
	}
	email := candidateNamed(t, obs, "textbox", "Email address")
	if !email.Required || email.Invalid || email.Value != "" {
		t.Fatalf("the required field = %+v", email)
	}
	if len(obs.Alerts) != 0 {
		t.Fatalf("a page that has said nothing reported %q", obs.Alerts)
	}

	// Typing opens the list: the state the page writes on itself arrives, and
	// the options it created are candidates of their own.
	res, err := e.Act(ctxT(t), "alpha", proto.Action{Kind: proto.ActType, Ref: city.Ref, Text: "Ma"})
	if err != nil || !res.OK {
		t.Fatalf("type: %+v %v", res, err)
	}
	obs = observe(t, e, "alpha", proto.ObserveRequest{MaxCandidates: proto.MaxCandidates})
	city = candidateNamed(t, obs, "combobox", "City")
	if city.Expanded == nil || !*city.Expanded {
		t.Fatalf("the open combobox = %+v, want an expanded state of true", city)
	}
	for _, name := range []string{"Madrid", "Murcia", "Marbella"} {
		_ = candidateNamed(t, obs, "option", name)
	}

	// Submitting an empty required field: the page's complaint is what the
	// executor is told, not the field it came from.
	res, err = e.Act(ctxT(t), "alpha", proto.Action{
		Kind: proto.ActClick, Ref: refOf(t, obs, "button", "Send"),
	})
	if err != nil || !res.OK {
		t.Fatalf("click: %+v %v", res, err)
	}
	obs = observe(t, e, "alpha", proto.ObserveRequest{MaxCandidates: proto.MaxCandidates})
	if len(obs.Alerts) == 0 || !strings.Contains(obs.Alerts[0], "Enter your email address") {
		t.Fatalf("alerts = %q, want the page's complaint", obs.Alerts)
	}
	if email = candidateNamed(t, obs, "textbox", "Email address"); !email.Invalid {
		t.Fatalf("the rejected field = %+v, want it invalid", email)
	}

	// The excerpt can be asked for around one element of the observation just
	// made: the complaint is what sits beside the field it is about, and the
	// beginning of the page is not what a reader of a rejected form needs.
	const textBytes = 24
	// The window first: the ref belongs to the observation just made, and the
	// next one supersedes it.
	around := observe(t, e, "alpha", proto.ObserveRequest{TextBytes: textBytes, TextAround: email.Ref})
	top := observe(t, e, "alpha", proto.ObserveRequest{TextBytes: textBytes})
	if !top.Truncated || !around.Truncated {
		t.Fatalf("a page cut to %d bytes is not truncated: %+v %+v", textBytes, top, around)
	}
	if !strings.Contains(around.Text, "Send") || strings.Contains(around.Text, "City") {
		t.Fatalf("the window around the field = %q, want the page beside it", around.Text)
	}
	if !strings.HasPrefix(top.Text, "City") {
		t.Fatalf("the unanchored excerpt = %q, want the top of the page", top.Text)
	}
	if around.Text == top.Text {
		t.Fatalf("TextAround gave the top of the page: %q", top.Text)
	}
	// A ref of an observation that has been superseded is not a place on the
	// page: the text is the beginning of it, as it is without one.
	stale := observe(t, e, "alpha", proto.ObserveRequest{TextBytes: textBytes, TextAround: email.Ref})
	if stale.Text != top.Text {
		t.Fatalf("an unknown ref gave %q, want the top %q", stale.Text, top.Text)
	}
}

// What a flat list of roles and names leaves out: which element takes
// characters, which box an option came out of, and what the page has opened
// over itself.
func TestIntegrationObserveWidgets(t *testing.T) {
	e := open(t, "alpha")
	goTo(t, e, "alpha", "/widgets")
	all := proto.ObserveRequest{MaxCandidates: proto.MaxCandidates}

	// A modal is the page: the shortlist holds its own controls and nothing
	// else, while Total still counts everything behind it.
	obs := observe(t, e, "alpha", all)
	if obs.Modal != "Cookies" || !slices.Contains(obs.Dialogs, "Cookies") {
		t.Fatalf("modal = %q, dialogs = %q, want the cookie wall", obs.Modal, obs.Dialogs)
	}
	if len(obs.Candidates) != 2 {
		t.Fatalf("shortlist behind a modal = %+v, want its two buttons", obs.Candidates)
	}
	if obs.Total <= len(obs.Candidates) {
		t.Fatalf("total = %d with a shortlist of %d: the page behind the modal was not counted",
			obs.Total, len(obs.Candidates))
	}
	res, err := e.Act(ctxT(t), "alpha", proto.Action{Kind: proto.ActClick, Ref: refOf(t, obs, "button", "Accept")})
	if err != nil || !res.OK {
		t.Fatalf("accept: %+v %v", res, err)
	}

	// The wall is gone and the page is reachable again. A div the page calls a
	// combobox is a button that opens a list; the date field is a field.
	obs = observe(t, e, "alpha", all)
	if obs.Modal != "" || len(obs.Dialogs) != 0 {
		t.Fatalf("after accepting: modal = %q, dialogs = %q", obs.Modal, obs.Dialogs)
	}
	trip := candidateNamed(t, obs, "combobox", "Trip type")
	if trip.Editable {
		t.Fatalf("the div combobox = %+v, want it not editable", trip)
	}
	depart := candidateNamed(t, obs, "textbox", "Departure date")
	if !depart.Editable {
		t.Fatalf("the date field = %+v, want it editable", depart)
	}
	_ = refOf(t, obs, "button", "Search")

	// Opening the list: every option says which box it came out of, and the
	// box says which control opened it.
	res, err = e.Act(ctxT(t), "alpha", proto.Action{Kind: proto.ActClick, Ref: trip.Ref})
	if err != nil || !res.OK {
		t.Fatalf("click the combobox: %+v %v", res, err)
	}
	obs = observe(t, e, "alpha", all)
	trip = candidateNamed(t, obs, "combobox", "Trip type")
	list := candidateNamed(t, obs, "listbox", "Ticket type")
	if list.In != trip.Ref {
		t.Fatalf("the listbox is in %q, want the combobox %q", list.In, trip.Ref)
	}
	options := 0
	for _, c := range obs.Candidates {
		if c.Role != "option" {
			continue
		}
		options++
		if c.In != list.Ref {
			t.Fatalf("option %d is in %q, want the listbox %q", options, c.In, list.Ref)
		}
	}
	if options != 3 {
		t.Fatalf("the open list gave %d options, want 3", options)
	}

	// A field that opens a dialog when it takes focus: the page the rest of a
	// queue was not decided on.
	res, err = e.Act(ctxT(t), "alpha", proto.Action{
		Kind: proto.ActClick, Ref: refOf(t, obs, "textbox", "Departure date"),
	})
	if err != nil || !res.OK {
		t.Fatalf("focus the date field: %+v %v", res, err)
	}
	obs = observe(t, e, "alpha", all)
	if !slices.Contains(obs.Dialogs, "Choose date") {
		t.Fatalf("dialogs = %q, want the date picker", obs.Dialogs)
	}
	if obs.Modal != "" {
		t.Fatalf("a picker that never claimed to be modal gave modal = %q", obs.Modal)
	}
}

// candidateNamed finds one candidate by role and name.
func candidateNamed(t *testing.T, obs proto.Observation, role, name string) proto.Candidate {
	t.Helper()
	for _, c := range obs.Candidates {
		if c.Role == role && c.Name == name {
			return c
		}
	}
	t.Fatalf("no %s named %q among %+v", role, name, obs.Candidates)
	return proto.Candidate{}
}

// The date field a real booking form has, read off a running browser: a field
// nothing can be typed into, the sentence the page wrote about how it works,
// the calendar of roleless divs it opens, and the banner it keeps standing.
func TestIntegrationObserveDates(t *testing.T) {
	e := open(t, "alpha")
	goTo(t, e, "alpha", "/dates")
	all := proto.ObserveRequest{MaxCandidates: proto.MaxCandidates}

	obs := observe(t, e, "alpha", all)
	depart := candidateNamed(t, obs, "textbox", "Fecha vuelta")
	if depart.Editable {
		t.Fatalf("the readonly date field = %+v, want it not editable", depart)
	}
	if !strings.Contains(depart.Description, "flechas de dirección") {
		t.Fatalf("description = %q, want the page's own sentence", depart.Description)
	}
	if !candidateNamed(t, obs, "textbox", "Pasajeros").Editable {
		t.Fatal("a plain field beside it is not editable")
	}
	// A button drawn rather than written is still called something.
	_ = refOf(t, obs, "button", "Abrir calendario")
	// The banner is an announcement the first time it is seen and not after:
	// a live region that never changes a word is news once.
	if !slices.Contains(obs.Alerts, "We use cookies to make this site work") {
		t.Fatalf("alerts = %q, want the live region", obs.Alerts)
	}
	again := observe(t, e, "alpha", all)
	if len(again.Alerts) != 0 {
		t.Fatalf("the same banner was announced twice: %q", again.Alerts)
	}

	// Clicking the field opens a calendar of divs with no role at all. They
	// rank below every field, button and link on the page and would be cut —
	// what puts them on the list is that they were not there before. The ref
	// is the one the observation just made minted; the earlier one is stale.
	res, err := e.Act(ctxT(t), "alpha", proto.Action{
		Kind: proto.ActClick, Ref: refOf(t, again, "textbox", "Fecha vuelta"),
	})
	if err != nil || !res.OK {
		t.Fatalf("click the date field: %+v %v", res, err)
	}
	obs = observe(t, e, "alpha", all)
	if len(obs.Candidates) > proto.MaxCandidates {
		t.Fatalf("shortlist of %d, want at most %d", len(obs.Candidates), proto.MaxCandidates)
	}
	days := 0
	for _, c := range obs.Candidates {
		if c.Role != "button" || c.Name != strconv.Itoa(days+1) {
			break
		}
		if !c.New {
			t.Fatalf("the day cell %q is not marked new: %+v", c.Name, c)
		}
		days++
	}
	if days != 30 {
		t.Fatalf("the shortlist starts with %d day cells, want the 30 the click opened: %+v",
			days, obs.Candidates)
	}
	if obs.Candidates[days].New {
		t.Fatalf("the page the calendar opened over is marked new: %+v", obs.Candidates[days])
	}

	// And they are still there on the next observation: a cell that vanished
	// the moment it stopped being new would be a calendar nobody can finish
	// using. The picker is worked through the ref this observation minted.
	obs = observe(t, e, "alpha", all)
	if c := candidateNamed(t, obs, "button", "17"); c.New {
		t.Fatalf("a cell that was there before is marked new: %+v", c)
	}
	res, err = e.Act(ctxT(t), "alpha", proto.Action{
		Kind: proto.ActClick, Ref: refOf(t, obs, "button", "17"),
	})
	if err != nil || !res.OK {
		t.Fatalf("click the day cell: %+v %v", res, err)
	}
	obs = observe(t, e, "alpha", all)
	if v := candidateNamed(t, obs, "textbox", "Fecha vuelta").Value; v != "17" {
		t.Fatalf("the date field holds %q, want the day the click picked", v)
	}
}
