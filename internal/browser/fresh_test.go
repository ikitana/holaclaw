package browser

import (
	"context"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// TestEachRunStartsOnAPageOfItsOwn is issue #136: one run of an agent left the
// browser on a page, and the next run — another session's — began on it. Two
// runs in a row; the second one's first observation is not where the first one
// ended.
func TestEachRunStartsOnAPageOfItsOwn(t *testing.T) {
	r := newRig(t, nil)
	r.searchSite()
	one := moveJSON(t, false, "", click("r2", "results are listed"))
	r.chat.replies = []string{one, one}
	r.dec.script = []answers{
		{}, {effect: 0.9, goal: 1}, {span: "s2", cover: 0.9},
		{}, {effect: 0.9, goal: 1}, {span: "s2", cover: 0.9},
	}

	first := r.run(Task{})
	if first.Outcome != OutcomeDone || first.URL != "https://search.example/results" {
		t.Fatalf("first run: outcome %q / %q at %s", first.Outcome, first.Notes, first.URL)
	}
	n := len(r.side.observedURLs())

	second := r.run(Task{})
	if second.Outcome != OutcomeDone {
		t.Fatalf("second run: outcome %q / %q", second.Outcome, second.Notes)
	}
	urls := r.side.observedURLs()
	if len(urls) <= n {
		t.Fatalf("the second run observed nothing: %v", urls)
	}
	if got := urls[n]; got == first.URL {
		t.Fatalf("the second run started on %s, where the first one ended", got)
	}
	if opens, freshes := r.side.openCounts(); opens != 2 || freshes != 2 {
		t.Errorf("%d opens, %d of them fresh: want every run on a fresh tab", opens, freshes)
	}
}

// TestABlankStartIsNotAPicture: a run without a start URL opens on a blank tab.
// That page has no text, but it is not an image to read off the screen — it is
// nothing, and the move call takes the run somewhere.
func TestABlankStartIsNotAPicture(t *testing.T) {
	vision := &fakeChat{replies: []string{"nothing here"}}
	r := newRig(t, func(o *Options) {
		o.MaxMoveCalls = 1
		o.Vision, o.VisionModel = vision, provider.Model{Name: "vision"}
	})
	r.searchSite()
	r.side.home = "about:blank"
	r.side.pages["about:blank"] = page{}
	r.side.shot = []byte("jpeg-bytes")
	r.chat.replies = []string{moveJSON(t, false, "",
		Move{Action: MoveNavigate, URL: "https://search.example/", Expect: "the search page is open"})}
	r.dec.script = []answers{{}, {effect: 0.9}}

	res := r.run(Task{})
	if urls := r.side.observedURLs(); len(urls) == 0 || urls[0] != "about:blank" {
		t.Fatalf("observed %v, want the run to start on the blank tab", urls)
	}
	if n := r.side.screenshots(); n != 0 {
		t.Errorf("%d screenshots of a blank tab, want none", n)
	}
	if res.Stats.MoveCalls == 0 {
		t.Error("no move call: want the blank tab handed to the move call")
	}
	if res.URL != "https://search.example/" {
		t.Errorf("ended at %q, want where the move took it", res.URL)
	}
}

// TestAQueuedRunSaysSo: runs of one agent are serialised, and one that has to
// wait for another — a chat turn behind a background task — says so on the
// trace at once, then says how long it waited when it starts.
func TestAQueuedRunSaysSo(t *testing.T) {
	r := newRig(t, nil)
	r.searchSite()
	// The first run holds the browser: it is waiting for a person to hand it
	// back, which is as good a reason as any to keep it.
	r.humans.hold = make(chan struct{})
	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel1()
	defer cancel2()

	task := func(trace event.ID) Task {
		return Task{Agent: "muse", Intent: "find the widget price", TraceID: trace}
	}
	t1, t2 := event.NewID(), event.NewID()
	done := make(chan Result, 2)
	go func() {
		res, _ := r.exec.Run(ctx1, task(t1))
		done <- res
	}()
	waitFor(t, "the first run to hold the browser", func() bool { return r.humans.waitCount() > 0 })
	go func() {
		res, _ := r.exec.Run(ctx2, task(t2))
		done <- res
	}()

	queued := func() []event.Event {
		var out []event.Event
		for _, ev := range r.typed(event.BrowserStep) {
			if r.attrs(ev)["held"] == heldQueued {
				out = append(out, ev)
			}
		}
		return out
	}
	waitFor(t, "the second run to say it is queued", func() bool { return len(queued()) > 0 })
	q := queued()
	if len(q) != 1 || q[0].TraceID != t2 || r.attrs(q[0])["why"] != whyBusyRun {
		t.Fatalf("queued steps %+v, want one of the second run's trace", q)
	}
	// Nothing of the second run has started: it is waiting, and says only that.
	for _, ev := range r.typed(event.BrowserTaskStarted) {
		if ev.TraceID == t2 {
			t.Fatal("the second run started while the first one held the browser")
		}
	}

	cancel1()
	<-done
	waitFor(t, "the second run to start", func() bool {
		for _, ev := range r.typed(event.BrowserTaskStarted) {
			if ev.TraceID == t2 {
				return true
			}
		}
		return false
	})
	cancel2()
	<-done

	for _, ev := range r.typed(event.BrowserTaskStarted) {
		_, has := r.attrs(ev)["queued_ms"]
		if want := ev.TraceID == t2; has != want {
			t.Errorf("trace %s: queued_ms present %v, want %v", ev.TraceID, has, want)
		}
	}
}

// TestAFreeBrowserIsNotQueued: a run that finds the browser free says nothing
// about a queue.
func TestAFreeBrowserIsNotQueued(t *testing.T) {
	r := newRig(t, nil)
	r.searchSite()
	r.chat.replies = []string{moveJSON(t, false, "", click("r2", "results are listed"))}
	r.dec.script = []answers{{}, {effect: 0.9, goal: 1}, {span: "s2", cover: 0.9}}

	r.run(Task{})
	for _, ev := range r.typed(event.BrowserStep) {
		if r.attrs(ev)["held"] == heldQueued {
			t.Fatalf("a run on a free browser said it was queued: %s", ev.Attrs)
		}
	}
	if _, has := r.attrs(r.typed(event.BrowserTaskStarted)[0])["queued_ms"]; has {
		t.Error("queued_ms on a run that never waited")
	}
}
