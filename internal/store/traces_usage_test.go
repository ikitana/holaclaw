package store

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// usageFixture is one trace's model calls as the emitters write them: a chat
// loop of a priced and an unpriced response and the run.finished that
// repeats their sum, a stream that dropped after answering, a browser task
// with a move that is already inside its total, a guard call and a cached
// verdict, a taint score on a tool result, a compaction, a free vision call,
// a transcription, an embedding and an image. Around them: a browser task
// from before the output_tokens rename, single calls written before and after
// they carried unpriced_calls, failures that made no call, an event
// that is not a model call but carries usage-shaped keys, values that are
// not numbers, and an attrs value that is not JSON at all. Costs come in both
// forms: cost_nanousd, and the USD float cost of events written before it.
func usageFixture() []event.Event {
	type ev struct {
		t     event.Type
		attrs string
	}
	evs := []ev{
		{event.RunStarted, `{"model":"m"}`},
		{event.ModelResponse, `{"prompt_tokens":1000,"completion_tokens":100,"cost_nanousd":1000000,"cached_tokens":600,"reasoning_tokens":20}`},
		{event.GuardDecision, `{"score":1,"prompt_tokens":50,"completion_tokens":1,"cost":0.0001}`},
		{event.GuardDecision, `{"score":1}`}, // a cached verdict: no call
		{event.ToolResult, `{"tool":"fetch","prompt_tokens":40,"completion_tokens":2,"cost_nanousd":20000,"unpriced_calls":0}`},
		{event.BrowserMove, `{"n":1,"prompt_tokens":300,"completion_tokens":30,"cost_nanousd":300000,"unpriced_calls":0}`},
		{event.BrowserTaskFinished, `{"prompt_tokens":900,"completion_tokens":90,"cost":0.0009,"reasoning_tokens":5,"unpriced_calls":2}`},
		{event.BrowserTaskFinished, `{"prompt_tokens":10,"output_tokens":5}`},  // before the rename: unpriced
		{event.ModelResponse, `{"prompt_tokens":1200,"completion_tokens":80}`}, // unpriced, from before unpriced_calls
		{event.ModelResponse, `{"prompt_tokens":500,"completion_tokens":50,"unpriced_calls":1}`},
		{event.GuardDecision, `{"score":0,"prompt_tokens":30,"completion_tokens":1,"cost":0.0002,"unpriced_calls":0}`},
		{event.RunFinished, `{"prompt_tokens":2200,"completion_tokens":180,"cost":0.001,"unpriced_calls":1}`},
		{event.ModelError, `{"kind":"timeout"}`}, // no reply: no call
		{event.ModelError, `{"kind":"dropped","prompt_tokens":7,"completion_tokens":0}`},
		{event.ContextCompacted, `{"outcome":"committed","prompt_tokens":3000,"completion_tokens":200,"cost_nanousd":3000000}`},
		{event.MediaDescribed, `{"prompt_tokens":700,"completion_tokens":60,"cost_nanousd":0}`}, // free
		{event.MediaTranscribed, `{"prompt_tokens":14,"completion_tokens":45,"cached_tokens":0}`},
		{event.MediaRefused, `{"reason":"too_large"}`},
		{event.MemoryEmbedded, `{"purpose":"item","prompt_tokens":9,"completion_tokens":0,"cost":0.000001}`},
		{event.ImageGenerated, `{"model":"img","cost":0.04}`},
		{event.PolicyDecision, `{"cost_nanousd":9,"cost":9,"prompt_tokens":9}`}, // not a model call
		{event.ModelResponse, `{"prompt_tokens":"many","cost_nanousd":"x","cost":true,"cached_tokens":"x"}`},
		{event.ModelResponse, `not json`},
	}
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	out := make([]event.Event, 0, len(evs))
	for i, e := range evs {
		at := t0.Add(time.Duration(i) * time.Second)
		out = append(out, event.Event{
			ID: event.NewIDAt(at), SpanID: event.NewIDAt(at), Type: e.t, Time: at,
			Attrs: json.RawMessage(e.attrs),
		})
	}
	return out
}

func nu(v int64) *provider.NanoUSD { n := provider.NanoUSD(v); return &n }
func ip(v int) *int                { return &v }

// wantFixtureUsage is the fixture's total by hand: the counted events only,
// run.finished and browser.move left out, the old browser task's output
// tokens read as completion tokens. The cost covers the priced calls (the
// free one adds a zero); the unpriced ones are counted instead: two inside
// the browser task, the old task, two responses, the dropped stream and the
// transcription.
var wantFixtureUsage = event.Usage{
	PromptTokens:     1000 + 50 + 40 + 900 + 10 + 1200 + 500 + 30 + 7 + 3000 + 700 + 14 + 9,
	CompletionTokens: 100 + 1 + 2 + 90 + 5 + 80 + 50 + 1 + 0 + 200 + 60 + 45 + 0,
	Cost:             nu(1000000 + 100000 + 20000 + 900000 + 200000 + 3000000 + 0 + 1000 + 40000000),
	CachedTokens:     ip(600 + 0),
	ReasoningTokens:  ip(20 + 5),
	UnpricedCalls:    2 + 1 + 1 + 1 + 1 + 1,
}

func sameUsage(a, b event.Usage) bool {
	samePtr := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	sameCost := (a.Cost == nil) == (b.Cost == nil) && (a.Cost == nil || *a.Cost == *b.Cost)
	return a.PromptTokens == b.PromptTokens && a.CompletionTokens == b.CompletionTokens && sameCost &&
		samePtr(a.CachedTokens, b.CachedTokens) && samePtr(a.ReasoningTokens, b.ReasoningTokens) &&
		a.UnpricedCalls == b.UnpricedCalls
}

func showUsage(u event.Usage) string {
	b, _ := json.Marshal(u)
	return string(b)
}

// The store's SQL and the Go helper the browser uses are the same rule: on the
// same events they give the same totals, and both count every model call once.
func TestUsageSQLMatchesGo(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sess := mustSession(t, s, "one")
	other := mustSession(t, s, "two")
	trace := event.NewID()

	var inGo event.Usage
	for _, e := range usageFixture() {
		e.SessionID, e.TraceID = sess.ID, trace
		s.Emit(ctx, e)
		inGo.Add(event.UsageOfEvent(e))
	}
	if !sameUsage(inGo, wantFixtureUsage) {
		t.Fatalf("Go total = %s, want %s", showUsage(inGo), showUsage(wantFixtureUsage))
	}

	// A second trace in the session, so the session total is a sum of traces.
	second := event.NewID()
	emitUsage(t, s, sess.ID, second, event.ModelResponse, `{"prompt_tokens":5,"completion_tokens":1,"cost_nanousd":500000000}`)
	// Another session's calls stay in that session.
	emitUsage(t, s, other.ID, event.NewID(), event.ModelResponse, `{"prompt_tokens":7,"completion_tokens":7,"cost":7}`)

	traces, err := s.ListTraces(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list traces: %v", err)
	}
	got := map[event.ID]event.Usage{}
	for _, tr := range traces {
		got[tr.TraceID] = tr.Usage
	}
	if !sameUsage(got[trace], inGo) {
		t.Errorf("trace usage in SQL = %s, in Go %s", showUsage(got[trace]), showUsage(inGo))
	}
	if want := (event.Usage{PromptTokens: 5, CompletionTokens: 1, Cost: nu(500000000)}); !sameUsage(got[second], want) {
		t.Errorf("second trace usage = %s, want %s", showUsage(got[second]), showUsage(want))
	}

	all, err := s.SessionUsage(ctx)
	if err != nil {
		t.Fatalf("session usage: %v", err)
	}
	wantSess := inGo
	wantSess.Add(event.Usage{PromptTokens: 5, CompletionTokens: 1, Cost: nu(500000000)})
	if !sameUsage(all[sess.ID], wantSess) {
		t.Errorf("session usage = %s, want %s", showUsage(all[sess.ID]), showUsage(wantSess))
	}
	if want := (event.Usage{PromptTokens: 7, CompletionTokens: 7, Cost: nu(7000000000)}); !sameUsage(all[other.ID], want) {
		t.Errorf("other session usage = %s, want %s", showUsage(all[other.ID]), showUsage(want))
	}

	// Narrowed to one session, the others are not read.
	one, err := s.SessionUsage(ctx, other.ID)
	if err != nil {
		t.Fatalf("session usage: %v", err)
	}
	if len(one) != 1 || !sameUsage(one[other.ID], all[other.ID]) {
		t.Errorf("narrowed usage = %+v", one)
	}
}

// Not reported stays not reported through the sum: a trace of unpriced calls
// has no cost (not a zero), a trace of free calls a zero cost, and a mixed
// one the priced part with the rest counted. SQL and Go agree on each.
func TestUsageCostCases(t *testing.T) {
	cases := []struct {
		name  string
		attrs []string
		want  event.Usage
	}{
		{"unpriced", []string{`{"prompt_tokens":10,"completion_tokens":1}`, `{"prompt_tokens":20,"completion_tokens":2}`},
			event.Usage{PromptTokens: 30, CompletionTokens: 3, UnpricedCalls: 2}},
		{"free", []string{`{"prompt_tokens":10,"completion_tokens":1,"cost_nanousd":0}`},
			event.Usage{PromptTokens: 10, CompletionTokens: 1, Cost: nu(0)}},
		{"free, legacy", []string{`{"prompt_tokens":10,"completion_tokens":1,"cost":0}`},
			event.Usage{PromptTokens: 10, CompletionTokens: 1, Cost: nu(0)}},
		{"mixed", []string{`{"prompt_tokens":10,"completion_tokens":1,"cost_nanousd":250000000}`, `{"prompt_tokens":20,"completion_tokens":2}`},
			event.Usage{PromptTokens: 30, CompletionTokens: 3, Cost: nu(250000000), UnpricedCalls: 1}},
		{"details", []string{`{"prompt_tokens":10,"completion_tokens":1,"cost_nanousd":0,"reasoning_tokens":0}`,
			`{"prompt_tokens":20,"completion_tokens":2,"cost":0,"cached_tokens":15}`},
			event.Usage{PromptTokens: 30, CompletionTokens: 3, Cost: nu(0), CachedTokens: ip(15), ReasoningTokens: ip(0)}},
		// Exactness: as REALs 0.1 + 0.2 is 0.30000000000000004; the legacy
		// floats are rounded to nano-USD per event and summed as integers.
		{"legacy 0.1 + 0.2", []string{`{"prompt_tokens":1,"cost":0.1}`, `{"prompt_tokens":1,"cost":0.2}`},
			event.Usage{PromptTokens: 2, Cost: nu(300000000)}},
		{"legacy and new", []string{`{"prompt_tokens":1,"cost":0.000123}`, `{"prompt_tokens":1,"cost_nanousd":877}`,
			`{"prompt_tokens":1,"cost_nanousd":5,"cost":99}`},
			event.Usage{PromptTokens: 3, Cost: nu(123000 + 877 + 5)}},
		{"ten calls of 100 nano-USD", slices.Repeat([]string{`{"prompt_tokens":1,"cost_nanousd":100}`}, 10),
			event.Usage{PromptTokens: 10, Cost: nu(1000)}},
		// Neither form as a usable number: unpriced, in SQL as in Go.
		{"not a cost", []string{`{"prompt_tokens":1,"cost":-0.5}`, `{"prompt_tokens":1,"cost_nanousd":1.5}`,
			`{"prompt_tokens":1,"cost_nanousd":"7"}`},
			event.Usage{PromptTokens: 3, UnpricedCalls: 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := newStore(t)
			sess := mustSession(t, s, tc.name)
			trace := event.NewID()
			var inGo event.Usage
			for _, a := range tc.attrs {
				e := emitUsage(t, s, sess.ID, trace, event.ModelResponse, a)
				inGo.Add(event.UsageOfEvent(e))
			}
			traces, err := s.ListTraces(ctx, sess.ID)
			if err != nil || len(traces) != 1 {
				t.Fatalf("list traces = %+v, %v", traces, err)
			}
			all, err := s.SessionUsage(ctx, sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			for name, u := range map[string]event.Usage{"go": inGo, "trace": traces[0].Usage, "session": all[sess.ID]} {
				if !sameUsage(u, tc.want) {
					t.Errorf("%s = %s, want %s", name, showUsage(u), showUsage(tc.want))
				}
			}
		})
	}
}

// A trace or session without a single model call totals zero, and a session
// with no counted events is simply absent from the map.
func TestUsageAbsent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sess := mustSession(t, s, "one")
	trace := event.NewID()
	emitUsage(t, s, sess.ID, trace, event.RunStarted, `{"model":"m"}`)
	emitUsage(t, s, sess.ID, trace, event.RunFinished, `{"prompt_tokens":10,"completion_tokens":1,"cost":1}`)
	emitUsage(t, s, sess.ID, trace, event.GuardDecision, `{"score":1}`)

	traces, err := s.ListTraces(ctx, sess.ID)
	if err != nil || len(traces) != 1 {
		t.Fatalf("list traces = %+v, %v", traces, err)
	}
	if !traces[0].Usage.IsZero() {
		t.Errorf("usage of a trace with no counted call = %s", showUsage(traces[0].Usage))
	}
	all, err := s.SessionUsage(ctx, sess.ID)
	if err != nil {
		t.Fatalf("session usage: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("session usage = %+v, want none", all)
	}
	var _ UsageLister = (*SQLite)(nil)
}

func TestSessionUsageClosedStore(t *testing.T) {
	s := newStore(t)
	_ = s.db.Close()
	if _, err := s.SessionUsage(context.Background()); err == nil {
		t.Error("session usage on a closed store: no error")
	}
}

func emitUsage(t *testing.T, s *SQLite, sessionID, traceID event.ID, typ event.Type, attrs string) event.Event {
	t.Helper()
	now := time.Now().UTC()
	e := event.Event{
		ID: event.NewIDAt(now), TraceID: traceID, SpanID: event.NewIDAt(now), SessionID: sessionID,
		Type: typ, Time: now, Attrs: json.RawMessage(attrs),
	}
	s.Emit(context.Background(), e)
	return e
}
