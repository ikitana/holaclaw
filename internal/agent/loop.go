package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/taint"
	"holaclaw/internal/tool"
)

// Transcript is the narrow persistence the loop needs; store.Store satisfies
// it. The loop only ever appends.
type Transcript interface {
	AppendMessage(ctx context.Context, m event.Message, pending bool) (bool, error)
}

// ModelRef is a model together with the provider that serves it.
type ModelRef struct {
	Model    provider.Model
	Provider provider.Provider
}

// StopReason says why a turn ended.
type StopReason string

const (
	StopFinal        StopReason = "final"
	StopStepLimit    StopReason = "step_limit"
	StopLoopDetected StopReason = "loop_detected"
	StopAborted      StopReason = "aborted"
	StopError        StopReason = "error"
)

// DefaultMaxSteps is the hard step limit when Definition.MaxSteps is zero.
const DefaultMaxSteps = 12

// DefaultTaintThreshold is the taint score at or above which untrusted
// content escalates policy (docs/security.md). It lives here rather than in
// internal/tool because the gate acts on Call.Tainted and this loop is what
// decides when a result sets it. Measured: `eval taint` puts ordinary content
// and steering text on either side of it with room to spare (docs/models.md,
// "Taint scorer").
const DefaultTaintThreshold = 0.5

const (
	// maxArgsAttr caps the raw arguments copied into a tool.call event.
	maxArgsAttr = 2 << 10
	// maxProblemAttr caps the schema problem copied into a tool.rejected
	// event; the model still sees it whole.
	maxProblemAttr = 200
	// loopThreshold is the number of identical calls that trips the detector;
	// the call that reaches it is not executed.
	loopThreshold = 3
	// maxInvalidCalls is how many consecutive rejected calls a turn tolerates
	// before tools are taken away.
	maxInvalidCalls = 3
	// extraFinalSteps bounds the re-elicitation steps after the step limit.
	extraFinalSteps = 4
)

// Notes appended to the prompt (never persisted) to steer the next step.
const (
	noteFinalAnswer = "Step limit reached; answer now with what you have."
	noteSkillSaved  = "The skill is saved; answer now without further tool calls."
	noteTextLeak    = "Your last message contained a tool call written as text. " +
		"Use the tool interface to call tools, or answer in plain text."
	noteTruncated = "Your reply was cut off. Answer more briefly."
	noteEmpty     = "Your reply was empty."
)

// Canned replies for turns the model could not finish itself.
const (
	emptyReply = "Sorry, I have nothing to add."
	leakReply  = "Sorry, I could not complete that."
)

// ErrDroppedStream is returned when a stream ends without a finish reason
// twice in a row for the same step (docs/models.md).
var ErrDroppedStream = errors.New("agent: model stream dropped twice")

// ErrAssistantLast is returned when the prompt would end with an assistant
// message: there is nothing to answer, and some providers (ollama) take it as
// the start of the reply and hand that old answer back.
var ErrAssistantLast = errors.New("agent: prompt ends with an assistant message")

// Runner executes one turn of one session. It is a pure library: everything
// outside is reached through Provider, tool.Executor, Builder, Transcript and
// event.Sink (ARCHITECTURE.md §2). A Runner is used by one session actor at a
// time; Turn is not safe for concurrent use.
type Runner struct {
	Def        Definition
	Model      provider.Model
	Provider   provider.Provider
	Fallback   *ModelRef // opt-in second model; every use is a trace event
	Tools      tool.Executor
	Builder    Builder
	Transcript Transcript
	Events     event.Sink
	Now        func() time.Time
	// TaintDecay is how many tool calls after untrusted content entered the
	// turn still count as tainted (docs/security.md); 0 means
	// tool.DefaultTaintDecayCalls.
	TaintDecay int
	// Taint grades untrusted results: provenance is a fact, escalation is
	// graded (docs/security.md). nil means every untrusted result counts as 1
	// — the old behaviour, and what a deployment without a decision model
	// gets.
	Taint taint.Scorer
	// TaintThreshold is the score at or above which an untrusted result arms
	// the taint; 0 means DefaultTaintThreshold.
	TaintThreshold float64
}

// TurnInput is one turn: the stored transcript plus the trusted fields every
// tool call inherits.
type TurnInput struct {
	SessionID, TraceID event.ID
	// History is the transcript so far, including the new user message(s),
	// already stored.
	History    []event.Message
	Injections []Injection
	// Call is the template for every tool call: its trusted fields (Origin,
	// Role, UserID, RunKind, Agent, Partition, Workspace, …) are copied in.
	Call   tool.Call
	Images func(ctx context.Context, a event.Attachment) ([]byte, error)
	// MaxSteps tightens the agent's step limit for this turn; 0 = agent default.
	MaxSteps int
	// Skills is the run's skill index, decided by the daemon for this sender
	// and space. nil means Def.Skills; empty (non-nil) means none, which also
	// hides skill.load.
	Skills []SkillEntry
	// OnText receives visible token deltas; may be nil.
	OnText func(delta string)
}

// TurnResult is what one turn produced.
type TurnResult struct {
	Reply           event.Message   // final assistant message, already appended
	New             []event.Message // every message the turn appended, in order
	Steps           int
	Usage           event.Usage
	Stop            StopReason
	NeedsCompaction bool
}

// turnState is the mutable state of one turn.
type turnState struct {
	in  TurnInput
	run event.ID // span id of run.started: parent of every event in the turn

	model      provider.Model
	prov       provider.Provider
	onFallback bool

	specs    map[string]provider.ToolSpec
	specList []provider.ToolSpec

	history  []event.Message // stored transcript + prompt-only notes
	newMsgs  []event.Message
	reply    event.Message
	attached []event.Attachment
	reacted  bool // the turn reacted to its inbound message: no text is fine

	usage event.Usage // every answered call, which run.finished repeats
	steps int
	// taintTTL is how many more tool calls still count as tainted; it is set
	// (or refreshed) to taintDecay whenever a tool result comes back
	// untrusted, and counts down by one on every dispatched call.
	taintTTL   int
	taintDecay int
	// taintThr is the score at or above which a result arms the taint;
	// taintMax is the highest score this turn has seen, the history window
	// included. The max is stamped on the reply, so the next turn reads it
	// back instead of re-arming on provenance alone.
	taintThr float64
	taintMax float64
	// taintEver marks the turn's own messages (reply, tool results) as
	// carrying untrusted content for the record, even once taintTTL has
	// decayed back to 0 and later calls are no longer gated by it.
	taintEver bool
	calls     map[string]int
	invalid   int
	loopHit   bool
	final     bool // tools withdrawn, next model call must answer
	stop      StopReason
	partial   strings.Builder // text streamed for the current step
	compact   bool
	leakDone  bool
	truncated bool
	emptyDone bool
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// skills is the index of this turn: the run's own when the caller set one,
// else the definition's.
func (r *Runner) skills(in TurnInput) []SkillEntry {
	if in.Skills != nil {
		return in.Skills
	}
	return r.Def.Skills
}

// maxSteps is the hard step limit of a turn: the agent's, tightened — never
// loosened — by a per-run limit (a task's max_steps).
func (r *Runner) maxSteps(in TurnInput) int {
	n := DefaultMaxSteps
	if r.Def.MaxSteps > 0 {
		n = r.Def.MaxSteps
	}
	if in.MaxSteps > 0 && in.MaxSteps < n {
		n = in.MaxSteps
	}
	return n
}

// Turn runs the model/tool loop until the model produces a final reply or a
// limit stops it. The returned result is valid even when err is non-nil:
// everything already appended is reported.
func (r *Runner) Turn(ctx context.Context, in TurnInput) (TurnResult, error) {
	decay := r.TaintDecay
	if decay <= 0 {
		decay = tool.DefaultTaintDecayCalls
	}
	thr := r.TaintThreshold
	if thr <= 0 {
		thr = DefaultTaintThreshold
	}
	st := &turnState{
		in:         in,
		model:      r.Model,
		prov:       r.Provider,
		specs:      map[string]provider.ToolSpec{},
		history:    append([]event.Message(nil), in.History...),
		calls:      map[string]int{},
		taintDecay: decay,
		taintThr:   thr,
	}
	// Provenance carries over unconditionally: untrusted content still in the
	// history window keeps this turn's own messages marked and wrapped.
	// Escalation carries over only when that content scored as steering
	// (docs/security.md); it then taints the next `decay` calls of this turn
	// too, because the model can still be steered by it — bounded rather than
	// sticking for good.
	//
	// A legacy message has Tainted with no score (it predates the scorer) and
	// does not arm. Escalation was always meant to be bounded, and re-arming
	// on every old browse is exactly what docs/security.md removed; the content is still
	// wrapped as untrusted, which is the part that must hold.
	if anyTainted(in.History) {
		st.taintEver = true
	}
	st.taintMax = maxTaintScore(in.History)
	if armedBy(in.History, thr) {
		st.taintTTL = decay
	}
	if r.Tools != nil {
		st.specList = VisibleTools(r.Tools.Specs(), r.skills(in), in.Call)
		for _, s := range st.specList {
			st.specs[s.Name] = s
		}
	}
	start := r.now()
	runEv := r.newEvent(event.RunStarted, st, "", map[string]any{
		"agent":     r.Def.Name,
		"model":     st.model.Name,
		"provider":  st.model.Provider,
		"max_steps": r.maxSteps(in),
		"tools":     len(st.specList),
	})
	r.emit(ctx, runEv)
	st.run = runEv.SpanID

	err := r.run(ctx, st)
	if st.stop == "" {
		if err != nil {
			st.stop = StopError
		} else {
			st.stop = StopFinal
		}
	}
	finAttrs := map[string]any{"steps": st.steps, "stop": string(st.stop)}
	event.PutUsage(finAttrs, st.usage)
	fin := r.newEvent(event.RunFinished, st, st.run, finAttrs)
	fin.Duration = r.now().Sub(start)
	if st.reply.ID != "" {
		fin.MessageID = st.reply.ID
	}
	r.emit(context.WithoutCancel(ctx), fin)

	return TurnResult{
		Reply:           st.reply,
		New:             st.newMsgs,
		Steps:           st.steps,
		Usage:           st.usage,
		Stop:            st.stop,
		NeedsCompaction: st.compact,
	}, err
}

// run is the step loop.
func (r *Runner) run(ctx context.Context, st *turnState) error {
	for {
		if err := ctx.Err(); err != nil {
			return r.abort(ctx, st, err)
		}
		if !st.final && st.steps >= r.maxSteps(st.in) {
			r.enterFinal(ctx, st, StopStepLimit, event.RunStepLimit, map[string]any{
				"steps": st.steps, "limit": r.maxSteps(st.in), "reason": "max_steps",
			})
		}
		if st.steps >= r.maxSteps(st.in)+extraFinalSteps {
			// Safety net: the model keeps refusing to finish.
			st.stop = StopStepLimit
			return r.finish(ctx, st, emptyReply)
		}
		p, err := r.build(ctx, st)
		if err != nil {
			return err
		}
		resp, err := r.chat(ctx, st, p)
		if err != nil {
			if ctx.Err() != nil {
				return r.abort(ctx, st, ctx.Err())
			}
			return err
		}
		st.steps++
		done, err := r.handle(ctx, st, resp)
		if err != nil {
			if ctx.Err() != nil {
				return r.abort(ctx, st, ctx.Err())
			}
			return err
		}
		if done {
			return nil
		}
	}
}

// build asks the context builder for a prompt and records context.built.
func (r *Runner) build(ctx context.Context, st *turnState) (Prompt, error) {
	var specs []provider.ToolSpec
	if !st.final {
		specs = st.specList
	}
	def := r.Def
	def.Skills = r.skills(st.in)
	p, err := r.Builder.Build(ctx, BuildInput{
		Def:        def,
		Model:      st.model,
		Tools:      specs,
		History:    st.history,
		Injections: st.in.Injections,
		Images:     st.in.Images,
	})
	if err != nil {
		return Prompt{}, fmt.Errorf("agent: build prompt: %w", err)
	}
	if n := len(p.Messages); n > 0 && p.Messages[n-1].Role == provider.RoleAssistant {
		return Prompt{}, ErrAssistantLast
	}
	st.compact = p.NeedsCompaction
	if st.final {
		p.Tools = nil
	}
	attrs := BuiltAttrs(p)
	attrs["tools"] = len(p.Tools)
	r.emit(ctx, r.newEvent(event.ContextBuilt, st, st.run, attrs))
	return p, nil
}

// chat performs one model call, with the dropped-stream retry and the opt-in
// fallback model. It never counts steps; the caller does.
func (r *Runner) chat(ctx context.Context, st *turnState, p Prompt) (*provider.Response, error) {
	req := provider.Request{
		Model:     st.model,
		System:    p.System,
		Messages:  p.Messages,
		Tools:     p.Tools,
		MaxTokens: p.Breakdown.ReplyReserve,
		SessionID: string(st.in.SessionID),
	}
	drops := 0
	for {
		req.Model = st.model
		st.partial.Reset()
		r.emit(ctx, r.newEvent(event.ModelRequest, st, st.run, map[string]any{
			"model":      st.model.Name,
			"model_id":   st.model.ID,
			"provider":   st.model.Provider,
			"step":       st.steps + 1,
			"messages":   len(req.Messages),
			"tools":      len(req.Tools),
			"max_tokens": req.MaxTokens,
			"fallback":   st.onFallback,
		}))
		start := r.now()
		streamed := false
		var resp *provider.Response
		stream, err := st.prov.Chat(ctx, req)
		if err == nil {
			resp, err = provider.CollectProgress(stream, func(d string) {
				if d != "" {
					streamed = true
					st.partial.WriteString(d)
				}
				if st.in.OnText != nil {
					st.in.OnText(d)
				}
			}, func() {
				r.emit(ctx, r.newEvent(event.ModelProgress, st, st.run, map[string]any{"elapsed_ms": r.now().Sub(start).Milliseconds()}))
			})
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			kind := provider.KindOf(err)
			var pe *provider.Error
			if errors.As(err, &pe) && pe.Kind == provider.ErrUsageLimit {
				r.emit(ctx, r.newEvent(event.ModelUsageLimit, st, st.run, map[string]any{
					"provider": pe.Provider, "model": st.model.Name,
					"reset_at": pe.ResetAt.UTC().Format(time.RFC3339),
				}))
			}
			if r.Fallback != nil && !st.onFallback && !streamed && fallbackKind(kind) {
				r.emit(ctx, r.newEvent(event.ModelFallback, st, st.run, map[string]any{
					"from": st.model.Name, "to": r.Fallback.Model.Name, "reason": string(kind),
				}))
				st.onFallback = true
				st.model = r.Fallback.Model
				st.prov = r.Fallback.Provider
				continue
			}
			r.emit(ctx, r.newEvent(event.ModelError, st, st.run, map[string]any{
				"model": st.model.Name, "kind": string(kind), "error": err.Error(),
			}))
			return nil, err
		}
		if resp.Finish == provider.FinishDropped {
			// The stream was answered, so it is a call made: its usage (what
			// arrived of it) is counted here, on the only event it gets.
			drops++
			attrs := map[string]any{"model": st.model.Name, "kind": "dropped", "attempt": drops}
			u := event.Call(resp.Usage)
			event.PutUsage(attrs, u)
			st.usage.Add(u)
			r.emit(ctx, r.newEvent(event.ModelError, st, st.run, attrs))
			if drops > 1 {
				return nil, fmt.Errorf("%w (model %s)", ErrDroppedStream, st.model.Name)
			}
			continue
		}
		attrs := map[string]any{
			"model":      st.model.Name,
			"finish":     string(resp.Finish),
			"text_bytes": len(resp.Text),
			"tool_calls": len(resp.ToolCalls),
			"repairs":    resp.Repairs,
		}
		u := event.Call(resp.Usage)
		event.PutUsage(attrs, u)
		ev := r.newEvent(event.ModelResponse, st, st.run, attrs)
		ev.Duration = r.now().Sub(start)
		r.emit(ctx, ev)
		st.usage.Add(u)
		return resp, nil
	}
}

// fallbackKind reports whether an error kind may be retried on the fallback
// model (docs/models.md).
func fallbackKind(k provider.ErrorKind) bool {
	switch k {
	case provider.ErrTimeout, provider.ErrServer, provider.ErrNetwork,
		provider.ErrRateLimit, provider.ErrUsageLimit:
		return true
	default:
		return false
	}
}

// handle consumes one model response. done reports that the turn is over.
func (r *Runner) handle(ctx context.Context, st *turnState, resp *provider.Response) (bool, error) {
	text := resp.Text
	calls := resp.ToolCalls

	if resp.Finish == provider.FinishLength {
		// Truncated tool calls are never trusted: they are discarded, nothing
		// is fed back, and the model is asked once for a shorter answer.
		if len(calls) > 0 && !st.truncated {
			st.truncated = true
			st.note(noteTruncated)
			return false, nil
		}
		return true, r.finish(ctx, st, truncateMark(text))
	}

	if len(calls) > 0 && !st.final && r.Tools != nil {
		return false, r.runTools(ctx, st, resp, calls)
	}

	if leaked, stripped, ok := r.leaked(text, st); ok {
		text = stripped
		if st.model.Strict && len(leaked) == 1 && !st.final && r.Tools != nil {
			if c, promoted := r.promote(ctx, st, leaked[0]); promoted {
				return false, r.runTools(ctx, st, &provider.Response{Text: text}, []provider.ToolCall{c})
			}
		}
		for _, l := range leaked {
			r.emit(ctx, r.newEvent(event.ToolRejected, st, st.run, map[string]any{
				"tool": l.Name, "reason": "text_leak",
			}))
		}
		if !st.leakDone && !st.final {
			st.leakDone = true
			st.note(noteTextLeak)
			return false, nil
		}
		if strings.TrimSpace(text) == "" {
			text = leakReply
		}
	}

	if strings.TrimSpace(text) == "" && len(st.attached) == 0 {
		if st.reacted {
			// The reaction was the answer: an empty reply is stored and
			// Silent keeps it from being sent.
			return true, r.finish(ctx, st, "")
		}
		if !st.emptyDone {
			st.emptyDone = true
			st.note(noteEmpty)
			return false, nil
		}
		text = emptyReply
	}
	return true, r.finish(ctx, st, text)
}

// leaked runs the text-leak detector against the tools this agent has.
func (r *Runner) leaked(text string, st *turnState) ([]leakedCall, string, bool) {
	if len(st.specs) == 0 {
		return nil, text, false
	}
	return detectLeakedCalls(text, func(name string) bool {
		_, ok := st.specs[name]
		return ok
	})
}

// promote turns a leaked call into a native one, but only when the model is
// flagged Strict and the call is schema-valid (docs/models.md).
func (r *Runner) promote(ctx context.Context, st *turnState, l leakedCall) (provider.ToolCall, bool) {
	spec, ok := r.spec(st, l.Name)
	if !ok {
		return provider.ToolCall{}, false
	}
	if !json.Valid([]byte(l.Args)) {
		return provider.ToolCall{}, false
	}
	var obj map[string]any
	if json.Unmarshal([]byte(l.Args), &obj) != nil {
		return provider.ToolCall{}, false
	}
	if validateArgs(spec.Parameters, json.RawMessage(l.Args)) != nil {
		return provider.ToolCall{}, false
	}
	c := provider.ToolCall{ID: "leak_" + string(event.NewIDAt(r.now())), Name: l.Name, Arguments: l.Args}
	r.emit(ctx, r.newEvent(event.ToolRepaired, st, st.run, map[string]any{
		"tool": l.Name, "call_id": c.ID, "stage": "promoted",
	}))
	return c, true
}

// runTools appends the assistant message and executes its calls in order.
func (r *Runner) runTools(ctx context.Context, st *turnState, resp *provider.Response, calls []provider.ToolCall) error {
	for _, c := range calls {
		if stage, ok := resp.Repairs[c.ID]; ok {
			r.emit(ctx, r.newEvent(event.ToolRepaired, st, st.run, map[string]any{
				"tool": c.Name, "call_id": c.ID, "stage": stage,
			}))
		}
	}
	am := r.message(st, event.RoleAssistant, resp.Text)
	for _, c := range calls {
		am.ToolCalls = append(am.ToolCalls, event.ToolCall{ID: c.ID, Name: c.Name, Arguments: c.Arguments})
	}
	if err := r.append(ctx, st, am); err != nil {
		return err
	}
	for i, c := range calls {
		if err := ctx.Err(); err != nil {
			return err
		}
		var (
			res   tool.Result
			score float64
			err   error
		)
		if c.Name == SkillSaveTool && i < len(calls)-1 {
			// Not a malformed call, so it does not count toward st.invalid.
			r.emit(ctx, r.newEvent(event.ToolRejected, st, st.run, map[string]any{
				"tool": c.Name, "call_id": c.ID, "reason": "not_last",
			}))
			res = errResult(noteSaveNotLast)
		} else if res, score, err = r.dispatch(ctx, st, c); err != nil {
			return err
		}
		tm := r.message(st, event.RoleTool, res.Text)
		tm.ToolCallID = c.ID
		tm.ToolName = c.Name
		// Provenance is unconditional, escalation is graded (docs/security.md): the message is marked untrusted whatever it scored, and only
		// a score at the threshold arms the next calls.
		tm.Tainted = res.Tainted
		tm.TaintScore = score
		if err := r.append(ctx, st, tm); err != nil {
			return err
		}
		if res.Tainted {
			st.taintEver = true
			if score > st.taintMax {
				st.taintMax = score
			}
			if score >= st.taintThr {
				st.taintTTL = st.taintDecay
			}
		}
		st.attached = append(st.attached, res.Attachments...)
		st.reacted = st.reacted || res.Reacted
		if c.Name == SkillSaveTool && !res.IsError && !st.final {
			// The "last call of a turn" rule (docs/skills.md §5): a saved
			// skill withdraws the tools, so nothing can follow it. A failed
			// save leaves them, and the model fixes it in its next call.
			st.withdrawTools(StopFinal, noteSkillSaved)
		}
	}
	if st.invalid >= maxInvalidCalls && !st.final {
		r.enterFinal(ctx, st, StopStepLimit, event.RunStepLimit, map[string]any{
			"steps": st.steps, "reason": "invalid_tool_calls",
		})
	}
	return nil
}

// dispatch validates one call and, if it survives, executes it. Validation
// failures and loop hits never reach the Executor.
func (r *Runner) dispatch(ctx context.Context, st *turnState, c provider.ToolCall) (tool.Result, float64, error) {
	spec, known := r.spec(st, c.Name)
	if !known {
		st.invalid++
		r.emit(ctx, r.newEvent(event.ToolRejected, st, st.run, map[string]any{
			"tool": c.Name, "call_id": c.ID, "reason": "unknown_tool",
		}))
		return errResult(fmt.Sprintf("unknown tool %q. Available tools: %s",
			c.Name, strings.Join(specNames(st.specList), ", "))), 0, nil
	}
	// Resolved before the call: a connector's tool list may change while it
	// runs, and the result must still be attributed and trusted as called.
	conn, trusted := r.connector(c.Name)
	args := strings.TrimSpace(c.Arguments)
	if args == "" {
		args = "{}"
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(args), &obj); err != nil {
		st.invalid++
		r.emit(ctx, r.newEvent(event.ToolRejected, st, st.run, map[string]any{
			"tool": c.Name, "call_id": c.ID, "reason": "bad_arguments",
		}))
		return errResult(fmt.Sprintf("arguments for %s must be a JSON object: %v", c.Name, err)), 0, nil
	}
	if err := validateArgs(spec.Parameters, json.RawMessage(args)); err != nil {
		st.invalid++
		r.emit(ctx, r.newEvent(event.ToolRejected, st, st.run, map[string]any{
			// Capped: the schema is the server's for a connector tool, and
			// its property names and enum values reach the problem text.
			"tool": c.Name, "call_id": c.ID, "reason": "schema", "problem": tool.Cap(err.Error(), maxProblemAttr),
		}))
		return errResult(fmt.Sprintf("invalid arguments for %s: %v", c.Name, err)), 0, nil
	}

	key := c.Name + "\x00" + canonicalArgs(args)
	st.calls[key]++
	if n := st.calls[key]; n >= loopThreshold {
		r.emit(ctx, r.newEvent(event.RunLoopDetected, st, st.run, map[string]any{
			"tool": c.Name, "call_id": c.ID, "count": n,
		}))
		if st.loopHit && !st.final {
			r.enterFinal(ctx, st, StopLoopDetected, event.RunLoopDetected, map[string]any{
				"tool": c.Name, "reason": "repeated",
			})
		}
		st.loopHit = true
		return errResult(fmt.Sprintf(
			"You already called %s with the same arguments %d times. Use the previous result or answer.",
			c.Name, n-1)), 0, nil
	}
	st.invalid = 0

	call := st.in.Call
	call.CallID = c.ID
	call.Name = c.Name
	call.Args = json.RawMessage(args)
	call.TraceID = st.in.TraceID
	call.SessionID = st.in.SessionID
	call.Tainted = call.Tainted || st.taintTTL > 0
	if st.taintTTL > 0 {
		st.taintTTL--
	}

	r.emit(ctx, r.newEvent(event.ToolCallEvent, st, st.run, map[string]any{
		"tool": c.Name, "call_id": c.ID, "args_bytes": len(args),
		"tainted": call.Tainted, "args": cap2k(args),
	}))
	start := r.now()
	res, err := r.Tools.Execute(ctx, call)
	attrs := map[string]any{
		"tool": c.Name, "call_id": c.ID,
		"is_error": res.IsError, "tainted": res.Tainted, "result_bytes": len(res.Text),
	}
	if err != nil {
		if ctx.Err() != nil {
			return tool.Result{}, 0, err
		}
		// An internal tool failure is fed back so the model can recover.
		res = errResult(fmt.Sprintf("tool %s failed: %v", c.Name, err))
		attrs["is_error"] = true
		attrs["error"] = err.Error()
		attrs["result_bytes"] = len(res.Text)
	}
	if conn != "" {
		attrs["connector"] = conn
	}
	score := 0.0
	if res.Tainted {
		if trusted {
			// A trusted connector's result stays wrapped as untrusted but is
			// not scored, so it never arms the taint (docs/connectors.md §6).
			attrs["taint_trusted"] = true
		} else {
			score = r.score(ctx, res.Text, attrs)
		}
		attrs["taint_score"] = round2(score)
		attrs["taint_armed"] = score >= st.taintThr
	}
	ev := r.newEvent(event.ToolResult, st, st.run, attrs)
	ev.Duration = r.now().Sub(start)
	r.emit(ctx, ev)
	return res, score, nil
}

// connector reports the connector id of the tool behind name, empty for any
// other tool, and whether the tool's results are trusted.
func (r *Runner) connector(name string) (string, bool) {
	lu, ok := r.Tools.(tool.Lookuper)
	if !ok {
		return "", false
	}
	t, found := lu.Lookup(name)
	if !found {
		return "", false
	}
	id := ""
	if ct, ok := t.(tool.ConnectorTool); ok {
		id = ct.Connector()
	}
	tt, ok := t.(tool.TrustedTool)
	return id, ok && tt.Trusted()
}

// score grades one untrusted result. Everything that is not a clear answer is
// a 1: no scorer (the deployment has no decision model), an error, a timeout.
// Only the error's kind reaches the trace — the message could quote the very
// text being judged, and a trace is read by people.
//
// A scorer whose questions were answered puts their usage on the result's
// attrs — after an error too, for the answers that did arrive — which is how
// tool.result counts towards a trace's total (event.UsageTypes). No question
// answered, no usage attributes: nothing was paid for.
func (r *Runner) score(ctx context.Context, text string, attrs map[string]any) float64 {
	if r.Taint == nil {
		return 1
	}
	v, err := r.Taint.Score(ctx, text)
	event.PutUsage(attrs, v.Usage)
	if err != nil {
		kind := provider.KindOf(err)
		if kind == "" {
			kind = "unknown"
		}
		attrs["taint_error"] = string(kind)
		return 1
	}
	return v.Score
}

// NoReply is the answer that sends nothing. A group with reply = "always"
// starts a turn for every message; NoReplyNote, injected when none of the
// turn's messages mentions the bot, lets the model pass on chatter.
const (
	NoReply     = "NO_REPLY"
	NoReplyNote = "note: none of the new messages mentions you. If they do not need an answer from you, reply with exactly " +
		NoReply + " and nothing else; it is not sent."
)

// Passed reports whether reply is NoReply: stored, never delivered.
func Passed(reply event.Message) bool {
	return len(reply.Attachments) == 0 && strings.TrimSpace(reply.Text) == NoReply
}

// Silent reports a reply with nothing to send: a turn that only reacted
// (tool.Result.Reacted) stores an empty answer, never delivered.
func Silent(reply event.Message) bool {
	return len(reply.Attachments) == 0 && strings.TrimSpace(reply.Text) == ""
}

// finish appends the final assistant message and records reply.
func (r *Runner) finish(ctx context.Context, st *turnState, text string) error {
	m := r.message(st, event.RoleAssistant, text)
	m.Attachments = st.attached
	m.Tainted = st.taintEver
	m.TaintScore = st.taintMax
	if err := r.append(ctx, st, m); err != nil {
		return err
	}
	st.reply = m
	ev := r.newEvent(event.Reply, st, st.run, map[string]any{
		"text_bytes": len(text), "attachments": len(m.Attachments),
	})
	ev.MessageID = m.ID
	r.emit(ctx, ev)
	return nil
}

// abort ends the turn on context cancellation. Whatever was appended stays;
// a partial assistant text is stored with a marker.
func (r *Runner) abort(ctx context.Context, st *turnState, cause error) error {
	ctx = context.WithoutCancel(ctx)
	st.stop = StopAborted
	if partial := strings.TrimSpace(st.partial.String()); partial != "" {
		m := r.message(st, event.RoleAssistant, partial+" [aborted]")
		m.Tainted = st.taintEver
		m.TaintScore = st.taintMax
		if err := r.append(ctx, st, m); err == nil {
			st.reply = m
		}
	}
	r.emit(ctx, r.newEvent(event.RunAborted, st, st.run, map[string]any{
		"steps": st.steps, "cause": cause.Error(), "partial": st.partial.Len() > 0,
	}))
	return cause
}

// enterFinal withdraws the tools and asks for one last answer.
func (r *Runner) enterFinal(ctx context.Context, st *turnState, stop StopReason, t event.Type, attrs map[string]any) {
	st.withdrawTools(stop, noteFinalAnswer)
	r.emit(ctx, r.newEvent(t, st, st.run, attrs))
}

// withdrawTools takes the tools away for the rest of the turn and leaves a
// note saying why.
func (st *turnState) withdrawTools(stop StopReason, note string) {
	st.final = true
	st.stop = stop
	st.note(note)
}

// note appends a prompt-only user note. Notes steer the next step and are
// never persisted in the transcript.
func (st *turnState) note(text string) {
	st.history = append(st.history, event.Message{
		Schema:    event.SchemaVersion,
		SessionID: st.in.SessionID,
		TraceID:   st.in.TraceID,
		Role:      event.RoleUser,
		Text:      text,
	})
}

// message builds a transcript message with fresh ids and the injected clock.
func (r *Runner) message(st *turnState, role event.Role, text string) event.Message {
	now := r.now()
	return event.Message{
		Schema:    event.SchemaVersion,
		ID:        event.NewIDAt(now),
		SessionID: st.in.SessionID,
		TraceID:   st.in.TraceID,
		Role:      role,
		Text:      text,
		CreatedAt: now.UTC(),
	}
}

// append stores a message and adds it to the working history.
func (r *Runner) append(ctx context.Context, st *turnState, m event.Message) error {
	if r.Transcript != nil {
		if _, err := r.Transcript.AppendMessage(ctx, m, false); err != nil {
			return fmt.Errorf("agent: append %s message: %w", m.Role, err)
		}
	}
	st.history = append(st.history, m)
	st.newMsgs = append(st.newMsgs, m)
	return nil
}

// spec resolves the schema of a tool the model may call. Only tools offered
// in this turn are known.
func (r *Runner) spec(st *turnState, name string) (provider.ToolSpec, bool) {
	s, ok := st.specs[name]
	if !ok {
		return provider.ToolSpec{}, false
	}
	if lu, has := r.Tools.(tool.Lookuper); has {
		if t, found := lu.Lookup(name); found {
			return t.Spec(), true
		}
	}
	return s, true
}

func (r *Runner) newEvent(t event.Type, st *turnState, parent event.ID, attrs map[string]any) event.Event {
	return event.New(t, st.in.TraceID, st.in.SessionID, parent, attrs)
}

func (r *Runner) emit(ctx context.Context, e event.Event) {
	if r.Events != nil {
		r.Events.Emit(ctx, e)
	}
}

func errResult(text string) tool.Result { return tool.Result{Text: text, IsError: true} }

// anyTainted reports whether the history window carries untrusted content.
// That is provenance: what decides wrapping and what a summary inherits.
func anyTainted(msgs []event.Message) bool {
	for _, m := range msgs {
		if m.Tainted {
			return true
		}
	}
	return false
}

// armedBy reports whether the window holds untrusted content that scored as
// steering. That is escalation, and it is a different question from
// anyTainted: a browsed price is untrusted forever and arms nothing.
func armedBy(msgs []event.Message, thr float64) bool {
	return maxTaintScore(msgs) >= thr
}

// maxTaintScore is the highest taint score in the window, 0 for none.
func maxTaintScore(msgs []event.Message) float64 {
	var max float64
	for _, m := range msgs {
		if m.TaintScore > max {
			max = m.TaintScore
		}
	}
	return max
}

// round2 trims a score to two decimals for a trace attribute.
func round2(f float64) float64 { return math.Round(f*100) / 100 }

func truncateMark(text string) string {
	return strings.TrimRight(text, " \t\n") + " […]"
}

func cap2k(s string) string {
	if len(s) <= maxArgsAttr {
		return s
	}
	return s[:maxArgsAttr] + "…"
}

func specNames(specs []provider.ToolSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}
