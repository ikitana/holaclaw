package browser

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"holaclaw/internal/browser/proto"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// The executor is the daemon half of docs/browser.md. M11's shape: one text
// call per turn, made on a page it has already read, asking for up to three
// moves; the decision model binds an element when the move only described one,
// classifies what a move commits, and scores what each move did against what it
// said it would do and against the request. There is no plan written before the
// first page, and no re-plan — a move call *is* the re-plan, with the page and
// the failures in front of it.
//
// Nothing a page says reaches a log line. The trace carries the model's own
// account of what it is doing (the moves, the final title and URL), opaque
// refs, closed-set labels, scores, counts and durations — and never a value
// typed into a field.

// Defaults for Options.
const (
	DefaultMaxSteps        = 40
	DefaultMaxMoveCalls    = 6
	DefaultMoveCallCeiling = 15
	DefaultDeadline        = 4 * time.Minute
	DefaultConfidenceFloor = 0.6
	DefaultMaxAnswerBytes  = 4096
	DefaultPlanTimeout     = 30 * time.Second
)

// Floors and reserves that are not worth a knob. effectFloor and goalFloor are
// the two gates of the loop: below effectFloor a move did not do what it was
// for and the queue behind it goes; at or above goalFloor the run reads its
// answer off the page and stops.
//
// The reserves are generous, not tight. Every answer this package asks for is a
// few lines long, but a reasoning model spends its reply budget on thinking
// before it writes any of them, and a reserve sized for the answer is spent on
// the reasoning: the reply comes back finished "length" with no text at all,
// which is not a bad answer but no answer. Output tokens are only billed for
// what is generated, so a reserve that is never reached costs nothing.
const (
	effectFloor    = 0.5
	goalFloor      = 0.6
	moveReserve    = 4096
	decideReserve  = 2048
	extractReserve = 1024
	visionReserve  = 512
	// extractTextBytes is the whole of what proto allows: the final read is
	// one call per run, and a page cut short is the one failure the rung
	// cannot recover from — a list whose tail was never collected cannot be
	// selected or summarised back. The move loop keeps its smaller budget,
	// since it is billed once per step and only needs enough to steer.
	extractTextBytes = proto.MaxTextBytes
	moveTextBytes    = 4 << 10
	maxTakeovers     = 2
	// maxNoMove is how often the move call may find nothing to do on a page
	// before the late rungs are offered: once is a page that may still be
	// settling, twice is one the text model cannot read.
	maxNoMove   = 2
	scrollDelta = 600
	heldPoll    = time.Second

	// The move budget is earned rather than fixed: a task nobody can size in
	// advance is sized by what it is doing. moveCallExtension is what a run
	// that is still getting somewhere is granted when its budget runs out, and
	// progressWindow how recently the last move that worked has to have been —
	// past that the run is going in circles and the budget ends. MoveCallCeiling,
	// MaxSteps and Deadline are the hard stops either way.
	moveCallExtension = 3
	progressWindow    = 2

	// The load gate. loadFloor is the probability at or above which the
	// decision model's "is this page still loading" answer is believed, and
	// maxLoadWaits how many times in a row one step may be held back.
	//
	// The waiting itself is the browser's, not ours: the executor asks the
	// sidecar for a proto.ActWait and Chromium's lifecycle events and the
	// document's own quiescence decide when it returns (page/quiet.go). The two
	// bounds below are only the points at which a page that never stops is
	// decided on as it stands.
	//
	// They are deliberately far apart. loadWait is for a page that says it is
	// still loading — a navigation is worth waiting out. settleWait is for a
	// page that has arrived and is merely catching up with a move: a form
	// applying a value, a suggestion list drawing itself. That one is bounded
	// hard, because the loop can simply look again — three settles cost less
	// than a second between them, and a page still not ready after that wants
	// the move call, not more waiting.
	loadFloor    = 0.6
	maxLoadWaits = 3
	loadWait     = 10 * time.Second
	settleWait   = 250 * time.Millisecond
)

// Rungs of the ladder, as they appear in a browser.step event and in
// Stats.Rungs. The shape is M10's, so a trace and the eval histogram still
// read: 2 is a move the model grounded itself, 3 one the decision model bound
// from a description, 5 vision, 6 a person, 7 an honest failure.
const (
	rungMove    = 2
	rungChoice  = 3
	rungVision  = 5
	rungPerson  = 6
	rungFailure = 7
)

// Fixed reasons and notes. Every one of them is a constant of this package: a
// note reaches the model and a reason reaches the trace, and page text in
// either is how an injected instruction escapes the fence.
const (
	whyNotAllowed = "not_allowed"
	whyApproval   = "approval_refused"

	notesApprovalRefused = "approval refused"
	notesNotAllowed      = "outside the allow-list"
	notesSteps           = "ran out of steps"
	notesTime            = "ran out of time"
	notesOpenFailed      = "the starting page could not be opened"
)

// emptyPageFmt is the fail-fast on a start URL — often one an agent invented as
// a deep link — that rendered nothing at all. Looping over a blank page only
// spends the budget for nothing.
const emptyPageFmt = "empty page at %s: no usable content; try the site's entry page"

// outOfBudgetNotes is the honest failure of a run that spent its move calls:
// where it ended up, in fixed words around two page-derived values (a title and
// a URL) the agent is told about deliberately, the same way Result.Answer is.
func outOfBudgetNotes(r *run) string {
	return fmt.Sprintf("ran out of moves: ended at %q — %s", r.title, r.url)
}

// errRunEnded is the run giving up while it waited for the takeover lock to
// come free: the deadline passed, or the caller went away. It is a Result, not
// a Go error.
var errRunEnded = errors.New("browser: the run ended while a person was driving")

// ended reports whether err is the run giving up rather than a real failure.
func ended(err error) bool { return errors.Is(err, errRunEnded) }

// Options is the executor's wiring. Sidecar, Humans, Chat and ChatModel are
// required; everything else has a default or is a rung that is skipped.
type Options struct {
	Sidecar Sidecar
	Humans  Humans

	// Chat is the agent's `chat` role: the extractor, the move call when there
	// is no `plan` role, and — with no Decider — the narrow questions too.
	Chat      provider.Provider
	ChatModel provider.Model
	// Decider is the `decide` role. Without it the chat model answers the
	// narrow questions through a strict JSON prompt: slower, works.
	Decider     provider.Decider
	DecideModel provider.Model
	// Vision is the `vision` role, rung 5. nil skips the rung.
	Vision      provider.Provider
	VisionModel provider.Model
	// Plan is the `plan` role: the model that chooses the moves (M11 — the role
	// no longer plans anything in advance). nil falls back to Chat/ChatModel;
	// the route matters more than the model (docs/models.md "Planner latency").
	Plan      provider.Provider
	PlanModel provider.Model

	Events event.Sink
	Logger *slog.Logger
	Now    func() time.Time

	// MaxSteps bounds the actions one task may take, MaxMoveCalls the text
	// calls it may spend deciding them, Deadline the wall time of the run.
	MaxSteps     int
	MaxMoveCalls int
	// MoveCallCeiling is how far MaxMoveCalls may be extended for a run that
	// keeps getting somewhere: the budget starts at MaxMoveCalls and is only
	// ever raised by moves that did what they said they would.
	MoveCallCeiling int
	Deadline        time.Duration
	// ConfidenceFloor is the lowest element confidence a move the decision
	// model bound may be carried out with.
	ConfidenceFloor float64
	// MaxAnswerBytes caps the extracted answer.
	MaxAnswerBytes int
	// PlanTimeout bounds each move call's own context: a route that hangs costs
	// that attempt rather than the whole task.
	PlanTimeout time.Duration
}

// Executor runs browser tasks. One run per agent at a time; others queue.
// Safe for concurrent use.
type Executor struct {
	o    Options
	mu   sync.Mutex
	busy map[string]chan struct{}
}

// NewExecutor checks the wiring and fills the defaults in.
func NewExecutor(o Options) (*Executor, error) {
	switch {
	case o.Sidecar == nil:
		return nil, errors.New("browser: NewExecutor needs a sidecar")
	case o.Humans == nil:
		return nil, errors.New("browser: NewExecutor needs a Humans")
	case o.Chat == nil:
		return nil, errors.New("browser: NewExecutor needs a chat provider")
	}
	if o.Events == nil {
		o.Events = event.Discard
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.MaxSteps <= 0 {
		o.MaxSteps = DefaultMaxSteps
	}
	if o.MaxMoveCalls <= 0 {
		o.MaxMoveCalls = DefaultMaxMoveCalls
	}
	if o.MoveCallCeiling <= 0 {
		o.MoveCallCeiling = DefaultMoveCallCeiling
	}
	if o.MoveCallCeiling < o.MaxMoveCalls {
		// A ceiling below the budget it caps would shorten the run instead of
		// lengthening it: the configured budget wins.
		o.MoveCallCeiling = o.MaxMoveCalls
	}
	if o.Deadline <= 0 {
		o.Deadline = DefaultDeadline
	}
	if o.ConfidenceFloor <= 0 {
		o.ConfidenceFloor = DefaultConfidenceFloor
	}
	if o.MaxAnswerBytes <= 0 {
		o.MaxAnswerBytes = DefaultMaxAnswerBytes
	}
	if o.PlanTimeout <= 0 {
		o.PlanTimeout = DefaultPlanTimeout
	}
	return &Executor{o: o, busy: map[string]chan struct{}{}}, nil
}

// run is one task in flight.
type run struct {
	task      Task
	stats     Stats
	downloads []proto.Download
	url       string
	// title is the last observed page's own title, kept alongside url for the
	// "ended at" line of a finished or failed run.
	title   string
	started time.Time
	// moveCalls is the budget spent and moveBudget the budget itself, which
	// starts at Options.MaxMoveCalls and grows with progress; progressAt is
	// the move call the last move that worked belonged to. history is what has
	// been tried, oldest first, and lastNote the one line the move call left
	// itself.
	moveCalls  int
	moveBudget int
	progressAt int
	history    []tried
	lastNote   string
	vision     bool // the vision rung has been spent
	// track is what the run has actually done — the pages it has been on, the
	// goal it has reached, the elements that keep failing — and the whole of
	// what says it is getting somewhere or stuck (track.go).
	track *track
	// firstURL is the address of the first web page the run ever read, which
	// is where a restart goes back to. "" until the run has been on one: a
	// task without a start URL begins on a blank page, which is nowhere to go
	// back to.
	firstURL string
	// avoid is what a recovery tells the next move call to leave alone: the
	// elements a move has failed on twice, as `role "name"`. Page-derived, so
	// it goes into the move request inside the fence.
	avoid []string
	// lastStepURL is the URL the last-emitted browser.step event carried: the
	// attribute is only worth repeating when it changed.
	lastStepURL string
	// emptyCheckURL is the start URL the run opened, kept only long enough for
	// the fail-fast on the first observation; "" once that check has run.
	emptyCheckURL string
	// extract is what the extraction call cost, for the finished event, and
	// which rung produced the answer.
	extractPrompt, extractOutput int
	// partialAt is the page an extraction last called partial on, and
	// partialNotes what a result carries when the run settled for the part a
	// page had (extract.go).
	partialAt    fingerprint
	partialNotes string
	extractBy    string
	// screenWhy is which of the extraction's three triggers asked for the
	// screenshot, in fixed words: "thin", "look" or "not_found".
	screenWhy string
	// screenedAt is the page the loop last offered to the screen read before
	// making a move on it: one read per page, so a click that comes back to
	// the same picture does not pay for a second screenshot.
	screenedAt fingerprint
}

// progress books the run getting somewhere, which is the only thing that buys
// it more move calls. What counts is a fact about the page or about the run —
// an address it had never been at, a goal score higher than any before it, an
// alert or an invalid mark a move cleared, a suggestion taken — and never a
// move's own effect score: a calendar that opens and closes scores well every
// time and gets no closer to anything (track.go).
func (r *run) progress() { r.progressAt = r.moveCalls }

// spent reports whether the run has no move call left, extending the budget
// first when it is still getting somewhere. A task's complexity is not
// knowable before the first page — M11 removed the plan that pretended it was
// — so the budget is earned from what the run has actually done: another
// moveCallExtension calls while a move that worked is no further back than
// progressWindow, up to MoveCallCeiling. A run going in circles still stops on
// the original MaxMoveCalls.
func (e *Executor) spent(r *run) bool {
	if r.moveCalls < r.moveBudget {
		return false
	}
	// progressAt is zero for a run that has not yet made a move that worked:
	// nothing to extend for.
	if r.moveBudget >= e.o.MoveCallCeiling || r.progressAt == 0 || r.moveCalls-r.progressAt > progressWindow {
		return true
	}
	r.moveBudget = min(r.moveBudget+moveCallExtension, e.o.MoveCallCeiling)
	e.log(r).Info("browser: the move budget was extended for a run that is still getting somewhere",
		"budget", r.moveBudget, "ceiling", e.o.MoveCallCeiling)
	return false
}

func (r *run) result(o Outcome, notes string) Result {
	return Result{Outcome: o, Notes: notes, URL: r.url, Title: r.title}
}

// count adds one answered call, generative or decision, to the task's
// measurement. Every caller counts a reply as soon as it has one, before it
// knows whether the reply is usable, so each call is paid for once.
func (r *run) count(u provider.Usage) { r.stats.Usage.Add(event.Call(u)) }

// stepURL is what a browser.step event's own URL attribute carries: url when it
// differs from the last step's, "" otherwise.
func (r *run) stepURL(url string) string {
	if url == "" || url == r.lastStepURL {
		return ""
	}
	r.lastStepURL = url
	return url
}

// startedAttrs / finishedAttrs bracket a task in the trace.
type startedAttrs struct {
	Agent    string `json:"agent"`
	Detached bool   `json:"detached"`
	// Decider is which backend answers the narrow questions: "decision" for a
	// decision model, "chat" for the fallback — so a slow run explains itself.
	Decider string `json:"decider"`
	// QueuedMS is how long the run waited for another run of the same agent
	// to give the browser back; absent when it did not wait, and present (if
	// zero) when it did, however briefly.
	QueuedMS *int64 `json:"queued_ms,omitempty"`
}

// queuedAttrs is the browser.step a run emits when it has to wait for the
// browser: the agent has one, another run of it holds it, and whoever is
// waiting on this one is told why nothing is happening yet.
type queuedAttrs struct {
	Held string `json:"held"`
	Why  string `json:"why"`
}

// The queued step's words: fixed, like every other held step.
const (
	heldQueued = "queued"
	whyBusyRun = "another_run"
)

type finishedAttrs struct {
	Outcome         string `json:"outcome"`
	Steps           int    `json:"steps"`
	GenerativeCalls int    `json:"generative_calls"`
	DecisionCalls   int    `json:"decision_calls"`
	MoveCalls       int    `json:"move_calls"`
	// MoveBudget is what the budget had grown to by the end: MaxMoveCalls for
	// a run that never earned an extension, more for one that kept working.
	// Waits is how often a step was held back for a page that had not
	// finished loading.
	MoveBudget int `json:"move_budget,omitempty"`
	Waits      int `json:"waits,omitempty"`
	// Recoveries is how often the run was pulled out of a dead end: a stuck
	// reason fired and a strategy was carried out.
	Recoveries int `json:"recoveries,omitempty"`
	// The task's usage: every call it made, generative and decision alike
	// (the extraction's included — the extract_* numbers are its share, not
	// an addition), which is why this event and not browser.move counts
	// towards a trace's total (event.UsageTypes). None when no call was made.
	*event.UsageAttrs
	Downloads int   `json:"downloads,omitempty"`
	Answer    bool  `json:"answer"`
	TookMS    int64 `json:"took_ms"`
	// Rungs is the ladder histogram (docs/browser.md): index 2 a grounded move, 3 an
	// element the decision model bound, 5 vision, 6 a person, 7 failure.
	Rungs [8]int `json:"rungs"`
	// The extraction rung's own measurement. ExtractBy is which rung answered:
	// "selection" when the decision model picked the span off the page, "chat"
	// when a generative call read it, "screen" when one looked at a
	// screenshot of it. ScreenReads says a screenshot was read; neither the
	// image nor what it showed is ever traced. The spans are page text and
	// are never traced either.
	ExtractBy               string `json:"extract_by,omitempty"`
	ExtractTookMS           int64  `json:"extract_took_ms,omitempty"`
	ExtractPromptTokens     int    `json:"extract_prompt_tokens,omitempty"`
	ExtractCompletionTokens int    `json:"extract_completion_tokens,omitempty"`
	ScreenReads             int    `json:"screen_reads,omitempty"`
	// ScreenWhy is what asked for the screenshot: "thin" (the page had no
	// text to read), "look" (the text rung said the answer is in a picture)
	// or "not_found" (the text rung did not find it).
	ScreenWhy string `json:"screen_why,omitempty"`
	// URL and Title are where the browser ended up: the same two page-derived
	// values Result carries to the agent, kept on the trace too so a person
	// reading it sees where a run stopped without having to replay it.
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
}

type blockedAttrs struct {
	Why string `json:"why"`
}

// Run carries out one task. A Go error means the executor itself failed (the
// sidecar is unreachable, a provider call failed); everything the agent should
// see — blocked, failed, done — comes back as a Result.
func (e *Executor) Run(ctx context.Context, t Task) (Result, error) {
	switch {
	case t.Agent == "":
		return Result{}, errors.New("browser: task without an agent")
	case strings.TrimSpace(t.Intent) == "":
		return Result{}, errors.New("browser: task without an intent")
	case t.URL != "" && !isHTTPURL(t.URL):
		return Result{}, errors.New("browser: task URL is not http(s)")
	}

	r := &run{task: t, moveBudget: e.o.MaxMoveCalls, track: newTrack()}
	asked := e.now()
	queued := false
	release, err := e.acquire(ctx, t.Agent, func() {
		queued = true
		e.log(r).Info("browser: waiting for another run of this agent to give the browser back")
		e.emit(ctx, r, event.BrowserStep, queuedAttrs{Held: heldQueued, Why: whyBusyRun})
	})
	if err != nil {
		return Result{}, err
	}
	defer release()

	start := e.now()
	ctx, cancel := context.WithDeadline(ctx, start.Add(e.o.Deadline))
	defer cancel()

	r.started = start
	decider := "chat"
	if e.o.Decider != nil {
		decider = "decision"
	}
	started := startedAttrs{Agent: t.Agent, Detached: t.Detached, Decider: decider}
	if queued {
		ms := start.Sub(asked).Milliseconds()
		started.QueuedMS = &ms
	}
	e.emit(ctx, r, event.BrowserTaskStarted, started)

	res, err := e.execute(ctx, r)
	r.stats.Took = e.now().Sub(start)
	outcome := "error"
	if err == nil {
		res.Stats = r.stats
		res.Downloads = r.downloads
		if res.URL == "" {
			res.URL = r.url
		}
		if res.Title == "" {
			res.Title = r.title
		}
		outcome = string(res.Outcome)
	}
	e.emit(ctx, r, event.BrowserTaskFinished, finishedAttrs{
		Outcome: outcome, Steps: r.stats.Steps, GenerativeCalls: r.stats.GenerativeCall,
		DecisionCalls: r.stats.DecisionCalls, MoveCalls: r.stats.MoveCalls,
		MoveBudget: r.moveBudget, Waits: r.stats.Waits, Recoveries: r.stats.Recoveries,
		UsageAttrs: r.stats.Usage.Attrs(), Downloads: len(r.downloads), Answer: res.Answer != "",
		TookMS: r.stats.Took.Milliseconds(), Rungs: r.stats.Rungs,
		ExtractBy:           r.extractBy,
		ExtractTookMS:       r.stats.ExtractTook.Milliseconds(),
		ExtractPromptTokens: r.extractPrompt, ExtractCompletionTokens: r.extractOutput,
		ScreenReads: r.stats.ScreenReads, ScreenWhy: r.screenWhy,
		URL: res.URL, Title: res.Title,
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

// execute is Run without the bracketing: open, go where the task said to start,
// then the loop.
//
// Every run opens on a page of its own (proto.OpenRequest.Fresh): the agent
// has one browser and one profile, but where a run starts is never where the
// last one — another session's, or a background task's — left off. A person
// driving the browser keeps it until they hand it back: the tabs they are on
// are not replaced under them.
func (e *Executor) execute(ctx context.Context, r *run) (Result, error) {
	if err := e.waitReleased(ctx, r); err != nil {
		return r.result(OutcomeFailed, notesTime), nil
	}
	if _, err := e.o.Sidecar.Open(ctx, r.task.Agent, proto.OpenRequest{Allow: r.task.Allow, Fresh: true}); err != nil {
		return Result{}, fmt.Errorf("browser: open: %w", err)
	}

	if u := strings.TrimSpace(r.task.URL); isHTTPURL(u) {
		res, err := e.act(ctx, r, proto.Action{Kind: proto.ActNavigate, URL: u})
		if ended(err) {
			return r.result(OutcomeFailed, notesTime), nil
		}
		if err != nil {
			return Result{}, fmt.Errorf("browser: open start url: %w", err)
		}
		if !res.OK {
			if res.Code == proto.ErrNotAllowed {
				e.emitBlocked(ctx, r, whyNotAllowed)
				return r.result(OutcomeBlocked, notesNotAllowed), nil
			}
			return r.result(OutcomeFailed, notesOpenFailed), nil
		}
		r.emptyCheckURL = u
	}
	return e.loop(ctx, r)
}

// acquire serialises runs per agent: one browser, one driver. Waiting honours
// ctx, so a queued task still gives up when its turn never comes. waiting is
// called once, before the first wait, and not at all when the browser is free.
//
// Runs of one agent stay serialised, whichever session they come from: the
// sidecar drives one active tab per agent, and two runs side by side would
// share its profile, its downloads and its takeover lock with no way to tell
// whose is whose. What a queued run gets instead is to say it is queued.
func (e *Executor) acquire(ctx context.Context, agent string, waiting func()) (func(), error) {
	for {
		e.mu.Lock()
		ch, held := e.busy[agent]
		if !held {
			ch = make(chan struct{})
			e.busy[agent] = ch
			e.mu.Unlock()
			return func() {
				e.mu.Lock()
				delete(e.busy, agent)
				e.mu.Unlock()
				close(ch)
			}, nil
		}
		e.mu.Unlock()
		if waiting != nil {
			waiting()
			waiting = nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// chat is one non-streaming model call: every generative rung goes through it.
// Every rung says how much thinking it needs: a move call a little, everything
// else none — measured: a reasoning model spent ~2k tokens (88 s) thinking
// before a two-step plan.
func (e *Executor) chat(ctx context.Context, m provider.Model, p provider.Provider, system string, reserve int, reasoning provider.Reasoning, msgs ...provider.Message) (*provider.Response, error) {
	stream, err := p.Chat(ctx, provider.Request{
		Model: m, System: system, Messages: msgs, MaxTokens: reserve, Reasoning: reasoning,
	})
	if err != nil {
		return nil, err
	}
	return provider.Collect(stream, nil)
}

func (e *Executor) now() time.Time { return e.o.Now() }

// sleep waits, or gives up when the run does.
func (e *Executor) sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Executor) emit(ctx context.Context, r *run, t event.Type, attrs any) {
	e.o.Events.Emit(ctx, event.New(t, r.task.TraceID, r.task.SessionID, "", attrs))
}

func (e *Executor) emitBlocked(ctx context.Context, r *run, why string) {
	e.emit(ctx, r, event.BrowserBlocked, blockedAttrs{Why: why})
}

// log carries the agent and the trace, and nothing a page wrote.
func (e *Executor) log(r *run) *slog.Logger {
	return e.o.Logger.With("agent", r.task.Agent, "trace", string(r.task.TraceID))
}
