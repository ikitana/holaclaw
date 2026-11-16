package task

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
)

// DefaultAgent is the agent a task without one runs as.
const DefaultAgent = "main"

// draft is the validated shape both creation paths produce. Everything in it
// has already been checked; create() only persists.
type draft struct {
	agent    string
	name     string
	prompt   string
	schedule Schedule
	target   Target
	grants   []string
	missed   MissedPolicy
	maxSteps int

	createdBy   event.ID
	parentTrace event.ID
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// validateAgent resolves and checks the agent name.
func (s *Service) validateAgent(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = DefaultAgent
	}
	if s.d.Agents != nil && !s.d.Agents(name) {
		return "", invalid("unknown agent %q", name)
	}
	return name, nil
}

// MaxName caps the label a task carries into delivery prefixes.
const MaxName = 100

func validateName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if len(n) > MaxName {
		return "", invalid("name is longer than %d bytes", MaxName)
	}
	return strings.Join(strings.Fields(n), " "), nil
}

func validatePrompt(p string) (string, error) {
	p = strings.TrimSpace(p)
	switch {
	case p == "":
		return "", invalid("prompt is required")
	case len(p) > MaxPrompt:
		return "", invalid("prompt is %d bytes, the cap is %d", len(p), MaxPrompt)
	}
	return p, nil
}

// validateSchedule checks one schedule and returns it together with the first
// run. A cron expression is compiled here, so an unparseable one never
// reaches the database.
func (s *Service) validateSchedule(sch Schedule, now time.Time) (Schedule, time.Time, error) {
	switch sch.Kind {
	case ScheduleCron:
		expr := strings.TrimSpace(sch.Cron)
		c, err := ParseCron(expr, s.d.Location)
		if err != nil {
			return Schedule{}, time.Time{}, invalid("%s", err)
		}
		next := c.Next(now)
		if next.IsZero() {
			return Schedule{}, time.Time{}, invalid("cron %q never matches a date", expr)
		}
		return Schedule{Kind: ScheduleCron, Cron: expr}, next, nil
	case ScheduleAt:
		at := sch.At.UTC()
		switch {
		case !at.After(now):
			return Schedule{}, time.Time{}, invalid("at must be in the future")
		case at.Sub(now) > MaxAhead:
			return Schedule{}, time.Time{}, invalid("at is more than a year away")
		}
		return Schedule{Kind: ScheduleAt, At: at}, at, nil
	case ScheduleNow:
		return Schedule{Kind: ScheduleNow}, now, nil
	default:
		return Schedule{}, time.Time{}, invalid("schedule kind must be cron, at or now")
	}
}

// sessionTarget builds a delivery target from a session row. The origin is
// copied from the row, never from the request: a delivery target is derived
// from trusted state (docs/security.md).
func (s *Service) sessionTarget(ctx context.Context, id event.ID) (Target, error) {
	if id == "" {
		return Target{}, invalid("delivery kind session needs a session_id")
	}
	sess, err := s.d.Store.GetSession(ctx, id)
	if err != nil {
		return Target{}, invalid("unknown session %s", id)
	}
	if sess.Kind != store.SessionChat {
		return Target{}, invalid("session %s is not a chat session", id)
	}
	return Target{Kind: TargetSession, SessionID: sess.ID, Origin: sess.Origin}, nil
}

// validateGrants checks that every requested grant names a tool of that agent.
func (s *Service) validateGrants(agent string, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if s.d.KnownTools == nil {
		return nil, invalid("grants are not available: no tool list for agent %q", agent)
	}
	known := s.d.KnownTools(agent)
	out := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if !slices.Contains(known, n) {
			return nil, invalid("agent %q has no tool %q", agent, n)
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

func validateMissed(m MissedPolicy) (MissedPolicy, error) {
	switch m {
	case "":
		return MissedSkip, nil
	case MissedSkip, MissedRunOnce:
		return m, nil
	}
	return "", invalid("missed must be skip or run_once")
}

func validateMaxSteps(n int) (int, error) {
	if n < 0 || n > MaxSteps {
		return 0, invalid("max_steps must be between 0 and %d", MaxSteps)
	}
	return n, nil
}

// create persists a validated draft, writes its grant set and arms the
// scheduler.
func (s *Service) create(ctx context.Context, d draft, next time.Time) (Task, error) {
	now := s.now()
	t := Task{
		ID: event.NewIDAt(now), Agent: d.agent, Name: d.name, Prompt: d.prompt,
		Schedule: d.schedule, Target: d.target, Grants: d.grants, Missed: d.missed,
		MaxSteps: d.maxSteps, Status: StatusActive, NextRun: &next,
		ParentTrace: d.parentTrace, CreatedBy: d.createdBy, CreatedAt: now,
	}
	if err := s.insert(ctx, t); err != nil {
		return Task{}, err
	}
	// Grants are scoped to this task id alone: they turn an `ask` into an
	// `allow` inside this task's runs and nowhere else (docs/security.md).
	for _, name := range d.grants {
		if s.d.Grants == nil {
			return Task{}, invalid("grants are not available in this build")
		}
		if _, err := s.d.Grants.AddGrant(ctx, tool.Grant{
			Agent: t.Agent, Tool: name, Scope: tool.GrantTask, TaskID: t.ID,
			CreatedBy: t.CreatedBy, CreatedAt: now,
		}); err != nil {
			return Task{}, fmt.Errorf("task: grant %s: %w", name, err)
		}
	}
	s.emit(ctx, event.TaskScheduled, t.ParentTrace, "", map[string]any{
		"task": string(t.ID), "agent": t.Agent, "schedule": string(t.Schedule.Kind),
		"delivery": string(t.Target.Kind), "next_run": next.UTC(), "grants": len(t.Grants),
	})
	s.Wake()
	return t, nil
}

// CreateFromTool schedules a task on behalf of a running agent. The model
// supplies name, prompt and schedule; everything that points at a person or a
// capability comes from the call envelope: the delivery target is the calling
// session, the agent is the calling agent, and the task carries no grants, so
// every `ask` decision inside its runs is a deny (docs/security.md, docs/tasks.md).
//
// The signature is the one internal/tool/tools declares as TaskCreator, so the
// tools package never imports this one.
func (s *Service) CreateFromTool(ctx context.Context, c tool.Call, name, prompt, cronExpr string, at time.Time) (string, time.Time, error) {
	now := s.now()
	switch {
	case c.Role == "guest":
		return "", time.Time{}, invalid("guests cannot schedule tasks")
	case c.RunKind == tool.RunTask:
		return "", time.Time{}, invalid("a task cannot schedule another task")
	case c.SessionID == "":
		return "", time.Time{}, invalid("no session to deliver to")
	}
	prompt, err := validatePrompt(prompt)
	if err != nil {
		return "", time.Time{}, err
	}
	sch := Schedule{Kind: ScheduleNow}
	switch {
	case cronExpr != "" && !at.IsZero():
		return "", time.Time{}, invalid("give either cron or a time, not both")
	case cronExpr != "":
		sch = Schedule{Kind: ScheduleCron, Cron: cronExpr}
	case !at.IsZero():
		sch = Schedule{Kind: ScheduleAt, At: at}
	}
	sch, next, err := s.validateSchedule(sch, now)
	if err != nil {
		return "", time.Time{}, err
	}
	// A model-set schedule may not busy-loop the task lane.
	if sch.Kind == ScheduleCron {
		cr, err := ParseCron(sch.Cron, s.d.Location)
		if err != nil {
			return "", time.Time{}, invalid("%s", err)
		}
		if gap := cr.MinInterval(now); gap < MinToolInterval {
			return "", time.Time{}, invalid("a task may run at most every %d minutes, %q runs every %d",
				int(MinToolInterval.Minutes()), sch.Cron, int(gap.Minutes()))
		}
	}
	if n, err := s.countActive(ctx, c.UserID); err != nil {
		return "", time.Time{}, err
	} else if n >= MaxPerUser {
		return "", time.Time{}, invalid("you already have %d active tasks, the cap is %d: delete one first",
			n, MaxPerUser)
	}
	agent, err := s.validateAgent(c.Agent)
	if err != nil {
		return "", time.Time{}, err
	}
	target, err := s.sessionTarget(ctx, c.SessionID)
	if err != nil {
		return "", time.Time{}, err
	}
	label, err := validateName(name)
	if err != nil {
		return "", time.Time{}, err
	}
	t, err := s.create(ctx, draft{
		agent: agent, name: label, prompt: prompt, schedule: sch,
		target: target, missed: MissedSkip, createdBy: c.UserID, parentTrace: c.TraceID,
	}, next)
	if err != nil {
		return "", time.Time{}, err
	}
	return string(t.ID), next, nil
}
