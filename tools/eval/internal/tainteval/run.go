package tainteval

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"holaclaw/internal/agent"
	"holaclaw/internal/provider"
	"holaclaw/internal/taint"
	"holaclaw/tools/eval/internal/record"
)

// Target is one decision model to evaluate. The scorer has no chat backend:
// the question is a noul, and only a decision model answers one.
type Target struct {
	Name string
	// Spec builds the provider.Model for this target.
	Spec func() provider.Model
	// NewDecider opens a live decisions transport; unused in replay mode.
	NewDecider func() (provider.Decider, error)
	// Skip, when set, explains why this target cannot run.
	Skip string
	// Reference marks the scorer the project measures itself against: replay
	// fails when it calls an injected snippet clean.
	Reference bool
}

// Options configures one taint evaluation.
type Options struct {
	Targets   []Target
	Cases     []Case
	Mode      record.Mode
	RecordDir string
	// Timeout bounds one window; 0 leaves the scorer's own default.
	Timeout     time.Duration
	Concurrency int
	// Threshold is the score at or above which content counts as steering;
	// 0 means agent.DefaultTaintThreshold, which is what the loop uses.
	Threshold float64
}

// Status is the outcome of one case.
type Status string

const (
	StatusPass  Status = "pass"
	StatusFail  Status = "fail"
	StatusError Status = "error" // provider, timeout or recording failure
	StatusSkip  Status = "skipped"
)

// CaseResult is one model × case.
type CaseResult struct {
	Model    string           `json:"model"`
	Case     string           `json:"case"`
	Injected bool             `json:"injected"`
	Expect   string           `json:"expect"`
	Got      string           `json:"got,omitempty"`
	Score    float64          `json:"score"`
	Windows  int              `json:"windows,omitempty"`
	Cost     provider.NanoUSD `json:"cost_nanousd,omitempty"`
	Status   Status           `json:"status"`
	Error    string           `json:"error,omitempty"`
	Latency  time.Duration    `json:"latency_ns"`
}

// FalseClean is the failure that matters: steering content scored below the
// threshold, so the turn never arms.
func (r CaseResult) FalseClean() bool {
	return r.Injected && r.Status == StatusFail
}

// FalseInjected is ordinary content that armed the taint — the cost of a
// threshold set too low, paid in approval prompts.
func (r CaseResult) FalseInjected() bool {
	return !r.Injected && r.Status == StatusFail
}

// Summary aggregates one model's cases.
type Summary struct {
	Model     string `json:"model"`
	Reference bool   `json:"reference,omitempty"`
	Cases     int    `json:"cases"`
	Scored    int    `json:"scored"`
	Correct   int    `json:"correct"`
	Injected  int    `json:"injected"`
	Clean     int    `json:"clean"`

	FalseClean        int     `json:"false_clean"`
	FalseCleanRate    float64 `json:"false_clean_rate"`
	FalseInjected     int     `json:"false_injected"`
	FalseInjectedRate float64 `json:"false_injected_rate"`
	Accuracy          float64 `json:"accuracy"`
	Errors            int     `json:"errors"`
	Skipped           int     `json:"skipped"`

	// The score ranges are what a threshold is chosen from: the two intervals
	// should not overlap.
	CleanMin    float64 `json:"clean_min"`
	CleanMax    float64 `json:"clean_max"`
	InjectedMin float64 `json:"injected_min"`
	InjectedMax float64 `json:"injected_max"`

	// P50 and P95 are measured only in live and record mode: a replayed call
	// touches no network.
	P50       time.Duration    `json:"p50_ns"`
	P95       time.Duration    `json:"p95_ns"`
	TotalCost provider.NanoUSD `json:"total_cost_nanousd"`
	Skip      string           `json:"skip,omitempty"`
}

// Report is everything one taint evaluation produced.
type Report struct {
	Mode      string       `json:"mode"`
	When      time.Time    `json:"when"`
	Threshold float64      `json:"threshold"`
	Results   []CaseResult `json:"results"`
	Models    []Summary    `json:"models"`
}

// ReferenceFailed reports whether the reference scorer called an injected
// snippet clean. That is the CI gate in replay mode.
func (r Report) ReferenceFailed() bool {
	for _, m := range r.Models {
		if m.Reference && m.FalseClean > 0 {
			return true
		}
	}
	return false
}

// Errors counts cases that produced no score at all.
func (r Report) Errors() int {
	n := 0
	for _, m := range r.Models {
		n += m.Errors
	}
	return n
}

// TotalCost sums every case's cost.
func (r Report) TotalCost() provider.NanoUSD {
	var sum provider.NanoUSD
	for _, res := range r.Results {
		sum += res.Cost
	}
	return sum
}

// Run scores every case with every target and summarises the outcome. A wrong
// score is a result, never an error; err is reserved for problems that stop
// the whole evaluation.
func Run(ctx context.Context, o Options) (Report, error) {
	if len(o.Targets) == 0 {
		return Report{}, errors.New("eval: no taint models selected")
	}
	if len(o.Cases) == 0 {
		return Report{}, errors.New("eval: no taint cases selected")
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 1
	}
	if o.Threshold <= 0 {
		o.Threshold = agent.DefaultTaintThreshold
	}

	type job struct {
		t Target
		c Case
	}
	var jobs []job
	for _, t := range o.Targets {
		for _, c := range o.Cases {
			jobs = append(jobs, job{t: t, c: c})
		}
	}

	results := make([]CaseResult, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, o.Concurrency)
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = runCase(ctx, o, j.t, j.c)
		}()
	}
	wg.Wait()

	return Report{
		Mode:      string(o.Mode),
		When:      time.Now().UTC(),
		Threshold: o.Threshold,
		Results:   results,
		Models:    summarise(o.Targets, results, o.Mode),
	}, nil
}

// runCase scores one snippet. Each case gets its own scorer and its own
// recording, so a replay serves exactly the calls that case made.
func runCase(ctx context.Context, o Options, t Target, c Case) CaseResult {
	res := CaseResult{Model: t.Name, Case: c.Name, Injected: c.Injected(), Expect: c.Expect}
	if t.Skip != "" {
		res.Status, res.Error = StatusSkip, t.Skip
		return res
	}
	fail := func(err error) CaseResult {
		res.Status, res.Error = StatusError, err.Error()
		return res
	}

	var inner provider.Decider
	if o.Mode != record.ModeReplay {
		d, err := t.NewDecider()
		if err != nil {
			return fail(err)
		}
		inner = d
	}
	path := record.Path(o.RecordDir, t.Name, c.Name, 1)
	rec, err := record.OpenDecider(o.Mode, inner, path, record.DecisionRun{Model: t.Name, Case: c.Name})
	if err != nil {
		return fail(err)
	}
	s := taint.New(taint.Options{Model: t.Spec(), Decider: rec, Timeout: o.Timeout})

	start := time.Now()
	v, err := s.Score(ctx, c.Text)
	res.Latency = time.Since(start)
	if err != nil {
		if serr := rec.Save(); serr != nil {
			err = errors.Join(err, serr)
		}
		return fail(err)
	}
	if err := rec.Save(); err != nil {
		return fail(err)
	}

	res.Score, res.Windows = v.Score, v.Windows
	if v.Usage.Cost != nil {
		res.Cost = *v.Usage.Cost // unpriced questions are reported as free here
	}
	res.Got = ExpectClean
	if v.Score >= o.Threshold {
		res.Got = ExpectInjected
	}
	res.Status = StatusFail
	if res.Got == c.Expect {
		res.Status = StatusPass
	}
	return res
}

// summarise computes the per-model metrics, targets in their configured
// order. Latency is only collected outside replay mode (see Summary.P50).
func summarise(targets []Target, results []CaseResult, mode record.Mode) []Summary {
	index := map[string]int{}
	out := make([]Summary, 0, len(targets))
	for _, t := range targets {
		index[t.Name] = len(out)
		out = append(out, Summary{Model: t.Name, Reference: t.Reference, Skip: t.Skip,
			CleanMin: -1, InjectedMin: -1})
	}
	lat := make([][]time.Duration, len(out))

	for _, r := range results {
		i, ok := index[r.Model]
		if !ok {
			continue
		}
		s := &out[i]
		s.Cases++
		if r.Injected {
			s.Injected++
		} else {
			s.Clean++
		}
		switch r.Status {
		case StatusSkip:
			s.Skipped++
			continue
		case StatusError:
			s.Errors++
			continue
		case StatusPass:
			s.Correct++
		case StatusFail:
			if r.FalseClean() {
				s.FalseClean++
			}
			if r.FalseInjected() {
				s.FalseInjected++
			}
		}
		s.Scored++
		s.TotalCost += r.Cost
		if r.Injected {
			s.InjectedMin, s.InjectedMax = span(s.InjectedMin, s.InjectedMax, r.Score)
		} else {
			s.CleanMin, s.CleanMax = span(s.CleanMin, s.CleanMax, r.Score)
		}
		if mode != record.ModeReplay {
			lat[i] = append(lat[i], r.Latency)
		}
	}

	for i := range out {
		s := &out[i]
		s.Accuracy = ratio(s.Correct, s.Scored)
		s.FalseCleanRate = ratio(s.FalseClean, scoredOf(results, s.Model, true))
		s.FalseInjectedRate = ratio(s.FalseInjected, scoredOf(results, s.Model, false))
		s.P50, s.P95 = percentiles(lat[i])
		if s.CleanMin < 0 {
			s.CleanMin = 0
		}
		if s.InjectedMin < 0 {
			s.InjectedMin = 0
		}
	}
	return out
}

// span widens [min, max] by score; min < 0 means "no score yet".
func span(min, max, score float64) (float64, float64) {
	if min < 0 || score < min {
		min = score
	}
	if score > max {
		max = score
	}
	return min, max
}

// scoredOf counts the injected (or clean) cases of a model that produced a
// score: the denominator of the two rates.
func scoredOf(results []CaseResult, model string, injected bool) int {
	n := 0
	for _, r := range results {
		if r.Model == model && r.Injected == injected && (r.Status == StatusPass || r.Status == StatusFail) {
			n++
		}
	}
	return n
}

func ratio(n, of int) float64 {
	if of == 0 {
		return 0
	}
	return float64(n) / float64(of)
}

// percentiles returns the p50 and p95 of d (nearest-rank, so a small sample
// reports a real measurement rather than an interpolation).
func percentiles(d []time.Duration) (p50, p95 time.Duration) {
	if len(d) == 0 {
		return 0, 0
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[rank(len(s), 0.50)], s[rank(len(s), 0.95)]
}

func rank(n int, q float64) int {
	i := int(q*float64(n)+0.999999) - 1
	if i < 0 {
		i = 0
	}
	if i >= n {
		i = n - 1
	}
	return i
}
