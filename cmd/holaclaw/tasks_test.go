package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
)

func TestTasksList(t *testing.T) {
	e := startDaemon(t, "", "")
	ctx := context.Background()

	code, out, errOut := runCLI(t, "", "tasks", "list", "--config", e.dir)
	if code != 0 || out != "no tasks\n" {
		t.Fatalf("tasks list with no tasks: exit = %d\n%s%s", code, out, errOut)
	}

	owner, err := e.daemon.Auth.Owner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cron := gen.TaskCreate{Name: ptrOf("notifications"), Prompt: "Summarise my\nunread notifications."}
	cron.Schedule.Kind = "cron"
	cron.Schedule.Cron = ptrOf("0 8 * * MON-FRI")
	recurring, err := e.daemon.Backend.Tasks.CreateTask(ctx, cron, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	at := gen.TaskCreate{Prompt: "Send the invoice."}
	at.Schedule.Kind = "at"
	at.Schedule.At = ptrOf(time.Now().Add(48 * time.Hour))
	once, err := e.daemon.Backend.Tasks.CreateTask(ctx, at, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.daemon.Backend.Tasks.DeleteTask(ctx, once.Id); err != nil {
		t.Fatal(err)
	}

	code, out, errOut = runCLI(t, "", "tasks", "list", "--config", e.dir)
	if code != 0 {
		t.Fatalf("tasks list: exit = %d\n%s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ID ") {
		t.Fatalf("tasks list = %q", out)
	}
	// Newest first; a prompt is folded onto its row, and a task that never
	// ran has no last run.
	stampRE := `\d{4}-\d\d-\d\d \d\d:\d\d:\d\dZ`
	wantOnce := regexp.MustCompile(`^` + once.Id + `\s+main\s+-\s+at ` + stampRE + `\s+cancelled\s+.*Send the invoice\.$`)
	if !wantOnce.MatchString(lines[1]) {
		t.Errorf("one-shot row = %q", lines[1])
	}
	wantRecurring := regexp.MustCompile(`^` + recurring.Id + `\s+main\s+notifications\s+0 8 \* \* MON-FRI\s+active\s+` +
		stampRE + `\s+-\s+Summarise my unread notifications\.$`)
	if !wantRecurring.MatchString(lines[2]) {
		t.Errorf("recurring row = %q", lines[2])
	}
}

func TestTasksListUsage(t *testing.T) {
	for _, args := range [][]string{
		{"tasks", "list", "extra"},
		{"tasks", "show"},
	} {
		if code, _, errOut := runCLI(t, "", args...); code != 2 || !strings.Contains(errOut, "usage: holaclaw tasks") {
			t.Errorf("%v: exit = %d, stderr = %q", args, code, errOut)
		}
	}
}

// A refusal from the daemon is reported, with nothing on stdout.
func TestTasksListRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/status" {
			_ = json.NewEncoder(w).Encode(gen.Status{})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(gen.Error{Code: "forbidden", Message: "the admin scope is required"})
	}))
	t.Cleanup(srv.Close)
	code, out, errOut := runCLI(t, "", "tasks", "list", "--addr", srv.URL)
	if code != 1 || out != "" || !strings.Contains(errOut, "the admin scope is required") {
		t.Fatalf("exit = %d\nstdout %q\nstderr %q", code, out, errOut)
	}
}
