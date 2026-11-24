package browser

import (
	"context"
	"strings"

	"holaclaw/internal/browser/proto"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// Screenshots: every one a run takes is reported as a browser.screenshot
// event, and a run that has its answer may return one of the page it ended on.
// The image is the page's own pixels. It reaches a model that can look at it,
// and the agent as an attachment; it never reaches a log, a trace or an event.

// What a browser.screenshot event carries as why: what the screenshot was for.
const (
	ShotLook     = "look"     // the vision rung looked for a control
	ShotScreen   = "screen"   // the extraction read the answer off the screen
	ShotThin     = "thin"     // the same, of a page with no text to read
	ShotReturn   = "return"   // the picture a run returns with its answer
	ShotTakeover = "takeover" // the one a chat channel gets with a takeover request
)

// pictureFloor is the probability at or above which the decision model's
// "should the answer come with a picture" is believed.
const pictureFloor = 0.6

// pictureYes are the ways the chat model says yes to the same question.
var pictureYes = []string{"yes", "sí"}

// ScreenshotAttrs is a browser.screenshot event: one per screenshot taken,
// whatever took it. URL and Title are the page's, the two page-derived values
// the trace carries anyway; they are absent on a takeover's. Attached says the
// image was returned to the agent.
type ScreenshotAttrs struct {
	Why      string `json:"why"`
	Size     int    `json:"size"`
	TookMS   int64  `json:"took_ms"`
	URL      string `json:"url,omitempty"`
	Title    string `json:"title,omitempty"`
	Attached bool   `json:"attached"`
}

// shotUse is what became of a screenshot once it was taken.
type shotUse int

const (
	shotKept     shotUse = iota // a model looked at it, and that was all
	shotAttached                // it was returned to the agent
)

// shot is one screenshot and what its event says of it.
type shot struct {
	data  []byte
	attrs ScreenshotAttrs
}

// shoot takes one screenshot of the page obs was read off. nil when the
// sidecar could not take one.
func (e *Executor) shoot(ctx context.Context, r *run, why string, obs proto.Observation) *shot {
	start := e.now()
	data, err := e.o.Sidecar.Screenshot(ctx, r.task.Agent)
	if err != nil || len(data) == 0 {
		return nil
	}
	return &shot{data: data, attrs: ScreenshotAttrs{
		Why: why, Size: len(data), TookMS: e.now().Sub(start).Milliseconds(),
		URL: obs.URL, Title: obs.Title,
	}}
}

// emitShot reports a screenshot and what became of it; nil is none taken.
func (e *Executor) emitShot(ctx context.Context, r *run, s *shot, use shotUse) {
	if s == nil {
		return
	}
	a := s.attrs
	a.Attached = use == shotAttached
	e.emit(ctx, r, event.BrowserScreenshot, a)
}

// picture is the screenshot a done run returns with its answer, or nil. read
// is the screenshot the extraction read this same page off, which is reused;
// use says what became of read. A question nobody could answer and a
// screenshot that failed both mean no picture, and the run is done all the
// same.
func (e *Executor) picture(ctx context.Context, r *run, answer string, read *shot) (data []byte, use shotUse) {
	if !e.wantsPicture(ctx, r, answer) {
		return nil, shotKept
	}
	if read != nil {
		return read.data, shotAttached
	}
	s := e.shoot(ctx, r, ShotReturn, proto.Observation{URL: r.url, Title: r.title})
	if s == nil {
		e.log(r).Info("browser: the page could not be photographed for the answer")
		return nil, shotKept
	}
	e.emitShot(ctx, r, s, shotAttached)
	return s.data, shotKept
}

// wantsPicture asks once whether the answer should come with a picture of the
// page: the request asks for one, or the answer is clearer as one. The
// decision model answers when there is one, else the chat model with one word.
// It is a model's call on the request and the answer, never a list of words a
// request might contain. Anything but a readable yes is a no.
func (e *Executor) wantsPicture(ctx context.Context, r *run, answer string) bool {
	var state strings.Builder
	state.WriteString(pageRule + "\n\nrequest: " + oneLine(r.task.Intent, 4000) + "\n")
	if r.extractBy == extractByScreen {
		state.WriteString(pictureReadOffScreen + "\n")
	}
	// The answer came off the page, so it goes inside the fence with it.
	state.WriteString("\n" + fencePage("url: "+oneLine(r.url, 300)+"\ntitle: "+oneLine(r.title, proto.MaxNameBytes)+
		"\nanswer: "+oneLine(answer, 1000)+"\n"))

	if e.o.Decider == nil {
		resp, err := e.chat(ctx, e.o.ChatModel, e.o.Chat, pictureSystem, decideReserve, provider.ReasoningNone,
			provider.TextMessage(provider.RoleUser, state.String()))
		r.stats.DecisionCalls++
		if err != nil {
			e.log(r).Info("browser: nobody could say whether the answer wants a picture")
			return false
		}
		r.count(resp.Usage)
		return sentinel(resp.Text, pictureYes)
	}
	resp, err := e.o.Decider.Decide(ctx, provider.DecisionRequest{
		Model: e.o.DecideModel, State: state.String(),
		Questions: map[string]provider.Question{QuestionPicture: {
			Type:         provider.QuestionNoul,
			Instructions: pictureInstructions,
			Criteria:     map[string]string{"true": pictureTrue, "false": pictureFalse},
		}},
	})
	r.stats.DecisionCalls++
	if resp != nil {
		r.count(resp.Usage) // an invalid reply comes with its usage too
	}
	if err != nil || resp == nil {
		e.log(r).Info("browser: nobody could say whether the answer wants a picture")
		return false
	}
	a, ok := resp.Answers[QuestionPicture]
	return ok && clamp01(a.Noul) >= pictureFloor
}
