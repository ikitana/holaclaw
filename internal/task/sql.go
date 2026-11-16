package task

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

const taskColumns = `id, agent, name, prompt, schedule_kind, cron, at, delivery_kind, delivery,
	missed, max_steps, status, next_run, last_run, last_trace_id, parent_trace, created_by, created_at`

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func nullMS(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ms(*t)
}

func msPtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMS(v.Int64)
	return &t
}

// insert writes a new task row.
func (s *Service) insert(ctx context.Context, t Task) error {
	target, err := json.Marshal(t.Target)
	if err != nil {
		return fmt.Errorf("task: encode delivery: %w", err)
	}
	var at any
	if t.Schedule.Kind == ScheduleAt {
		at = ms(t.Schedule.At)
	}
	_, err = s.d.DB.ExecContext(ctx, `INSERT INTO tasks (`+taskColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(t.ID), t.Agent, t.Name, t.Prompt, string(t.Schedule.Kind), t.Schedule.Cron, at,
		string(t.Target.Kind), string(target), string(t.Missed), t.MaxSteps, string(t.Status),
		nullMS(t.NextRun), nullMS(t.LastRun), string(t.LastTraceID), string(t.ParentTrace),
		string(t.CreatedBy), ms(t.CreatedAt))
	if err != nil {
		return fmt.Errorf("task: insert: %w", err)
	}
	return nil
}

func scanTask(sc interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var id, kind, deliveryKind, delivery, missed, status, lastTrace, parent, by string
	var at, next, last sql.NullInt64
	var created int64
	if err := sc.Scan(&id, &t.Agent, &t.Name, &t.Prompt, &kind, &t.Schedule.Cron, &at,
		&deliveryKind, &delivery, &missed, &t.MaxSteps, &status, &next, &last,
		&lastTrace, &parent, &by, &created); err != nil {
		return Task{}, err
	}
	t.ID, t.Schedule.Kind = event.ID(id), ScheduleKind(kind)
	if at.Valid {
		t.Schedule.At = fromMS(at.Int64)
	}
	if err := json.Unmarshal([]byte(delivery), &t.Target); err != nil {
		return Task{}, fmt.Errorf("task: decode delivery: %w", err)
	}
	t.Target.Kind = TargetKind(deliveryKind)
	t.Missed, t.Status = MissedPolicy(missed), Status(status)
	t.NextRun, t.LastRun = msPtr(next), msPtr(last)
	t.LastTraceID, t.ParentTrace, t.CreatedBy = event.ID(lastTrace), event.ID(parent), event.ID(by)
	t.CreatedAt = fromMS(created)
	return t, nil
}

// get loads one task. An unknown id is store.ErrNotFound, so the API answers
// 404 without a special case.
func (s *Service) get(ctx context.Context, id event.ID) (Task, error) {
	row := s.d.DB.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, string(id))
	t, err := scanTask(row)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Task{}, store.ErrNotFound
	case err != nil:
		return Task{}, fmt.Errorf("task: get: %w", err)
	}
	return t, nil
}

func (s *Service) query(ctx context.Context, where string, args ...any) ([]Task, error) {
	rows, err := s.d.DB.QueryContext(ctx, `SELECT `+taskColumns+` FROM tasks `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("task: query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("task: query: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("task: query: %w", err)
	}
	return out, nil
}

// creatorEnabled leaves out the tasks of a disabled creator. To the scheduler
// they are in the daemon's downtime: next_run stays where it was, and
// RecoverCreator applies the missed-run policy when the creator is back.
const creatorEnabled = `NOT EXISTS (SELECT 1 FROM identities i WHERE i.id = tasks.created_by AND i.status = 'disabled')`

// due lists the active tasks whose next_run has arrived, earliest first.
func (s *Service) due(ctx context.Context, now time.Time) ([]Task, error) {
	return s.query(ctx, `WHERE status = 'active' AND next_run IS NOT NULL AND next_run <= ? AND `+creatorEnabled+`
		ORDER BY next_run, id`, ms(now))
}

// earliest is the next armed run, if any.
func (s *Service) earliest(ctx context.Context) (time.Time, bool) {
	var v sql.NullInt64
	err := s.d.DB.QueryRowContext(ctx,
		`SELECT MIN(next_run) FROM tasks WHERE status = 'active' AND next_run IS NOT NULL AND `+creatorEnabled).Scan(&v)
	if err != nil || !v.Valid {
		return time.Time{}, false
	}
	return fromMS(v.Int64), true
}

// claim moves next_run forward under the value the scheduler read. Exactly one
// caller can win: a second scheduler, or a second wake-up, finds the row
// changed and gets rows = 0. So does a creator disabled since due read it.
func (s *Service) claim(ctx context.Context, t Task, following *time.Time, now time.Time) (bool, error) {
	if t.NextRun == nil {
		return false, nil
	}
	res, err := s.d.DB.ExecContext(ctx,
		`UPDATE tasks SET next_run = ?, last_run = ? WHERE id = ? AND status = 'active' AND next_run = ? AND `+creatorEnabled,
		nullMS(following), ms(now), string(t.ID), ms(*t.NextRun))
	if err != nil {
		return false, fmt.Errorf("task: claim: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("task: claim: %w", err)
	}
	return n == 1, nil
}

// setNextRun re-arms a task (missed-run policy).
func (s *Service) setNextRun(ctx context.Context, id event.ID, next *time.Time) error {
	_, err := s.d.DB.ExecContext(ctx, `UPDATE tasks SET next_run = ? WHERE id = ?`, nullMS(next), string(id))
	if err != nil {
		return fmt.Errorf("task: set next run: %w", err)
	}
	return nil
}

// setStatus ends a task. A finished task is never armed.
func (s *Service) setStatus(ctx context.Context, id event.ID, st Status) error {
	_, err := s.d.DB.ExecContext(ctx, `UPDATE tasks SET status = ?, next_run = NULL WHERE id = ?`,
		string(st), string(id))
	if err != nil {
		return fmt.Errorf("task: set status: %w", err)
	}
	return nil
}

func (s *Service) setLastTrace(ctx context.Context, id, traceID event.ID) error {
	_, err := s.d.DB.ExecContext(ctx, `UPDATE tasks SET last_trace_id = ? WHERE id = ?`,
		string(traceID), string(id))
	if err != nil {
		return fmt.Errorf("task: set trace: %w", err)
	}
	return nil
}

// countActive is the per-user cap of the `task.create` tool.
func (s *Service) countActive(ctx context.Context, identityID event.ID) (int, error) {
	var n int
	err := s.d.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tasks WHERE status = 'active' AND created_by = ?`, string(identityID)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("task: count tasks: %w", err)
	}
	return n, nil
}
