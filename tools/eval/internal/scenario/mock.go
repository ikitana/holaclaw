package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

// Call is one tool call the model actually made, as the harness saw it.
type Call struct {
	Name string
	Args map[string]any
	Raw  string
}

// Executor is a tool.Executor built from the mocked tools of one scenario. It
// records every call so assertions can be checked afterwards. It is used by
// one Runner at a time, like the Runner itself.
type Executor struct {
	specs []provider.ToolSpec
	tools map[string]*mockTool
	calls []Call
}

var (
	_ tool.Executor = (*Executor)(nil)
	_ tool.Lookuper = (*Executor)(nil)
)

// NewExecutor builds the mocked tool set of a scenario.
func NewExecutor(s Scenario) (*Executor, error) {
	e := &Executor{tools: map[string]*mockTool{}}
	for _, t := range s.Tools {
		risk, err := riskOf(t.Risk)
		if err != nil {
			return nil, fmt.Errorf("eval: tool %s: %w", t.Name, err)
		}
		m := &mockTool{def: t, risk: risk}
		e.tools[t.Name] = m
		e.specs = append(e.specs, t.spec())
	}
	return e, nil
}

// Specs lists the schemas offered to the model.
func (e *Executor) Specs() []provider.ToolSpec { return e.specs }

// Execute answers a call from the canned responses and records it.
func (e *Executor) Execute(ctx context.Context, c tool.Call) (tool.Result, error) {
	rec := Call{Name: c.Name, Raw: string(c.Args)}
	_ = json.Unmarshal(c.Args, &rec.Args)
	e.calls = append(e.calls, rec)
	m, ok := e.tools[c.Name]
	if !ok {
		return tool.Result{Text: fmt.Sprintf("unknown tool %q", c.Name), IsError: true}, nil
	}
	return m.Run(ctx, c)
}

// Lookup exposes the mock behind a name (the loop reads risk and schema).
func (e *Executor) Lookup(name string) (tool.Tool, bool) {
	m, ok := e.tools[name]
	if !ok {
		return nil, false
	}
	return m, true
}

// Calls returns the calls recorded so far, in order.
func (e *Executor) Calls() []Call { return e.calls }

// Reset drops the recorded calls; the harness does this between turns.
func (e *Executor) Reset() { e.calls = nil }

// mockTool answers from the scenario's canned responses.
type mockTool struct {
	def  Tool
	risk tool.Risk
}

func (m *mockTool) Spec() provider.ToolSpec { return m.def.spec() }
func (m *mockTool) Risk() tool.Risk         { return m.risk }

func (m *mockTool) Run(_ context.Context, c tool.Call) (tool.Result, error) {
	var args map[string]any
	_ = json.Unmarshal(c.Args, &args)
	r, ok := Pick(m.def.Responses, args)
	if !ok {
		return tool.Result{
			Text:    fmt.Sprintf("mock %s has no response for these arguments", m.def.Name),
			IsError: true,
		}, nil
	}
	return tool.Result{Text: r.Text, IsError: r.IsError, Tainted: r.Tainted}, nil
}

// Pick chooses the response for a set of arguments: the first conditional
// response that matches, else the first unconditional one.
func Pick(rs []Response, args map[string]any) (Response, bool) {
	for _, r := range rs {
		if len(r.When) > 0 && Matches(r.When, args) {
			return r, true
		}
	}
	for _, r := range rs {
		if len(r.When) == 0 {
			return r, true
		}
	}
	return Response{}, false
}

// Matches reports whether every condition holds for args. A condition value
// starting with "~" is a case-insensitive substring test, anything else an
// exact match on the argument rendered as text.
func Matches(when map[string]string, args map[string]any) bool {
	for k, want := range when {
		v, ok := args[k]
		if !ok {
			return false
		}
		got := render(v)
		if sub, isSub := strings.CutPrefix(want, "~"); isSub {
			if !strings.Contains(strings.ToLower(got), strings.ToLower(sub)) {
				return false
			}
			continue
		}
		if got != want {
			return false
		}
	}
	return true
}

// render turns a JSON argument value into the text conditions compare with.
func render(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}
