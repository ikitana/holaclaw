package daemon

import (
	"context"
	"errors"
	"fmt"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/task"
)

// setupTasks builds the task service: scheduler, API backend and the target
// of the task.create tool.
func (d *Daemon) setupTasks(ctx context.Context) error {
	a := d.App
	svc, err := task.New(task.Deps{
		DB: a.Store.DB(), Store: a.Store, Submit: d.Sessions.Submit, Busy: d.Sessions.Busy,
		Grants: d.Approvals, Events: a.Events, Logger: d.log,
		Agents: func(name string) bool { _, ok := a.Config.Agents[name]; return ok },
		KnownTools: func(agent string) []string {
			exec, err := a.Executor(agent)
			if err != nil || exec == nil {
				return nil
			}
			var names []string
			for _, s := range exec.Specs() {
				names = append(names, s.Name)
			}
			return names
		},
	})
	if err != nil {
		return err
	}
	if err := svc.Recover(ctx); err != nil {
		return err
	}
	d.tasks = svc
	a.Tasks = svc
	d.Backend.Tasks = taskBackend{svc}
	return nil
}

// finishTask closes a task run and delivers its result to where the task was
// created. The target comes from the task row, never from the model.
func (d *Daemon) finishTask(ctx context.Context, sess store.Session, traceID event.ID, reply event.Message) error {
	dl, err := d.tasks.OnRunFinished(ctx, sess, traceID, reply, nil)
	if err != nil || dl == nil {
		return err
	}
	d.mu.Lock()
	fn := d.deliverers[dl.Session.Origin.Channel]
	d.mu.Unlock()
	if fn == nil {
		// API / CLI: make the delivered message visible on the event stream.
		ev := event.New(event.Reply, traceID, dl.Session.ID, "", map[string]any{"task": string(sess.ID)})
		ev.MessageID = dl.Message.ID
		d.App.Events.Emit(ctx, ev)
		return nil
	}
	return fn(ctx, dl.Session, traceID, dl.Message)
}

// taskBackend maps validation failures to the API's bad-input error.
type taskBackend struct{ s *task.Service }

func wrapTaskErr(err error) error {
	if err != nil && errors.Is(err, task.ErrInvalid) {
		return fmt.Errorf("%w: %s", api.ErrBadInput, err.Error())
	}
	return err
}

func (t taskBackend) ListTasks(ctx context.Context) ([]gen.Task, error) { return t.s.ListTasks(ctx) }

func (t taskBackend) GetTask(ctx context.Context, id string) (gen.Task, error) {
	return t.s.GetTask(ctx, id)
}

func (t taskBackend) CreateTask(ctx context.Context, in gen.TaskCreate, by event.ID) (gen.Task, error) {
	out, err := t.s.CreateTask(ctx, in, by)
	return out, wrapTaskErr(err)
}

func (t taskBackend) DeleteTask(ctx context.Context, id string) error { return t.s.DeleteTask(ctx, id) }
