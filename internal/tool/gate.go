package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// GrantMatcher looks up persisted grants. approval.Service implements it; the
// gate keeps the dependency one-way (approval imports tool, never the reverse).
type GrantMatcher interface {
	// MatchGrant reports the first grant of agent for toolName whose scope and
	// pattern cover this call. A lookup failure reports no match: grants only
	// ever turn Ask into Allow, so failing closed means asking the human.
	MatchGrant(ctx context.Context, agent, toolName, sessionID, taskID, pattern string) (Grant, bool)
}

// AlwaysAsker is implemented by a tool whose calls a person must approve
// whatever the class policy says, a connector tool the server marks
// `destructiveHint` (docs/connectors.md §6). A deny still wins. Optional: the
// gate asks the tool with a type assertion.
type AlwaysAsker interface {
	AlwaysAsk() bool
}

// TrustedTool is implemented by a tool whose results come from a source the
// operator vouches for, a `trusted` connector (docs/connectors.md §6). Its
// results stay wrapped as untrusted content (Result.Tainted) but are not
// scored, so they never arm the taint. Optional, like AlwaysAsker.
type TrustedTool interface {
	Trusted() bool
}

// ConnectorTool is implemented by a tool of a connector: Connector is its id.
// A policy.tools key naming the id covers every tool of the connector, the
// exact tool name winning, and policy.decision and tool.result carry it
// (docs/connectors.md §6, §10). Optional, like AlwaysAsker.
type ConnectorTool interface {
	Connector() string
}

// GateConfig wires one agent's policy to the runtime pieces that can relax it.
// Grants, Approver and Guard are all optional; without them the gate decides
// from the static policy alone and an Ask becomes a Deny.
type GateConfig struct {
	Policy   Policy
	Grants   GrantMatcher
	Approver Approver
	Guard    Guard
	Events   event.Sink
	// ExecCommand extracts the shell command from an exec tool's arguments. It
	// feeds the allow-list, grant patterns and the approval summary. Without it
	// exec calls carry no pattern, so only pattern-less grants match them.
	ExecCommand func(json.RawMessage) (string, error)
}

// Summary / args excerpt caps. The summary reaches a human on a phone; the
// excerpt reaches the trace and the approvals table.
const (
	maxSummaryArgs = 200
	// MaxApprovalArgs caps the raw argument excerpt stored and traced with an
	// approval. Arguments are model output and may be long or sensitive.
	MaxApprovalArgs = 512
)

// NewGate wraps inner with the policy pipeline of docs/security.md: risk class
// → policy → rules → guard → human approval. It is the only path from the
// agent loop to a tool: inner.Execute runs in the Allow branch and nowhere
// else. The returned Executor also implements Lookuper.
func NewGate(inner Executor, cfg GateConfig) Executor {
	if cfg.Events == nil {
		cfg.Events = event.Discard
	}
	return &gate{inner: inner, cfg: cfg}
}

type gate struct {
	inner Executor
	cfg   GateConfig
}

func (g *gate) Specs() []provider.ToolSpec { return g.inner.Specs() }

// Lookup forwards to the wrapped executor so a gate can wrap a gate.
func (g *gate) Lookup(name string) (Tool, bool) {
	lk, ok := g.inner.(Lookuper)
	if !ok {
		return nil, false
	}
	return lk.Lookup(name)
}

// decisionAttrs is the attrs object of every policy.decision event. Never
// carries arguments, and Reason is always a fixed string: both reach the
// trace, where model text could carry injected instructions.
type decisionAttrs struct {
	Tool     string   `json:"tool"`
	Risk     Risk     `json:"risk,omitempty"`
	Step     string   `json:"step"`
	Decision Decision `json:"decision"`
	Reason   string   `json:"reason,omitempty"`
	// Connector is the id of a connector tool's connector (docs/connectors.md
	// §12), empty for every other tool.
	Connector string `json:"connector,omitempty"`
}

// guardAttrs is the attrs object of guard.decision: the same fields plus what
// only the guard has (docs/traceability.md). Score is always written, zero
// included — a missing score would read as "no verdict". The classifier
// call's usage is written when a call was answered and left out when none was
// (a cached verdict, an error before any reply), so a replayed verdict is
// never counted as a call (event.UsageTypes).
type guardAttrs struct {
	decisionAttrs
	Mode  GuardMode `json:"mode"`
	Score float64   `json:"score"`
	*event.UsageAttrs
}

// gateCall is a Call with the connector id of its tool, resolved once so
// every event of the call carries it.
type gateCall struct {
	Call
	connector string
}

func (g *gate) emit(ctx context.Context, t event.Type, c gateCall, risk Risk, step string, d Decision, reason string) {
	g.cfg.Events.Emit(ctx, event.New(t, c.TraceID, c.SessionID, "", decisionAttrs{
		Tool: c.Name, Risk: risk, Step: step, Decision: d, Reason: reason, Connector: c.connector,
	}))
}

// emitGuard emits guard.decision, timed: the guard is the one step of the
// pipeline that costs latency and money.
func (g *gate) emitGuard(ctx context.Context, c gateCall, risk Risk, d Decision, reason string, mode GuardMode, v Verdict, dur time.Duration) {
	attrs := guardAttrs{
		decisionAttrs: decisionAttrs{
			Tool: c.Name, Risk: risk, Step: "guard", Decision: d, Reason: reason, Connector: c.connector,
		},
		Mode: mode, Score: v.Score,
	}
	if v.Usage != nil {
		attrs.UsageAttrs = event.Call(*v.Usage).Attrs()
	}
	e := event.New(event.GuardDecision, c.TraceID, c.SessionID, "", attrs)
	e.Duration = dur
	g.cfg.Events.Emit(ctx, e)
}

func (g *gate) Execute(ctx context.Context, call Call) (Result, error) {
	c := gateCall{Call: call}
	// (a) metadata. Without a Lookuper the gate cannot know the risk class, so
	// nothing may run.
	lk, ok := g.inner.(Lookuper)
	if !ok {
		g.emit(ctx, event.PolicyDecision, c, "", "lookup", Deny, "no tool metadata")
		return denied(c.Name, "no tool metadata"), nil
	}
	t, known := lk.Lookup(c.Name)
	if !known {
		// The gate answers with the registry's unknown-tool error itself:
		// a live inner Lookup (connector tools) could find a tool that
		// appeared since, and inner.Execute would run it with no policy.
		g.emit(ctx, event.PolicyDecision, c, "", "lookup", Allow, "unknown tool")
		return Result{Text: fmt.Sprintf("unknown tool %q", c.Name), IsError: true}, nil
	}
	risk := t.Risk()
	if ct, ok := t.(ConnectorTool); ok {
		c.connector = ct.Connector()
	}

	// (b) class by origin.
	d, reason := g.classDecision(call, risk)
	g.emit(ctx, event.PolicyDecision, c, risk, "class", d, reason)

	// (c) per-tool override.
	if od, has := g.override(c); has {
		switch {
		case d == Deny:
			reason = "class deny"
		case c.Role == RoleGuest:
			// Guests: an override may only tighten the guest class.
			d, reason = stricter(d, od), "override:guest"
		default:
			d, reason = od, "override"
		}
		g.emit(ctx, event.PolicyDecision, c, risk, "override", d, reason)
	}

	// (c2) destructiveHint: a person approves the call whatever the class and
	// the override say. A Deny still wins.
	always := alwaysAsks(t)
	if always && d == Allow {
		d, reason = Ask, "destructive"
		g.emit(ctx, event.PolicyDecision, c, risk, "destructive", d, reason)
	}

	// (d) taint escalation. Whether the turn is tainted does not depend on
	// the decision so far: an Ask reached by policy must not be rescued by the
	// static allow-list either.
	tainted := g.cfg.Policy.TaintEscalates && c.Tainted && (risk == RiskExec || risk == RiskExternal)
	if tainted && d == Allow {
		d, reason = Ask, "taint"
		g.emit(ctx, event.PolicyDecision, c, risk, "taint", d, reason)
	}

	// (e) rules: allow-list and persisted grants may turn Ask into Allow.
	// granted remembers that the Allow is a human decision, which the guard
	// treats differently from an allow-list entry in auto mode.
	granted := false
	if d == Ask {
		// Only a grant answers an always-ask call, as for a tainted one.
		d, reason = g.rules(ctx, call, risk, tainted || always)
		g.emit(ctx, event.PolicyDecision, c, risk, "rules", d, reason)
		granted = d == Allow && reason == "grant"
	}

	// (f) guard: never sees a Deny and never lifts one. What its verdict may
	// change is Policy.GuardMode (docs/security.md, "Guard modes"), never the guard itself.
	var verdict *Verdict
	if g.cfg.Guard != nil && d != Deny && risk != RiskRead {
		// The auto-mode taint rule does not depend on taint_escalates: that key
		// is about asking a human, and auto is the mode where nobody would be.
		untrusted := c.Tainted && (risk == RiskExec || risk == RiskExternal)
		d, reason, verdict = g.guard(ctx, c, t, risk, d, granted, untrusted)
		g.emit(ctx, event.PolicyDecision, c, risk, "guard", d, reason)
	}

	// (g) final.
	switch d {
	case Allow:
		g.emit(ctx, event.PolicyDecision, c, risk, "final", Allow, reason)
		return g.inner.Execute(ctx, call)
	case Deny:
		g.emit(ctx, event.PolicyDecision, c, risk, "final", Deny, reason)
		return denied(c.Name, reason), nil
	}
	return g.ask(ctx, c, risk, reason, verdict)
}

// guard runs the guard step and applies the mode. It returns the new
// decision, a fixed reason, and the verdict when there is one (nil on a guard
// failure) so an approval prompt can carry it.
//
// The modes differ only in what an unsafe verdict may do. In recommend the
// guard is advice and an extra question, never an extra permission: it can
// push an Allow down to Ask and nothing else — in particular it can no longer
// turn an Ask into an Allow, which is the one thing only a human may do. In
// auto nobody is asked, so an unsafe verdict denies, with three exceptions
// towards caution (a tainted exec / external call and an always-ask call
// without a grant are never allowed by the guard, and a Deny never reached
// here at all) and one towards the human: a
// call allowed by a *grant* is downgraded to Ask rather than denied. A grant
// is a decision a person made about this exact call; a classifier that
// disagrees has found something worth a second look, not authority to
// overrule the person. Allow-list and class allows carry no such decision and
// are denied.
func (g *gate) guard(ctx context.Context, c gateCall, t Tool, risk Risk, d Decision, granted, tainted bool) (Decision, string, *Verdict) {
	mode := g.cfg.Policy.GuardMode
	if mode == "" {
		mode = GuardRecommend
	}
	thr := g.cfg.Policy.GuardThreshold
	if thr <= 0 {
		thr = DefaultGuardThreshold
	}

	start := time.Now()
	v, err := g.cfg.Guard.Classify(ctx, c.Call, t)
	dur := time.Since(start)

	var verdict *Verdict
	reason, safe := "guard error", false
	if err != nil {
		// Only the kind of failure, never the upstream message: it reaches the
		// trace and the model's result text, and it may quote content the guard
		// was shown.
		if k := provider.KindOf(err); k != "" {
			reason += ": " + string(k)
		}
		v = Verdict{Usage: v.Usage} // a reply that failed was still paid for
	} else {
		verdict, safe, reason = &v, v.Score >= thr, v.Label
		if reason == "" {
			reason = "guard"
		} else if !safe {
			// The label says what the call looks like, the threshold says
			// whether that was enough: "guard: benign" alone on an Ask reads as
			// a contradiction in the trace. Still a fixed string built from
			// closed-set parts — no model text.
			reason += " below threshold"
		}
	}

	nd := d
	switch mode {
	case GuardAuto:
		switch {
		case safe && tainted:
			nd = Ask // docs/security.md: injected text could be steering the guard too.
		case safe && !granted && alwaysAsks(t):
			// destructiveHint: a person approves the call. A grant is that
			// approval; a classifier's opinion is not.
			nd = Ask
		case safe && d == Ask && c.RunKind == RunTask:
			// docs/tasks.md: a detached run turns ask into allow with a scoped grant set
			// by a person, never with a classifier's opinion. Staying at Ask
			// means deny for a task.
			nd = Ask
		case safe:
			nd = Allow
		case granted:
			nd = Ask // a model does not overrule a human grant; it re-asks.
		case v.Label == BenignLabel && c.RunKind != RunTask:
			// The guard called it ordinary and only its confidence fell short
			// of the threshold. A person's yes is exactly the confidence that
			// is missing, so the call is asked rather than denied — auto mode
			// keeps deciding the clear cases on its own, and the person is
			// brought in on the one kind of call where saying no was never
			// the guard's finding. Any other label, and an error, stay a
			// deny: there the guard did find something. A detached run has
			// nobody to ask (docs/tasks.md) and keeps the deny.
			nd = Ask
		default:
			nd = Deny
		}
	default: // GuardRecommend
		if !safe {
			nd = Ask
		}
	}
	g.emitGuard(ctx, c, risk, nd, reason, mode, v, dur)
	return nd, reason, verdict
}

// override finds the policy.tools entry of a call: its exact tool name, else
// its connector's id. Only connector tools fall back, so a key "memory" never
// covers the built-in "memory.save".
func (g *gate) override(c gateCall) (Decision, bool) {
	if d, ok := g.cfg.Policy.Tools[c.Name]; ok {
		return d, true
	}
	if c.connector == "" {
		return "", false
	}
	d, ok := g.cfg.Policy.Tools[c.connector]
	return d, ok
}

// alwaysAsks reports whether a person must approve every call of t.
func alwaysAsks(t Tool) bool {
	aa, ok := t.(AlwaysAsker)
	return ok && aa.AlwaysAsk()
}

// classDecision picks the Classes block this call is judged by. A guest task
// run is judged by the stricter of both.
func (g *gate) classDecision(c Call, risk Risk) (Decision, string) {
	guest := c.Role == RoleGuest
	task := c.RunKind == RunTask
	switch {
	case guest && task:
		return stricter(g.cfg.Policy.Guest.For(risk), g.cfg.Policy.Task.For(risk)), "class:guest+task"
	case guest:
		return g.cfg.Policy.Guest.For(risk), "class:guest"
	case task:
		return g.cfg.Policy.Task.For(risk), "class:task"
	default:
		return g.cfg.Policy.Default.For(risk), "class:default"
	}
}

// rules applies the static exec allow-list and persisted grants to an Ask.
// With grantOnly set (a turn that carries untrusted content, an always-ask
// tool) only an explicit grant may rescue the call: the allow-list is
// skipped, so such a call never silently runs an allow-listed command.
func (g *gate) rules(ctx context.Context, c Call, risk Risk, grantOnly bool) (Decision, string) {
	cmd := ""
	if risk == RiskExec && g.cfg.ExecCommand != nil {
		if s, err := g.cfg.ExecCommand(c.Args); err == nil {
			cmd = s
		}
	}
	if !grantOnly && risk == RiskExec && cmd != "" {
		for _, p := range g.cfg.Policy.ExecAllow {
			if MatchCommand(p, cmd) {
				return Allow, "exec allow-list"
			}
		}
	}
	// Grants never apply to guests, and an "always" grant covers only the
	// person who approved it: one owner saying "always" in their own DM must
	// not turn ask into allow for every other user of the agent.
	if g.cfg.Grants != nil && c.Role != RoleGuest {
		if gr, ok := g.cfg.Grants.MatchGrant(ctx, c.Agent, c.Name, string(c.SessionID), string(c.TaskID), cmd); ok {
			if gr.Scope != GrantAlways || (gr.CreatedBy != "" && gr.CreatedBy == c.UserID) {
				return Allow, "grant"
			}
		}
	}
	return Ask, "ask"
}

// ask suspends this call on a human decision. Detached runs never do: docs/tasks.md makes
// ask = deny unless a grant already answered above.
func (g *gate) ask(ctx context.Context, c gateCall, risk Risk, reason string, v *Verdict) (Result, error) {
	if c.RunKind == RunTask {
		g.emit(ctx, event.PolicyDecision, c, risk, "final", Deny, "detached run without grant")
		return denied(c.Name, "detached run without grant"), nil
	}
	if g.cfg.Approver == nil {
		g.emit(ctx, event.PolicyDecision, c, risk, "final", Deny, "no approver")
		return denied(c.Name, "no approver"), nil
	}
	g.emit(ctx, event.PolicyDecision, c, risk, "final", Ask, reason)

	a, err := g.cfg.Approver.Request(ctx, Approval{
		TraceID: c.TraceID, SessionID: c.SessionID,
		Agent: c.Agent, Tool: c.Name, Summary: verdictPrefix(v) + g.summary(c.Call, risk), Args: string(c.Args),
	})
	if err != nil {
		g.emit(ctx, event.PolicyDecision, c, risk, "approval", Deny, "approval failed")
		return denied(c.Name, "approval failed"), nil
	}
	switch a.Status {
	case ApprovalApproved:
		g.emit(ctx, event.PolicyDecision, c, risk, "approval", Allow, "approved")
		return g.inner.Execute(ctx, c.Call)
	case ApprovalExpired:
		g.emit(ctx, event.PolicyDecision, c, risk, "approval", Deny, "approval timed out")
		return denied(c.Name, "approval timed out"), nil
	default:
		g.emit(ctx, event.PolicyDecision, c, risk, "approval", Deny, "approval denied")
		return denied(c.Name, "approval denied"), nil
	}
}

// summary is what a human sees on their phone: the tool, and for exec the
// command itself. Anything else is a short, capped excerpt of the arguments.
func (g *gate) summary(c Call, risk Risk) string {
	if risk == RiskExec && g.cfg.ExecCommand != nil {
		if cmd, err := g.cfg.ExecCommand(c.Args); err == nil && cmd != "" {
			return c.Name + ": " + plainSummary(Cap(cmd, maxSummaryArgs))
		}
	}
	if len(c.Args) == 0 {
		return c.Name
	}
	return c.Name + " " + plainSummary(Cap(string(c.Args), maxSummaryArgs))
}

// BenignLabel is the verdict label of a call the guard found ordinary — the
// guard package builds it as "guard: " + its benign kind, and a test there
// holds the two together. The gate reads it for one decision only: in auto
// mode, an ordinary call that merely fell short of the threshold is asked,
// not denied.
const BenignLabel = "guard: benign"

// verdictPrefix is what the guard adds to that line: "[guard: 0.12
// destructive] ". It comes first, so no argument can push it off a phone
// screen, and the label is a fixed string from the guard's closed set — its
// own "guard: " prefix is dropped rather than repeated.
func verdictPrefix(v *Verdict) string {
	if v == nil {
		return ""
	}
	label := strings.TrimSpace(strings.TrimPrefix(v.Label, "guard: "))
	if label == "" {
		return fmt.Sprintf("[guard: %.2f] ", v.Score)
	}
	return fmt.Sprintf("[guard: %.2f %s] ", v.Score, label)
}

// plainSummary keeps model-chosen text from imitating the verdict: only the
// gate writes square brackets into an approval line.
func plainSummary(s string) string {
	return strings.NewReplacer("[", "(", "]", ")").Replace(s)
}

func denied(name, reason string) Result {
	return Result{IsError: true, Text: fmt.Sprintf("denied by policy: %s (%s)", name, reason)}
}

// Cap truncates s to at most n bytes without splitting a rune, marking a cut.
func Cap(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

// stricter returns the stronger of two decisions: Deny > Ask > Allow.
func stricter(a, b Decision) Decision {
	if rank(a) >= rank(b) {
		return a
	}
	return b
}

func rank(d Decision) int {
	switch d {
	case Allow:
		return 0
	case Ask:
		return 1
	case Deny:
		return 2
	}
	return 2 // unknown values are treated as Deny
}

// execMeta are the shell metacharacters that make a command more than the
// prefix it starts with.
const execMeta = ";&|$`><()\n\r*?{}~"

// MatchCommand reports whether cmd is covered by the prefix rule prefix. The
// prefix must match whole words ("git status" covers "git status -s" but not
// "git statusx"), and a command containing any shell metacharacter is never
// covered: the gate cannot tell what the rest of the line would run, so
// allow-list entries and grant patterns simply do not apply to it.
func MatchCommand(prefix, cmd string) bool {
	prefix, cmd = strings.TrimSpace(prefix), strings.TrimSpace(cmd)
	if prefix == "" || cmd == "" {
		return false
	}
	if strings.ContainsAny(cmd, execMeta) {
		return false
	}
	if !strings.HasPrefix(cmd, prefix) {
		return false
	}
	rest := cmd[len(prefix):]
	if rest == "" {
		return true
	}
	// ASCII blanks only: that is what sh splits words on.
	return rest[0] == ' ' || rest[0] == '\t'
}
