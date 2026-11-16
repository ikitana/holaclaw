package tool

import (
	"context"
	"strings"
	"testing"

	"holaclaw/internal/provider"
)

// specsOnly offers a spec it cannot look up, the way a misbehaving executor
// might: Merge must not let that name run past the gate.
type specsOnly struct {
	*countingExec
	extra string
}

func (e *specsOnly) Specs() []provider.ToolSpec {
	return append(e.countingExec.Specs(), fakeTool{name: e.extra}.Spec())
}

func TestMergeDispatchesToTheOwner(t *testing.T) {
	builtin := newInner(fakeTool{name: "fetch", risk: RiskExternal})
	conn := newInner(fakeTool{name: "gh.create_issue", risk: RiskExternal}, fakeTool{name: "gh.list_issues", risk: RiskRead})
	m, err := Merge(builtin, conn)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if got := len(m.Specs()); got != 3 {
		t.Fatalf("specs = %d, want 3", got)
	}
	if m.Specs()[0].Name != "fetch" {
		t.Fatalf("first spec = %q, want the first executor's tools first", m.Specs()[0].Name)
	}

	res, err := m.Execute(context.Background(), Call{Name: "gh.create_issue"})
	if err != nil || res.Text != "ran gh.create_issue" {
		t.Fatalf("Execute = %q, %v", res.Text, err)
	}
	if builtin.count() != 0 || conn.count() != 1 {
		t.Fatalf("runs: builtin %d, connector %d", builtin.count(), conn.count())
	}

	lk, ok := m.(Lookuper)
	if !ok {
		t.Fatal("merged executor must implement Lookuper")
	}
	if tl, ok := lk.Lookup("gh.list_issues"); !ok || tl.Risk() != RiskRead {
		t.Fatalf("Lookup = %v, %v", tl, ok)
	}
	if _, ok := lk.Lookup("nope"); ok {
		t.Fatal("Lookup invented a tool")
	}
}

func TestMergeUnknownToolRunsNothing(t *testing.T) {
	a := newInner(fakeTool{name: "a", risk: RiskRead})
	m, err := Merge(a)
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.Execute(context.Background(), Call{Name: "b"})
	if err != nil || !res.IsError || !strings.Contains(res.Text, "unknown tool") {
		t.Fatalf("Execute = %+v, %v", res, err)
	}
	if a.count() != 0 {
		t.Fatal("an unknown name reached an executor")
	}
}

func TestMergeRejectsDuplicates(t *testing.T) {
	a := newInner(fakeTool{name: "gh.x", risk: RiskRead})
	b := newInner(fakeTool{name: "gh.x", risk: RiskExec})
	if _, err := Merge(a, b); err == nil || !strings.Contains(err.Error(), "gh.x") {
		t.Fatalf("Merge = %v, want a duplicate error naming the tool", err)
	}
}

func TestMergeRequiresLookup(t *testing.T) {
	if _, err := Merge(newInner(), &bareExec{}); err == nil {
		t.Fatal("Merge accepted an executor without tool metadata")
	}
}

func TestMergeNeverRunsWhatItCannotLookUp(t *testing.T) {
	e := &specsOnly{countingExec: newInner(fakeTool{name: "a", risk: RiskRead}), extra: "ghost"}
	m, err := Merge(e)
	if err != nil {
		t.Fatal(err)
	}
	// Through the gate: the gate sees "ghost" as unknown and passes it on, and
	// the merged executor must still refuse it.
	g := NewGate(m, GateConfig{Policy: Policy{Default: classes(Deny, Deny, Deny, Deny)}})
	res, err := g.Execute(context.Background(), Call{Name: "ghost"})
	if err != nil || !res.IsError || !strings.Contains(res.Text, "unknown tool") {
		t.Fatalf("Execute = %+v, %v", res, err)
	}
	if e.count() != 0 {
		t.Fatal("a tool without metadata ran")
	}
}

func TestMergeUnderGateKeepsPolicy(t *testing.T) {
	conn := newInner(fakeTool{name: "gh.delete_repo", risk: RiskExec})
	m, err := Merge(newInner(fakeTool{name: "fetch", risk: RiskExternal}), conn)
	if err != nil {
		t.Fatal(err)
	}
	g := NewGate(m, GateConfig{Policy: Policy{Default: classes(Allow, Allow, Allow, Deny)}})
	res, _ := g.Execute(context.Background(), Call{Name: "gh.delete_repo"})
	if !res.IsError || conn.count() != 0 {
		t.Fatalf("exec-class connector tool ran under a deny: %q", res.Text)
	}
}

func TestMergeEmpty(t *testing.T) {
	m, err := Merge()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Specs()) != 0 {
		t.Fatalf("specs = %v", m.Specs())
	}
}
