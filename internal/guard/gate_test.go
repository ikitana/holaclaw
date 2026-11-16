package guard

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

// --- doubles -----------------------------------------------------------

// inner is the executor the gate wraps. It counts executions so a test can
// tell "the guard allowed it" from "the guard was never asked".
type inner struct {
	mu    sync.Mutex
	runs  int
	tools map[string]tool.Tool
}

func newInner(risks map[string]tool.Risk) *inner {
	m := map[string]tool.Tool{}
	for name, r := range risks {
		m[name] = namedTool{name: name, risk: r}
	}
	return &inner{tools: m}
}

func (e *inner) Specs() []provider.ToolSpec { return nil }

func (e *inner) Execute(_ context.Context, c tool.Call) (tool.Result, error) {
	e.mu.Lock()
	e.runs++
	e.mu.Unlock()
	return tool.Result{Text: "ran " + c.Name}, nil
}

func (e *inner) Lookup(name string) (tool.Tool, bool) { t, ok := e.tools[name]; return t, ok }

func (e *inner) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runs
}

type namedTool struct {
	name string
	risk tool.Risk
}

func (t namedTool) Spec() provider.ToolSpec {
	return provider.ToolSpec{Name: t.name, Description: "x", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (t namedTool) Risk() tool.Risk { return t.risk }
func (t namedTool) Run(context.Context, tool.Call) (tool.Result, error) {
	return tool.Result{Text: "ran"}, nil
}

// approver records every human question and answers with a fixed status.
type approver struct {
	mu     sync.Mutex
	asked  int
	status tool.ApprovalStatus
	err    error
}

func (a *approver) Request(_ context.Context, ap tool.Approval) (tool.Approval, error) {
	a.mu.Lock()
	a.asked++
	a.mu.Unlock()
	if a.err != nil {
		return tool.Approval{}, a.err
	}
	ap.Status = a.status
	return ap, nil
}

func (a *approver) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.asked
}

// gateWith builds a gate around a real guard in recommend mode (the default).
func gateWith(t *testing.T, p provider.Provider, classes tool.Classes, risks map[string]tool.Risk, ap *approver) (tool.Executor, *inner) {
	t.Helper()
	return gateMode(t, "", p, classes, risks, ap)
}

func gateMode(t *testing.T, mode tool.GuardMode, p provider.Provider, classes tool.Classes, risks map[string]tool.Risk, ap *approver) (tool.Executor, *inner) {
	t.Helper()
	in := newInner(risks)
	g := tool.NewGate(in, tool.GateConfig{
		Policy:   tool.Policy{Default: classes, GuardMode: mode},
		Approver: ap,
		Guard:    newGuard(t, p, nil),
	})
	return g, in
}

func gateCall(name string) tool.Call {
	return tool.Call{
		Name: name, Args: json.RawMessage(`{"a":1}`),
		Agent: "eval", Role: "owner", RunKind: tool.RunInteractive,
	}
}

// --- the four ways a guard can change a call ---------------------------

// Only auto mode lets a guard turn an Ask into an Allow.
func TestGateAskBecomesAllowInAutoMode(t *testing.T) {
	p := &fakeProvider{answers: []string{"allow"}}
	ap := &approver{status: tool.ApprovalApproved}
	g, in := gateMode(t, tool.GuardAuto, p, tool.Classes{Exec: tool.Ask},
		map[string]tool.Risk{"shell": tool.RiskExec}, ap)

	res, err := g.Execute(context.Background(), gateCall("shell"))
	if err != nil || res.IsError {
		t.Fatalf("execute: %v %#v", err, res)
	}
	if in.count() != 1 {
		t.Errorf("tool ran %d times, want 1", in.count())
	}
	if ap.count() != 0 {
		t.Errorf("human asked %d times, want 0: the guard allowed the call", ap.count())
	}
}

// In recommend mode it does not: a guard is advice, never permission.
func TestGateAskStaysAskInRecommendMode(t *testing.T) {
	p := &fakeProvider{answers: []string{"allow"}}
	ap := &approver{status: tool.ApprovalApproved}
	g, in := gateWith(t, p, tool.Classes{Exec: tool.Ask}, map[string]tool.Risk{"shell": tool.RiskExec}, ap)

	res, err := g.Execute(context.Background(), gateCall("shell"))
	if err != nil || res.IsError {
		t.Fatalf("execute: %v %#v", err, res)
	}
	if ap.count() != 1 {
		t.Errorf("human asked %d times, want 1: recommend never lifts an ask", ap.count())
	}
	if in.count() != 1 {
		t.Errorf("tool ran %d times after approval, want 1", in.count())
	}
}

// End to end on the decision backend: an unsafe kind clamps the score below
// the threshold, so auto mode denies without asking anyone.
func TestGateWithDecisionGuard(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resp     *provider.DecisionResponse
		mode     tool.GuardMode
		wantRuns int
		wantAsk  int
	}{
		{"auto allows a benign call", answer(0.95, KindBenign, 0), tool.GuardAuto, 1, 0},
		{"auto denies a destructive call", answer(0.99, KindDestructive, 0), tool.GuardAuto, 0, 0},
		{"recommend asks about a destructive call", answer(0.99, KindDestructive, 0), "", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := newInner(map[string]tool.Risk{"shell": tool.RiskExec})
			ap := &approver{status: tool.ApprovalApproved}
			g := tool.NewGate(in, tool.GateConfig{
				Policy:   tool.Policy{Default: tool.Classes{Exec: tool.Allow}, GuardMode: tc.mode},
				Approver: ap,
				Guard: New(Options{
					Model:   provider.Model{Name: "jev", API: provider.APIDecisions},
					Decider: &fakeDecider{resps: []*provider.DecisionResponse{tc.resp}},
				}),
			})
			if _, err := g.Execute(context.Background(), gateCall("shell")); err != nil {
				t.Fatalf("execute: %v", err)
			}
			if in.count() != tc.wantRuns || ap.count() != tc.wantAsk {
				t.Fatalf("runs=%d asked=%d, want %d/%d", in.count(), ap.count(), tc.wantRuns, tc.wantAsk)
			}
		})
	}
}

func TestGateAllowBecomesAsk(t *testing.T) {
	p := &fakeProvider{answers: []string{"ask"}}
	ap := &approver{status: tool.ApprovalDenied}
	g, in := gateWith(t, p, tool.Classes{Write: tool.Allow}, map[string]tool.Risk{"files.write": tool.RiskWrite}, ap)

	res, err := g.Execute(context.Background(), gateCall("files.write"))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !res.IsError {
		t.Errorf("want the denied-by-policy result, got %#v", res)
	}
	if ap.count() != 1 {
		t.Errorf("human asked %d times, want 1", ap.count())
	}
	if in.count() != 0 {
		t.Errorf("tool ran %d times after a human denial, want 0", in.count())
	}
}

func TestGateGuardErrorAsksTheHuman(t *testing.T) {
	p := &fakeProvider{err: errors.New("guard offline")}
	ap := &approver{status: tool.ApprovalApproved}
	g, in := gateWith(t, p, tool.Classes{External: tool.Allow}, map[string]tool.Risk{"fetch": tool.RiskExternal}, ap)

	if _, err := g.Execute(context.Background(), gateCall("fetch")); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if ap.count() != 1 {
		t.Fatalf("human asked %d times, want 1: a guard failure must fail closed", ap.count())
	}
	if in.count() != 1 {
		t.Errorf("tool ran %d times after approval, want 1", in.count())
	}
}

func TestGateDenyNeverReachesTheGuard(t *testing.T) {
	p := &fakeProvider{answers: []string{"allow"}}
	ap := &approver{status: tool.ApprovalApproved}
	g, in := gateWith(t, p, tool.Classes{Exec: tool.Deny}, map[string]tool.Risk{"shell": tool.RiskExec}, ap)

	res, err := g.Execute(context.Background(), gateCall("shell"))
	if err != nil || !res.IsError {
		t.Fatalf("want a denial, got %#v (%v)", res, err)
	}
	if p.calls() != 0 {
		t.Errorf("guard model called %d times on a denied call, want 0", p.calls())
	}
	if in.count() != 0 || ap.count() != 0 {
		t.Errorf("denied call reached the tool (%d) or a human (%d)", in.count(), ap.count())
	}
}

func TestGateReadRiskNeverReachesTheGuard(t *testing.T) {
	p := &fakeProvider{answers: []string{"ask"}}
	ap := &approver{status: tool.ApprovalApproved}
	g, in := gateWith(t, p, tool.Classes{Read: tool.Allow}, map[string]tool.Risk{"files.read": tool.RiskRead}, ap)

	if _, err := g.Execute(context.Background(), gateCall("files.read")); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if p.calls() != 0 {
		t.Errorf("guard model called %d times for a read, want 0", p.calls())
	}
	if in.count() != 1 {
		t.Errorf("read ran %d times, want 1", in.count())
	}
	if ap.count() != 0 {
		t.Errorf("human asked %d times for an allowed read, want 0", ap.count())
	}
}
