package task

import (
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
)

func (f *fixture) setCreator(t *testing.T, id event.ID, status string) {
	t.Helper()
	if _, err := f.store.DB().ExecContext(t.Context(), `UPDATE identities SET status = ? WHERE id = ?`, status, string(id)); err != nil {
		t.Fatal(err)
	}
}

// A disabled creator's task is in downtime: nothing fires, nothing is claimed,
// and the scheduler doesn't spin on the next_run left in the past.
func TestDisabledCreatorsTaskDoesNotRun(t *testing.T) {
	f := newFixture(t)
	f.user(t, "u1")
	task := f.newTask(t, func(c *gen.TaskCreate) {
		c.Schedule.Kind, c.Schedule.Cron = "cron", ptr("0 * * * *")
	})
	armed := *task.NextRun
	f.setCreator(t, "u1", "disabled")
	f.clock.Add(3 * time.Hour)

	f.svc.tick(t.Context())
	if f.runs.count() != 0 {
		t.Fatalf("runs = %d", f.runs.count())
	}
	if got := f.get(t, task.ID); !got.NextRun.Equal(armed) {
		t.Fatalf("next_run moved to %v", got.NextRun)
	}
	if _, ok := f.svc.earliest(t.Context()); ok {
		t.Fatal("earliest counts a disabled creator's task")
	}
	// A restart while disabled leaves it on hold as well.
	if err := f.svc.Recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.get(t, task.ID); !got.NextRun.Equal(armed) || f.events.has(event.TaskMissed) {
		t.Fatalf("Recover touched it: %v", got.NextRun)
	}
}

// A creator disabled between due and the claim: the claim loses, nothing runs
// and next_run stays for the missed-run policy.
func TestDisabledBetweenDueAndClaim(t *testing.T) {
	f := newFixture(t)
	f.user(t, "u1")
	task := f.newTask(t, func(c *gen.TaskCreate) {
		c.Schedule.Kind, c.Schedule.Cron = "cron", ptr("0 * * * *")
	})
	armed := *task.NextRun
	f.clock.Add(time.Hour + time.Minute)
	now := f.clock.Now()
	due, err := f.svc.due(t.Context(), now)
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %v, %v", due, err)
	}
	f.setCreator(t, "u1", "disabled")
	if err := f.svc.fire(t.Context(), due[0], now); err != nil {
		t.Fatal(err)
	}
	if f.runs.count() != 0 {
		t.Fatalf("runs = %d", f.runs.count())
	}
	if got := f.get(t, task.ID); !got.NextRun.Equal(armed) || got.LastRun != nil {
		t.Fatalf("claimed: next_run %v, last_run %v", got.NextRun, got.LastRun)
	}
}

// Other creators' tasks aren't affected.
func TestDisabledCreatorLeavesOthersRunning(t *testing.T) {
	f := newFixture(t)
	f.user(t, "u1")
	f.user(t, "u2")
	f.newTask(t, func(c *gen.TaskCreate) { c.Schedule.Kind, c.Schedule.Cron = "now", nil })
	f.setCreator(t, "u2", "disabled")
	f.svc.tick(t.Context())
	if f.runs.count() != 1 {
		t.Fatalf("runs = %d", f.runs.count())
	}
}

// Enabling behaves like the daemon coming back up: the missed-run policy, and
// no burst. As the daemon does, the tasks are recovered before the creator
// reads as enabled, so the first tick after it finds nothing stale.
func TestRecoverCreator(t *testing.T) {
	t.Run("skip forgets what it missed", func(t *testing.T) {
		f := newFixture(t)
		f.user(t, "u1")
		task := f.newTask(t, func(c *gen.TaskCreate) {
			c.Schedule.Kind, c.Schedule.Cron = "cron", ptr("0 * * * *")
		})
		f.setCreator(t, "u1", "disabled")
		f.clock.Add(3*time.Hour + time.Minute)
		if err := f.svc.RecoverCreator(t.Context(), "u1"); err != nil {
			t.Fatal(err)
		}
		f.setCreator(t, "u1", "confirmed")
		f.svc.tick(t.Context())
		if f.runs.count() != 0 {
			t.Fatalf("runs = %d, want none", f.runs.count())
		}
		if got := f.get(t, task.ID); !got.NextRun.After(f.clock.Now()) {
			t.Fatalf("next_run = %v, want in the future", got.NextRun)
		}
		if !f.events.has(event.TaskMissed) {
			t.Fatal("no task.missed")
		}
	})

	t.Run("run_once runs one", func(t *testing.T) {
		f := newFixture(t)
		f.user(t, "u1")
		f.newTask(t, func(c *gen.TaskCreate) {
			c.Schedule.Kind, c.Schedule.Cron = "cron", ptr("0 * * * *")
			c.Missed = ptr(gen.RunOnce)
		})
		f.setCreator(t, "u1", "disabled")
		f.clock.Add(3*time.Hour + time.Minute)
		if err := f.svc.RecoverCreator(t.Context(), "u1"); err != nil {
			t.Fatal(err)
		}
		f.svc.tick(t.Context())
		if f.runs.count() != 0 {
			t.Fatalf("ran before the creator was enabled: %d", f.runs.count())
		}
		f.setCreator(t, "u1", "confirmed")
		f.svc.tick(t.Context())
		f.svc.tick(t.Context())
		if f.runs.count() != 1 {
			t.Fatalf("runs = %d, want exactly one", f.runs.count())
		}
	})

	t.Run("a one-shot whose moment passed fails", func(t *testing.T) {
		f := newFixture(t)
		f.user(t, "u1")
		task := f.newTask(t, func(c *gen.TaskCreate) {
			at := f.clock.Now().Add(time.Hour)
			c.Schedule.Kind, c.Schedule.Cron, c.Schedule.At = "at", nil, &at
		})
		f.setCreator(t, "u1", "disabled")
		f.clock.Add(2 * time.Hour)
		if err := f.svc.RecoverCreator(t.Context(), "u1"); err != nil {
			t.Fatal(err)
		}
		if got := f.get(t, task.ID); got.Status != StatusFailed {
			t.Fatalf("status = %s", got.Status)
		}
	})
}
