package task

import (
	"context"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/tool"
)

// CreateTask schedules a task for a client (POST /v1/tasks). Unlike the tool
// path, a client may pick the agent, the delivery session and a grant set —
// but a session delivery still copies its origin from the session row, and
// the `channel` kind is refused: v1 has no way to check that a freely chosen
// channel target is one the creator may write to (docs/security.md).
func (s *Service) CreateTask(ctx context.Context, in gen.TaskCreate, by event.ID) (gen.Task, error) {
	now := s.now()
	agent, err := s.validateAgent(deref(in.Agent))
	if err != nil {
		return gen.Task{}, err
	}
	prompt, err := validatePrompt(in.Prompt)
	if err != nil {
		return gen.Task{}, err
	}
	name, err := validateName(deref(in.Name))
	if err != nil {
		return gen.Task{}, err
	}
	sch := Schedule{Kind: ScheduleKind(in.Schedule.Kind), Cron: deref(in.Schedule.Cron)}
	if in.Schedule.At != nil {
		sch.At = *in.Schedule.At
	}
	sch, next, err := s.validateSchedule(sch, now)
	if err != nil {
		return gen.Task{}, err
	}
	target := Target{Kind: TargetStore}
	if in.Delivery != nil {
		switch TargetKind(in.Delivery.Kind) {
		case TargetSession:
			id := event.ID(deref(in.Delivery.SessionId))
			if target, err = s.sessionTarget(ctx, id); err != nil {
				return gen.Task{}, err
			}
		case TargetStore:
			target = Target{Kind: TargetStore}
		case TargetChannel:
			return gen.Task{}, invalid("delivery kind channel is not available in v1: " +
				"use kind session, whose target is derived from the session row")
		default:
			return gen.Task{}, invalid("delivery kind must be session or store")
		}
	}
	grants, err := s.validateGrants(agent, derefSlice(in.Grants))
	if err != nil {
		return gen.Task{}, err
	}
	missed, err := validateMissed(MissedPolicy(deref((*string)(in.Missed))))
	if err != nil {
		return gen.Task{}, err
	}
	maxSteps, err := validateMaxSteps(deref(in.MaxSteps))
	if err != nil {
		return gen.Task{}, err
	}
	t, err := s.create(ctx, draft{
		agent: agent, name: name, prompt: prompt, schedule: sch, target: target,
		grants: grants, missed: missed, maxSteps: maxSteps, createdBy: by,
	}, next)
	if err != nil {
		return gen.Task{}, err
	}
	return toWire(t), nil
}

// ListTasks returns every task, newest first.
func (s *Service) ListTasks(ctx context.Context) ([]gen.Task, error) {
	rows, err := s.query(ctx, `ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	out := make([]gen.Task, 0, len(rows))
	for _, t := range rows {
		if err := s.loadGrants(ctx, &t); err != nil {
			return nil, err
		}
		out = append(out, toWire(t))
	}
	return out, nil
}

// GetTask returns one task.
func (s *Service) GetTask(ctx context.Context, id string) (gen.Task, error) {
	t, err := s.get(ctx, event.ID(id))
	if err != nil {
		return gen.Task{}, err
	}
	if err := s.loadGrants(ctx, &t); err != nil {
		return gen.Task{}, err
	}
	return toWire(t), nil
}

// DeleteTask cancels a task and revokes its grants. The row stays: the trace
// of its past runs refers to it (docs/traceability.md).
func (s *Service) DeleteTask(ctx context.Context, id string) error {
	t, err := s.get(ctx, event.ID(id))
	if err != nil {
		return err
	}
	if err := s.setStatus(ctx, t.ID, StatusCancelled); err != nil {
		return err
	}
	if err := s.revokeGrants(ctx, t); err != nil {
		return err
	}
	s.Wake()
	return nil
}

// loadGrants mirrors the task's grant set onto the row for the wire type.
func (s *Service) loadGrants(ctx context.Context, t *Task) error {
	if s.d.Grants == nil {
		return nil
	}
	all, err := s.d.Grants.ListGrants(ctx, t.Agent)
	if err != nil {
		return err
	}
	t.Grants = nil
	for _, g := range all {
		if g.Scope == tool.GrantTask && g.TaskID == t.ID {
			t.Grants = append(t.Grants, g.Tool)
		}
	}
	return nil
}

func (s *Service) revokeGrants(ctx context.Context, t Task) error {
	if s.d.Grants == nil {
		return nil
	}
	all, err := s.d.Grants.ListGrants(ctx, t.Agent)
	if err != nil {
		return err
	}
	for _, g := range all {
		if g.Scope == tool.GrantTask && g.TaskID == t.ID {
			if err := s.d.Grants.RevokeGrant(ctx, g.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// toWire renders a task as the API type.
func toWire(t Task) gen.Task {
	schedule := map[string]any{"kind": string(t.Schedule.Kind)}
	switch t.Schedule.Kind {
	case ScheduleCron:
		schedule["cron"] = t.Schedule.Cron
	case ScheduleAt:
		schedule["at"] = t.Schedule.At.UTC()
	}
	delivery := map[string]any{"kind": string(t.Target.Kind)}
	if t.Target.SessionID != "" {
		delivery["session_id"] = string(t.Target.SessionID)
	}
	out := gen.Task{
		Id: string(t.ID), Agent: t.Agent, Prompt: t.Prompt, Schedule: schedule,
		Delivery: &delivery, Status: gen.TaskStatus(t.Status), CreatedAt: t.CreatedAt.UTC(),
		NextRun: utcPtr(t.NextRun), LastRun: utcPtr(t.LastRun),
	}
	if t.Name != "" {
		out.Name = &t.Name
	}
	if t.Missed != "" {
		m := string(t.Missed)
		out.Missed = &m
	}
	if t.LastTraceID != "" {
		id := string(t.LastTraceID)
		out.LastTraceId = &id
	}
	if len(t.Grants) > 0 {
		g := t.Grants
		out.Grants = &g
	}
	return out
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

func derefSlice(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

// CancelByUser cancels every active task a user created and revokes their
// grants. The daemon calls it when the user is deleted: a task must not keep
// firing, with live grants, on behalf of someone who no longer exists.
func (s *Service) CancelByUser(ctx context.Context, identityID event.ID) (int, error) {
	rows, err := s.d.DB.QueryContext(ctx, `SELECT id FROM tasks WHERE created_by = ? AND status = 'active'`, string(identityID))
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := s.DeleteTask(ctx, id); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// MaxSteps returns the step limit of a task (0 = the agent's default), for
// the daemon to apply to the task's runs.
func (s *Service) MaxSteps(ctx context.Context, id event.ID) int {
	t, err := s.get(ctx, id)
	if err != nil {
		return 0
	}
	return t.MaxSteps
}
