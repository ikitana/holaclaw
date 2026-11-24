// Package browser is the daemon half of the browser (docs/browser.md): the
// executor that turns an intention into moves on a page it has already read,
// lets a decision model verify what each move did, and reports a compact
// result. It never holds a CDP connection — the sidecar (subpackages cdp,
// chromium, sidecar) owns Chromium and is reached through the Sidecar
// interface.
//
// Everything a page says is untrusted. It reaches a generative model only
// wrapped as untrusted content, it reaches a decision model only as state to
// be judged against closed sets, and it never reaches a trace event.
package browser

import (
	"context"
	"time"

	"holaclaw/internal/browser/proto"
	"holaclaw/internal/event"
)

// Task is one browser.do call.
type Task struct {
	Agent  string
	Intent string // what the user wants, in the agent's words
	URL    string // optional starting point
	// Allow is the agent's navigation allow-list; empty = any site.
	Allow []string

	TraceID   event.ID
	SessionID event.ID
	UserID    event.ID
	Role      string
	// Detached is true for task runs: nobody can take over, so a blocker that
	// needs a person fails the task.
	Detached bool
}

// Outcome is how a task ended.
type Outcome string

const (
	OutcomeDone    Outcome = "done"    // the intent was carried out
	OutcomeBlocked Outcome = "blocked" // login, captcha, 2fa, refused approval, allow-list
	OutcomeFailed  Outcome = "failed"  // ran out of moves, steps or time
)

// Result is all the agent loop ever sees of a browsing session: no snapshot,
// no page dump. Answer and Notes derive from page content, so the tool marks
// the turn tainted.
type Result struct {
	Outcome Outcome
	Answer  string // what was asked for, extracted; capped
	Notes   string // one line on where it stopped and why, for a blocked or failed task
	URL     string // where the browser ended up
	Title   string // the final page's own title, alongside URL (M11)
	// Downloads are files saved during the task, in the sidecar's quarantine.
	Downloads []proto.Download
	// Screenshot is one JPEG of the page a done run ended on, when the request
	// asked for a picture or the answer is clearer as one; nil otherwise. It is
	// the page's own pixels: tainted like Answer, and never logged or traced.
	Screenshot []byte
	Stats      Stats
}

// Stats is the measurement every task carries (docs/browser.md: the rung
// histogram is the metric nobody else publishes).
type Stats struct {
	Steps          int
	GenerativeCall int // move calls, vision, chat extraction
	DecisionCalls  int
	// MoveCalls is how many times the text model was asked what to do next:
	// the budget M11 spends instead of a plan and its re-plans.
	MoveCalls int
	// Waits is how often the run held a step back for a page that had not
	// finished loading, whether the page itself said so or the decision model
	// read it off what was rendered.
	Waits int
	// Recoveries is how often the run noticed it was stuck and did something
	// about it: at most maxRecoveries per run, after which it gives up.
	Recoveries int
	// ScreenReads is how often the extraction read the answer off a
	// screenshot instead of off the text: 0 or 1 per run, and the count says
	// it happened without saying anything about what was seen.
	ScreenReads int
	// Usage is every call the task made, generative and decision alike.
	Usage event.Usage
	Took  time.Duration
	// MoveTook is the time spent in move calls, ExtractTook in the extraction.
	MoveTook    time.Duration
	ExtractTook time.Duration
	// Rungs counts how often each rung of the ladder (docs/browser.md, M11) carried a
	// step: index 2 a move with its own ref, 3 an element the decision model
	// chose from a description, 5 vision, 6 a person, 7 failure. The histogram
	// is the metric nobody else publishes, so the shape is kept.
	Rungs [8]int
}

// MoveAction is the closed set of things one move may be. It maps onto
// proto.ActionKind; the model never emits selectors, only an opaque ref this
// package offered it, or words for the decision model to bind.
type MoveAction string

const (
	MoveClick    MoveAction = "click"
	MoveType     MoveAction = "type"   // Value into the element
	MoveSelect   MoveAction = "select" // choose Value in a select or combobox
	MoveKey      MoveAction = "key"    // press one key of moveKeys on the element
	MoveScroll   MoveAction = "scroll"
	MoveBack     MoveAction = "back"
	MoveWait     MoveAction = "wait"
	MoveNavigate MoveAction = "navigate" // URL, checked against the allow-list
)

// Move is one action the move call asked for, already validated: Action is a
// member of the set above, Ref (when set) is a ref of the observation the move
// was decided on, and URL (when set) is http(s).
type Move struct {
	Action MoveAction `json:"action"`
	// Ref is the element from the observation the model was shown. When it is
	// empty, Describe names the element in words and the decision model binds
	// it to a candidate of the live page.
	Ref      string `json:"ref,omitempty"`
	Describe string `json:"describe,omitempty"`
	// Value is what a type or select move puts in. It comes from the request,
	// never from the page, and is never logged, traced or shown to a person.
	// On a key move it is not text at all but the name of the key pressed,
	// which is a word of a closed set and may be written down.
	Value  string `json:"value,omitempty"`
	Submit bool   `json:"submit,omitempty"`
	URL    string `json:"url,omitempty"`
	// Expect is one short sentence that should be true afterwards; the
	// decision model scores the before/after diff against it.
	Expect string `json:"expect,omitempty"`
}

// MaxMovesPerCall bounds one move call's queue: enough to fill a form, too few
// to walk a site blind.
const MaxMovesPerCall = 3

// Blocker is the closed set of things that stop a run and need a person.
type Blocker string

const (
	BlockNone    Blocker = "none"
	BlockLogin   Blocker = "login"   // needs a person
	BlockCaptcha Blocker = "captcha" // needs a person
	Block2FA     Blocker = "2fa"     // needs a person
	BlockError   Blocker = "error"   // nothing on the page to act on
)

// RiskKind classifies a step for the approval framework.
type RiskKind string

const (
	RiskNone     RiskKind = "none"
	RiskPurchase RiskKind = "purchase"
	RiskSend     RiskKind = "send"    // posts, messages, emails, form submissions to people
	RiskDelete   RiskKind = "delete"  // deletes or cancels something
	RiskAccount  RiskKind = "account" // changes credentials or account settings
)

// Sidecar is the executor's view of the browser process. The client lives in
// subpackage sidecar; tests use a fake.
type Sidecar interface {
	Open(ctx context.Context, agent string, req proto.OpenRequest) (proto.Session, error)
	Observe(ctx context.Context, agent string, req proto.ObserveRequest) (proto.Observation, error)
	// Act reports an action that failed for a protocol reason as OK=false
	// with its Code, not as an error; the error is for transport failures.
	Act(ctx context.Context, agent string, a proto.Action) (proto.ActResult, error)
	Screenshot(ctx context.Context, agent string) ([]byte, error)
	Close(ctx context.Context, agent string) error
}

// Humans is how the executor reaches people. Both calls block until answered,
// timed out or ctx ends; the daemon implements them on the approval framework.
type Humans interface {
	// Approve asks owner / admins to allow one risky step. summary is built
	// by the executor from fixed words plus the step's own Target, never from
	// page text.
	Approve(ctx context.Context, t Task, kind RiskKind, summary string) (bool, error)
	// Takeover asks a person to get past a blocker in the live view and hand
	// the browser back. false = nobody did in time.
	Takeover(ctx context.Context, t Task, why Blocker) (bool, error)
	// WaitReleased blocks while a person holds the takeover lock of the
	// task's agent (a takeover the executor did not ask for) and returns
	// once it is free, so no observation is made — and no page state reaches
	// a model — while a person is driving. It returns at once when nobody
	// holds it; an error only when ctx ends.
	WaitReleased(ctx context.Context, t Task) error
}
