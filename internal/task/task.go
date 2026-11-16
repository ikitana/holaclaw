// Package task owns detached agent runs (docs/tasks.md): the task rows,
// the scheduler that claims a due task and submits its prompt to a task-kind
// session, and the missed-run policy. A run is nothing special — it is one
// message in a session whose kind is `task`, which is what puts it on the task
// lane, keeps it out of recall (docs/sessions.md) and makes the policy gate treat `ask` as
// `deny` unless the task carries a grant (docs/security.md).
//
// Two creation paths share one validator. Through the API a client may pick
// the agent, the delivery target and a grant set. Through the `task.create`
// tool the model may pick nothing that points at a person or a capability:
// agent, delivery and user come from the trusted Call, and a task created by
// an agent carries no grants at all (docs/security.md, origin-derived targets).
package task

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
)

// ErrInvalid wraps every rejected input, so the API layer can tell a bad
// request from an internal failure.
var ErrInvalid = errors.New("task: invalid")

// Limits on what one task may be.
const (
	MaxPrompt = 8000
	// MaxSteps caps the per-task step limit a client may set.
	MaxSteps = 50
	// MaxAhead is how far in the future a one-shot task may be scheduled.
	MaxAhead = 365 * 24 * time.Hour
	// MaxPerUser is how many active tasks one user may have through the
	// `task.create` tool.
	MaxPerUser = 20
	// MinToolInterval is the shortest recurrence a model-created task may ask
	// for. Clients are trusted with anything cron can express.
	MinToolInterval = 5 * time.Minute
)

// ScheduleKind is when a task runs.
type ScheduleKind string

const (
	ScheduleCron ScheduleKind = "cron"
	ScheduleAt   ScheduleKind = "at"
	ScheduleNow  ScheduleKind = "now"
)

// OneShot reports whether a schedule runs exactly once.
func (k ScheduleKind) OneShot() bool { return k == ScheduleAt || k == ScheduleNow }

// Schedule is the persisted schedule of a task.
type Schedule struct {
	Kind ScheduleKind
	Cron string
	At   time.Time
}

// TargetKind is where the result of a run goes.
type TargetKind string

const (
	// TargetSession appends the reply to the chat session the task came from
	// and pushes it through that session's channel.
	TargetSession TargetKind = "session"
	// TargetChannel is reserved: v1 refuses it through the API, because a
	// target that is not derived from a session row is a target the creator
	// chose freely (docs/security.md).
	TargetChannel TargetKind = "channel"
	// TargetStore keeps the result in the task session only.
	TargetStore TargetKind = "store"
)

// Target is the delivery target. Origin is copied from the session row at
// creation and never taken from the request.
type Target struct {
	Kind      TargetKind   `json:"kind"`
	SessionID event.ID     `json:"session_id,omitempty"`
	Origin    event.Origin `json:"origin,omitzero"`
}

// MissedPolicy says what happens to occurrences the daemon slept through.
type MissedPolicy string

const (
	// MissedSkip forgets them and arms the next future occurrence.
	MissedSkip MissedPolicy = "skip"
	// MissedRunOnce runs one catch-up run, never a burst.
	MissedRunOnce MissedPolicy = "run_once"
)

// Status of a task row. Rows are never deleted: the trace refers to them.
type Status string

const (
	StatusActive    Status = "active"
	StatusDone      Status = "done"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// Task is one persisted detached run definition.
type Task struct {
	ID          event.ID
	Agent       string
	Name        string
	Prompt      string
	Schedule    Schedule
	Target      Target
	Grants      []string // tool names, mirrored from the grants table
	Missed      MissedPolicy
	MaxSteps    int
	Status      Status
	NextRun     *time.Time
	LastRun     *time.Time
	LastTraceID event.ID
	ParentTrace event.ID
	CreatedBy   event.ID
	CreatedAt   time.Time
}

// Delivery is what OnRunFinished hands back: the reply, already stored in the
// origin session, plus the session it belongs to, so the daemon can push it
// through that session's channel exactly as it pushes an interactive reply.
type Delivery struct {
	Session store.Session
	Message event.Message
}

// GrantStore is the slice of approval.Service the task service needs: task
// grants are written at creation and revoked when the task is cancelled.
type GrantStore interface {
	AddGrant(ctx context.Context, g tool.Grant) (tool.Grant, error)
	ListGrants(ctx context.Context, agent string) ([]tool.Grant, error)
	RevokeGrant(ctx context.Context, id event.ID) error
}

// Deps is the wiring. DB and Store are required; without Submit the scheduler
// can claim a task but not run it, so the daemon always sets it.
type Deps struct {
	// DB is the shared handle from store.SQLite.DB().
	DB *sql.DB
	// Store owns sessions and messages.
	Store store.Store
	// Submit is session.Manager.Submit: a task run is a message submitted to
	// a task-kind session.
	Submit func(ctx context.Context, sess store.Session, msg event.Message) (bool, error)
	// Grants persists the scoped grant set of a task (approval.Service).
	Grants GrantStore
	// Busy reports whether a session is mid-turn; a task whose previous run is
	// still going skips this occurrence.
	Busy func(sessionID event.ID) bool
	// Agents reports whether an agent name is configured. Nil accepts any
	// non-empty name.
	Agents func(name string) bool
	// KnownTools lists the tools an agent may use, for validating grants. Nil
	// refuses every grant.
	KnownTools func(agent string) []string
	Events     event.Sink
	// Now is the clock; Location is the zone cron expressions are read in.
	Now      func() time.Time
	Location *time.Location
	Logger   *slog.Logger
	// After is the sleep hook of the scheduler loop (tests drive it).
	After func(d time.Duration) <-chan time.Time
}

// Service implements api.TaskBackend and owns the scheduler goroutine.
type Service struct {
	d    Deps
	log  *slog.Logger
	wake chan struct{}
}

// New builds the service. Run starts the scheduler; Recover applies the
// missed-run policy and must run before it.
func New(d Deps) (*Service, error) {
	if d.DB == nil {
		return nil, errors.New("task: no database")
	}
	if d.Store == nil {
		return nil, errors.New("task: no store")
	}
	if d.Events == nil {
		d.Events = event.Discard
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Location == nil {
		d.Location = time.Local
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.After == nil {
		d.After = time.After
	}
	return &Service{d: d, log: d.Logger, wake: make(chan struct{}, 1)}, nil
}

func (s *Service) now() time.Time { return s.d.Now().UTC() }

// Wake re-arms the scheduler: the earliest next_run may have moved.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) emit(ctx context.Context, t event.Type, traceID, sessionID event.ID, attrs any) {
	s.d.Events.Emit(ctx, event.New(t, traceID, sessionID, "", attrs))
}
