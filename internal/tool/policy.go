package tool

import (
	"context"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// Decision is a policy verdict. Deny always wins.
type Decision string

const (
	Allow Decision = "allow"
	Ask   Decision = "ask"
	Deny  Decision = "deny"
)

// Classes holds one decision per risk class.
type Classes struct {
	Read, Write, External, Exec Decision
}

// For returns the decision for a risk class (Deny for an unknown class).
func (c Classes) For(r Risk) Decision {
	switch r {
	case RiskRead:
		return c.Read
	case RiskWrite:
		return c.Write
	case RiskExternal:
		return c.External
	case RiskExec:
		return c.Exec
	}
	return Deny
}

// Policy is the static policy of one agent, from agent.toml (docs/security.md).
type Policy struct {
	Default Classes             // owner / admin / user on interactive runs
	Guest   Classes             // origin role guest
	Task    Classes             // detached runs; Ask is treated as Deny without a grant
	Tools   map[string]Decision // per-tool override; can tighten or loosen, but a class Deny still wins
	// ExecAllow lists command prefixes that turn an exec Ask into Allow.
	ExecAllow []string
	// TaintEscalates moves exec / external from Allow to Ask once untrusted
	// content entered the turn.
	TaintEscalates bool
	// GuardMode says how a guard verdict is used; "" means GuardRecommend.
	GuardMode GuardMode
	// GuardThreshold is the score at or above which a call counts as safe;
	// 0 means DefaultGuardThreshold.
	GuardThreshold float64
	// TaintDecayCalls is how many tool calls after untrusted content entered
	// the turn still count as tainted; 0 means DefaultTaintDecayCalls.
	TaintDecayCalls int
}

// GuardMode is how the gate uses the guard (docs/security.md, "Guard modes").
type GuardMode string

const (
	// GuardRecommend: the guard is advice and an extra question, never an extra
	// permission. Below the threshold an Allow becomes Ask; an Ask stays Ask and
	// the approval carries the verdict. A guard error means Ask.
	GuardRecommend GuardMode = "recommend"
	// GuardAuto: nobody is asked. At or above the threshold the call runs,
	// below it is denied, and a guard error is a deny. Two exceptions, both
	// towards caution: a Deny is never lifted, and an exec / external call in a
	// tainted turn is never allowed by the guard — it goes to a human.
	GuardAuto GuardMode = "auto"
)

// DefaultGuardThreshold is the score a call needs to count as safe. Measured on
// the 28 synthetic guard cases (2026-09-19, typesafe/jev): risky calls scored
// at most 0.31, benign ones at least 0.74; a non-benign label caps at 0.5.
const DefaultGuardThreshold = 0.7

// DefaultTaintDecayCalls is how many tool calls after untrusted content
// enters the turn still count as tainted, when unconfigured.
const DefaultTaintDecayCalls = 3

// GrantScope is how long an approval lasts.
type GrantScope string

const (
	GrantOnce    GrantScope = "once"    // not persisted
	GrantSession GrantScope = "session" // this session only
	GrantAlways  GrantScope = "always"  // this agent, any session
	GrantTask    GrantScope = "task"    // set at task creation, scoped to that task
)

// Grant is a persisted rule that turns Ask into Allow. It never lifts a Deny.
type Grant struct {
	ID        event.ID
	Agent     string
	Tool      string // exact tool name
	Pattern   string // optional argument pattern: for exec, a command prefix; "" = any
	Scope     GrantScope
	SessionID event.ID // GrantSession
	TaskID    event.ID // GrantTask
	CreatedBy event.ID
	CreatedAt time.Time
}

// ApprovalStatus of a request. Timeout = deny.
type ApprovalStatus string

const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalApproved ApprovalStatus = "approved"
	ApprovalDenied   ApprovalStatus = "denied"
	ApprovalExpired  ApprovalStatus = "expired"
)

// Approval is a pending or resolved human decision on a tool call, persisted in
// the DB.
type Approval struct {
	ID        event.ID
	Status    ApprovalStatus
	TraceID   event.ID
	SessionID event.ID
	Agent     string
	// The call.
	Tool    string
	Summary string // short, human-readable, secrets-free description of the call
	Args    string // raw JSON, capped
	// Resolution.
	Choice     GrantScope // once | session | always (approved only)
	ResolvedBy event.ID
	CreatedAt  time.Time
	ExpiresAt  time.Time
	ResolvedAt *time.Time
}

// Verdict is what a guard says about one call.
type Verdict struct {
	// Score is how safe the call is to run unasked, 0..1. A guard that only
	// knows allow / ask reports 1 / 0.
	Score float64
	// Label is a fixed string from a closed set owned by the guard package
	// ("guard: destructive"), never model text: it reaches traces and approval
	// prompts, and model text could carry injected instructions.
	Label string
	// Usage is what the classifier call cost; nil when no call was answered
	// — a verdict replayed from a cache was not paid for again. A guard that
	// fails after its reply arrived returns the error with a Verdict holding
	// only the Usage.
	Usage *provider.Usage
}

// Guard is the optional classifier model. It never sees a Deny; what its
// verdict may change is decided by the gate from Policy.GuardMode, never by
// the guard itself.
type Guard interface {
	Classify(ctx context.Context, c Call, t Tool) (Verdict, error)
}

// Approver requests a human decision and blocks until it is resolved, expires
// or ctx ends. Only the calling session suspends.
type Approver interface {
	Request(ctx context.Context, a Approval) (Approval, error)
}
