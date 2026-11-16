package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

// step scripts one provider call: the deltas it streams, the response it
// finishes with, or the error it fails with.
type step struct {
	deltas []string
	resp   *provider.Response
	err    error // from Chat when there are no deltas, else from the stream
	before func()
	during func() // after the first delta
}

type fakeProvider struct {
	name  string
	steps []step
	n     int
	reqs  []provider.Request
}

func (p *fakeProvider) Name() string { return p.name }

func (p *fakeProvider) Chat(ctx context.Context, req provider.Request) (provider.Stream, error) {
	p.reqs = append(p.reqs, req)
	if p.n >= len(p.steps) {
		return nil, fmt.Errorf("fake provider %s: unscripted call %d", p.name, p.n+1)
	}
	s := p.steps[p.n]
	p.n++
	if s.before != nil {
		s.before()
	}
	if s.err != nil && len(s.deltas) == 0 {
		return nil, s.err
	}
	return &fakeStream{step: s, ctx: ctx}, nil
}

type fakeStream struct {
	step step
	ctx  context.Context
	i    int
	done bool
}

func (s *fakeStream) Next() (provider.Chunk, error) {
	// Real providers abort on cancellation; so does this one.
	if err := s.ctx.Err(); err != nil {
		return provider.Chunk{}, err
	}
	if s.i < len(s.step.deltas) {
		d := s.step.deltas[s.i]
		s.i++
		if s.i == 1 && s.step.during != nil {
			s.step.during()
		}
		return provider.Chunk{Kind: provider.ChunkText, Text: d}, nil
	}
	if s.step.err != nil {
		return provider.Chunk{}, s.step.err
	}
	if s.done {
		return provider.Chunk{}, io.EOF
	}
	s.done = true
	return provider.Chunk{Kind: provider.ChunkDone, Response: s.step.resp}, nil
}

func (s *fakeStream) Close() error { return nil }

func textStep(text string) step {
	return step{deltas: []string{text}, resp: &provider.Response{Text: text, Finish: provider.FinishStop}}
}

func callStep(calls ...provider.ToolCall) step {
	return step{resp: &provider.Response{Finish: provider.FinishToolCalls, ToolCalls: calls}}
}

func call(id, name, args string) provider.ToolCall {
	return provider.ToolCall{ID: id, Name: name, Arguments: args}
}

// fakeTool carries a spec only; the fake executor answers for it.
type fakeTool struct{ spec provider.ToolSpec }

func (t fakeTool) Spec() provider.ToolSpec { return t.spec }
func (t fakeTool) Risk() tool.Risk         { return tool.RiskRead }
func (t fakeTool) Run(context.Context, tool.Call) (tool.Result, error) {
	return tool.Result{}, nil
}

type fakeExec struct {
	specs []provider.ToolSpec
	calls []tool.Call
	// fn answers a call; nil means a plain ok result.
	fn func(c tool.Call) (tool.Result, error)
}

func newExec(specs ...provider.ToolSpec) *fakeExec { return &fakeExec{specs: specs} }

func (e *fakeExec) Specs() []provider.ToolSpec { return e.specs }

func (e *fakeExec) Execute(_ context.Context, c tool.Call) (tool.Result, error) {
	e.calls = append(e.calls, c)
	if e.fn != nil {
		return e.fn(c)
	}
	return tool.Result{Text: "ok:" + c.Name}, nil
}

func (e *fakeExec) Lookup(name string) (tool.Tool, bool) {
	for _, s := range e.specs {
		if s.Name == name {
			return fakeTool{spec: s}, true
		}
	}
	return nil, false
}

func (e *fakeExec) names() []string {
	out := make([]string, 0, len(e.calls))
	for _, c := range e.calls {
		out = append(out, c.Name)
	}
	return out
}

func spec(name, schema string) provider.ToolSpec {
	return provider.ToolSpec{Name: name, Description: name, Parameters: json.RawMessage(schema)}
}

// objectSchema is a permissive schema used where validation is not the point.
const objectSchema = `{"type":"object"}`

// fakeBuilder passes the history through as provider messages.
type fakeBuilder struct {
	builds   int
	last     BuildInput
	compact  bool
	buildErr error
}

func (b *fakeBuilder) Build(_ context.Context, in BuildInput) (Prompt, error) {
	b.builds++
	b.last = in
	if b.buildErr != nil {
		return Prompt{}, b.buildErr
	}
	p := Prompt{
		System:          "sys",
		Tools:           in.Tools,
		PrefixHash:      "deadbeef",
		NeedsCompaction: b.compact,
		Breakdown:       Breakdown{Window: in.Model.Context, ReplyReserve: 256, Total: 100},
	}
	for _, m := range in.History {
		p.MessageIDs = append(p.MessageIDs, m.ID)
		role := provider.Role(m.Role)
		pm := provider.Message{Role: role, Parts: []provider.Part{{Text: m.Text}}}
		if m.Role == event.RoleTool {
			pm.ToolCallID, pm.ToolName = m.ToolCallID, m.ToolName
		}
		for _, tc := range m.ToolCalls {
			pm.ToolCalls = append(pm.ToolCalls, provider.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
		}
		p.Messages = append(p.Messages, pm)
	}
	return p, nil
}

type memTranscript struct {
	mu   sync.Mutex
	msgs []event.Message
	err  error
}

func (m *memTranscript) AppendMessage(_ context.Context, msg event.Message, _ bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return false, m.err
	}
	m.msgs = append(m.msgs, msg)
	return true, nil
}

type recorder struct {
	mu  sync.Mutex
	evs []event.Event
}

func (r *recorder) Emit(_ context.Context, e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, e)
}

func (r *recorder) types() []event.Type {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]event.Type, 0, len(r.evs))
	for _, e := range r.evs {
		out = append(out, e.Type)
	}
	return out
}

func (r *recorder) count(t event.Type) int {
	n := 0
	for _, e := range r.types() {
		if e == t {
			n++
		}
	}
	return n
}

func (r *recorder) first(t event.Type) (event.Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.evs {
		if e.Type == t {
			return e, true
		}
	}
	return event.Event{}, false
}

func (r *recorder) attrs(t *testing.T, e event.Event) map[string]any {
	t.Helper()
	var m map[string]any
	if len(e.Attrs) == 0 {
		return m
	}
	if err := json.Unmarshal(e.Attrs, &m); err != nil {
		t.Fatalf("attrs of %s: %v", e.Type, err)
	}
	return m
}

// rig wires a Runner to fakes.
type rig struct {
	prov  *fakeProvider
	tools *fakeExec
	tr    *memTranscript
	ev    *recorder
	b     *fakeBuilder
	r     *Runner
	clock time.Time
}

func newRig(_ *testing.T, steps ...step) *rig {
	g := &rig{
		prov:  &fakeProvider{name: "fake", steps: steps},
		tr:    &memTranscript{},
		ev:    &recorder{},
		b:     &fakeBuilder{},
		clock: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	g.r = &Runner{
		Def:        Definition{Name: "test"},
		Model:      provider.Model{Name: "main", Provider: "fake", ID: "m1", Context: 8192, Tools: true},
		Provider:   g.prov,
		Builder:    g.b,
		Transcript: g.tr,
		Events:     g.ev,
		Now: func() time.Time {
			g.clock = g.clock.Add(time.Millisecond)
			return g.clock
		},
	}
	return g
}

// withTools attaches a fake executor with the given specs.
func (g *rig) withTools(specs ...provider.ToolSpec) *rig {
	g.tools = newExec(specs...)
	g.r.Tools = g.tools
	return g
}

func (g *rig) turn(ctx context.Context) (TurnResult, error) {
	return g.r.Turn(ctx, TurnInput{
		SessionID: "S1",
		TraceID:   "T1",
		History:   []event.Message{{Schema: event.SchemaVersion, ID: "M1", Role: event.RoleUser, Text: "hello"}},
		Call:      tool.Call{Agent: "test", Role: "owner", RunKind: tool.RunInteractive, Workspace: "/ws"},
	})
}

func providerError(kind provider.ErrorKind) error {
	return &provider.Error{Kind: kind, Provider: "fake", Message: string(kind)}
}
