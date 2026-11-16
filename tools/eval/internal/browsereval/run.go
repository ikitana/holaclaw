package browsereval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"holaclaw/internal/browser"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/tools/eval/internal/record"
	"holaclaw/tools/eval/internal/sites"
)

// Retry policy for free tiers, which rate-limit aggressively — the same
// policy the scenario harness applies (harness.maxRateLimitRetries /
// harness.rateLimitBackoff).
const (
	maxRateLimitRetries = 3
	rateLimitBackoff    = 10 * time.Second
)

// One run is one (model × task × attempt): reset the site's per-run state,
// hand the intent to the real executor against the real page engine, and score
// what came back. Runs are sequential on purpose — one agent drives one
// browser at a time (docs/browser.md), and parallel runs would make the wall
// time and token numbers meaningless anyway.

// Agent is the agent name every run browses as: one profile, reused.
const Agent = "eval"

// DefaultTimeout bounds one task end to end.
const DefaultTimeout = 5 * time.Minute

// Target is one column of the report: a chat model, optionally a decision
// model, or the naive baseline loop over the same chat model.
type Target struct {
	// Label is what the report and the recording path call this column:
	// "<model>", "<model>+<decide>" or "<model>-baseline".
	Label string
	Model string // chat model name, as in models.toml
	// Decide is the decision model's name, "" for the chat fallback.
	Decide string
	// Plan is the plan role's model name (M10), "" for the chat fallback.
	Plan string

	// ChatSpec and NewChat build the chat role; DecideSpec and NewDecider the
	// decide role; PlanSpec and NewPlan the plan role — all nil when there is
	// none.
	ChatSpec   func() provider.Model
	NewChat    func() (provider.Provider, error)
	DecideSpec func() provider.Model
	NewDecider func() (provider.Decider, error)
	PlanSpec   func() provider.Model
	NewPlan    func() (provider.Provider, error)

	// Baseline runs the per-click chat loop of baseline.go instead of the
	// executor: the column the ladder is measured against.
	Baseline bool
	// Skip, when set, explains why this target cannot run (no API key).
	Skip string
}

// Options configures one browser evaluation.
type Options struct {
	Targets []Target
	Tasks   []Task
	// Sidecar is the page engine (or a fake): one browser, in this process.
	Sidecar browser.Sidecar
	// StartURL builds a site's starting URL for one run id.
	StartURL func(s sites.Site, run string) string
	// Host is the agent's navigation allow-list: the loopback host.
	Host string

	Runs      int
	Mode      record.Mode
	RecordDir string
	Timeout   time.Duration
	Logger    *slog.Logger
	// Browser is what the report says was driven, for the header line.
	Browser string
	// Sleep is the rate-limit backoff; nil means time.Sleep.
	Sleep func(time.Duration)
}

// Status is the outcome of one run.
type Status string

const (
	StatusPass  Status = "pass"
	StatusFail  Status = "fail"
	StatusError Status = "error"   // provider, engine or recording failure
	StatusSkip  Status = "skipped" // no key, no recording, recording drift
)

// Metrics is what one run measured.
type Metrics struct {
	Steps           int              `json:"steps"`
	GenerativeCalls int              `json:"generative_calls"`
	DecisionCalls   int              `json:"decision_calls"`
	MoveCalls       int              `json:"move_calls"`
	PromptTokens    int              `json:"prompt_tokens"`
	OutputTokens    int              `json:"output_tokens"`
	Cost            provider.NanoUSD `json:"cost_nanousd,omitempty"`
	Took            time.Duration    `json:"took_ns"`
	MoveTook        time.Duration    `json:"move_took_ns"`
	// Rungs is the ladder histogram (docs/browser.md, M11): index 2 a grounded move, 3 an
	// element the decision model bound, 5 vision, 6 person, 7 failure. The
	// baseline has no ladder and leaves it empty.
	Rungs [8]int `json:"rungs"`
}

// Calls is the two model-call counts together, what the matrix prints.
func (m Metrics) Calls() int { return m.GenerativeCalls + m.DecisionCalls }

// RunResult is one model × task × attempt.
type RunResult struct {
	Model   string    `json:"model"`
	Task    string    `json:"task"`
	Run     int       `json:"run"`
	Status  Status    `json:"status"`
	Outcome string    `json:"outcome,omitempty"`
	Expect  string    `json:"expect"`
	Verdict Judgement `json:"verdict"`
	Metrics Metrics   `json:"metrics"`
	Error   string    `json:"error,omitempty"`
	Blocked []string  `json:"blocked,omitempty"`
	Risks   []string  `json:"approvals,omitempty"`
}

// Run drives every target over every task. A failed verdict is a result, never
// an error; err is reserved for what stops the whole evaluation.
func Run(ctx context.Context, o Options) (Report, error) {
	switch {
	case len(o.Targets) == 0:
		return Report{}, errors.New("eval: no models selected")
	case len(o.Tasks) == 0:
		return Report{}, errors.New("eval: no tasks selected")
	case o.Sidecar == nil:
		return Report{}, errors.New("eval: no browser engine")
	case o.StartURL == nil:
		return Report{}, errors.New("eval: no start URL builder")
	}
	if o.Mode == record.ModeReplay {
		for _, task := range o.Tasks {
			if task.Real != nil {
				return Report{}, fmt.Errorf("eval: --real cannot replay %q: page content is live", task.Name())
			}
		}
	}
	if o.Runs <= 0 {
		o.Runs = 1
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}

	var results []RunResult
	for _, t := range o.Targets {
		for _, task := range o.Tasks {
			for n := 1; n <= o.Runs; n++ {
				if err := ctx.Err(); err != nil {
					return Report{}, err
				}
				results = append(results, runWithRetries(ctx, o, t, task, n))
			}
		}
	}
	return Report{
		Mode:    string(o.Mode),
		When:    time.Now().UTC(),
		Browser: o.Browser,
		Results: results,
		Models:  summarise(o.Targets, results),
		Tasks:   taskSummaries(o.Tasks, results),
	}, nil
}

// runWithRetries retries a rate-limited run a few times before skipping it —
// the browser counterpart of harness.runWithRetries, same policy.
func runWithRetries(ctx context.Context, o Options, t Target, task Task, n int) RunResult {
	for attempt := 0; ; attempt++ {
		res, wait, limited := runOne(ctx, o, t, task, n)
		if !limited {
			return res
		}
		if attempt >= maxRateLimitRetries || ctx.Err() != nil {
			res.Status, res.Error = StatusSkip, "skipped: rate limited"
			return res
		}
		if wait <= 0 {
			wait = rateLimitBackoff
		}
		o.Sleep(wait)
	}
}

// runOne carries out one attempt and scores it. It reports a rate-limit
// failure separately so the caller can retry it (harness.runOnce's shape).
func runOne(ctx context.Context, o Options, t Target, task Task, n int) (RunResult, time.Duration, bool) {
	res := RunResult{Model: t.Label, Task: task.Name(), Run: n, Expect: task.expect()}
	if t.Skip != "" {
		res.Status, res.Error = StatusSkip, t.Skip
		return res, 0, false
	}
	id := runID(t.Label, task.Name(), n)
	if task.Real == nil {
		sites.Reset(id)
	}

	chat, decide, plan, save, err := open(o, t, task.Name(), n)
	if err != nil {
		return fail(res, err)
	}
	defer func() {
		// The browser keeps a tab, a history and cookies; the next task starts
		// without them. Closing also stops a run that hung on a live view.
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_ = o.Sidecar.Close(closeCtx, Agent)
	}()

	h := &humans{approve: task.Approve}
	events := &sink{}
	task2 := browser.Task{
		Agent:  Agent,
		Intent: task.intent(),
		URL:    task.startURL(o.StartURL, id),
		Allow:  task.allow(o.Host),
	}

	runCtx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()

	var out browser.Result
	if t.Baseline {
		out, err = runBaseline(runCtx, baselineOptions{
			Sidecar: o.Sidecar,
			Chat:    chat,
			Model:   t.ChatSpec(),
			Task:    task2,
		})
	} else {
		out, err = runExecutor(runCtx, o, t, chat, decide, plan, events, h, task2)
	}
	if serr := save(); serr != nil && err == nil {
		err = serr
	}
	if err != nil {
		return fail(res, err)
	}

	res.Outcome = string(out.Outcome)
	res.Metrics = metricsOf(out.Stats)
	res.Blocked, res.Risks = events.whys(), riskWords(h.risks())
	res.Verdict = judge(task, id, observed{
		Outcome: out.Outcome, Answer: out.Answer,
		Blocked: res.Blocked, Approvals: h.risks(),
	})
	res.Status = StatusFail
	if res.Verdict.Pass {
		res.Status = StatusPass
	}
	return res, 0, false
}

// fail classifies a run's terminal error: a rate limit is retried by the
// caller, a "model unavailable" bad request is skipped outright (free-tier
// models routinely vanish without warning), and anything else is finish's
// concern.
func fail(res RunResult, err error) (RunResult, time.Duration, bool) {
	var pe *provider.Error
	if errors.As(err, &pe) {
		switch {
		case pe.Kind == provider.ErrRateLimit:
			return res, pe.RetryAfter, true
		case pe.Kind == provider.ErrBadRequest && unavailable(pe.Message):
			res.Status, res.Error = StatusSkip, "skipped: model unavailable"
			return res, 0, false
		}
	}
	return finish(res, err), 0, false
}

// unavailable reports whether a bad-request message says the model itself
// cannot be used, as opposed to a malformed request.
func unavailable(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "unavailable")
}

// runExecutor is the real thing: the ladder of docs/browser.md.
func runExecutor(ctx context.Context, o Options, t Target, chat provider.Provider, decide provider.Decider, plan provider.Provider, events event.Sink, h browser.Humans, task browser.Task) (browser.Result, error) {
	opts := browser.Options{
		Sidecar: o.Sidecar,
		Humans:  h,
		Chat:    chat, ChatModel: t.ChatSpec(),
		Events: events, Logger: o.Logger,
		Deadline: o.Timeout,
	}
	if decide != nil {
		opts.Decider, opts.DecideModel = decide, t.DecideSpec()
	}
	if plan != nil {
		opts.Plan, opts.PlanModel = plan, t.PlanSpec()
	}
	ex, err := browser.NewExecutor(opts)
	if err != nil {
		return browser.Result{}, err
	}
	return ex.Run(ctx, task)
}

// finish turns a Go error into the run's status: a recording that no longer
// matches is a skip, not a failure (see the README: replay of a browser eval
// is best-effort, because the page the digests were taken over is live).
func finish(res RunResult, err error) RunResult {
	switch {
	case drifted(err):
		res.Status, res.Error = StatusSkip, "recording drift"
	case errors.Is(err, os.ErrNotExist):
		res.Status, res.Error = StatusSkip, "no recording (run with --mode record)"
	default:
		res.Status, res.Error = StatusError, err.Error()
	}
	return res
}

// drifted reports whether err is a digest mismatch of either recording kind.
func drifted(err error) bool {
	var chat *record.MismatchError
	var dec *record.DecisionMismatchError
	return errors.As(err, &chat) || errors.As(err, &dec)
}

// open builds the recording wrappers for one run. The chat and the decision
// calls get one file each, side by side: the record package keeps a run of
// each kind in its own shape, and mixing them in one file would make a
// mismatch unreadable.
func open(o Options, t Target, task string, n int) (provider.Provider, provider.Decider, provider.Provider, func() error, error) {
	dir := filepath.Join(o.RecordDir, record.Slug(t.Label), record.Slug(task))
	var live provider.Provider
	if o.Mode != record.ModeReplay {
		p, err := t.NewChat()
		if err != nil {
			return nil, nil, nil, nil, err
		}
		live = p
	}
	chat, err := record.Open(o.Mode, live, filepath.Join(dir, fmt.Sprintf("%d.json", n)),
		record.Run{Model: t.Label, Scenario: task, Run: n})
	if err != nil {
		return nil, nil, nil, nil, err
	}

	var dec *record.Decider
	if t.NewDecider != nil {
		var liveDec provider.Decider
		if o.Mode != record.ModeReplay {
			d, err := t.NewDecider()
			if err != nil {
				return nil, nil, nil, nil, err
			}
			liveDec = d
		}
		dec, err = record.OpenDecider(o.Mode, liveDec, filepath.Join(dir, fmt.Sprintf("%d.decide.json", n)),
			record.DecisionRun{Model: t.Label, Case: task})
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}

	// The plan role (M10) records like a second chat provider, in its own
	// file: it is a chat-shaped call, not the decide role's JSON shape.
	var plan *record.Provider
	if t.NewPlan != nil {
		var livePlan provider.Provider
		if o.Mode != record.ModeReplay {
			p, err := t.NewPlan()
			if err != nil {
				return nil, nil, nil, nil, err
			}
			livePlan = p
		}
		plan, err = record.Open(o.Mode, livePlan, filepath.Join(dir, fmt.Sprintf("%d.plan.json", n)),
			record.Run{Model: t.Label, Scenario: task, Run: n})
		if err != nil {
			return nil, nil, nil, nil, err
		}
	}

	save := func() error {
		errs := []error{chat.Save()}
		if dec != nil {
			errs = append(errs, dec.Save())
		}
		if plan != nil {
			errs = append(errs, plan.Save())
		}
		return errors.Join(errs...)
	}

	var decOut provider.Decider
	if dec != nil {
		decOut = dec
	}
	var planOut provider.Provider
	if plan != nil {
		planOut = plan
	}
	return chat, decOut, planOut, save, nil
}

// runID scopes one attempt's server-side state. Every link and form action of
// a synthetic site carries it, so two runs never share data.
func runID(label, task string, n int) string {
	return fmt.Sprintf("%s-%s-%d", record.Slug(label), task, n)
}

func metricsOf(s browser.Stats) Metrics {
	m := Metrics{
		Steps: s.Steps, GenerativeCalls: s.GenerativeCall, DecisionCalls: s.DecisionCalls,
		MoveCalls: s.MoveCalls, PromptTokens: s.Usage.PromptTokens, OutputTokens: s.Usage.CompletionTokens,
		Took: s.Took, MoveTook: s.MoveTook, Rungs: s.Rungs,
	}
	if s.Usage.Cost != nil {
		m.Cost = *s.Usage.Cost // unpriced calls are reported as free here
	}
	return m
}

// humans is the scripted approval framework: one task may have its approval
// granted (the task list says so), and nobody is ever watching, so a takeover
// is always refused.
type humans struct {
	approve bool
	mu      sync.Mutex
	asked   []browser.RiskKind
}

var _ browser.Humans = (*humans)(nil)

func (h *humans) Approve(_ context.Context, _ browser.Task, kind browser.RiskKind, _ string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked = append(h.asked, kind)
	return h.approve, nil
}

// WaitReleased: nobody takes over in an eval, so the lock is never held.
func (h *humans) WaitReleased(context.Context, browser.Task) error { return nil }

func (h *humans) Takeover(context.Context, browser.Task, browser.Blocker) (bool, error) {
	return false, nil
}

func (h *humans) risks() []browser.RiskKind {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]browser.RiskKind(nil), h.asked...)
}

// sink collects the browser.blocked events of one run. Nothing else is kept:
// the trace carries closed-set words and numbers only, and the verdict needs
// exactly one of them.
type sink struct {
	mu   sync.Mutex
	whyS []string
}

var _ event.Sink = (*sink)(nil)

func (s *sink) Emit(_ context.Context, e event.Event) {
	if e.Type != event.BrowserBlocked {
		return
	}
	var a struct {
		Why string `json:"why"`
	}
	if err := json.Unmarshal(e.Attrs, &a); err != nil || a.Why == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.whyS = append(s.whyS, a.Why)
}

func (s *sink) whys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.whyS...)
}
