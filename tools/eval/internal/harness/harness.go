// Package harness runs eval scenarios against the real agent library: the
// same agent.Runner and context builder the daemon uses, with mocked tools and
// either a live model or a recording (ARCHITECTURE.md §2, tools/eval/README.md).
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"holaclaw/internal/agent"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
	"holaclaw/tools/eval/internal/record"
	"holaclaw/tools/eval/internal/scenario"
)

// Retry policy for free tiers, which rate-limit aggressively.
const (
	maxRateLimitRetries = 3
	rateLimitBackoff    = 10 * time.Second
)

// Target is one model to evaluate.
type Target struct {
	Name string
	// Spec builds the provider.Model for a scenario window.
	Spec func(window int) provider.Model
	// New opens a live provider; nil (and unused) in replay mode.
	New func() (provider.Provider, error)
	// Skip, when set, explains why this target cannot run.
	Skip string
}

// Options configures one evaluation.
type Options struct {
	Targets     []Target
	Scenarios   []scenario.Scenario
	Runs        int
	Mode        record.Mode
	RecordDir   string
	Timeout     time.Duration
	Concurrency int
	// Sleep is the rate-limit backoff; nil means time.Sleep.
	Sleep func(time.Duration)
}

// Status is the outcome of one run.
type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
	StatusSkip Status = "skipped"
)

// TurnResult is the outcome of one turn of one run.
type TurnResult struct {
	Turn         int           `json:"turn"`
	User         string        `json:"user"`
	Reply        string        `json:"reply"`
	Pass         bool          `json:"pass"`
	Failures     []string      `json:"failures,omitempty"`
	Steps        int           `json:"steps"`
	PromptTokens int           `json:"prompt_tokens"`
	Usage        event.Usage   `json:"usage"`
	Stop         string        `json:"stop"`
	Latency      time.Duration `json:"latency_ns"`
	Calls        []string      `json:"calls,omitempty"`
}

// RunResult is one (model × scenario × run) triple.
type RunResult struct {
	Model    string       `json:"model"`
	Scenario string       `json:"scenario"`
	Run      int          `json:"run"`
	Status   Status       `json:"status"`
	Reason   string       `json:"reason,omitempty"` // skip reason or harness error
	Turns    []TurnResult `json:"turns,omitempty"`
	Steps    int          `json:"steps"`
	// PromptTokens is the largest per-turn prompt of the run.
	PromptTokens int           `json:"prompt_tokens"`
	Usage        event.Usage   `json:"usage"`
	Latency      time.Duration `json:"latency_ns"`
	Repairs      int           `json:"repairs"`
	Rejects      int           `json:"rejects"`
	Leaks        int           `json:"leaks"`
	Loops        int           `json:"loops"`
}

// Report is everything one eval produced.
type Report struct {
	Mode string      `json:"mode"`
	When time.Time   `json:"when"`
	Runs []RunResult `json:"runs"`
}

// Failed counts runs that failed (skipped runs do not count).
func (r Report) Failed() int {
	n := 0
	for _, run := range r.Runs {
		if run.Status == StatusFail {
			n++
		}
	}
	return n
}

// Run evaluates every target × scenario × run and returns the report. A
// failing run is reported, never returned as an error; err is reserved for
// problems that stop the whole evaluation.
func Run(ctx context.Context, o Options) (Report, error) {
	if len(o.Targets) == 0 {
		return Report{}, errors.New("eval: no models selected")
	}
	if len(o.Scenarios) == 0 {
		return Report{}, errors.New("eval: no scenarios selected")
	}
	if o.Runs <= 0 {
		o.Runs = 1
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 1
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}

	type job struct {
		t   Target
		s   scenario.Scenario
		run int
	}
	var jobs []job
	for _, t := range o.Targets {
		for _, s := range o.Scenarios {
			for i := 1; i <= o.Runs; i++ {
				jobs = append(jobs, job{t: t, s: s, run: i})
			}
		}
	}

	results := make([]RunResult, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, o.Concurrency)
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = runWithRetries(ctx, o, j.t, j.s, j.run)
		}()
	}
	wg.Wait()
	return Report{Mode: string(o.Mode), When: time.Now().UTC(), Runs: results}, nil
}

// runWithRetries retries a rate-limited run a few times before skipping it.
func runWithRetries(ctx context.Context, o Options, t Target, s scenario.Scenario, run int) RunResult {
	for attempt := 0; ; attempt++ {
		res, wait, limited := runOnce(ctx, o, t, s, run)
		if !limited {
			return res
		}
		if attempt >= maxRateLimitRetries || ctx.Err() != nil {
			res.Status = StatusSkip
			res.Reason = "skipped: rate limited"
			return res
		}
		if wait <= 0 {
			wait = rateLimitBackoff
		}
		o.Sleep(wait)
	}
}

// runOnce plays one scenario end to end. It reports a rate-limit failure
// separately so the caller can retry it.
func runOnce(ctx context.Context, o Options, t Target, s scenario.Scenario, run int) (RunResult, time.Duration, bool) {
	res := RunResult{Model: t.Name, Scenario: s.Name, Run: run, Status: StatusPass}
	if t.Skip != "" {
		res.Status, res.Reason = StatusSkip, t.Skip
		return res, 0, false
	}
	fail := func(err error) (RunResult, time.Duration, bool) {
		var pe *provider.Error
		if errors.As(err, &pe) && pe.Kind == provider.ErrRateLimit {
			return res, pe.RetryAfter, true
		}
		res.Status, res.Reason = StatusFail, err.Error()
		return res, 0, false
	}

	var inner provider.Provider
	if o.Mode != record.ModeReplay {
		p, err := t.New()
		if err != nil {
			return fail(err)
		}
		inner = p
	}
	rec, err := record.Open(o.Mode, inner, record.Path(o.RecordDir, t.Name, s.Name, run),
		record.Run{Model: t.Name, Scenario: s.Name, Run: run})
	if err != nil {
		return fail(err)
	}
	exec, err := scenario.NewExecutor(s)
	if err != nil {
		return fail(err)
	}

	sink := &sink{}
	store := &transcript{}
	var skills []agent.SkillEntry
	for _, sk := range s.Skills {
		skills = append(skills, agent.SkillEntry{Name: sk.Name, Description: sk.Description})
	}
	runner := &agent.Runner{
		Def: agent.Definition{
			Name:     "eval",
			Persona:  s.Persona,
			Skills:   skills,
			MaxSteps: s.MaxSteps,
		},
		Model:      t.Spec(s.Window()),
		Provider:   rec,
		Tools:      exec,
		Builder:    agent.NewBuilder(),
		Transcript: store,
		Events:     sink,
	}

	sessionID := event.ID("eval-" + record.Slug(s.Name))
	var history []event.Message
	for i, turn := range s.Turns {
		user := event.Message{
			Schema:    event.SchemaVersion,
			ID:        event.NewIDAt(time.Now()),
			SessionID: sessionID,
			TraceID:   event.ID(fmt.Sprintf("eval-t%d", i+1)),
			Role:      event.RoleUser,
			Text:      turn.User,
			CreatedAt: time.Now().UTC(),
		}
		history = append(history, user)
		exec.Reset()
		sink.reset()

		turnCtx, cancel := withTimeout(ctx, o.Timeout)
		start := time.Now()
		out, err := runner.Turn(turnCtx, agent.TurnInput{
			SessionID: sessionID,
			TraceID:   user.TraceID,
			History:   history,
			Call: tool.Call{
				Agent: "eval", Role: "owner", RunKind: tool.RunInteractive,
				Workspace: "/eval", Origin: event.Origin{Channel: "eval", ChannelType: "cli"},
			},
		})
		latency := time.Since(start)
		cancel()
		if err != nil {
			if serr := rec.Save(); serr != nil {
				err = errors.Join(err, serr)
			}
			return fail(fmt.Errorf("turn %d: %w", i+1, err))
		}
		history = append(history, out.New...)

		tr := TurnResult{
			Turn: i + 1, User: turn.User, Reply: out.Reply.Text,
			Steps: out.Steps, PromptTokens: sink.promptTokens,
			Usage: out.Usage, Stop: string(out.Stop), Latency: latency,
		}
		for _, c := range exec.Calls() {
			tr.Calls = append(tr.Calls, c.Name)
		}
		tr.Failures = Check(turn.Expect, Observed{
			Calls:        exec.Calls(),
			Reply:        out.Reply.Text,
			Steps:        out.Steps,
			PromptTokens: sink.promptTokens,
			Stop:         string(out.Stop),
		})
		tr.Pass = len(tr.Failures) == 0
		if !tr.Pass {
			res.Status = StatusFail
		}
		res.Turns = append(res.Turns, tr)
		res.Steps += out.Steps
		res.Usage.Add(out.Usage)
		res.Latency += latency
		res.Repairs += sink.repairs
		res.Rejects += sink.rejects
		res.Leaks += sink.leaks
		res.Loops += sink.loops
		if sink.promptTokens > res.PromptTokens {
			res.PromptTokens = sink.promptTokens
		}
	}
	if err := rec.Save(); err != nil {
		return fail(err)
	}
	return res, 0, false
}

// transcript is the in-memory agent.Transcript one scenario runs against.
type transcript struct {
	mu   sync.Mutex
	msgs []event.Message
}

func (t *transcript) AppendMessage(_ context.Context, m event.Message, _ bool) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.msgs = append(t.msgs, m)
	return true, nil
}

// sink collects the per-turn metrics from the trace events.
type sink struct {
	mu           sync.Mutex
	promptTokens int // context.built total of the first step
	repairs      int
	rejects      int
	leaks        int
	loops        int
	firstBuilt   bool
}

func (s *sink) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.promptTokens, s.repairs, s.rejects, s.leaks, s.loops = 0, 0, 0, 0, 0
	s.firstBuilt = false
}

// withTimeout bounds one turn; a zero duration means no deadline of our own.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

func (s *sink) Emit(_ context.Context, e event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch e.Type {
	case event.ContextBuilt:
		if s.firstBuilt {
			return
		}
		s.firstBuilt = true
		var a struct {
			Total int `json:"total"`
		}
		if json.Unmarshal(e.Attrs, &a) == nil {
			s.promptTokens = a.Total
		}
	case event.ToolRepaired:
		s.repairs++
	case event.ToolRejected:
		s.rejects++
		var a struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(e.Attrs, &a) == nil && a.Reason == "text_leak" {
			s.leaks++
		}
	case event.RunLoopDetected:
		s.loops++
	}
}
