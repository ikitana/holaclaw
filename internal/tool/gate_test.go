package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// --- doubles -----------------------------------------------------------

type fakeTool struct {
	name string
	risk Risk
}

func (f fakeTool) Spec() provider.ToolSpec {
	return provider.ToolSpec{Name: f.name, Description: "x", Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (f fakeTool) Risk() Risk { return f.risk }
func (f fakeTool) Run(context.Context, Call) (Result, error) {
	return Result{Text: "ran " + f.name}, nil
}

// countingExec is the inner executor. It counts every execution so a test can
// assert the gate never reaches a tool outside the Allow branch.
type countingExec struct {
	mu    sync.Mutex
	runs  int
	tools map[string]Tool
}

func newInner(ts ...Tool) *countingExec {
	m := map[string]Tool{}
	for _, t := range ts {
		m[t.Spec().Name] = t
	}
	return &countingExec{tools: m}
}

func (e *countingExec) Specs() []provider.ToolSpec {
	out := []provider.ToolSpec{}
	for _, t := range e.tools {
		out = append(out, t.Spec())
	}
	return out
}

func (e *countingExec) Execute(ctx context.Context, c Call) (Result, error) {
	e.mu.Lock()
	e.runs++
	e.mu.Unlock()
	t, ok := e.tools[c.Name]
	if !ok {
		return Result{Text: "unknown tool " + c.Name, IsError: true}, nil
	}
	return t.Run(ctx, c)
}

func (e *countingExec) Lookup(name string) (Tool, bool) { t, ok := e.tools[name]; return t, ok }

func (e *countingExec) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runs
}

// bareExec is an inner executor that does not implement Lookuper.
type bareExec struct{ runs int }

func (e *bareExec) Specs() []provider.ToolSpec { return nil }
func (e *bareExec) Execute(context.Context, Call) (Result, error) {
	e.runs++
	return Result{Text: "ran"}, nil
}
func (e *bareExec) count() int { return e.runs }

type recorder struct {
	mu  sync.Mutex
	evs []event.Event
}

func (r *recorder) Emit(_ context.Context, e event.Event) {
	r.mu.Lock()
	r.evs = append(r.evs, e)
	r.mu.Unlock()
}

func (r *recorder) all() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Event(nil), r.evs...)
}

func (r *recorder) steps() []string {
	var out []string
	for _, e := range r.all() {
		var a decisionAttrs
		if json.Unmarshal(e.Attrs, &a) == nil {
			out = append(out, string(e.Type)+"/"+a.Step+"/"+string(a.Decision))
		}
	}
	return out
}

type fakeGrants struct {
	grant   *Grant // returned when set; else a session grant
	match   bool
	pattern string
	calls   int
}

func (f *fakeGrants) MatchGrant(_ context.Context, _, _, _, _, pattern string) (Grant, bool) {
	f.calls++
	f.pattern = pattern
	if !f.match {
		return Grant{}, false
	}
	if f.grant != nil {
		return *f.grant, true
	}
	return Grant{ID: "g1", Scope: GrantSession}, true
}

// fakeGuard answers one fixed verdict. A nil-safe counter lets a test assert
// the guard was never consulted.
type fakeGuard struct {
	mu    sync.Mutex
	v     Verdict
	err   error
	calls int
}

func (f *fakeGuard) Classify(context.Context, Call, Tool) (Verdict, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.v, f.err
}

func (f *fakeGuard) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// The two verdicts every table case uses, both with labels from the guard's
// closed set.
func safeVerdict() Verdict   { return Verdict{Score: 1, Label: "guard: benign"} }
func unsafeVerdict() Verdict { return Verdict{Score: 0.12, Label: "guard: destructive"} }

type fakeApprover struct {
	status tool_status
	err    error
	calls  int
	last   Approval
}

type tool_status = ApprovalStatus

func (f *fakeApprover) Request(_ context.Context, a Approval) (Approval, error) {
	f.calls++
	f.last = a
	if f.err != nil {
		return Approval{}, f.err
	}
	a.Status = f.status
	return a, nil
}

func execCommand(raw json.RawMessage) (string, error) {
	var v struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &v); err != nil || v.Command == "" {
		return "", errors.New("no command")
	}
	return v.Command, nil
}

// classes is a shorthand for a Classes block.
func classes(read, write, ext, exec Decision) Classes {
	return Classes{Read: read, Write: write, External: ext, Exec: exec}
}

// --- decision table ----------------------------------------------------

func TestGateDecisionTable(t *testing.T) {
	const allowAll = Allow
	_ = allowAll

	cases := []struct {
		name     string
		policy   Policy
		call     Call
		risk     Risk
		grant    bool
		guard    *fakeGuard
		noGuard  bool           // the guard must not be consulted at all
		approval ApprovalStatus // "" = no approver configured
		approver bool
		want     Decision // expected final outcome
		wantRuns int
		contains string // substring of the error text when not allowed
	}{{
		name:     "default allow read",
		policy:   Policy{Default: classes(Allow, Allow, Ask, Deny)},
		risk:     RiskRead,
		want:     Allow,
		wantRuns: 1,
	}, {
		name:     "default deny exec",
		policy:   Policy{Default: classes(Allow, Allow, Ask, Deny)},
		risk:     RiskExec,
		want:     Deny,
		contains: "denied by policy",
	}, {
		name:     "guest class is used for role guest",
		policy:   Policy{Default: classes(Allow, Allow, Allow, Allow), Guest: classes(Allow, Deny, Deny, Deny)},
		call:     Call{Role: "guest"},
		risk:     RiskWrite,
		want:     Deny,
		contains: "class:guest",
	}, {
		name:   "task class is used for detached runs",
		policy: Policy{Default: classes(Allow, Allow, Allow, Allow), Task: classes(Allow, Allow, Ask, Deny)},
		call:   Call{RunKind: RunTask},
		risk:   RiskExternal,
		// Ask on a task with no grant = deny (docs/tasks.md).
		want:     Deny,
		contains: "detached run without grant",
	}, {
		name: "guest+task takes the stricter class",
		policy: Policy{
			Guest: classes(Allow, Ask, Ask, Ask),
			Task:  classes(Allow, Allow, Deny, Allow),
		},
		call:     Call{Role: "guest", RunKind: RunTask},
		risk:     RiskExternal,
		want:     Deny,
		contains: "class:guest+task",
	}, {
		name: "guest+task stricter picks the guest side",
		policy: Policy{
			Guest: classes(Allow, Deny, Allow, Allow),
			Task:  classes(Allow, Allow, Allow, Allow),
		},
		call:     Call{Role: "guest", RunKind: RunTask},
		risk:     RiskWrite,
		want:     Deny,
		contains: "denied by policy",
	}, {
		name: "override may loosen ask to allow",
		policy: Policy{
			Default: classes(Allow, Ask, Ask, Ask),
			Tools:   map[string]Decision{"t": Allow},
		},
		risk:     RiskWrite,
		want:     Allow,
		wantRuns: 1,
	}, {
		name: "override may tighten allow to ask",
		policy: Policy{
			Default: classes(Allow, Allow, Allow, Allow),
			Tools:   map[string]Decision{"t": Ask},
		},
		risk:     RiskWrite,
		approver: true, approval: ApprovalApproved,
		want:     Allow,
		wantRuns: 1,
	}, {
		name: "class deny always wins over a loosening override",
		policy: Policy{
			Default: classes(Allow, Deny, Deny, Deny),
			Tools:   map[string]Decision{"t": Allow},
		},
		risk:     RiskExec,
		want:     Deny,
		contains: "class deny",
	}, {
		name: "guest class deny wins over an override",
		policy: Policy{
			Guest: classes(Allow, Deny, Deny, Deny),
			Tools: map[string]Decision{"t": Allow},
		},
		call:     Call{Role: "guest"},
		risk:     RiskExec,
		want:     Deny,
		contains: "class deny",
	}, {
		name: "guest override cannot loosen ask to allow",
		policy: Policy{
			Guest: classes(Allow, Ask, Ask, Ask),
			Tools: map[string]Decision{"t": Allow},
		},
		call:     Call{Role: "guest"},
		risk:     RiskWrite,
		approver: false,
		want:     Deny, // still Ask, and no approver configured
		contains: "no approver",
	}, {
		name: "guest override may still tighten to deny",
		policy: Policy{
			Guest: classes(Allow, Ask, Ask, Ask),
			Tools: map[string]Decision{"t": Deny},
		},
		call:     Call{Role: "guest"},
		risk:     RiskWrite,
		want:     Deny,
		contains: "override:guest",
	}, {
		name: "non-guest override may loosen for a task too",
		policy: Policy{
			Task:  classes(Allow, Ask, Ask, Ask),
			Tools: map[string]Decision{"t": Allow},
		},
		call:     Call{RunKind: RunTask},
		risk:     RiskWrite,
		want:     Allow,
		wantRuns: 1,
	}, {
		name: "taint escalates exec allow to ask",
		policy: Policy{
			Default: classes(Allow, Allow, Allow, Allow), TaintEscalates: true,
		},
		call:     Call{Tainted: true},
		risk:     RiskExec,
		approver: true, approval: ApprovalApproved,
		want:     Allow,
		wantRuns: 1,
	}, {
		name: "taint does not escalate write",
		policy: Policy{
			Default: classes(Allow, Allow, Allow, Allow), TaintEscalates: true,
		},
		call:     Call{Tainted: true},
		risk:     RiskWrite,
		want:     Allow,
		wantRuns: 1,
	}, {
		name: "taint escalation off leaves allow",
		policy: Policy{
			Default: classes(Allow, Allow, Allow, Allow), TaintEscalates: false,
		},
		call:     Call{Tainted: true},
		risk:     RiskExec,
		want:     Allow,
		wantRuns: 1,
	}, {
		name: "grant turns ask into allow",
		policy: Policy{
			Default: classes(Allow, Ask, Ask, Ask),
		},
		risk:     RiskWrite,
		grant:    true,
		want:     Allow,
		wantRuns: 1,
	}, {
		name: "auto mode: a safe verdict turns ask into allow",
		policy: Policy{
			Default: classes(Allow, Ask, Ask, Ask), GuardMode: GuardAuto,
		},
		risk:     RiskWrite,
		guard:    &fakeGuard{v: safeVerdict()},
		want:     Allow,
		wantRuns: 1,
	}, {
		// The v1 behaviour change: in recommend a guard is never permission,
		// so the Ask survives and (with no approver) becomes a deny.
		name: "recommend mode: a safe verdict never turns ask into allow",
		policy: Policy{
			Default: classes(Allow, Ask, Ask, Ask),
		},
		risk:     RiskWrite,
		guard:    &fakeGuard{v: safeVerdict()},
		want:     Deny,
		contains: "no approver",
	}, {
		name: "guard may move allow to ask",
		policy: Policy{
			Default: classes(Allow, Allow, Allow, Allow),
		},
		risk:     RiskWrite,
		guard:    &fakeGuard{v: unsafeVerdict()},
		approver: true, approval: ApprovalApproved,
		want:     Allow,
		wantRuns: 1,
	}, {
		name: "unsafe verdict fails closed to the human, never to allow",
		policy: Policy{
			Default: classes(Allow, Allow, Allow, Allow),
		},
		risk:     RiskWrite,
		guard:    &fakeGuard{v: unsafeVerdict()},
		approver: true, approval: ApprovalDenied,
		want:     Deny,
		contains: "approval denied",
	}, {
		name: "guard error fails closed to the human",
		policy: Policy{
			Default: classes(Allow, Allow, Allow, Allow),
		},
		risk:     RiskWrite,
		guard:    &fakeGuard{v: safeVerdict(), err: errors.New("boom")},
		approver: true, approval: ApprovalApproved,
		want:     Allow,
		wantRuns: 1,
	}, {
		name: "auto mode: a guard error denies",
		policy: Policy{
			Default: classes(Allow, Allow, Allow, Allow), GuardMode: GuardAuto,
		},
		risk:     RiskWrite,
		guard:    &fakeGuard{v: safeVerdict(), err: errors.New("boom")},
		approver: true, approval: ApprovalApproved,
		want:     Deny,
		contains: "guard error",
	}, {
		name: "guard is not consulted for read",
		policy: Policy{
			Default: classes(Allow, Allow, Allow, Allow),
		},
		risk:     RiskRead,
		guard:    &fakeGuard{v: unsafeVerdict()},
		noGuard:  true,
		want:     Allow,
		wantRuns: 1,
	}, {
		name: "guard is not consulted for deny",
		policy: Policy{
			Default: classes(Allow, Deny, Deny, Deny), GuardMode: GuardAuto,
		},
		risk:     RiskExec,
		guard:    &fakeGuard{v: safeVerdict()},
		noGuard:  true,
		want:     Deny,
		contains: "denied by policy",
	}, {
		name:     "approval denied does not run the tool",
		policy:   Policy{Default: classes(Allow, Ask, Ask, Ask)},
		risk:     RiskWrite,
		approver: true, approval: ApprovalDenied,
		want:     Deny,
		contains: "approval denied",
	}, {
		name:     "approval expired does not run the tool",
		policy:   Policy{Default: classes(Allow, Ask, Ask, Ask)},
		risk:     RiskWrite,
		approver: true, approval: ApprovalExpired,
		want:     Deny,
		contains: "approval timed out",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := newInner(fakeTool{name: "t", risk: tc.risk})
			rec := &recorder{}
			cfg := GateConfig{Policy: tc.policy, Events: rec, ExecCommand: execCommand}
			if tc.grant {
				cfg.Grants = &fakeGrants{match: true}
			}
			if tc.guard != nil {
				cfg.Guard = tc.guard
			}
			if tc.approver {
				cfg.Approver = &fakeApprover{status: tc.approval}
			}
			g := NewGate(inner, cfg)

			c := tc.call
			c.Name = "t"
			c.Args = json.RawMessage(`{"command":"echo hi"}`)
			res, err := g.Execute(context.Background(), c)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if tc.want == Allow {
				if res.IsError {
					t.Fatalf("want allow, got error result %q", res.Text)
				}
			} else if !res.IsError {
				t.Fatalf("want denial, got %q", res.Text)
			}
			if tc.contains != "" && !strings.Contains(res.Text, tc.contains) {
				t.Fatalf("result %q does not contain %q", res.Text, tc.contains)
			}
			if got := inner.count(); got != tc.wantRuns {
				t.Fatalf("inner executions = %d, want %d (steps: %v)", got, tc.wantRuns, rec.steps())
			}
			if len(rec.all()) == 0 {
				t.Fatal("no policy events emitted")
			}
			if tc.guard != nil {
				wantCalls := 1
				if tc.noGuard {
					wantCalls = 0
				}
				if got := tc.guard.count(); got != wantCalls {
					t.Fatalf("guard consulted %d times, want %d", got, wantCalls)
				}
			}
		})
	}
}

// --- rules order: taint vs allow-list vs grant -------------------------

func TestTaintSkipsAllowListButNotGrant(t *testing.T) {
	base := Policy{
		Default:        classes(Allow, Allow, Allow, Allow),
		ExecAllow:      []string{"git status"},
		TaintEscalates: true,
	}
	args := json.RawMessage(`{"command":"git status -s"}`)

	t.Run("untainted allow-list satisfies the ask", func(t *testing.T) {
		p := base
		p.Default.Exec = Ask
		inner := newInner(fakeTool{name: "sh", risk: RiskExec})
		g := NewGate(inner, GateConfig{Policy: p, ExecCommand: execCommand})
		res, _ := g.Execute(context.Background(), Call{Name: "sh", Args: args})
		if res.IsError || inner.count() != 1 {
			t.Fatalf("want allow-listed run, got %q runs=%d", res.Text, inner.count())
		}
	})

	t.Run("tainted turn is not rescued by the allow-list", func(t *testing.T) {
		inner := newInner(fakeTool{name: "sh", risk: RiskExec})
		g := NewGate(inner, GateConfig{Policy: base, ExecCommand: execCommand})
		res, _ := g.Execute(context.Background(), Call{Name: "sh", Args: args, Tainted: true})
		if !res.IsError || inner.count() != 0 {
			t.Fatalf("tainted allow-listed command must not run: %q runs=%d", res.Text, inner.count())
		}
	})

	t.Run("tainted turn is rescued by an explicit grant", func(t *testing.T) {
		inner := newInner(fakeTool{name: "sh", risk: RiskExec})
		gr := &fakeGrants{match: true}
		g := NewGate(inner, GateConfig{Policy: base, Grants: gr, ExecCommand: execCommand})
		res, _ := g.Execute(context.Background(), Call{Name: "sh", Args: args, Tainted: true})
		if res.IsError || inner.count() != 1 {
			t.Fatalf("granted tainted command should run: %q", res.Text)
		}
		if gr.pattern != "git status -s" {
			t.Fatalf("grant lookup pattern = %q", gr.pattern)
		}
	})

	t.Run("non-exec ask looks up grants with an empty pattern", func(t *testing.T) {
		p := Policy{Default: classes(Allow, Ask, Ask, Ask)}
		inner := newInner(fakeTool{name: "w", risk: RiskWrite})
		gr := &fakeGrants{}
		g := NewGate(inner, GateConfig{Policy: p, Grants: gr, ExecCommand: execCommand})
		_, _ = g.Execute(context.Background(), Call{Name: "w", Args: json.RawMessage(`{"path":"a"}`)})
		if gr.calls != 1 || gr.pattern != "" {
			t.Fatalf("grant lookup calls=%d pattern=%q", gr.calls, gr.pattern)
		}
	})
}

// --- command matcher ---------------------------------------------------

func TestMatchCommand(t *testing.T) {
	cases := []struct {
		prefix, cmd string
		want        bool
	}{
		{"git status", "git status", true},
		{"git status", "git status -s", true},
		{"git status", "git status  -s --short", true},
		{"git status", "git statusx", false},
		{"git status", "git status-foo", false},
		{"git status", "git stat", false},
		{"git", "git status", true},
		{"git status", "  git status -s  ", true},
		{"", "git status", false},
		{"git status", "", false},
		{"git status", "xgit status", false},
		// Shell metacharacters disable pattern matching entirely.
		{"git status", "git status; rm -rf /", false},
		{"git status", "git status && x", false},
		{"git status", "git status | tee x", false},
		{"git status", "git status > out", false},
		{"git status", "git status < in", false},
		{"git status", "git status $(x)", false},
		{"git status", "git status `x`", false},
		{"git status", "git status\nrm -rf /", false},
		{"git status", "git status $HOME", false},
		{"git status", "(git status)", false},
	}
	for _, tc := range cases {
		if got := MatchCommand(tc.prefix, tc.cmd); got != tc.want {
			t.Errorf("MatchCommand(%q, %q) = %v, want %v", tc.prefix, tc.cmd, got, tc.want)
		}
	}
}

func TestStricter(t *testing.T) {
	cases := []struct{ a, b, want Decision }{
		{Allow, Ask, Ask},
		{Ask, Allow, Ask},
		{Ask, Deny, Deny},
		{Deny, Allow, Deny},
		{Allow, Allow, Allow},
		{Decision("weird"), Allow, Decision("weird")}, // unknown ranks as deny
	}
	for _, tc := range cases {
		if got := stricter(tc.a, tc.b); got != tc.want {
			t.Errorf("stricter(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// --- structural guarantees ---------------------------------------------

func TestUnknownToolPassesThrough(t *testing.T) {
	inner := newInner(fakeTool{name: "t", risk: RiskRead})
	rec := &recorder{}
	g := NewGate(inner, GateConfig{Policy: Policy{Default: classes(Deny, Deny, Deny, Deny)}, Events: rec})
	res, err := g.Execute(context.Background(), Call{Name: "nope"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.Text != `unknown tool "nope"` {
		t.Fatalf("want the unknown-tool error, got %q", res.Text)
	}
	if inner.count() != 0 {
		t.Fatalf("unknown tool reached inner %d times", inner.count())
	}
	if !hasStep(rec, "lookup", Allow, "unknown tool") {
		t.Fatalf("no lookup event in %v", rec.steps())
	}
}

// liveExec misses on Lookup but would run anything on Execute: a connector
// tool that appeared between the gate's lookup and the inner one.
type liveExec struct{ countingExec }

func (e *liveExec) Lookup(string) (Tool, bool) { return nil, false }

func (e *liveExec) Execute(context.Context, Call) (Result, error) {
	e.mu.Lock()
	e.runs++
	e.mu.Unlock()
	return Result{Text: "ran"}, nil
}

func TestUnknownToolNeverReachesInner(t *testing.T) {
	inner := &liveExec{}
	g := NewGate(inner, GateConfig{Policy: Policy{Default: classes(Allow, Allow, Allow, Allow)}})
	res, err := g.Execute(context.Background(), Call{Name: "gh.new_tool"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || inner.count() != 0 {
		t.Fatalf("a tool unknown to the gate ran: %q runs=%d", res.Text, inner.count())
	}
}

func TestInnerWithoutLookupDeniesEverything(t *testing.T) {
	inner := &bareExec{}
	g := NewGate(inner, GateConfig{Policy: Policy{Default: classes(Allow, Allow, Allow, Allow)}})
	res, _ := g.Execute(context.Background(), Call{Name: "t"})
	if !res.IsError || !strings.Contains(res.Text, "no tool metadata") {
		t.Fatalf("got %q", res.Text)
	}
	if inner.count() != 0 {
		t.Fatal("inner must not run without tool metadata")
	}
}

func TestSpecsAndLookupPassThrough(t *testing.T) {
	inner := newInner(fakeTool{name: "t", risk: RiskRead})
	g := NewGate(inner, GateConfig{})
	if len(g.Specs()) != 1 {
		t.Fatalf("specs = %v", g.Specs())
	}
	lk, ok := g.(Lookuper)
	if !ok {
		t.Fatal("gate must implement Lookuper")
	}
	if _, ok := lk.Lookup("t"); !ok {
		t.Fatal("lookup should find t")
	}
	if _, ok := lk.Lookup("other"); ok {
		t.Fatal("lookup should not invent tools")
	}
	// A gate wrapping a gate still resolves metadata.
	if _, ok := NewGate(g, GateConfig{}).(Lookuper).Lookup("t"); !ok {
		t.Fatal("nested gate lost lookup")
	}
}

func TestApproverErrorDenies(t *testing.T) {
	inner := newInner(fakeTool{name: "t", risk: RiskWrite})
	ap := &fakeApprover{err: errors.New("db down")}
	g := NewGate(inner, GateConfig{
		Policy: Policy{Default: classes(Allow, Ask, Ask, Ask)}, Approver: ap,
	})
	res, err := g.Execute(context.Background(), Call{Name: "t"})
	if err != nil {
		t.Fatalf("gate must not surface approver errors: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Text, "approval failed") {
		t.Fatalf("got %q", res.Text)
	}
	if inner.count() != 0 {
		t.Fatal("tool ran despite an approver error")
	}
}

func TestApprovalSummaryAndArgs(t *testing.T) {
	long := `{"command":"` + strings.Repeat("a", 800) + `"}`

	t.Run("exec summary carries the command", func(t *testing.T) {
		ap := &fakeApprover{status: ApprovalDenied}
		g := NewGate(newInner(fakeTool{name: "sh", risk: RiskExec}), GateConfig{
			Policy: Policy{Default: classes(Allow, Ask, Ask, Ask)}, Approver: ap, ExecCommand: execCommand,
		})
		_, _ = g.Execute(context.Background(), Call{Name: "sh", Args: json.RawMessage(`{"command":"rm -rf /"}`)})
		if ap.last.Summary != "sh: rm -rf /" {
			t.Fatalf("summary = %q", ap.last.Summary)
		}
	})

	t.Run("other tools get a capped args excerpt", func(t *testing.T) {
		ap := &fakeApprover{status: ApprovalDenied}
		g := NewGate(newInner(fakeTool{name: "w", risk: RiskWrite}), GateConfig{
			Policy: Policy{Default: classes(Allow, Ask, Ask, Ask)}, Approver: ap,
		})
		_, _ = g.Execute(context.Background(), Call{Name: "w", Args: json.RawMessage(long)})
		if n := len(ap.last.Summary); n > maxSummaryArgs+len("w ")+4 {
			t.Fatalf("summary is %d bytes, want capped near %d", n, maxSummaryArgs)
		}
		if !strings.HasPrefix(ap.last.Summary, "w ") {
			t.Fatalf("summary = %q", ap.last.Summary)
		}
	})

	t.Run("empty args summary is just the tool", func(t *testing.T) {
		ap := &fakeApprover{status: ApprovalDenied}
		g := NewGate(newInner(fakeTool{name: "w", risk: RiskWrite}), GateConfig{
			Policy: Policy{Default: classes(Allow, Ask, Ask, Ask)}, Approver: ap,
		})
		_, _ = g.Execute(context.Background(), Call{Name: "w"})
		if ap.last.Summary != "w" {
			t.Fatalf("summary = %q", ap.last.Summary)
		}
	})
}

func TestCap(t *testing.T) {
	if got := Cap("hello", 10); got != "hello" {
		t.Fatalf("got %q", got)
	}
	if got := Cap("hello", 3); got != "hel…" {
		t.Fatalf("got %q", got)
	}
	// Never splits a rune.
	if got := Cap("héllo", 2); got != "h…" {
		t.Fatalf("got %q", got)
	}
	if !json.Valid([]byte(`"` + Cap("é", 1) + `"`)) {
		t.Fatal("cap produced invalid UTF-8")
	}
}

// --- events ------------------------------------------------------------

func TestEventsPerStep(t *testing.T) {
	rec := &recorder{}
	inner := newInner(fakeTool{name: "sh", risk: RiskExec})
	g := NewGate(inner, GateConfig{
		Policy: Policy{
			Default:        classes(Allow, Allow, Allow, Allow),
			Tools:          map[string]Decision{"sh": Allow},
			TaintEscalates: true,
		},
		Grants:      &fakeGrants{match: true},
		Guard:       &fakeGuard{v: safeVerdict()},
		Events:      rec,
		ExecCommand: execCommand,
	})
	res, _ := g.Execute(context.Background(), Call{
		Name: "sh", Args: json.RawMessage(`{"command":"ls"}`), Tainted: true,
		TraceID: event.ID("trace"), SessionID: event.ID("sess"),
	})
	if res.IsError {
		t.Fatalf("want allow: %q", res.Text)
	}
	want := []string{
		"policy.decision/class/allow",
		"policy.decision/override/allow",
		"policy.decision/taint/ask",
		"policy.decision/rules/allow",
		"guard.decision/guard/allow",
		"policy.decision/guard/allow",
		"policy.decision/final/allow",
	}
	got := rec.steps()
	if len(got) != len(want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
	for _, e := range rec.all() {
		if e.TraceID != "trace" || e.SessionID != "sess" {
			t.Fatalf("event %s lost its trace ids", e.Type)
		}
		if strings.Contains(string(e.Attrs), "ls\"") && strings.Contains(string(e.Attrs), "command") {
			t.Fatalf("policy event leaked arguments: %s", e.Attrs)
		}
	}
}

func TestDenyEmitsFinalEventAndNeverRuns(t *testing.T) {
	rec := &recorder{}
	inner := newInner(fakeTool{name: "sh", risk: RiskExec})
	g := NewGate(inner, GateConfig{Policy: Policy{Default: classes(Allow, Allow, Allow, Deny)}, Events: rec})
	if _, err := g.Execute(context.Background(), Call{Name: "sh"}); err != nil {
		t.Fatal(err)
	}
	steps := rec.steps()
	if steps[len(steps)-1] != "policy.decision/final/deny" {
		t.Fatalf("steps = %v", steps)
	}
	if inner.count() != 0 {
		t.Fatal("denied call reached the tool")
	}
}

func TestNilEventsSinkIsSafe(t *testing.T) {
	g := NewGate(newInner(fakeTool{name: "t", risk: RiskRead}), GateConfig{
		Policy: Policy{Default: classes(Allow, Allow, Allow, Allow)},
	})
	if _, err := g.Execute(context.Background(), Call{Name: "t"}); err != nil {
		t.Fatal(err)
	}
}

// Security review H4: with exec already at Ask by policy, a tainted turn must
// still not be rescued by the static allow-list.
func TestTaintedAskIsNotRescuedByAllowList(t *testing.T) {
	p := Policy{
		Default:        classes(Allow, Allow, Allow, Ask),
		ExecAllow:      []string{"git status"},
		TaintEscalates: true,
	}
	inner := newInner(fakeTool{name: "sh", risk: RiskExec})
	g := NewGate(inner, GateConfig{Policy: p, ExecCommand: execCommand})
	res, _ := g.Execute(context.Background(), Call{Name: "sh", Args: json.RawMessage(`{"command":"git status -s"}`), Tainted: true})
	if !res.IsError || inner.count() != 0 {
		t.Fatalf("tainted allow-listed command ran: %q runs=%d", res.Text, inner.count())
	}
}

// Security review M2: grants never apply to guests, and an "always" grant
// covers only the user who approved it.
func TestGrantsAreBoundToTheirApprover(t *testing.T) {
	p := Policy{Default: classes(Allow, Ask, Allow, Deny), Guest: classes(Allow, Ask, Deny, Deny)}
	always := &Grant{ID: "g", Scope: GrantAlways, CreatedBy: "owner1"}
	cases := []struct {
		name string
		call Call
		runs int
	}{
		{"approver", Call{Name: "w", UserID: "owner1", Role: "owner"}, 1},
		{"another user", Call{Name: "w", UserID: "user2", Role: "user"}, 0},
		{"guest", Call{Name: "w", UserID: "owner1", Role: "guest"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inner := newInner(fakeTool{name: "w", risk: RiskWrite})
			g := NewGate(inner, GateConfig{Policy: p, Grants: &fakeGrants{match: true, grant: always}})
			tc.call.Args = json.RawMessage(`{}`)
			_, _ = g.Execute(context.Background(), tc.call)
			if inner.count() != tc.runs {
				t.Fatalf("runs = %d, want %d", inner.count(), tc.runs)
			}
		})
	}
}

func TestMatchCommandBoundaryAndGlobs(t *testing.T) {
	for cmd, want := range map[string]bool{
		"git status":           true,
		"git status -s":        true,
		"git status\t-s":       true,
		"git status -s":        false, // not a word boundary for sh
		"git status *":         false,
		"git status ~/x":       false,
		"git status {a,b}":     false,
		"git status\rrm -rf /": false,
	} {
		if got := MatchCommand("git status", cmd); got != want {
			t.Errorf("MatchCommand(%q) = %v, want %v", cmd, got, want)
		}
	}
}
