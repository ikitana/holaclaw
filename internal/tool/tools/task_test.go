package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/tool"
)

// fakeCreator records what reached the service and answers with a fixed id.
type fakeCreator struct {
	mu     sync.Mutex
	call   tool.Call
	name   string
	prompt string
	cron   string
	at     time.Time
	calls  int
	err    error
}

func (f *fakeCreator) CreateFromTool(_ context.Context, c tool.Call, name, prompt, cron string, at time.Time) (string, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.call, f.name, f.prompt, f.cron, f.at = c, name, prompt, cron, at
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	return "01TASK", time.Date(2026, 5, 4, 9, 0, 0, 0, time.UTC), nil
}

func registerTask(t *testing.T) (*tool.Registry, *fakeCreator) {
	t.Helper()
	r, c := tool.NewRegistry(), &fakeCreator{}
	if err := RegisterTask(r, c); err != nil {
		t.Fatalf("RegisterTask: %v", err)
	}
	return r, c
}

func runTask(t *testing.T, r *tool.Registry, c tool.Call, args string) tool.Result {
	t.Helper()
	tl, ok := r.Get("task.create")
	if !ok {
		t.Fatal("task.create is not registered")
	}
	c.Name, c.Args = "task.create", json.RawMessage(args)
	res, err := tl.Run(t.Context(), c)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// The schema budget of docs/context.md: every tool spec must fit MaxSchemaTokens. The
// number is logged so a change to the description is visible in the diff.
func TestTaskCreateSchemaFitsTheBudget(t *testing.T) {
	r, _ := registerTask(t)
	tl, _ := r.Get("task.create")
	n := tool.SchemaTokens(tl.Spec())
	t.Logf("task.create schema: %d tokens (cap %d)", n, tool.MaxSchemaTokens)
	if n > tool.MaxSchemaTokens {
		t.Fatalf("schema is %d tokens, cap is %d", n, tool.MaxSchemaTokens)
	}
	if tl.Risk() != tool.RiskWrite {
		t.Fatalf("risk %q, want write", tl.Risk())
	}
}

func TestRegisterTaskNeedsACreator(t *testing.T) {
	if err := RegisterTask(tool.NewRegistry(), nil); err == nil {
		t.Fatal("RegisterTask(nil) succeeded")
	}
}

func TestTaskCreateSchedules(t *testing.T) {
	r, c := registerTask(t)
	res := runTask(t, r, tool.Call{Agent: "main", SessionID: "s1", UserID: "u1"},
		`{"name":"digest","prompt":"summarise","cron":"0 9 * * *"}`)
	if res.IsError {
		t.Fatalf("result = %+v", res)
	}
	if want := "scheduled task 01TASK: next run 2026-05-04T09:00:00Z"; res.Text != want {
		t.Fatalf("text = %q, want %q", res.Text, want)
	}
	if c.cron != "0 9 * * *" || c.name != "digest" || c.prompt != "summarise" || !c.at.IsZero() {
		t.Fatalf("service saw name %q prompt %q cron %q at %s", c.name, c.prompt, c.cron, c.at)
	}
}

// The model can name neither the agent, the delivery session nor a grant: the
// tool has no such parameters, and unknown fields in the arguments are
// ignored, so the trusted Call is what reaches the service (docs/security.md).
func TestTaskCreateIsOriginDerived(t *testing.T) {
	r, c := registerTask(t)
	call := tool.Call{Agent: "main", SessionID: "s1", UserID: "u1", Role: "user", TraceID: "t1"}
	res := runTask(t, r, call,
		`{"prompt":"do it","agent":"admin-bot","session_id":"s-victim","grants":["exec"],"user_id":"owner"}`)
	if res.IsError {
		t.Fatalf("result = %+v", res)
	}
	got := c.call
	if got.Agent != "main" || got.SessionID != "s1" || got.UserID != "u1" || got.TraceID != "t1" {
		t.Fatalf("the call was not the trusted one: %+v", got)
	}
}

func TestTaskCreateArguments(t *testing.T) {
	cases := []struct {
		name, args, want string
	}{
		{"not an object", `"hello"`, "must be a JSON object"},
		{"cron and at", `{"prompt":"p","cron":"0 9 * * *","at":"2026-05-04T09:00:00Z"}`, "at most one"},
		{"at and in_minutes", `{"prompt":"p","at":"2026-05-04T09:00:00Z","in_minutes":5}`, "at most one"},
		{"bad at", `{"prompt":"p","at":"tomorrow"}`, "RFC3339"},
		{"in_minutes too small", `{"prompt":"p","in_minutes":-3}`, "in_minutes must be"},
		{"in_minutes too large", `{"prompt":"p","in_minutes":600000}`, "in_minutes must be"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, c := registerTask(t)
			res := runTask(t, r, tool.Call{Agent: "main", SessionID: "s1"}, tc.args)
			if !res.IsError || !strings.Contains(res.Text, tc.want) {
				t.Fatalf("result = %+v, want an error containing %q", res, tc.want)
			}
			if c.calls != 0 {
				t.Fatal("a rejected call still reached the service")
			}
		})
	}
}

func TestTaskCreateInMinutesAndNow(t *testing.T) {
	r, c := registerTask(t)
	before := time.Now().UTC()
	if res := runTask(t, r, tool.Call{Agent: "main", SessionID: "s1"},
		`{"prompt":"p","in_minutes":30}`); res.IsError {
		t.Fatalf("result = %+v", res)
	}
	if c.cron != "" {
		t.Fatalf("cron = %q, want none", c.cron)
	}
	if d := c.at.Sub(before); d < 29*time.Minute || d > 31*time.Minute {
		t.Fatalf("at is %s away, want about 30m", d)
	}

	// No schedule at all means "now": no cron, no time.
	if res := runTask(t, r, tool.Call{Agent: "main", SessionID: "s1"}, `{"prompt":"p"}`); res.IsError {
		t.Fatalf("result = %+v", res)
	}
	if c.cron != "" || !c.at.IsZero() {
		t.Fatalf("cron %q at %s, want an immediate run", c.cron, c.at)
	}
}

// A refusal from the service is a recoverable tool error, never a Go error.
func TestTaskCreateReportsServiceRefusals(t *testing.T) {
	r, c := registerTask(t)
	c.err = errors.New("task: invalid: guests cannot schedule tasks")
	res := runTask(t, r, tool.Call{Agent: "main", SessionID: "s1", Role: "guest"}, `{"prompt":"p"}`)
	if !res.IsError || !strings.Contains(res.Text, "guests cannot schedule tasks") {
		t.Fatalf("result = %+v", res)
	}
}
