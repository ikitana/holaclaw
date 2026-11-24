package browser

import (
	"errors"
	"strings"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// shotBytes stands for a JPEG. It is distinctive so a test can look for it
// where no image may ever be.
const shotBytes = "QQJPEGQQ-bytes"

// pictureRun is the ordinary run — one click, the answer selected off the
// result page — with the picture question answered p.
func pictureRun(t *testing.T, intent string, p answers) (*rig, Result) {
	t.Helper()
	r := newRig(t, nil)
	r.searchSite()
	r.side.shot = []byte(shotBytes)
	r.chat.replies = []string{moveJSON(t, false, "", click("r2", "results are listed"))}
	r.dec.script = []answers{{}, {effect: 0.9, goal: 1}, {span: "s2", cover: 0.9}, p}
	return r, r.run(Task{Intent: intent})
}

// shotEvents is the attributes of every browser.screenshot the run emitted.
func (r *rig) shotEvents() []map[string]any {
	var out []map[string]any
	for _, e := range r.typed(event.BrowserScreenshot) {
		out = append(out, r.attrs(e))
	}
	return out
}

// finishedScreenshot is the screenshot attribute of browser.task.finished,
// which is always there.
func (r *rig) finishedScreenshot() bool {
	r.t.Helper()
	v, ok := r.attrs(r.typed(event.BrowserTaskFinished)[0])["screenshot"].(bool)
	if !ok {
		r.t.Fatalf("browser.task.finished carries no screenshot bool")
	}
	return v
}

// TestARequestForAScreenshotReturnsOne: the intent asks for a picture, the
// decision model says so, and the run ends done with one JPEG of the page it
// ended on.
func TestARequestForAScreenshotReturnsOne(t *testing.T) {
	r, res := pictureRun(t, "find the widget and send me a screenshot of the results", answers{picture: 0.9})
	if res.Outcome != OutcomeDone || res.Answer != "12 EUR" {
		t.Fatalf("outcome %q / %q, answer %q", res.Outcome, res.Notes, res.Answer)
	}
	if string(res.Screenshot) != shotBytes {
		t.Fatalf("screenshot %q, want the page's", res.Screenshot)
	}
	if n := r.side.screenshots(); n != 1 {
		t.Errorf("%d screenshots, want exactly one", n)
	}
	if !r.finishedScreenshot() {
		t.Error("browser.task.finished says no screenshot was returned")
	}
	evs := r.shotEvents()
	if len(evs) != 1 {
		t.Fatalf("%d browser.screenshot events, want 1: %v", len(evs), evs)
	}
	e := evs[0]
	if e["why"] != ShotReturn || e["attached"] != true || e["size"] != float64(len(shotBytes)) {
		t.Errorf("event %v", e)
	}
	if e["url"] != "https://search.example/results" || e["title"] != "Results" {
		t.Errorf("event names the page %v / %v", e["url"], e["title"])
	}
	if _, ok := e["took_ms"]; !ok {
		t.Errorf("event has no took_ms: %v", e)
	}

	// The question is asked once, on the request and the answer, and the
	// answer — which came off the page — is inside the fence.
	if n := r.dec.timesAsked(QuestionPicture); n != 1 {
		t.Fatalf("the picture question was asked %d times, want once", n)
	}
	st := r.dec.states[len(r.dec.states)-1]
	i := strings.LastIndex(st, pageOpen)
	if i < 0 || !strings.Contains(st[:i], "send me a screenshot") {
		t.Errorf("the request is not in the state, outside the fence: %q", st)
	}
	if !strings.Contains(st[i:], "answer: 12 EUR") || strings.Contains(st[:i], "12 EUR") {
		t.Errorf("the answer is not inside the fence: %q", st)
	}
}

// TestAnAnswerClearerAsAPictureReturnsOne: nobody asked for a picture. The
// model judges the answer clearer as one, on criteria that name what words
// lose, and that is enough.
func TestAnAnswerClearerAsAPictureReturnsOne(t *testing.T) {
	r, res := pictureRun(t, "how did the widget's price move this year", answers{picture: 0.8})
	if res.Outcome != OutcomeDone || string(res.Screenshot) != shotBytes {
		t.Fatalf("outcome %q, screenshot %q", res.Outcome, res.Screenshot)
	}
	q, ok := r.dec.asked(QuestionPicture)
	if !ok || q.Type != provider.QuestionNoul {
		t.Fatalf("the picture question was not asked as a score: %+v", q)
	}
	if !strings.Contains(q.Criteria["true"], "chart") || !strings.Contains(q.Criteria["true"], "asks for a screenshot") {
		t.Errorf("the criteria do not offer both grounds: %q", q.Criteria["true"])
	}
	if !r.finishedScreenshot() {
		t.Error("browser.task.finished says no screenshot was returned")
	}
}

// TestAnOrdinaryAnswerReturnsNoScreenshot: the model says the answer is
// complete as text, and the page is never photographed.
func TestAnOrdinaryAnswerReturnsNoScreenshot(t *testing.T) {
	for _, p := range []float64{0, 0.3, pictureFloor - 0.01} {
		r, res := pictureRun(t, "", answers{picture: p})
		if res.Outcome != OutcomeDone || res.Answer != "12 EUR" {
			t.Fatalf("p=%v: outcome %q / %q", p, res.Outcome, res.Notes)
		}
		if res.Screenshot != nil {
			t.Errorf("p=%v: a screenshot was returned", p)
		}
		if n := r.side.screenshots(); n != 0 {
			t.Errorf("p=%v: %d screenshots, want none", p, n)
		}
		if len(r.shotEvents()) != 0 || r.finishedScreenshot() {
			t.Errorf("p=%v: the trace says a screenshot was taken", p)
		}
	}
}

// TestAnUnreadablePictureAnswerMeansNoScreenshot: a reply that does not carry
// the answer is no answer, and the run is done all the same.
func TestAnUnreadablePictureAnswerMeansNoScreenshot(t *testing.T) {
	r, res := pictureRun(t, "send me a screenshot of the results", answers{noPicture: true})
	if res.Outcome != OutcomeDone || res.Answer != "12 EUR" {
		t.Fatalf("outcome %q / %q", res.Outcome, res.Notes)
	}
	if res.Screenshot != nil || r.side.screenshots() != 0 || r.finishedScreenshot() {
		t.Errorf("a screenshot was taken on an unreadable answer")
	}
}

// TestWithoutADecisionModelTheChatModelDecides: with no `decide` role the chat
// model is asked the same thing and answers with one word. Anything that is
// not a readable yes is a no.
func TestWithoutADecisionModelTheChatModelDecides(t *testing.T) {
	cases := []struct {
		reply string
		want  bool
	}{
		{"yes", true},
		{"Yes.", true},
		{"Yes: the request asks for one", true},
		{"no", false},
		{"it depends on the page", false},
		{"", false},
		{"yesterday's prices are text", false},
	}
	for _, tc := range cases {
		t.Run(tc.reply, func(t *testing.T) {
			r := newRig(t, func(o *Options) { o.Decider = nil })
			r.searchSite()
			r.side.shot = []byte(shotBytes)
			r.chat.replies = []string{
				moveJSON(t, false, "", click("r2", "results are listed")),
				`{"risk":"none"}`,
				`{"effect":0.9,"goal":1.0}`,
				"12 EUR",
				tc.reply,
			}

			res := r.run(Task{Intent: "send me a screenshot of the widget results"})
			if res.Outcome != OutcomeDone || res.Answer != "12 EUR" {
				t.Fatalf("outcome %q / %q, answer %q", res.Outcome, res.Notes, res.Answer)
			}
			if got := res.Screenshot != nil; got != tc.want {
				t.Fatalf("screenshot returned = %v, want %v", got, tc.want)
			}
			if got := r.finishedScreenshot(); got != tc.want {
				t.Errorf("finished screenshot = %v, want %v", got, tc.want)
			}
			reqs := r.chat.requests()
			last := reqs[len(reqs)-1]
			if last.System != pictureSystem {
				t.Fatalf("the last chat call is not the picture question: %q", last.System)
			}
			if !strings.Contains(last.System, "exactly one word") || !strings.Contains(last.System, pageRule) {
				t.Errorf("the prompt asks for no single word, or has no page rule: %q", last.System)
			}
			if imageOf(last) != nil {
				t.Error("the question carried an image")
			}
		})
	}
}

// TestAFailingScreenshotKeepsTheOutcome: the model wants a picture and the
// sidecar cannot take one. The run is done all the same, without one.
func TestAFailingScreenshotKeepsTheOutcome(t *testing.T) {
	r := newRig(t, nil)
	r.searchSite()
	r.side.shotErr = errors.New("the tab is gone")
	r.chat.replies = []string{moveJSON(t, false, "", click("r2", "results are listed"))}
	r.dec.script = []answers{{}, {effect: 0.9, goal: 1}, {span: "s2", cover: 0.9}, {picture: 0.9}}

	res := r.run(Task{Intent: "send me a screenshot of the results"})
	if res.Outcome != OutcomeDone || res.Answer != "12 EUR" {
		t.Fatalf("outcome %q / %q, answer %q", res.Outcome, res.Notes, res.Answer)
	}
	if res.Screenshot != nil || r.finishedScreenshot() {
		t.Error("a screenshot was returned though none could be taken")
	}
	if n := r.side.screenshots(); n != 1 {
		t.Errorf("%d attempts, want one", n)
	}
	if evs := r.shotEvents(); len(evs) != 0 {
		t.Errorf("a screenshot that was not taken was logged: %v", evs)
	}
}

// TestAScreenReadIsReusedAsThePicture: the extraction already photographed
// this page to read it. The run returns that image, takes no second one, and
// its one event says it was attached.
func TestAScreenReadIsReusedAsThePicture(t *testing.T) {
	r := newRig(t, func(o *Options) { o.ChatModel = provider.Model{Name: "chat", Vision: true} })
	r.searchSite()
	r.resultsText("")
	r.side.shot = []byte(shotBytes)
	r.chat.replies = []string{
		moveJSON(t, false, "", click("r2", "results are listed")),
		"12 EUR",
	}
	r.dec.script = []answers{{}, {effect: 0.9, goal: 1}, {picture: 0.9}}

	res := r.run(Task{})
	if res.Outcome != OutcomeDone || res.Answer != "12 EUR" {
		t.Fatalf("outcome %q / %q, answer %q", res.Outcome, res.Notes, res.Answer)
	}
	if string(res.Screenshot) != shotBytes {
		t.Fatalf("screenshot %q, want the one the page was read off", res.Screenshot)
	}
	if n := r.side.screenshots(); n != 1 {
		t.Errorf("%d screenshots, want the screen read's alone", n)
	}
	evs := r.shotEvents()
	if len(evs) != 1 || evs[0]["why"] != ShotThin || evs[0]["attached"] != true {
		t.Fatalf("events %v, want one thin screenshot, attached", evs)
	}
	// The question is told what the text cannot say, outside the fence.
	st := r.dec.states[len(r.dec.states)-1]
	if i := strings.LastIndex(st, pageOpen); i < 0 || !strings.Contains(st[:i], pictureReadOffScreen) {
		t.Errorf("the state does not say the answer was read off the screen: %q", st)
	}
}

// TestEveryScreenshotIsLogged: whatever takes a screenshot, the trace gets one
// browser.screenshot for it, with what it was for and whether it was returned.
func TestEveryScreenshotIsLogged(t *testing.T) {
	t.Run("look", func(t *testing.T) {
		vision := &fakeChat{replies: []string{"the round blue Go button"}}
		r := newRig(t, func(o *Options) {
			o.Vision, o.VisionModel = vision, provider.Model{Name: "vision"}
		})
		r.searchSite()
		r.side.shot = []byte(shotBytes)
		r.chat.replies = []string{`{"done":false,"moves":[]}`, `{"done":false,"moves":[]}`}
		r.dec.script = []answers{
			{strategy: string(stratLook)},
			{element: "r2", conf: 0.9}, {effect: 0.9, goal: 1}, {span: "s2", cover: 0.9}, {},
		}
		if res := r.run(Task{}); res.Outcome != OutcomeDone || res.Screenshot != nil {
			t.Fatalf("outcome %q / %q", res.Outcome, res.Notes)
		}
		evs := r.shotEvents()
		if len(evs) != 1 || evs[0]["why"] != ShotLook || evs[0]["attached"] != false {
			t.Fatalf("events %v, want one look, not attached", evs)
		}
		if evs[0]["url"] != "https://search.example/" || evs[0]["title"] != "Search" {
			t.Errorf("the look names the wrong page: %v", evs[0])
		}
	})

	t.Run("screen read", func(t *testing.T) {
		vision := &fakeChat{replies: []string{"12 EUR"}}
		r := newRig(t, func(o *Options) {
			o.Vision, o.VisionModel = vision, provider.Model{Name: "vision"}
		})
		r.searchSite()
		r.resultsText(longPage)
		r.side.shot = []byte(shotBytes)
		r.chat.replies = []string{
			moveJSON(t, false, "", click("r2", "results are listed")),
			"not found",
		}
		r.dec.script = []answers{{}, {effect: 0.9, goal: 1}, {}, {}}
		if res := r.run(Task{}); res.Outcome != OutcomeDone || res.Screenshot != nil {
			t.Fatalf("outcome %q / %q", res.Outcome, res.Notes)
		}
		evs := r.shotEvents()
		if len(evs) != 1 || evs[0]["why"] != ShotScreen || evs[0]["attached"] != false {
			t.Fatalf("events %v, want one screen read, not attached", evs)
		}
	})

	t.Run("thin page", func(t *testing.T) {
		r := newRig(t, func(o *Options) { o.ChatModel = provider.Model{Name: "chat", Vision: true} })
		r.searchSite()
		r.resultsText("")
		r.side.shot = []byte(shotBytes)
		r.chat.replies = []string{
			moveJSON(t, false, "", click("r2", "results are listed")),
			"12 EUR",
		}
		r.dec.script = []answers{{}, {effect: 0.9, goal: 1}, {picture: 0.1}}
		if res := r.run(Task{}); res.Outcome != OutcomeDone || res.Screenshot != nil {
			t.Fatalf("outcome %q / %q", res.Outcome, res.Notes)
		}
		evs := r.shotEvents()
		if len(evs) != 1 || evs[0]["why"] != ShotThin || evs[0]["attached"] != false {
			t.Fatalf("events %v, want one thin screenshot, not attached", evs)
		}
	})

	t.Run("a screen read that found nothing", func(t *testing.T) {
		// The run goes on after it, and the screenshot is logged all the same.
		r := newRig(t, func(o *Options) {
			o.ChatModel = provider.Model{Name: "chat", Vision: true}
			o.MaxMoveCalls = 1
		})
		r.searchSite()
		r.resultsText("")
		r.side.shot = []byte(shotBytes)
		r.chat.replies = []string{
			moveJSON(t, false, "", click("r2", "results are listed")),
			"not found", "not found", "not found",
		}
		r.dec.script = []answers{{}, {effect: 0.9, goal: 1}}
		if res := r.run(Task{}); res.Outcome != OutcomeFailed || res.Screenshot != nil {
			t.Fatalf("outcome %q / %q", res.Outcome, res.Notes)
		}
		evs := r.shotEvents()
		if len(evs) == 0 {
			t.Fatal("no browser.screenshot for a screen read that found nothing")
		}
		for _, e := range evs {
			if e["why"] != ShotThin || e["attached"] != false {
				t.Errorf("event %v, want thin, not attached", e)
			}
		}
		if n := r.side.screenshots(); n != len(evs) {
			t.Errorf("%d screenshots, %d events", n, len(evs))
		}
		if r.finishedScreenshot() {
			t.Error("a failed run says it returned a screenshot")
		}
	})
}

// TestNoImageBytesReachATraceOrALog: the image is returned to the agent and
// shown to a model that can look at it. No event and no log line carries it.
func TestNoImageBytesReachATraceOrALog(t *testing.T) {
	r, res := pictureRun(t, "send me a screenshot of the results", answers{picture: 0.9})
	if string(res.Screenshot) != shotBytes {
		t.Fatalf("no screenshot to look for: %q", res.Screenshot)
	}
	if strings.Contains(r.every(), "QQJPEGQQ") {
		t.Error("an event carries the image")
	}
	if strings.Contains(r.logs.String(), "QQJPEGQQ") {
		t.Error("a log line carries the image")
	}
	for _, st := range r.dec.decided() {
		if strings.Contains(st, "QQJPEGQQ") {
			t.Error("a decision state carries the image")
		}
	}
}
