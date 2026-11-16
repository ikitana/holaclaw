package task

import (
	"context"
	"fmt"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/session"
	"holaclaw/internal/store"
)

// maxSleep bounds how long the scheduler sleeps between checks even when the
// next run is far away, so a suspended laptop or a stepped clock is noticed.
const maxSleep = time.Minute

// maxCatchUp bounds the occurrence counting of the missed-run policy: a task
// that was armed a year ago is reported as "many", not counted one by one.
const maxCatchUp = 1000

// Run is the scheduler: one goroutine that sleeps until the earliest armed
// next_run, claims what is due and submits it. Call Recover first. It returns
// when ctx ends.
func (s *Service) Run(ctx context.Context) error {
	for {
		d := maxSleep
		if next, ok := s.earliest(ctx); ok {
			if wait := next.Sub(s.now()); wait < d {
				d = max(wait, 0)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.wake:
			// Create or Delete moved the earliest run: re-arm without firing.
			continue
		case <-s.d.After(d):
		}
		s.tick(ctx)
	}
}

// tick fires every task whose next_run has arrived.
func (s *Service) tick(ctx context.Context) {
	now := s.now()
	due, err := s.due(ctx, now)
	if err != nil {
		s.log.Error("task: due query failed", "err", err)
		return
	}
	for _, t := range due {
		if ctx.Err() != nil {
			return
		}
		if err := s.fire(ctx, t, now); err != nil {
			s.log.Error("task: run failed", "task", t.ID, "err", err)
		}
	}
}

// following is the occurrence after the one being run: nil for a one-shot
// task, which is what makes its next_run NULL when it is claimed.
func (s *Service) following(t Task, after time.Time) (*time.Time, error) {
	if t.Schedule.Kind != ScheduleCron {
		return nil, nil
	}
	c, err := ParseCron(t.Schedule.Cron, s.d.Location)
	if err != nil {
		// A row with an expression that no longer parses must not be retried
		// forever; disarming it ends the task on the next status write.
		return nil, fmt.Errorf("task %s: %w", t.ID, err)
	}
	next := c.Next(after)
	if next.IsZero() {
		return nil, nil
	}
	return &next, nil
}

// fire claims one occurrence and starts its run. The claim is the whole
// concurrency story: the UPDATE only succeeds for the scheduler that still
// sees the next_run it read, so two daemons — or one daemon woken twice —
// can never run the same occurrence twice.
func (s *Service) fire(ctx context.Context, t Task, now time.Time) error {
	following, err := s.following(t, *t.NextRun)
	if err != nil {
		s.log.Error("task: schedule is unusable, disarming", "task", t.ID, "err", err)
		if serr := s.setStatus(ctx, t.ID, StatusFailed); serr != nil {
			return serr
		}
		return nil
	}
	won, err := s.claim(ctx, t, following, now)
	if err != nil || !won {
		return err
	}
	// Overlap: the previous run of this same task is still going. The
	// occurrence is dropped, not queued — a slow task must not pile up.
	if s.d.Busy != nil && s.d.Busy(t.ID) {
		s.emit(ctx, event.TaskMissed, t.ParentTrace, t.ID, map[string]any{
			"task": string(t.ID), "reason": "overlap", "count": 1, "policy": string(t.Missed),
		})
		return nil
	}
	return s.start(ctx, t, now)
}

// start opens (or reuses) the task session and submits the prompt. The
// session id is the task id, so the policy gate's Call.TaskID — which the
// daemon fills from the session — matches the task's scoped grants.
func (s *Service) start(ctx context.Context, t Task, now time.Time) error {
	if s.d.Submit == nil {
		return fmt.Errorf("task: no submit configured")
	}
	sess, _, err := s.d.Store.EnsureSession(ctx, store.Session{
		ID:     t.ID,
		Key:    session.TaskKey(t.Agent, t.ID),
		Agent:  t.Agent,
		Kind:   store.SessionTask,
		UserID: t.CreatedBy,
		// Synthetic origin: a task run comes from no conversation, and the
		// delivery target lives on the task, not here.
		Origin: event.Origin{
			Channel: "task", ChannelType: "task",
			Conversation: string(t.ID), ConversationKind: event.ConversationDM,
		},
		CreatedAt: now,
	})
	if err != nil {
		return fmt.Errorf("task: session: %w", err)
	}
	trace := event.NewIDAt(now)
	s.emit(ctx, event.TaskStarted, trace, sess.ID, map[string]any{
		"task": string(t.ID), "agent": t.Agent, "trace": string(trace),
		"parent_trace": string(t.ParentTrace),
	})
	msg := event.Message{
		Schema: event.SchemaVersion, ID: event.NewIDAt(now), SessionID: sess.ID, TraceID: trace,
		Role: event.RoleUser, Text: t.Prompt, CreatedAt: now,
	}
	if _, err := s.d.Submit(ctx, sess, msg); err != nil {
		return fmt.Errorf("task: submit: %w", err)
	}
	if err := s.setLastTrace(ctx, t.ID, trace); err != nil {
		return err
	}
	// A one-shot task is done once its only run is under way. If the run
	// fails, OnRunFinished moves it to failed.
	if t.Schedule.Kind.OneShot() {
		return s.setStatus(ctx, t.ID, StatusDone)
	}
	return nil
}

// Recover applies the missed-run policy to everything that came due while the
// daemon was down. It runs before Run and never produces a burst of catch-up
// runs: `skip` forgets the missed occurrences, `run_once` runs exactly one.
func (s *Service) Recover(ctx context.Context) error {
	now := s.now()
	late, err := s.due(ctx, now)
	if err != nil {
		return err
	}
	for _, t := range late {
		if err := s.recoverOne(ctx, t, now); err != nil {
			return err
		}
	}
	s.Wake()
	return nil
}

// RecoverCreator is Recover for one creator, whose tasks came due while they
// were disabled. The daemon calls it right before enabling them, while the
// scheduler still leaves their tasks alone, and calls Wake once they are
// enabled so the re-armed tasks are picked up at once.
func (s *Service) RecoverCreator(ctx context.Context, id event.ID) error {
	now := s.now()
	late, err := s.query(ctx, `WHERE status = 'active' AND next_run IS NOT NULL AND next_run <= ? AND created_by = ?
		ORDER BY next_run, id`, ms(now), string(id))
	if err != nil {
		return err
	}
	for _, t := range late {
		if err := s.recoverOne(ctx, t, now); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) recoverOne(ctx context.Context, t Task, now time.Time) error {
	missed, next := s.missedSince(t, *t.NextRun, now)
	s.emit(ctx, event.TaskMissed, t.ParentTrace, t.ID, map[string]any{
		"task": string(t.ID), "count": missed, "policy": string(t.Missed),
	})
	if t.Missed == MissedRunOnce {
		// Arm it for right now: the first tick claims this single occurrence
		// and advances to the next future one.
		return s.setNextRun(ctx, t.ID, &now)
	}
	if next == nil {
		// A one-shot task whose moment passed unrun never runs at all.
		return s.setStatus(ctx, t.ID, StatusFailed)
	}
	return s.setNextRun(ctx, t.ID, next)
}

// missedSince counts the occurrences between an armed next_run and now, and
// returns the first occurrence still in the future (nil for a one-shot task).
func (s *Service) missedSince(t Task, from, now time.Time) (int, *time.Time) {
	if t.Schedule.Kind != ScheduleCron {
		return 1, nil
	}
	c, err := ParseCron(t.Schedule.Cron, s.d.Location)
	if err != nil {
		return 1, nil
	}
	count, at := 1, from
	for i := 0; i < maxCatchUp; i++ {
		next := c.Next(at)
		if next.IsZero() {
			return count, nil
		}
		at = next
		if at.After(now) {
			return count, &at
		}
		count++
	}
	return count, nil
}

// OnRunFinished is what the daemon calls from its deliver path for a task
// session: it records the trace, closes a one-shot task and performs the
// delivery. For `session` delivery the reply is appended to the origin chat
// session and returned, so the daemon pushes it through that session's
// channel the way it pushes any other reply; `store` delivery keeps it in the
// task session only. A session that is not a task run yields no delivery.
func (s *Service) OnRunFinished(ctx context.Context, sess store.Session, traceID event.ID, reply event.Message, runErr error) (*Delivery, error) {
	if sess.Kind != store.SessionTask {
		return nil, nil
	}
	t, err := s.get(ctx, sess.ID)
	if err != nil {
		return nil, err
	}
	if traceID != "" {
		if err := s.setLastTrace(ctx, t.ID, traceID); err != nil {
			return nil, err
		}
	}
	status := "ok"
	if runErr != nil {
		status = "error"
	}
	s.emit(ctx, event.TaskFinished, traceID, sess.ID, map[string]any{
		"task": string(t.ID), "status": status, "delivery": string(t.Target.Kind),
	})
	if t.Schedule.Kind.OneShot() {
		st := StatusDone
		if runErr != nil {
			st = StatusFailed
		}
		if err := s.setStatus(ctx, t.ID, st); err != nil {
			return nil, err
		}
	}
	if runErr != nil || t.Target.Kind != TargetSession || reply.Text == "" {
		return nil, nil
	}
	origin, err := s.d.Store.GetSession(ctx, t.Target.SessionID)
	if err != nil {
		return nil, fmt.Errorf("task: delivery session: %w", err)
	}
	now := s.now()
	out := event.Message{
		Schema: event.SchemaVersion, ID: event.NewIDAt(now), SessionID: origin.ID, TraceID: traceID,
		Role: event.RoleAssistant, Text: label(t) + reply.Text, CreatedAt: now,
	}
	if _, err := s.d.Store.AppendMessage(ctx, out, false); err != nil {
		return nil, fmt.Errorf("task: deliver: %w", err)
	}
	return &Delivery{Session: origin, Message: out}, nil
}

// label prefixes a delivered reply so the reader can tell it from an answer to
// something they just said.
func label(t Task) string {
	name := t.Name
	if name == "" {
		name = string(t.ID)
	}
	return "[task " + name + "] "
}
