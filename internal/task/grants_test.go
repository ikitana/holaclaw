package task

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

// writeTool stands in for files.write: risk write, so a task policy of `ask`
// reaches the grant lookup.
type writeTool struct{ ran bool }

func (*writeTool) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name: "files.write", Description: "Write a file.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	}
}
func (*writeTool) Risk() tool.Risk { return tool.RiskWrite }
func (w *writeTool) Run(context.Context, tool.Call) (tool.Result, error) {
	w.ran = true
	return tool.Result{Text: "written"}, nil
}

// A task grant turns the gate's `ask` into `allow` for that task id and no
// other (docs/security.md: detached runs are ask = deny unless granted).
func TestTaskGrantsUnlockOnlyThatTask(t *testing.T) {
	f := newFixture(t)
	f.user(t, "u1")
	granted := f.newTask(t, func(c *gen.TaskCreate) { c.Grants = &[]string{"files.write"} })
	plain := f.newTask(t, nil)

	w := &writeTool{}
	reg := tool.NewRegistry()
	if err := reg.Register(w); err != nil {
		t.Fatalf("Register: %v", err)
	}
	inner, err := reg.Select([]string{"files.write"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	gate := tool.NewGate(inner, tool.GateConfig{
		Policy: tool.Policy{Task: tool.Classes{Write: tool.Ask, Read: tool.Allow}},
		Grants: f.grants,
	})
	call := func(taskID string) tool.Result {
		w.ran = false
		res, err := gate.Execute(t.Context(), tool.Call{
			Name: "files.write", Agent: "main", RunKind: tool.RunTask,
			TaskID: event.ID(taskID), SessionID: event.ID(taskID),
			Args: json.RawMessage(`{"path":"notes.md"}`),
		})
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return res
	}

	if res := call(string(granted.ID)); res.IsError || !w.ran {
		t.Fatalf("granted task was refused: %+v", res)
	}
	res := call(string(plain.ID))
	if !res.IsError || w.ran {
		t.Fatalf("a task without grants ran the tool: %+v", res)
	}
	if !strings.Contains(res.Text, "detached run without grant") {
		t.Fatalf("reason = %q", res.Text)
	}
	// Cancelling the task revokes the grant, so its next call is a deny too.
	if err := f.svc.DeleteTask(t.Context(), string(granted.ID)); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if res := call(string(granted.ID)); !res.IsError || w.ran {
		t.Fatalf("a cancelled task kept its grant: %+v", res)
	}
}
