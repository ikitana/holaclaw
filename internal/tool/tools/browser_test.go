package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"holaclaw/internal/browser"
	"holaclaw/internal/browser/proto"
	"holaclaw/internal/event"
	"holaclaw/internal/tool"
)

// fakeBrowser is a BrowserBackend with a scripted result and a record of the
// task it was handed.
type fakeBrowser struct {
	mu sync.Mutex

	res    browser.Result
	err    error
	allow  []string
	bodies map[string]string
	dlErr  error

	tasks []browser.Task
	runs  int
}

func (f *fakeBrowser) Run(_ context.Context, t browser.Task) (browser.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	f.tasks = append(f.tasks, t)
	return f.res, f.err
}

func (f *fakeBrowser) Download(_ context.Context, _ string, d proto.Download) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dlErr != nil {
		return nil, f.dlErr
	}
	body, ok := f.bodies[d.ID]
	if !ok {
		return nil, errors.New("no such download")
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

func (f *fakeBrowser) Allow(string) []string { return f.allow }

func (f *fakeBrowser) task(t *testing.T) browser.Task {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tasks) != 1 {
		t.Fatalf("the backend ran %d tasks, want 1", len(f.tasks))
	}
	return f.tasks[0]
}

// browserRig wires browser.do with fakes and captures its events.
type browserRig struct {
	tool   tool.Tool
	back   *fakeBrowser
	blobs  *fakeBlobs
	mu     sync.Mutex
	events []event.Event
}

func newBrowserRig(t *testing.T, back *fakeBrowser) *browserRig {
	t.Helper()
	rig := &browserRig{back: back, blobs: &fakeBlobs{}}
	r := tool.NewRegistry()
	sink := event.SinkFunc(func(_ context.Context, e event.Event) {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		rig.events = append(rig.events, e)
	})
	if err := RegisterBrowser(r, back, rig.blobs, sink); err != nil {
		t.Fatalf("RegisterBrowser: %v", err)
	}
	tl, ok := r.Get("browser.do")
	if !ok {
		t.Fatal("browser.do is not registered")
	}
	rig.tool = tl
	return rig
}

func (rig *browserRig) run(t *testing.T, c tool.Call) tool.Result {
	t.Helper()
	if c.Name == "" {
		c.Name = "browser.do"
	}
	if c.Agent == "" {
		c.Agent = "muse"
	}
	if c.Role == "" {
		c.Role = "owner"
	}
	c.TraceID, c.SessionID = event.NewID(), event.NewID()
	res, err := rig.tool.Run(context.Background(), c)
	if err != nil {
		t.Fatalf("browser.do returned a Go error: %v", err)
	}
	return res
}

func doneResult() browser.Result {
	return browser.Result{
		Outcome: browser.OutcomeDone,
		Answer:  "12 EUR",
		URL:     "https://shop.example/widget",
	}
}

func TestBrowserSchemaSize(t *testing.T) {
	rig := newBrowserRig(t, &fakeBrowser{res: doneResult()})
	n := tool.SchemaTokens(rig.tool.Spec())
	t.Logf("browser.do %d tokens", n)
	if n > tool.MaxSchemaTokens {
		t.Errorf("browser.do: %d tokens, cap is %d", n, tool.MaxSchemaTokens)
	}
	if rig.tool.Risk() != tool.RiskExternal {
		t.Errorf("risk %q, want external", rig.tool.Risk())
	}
}

// TestBrowserSpecSaysItReadsPictures: the browser reads a page's images,
// charts and photos off the screen, and a model that is never told so asks
// for a picture's text somewhere else — or not at all. A direct image URL is
// a fine place to start it, which the url parameter has to say.
func TestBrowserSpecSaysItReadsPictures(t *testing.T) {
	rig := newBrowserRig(t, &fakeBrowser{res: doneResult()})
	spec := rig.tool.Spec()
	if !strings.Contains(spec.Description, "images") {
		t.Errorf("the description never says it reads images: %q", spec.Description)
	}
	if !strings.Contains(string(spec.Parameters), "image URL") {
		t.Errorf("the url parameter never mentions an image URL: %s", spec.Parameters)
	}
}

func TestBrowserRefusesGuests(t *testing.T) {
	back := &fakeBrowser{res: doneResult()}
	rig := newBrowserRig(t, back)
	res := rig.run(t, tool.Call{Role: "guest", Args: json.RawMessage(`{"intent":"buy milk"}`)})
	if !res.IsError || !strings.Contains(res.Text, "guests") {
		t.Fatalf("result %+v", res)
	}
	if back.runs != 0 {
		t.Errorf("a guest reached the browser")
	}
}

func TestBrowserArguments(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"not an object", `["intent"]`, "JSON object"},
		{"no intent", `{}`, "intent is required"},
		{"blank intent", `{"intent":"   "}`, "intent is required"},
		{"intent too long", `{"intent":"` + strings.Repeat("x", maxIntentBytes+1) + `"}`, "cap is"},
		{"file url", `{"intent":"read it","url":"file:///etc/passwd"}`, "http or https"},
		{"javascript url", `{"intent":"read it","url":"javascript:alert(1)"}`, "http or https"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			back := &fakeBrowser{res: doneResult()}
			rig := newBrowserRig(t, back)
			res := rig.run(t, tool.Call{Args: json.RawMessage(tc.args)})
			if !res.IsError || !strings.Contains(res.Text, tc.want) {
				t.Fatalf("result %+v, want an error mentioning %q", res, tc.want)
			}
			if back.runs != 0 {
				t.Errorf("bad arguments still reached the browser")
			}
		})
	}
}

func TestBrowserResultIsTainted(t *testing.T) {
	back := &fakeBrowser{res: doneResult(), allow: []string{"shop.example"}}
	rig := newBrowserRig(t, back)

	res := rig.run(t, tool.Call{
		Args:    json.RawMessage(`{"intent":"find the widget price","url":"https://shop.example/"}`),
		Role:    "user",
		RunKind: tool.RunInteractive,
		UserID:  event.NewID(),
	})
	if res.IsError {
		t.Fatalf("result %+v", res)
	}
	if !res.Tainted {
		t.Error("a result read off a web page must be tainted")
	}
	for _, want := range []string{"outcome: done", "answer: 12 EUR", "url: https://shop.example/widget"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("result text %q does not hold %q", res.Text, want)
		}
	}

	task := back.task(t)
	switch {
	case task.Agent != "muse" || task.Intent != "find the widget price":
		t.Errorf("task %+v", task)
	case task.URL != "https://shop.example/":
		t.Errorf("task url %q", task.URL)
	case task.Role != "user" || task.UserID == "" || task.TraceID == "" || task.SessionID == "":
		t.Errorf("task envelope %+v", task)
	case len(task.Allow) != 1 || task.Allow[0] != "shop.example":
		t.Errorf("allow-list %v", task.Allow)
	case task.Detached:
		t.Error("an interactive run must not be detached")
	}
}

func TestBrowserTaskRunIsDetached(t *testing.T) {
	back := &fakeBrowser{res: browser.Result{Outcome: browser.OutcomeBlocked, Notes: "the site asked for a sign-in"}}
	rig := newBrowserRig(t, back)
	res := rig.run(t, tool.Call{
		Args: json.RawMessage(`{"intent":"check the balance"}`), RunKind: tool.RunTask, TaskID: event.NewID(),
	})
	if res.IsError {
		t.Errorf("a blocked task is a result, not an error: %+v", res)
	}
	if !strings.Contains(res.Text, "outcome: blocked") || !strings.Contains(res.Text, "notes: the site asked") {
		t.Errorf("result text %q", res.Text)
	}
	if !back.task(t).Detached {
		t.Error("a task run must be detached: nobody can take over")
	}
}

func TestBrowserDownloadsBecomeAttachments(t *testing.T) {
	res := doneResult()
	res.Downloads = []proto.Download{
		{ID: "d1", Name: "../../prices.csv", Size: 12},
		{ID: "d2", Name: "huge.bin", Size: maxAttachBytes + 1},
		{ID: "d3", Name: "gone.txt", Size: 3},
	}
	back := &fakeBrowser{res: res, bodies: map[string]string{"d1": "a,b\n1,2\n"}}
	rig := newBrowserRig(t, back)

	out := rig.run(t, tool.Call{Args: json.RawMessage(`{"intent":"download the prices"}`)})
	if len(out.Attachments) != 1 {
		t.Fatalf("attachments %+v", out.Attachments)
	}
	att := out.Attachments[0]
	switch {
	case att.Name != "prices.csv":
		t.Errorf("name %q: the sidecar's name must be reduced to a base name", att.Name)
	case att.MIME != "text/csv":
		t.Errorf("mime %q", att.MIME)
	case att.Size != 8 || att.BlobID == "" || att.SHA256 != att.BlobID:
		t.Errorf("attachment %+v", att)
	}
	if !strings.Contains(out.Text, "downloads attached: prices.csv") {
		t.Errorf("result text %q", out.Text)
	}

	rig.mu.Lock()
	defer rig.mu.Unlock()
	if len(rig.events) != 1 || rig.events[0].Type != event.BrowserDownload {
		t.Fatalf("events %+v", rig.events)
	}
	a := map[string]any{}
	if err := json.Unmarshal(rig.events[0].Attrs, &a); err != nil {
		t.Fatalf("attrs: %v", err)
	}
	if a["agent"] != "muse" || a["name"] != "prices.csv" || a["size"] != float64(8) {
		t.Errorf("browser.download attrs %v", a)
	}
}

func TestBrowserNoBrowserConfigured(t *testing.T) {
	back := &fakeBrowser{err: ErrNoBrowser}
	rig := newBrowserRig(t, back)
	res := rig.run(t, tool.Call{Args: json.RawMessage(`{"intent":"find it"}`)})
	if !res.IsError || !strings.Contains(res.Text, "no browser") {
		t.Fatalf("result %+v", res)
	}
	if res.Tainted {
		t.Error("a refused call read no page")
	}
}

func TestBrowserInternalFailureIsAGoError(t *testing.T) {
	back := &fakeBrowser{err: errors.New("the sidecar is unreachable")}
	rig := newBrowserRig(t, back)
	_, err := rig.tool.Run(context.Background(), tool.Call{
		Name: "browser.do", Agent: "muse", Role: "owner",
		Args: json.RawMessage(`{"intent":"find it"}`),
	})
	if err == nil {
		t.Fatal("an executor failure must be a Go error")
	}
}

func TestRegisterBrowserNeedsWiring(t *testing.T) {
	if err := RegisterBrowser(tool.NewRegistry(), nil, &fakeBlobs{}, nil); err == nil {
		t.Error("RegisterBrowser without a backend returned no error")
	}
	if err := RegisterBrowser(tool.NewRegistry(), &fakeBrowser{}, nil, nil); err == nil {
		t.Error("RegisterBrowser without a blob store returned no error")
	}
	if err := RegisterBrowser(tool.NewRegistry(), &fakeBrowser{}, &fakeBlobs{}, nil); err != nil {
		t.Errorf("RegisterBrowser with no sink: %v", err)
	}
}

// A run that returned a screenshot attaches exactly one JPEG, after the
// downloads, and says so in a line of its own.
func TestBrowserScreenshotBecomesAnAttachment(t *testing.T) {
	res := doneResult()
	res.Screenshot = []byte("\xff\xd8\xff a jpeg")
	res.Downloads = []proto.Download{{ID: "d1", Name: "prices.csv", Size: 8}}
	back := &fakeBrowser{res: res, bodies: map[string]string{"d1": "a,b\n1,2\n"}}
	rig := newBrowserRig(t, back)

	out := rig.run(t, tool.Call{Args: json.RawMessage(`{"intent":"send me a screenshot of the prices"}`)})
	if len(out.Attachments) != 2 {
		t.Fatalf("attachments %+v, want the download and the screenshot", out.Attachments)
	}
	att := out.Attachments[1]
	switch {
	case att.Name != "screenshot.jpg" || att.MIME != "image/jpeg" || att.Kind != event.KindFromMIME("image/jpeg"):
		t.Errorf("attachment %+v", att)
	case att.Size != int64(len(res.Screenshot)) || att.BlobID == "" || att.SHA256 != att.BlobID:
		t.Errorf("attachment %+v", att)
	}
	if string(rig.blobs.body) != string(res.Screenshot) || rig.blobs.mime != "image/jpeg" {
		t.Errorf("the blob is not the screenshot: %q as %q", rig.blobs.body, rig.blobs.mime)
	}
	if !strings.Contains(out.Text, "\nscreenshot: true") {
		t.Errorf("result text %q", out.Text)
	}
	// The screenshot is not a download: it is not listed as one, and no
	// browser.download is emitted for it.
	if !strings.Contains(out.Text, "downloads attached: prices.csv\n") {
		t.Errorf("result text %q", out.Text)
	}
	if !out.Tainted {
		t.Error("a result with a picture of a page must be tainted")
	}
	if strings.Contains(out.Text, "a jpeg") {
		t.Error("the image is in the result text")
	}
	rig.mu.Lock()
	defer rig.mu.Unlock()
	if len(rig.events) != 1 || rig.events[0].Type != event.BrowserDownload {
		t.Fatalf("events %+v, want the download's alone", rig.events)
	}
}

// The screenshot line is always there, and says false whenever no image is
// attached: none was returned, it is over the cap, or it could not be stored.
func TestBrowserScreenshotLineSaysFalseWithoutOne(t *testing.T) {
	cases := []struct {
		name    string
		shot    []byte
		blobErr error
		outcome browser.Outcome
	}{
		{"none returned", nil, nil, browser.OutcomeDone},
		{"a failed run", nil, nil, browser.OutcomeFailed},
		{"over the cap", make([]byte, maxAttachBytes+1), nil, browser.OutcomeDone},
		{"not stored", []byte("jpeg"), errors.New("disk full"), browser.OutcomeDone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := doneResult()
			res.Outcome, res.Screenshot = tc.outcome, tc.shot
			rig := newBrowserRig(t, &fakeBrowser{res: res})
			rig.blobs.err = tc.blobErr

			out := rig.run(t, tool.Call{Args: json.RawMessage(`{"intent":"find the price"}`)})
			if len(out.Attachments) != 0 {
				t.Errorf("attachments %+v, want none", out.Attachments)
			}
			if !strings.HasSuffix(out.Text, "\nscreenshot: false") {
				t.Errorf("result text %q", out.Text)
			}
			if out.IsError || !strings.Contains(out.Text, "outcome: "+string(tc.outcome)) {
				t.Errorf("the outcome changed: %q", out.Text)
			}
		})
	}
}

// At the cap exactly, the screenshot is still attached.
func TestBrowserScreenshotAtTheCap(t *testing.T) {
	res := doneResult()
	res.Screenshot = make([]byte, maxAttachBytes)
	rig := newBrowserRig(t, &fakeBrowser{res: res})
	out := rig.run(t, tool.Call{Args: json.RawMessage(`{"intent":"send me a screenshot"}`)})
	if len(out.Attachments) != 1 || !strings.HasSuffix(out.Text, "\nscreenshot: true") {
		t.Fatalf("attachments %+v, text %q", out.Attachments, out.Text)
	}
}
