package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

// TaskCreator schedules a detached run. internal/task implements it; the
// narrow shape keeps this package free of the task service.
//
// Everything that decides who the task acts for — the agent, the delivery
// target, the user — is read from the Call, not from these arguments, and a
// task created this way carries no grants: inside its runs every `ask`
// decision is a deny (docs/security.md). The model supplies a label, a prompt
// and a time; an empty cron and a zero time mean "run it now".
type TaskCreator interface {
	CreateFromTool(ctx context.Context, c tool.Call, name, prompt, cron string, at time.Time) (id string, next time.Time, err error)
}

// maxInMinutes bounds the convenience form at a year, the same as the service.
const maxInMinutes = 365 * 24 * 60

// RegisterTask adds task.create.
func RegisterTask(r *tool.Registry, c TaskCreator) error {
	if c == nil {
		return errors.New("tools: RegisterTask needs a task creator")
	}
	return r.Register(&taskCreate{tasks: c})
}

type taskCreate struct{ tasks TaskCreator }

func (*taskCreate) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name: "task.create",
		Description: "Run a prompt later and report back here. " +
			"At most one of cron, at, in_minutes; none means now.",
		Parameters: object{
			Properties: map[string]prop{
				"prompt":     {Type: tString},
				"name":       {Type: tString},
				"cron":       {Type: tString},
				"at":         {Type: tString},
				"in_minutes": {Type: tInteger},
			},
			Required: []string{"prompt"},
		}.raw(),
	}
}

func (*taskCreate) Risk() tool.Risk { return tool.RiskWrite }

type taskArgs struct {
	Name      string `json:"name"`
	Prompt    string `json:"prompt"`
	Cron      string `json:"cron"`
	At        string `json:"at"`
	InMinutes int    `json:"in_minutes"`
}

func (t *taskCreate) Run(ctx context.Context, c tool.Call) (tool.Result, error) {
	var a taskArgs
	if err := decodeArgs(c.Args, &a); err != nil {
		return errf("task.create: arguments must be a JSON object: %v", err), nil
	}
	a.Cron = strings.TrimSpace(a.Cron)
	at, res, ok := scheduleTime(a)
	if !ok {
		return res, nil
	}
	id, next, err := t.tasks.CreateFromTool(ctx, c, a.Name, a.Prompt, a.Cron, at)
	if err != nil {
		return errf("task.create: %v", err), nil
	}
	return tool.Result{Text: fmt.Sprintf("scheduled task %s: next run %s",
		id, next.UTC().Format(time.RFC3339))}, nil
}

// scheduleTime turns the two one-shot spellings into one instant and enforces
// that exactly one schedule form was given.
func scheduleTime(a taskArgs) (time.Time, tool.Result, bool) {
	given := 0
	for _, on := range []bool{a.Cron != "", strings.TrimSpace(a.At) != "", a.InMinutes != 0} {
		if on {
			given++
		}
	}
	if given > 1 {
		return time.Time{}, errf("task.create: give at most one of cron, at, in_minutes"), false
	}
	switch {
	case strings.TrimSpace(a.At) != "":
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(a.At))
		if err != nil {
			return time.Time{}, errf("task.create: at must be an RFC3339 time like 2026-01-31T09:00:00Z"), false
		}
		return at.UTC(), tool.Result{}, true
	case a.InMinutes != 0:
		if a.InMinutes < 1 || a.InMinutes > maxInMinutes {
			return time.Time{}, errf("task.create: in_minutes must be between 1 and %d", maxInMinutes), false
		}
		return time.Now().UTC().Add(time.Duration(a.InMinutes) * time.Minute), tool.Result{}, true
	}
	return time.Time{}, tool.Result{}, true
}
