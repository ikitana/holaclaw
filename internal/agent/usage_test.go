package agent

import (
	"context"
	"errors"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/taint"
)

func nu(v provider.NanoUSD) *provider.NanoUSD { return &v }

// events is a copy of everything the rig emitted so far.
func (g *rig) events() []event.Event {
	g.ev.mu.Lock()
	defer g.ev.mu.Unlock()
	return append([]event.Event(nil), g.ev.evs...)
}

// Every model.response carries its call's cost, run.finished the run's, and
// summing the trace under the accounting rule counts each call once.
func TestModelResponseAndRunCarryCost(t *testing.T) {
	g := newRig(t,
		callStep(call("c1", "echo", `{}`)),
		textStep("done"),
	).withTools(spec("echo", objectSchema))
	g.prov.steps[0].resp.Usage = provider.Usage{PromptTokens: 100, CompletionTokens: 10, Cost: nu(10000000)}
	g.prov.steps[1].resp.Usage = provider.Usage{PromptTokens: 200, CompletionTokens: 20, Cost: nu(20000000)}

	res, err := g.turn(context.Background())
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	if res.Usage.PromptTokens != 300 || res.Usage.CompletionTokens != 30 || *res.Usage.Cost != 30000000 {
		t.Fatalf("turn usage %+v", res.Usage)
	}
	var costs []any
	var total event.Usage
	for _, e := range g.events() {
		if e.Type == event.ModelResponse {
			costs = append(costs, g.ev.attrs(t, e)["cost_nanousd"])
		}
		total.Add(event.UsageOfEvent(e))
	}
	if len(costs) != 2 || costs[0] != float64(10000000) || costs[1] != float64(20000000) {
		t.Errorf("model.response costs = %v", costs)
	}
	fin, _ := g.ev.first(event.RunFinished)
	if a := g.ev.attrs(t, fin); a["cost_nanousd"] != float64(30000000) || a["unpriced_calls"] != float64(0) {
		t.Errorf("run.finished = %v", a)
	}
	if total.PromptTokens != 300 || total.CompletionTokens != 30 || *total.Cost != 30000000 || total.UnpricedCalls != 0 {
		t.Errorf("the trace sums to %+v, want each call once", total)
	}
}

// An upstream that reports no price leaves no cost: the call is unpriced,
// not free, and run.finished counts it as such.
func TestModelResponseWithoutAPriceIsUnpriced(t *testing.T) {
	g := newRig(t, textStep("hi"))
	if _, err := g.turn(context.Background()); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	e, _ := g.ev.first(event.ModelResponse)
	if a := g.ev.attrs(t, e); a["prompt_tokens"] != float64(0) || a["cost_nanousd"] != nil {
		t.Errorf("model.response = %v, want tokens and no cost", a)
	}
	if u := event.UsageOfEvent(e); u.UnpricedCalls != 1 || u.Cost != nil {
		t.Errorf("counted as %+v, want one unpriced call", u)
	}
	fin, _ := g.ev.first(event.RunFinished)
	if a := g.ev.attrs(t, fin); a["cost_nanousd"] != nil || a["unpriced_calls"] != float64(1) {
		t.Errorf("run.finished = %v", a)
	}
}

// A stream that drops after answering was still a call: its usage is on the
// model.error that reports it, counted once, and in the run's total. The
// retry that follows is a call of its own.
func TestDroppedStreamUsageIsCounted(t *testing.T) {
	dropped := step{resp: &provider.Response{Finish: provider.FinishDropped,
		Usage: provider.Usage{PromptTokens: 50, Cost: nu(5000000)}}}
	ok := textStep("hi")
	ok.resp.Usage = provider.Usage{PromptTokens: 60, CompletionTokens: 5, Cost: nu(6000000)}
	g := newRig(t, dropped, ok)
	if _, err := g.turn(context.Background()); err != nil {
		t.Fatalf("Turn: %v", err)
	}
	e, _ := g.ev.first(event.ModelError)
	if a := g.ev.attrs(t, e); a["kind"] != "dropped" || a["prompt_tokens"] != float64(50) || a["cost_nanousd"] != float64(5000000) {
		t.Errorf("model.error = %v", a)
	}
	var total event.Usage
	for _, e := range g.events() {
		total.Add(event.UsageOfEvent(e))
	}
	if total.PromptTokens != 110 || *total.Cost != 11000000 {
		t.Errorf("the trace sums to %+v", total)
	}
	fin, _ := g.ev.first(event.RunFinished)
	if a := g.ev.attrs(t, fin); a["prompt_tokens"] != float64(110) {
		t.Errorf("run.finished = %v", a)
	}
}

// usageScorer reports what its questions cost, or fails.
type usageScorer struct {
	v   taint.Verdict
	err error
}

func (s usageScorer) Score(context.Context, string) (taint.Verdict, error) { return s.v, s.err }

// The taint scorer's questions are model calls too: their usage is on the
// tool.result they scored — after a failure too, for what was answered —
// and absent when nothing was answered or there is no scorer.
func TestToolResultCarriesTheTaintScorersUsage(t *testing.T) {
	cases := []struct {
		name   string
		scorer taint.Scorer
		want   map[string]any // nil: no usage keys at all
	}{
		{"scored", usageScorer{v: taint.Verdict{Score: 0.1, Windows: 2,
			Usage: event.Usage{PromptTokens: 80, CompletionTokens: 2, Cost: nu(400000)}}},
			map[string]any{"prompt_tokens": float64(80), "completion_tokens": float64(2), "cost_nanousd": float64(400000), "unpriced_calls": float64(0)}},
		{"unpriced", usageScorer{v: taint.Verdict{Score: 0.1, Windows: 1,
			Usage: event.Usage{PromptTokens: 40, CompletionTokens: 1, UnpricedCalls: 1}}},
			map[string]any{"prompt_tokens": float64(40), "completion_tokens": float64(1), "unpriced_calls": float64(1)}},
		{"failed after an answer", usageScorer{v: taint.Verdict{Usage: event.Usage{PromptTokens: 40, Cost: nu(200000)}},
			err: errors.New("down")},
			map[string]any{"prompt_tokens": float64(40), "completion_tokens": float64(0), "cost_nanousd": float64(200000), "unpriced_calls": float64(0)}},
		{"failed", usageScorer{err: errors.New("down")}, nil},
		{"no scorer", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := fetchRig(t, "The price is $80")
			if c.scorer != nil {
				g.r.Taint = c.scorer
			}
			if _, err := g.turn(context.Background()); err != nil {
				t.Fatalf("Turn: %v", err)
			}
			e, _ := g.ev.first(event.ToolResult)
			attrs := g.ev.attrs(t, e)
			for _, k := range []string{"prompt_tokens", "completion_tokens", "cost_nanousd", "cost", "unpriced_calls"} {
				got, ok := attrs[k]
				want, wantOK := c.want[k]
				if ok != wantOK || got != want {
					t.Errorf("%s = %v (present %v), want %v (present %v)", k, got, ok, want, wantOK)
				}
			}
		})
	}
}

// usageStep is a summary step whose call reports usage.
func usageStep(s step, u provider.Usage) step {
	if s.resp != nil {
		s.resp.Usage = u
	}
	return s
}

// context.compacted carries the summarize call's usage whether the summary was
// committed or thrown away: an ineffective or empty summary was paid for all
// the same. A call that never answered carries no usage at all.
func TestCompactedCarriesTheSummarizeUsage(t *testing.T) {
	u := provider.Usage{PromptTokens: 3000, CompletionTokens: 200, Cost: nu(3000000)}
	cases := []struct {
		name    string
		step    step
		outcome string
		want    *provider.Usage
	}{
		{"committed", usageStep(cpSummaryStep("Goal\n- ship it"), u), "committed", &u},
		{"ineffective", usageStep(cpSummaryStep(cpFiller(4000)), u), "ineffective", &u},
		{"empty", usageStep(cpSummaryStep("   "), u), "failed", &u},
		{"model error", step{err: errors.New("boom")}, "failed", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newCompactRig(t, c.step)
			g.add(cpChat(30, 150)...)
			_, _ = g.c.Compact(context.Background(), g.in)
			e, ok := g.ev.first(event.ContextCompacted)
			if !ok {
				t.Fatal("no context.compacted")
			}
			a := g.ev.attrs(t, e)
			if a["outcome"] != c.outcome {
				t.Fatalf("outcome = %v, want %s", a["outcome"], c.outcome)
			}
			got := event.UsageOfEvent(e)
			if c.want == nil {
				if _, ok := a["prompt_tokens"]; ok || !got.IsZero() {
					t.Errorf("an unanswered call carries usage: %v", a)
				}
				return
			}
			if a["prompt_tokens"] != float64(c.want.PromptTokens) || a["completion_tokens"] != float64(c.want.CompletionTokens) ||
				a["cost_nanousd"] != float64(*c.want.Cost) {
				t.Errorf("attrs = %v, want %+v", a, *c.want)
			}
			if got.PromptTokens != c.want.PromptTokens || got.Cost == nil || *got.Cost != *c.want.Cost || got.UnpricedCalls != 0 {
				t.Errorf("counted as %+v", got)
			}
		})
	}
}
