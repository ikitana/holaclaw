package event

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"slices"
	"strconv"

	"holaclaw/internal/provider"
)

// The usage attributes: every event that records a model call carries the
// call's tokens under these names, zero when the upstream reported none, so
// a missing prompt_tokens means "this event is not a model call". The cost
// and the token details are written only when the upstream reported them: a
// model call without a cost is an unpriced call (a local engine, a
// subscription), never a free one. A call that was not made (a cached
// verdict) writes none of them. PutUsage and Usage.Attrs write them.
const (
	AttrPromptTokens     = "prompt_tokens"
	AttrCompletionTokens = "completion_tokens"
	// AttrCostNanoUSD is the cost as an integer of nano-USD
	// (provider.NanoUSD), so sums are exact, in SQL too.
	AttrCostNanoUSD = "cost_nanousd"
	// AttrLegacyCost is the cost in USD as a float, what events carried
	// before AttrCostNanoUSD; it is read when that is absent, so older
	// traces still add up.
	AttrLegacyCost = "cost"
	// AttrCachedTokens is the part of the prompt served from a cache,
	// AttrReasoningTokens the part of the completion spent reasoning.
	AttrCachedTokens    = "cached_tokens"
	AttrReasoningTokens = "reasoning_tokens"
	// AttrUnpricedCalls is how many of the event's calls reported no cost,
	// written always, zero included, by one call and a sum alike. Events
	// stored before it existed lack it (UsageOf).
	AttrUnpricedCalls = "unpriced_calls"
	// AttrOutputTokens is what browser.task.finished called its completion
	// tokens before they were renamed; it is read when completion_tokens is
	// absent, so traces written before the rename still add up.
	AttrOutputTokens = "output_tokens"
)

// UsageTypes is the accounting rule: the event types whose usage attributes
// are summed into a trace's (or a session's) total. Every model call is
// reported by exactly one of them, so nothing is counted twice:
//
//   - model.response is one chat-loop call; run.finished repeats the sum of
//     its run's calls and is left out.
//   - browser.task.finished is the whole browser task — move, extraction,
//     vision and recovery calls and its decision-model questions; the
//     browser.move events repeat parts of it and are left out (not every
//     browser call has an event of its own, so the total is the only
//     complete record).
//   - guard.decision is the guard's one call, tool.result the taint scorer's
//     questions about that result, context.compacted the summarize call,
//     media.described the vision call, media.transcribed the speech-to-text
//     call, image.generated the image model, memory.embedded one embeddings
//     call.
//   - Some failures are counted too, because the call was answered and paid
//     for: model.error for a dropped stream, media.refused for an empty
//     description, image.generated with an error for a reply without an
//     image. The same types written without a reply carry no usage and
//     count nothing.
//
// A new event that records a model call is added here, or its usage is
// silently missing from every total. The store's SQL mirrors UsageOf over
// this list (store.ListTraces, store.SessionUsage); a test proves both agree.
var UsageTypes = []Type{
	ModelResponse, ModelError, BrowserTaskFinished, GuardDecision, ToolResult,
	ContextCompacted, MediaDescribed, MediaTranscribed, MediaRefused, ImageGenerated, MemoryEmbedded,
}

// CountsUsage reports whether events of this type count towards a total.
func CountsUsage(t Type) bool { return slices.Contains(UsageTypes, t) }

// Usage is what some model calls cost: one call (Call) or a sum of them
// (Add). The zero Usage is no call at all. Tokens are as the providers
// reported them. Cost sums the costs that were reported and is nil when none
// was; UnpricedCalls counts the calls that reported none, so a Cost with
// UnpricedCalls > 0 covers only part of the calls. The token details are nil
// when no call reported them.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	Cost             *provider.NanoUSD
	CachedTokens     *int
	ReasoningTokens  *int
	UnpricedCalls    int
}

// Call is one answered model call. It is never the zero Usage: a call
// without a cost is an unpriced one.
func Call(u provider.Usage) Usage {
	out := Usage{
		PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens,
		Cost: u.Cost, CachedTokens: u.CachedTokens, ReasoningTokens: u.ReasoningTokens,
	}
	if u.Cost == nil {
		out.UnpricedCalls = 1
	}
	return out
}

// Add sums o into u. A pointer stays nil only when it is nil on both sides:
// a sum of reported values ignores the unreported ones.
func (u *Usage) Add(o Usage) {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.Cost = addPtr(u.Cost, o.Cost)
	u.CachedTokens = addPtr(u.CachedTokens, o.CachedTokens)
	u.ReasoningTokens = addPtr(u.ReasoningTokens, o.ReasoningTokens)
	u.UnpricedCalls += o.UnpricedCalls
}

// addPtr returns a fresh pointer, so a sum shares nothing with its operands.
func addPtr[T int | provider.NanoUSD](a, b *T) *T {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		v := *b
		return &v
	case b == nil:
		v := *a
		return &v
	}
	v := *a + *b
	return &v
}

// IsZero reports whether u counts nothing at all: no call was made.
func (u Usage) IsZero() bool {
	return u.PromptTokens == 0 && u.CompletionTokens == 0 && u.Cost == nil &&
		u.CachedTokens == nil && u.ReasoningTokens == nil && u.UnpricedCalls == 0
}

// UsageAttrs is the usage attributes as JSON fields, for events whose attrs
// are a typed struct: embed a *UsageAttrs (Usage.Attrs), nil when no call
// was made.
type UsageAttrs struct {
	PromptTokens     int               `json:"prompt_tokens"`
	CompletionTokens int               `json:"completion_tokens"`
	Cost             *provider.NanoUSD `json:"cost_nanousd,omitempty"`
	CachedTokens     *int              `json:"cached_tokens,omitempty"`
	ReasoningTokens  *int              `json:"reasoning_tokens,omitempty"`
	UnpricedCalls    int               `json:"unpriced_calls"`
}

// Attrs is u's usage attributes, nil for the zero Usage.
func (u Usage) Attrs() *UsageAttrs {
	if u.IsZero() {
		return nil
	}
	return &UsageAttrs{
		PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens,
		Cost: u.Cost, CachedTokens: u.CachedTokens, ReasoningTokens: u.ReasoningTokens,
		UnpricedCalls: u.UnpricedCalls,
	}
}

// PutUsage writes u's usage attributes into attrs, the same ones Attrs
// marshals to; the zero Usage writes none.
func PutUsage(attrs map[string]any, u Usage) {
	a := u.Attrs()
	if a == nil {
		return
	}
	attrs[AttrPromptTokens] = a.PromptTokens
	attrs[AttrCompletionTokens] = a.CompletionTokens
	if a.Cost != nil {
		attrs[AttrCostNanoUSD] = int64(*a.Cost)
	}
	if a.CachedTokens != nil {
		attrs[AttrCachedTokens] = *a.CachedTokens
	}
	if a.ReasoningTokens != nil {
		attrs[AttrReasoningTokens] = *a.ReasoningTokens
	}
	attrs[AttrUnpricedCalls] = a.UnpricedCalls
}

// UsageOf is one event's share of a total under the rule: its usage
// attributes when the type counts, zero otherwise. attrs is the decoded attrs
// object; absent or non-numeric values count as not reported. An event says
// itself how many of its calls were unpriced; one stored before it did, with
// a prompt_tokens count, is one call, unpriced when it has no cost under
// either name.
func UsageOf(t Type, attrs map[string]any) Usage {
	if !CountsUsage(t) || attrs == nil {
		return Usage{}
	}
	completion, ok := number(attrs[AttrCompletionTokens])
	if !ok {
		completion, _ = number(attrs[AttrOutputTokens])
	}
	prompt, call := number(attrs[AttrPromptTokens])
	u := Usage{PromptTokens: int(prompt), CompletionTokens: int(completion)}
	u.Cost = CostOf(attrs)
	u.CachedTokens = intAttr(attrs, AttrCachedTokens)
	u.ReasoningTokens = intAttr(attrs, AttrReasoningTokens)
	if n, ok := number(attrs[AttrUnpricedCalls]); ok {
		u.UnpricedCalls = int(n)
	} else if call && u.Cost == nil {
		u.UnpricedCalls = 1
	}
	return u
}

// UsageOfEvent is UsageOf for a stored event, whose attrs are still JSON. An
// attrs object that does not parse counts as zero, as it does in the SQL.
// Numbers stay JSON text, so a cost is read without a float.
func UsageOfEvent(e Event) Usage {
	if !CountsUsage(e.Type) || len(e.Attrs) == 0 {
		return Usage{}
	}
	var attrs map[string]any
	dec := json.NewDecoder(bytes.NewReader(e.Attrs))
	dec.UseNumber()
	if dec.Decode(&attrs) != nil || dec.Decode(new(any)) != io.EOF {
		return Usage{}
	}
	return UsageOf(e.Type, attrs)
}

// CostOf is the cost an event's attrs carry, of any type: cost_nanousd when
// it is an integer, else the legacy USD cost when it is a number and not
// negative, rounded to the nano-USD (the store's SQL does the same); nil
// when neither is.
func CostOf(attrs map[string]any) *provider.NanoUSD {
	var n provider.NanoUSD
	switch v := attrs[AttrCostNanoUSD].(type) {
	case json.Number:
		i, err := v.Int64()
		if err == nil {
			n = provider.NanoUSD(i)
			return &n
		}
	case int64:
		n = provider.NanoUSD(v)
		return &n
	case int:
		n = provider.NanoUSD(v)
		return &n
	case float64: // attrs decoded without UseNumber; exact below 2^53
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			n = provider.NanoUSD(v)
			return &n
		}
	}
	switch v := attrs[AttrLegacyCost].(type) {
	case json.Number:
		return provider.WireCost(json.RawMessage(v))
	case float64:
		return provider.WireCost(json.RawMessage(strconv.FormatFloat(v, 'g', -1, 64)))
	}
	return nil
}

func intAttr(attrs map[string]any, k string) *int {
	n, ok := number(attrs[k])
	if !ok {
		return nil
	}
	v := int(n)
	return &v
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
