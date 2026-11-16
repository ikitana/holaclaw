package task

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/session"
	"holaclaw/internal/store"
)

// newTask creates a task through the API path and returns the row.
func (f *fixture) newTask(t *testing.T, mut func(*gen.TaskCreate)) Task {
	t.Helper()
	out, err := f.svc.CreateTask(t.Context(), taskCreate(mut), "u1")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return f.get(t, event.ID(out.Id))
}

func deliverToSession(sess store.Session) func(*gen.TaskCreate) {
	return func(c *gen.TaskCreate) {
		c.Delivery = &struct {
			Kind      gen.TaskCreateDeliveryKind `json:"kind"`
			SessionId *string                    `json:"session_id,omitempty"`
		}{Kind: "session", SessionId: ptr(string(sess.ID))}
	}
}

func TestTickRunsANowTask(t *testing.T) {
	f := newFixture(t)
	f.user(t, "u1")
	task := f.newTask(t, func(c *gen.TaskCreate) {
		c.Schedule.Kind, c.Schedule.Cron = "now", nil
		c.Prompt = "water the plants"
	})
	f.svc.tick(t.Context())

	runs := f.runs.all()
	if len(runs) != 1 {
		t.Fatalf("%d runs, want 1", len(runs))
	}
	got := runs[0]
	switch {
	case got.Session.Kind != store.SessionTask:
		t.Fatalf("session kind = %q", got.Session.Kind)
	case got.Session.ID != task.ID:
		t.Fatalf("session id = %s, want the task id %s", got.Session.ID, task.ID)
	case got.Session.Key != session.TaskKey("main", task.ID):
		t.Fatalf("session key = %q", got.Session.Key)
	case got.Session.Origin.Channel != "task" || got.Session.Origin.ChannelType != "task":
		t.Fatalf("origin = %+v, want the synthetic task origin", got.Session.Origin)
	case got.Session.Origin.Conversation != string(task.ID):
		t.Fatalf("origin conversation = %q", got.Session.Origin.Conversation)
	case got.Message.Role != event.RoleUser || got.Message.Text != "water the plants":
		t.Fatalf("message = %+v", got.Message)
	case got.Message.TraceID == "" || got.Message.TraceID == task.ParentTrace:
		t.Fatalf("run trace = %q, want a fresh one", got.Message.TraceID)
	}
	after := f.get(t, task.ID)
	if after.Status != StatusDone || after.NextRun != nil || after.LastRun == nil {
		t.Fatalf("after the run: status %s next %v last %v", after.Status, after.NextRun, after.LastRun)
	}
	if after.LastTraceID != got.Message.TraceID {
		t.Fatalf("last trace = %q, want %q", after.LastTraceID, got.Message.TraceID)
	}
	started := f.events.typed(event.TaskStarted)
	if len(started) != 1 {
		t.Fatalf("%d task.started events, want 1", len(started))
	}

	// A second tick has nothing to do: the claim disarmed the task.
	f.svc.tick(t.Context())
	if n := f.runs.count(); n != 1 {
		t.Fatalf("%d runs after a second tick, want 1", n)
	}
}

func TestAtTaskRunsAtItsTime(t *testing.T) {
	f := newFixture(t)
	f.user(t, "u1")
	when := baseTime.Add(30 * time.Minute)
	task := f.newTask(t, func(c *gen.TaskCreate) {
		c.Schedule.Kind, c.Schedule.Cron, c.Schedule.At = "at", nil, &when
	})
	f.svc.tick(t.Context())
	if n := f.runs.count(); n != 0 {
		t.Fatalf("%d runs before the time, want 0", n)
	}
	f.clock.Set(when)
	f.svc.tick(t.Context())
	if n := f.runs.count(); n != 1 {
		t.Fatalf("%d runs at the time, want 1", n)
	}
	if got := f.get(t, task.ID); got.Status != StatusDone {
		t.Fatalf("status = %s, want done", got.Status)
	}
}

func TestRecurringTaskAdvances(t *testing.T) {
	f := newFixture(t)
	f.user(t, "u1")
	task := f.newTask(t, nil) // 0 9 * * *
	if want := baseTime.Add(time.Hour); !task.NextRun.Equal(want) {
		t.Fatalf("next_run = %s, want %s", task.NextRun, want)
	}
	f.clock.Add(time.Hour)
	f.svc.tick(t.Context())
	got := f.get(t, task.ID)
	if n := f.runs.count(); n != 1 {
		t.Fatalf("%d runs, want 1", n)
	}
	if got.Status != StatusActive {
		t.Fatalf("status = %s, want active", got.Status)
	}
	if want := baseTime.Add(25 * time.Hour); !got.NextRun.Equal(want) {
		t.Fatalf("next_run = %s, want %s", got.NextRun, want)
	}
	// The same session is reused by the next run.
	f.clock.Set(*got.NextRun)
	f.svc.tick(t.Context())
	runs := f.runs.all()
	if len(runs) != 2 {
		t.Fatalf("%d runs, want 2", len(runs))
	}
	if runs[0].Session.ID != runs[1].Session.ID {
		t.Fatal("the second run opened another session")
	}
	if runs[0].Message.TraceID == runs[1].Message.TraceID {
		t.Fatal("both runs share a trace id")
	}
}

// Eight schedulers over one database, all woken for the same occurrence: the
// conditional UPDATE lets exactly one of them run it.
func TestClaimIsAtomicAcrossSchedulers(t *testing.T) {
	f := newFixture(t)
	f.user(t, "u1")
	task := f.newTask(t, nil)
	f.clock.Add(time.Hour)

	const n = 8
	svcs := make([]*Service, 0, n)
	for i := 0; i < n; i++ {
		s, err := New(Deps{
			DB: f.store.DB(), Store: f.store, Submit: f.runs.submit, Events: f.events,
			Now: f.clock.Now, Location: time.UTC,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		svcs = append(svcs, s)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, s := range svcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s.tick(context.Background())
		}()
	}
	close(start)
	wg.Wait()

	if got := f.runs.count(); got != 1 {
		t.Fatalf("%d runs of one occurrence, want exactly 1", got)
	}
	if got := f.get(t, task.ID); !got.NextRun.Equal(baseTime.Add(25 * time.Hour)) {
		t.Fatalf("next_run = %s", got.NextRun)
	}
}

func TestOverlapSkipsTheOccurrence(t *testing.T) {
	f := newFixture(t)
	f.user(t, "u1")
	task := f.newTask(t, nil)
	f.setBusy(task.ID, true)
	f.clock.Add(time.Hour)
	f.svc.tick(t.Context())

	if n := f.runs.count(); n != 0 {
		t.Fatalf("%d runs while busy, want 0", n)
	}
	missed := f.events.typed(event.TaskMissed)
	if len(missed) != 1 {
		t.Fatalf("%d task.missed events, want 1", len(missed))
	}
	if !strings.Contains(string(missed[0].Attrs), `"reason":"overlap"`) {
		t.Fatalf("attrs = %s", missed[0].Attrs)
	}
	// The occurrence is dropped, not queued: the task is armed for the next.
	got := f.get(t, task.ID)
	if !got.NextRun.Equal(baseTime.Add(25 * time.Hour)) {
		t.Fatalf("next_run = %s", got.NextRun)
	}
	f.setBusy(task.ID, false)
	f.clock.Set(*got.NextRun)
	f.svc.tick(t.Context())
	if n := f.runs.count(); n != 1 {
		t.Fatalf("%d runs once free, want 1", n)
	}
}

func TestRecoverSkipsMissedOccurrences(t *testing.T) {
	f := newFixture(t)
	f.user(t, "u1")
	task := f.newTask(t, func(c *gen.TaskCreate) { c.Schedule.Cron = ptr("0 * * * *") })
	// Three days pass with the daemon down.
	f.clock.Add(72 * time.Hour)
	if err := f.svc.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n := f.runs.count(); n != 0 {
		t.Fatalf("%d catch-up runs with policy skip, want 0", n)
	}
	missed := f.events.typed(event.TaskMissed)
	if len(missed) != 1 {
		t.Fatalf("%d task.missed events, want 1", len(missed))
	}
	if !strings.Contains(string(missed[0].Attrs), `"policy":"skip"`) || !strings.Contains(string(missed[0].Attrs), `"count":72`) {
		t.Fatalf("attrs = %s", missed[0].Attrs)
	}
	got := f.get(t, task.ID)
	if !got.NextRun.After(f.clock.Now()) {
		t.Fatalf("next_run %s is not in the future", got.NextRun)
	}
	// And nothing is due right now.
	f.svc.tick(t.Context())
	if n := f.runs.count(); n != 0 {
		t.Fatalf("%d runs after recovery, want 0", n)
	}
}

func TestRecoverRunsOnceWithoutABurst(t *testing.T) {
	f := newFixture(t)
	f.user(t, "u1")
	task := f.newTask(t, func(c *gen.TaskCreate) {
		c.Schedule.Cron = ptr("0 * * * *")
		c.Missed = ptr(gen.TaskCreateMissed(MissedRunOnce))
	})
	f.clock.Add(10 * time.Hour)
	if err := f.svc.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	f.svc.tick(t.Context())
	f.svc.tick(t.Context())
	if n := f.runs.count(); n != 1 {
		t.Fatalf("%d catch-up runs with policy run_once, want exactly 1", n)
	}
	got := f.get(t, task.ID)
	if !got.NextRun.After(f.clock.Now()) {
		t.Fatalf("next_run %s is not in the future", got.NextRun)
	}
}

func TestRecoverFailsAMissedOneShot(t *testing.T) {
	f := newFixture(t)
	f.user(t, "u1")
	when := baseTime.Add(time.Hour)
	task := f.newTask(t, func(c *gen.TaskCreate) {
		c.Schedule.Kind, c.Schedule.Cron, c.Schedule.At = "at", nil, &when
	})
	f.clock.Add(5 * time.Hour)
	if err := f.svc.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got := f.get(t, task.ID)
	if got.Status != StatusFailed || got.NextRun != nil {
		t.Fatalf("status %s, next %v; want failed and disarmed", got.Status, got.NextRun)
	}
	if n := f.runs.count(); n != 0 {
		t.Fatalf("%d runs, want 0", n)
	}
}

func TestRecoverRunsAMissedOneShotWithRunOnce(t *testing.T) {
	f := newFixture(t)
	f.user(t, "u1")
	when := baseTime.Add(time.Hour)
	task := f.newTask(t, func(c *gen.TaskCreate) {
		c.Schedule.Kind, c.Schedule.Cron, c.Schedule.At = "at", nil, &when
		c.Missed = ptr(gen.TaskCreateMissed(MissedRunOnce))
	})
	f.clock.Add(5 * time.Hour)
	if err := f.svc.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	f.svc.tick(t.Context())
	f.svc.tick(t.Context())
	if n := f.runs.count(); n != 1 {
		t.Fatalf("%d runs, want 1", n)
	}
	if got := f.get(t, task.ID); got.Status != StatusDone {
		t.Fatalf("status = %s, want done", got.Status)
	}
}

func TestOnRunFinishedDeliversToTheOriginSession(t *testing.T) {
	f := newFixture(t)
	chat := f.chatSession(t, "u1")
	task := f.newTask(t, func(c *gen.TaskCreate) {
		deliverToSession(chat)(c)
		c.Schedule.Kind, c.Schedule.Cron = "now", nil
		c.Name = ptr("digest")
	})
	f.svc.tick(t.Context())
	run := f.runs.all()[0]

	reply := event.Message{Role: event.RoleAssistant, Text: "all clear"}
	got, err := f.svc.OnRunFinished(t.Context(), run.Session, run.Message.TraceID, reply, nil)
	if err != nil {
		t.Fatalf("OnRunFinished: %v", err)
	}
	if got == nil {
		t.Fatal("no delivery, want one to the origin session")
	}
	if got.Session.ID != chat.ID {
		t.Fatalf("delivered to %s, want the origin session %s", got.Session.ID, chat.ID)
	}
	if got.Message.Text != "[task digest] all clear" {
		t.Fatalf("text = %q", got.Message.Text)
	}
	msgs, err := f.store.ListMessages(t.Context(), chat.ID, "", 0)
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != got.Message.Text || msgs[0].Role != event.RoleAssistant {
		t.Fatalf("origin transcript = %+v", msgs)
	}
	fin := f.events.typed(event.TaskFinished)
	if len(fin) != 1 || !strings.Contains(string(fin[0].Attrs), `"status":"ok"`) {
		t.Fatalf("task.finished events = %+v", fin)
	}
	if got := f.get(t, task.ID); got.LastTraceID != run.Message.TraceID {
		t.Fatalf("last trace = %q", got.LastTraceID)
	}
}

func TestOnRunFinishedStoreDeliveryAndFailure(t *testing.T) {
	f := newFixture(t)
	f.user(t, "u1")
	task := f.newTask(t, func(c *gen.TaskCreate) {
		c.Schedule.Kind, c.Schedule.Cron = "now", nil
	})
	f.svc.tick(t.Context())
	run := f.runs.all()[0]

	// store delivery keeps the result in the task session.
	got, err := f.svc.OnRunFinished(t.Context(), run.Session, run.Message.TraceID,
		event.Message{Text: "done"}, nil)
	if err != nil || got != nil {
		t.Fatalf("OnRunFinished = %v, %v; want no delivery", got, err)
	}

	// A failed run of a one-shot task ends as failed.
	if _, err := f.svc.OnRunFinished(t.Context(), run.Session, run.Message.TraceID,
		event.Message{}, errors.New("model down")); err != nil {
		t.Fatalf("OnRunFinished: %v", err)
	}
	if got := f.get(t, task.ID); got.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
	fin := f.events.typed(event.TaskFinished)
	if len(fin) != 2 || !strings.Contains(string(fin[1].Attrs), `"status":"error"`) {
		t.Fatalf("task.finished events = %+v", fin)
	}
}

func TestOnRunFinishedIgnoresAChatSession(t *testing.T) {
	f := newFixture(t)
	chat := f.chatSession(t, "u1")
	got, err := f.svc.OnRunFinished(t.Context(), chat, "t", event.Message{Text: "hi"}, nil)
	if err != nil || got != nil {
		t.Fatalf("OnRunFinished on a chat session = %v, %v", got, err)
	}
}

func TestOnRunFinishedKeepsARecurringTaskActive(t *testing.T) {
	f := newFixture(t)
	f.user(t, "u1")
	task := f.newTask(t, nil)
	f.clock.Add(time.Hour)
	f.svc.tick(t.Context())
	run := f.runs.all()[0]
	if _, err := f.svc.OnRunFinished(t.Context(), run.Session, run.Message.TraceID,
		event.Message{Text: "ok"}, errors.New("boom")); err != nil {
		t.Fatalf("OnRunFinished: %v", err)
	}
	got := f.get(t, task.ID)
	if got.Status != StatusActive || got.NextRun == nil {
		t.Fatalf("status %s next %v; a failed occurrence must not end a recurring task", got.Status, got.NextRun)
	}
}

// The loop sleeps until the earliest next_run, re-arms on a wake and fires
// when its timer does. Nothing here sleeps for real: After is a test channel.
func TestRunLoop(t *testing.T) {
	f := newFixture(t)
	f.user(t, "u1")
	waits := make(chan time.Duration, 8)
	fire := make(chan time.Time)
	svc, err := New(Deps{
		DB: f.store.DB(), Store: f.store, Submit: f.runs.submit, Events: f.events,
		Now: f.clock.Now, Location: time.UTC,
		After: func(d time.Duration) <-chan time.Time {
			select {
			case waits <- d:
			default:
			}
			return fire
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	// Nothing armed: the loop waits its maximum.
	if d := <-waits; d != maxSleep {
		t.Fatalf("idle wait = %s, want %s", d, maxSleep)
	}
	task := f.newTask(t, nil) // due in an hour
	svc.Wake()
	// Still the maximum: a far-away run is re-checked every minute so a
	// stepped clock cannot be slept through.
	if d := <-waits; d != maxSleep {
		t.Fatalf("armed wait = %s, want %s", d, maxSleep)
	}
	// A run inside the next minute is slept for exactly.
	soon := baseTime.Add(30 * time.Second)
	f.newTask(t, func(c *gen.TaskCreate) {
		c.Schedule.Kind, c.Schedule.Cron, c.Schedule.At = "at", nil, &soon
	})
	svc.Wake()
	if d := <-waits; d != 30*time.Second {
		t.Fatalf("near wait = %s, want 30s", d)
	}
	f.clock.Set(soon)
	fire <- f.clock.Now()
	// The next wait tells us the tick has run.
	<-waits
	if n := f.runs.count(); n != 1 {
		t.Fatalf("%d runs, want 1", n)
	}
	f.clock.Add(time.Hour)
	fire <- f.clock.Now()
	<-waits
	if n := f.runs.count(); n != 2 {
		t.Fatalf("%d runs, want 2", n)
	}
	if got := f.get(t, task.ID); !got.NextRun.Equal(baseTime.Add(25 * time.Hour)) {
		t.Fatalf("next_run = %s", got.NextRun)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}
