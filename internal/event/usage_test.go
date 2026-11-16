package event

import (
	"encoding/json"
	"reflect"
	"testing"

	"holaclaw/internal/provider"
)

func nu(v int64) *provider.NanoUSD { n := provider.NanoUSD(v); return &n }
func i(v int) *int                 { return &v }

func TestUsageOf(t *testing.T) {
	cases := []struct {
		name  string
		t     Type
		attrs map[string]any
		want  Usage
	}{
		{"a priced chat call", ModelResponse,
			map[string]any{"prompt_tokens": 1200.0, "completion_tokens": 80.0, "cost_nanousd": 1200000.0},
			Usage{PromptTokens: 1200, CompletionTokens: 80, Cost: nu(1200000)}},
		{"a free call is priced at zero", ModelResponse,
			map[string]any{"prompt_tokens": 10.0, "completion_tokens": 2.0, "cost_nanousd": 0.0},
			Usage{PromptTokens: 10, CompletionTokens: 2, Cost: nu(0)}},
		{"a call without a cost is unpriced", ModelResponse,
			map[string]any{"prompt_tokens": 10.0, "completion_tokens": 2.0},
			Usage{PromptTokens: 10, CompletionTokens: 2, UnpricedCalls: 1}},
		{"a call says it was unpriced", ModelResponse,
			map[string]any{"prompt_tokens": 10.0, "completion_tokens": 2.0, "unpriced_calls": 1.0},
			Usage{PromptTokens: 10, CompletionTokens: 2, UnpricedCalls: 1}},
		{"the details are read when reported", ModelResponse,
			map[string]any{"prompt_tokens": 100.0, "completion_tokens": 50.0, "cached_tokens": 60.0, "reasoning_tokens": 0.0},
			Usage{PromptTokens: 100, CompletionTokens: 50, CachedTokens: i(60), ReasoningTokens: i(0), UnpricedCalls: 1}},
		{"a sum says how many of its calls were unpriced", BrowserTaskFinished,
			map[string]any{"prompt_tokens": 500.0, "completion_tokens": 40.0, "cost_nanousd": 10000000.0, "unpriced_calls": 3.0},
			Usage{PromptTokens: 500, CompletionTokens: 40, Cost: nu(10000000), UnpricedCalls: 3}},
		{"a sum of priced calls", ToolResult,
			map[string]any{"prompt_tokens": 5.0, "completion_tokens": 1.0, "cost_nanousd": 500000000.0, "unpriced_calls": 0.0},
			Usage{PromptTokens: 5, CompletionTokens: 1, Cost: nu(500000000)}},
		{"a run total repeats its responses", RunFinished,
			map[string]any{"prompt_tokens": 1200.0, "completion_tokens": 80.0, "cost_nanousd": 1200000.0}, Usage{}},
		{"a move is inside its task", BrowserMove,
			map[string]any{"prompt_tokens": 300.0, "completion_tokens": 30.0}, Usage{}},
		{"a task from before the rename", BrowserTaskFinished,
			map[string]any{"prompt_tokens": 10.0, "output_tokens": 5.0},
			Usage{PromptTokens: 10, CompletionTokens: 5, UnpricedCalls: 1}},
		{"completion_tokens wins over the old name", BrowserTaskFinished,
			map[string]any{"completion_tokens": 7.0, "output_tokens": 5.0}, Usage{CompletionTokens: 7}},
		{"a cached verdict is no call", GuardDecision, map[string]any{"score": 1.0}, Usage{}},
		{"a model error without a reply is no call", ModelError, map[string]any{"kind": "timeout"}, Usage{}},
		{"a dropped stream is a call", ModelError,
			map[string]any{"kind": "dropped", "prompt_tokens": 7.0, "completion_tokens": 0.0},
			Usage{PromptTokens: 7, UnpricedCalls: 1}},
		{"a transcription", MediaTranscribed,
			map[string]any{"prompt_tokens": 40.0, "completion_tokens": 12.0, "cost_nanousd": int64(300000)},
			Usage{PromptTokens: 40, CompletionTokens: 12, Cost: nu(300000)}},
		{"an embedding", MemoryEmbedded,
			map[string]any{"prompt_tokens": 9.0, "completion_tokens": 0.0},
			Usage{PromptTokens: 9, UnpricedCalls: 1}},
		{"a legacy USD cost", ModelResponse,
			map[string]any{"prompt_tokens": 10.0, "completion_tokens": 2.0, "cost": 0.0012},
			Usage{PromptTokens: 10, CompletionTokens: 2, Cost: nu(1200000)}},
		{"a legacy cost of zero is still priced", ModelResponse,
			map[string]any{"prompt_tokens": 10.0, "cost": 0.0},
			Usage{PromptTokens: 10, Cost: nu(0)}},
		{"cost_nanousd wins over the legacy cost", ModelResponse,
			map[string]any{"prompt_tokens": 1.0, "cost_nanousd": 7.0, "cost": 0.5},
			Usage{PromptTokens: 1, Cost: nu(7)}},
		{"a fractional cost_nanousd falls back to the legacy cost", ModelResponse,
			map[string]any{"prompt_tokens": 1.0, "cost_nanousd": json.Number("7.5"), "cost": json.Number("1.2e-05")},
			Usage{PromptTokens: 1, Cost: nu(12000)}},
		{"a negative legacy cost is unpriced", ModelResponse,
			map[string]any{"prompt_tokens": 1.0, "cost": -0.5},
			Usage{PromptTokens: 1, UnpricedCalls: 1}},
		{"not a number", ModelResponse, map[string]any{"prompt_tokens": "many", "cost_nanousd": true, "cost": "1"}, Usage{}},
		{"Go values", ToolResult, map[string]any{"prompt_tokens": 3, "completion_tokens": int64(2),
			"cost_nanousd": json.Number("500000000")}, Usage{PromptTokens: 3, CompletionTokens: 2, Cost: nu(500000000)}},
		{"nil attrs", ModelResponse, nil, Usage{}},
	}
	for _, c := range cases {
		if got := UsageOf(c.t, c.attrs); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: %s, want %s", c.name, show(got), show(c.want))
		}
	}
}

func TestUsageOfEvent(t *testing.T) {
	e := Event{Type: ImageGenerated, Attrs: json.RawMessage(`{"prompt_tokens":0,"completion_tokens":0,"cost_nanousd":40000000}`)}
	if got := UsageOfEvent(e); !reflect.DeepEqual(got, Usage{Cost: nu(40000000)}) {
		t.Errorf("image = %s", show(got))
	}
	// A legacy cost is read from its JSON text, not through a float.
	e = Event{Type: ImageGenerated, Attrs: json.RawMessage(`{"prompt_tokens":0,"completion_tokens":0,"cost":0.000000123456789}`)}
	if got := UsageOfEvent(e); !reflect.DeepEqual(got, Usage{Cost: nu(123)}) {
		t.Errorf("legacy image = %s", show(got))
	}
	// Beyond 2^53 a float would round: the integer is read exactly.
	e = Event{Type: ModelResponse, Attrs: json.RawMessage(`{"prompt_tokens":1,"cost_nanousd":9007199254740993}`)}
	if got := UsageOfEvent(e); got.Cost == nil || *got.Cost != 9007199254740993 {
		t.Errorf("large cost = %s", show(got))
	}
	e = Event{Type: ModelResponse, Attrs: json.RawMessage(`not json`)}
	if got := UsageOfEvent(e); !got.IsZero() {
		t.Errorf("unparseable attrs = %s, want zero", show(got))
	}
	e = Event{Type: ModelResponse, Attrs: json.RawMessage(`{"prompt_tokens":1} trailing`)}
	if got := UsageOfEvent(e); !got.IsZero() {
		t.Errorf("trailing data = %s, want zero", show(got))
	}
	if got := UsageOfEvent(Event{Type: ModelResponse}); !got.IsZero() {
		t.Errorf("no attrs = %s, want zero", show(got))
	}
}

func TestUsageAdd(t *testing.T) {
	var u Usage
	u.Add(Call(provider.Usage{PromptTokens: 1, CompletionTokens: 2}))
	if u.Cost != nil || u.CachedTokens != nil || u.UnpricedCalls != 1 {
		t.Fatalf("nil + nil = %s, want nil and one unpriced call", show(u))
	}
	priced := Call(provider.Usage{PromptTokens: 1, CompletionTokens: 2, Cost: nu(500000000), CachedTokens: i(1)})
	u.Add(priced)
	u.Add(Usage{Cost: nu(250000000), ReasoningTokens: i(0)})
	want := Usage{PromptTokens: 2, CompletionTokens: 4, Cost: nu(750000000), CachedTokens: i(1), ReasoningTokens: i(0), UnpricedCalls: 1}
	if !reflect.DeepEqual(u, want) {
		t.Errorf("add = %s, want %s", show(u), show(want))
	}
	*u.Cost = 9 // the sum shares nothing with its operands
	if *priced.Cost != 500000000 {
		t.Error("Add aliased an operand's pointer")
	}
	if got := Call(provider.Usage{Cost: nu(0)}); got.UnpricedCalls != 0 || got.Cost == nil || got.IsZero() {
		t.Errorf("a free call = %s, want a priced call", show(got))
	}
	// Integer adds are exact: 0.1 + 0.2 USD is 0.3, and ten calls of 100
	// nano-USD are 1000.
	var s Usage
	s.Add(Usage{Cost: nu(100000000)})
	s.Add(Usage{Cost: nu(200000000)})
	if *s.Cost != 300000000 {
		t.Errorf("0.1 + 0.2 = %d nano-USD", *s.Cost)
	}
	var ten Usage
	for range 10 {
		ten.Add(Usage{Cost: nu(100)})
	}
	if *ten.Cost != 1000 {
		t.Errorf("ten calls = %d nano-USD", *ten.Cost)
	}
}

// PutUsage writes tokens and unpriced_calls always, the rest only when
// reported, nothing for no call, the same keys the typed form marshals to,
// and what it writes reads back as what went in.
func TestPutUsage(t *testing.T) {
	cases := []struct {
		name string
		u    Usage
		want string
	}{
		{"no call", Usage{}, `{"mode":"x"}`},
		{"an unpriced call", Call(provider.Usage{PromptTokens: 5, CompletionTokens: 1}),
			`{"mode":"x","prompt_tokens":5,"completion_tokens":1,"unpriced_calls":1}`},
		{"a priced call", Call(provider.Usage{PromptTokens: 5, Cost: nu(0), CachedTokens: i(2), ReasoningTokens: i(3)}),
			`{"mode":"x","prompt_tokens":5,"completion_tokens":0,"cost_nanousd":0,"cached_tokens":2,"reasoning_tokens":3,"unpriced_calls":0}`},
		{"a sum", Usage{PromptTokens: 5, Cost: nu(500000000), UnpricedCalls: 2},
			`{"mode":"x","prompt_tokens":5,"completion_tokens":0,"cost_nanousd":500000000,"unpriced_calls":2}`},
	}
	for _, c := range cases {
		typed, err := json.Marshal(struct {
			Mode string `json:"mode"`
			*UsageAttrs
		}{"x", c.u.Attrs()})
		if err != nil || string(typed) != c.want {
			t.Errorf("%s: typed attrs = %s, %v, want %s", c.name, typed, err, c.want)
		}
		attrs := map[string]any{"mode": "x"}
		PutUsage(attrs, c.u)
		if m, _ := json.Marshal(attrs); !jsonEqual(m, typed) {
			t.Errorf("%s: PutUsage = %s, typed = %s", c.name, m, typed)
		}
		if got := UsageOf(ModelResponse, attrs); !reflect.DeepEqual(got, c.u) {
			t.Errorf("%s: read back = %s", c.name, show(got))
		}
		if got := UsageOfEvent(Event{Type: ModelResponse, Attrs: typed}); !reflect.DeepEqual(got, c.u) {
			t.Errorf("%s: read back from JSON = %s", c.name, show(got))
		}
	}
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

// Every type in the rule is a known event type: a typo would leave a model
// call out of every total without anybody noticing.
func TestUsageTypesAreKnown(t *testing.T) {
	known := map[Type]bool{}
	for _, ty := range Types {
		known[ty] = true
	}
	for _, u := range UsageTypes {
		if !known[u] {
			t.Errorf("%s counts towards totals but is not in Types", u)
		}
		if u == RunFinished || u == BrowserMove {
			t.Errorf("%s repeats usage another event reports", u)
		}
	}
}

func show(u Usage) string {
	b, _ := json.Marshal(u)
	return string(b)
}
