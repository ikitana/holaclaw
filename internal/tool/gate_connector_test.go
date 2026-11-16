package tool

import (
	"context"
	"encoding/json"
	"testing"

	"holaclaw/internal/event"
)

// Connector tools in the gate (docs/connectors.md §6, §10): policy.tools keys
// by connector id, destructiveHint as always-ask, and the connector id on
// every policy.decision.

// connTool is a connector tool: fakeTool plus the optional interfaces.
type connTool struct {
	fakeTool
	conn   string
	always bool
}

func (t connTool) Connector() string { return t.conn }
func (t connTool) AlwaysAsk() bool   { return t.always }

// alwaysExec is a non-connector exec tool marked always-ask, to show the
// allow-list cannot answer it.
type alwaysExec struct{ fakeTool }

func (alwaysExec) AlwaysAsk() bool { return true }

func ghIssue(always bool) connTool {
	return connTool{fakeTool: fakeTool{name: "gh.create_issue", risk: RiskExternal}, conn: "gh", always: always}
}

// run executes one call and reports whether the inner tool ran and how often
// a human was asked.
func run(t *testing.T, tl Tool, cfg GateConfig, c Call) (ran bool, asked int, res Result) {
	t.Helper()
	inner := newInner(tl)
	ap := &fakeApprover{status: ApprovalApproved}
	if cfg.Approver == nil {
		cfg.Approver = ap
	}
	c.Name = tl.Spec().Name
	if c.Agent == "" {
		c.Agent = "a"
	}
	res, err := NewGate(inner, cfg).Execute(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return inner.count() == 1, ap.calls, res
}

func TestOverrideByConnector(t *testing.T) {
	cases := []struct {
		name      string
		tool      Tool
		def       Decision // external / write class
		tools     map[string]Decision
		role      string
		wantRan   bool
		wantAsked int
	}{
		{"id key loosens every tool", ghIssue(false), Ask, map[string]Decision{"gh": Allow}, "owner", true, 0},
		{"id key tightens", ghIssue(false), Allow, map[string]Decision{"gh": Deny}, "owner", false, 0},
		{"exact tool name", ghIssue(false), Allow, map[string]Decision{"gh.create_issue": Ask}, "owner", true, 1},
		{"exact name beats id", ghIssue(false), Allow,
			map[string]Decision{"gh": Deny, "gh.create_issue": Allow}, "owner", true, 0},
		{"id key of another connector", ghIssue(false), Allow, map[string]Decision{"other": Deny}, "owner", true, 0},
		{"id key does not cover a built-in", fakeTool{name: "memory.save", risk: RiskWrite}, Allow,
			map[string]Decision{"memory": Deny}, "owner", true, 0},
		{"class deny beats id", ghIssue(false), Deny, map[string]Decision{"gh": Allow}, "owner", false, 0},
		{"class deny beats exact name", ghIssue(false), Deny,
			map[string]Decision{"gh": Allow, "gh.create_issue": Allow}, "owner", false, 0},
		{"guest id key only tightens", ghIssue(false), Ask, map[string]Decision{"gh": Allow}, RoleGuest, true, 1},
		{"guest id key tightens", ghIssue(false), Allow, map[string]Decision{"gh": Deny}, RoleGuest, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cl := classes(Allow, tc.def, tc.def, Allow)
			p := Policy{Default: cl, Guest: cl, Tools: tc.tools}
			ran, asked, res := run(t, tc.tool, GateConfig{Policy: p}, Call{Role: tc.role})
			if ran != tc.wantRan || asked != tc.wantAsked {
				t.Fatalf("ran=%v asked=%d (%q), want ran=%v asked=%d", ran, asked, res.Text, tc.wantRan, tc.wantAsked)
			}
		})
	}
}

func TestAlwaysAsk(t *testing.T) {
	allowAll := Policy{Default: classes(Allow, Allow, Allow, Allow), Task: classes(Allow, Allow, Allow, Allow)}

	t.Run("allow becomes ask", func(t *testing.T) {
		rec := &recorder{}
		ran, asked, _ := run(t, ghIssue(true), GateConfig{Policy: allowAll, Events: rec}, Call{Role: "owner"})
		if !ran || asked != 1 {
			t.Fatalf("ran=%v asked=%d, want the human's approval", ran, asked)
		}
		if !hasStep(rec, "destructive", Ask, "destructive") {
			t.Fatalf("no destructive step in %v", rec.steps())
		}
	})

	t.Run("an override allow still asks", func(t *testing.T) {
		p := allowAll
		p.Tools = map[string]Decision{"gh": Allow}
		if _, asked, _ := run(t, ghIssue(true), GateConfig{Policy: p}, Call{Role: "owner"}); asked != 1 {
			t.Fatalf("asked=%d", asked)
		}
	})

	t.Run("deny stays deny", func(t *testing.T) {
		p := allowAll
		p.Tools = map[string]Decision{"gh": Deny}
		rec := &recorder{}
		ran, asked, _ := run(t, ghIssue(true), GateConfig{Policy: p, Events: rec}, Call{Role: "owner"})
		if ran || asked != 0 {
			t.Fatalf("ran=%v asked=%d", ran, asked)
		}
		if hasStep(rec, "destructive", Ask, "destructive") {
			t.Fatal("a deny was turned into an ask")
		}
	})

	t.Run("a grant allows it", func(t *testing.T) {
		gr := &fakeGrants{match: true}
		ran, asked, _ := run(t, ghIssue(true), GateConfig{Policy: allowAll, Grants: gr}, Call{Role: "owner"})
		if !ran || asked != 0 || gr.calls != 1 {
			t.Fatalf("ran=%v asked=%d grants=%d", ran, asked, gr.calls)
		}
	})

	t.Run("the exec allow-list does not", func(t *testing.T) {
		p := allowAll
		p.ExecAllow = []string{"git status"}
		tl := alwaysExec{fakeTool{name: "sh", risk: RiskExec}}
		ap := &fakeApprover{status: ApprovalDenied}
		ran, asked, _ := run(t, tl, GateConfig{Policy: p, Approver: ap, ExecCommand: execCommand},
			Call{Role: "owner", Args: json.RawMessage(`{"command":"git status"}`)})
		if ran || asked != 0 || ap.calls != 1 {
			t.Fatalf("allow-listed always-ask call: ran=%v asked=%d", ran, ap.calls)
		}
	})

	t.Run("auto guard with a safe verdict asks", func(t *testing.T) {
		p := allowAll
		p.GuardMode = GuardAuto
		gd := &fakeGuard{v: safe()}
		ran, asked, _ := run(t, ghIssue(true), GateConfig{Policy: p, Guard: gd}, Call{Role: "owner"})
		if !ran || asked != 1 || gd.count() != 1 {
			t.Fatalf("ran=%v asked=%d guard=%d, want a human to decide", ran, asked, gd.count())
		}
	})

	t.Run("auto guard with a safe verdict keeps a grant", func(t *testing.T) {
		p := allowAll
		p.GuardMode = GuardAuto
		cfg := GateConfig{Policy: p, Guard: &fakeGuard{v: safe()}, Grants: &fakeGrants{match: true}}
		if ran, asked, _ := run(t, ghIssue(true), cfg, Call{Role: "owner"}); !ran || asked != 0 {
			t.Fatalf("ran=%v asked=%d", ran, asked)
		}
	})

	t.Run("auto guard with an unsafe verdict denies", func(t *testing.T) {
		p := allowAll
		p.GuardMode = GuardAuto
		if ran, asked, _ := run(t, ghIssue(true), GateConfig{Policy: p, Guard: &fakeGuard{v: unsafe()}}, Call{Role: "owner"}); ran || asked != 0 {
			t.Fatalf("ran=%v asked=%d", ran, asked)
		}
	})

	t.Run("recommend guard with a safe verdict still asks", func(t *testing.T) {
		if ran, asked, _ := run(t, ghIssue(true), GateConfig{Policy: allowAll, Guard: &fakeGuard{v: safe()}}, Call{Role: "owner"}); !ran || asked != 1 {
			t.Fatalf("ran=%v asked=%d", ran, asked)
		}
	})

	t.Run("task run without a grant is denied", func(t *testing.T) {
		p := allowAll
		p.GuardMode = GuardAuto
		ran, asked, res := run(t, ghIssue(true), GateConfig{Policy: p, Guard: &fakeGuard{v: safe()}}, Call{RunKind: RunTask})
		if ran || asked != 0 || !res.IsError {
			t.Fatalf("ran=%v asked=%d %q", ran, asked, res.Text)
		}
	})

	t.Run("task run with a grant runs", func(t *testing.T) {
		cfg := GateConfig{Policy: allowAll, Grants: &fakeGrants{match: true}}
		if ran, _, res := run(t, ghIssue(true), cfg, Call{RunKind: RunTask}); !ran {
			t.Fatalf("granted task call denied: %q", res.Text)
		}
	})

	t.Run("AlwaysAsk false changes nothing", func(t *testing.T) {
		p := allowAll
		p.GuardMode = GuardAuto
		rec := &recorder{}
		ran, asked, _ := run(t, ghIssue(false), GateConfig{Policy: p, Guard: &fakeGuard{v: safe()}, Events: rec}, Call{Role: "owner"})
		if !ran || asked != 0 {
			t.Fatalf("ran=%v asked=%d", ran, asked)
		}
		if hasStep(rec, "destructive", Ask, "destructive") {
			t.Fatal("destructive step without the hint")
		}
	})
}

func hasStep(rec *recorder, step string, d Decision, reason string) bool {
	for _, e := range rec.all() {
		var a decisionAttrs
		if json.Unmarshal(e.Attrs, &a) == nil && a.Step == step && a.Decision == d && a.Reason == reason {
			return true
		}
	}
	return false
}

// Every policy.decision and guard.decision of a connector call carries its id;
// no event of another tool has the key at all.
func TestDecisionEventsCarryConnector(t *testing.T) {
	p := Policy{Default: classes(Allow, Allow, Allow, Allow), Tools: map[string]Decision{"gh": Allow}}
	cfg := func(rec *recorder) GateConfig {
		return GateConfig{Policy: p, Events: rec, Guard: &fakeGuard{v: safe()}}
	}

	rec := &recorder{}
	if ran, _, _ := run(t, ghIssue(true), cfg(rec), Call{Role: "owner"}); !ran {
		t.Fatal("connector call did not run")
	}
	evs := rec.all()
	if len(evs) < 5 { // class, override, destructive, rules, guard ...
		t.Fatalf("events %v", rec.steps())
	}
	guard := false
	for _, e := range evs {
		var m map[string]any
		if err := json.Unmarshal(e.Attrs, &m); err != nil {
			t.Fatal(err)
		}
		if m["connector"] != "gh" {
			t.Fatalf("%s %v: connector = %v", e.Type, m["step"], m["connector"])
		}
		guard = guard || e.Type == event.GuardDecision
	}
	if !guard {
		t.Fatal("no guard.decision event")
	}

	rec = &recorder{}
	run(t, fakeTool{name: "fetch", risk: RiskExternal}, cfg(rec), Call{Role: "owner"})
	for _, e := range rec.all() {
		var m map[string]any
		if err := json.Unmarshal(e.Attrs, &m); err != nil {
			t.Fatal(err)
		}
		if _, has := m["connector"]; has {
			t.Fatalf("%s of a built-in carries connector: %v", e.Type, m)
		}
	}
}
