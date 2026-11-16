package task

import (
	"context"
	"errors"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
)

// create builds a minimal valid request and lets the case mutate it.
func taskCreate(mut func(*gen.TaskCreate)) gen.TaskCreate {
	in := gen.TaskCreate{Prompt: "check the build"}
	in.Schedule.Kind = gen.TaskCreateScheduleKind(ScheduleCron)
	in.Schedule.Cron = ptr("0 9 * * *")
	if mut != nil {
		mut(&in)
	}
	return in
}

func TestCreateTaskValidation(t *testing.T) {
	f := newFixture(t)
	sess := f.chatSession(t, "u1")
	future := baseTime.Add(time.Hour)

	cases := []struct {
		name string
		in   gen.TaskCreate
		want string // "" = accepted
	}{
		{"cron", taskCreate(nil), ""},
		{"at in the future", taskCreate(func(c *gen.TaskCreate) {
			c.Schedule.Kind, c.Schedule.Cron, c.Schedule.At = "at", nil, &future
		}), ""},
		{"now", taskCreate(func(c *gen.TaskCreate) {
			c.Schedule.Kind, c.Schedule.Cron = "now", nil
		}), ""},
		{"unknown agent", taskCreate(func(c *gen.TaskCreate) { c.Agent = ptr("ghost") }), "unknown agent"},
		{"known agent", taskCreate(func(c *gen.TaskCreate) { c.Agent = ptr("other") }), ""},
		{"empty prompt", taskCreate(func(c *gen.TaskCreate) { c.Prompt = "   " }), "prompt is required"},
		{"long prompt", taskCreate(func(c *gen.TaskCreate) {
			c.Prompt = string(make([]byte, MaxPrompt+1))
		}), "the cap is"},
		{"long name", taskCreate(func(c *gen.TaskCreate) {
			c.Name = ptr(string(make([]byte, MaxName+1)))
		}), "name is longer"},
		{"bad cron", taskCreate(func(c *gen.TaskCreate) { c.Schedule.Cron = ptr("0 99 * * *") }), "cron: hour"},
		{"impossible cron", taskCreate(func(c *gen.TaskCreate) {
			c.Schedule.Cron = ptr("0 0 30 2 *")
		}), "never matches"},
		{"at in the past", taskCreate(func(c *gen.TaskCreate) {
			past := baseTime.Add(-time.Hour)
			c.Schedule.Kind, c.Schedule.Cron, c.Schedule.At = "at", nil, &past
		}), "must be in the future"},
		{"at too far away", taskCreate(func(c *gen.TaskCreate) {
			far := baseTime.Add(2 * 365 * 24 * time.Hour)
			c.Schedule.Kind, c.Schedule.Cron, c.Schedule.At = "at", nil, &far
		}), "more than a year"},
		{"unknown schedule kind", taskCreate(func(c *gen.TaskCreate) {
			c.Schedule.Kind = "whenever"
		}), "schedule kind must be"},
		{"session delivery", taskCreate(func(c *gen.TaskCreate) {
			c.Delivery = &struct {
				Kind      gen.TaskCreateDeliveryKind `json:"kind"`
				SessionId *string                    `json:"session_id,omitempty"`
			}{Kind: "session", SessionId: ptr(string(sess.ID))}
		}), ""},
		{"session delivery without an id", taskCreate(func(c *gen.TaskCreate) {
			c.Delivery = &struct {
				Kind      gen.TaskCreateDeliveryKind `json:"kind"`
				SessionId *string                    `json:"session_id,omitempty"`
			}{Kind: "session"}
		}), "needs a session_id"},
		{"session delivery to an unknown session", taskCreate(func(c *gen.TaskCreate) {
			c.Delivery = &struct {
				Kind      gen.TaskCreateDeliveryKind `json:"kind"`
				SessionId *string                    `json:"session_id,omitempty"`
			}{Kind: "session", SessionId: ptr("nope")}
		}), "unknown session"},
		{"channel delivery is refused in v1", taskCreate(func(c *gen.TaskCreate) {
			c.Delivery = &struct {
				Kind      gen.TaskCreateDeliveryKind `json:"kind"`
				SessionId *string                    `json:"session_id,omitempty"`
			}{Kind: "channel"}
		}), "not available in v1"},
		{"store delivery", taskCreate(func(c *gen.TaskCreate) {
			c.Delivery = &struct {
				Kind      gen.TaskCreateDeliveryKind `json:"kind"`
				SessionId *string                    `json:"session_id,omitempty"`
			}{Kind: "store"}
		}), ""},
		{"grants of that agent", taskCreate(func(c *gen.TaskCreate) {
			c.Grants = &[]string{"files.write"}
		}), ""},
		{"grants of a tool the agent lacks", taskCreate(func(c *gen.TaskCreate) {
			c.Grants = &[]string{"exec"}
		}), `has no tool "exec"`},
		{"missed policy", taskCreate(func(c *gen.TaskCreate) {
			c.Missed = ptr(gen.TaskCreateMissed("run_once"))
		}), ""},
		{"bad missed policy", taskCreate(func(c *gen.TaskCreate) {
			c.Missed = ptr(gen.TaskCreateMissed("retry"))
		}), "missed must be"},
		{"max steps", taskCreate(func(c *gen.TaskCreate) { c.MaxSteps = ptr(10) }), ""},
		{"max steps over the cap", taskCreate(func(c *gen.TaskCreate) {
			c.MaxSteps = ptr(MaxSteps + 1)
		}), "max_steps must be"},
		{"negative max steps", taskCreate(func(c *gen.TaskCreate) { c.MaxSteps = ptr(-1) }), "max_steps must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.svc.CreateTask(t.Context(), tc.in, "u1")
			if tc.want == "" {
				if err != nil {
					t.Fatalf("CreateTask: %v", err)
				}
				if got.Id == "" || got.Status != gen.TaskStatus(StatusActive) || got.NextRun == nil {
					t.Fatalf("task is not armed: %+v", got)
				}
				return
			}
			mustContain(t, err, tc.want)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error %v does not wrap ErrInvalid", err)
			}
		})
	}
}

func TestCreateTaskCopiesTheSessionOrigin(t *testing.T) {
	f := newFixture(t)
	sess := f.chatSession(t, "u1")
	in := taskCreate(func(c *gen.TaskCreate) {
		c.Delivery = &struct {
			Kind      gen.TaskCreateDeliveryKind `json:"kind"`
			SessionId *string                    `json:"session_id,omitempty"`
		}{Kind: "session", SessionId: ptr(string(sess.ID))}
	})
	out, err := f.svc.CreateTask(t.Context(), in, "u1")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	got := f.get(t, event.ID(out.Id))
	if got.Target.Origin != sess.Origin {
		t.Fatalf("origin = %+v, want the session's %+v", got.Target.Origin, sess.Origin)
	}
	if got.Target.SessionID != sess.ID {
		t.Fatalf("session = %s, want %s", got.Target.SessionID, sess.ID)
	}
	if !f.events.has(event.TaskScheduled) {
		t.Fatal("no task.scheduled event")
	}
}

func TestCreateTaskRefusesADeliveryToATaskSession(t *testing.T) {
	f := newFixture(t)
	sess, _, err := f.store.EnsureSession(t.Context(), store.Session{
		Key: "main/task/x", Agent: "main", Kind: store.SessionTask, CreatedAt: baseTime,
	})
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	_, err = f.svc.CreateTask(t.Context(), taskCreate(func(c *gen.TaskCreate) {
		c.Delivery = &struct {
			Kind      gen.TaskCreateDeliveryKind `json:"kind"`
			SessionId *string                    `json:"session_id,omitempty"`
		}{Kind: "session", SessionId: ptr(string(sess.ID))}
	}), "u1")
	mustContain(t, err, "not a chat session")
}

func TestListGetDeleteTask(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	out, err := f.svc.CreateTask(ctx, taskCreate(func(c *gen.TaskCreate) {
		c.Grants = &[]string{"files.write", "memory.save"}
		c.Name = ptr("nightly  build")
	}), "u1")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	list, err := f.svc.ListTasks(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListTasks = %d tasks, %v", len(list), err)
	}
	if list[0].Grants == nil || len(*list[0].Grants) != 2 {
		t.Fatalf("grants are not reported: %+v", list[0].Grants)
	}
	if list[0].Name == nil || *list[0].Name != "nightly build" {
		t.Fatalf("name = %v, want the collapsed label", list[0].Name)
	}
	one, err := f.svc.GetTask(ctx, out.Id)
	if err != nil || one.Id != out.Id {
		t.Fatalf("GetTask = %+v, %v", one, err)
	}
	if _, err := f.svc.GetTask(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetTask(missing) = %v, want ErrNotFound", err)
	}

	// Delete cancels the row and revokes its grants; the row survives.
	if err := f.svc.DeleteTask(ctx, out.Id); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	got := f.get(t, event.ID(out.Id))
	if got.Status != StatusCancelled || got.NextRun != nil {
		t.Fatalf("after delete: status %s, next_run %v", got.Status, got.NextRun)
	}
	gs, err := f.grants.ListGrants(ctx, "main")
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	for _, g := range gs {
		if g.TaskID == got.ID {
			t.Fatalf("grant %s survived the delete", g.Tool)
		}
	}
	if err := f.svc.DeleteTask(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteTask(missing) = %v, want ErrNotFound", err)
	}
}

// --- the tool path ------------------------------------------------------------

func toolCall(f *fixture, sess store.Session) tool.Call {
	return tool.Call{
		Name: "task.create", TraceID: "trace-1", SessionID: sess.ID, Agent: "main",
		UserID: "u1", Role: "user", Origin: sess.Origin, RunKind: tool.RunInteractive,
	}
}

func TestCreateFromToolIsOriginDerived(t *testing.T) {
	f := newFixture(t)
	sess := f.chatSession(t, "u1")
	id, next, err := f.svc.CreateFromTool(t.Context(), toolCall(f, sess), "digest", "summarise the day", "0 9 * * *", time.Time{})
	if err != nil {
		t.Fatalf("CreateFromTool: %v", err)
	}
	got := f.get(t, event.ID(id))
	switch {
	case got.Agent != "main":
		t.Fatalf("agent = %q, want the calling agent", got.Agent)
	case got.Target.Kind != TargetSession || got.Target.SessionID != sess.ID:
		t.Fatalf("target = %+v, want the calling session", got.Target)
	case got.Target.Origin != sess.Origin:
		t.Fatalf("origin = %+v, want the session row's", got.Target.Origin)
	case len(got.Grants) != 0:
		t.Fatalf("a task created by the model carries grants: %v", got.Grants)
	case got.ParentTrace != "trace-1":
		t.Fatalf("parent trace = %q", got.ParentTrace)
	case got.CreatedBy != "u1":
		t.Fatalf("created by = %q", got.CreatedBy)
	case got.Missed != MissedSkip:
		t.Fatalf("missed = %q", got.Missed)
	}
	if want := baseTime.Add(time.Hour); !next.Equal(want) {
		t.Fatalf("next = %s, want %s", next, want)
	}
	// No grant row exists for it, so an `ask` inside its runs stays a deny.
	gs, err := f.grants.ListGrants(t.Context(), "main")
	if err != nil || len(gs) != 0 {
		t.Fatalf("grants = %v, %v", gs, err)
	}
}

func TestCreateFromToolRefusals(t *testing.T) {
	f := newFixture(t)
	sess := f.chatSession(t, "u1")
	cases := []struct {
		name string
		mut  func(*tool.Call)
		cron string
		at   time.Time
		want string
	}{
		{"guest", func(c *tool.Call) { c.Role = "guest" }, "", time.Time{}, "guests cannot"},
		{"a task cannot create a task", func(c *tool.Call) { c.RunKind = tool.RunTask }, "", time.Time{}, "cannot schedule another task"},
		{"no session", func(c *tool.Call) { c.SessionID = "" }, "", time.Time{}, "no session"},
		{"unknown session", func(c *tool.Call) { c.SessionID = "gone" }, "", time.Time{}, "unknown session"},
		{"both schedules", nil, "0 9 * * *", baseTime.Add(time.Hour), "not both"},
		{"every minute is too often", nil, "* * * * *", time.Time{}, "at most every 5 minutes"},
		{"every two minutes is too often", nil, "*/2 * * * *", time.Time{}, "at most every 5 minutes"},
		{"bad cron", nil, "nope", time.Time{}, "cron:"},
		{"at in the past", nil, "", baseTime.Add(-time.Minute), "must be in the future"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := toolCall(f, sess)
			if tc.mut != nil {
				tc.mut(&c)
			}
			_, _, err := f.svc.CreateFromTool(t.Context(), c, "n", "do it", tc.cron, tc.at)
			mustContain(t, err, tc.want)
		})
	}
	t.Run("empty prompt", func(t *testing.T) {
		_, _, err := f.svc.CreateFromTool(t.Context(), toolCall(f, sess), "n", " ", "", time.Time{})
		mustContain(t, err, "prompt is required")
	})
	t.Run("every five minutes is fine", func(t *testing.T) {
		if _, _, err := f.svc.CreateFromTool(t.Context(), toolCall(f, sess), "n", "do it", "*/5 * * * *", time.Time{}); err != nil {
			t.Fatalf("CreateFromTool: %v", err)
		}
	})
}

func TestCreateFromToolPerUserCap(t *testing.T) {
	f := newFixture(t)
	sess := f.chatSession(t, "u1")
	for i := 0; i < MaxPerUser; i++ {
		if _, _, err := f.svc.CreateFromTool(t.Context(), toolCall(f, sess), "n", "do it", "0 9 * * *", time.Time{}); err != nil {
			t.Fatalf("task %d: %v", i, err)
		}
	}
	_, _, err := f.svc.CreateFromTool(t.Context(), toolCall(f, sess), "n", "do it", "0 9 * * *", time.Time{})
	mustContain(t, err, "the cap is 20")

	// The cap counts active tasks of that user only: cancelling one frees a
	// slot, and another user is unaffected.
	list, err := f.svc.ListTasks(t.Context())
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if err := f.svc.DeleteTask(t.Context(), list[0].Id); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if _, _, err := f.svc.CreateFromTool(t.Context(), toolCall(f, sess), "n", "do it", "0 9 * * *", time.Time{}); err != nil {
		t.Fatalf("after freeing a slot: %v", err)
	}
	other := toolCall(f, sess)
	other.UserID = f.user(t, "u2")
	if _, _, err := f.svc.CreateFromTool(t.Context(), other, "n", "do it", "0 9 * * *", time.Time{}); err != nil {
		t.Fatalf("second user: %v", err)
	}
}

func TestCancelByUserStopsTheirTasksOnly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.user(t, "u1")
	f.user(t, "u2")
	mine, err := f.svc.CreateTask(ctx, taskCreate(nil), "u1")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := f.svc.CreateTask(ctx, taskCreate(nil), "u2")
	if err != nil {
		t.Fatal(err)
	}
	n, err := f.svc.CancelByUser(ctx, "u1")
	if err != nil || n != 1 {
		t.Fatalf("cancelled %d, err %v", n, err)
	}
	if got, _ := f.svc.GetTask(ctx, mine.Id); got.Status != gen.TaskStatusCancelled {
		t.Fatalf("u1 task status = %s", got.Status)
	}
	if got, _ := f.svc.GetTask(ctx, theirs.Id); got.Status != gen.TaskStatusActive {
		t.Fatalf("u2 task status = %s", got.Status)
	}
}
