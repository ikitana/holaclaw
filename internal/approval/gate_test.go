package approval

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

// The gate and the approval service are written against the interfaces in
// package tool; these tests wire them together over a real SQLite database.

type shTool struct{ runs *int32 }

func (s shTool) Spec() provider.ToolSpec {
	return provider.ToolSpec{Name: "sh", Description: "run", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (s shTool) Risk() tool.Risk { return tool.RiskExec }
func (s shTool) Run(context.Context, tool.Call) (tool.Result, error) {
	atomic.AddInt32(s.runs, 1)
	return tool.Result{Text: "ok"}, nil
}

type innerExec struct {
	t    shTool
	runs int32
}

func (e *innerExec) Specs() []provider.ToolSpec { return []provider.ToolSpec{e.t.Spec()} }
func (e *innerExec) Execute(ctx context.Context, c tool.Call) (tool.Result, error) {
	return e.t.Run(ctx, c)
}
func (e *innerExec) Lookup(name string) (tool.Tool, bool) {
	if name != "sh" {
		return nil, false
	}
	return e.t, true
}

// humanApprover stands in for the GUI: it counts prompts and answers each one
// the way the test says.
type humanApprover struct {
	svc     *Service
	approve bool
	choice  tool.GrantScope
	prompts int32
}

func (h *humanApprover) Request(ctx context.Context, a tool.Approval) (tool.Approval, error) {
	atomic.AddInt32(&h.prompts, 1)
	a.ID = event.NewID()
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := h.svc.Resolve(context.Background(), a.ID, h.approve, h.choice, "owner"); err == nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return h.svc.Request(ctx, a)
}

func execCommand(raw json.RawMessage) (string, error) {
	var v struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	return v.Command, nil
}

func grantPattern(a tool.Approval) string {
	cmd, err := execCommand(json.RawMessage(a.Args))
	if err != nil {
		return ""
	}
	return cmd
}

type wiring struct {
	svc      *Service
	inner    *innerExec
	gate     tool.Executor
	approver *humanApprover
}

func wire(t *testing.T, approve bool, choice tool.GrantScope) *wiring {
	t.Helper()
	svc, _ := newService(t, WithGrantPattern(grantPattern), WithTimeout(5*time.Second))
	inner := &innerExec{}
	inner.t = shTool{runs: &inner.runs}
	ap := &humanApprover{svc: svc, approve: approve, choice: choice}
	g := tool.NewGate(inner, tool.GateConfig{
		Policy: tool.Policy{
			Default: tool.Classes{Read: tool.Allow, Write: tool.Allow, External: tool.Allow, Exec: tool.Ask},
		},
		Grants:      svc,
		Approver:    ap,
		Events:      event.Discard,
		ExecCommand: execCommand,
	})
	return &wiring{svc: svc, inner: inner, gate: g, approver: ap}
}

func call(session event.ID, cmd string) tool.Call {
	return tool.Call{
		Name: "sh", Agent: "a", SessionID: session, TraceID: "tr",
		UserID: "owner", Role: "owner", // the approver: an "always" grant covers only them
		Args: json.RawMessage(`{"command":"` + cmd + `"}`),
	}
}

func TestGateApprovalOnce(t *testing.T) {
	w := wire(t, true, tool.GrantOnce)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		res, err := w.gate.Execute(ctx, call("s1", "ls -l"))
		if err != nil || res.IsError {
			t.Fatalf("call %d: %v %q", i, err, res.Text)
		}
	}
	if got := atomic.LoadInt32(&w.approver.prompts); got != 2 {
		t.Fatalf("once must prompt every time, prompts = %d", got)
	}
	if got := atomic.LoadInt32(&w.inner.runs); got != 2 {
		t.Fatalf("runs = %d", got)
	}
	if gs, _ := w.svc.ListGrants(ctx, "a"); len(gs) != 0 {
		t.Fatalf("once persisted a grant: %+v", gs)
	}
}

func TestGateApprovalSession(t *testing.T) {
	w := wire(t, true, tool.GrantSession)
	ctx := context.Background()

	if res, _ := w.gate.Execute(ctx, call("s1", "ls -l")); res.IsError {
		t.Fatalf("first call: %q", res.Text)
	}
	// Same session, same command: answered by the persisted grant.
	if res, _ := w.gate.Execute(ctx, call("s1", "ls -l")); res.IsError {
		t.Fatalf("second call: %q", res.Text)
	}
	if got := atomic.LoadInt32(&w.approver.prompts); got != 1 {
		t.Fatalf("session grant should stop the second prompt, prompts = %d", got)
	}
	// A different command in the same session is outside the pattern.
	if res, _ := w.gate.Execute(ctx, call("s1", "rm -rf /")); res.IsError {
		t.Fatalf("third call: %q", res.Text)
	}
	if got := atomic.LoadInt32(&w.approver.prompts); got != 2 {
		t.Fatalf("a different command must prompt, prompts = %d", got)
	}
	// Another session is not covered.
	if res, _ := w.gate.Execute(ctx, call("s2", "ls -l")); res.IsError {
		t.Fatalf("fourth call: %q", res.Text)
	}
	if got := atomic.LoadInt32(&w.approver.prompts); got != 3 {
		t.Fatalf("session grant leaked to another session, prompts = %d", got)
	}
}

func TestGateApprovalAlways(t *testing.T) {
	w := wire(t, true, tool.GrantAlways)
	ctx := context.Background()

	if res, _ := w.gate.Execute(ctx, call("s1", "ls -l")); res.IsError {
		t.Fatalf("first call: %q", res.Text)
	}
	if res, _ := w.gate.Execute(ctx, call("s2", "ls -l")); res.IsError {
		t.Fatalf("other session: %q", res.Text)
	}
	if got := atomic.LoadInt32(&w.approver.prompts); got != 1 {
		t.Fatalf("always should cover any session, prompts = %d", got)
	}
	if got := atomic.LoadInt32(&w.inner.runs); got != 2 {
		t.Fatalf("runs = %d", got)
	}
	// A grant never lifts a metacharacter command back into Allow.
	if res, _ := w.gate.Execute(ctx, call("s1", "ls -l; rm -rf /")); res.IsError {
		t.Fatalf("call: %q", res.Text)
	}
	if got := atomic.LoadInt32(&w.approver.prompts); got != 2 {
		t.Fatalf("a chained command must prompt, prompts = %d", got)
	}
}

func TestGateApprovalDenied(t *testing.T) {
	w := wire(t, false, "")
	res, err := w.gate.Execute(context.Background(), call("s1", "ls -l"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "approval denied") {
		t.Fatalf("got %q", res.Text)
	}
	if got := atomic.LoadInt32(&w.inner.runs); got != 0 {
		t.Fatalf("a denied call ran the tool %d times", got)
	}
}

func TestGateApprovalTimeout(t *testing.T) {
	svc, _ := newService(t, WithTimeout(40*time.Millisecond))
	inner := &innerExec{}
	inner.t = shTool{runs: &inner.runs}
	g := tool.NewGate(inner, tool.GateConfig{
		Policy: tool.Policy{Default: tool.Classes{Read: tool.Allow, Exec: tool.Ask}},
		Grants: svc, Approver: svc, ExecCommand: execCommand,
	})
	res, err := g.Execute(context.Background(), call("s1", "ls -l"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "approval timed out") {
		t.Fatalf("got %q", res.Text)
	}
	if got := atomic.LoadInt32(&inner.runs); got != 0 {
		t.Fatalf("a timed-out call ran the tool %d times", got)
	}
}

// A detached task never prompts: ask = deny unless the task carries a grant.
func TestGateDetachedTask(t *testing.T) {
	svc, _ := newService(t)
	inner := &innerExec{}
	inner.t = shTool{runs: &inner.runs}
	ap := &humanApprover{svc: svc, approve: true, choice: tool.GrantOnce}
	g := tool.NewGate(inner, tool.GateConfig{
		Policy: tool.Policy{
			Default: tool.Classes{Read: tool.Allow, Exec: tool.Allow},
			Task:    tool.Classes{Read: tool.Allow, Exec: tool.Ask},
		},
		Grants: svc, Approver: ap, ExecCommand: execCommand,
	})
	ctx := context.Background()
	c := call("s1", "ls -l")
	c.RunKind = tool.RunTask
	c.TaskID = "task1"

	res, _ := g.Execute(ctx, c)
	if !res.IsError || !strings.Contains(res.Text, "detached run without grant") {
		t.Fatalf("got %q", res.Text)
	}
	if got := atomic.LoadInt32(&ap.prompts); got != 0 {
		t.Fatalf("a detached run must never prompt, prompts = %d", got)
	}

	// With the grant set at task creation it runs.
	if _, err := svc.AddGrant(ctx, tool.Grant{
		Agent: "a", Tool: "sh", Scope: tool.GrantTask, TaskID: "task1", Pattern: "ls",
	}); err != nil {
		t.Fatal(err)
	}
	if res, _ := g.Execute(ctx, c); res.IsError {
		t.Fatalf("granted task call: %q", res.Text)
	}
	if got := atomic.LoadInt32(&inner.runs); got != 1 {
		t.Fatalf("runs = %d", got)
	}
	// The same grant does not reach another task.
	c.TaskID = "task2"
	if res, _ := g.Execute(ctx, c); !res.IsError {
		t.Fatal("a task grant leaked to another task")
	}
}

// Two sessions suspended at once: resolving one leaves the other waiting, and
// neither tool call runs before its own approval.
func TestGateTwoSessionsSuspendIndependently(t *testing.T) {
	svc, _ := newService(t, WithTimeout(5*time.Second))
	inner := &innerExec{}
	inner.t = shTool{runs: &inner.runs}
	g := tool.NewGate(inner, tool.GateConfig{
		Policy: tool.Policy{Default: tool.Classes{Read: tool.Allow, Exec: tool.Ask}},
		Grants: svc, Approver: svc, ExecCommand: execCommand,
	})

	var wg sync.WaitGroup
	results := make([]tool.Result, 2)
	started := make(chan struct{}, 2)
	for i, sess := range []event.ID{"s1", "s2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			started <- struct{}{}
			results[i], _ = g.Execute(context.Background(), call(sess, "ls -l"))
		}()
	}
	<-started
	<-started
	pending := waitPending(t, svc, 2)

	if _, err := svc.Resolve(context.Background(), pending[0].ID, true, tool.GrantOnce, "owner"); err != nil {
		t.Fatal(err)
	}
	// The other call is still blocked, so exactly one execution has happened.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&inner.runs) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt32(&inner.runs); got != 1 {
		t.Fatalf("runs = %d, want exactly the approved one", got)
	}
	if p, _ := svc.ListPending(context.Background()); len(p) != 1 {
		t.Fatalf("pending = %d, want the second session still suspended", len(p))
	}
	if _, err := svc.Resolve(context.Background(), pending[1].ID, false, "", "owner"); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if atomic.LoadInt32(&inner.runs) != 1 {
		t.Fatalf("the denied call ran anyway: %d", inner.runs)
	}
	errs := 0
	for _, r := range results {
		if r.IsError {
			errs++
		}
	}
	if errs != 1 {
		t.Fatalf("want exactly one denial, got %d", errs)
	}
}

// An "always" grant is bound to the person who approved it: another user of
// the same agent is asked again.
func TestGateApprovalAlwaysDoesNotCoverOtherUsers(t *testing.T) {
	w := wire(t, true, tool.GrantAlways)
	ctx := context.Background()
	if res, _ := w.gate.Execute(ctx, call("s1", "ls -l")); res.IsError {
		t.Fatalf("first call: %q", res.Text)
	}
	other := call("s2", "ls -l")
	other.UserID, other.Role = "someone-else", "user"
	if res, _ := w.gate.Execute(ctx, other); res.IsError {
		t.Fatalf("other user: %q", res.Text)
	}
	if got := atomic.LoadInt32(&w.approver.prompts); got != 2 {
		t.Fatalf("another user must be asked again, prompts = %d", got)
	}
}
