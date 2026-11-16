package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"holaclaw/internal/tool"
)

// testExec builds the exec tool with the test process's PATH, so ordinary
// utilities resolve on any distribution (NixOS has no /usr/bin).
func testExec() *execTool { return newExec(os.Getenv("PATH")) }

func runExec(t *testing.T, tl *execTool, ws, args string) tool.Result {
	t.Helper()
	res, err := tl.Run(context.Background(), tool.Call{Name: "exec", Args: json.RawMessage(args), Workspace: ws})
	if err != nil {
		t.Fatalf("exec returned a Go error: %v", err)
	}
	return res
}

func TestExecOutputAndExitCode(t *testing.T) {
	ws := t.TempDir()
	res := runExec(t, testExec(), ws, `{"command":"echo out; echo err 1>&2; exit 3"}`)
	if res.IsError {
		t.Errorf("a non-zero exit must not be IsError: %q", res.Text)
	}
	if !strings.Contains(res.Text, "out") || !strings.Contains(res.Text, "err") {
		t.Errorf("stdout+stderr not captured: %q", res.Text)
	}
	if !strings.HasSuffix(res.Text, "exit code: 3") {
		t.Errorf("text = %q, want it to end with the exit code", res.Text)
	}
	if res.Tainted {
		t.Error("exec results are not tainted by themselves")
	}

	res = runExec(t, testExec(), ws, `{"command":"   "}`)
	if !res.IsError || !strings.Contains(res.Text, "command is required") {
		t.Errorf("empty command = %q (err=%v)", res.Text, res.IsError)
	}
	if res := runExec(t, testExec(), ws, `{{`); !res.IsError {
		t.Errorf("bad JSON arguments accepted: %q", res.Text)
	}
}

func TestExecRunsInWorkspace(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "agent") // created by the tool
	res := runExec(t, testExec(), ws, `{"command":"pwd; echo marker > made.txt"}`)
	want, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, want) {
		t.Errorf("cwd = %q, want %s", res.Text, want)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "made.txt")); err != nil || string(b) != "marker\n" {
		t.Errorf("file written by the command = %q, %v", b, err)
	}
}

func TestExecEnvIsScrubbed(t *testing.T) {
	t.Setenv("HOLACLAW_TEST_SECRET", "s3cr3t-token-value")
	ws := t.TempDir()
	res := runExec(t, testExec(), ws, `{"command":"env"}`)
	if strings.Contains(res.Text, "s3cr3t-token-value") || strings.Contains(res.Text, "HOLACLAW_TEST_SECRET") {
		t.Errorf("the daemon's environment leaked into the child: %q", res.Text)
	}
	for _, want := range []string{"HOME=" + ws, "LANG=C.UTF-8", "TERM=dumb", "PATH="} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("child env is missing %q: %q", want, res.Text)
		}
	}
	// The default PATH is fixed, not inherited.
	if got := newExec("").path; got != DefaultExecPath {
		t.Errorf("default PATH = %q", got)
	}
}

// TestExecTimeoutKillsProcessGroup starts a sleep in the background of the
// shell and checks that the timeout kills it too, not just the shell.
func TestExecTimeoutKillsProcessGroup(t *testing.T) {
	ws := t.TempDir()
	start := time.Now()
	res := runExec(t, testExec(), ws,
		`{"command":"sleep 60 & echo $! > child.pid; sleep 60","timeout_s":1}`)
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("timeout took %s", d)
	}
	if !strings.Contains(res.Text, "timed out after 1s") {
		t.Errorf("text = %q, want a timeout notice", res.Text)
	}
	if !strings.Contains(res.Text, "exit code: -1") {
		t.Errorf("text = %q, want a killed exit code", res.Text)
	}

	b, err := os.ReadFile(filepath.Join(ws, "child.pid"))
	if err != nil {
		t.Fatalf("the background child never recorded its pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		if err := syscall.Kill(pid, 0); err != nil { // ESRCH: gone
			break
		}
		if i > 40 {
			t.Fatalf("background child %d survived the timeout", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestExecContextCancel(t *testing.T) {
	ws := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, err := testExec().Run(ctx, tool.Call{
		Name: "exec", Args: json.RawMessage(`{"command":"sleep 60"}`), Workspace: ws,
	})
	if err != nil {
		t.Fatalf("exec returned a Go error: %v", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Fatalf("cancel took %s", d)
	}
	if !strings.Contains(res.Text, "cancelled") {
		t.Errorf("text = %q, want a cancellation notice", res.Text)
	}
}

func TestExecOutputCap(t *testing.T) {
	ws := t.TempDir()
	res := runExec(t, testExec(), ws, `{"command":"seq 1 40000"}`)
	if len(res.Text) > maxExecOutput+128 {
		t.Errorf("output is %d bytes, cap is %d", len(res.Text), maxExecOutput)
	}
	if !strings.Contains(res.Text, "bytes omitted") {
		t.Errorf("no omission marker: %q", res.Text[:80])
	}
	if !strings.HasPrefix(res.Text, "1\n2\n3\n") {
		t.Errorf("head was not kept: %q", res.Text[:20])
	}
	if !strings.Contains(res.Text, "40000\n") {
		t.Error("tail was not kept")
	}
}

func TestCapWriter(t *testing.T) {
	w := &capWriter{max: 20}
	_, _ = fmt.Fprint(w, "0123456789")
	if got := w.String(); got != "0123456789" {
		t.Errorf("under the cap = %q", got)
	}
	_, _ = fmt.Fprint(w, "abcdefghij")
	if got := w.String(); got != "0123456789abcdefghij" {
		t.Errorf("exactly at the cap = %q", got)
	}
	_, _ = fmt.Fprint(w, "XYZ")
	got := w.String()
	if !strings.HasPrefix(got, "0123456789") || !strings.HasSuffix(got, "XYZ") {
		t.Errorf("head and tail not kept: %q", got)
	}
	if !strings.Contains(got, "[... 3 bytes omitted ...]") {
		t.Errorf("marker = %q", got)
	}
}

func TestExecCommand(t *testing.T) {
	got, err := ExecCommand(json.RawMessage(`{"command":" ls -la ","timeout_s":5}`))
	if err != nil || got != "ls -la" {
		t.Errorf("ExecCommand = %q, %v", got, err)
	}
	for _, args := range []string{`{}`, `{"command":""}`, `{"command":"   "}`, `not json`, ``} {
		if got, err := ExecCommand(json.RawMessage(args)); err == nil {
			t.Errorf("ExecCommand(%q) = %q, want an error", args, got)
		}
	}
}

func TestExecTimeoutClamp(t *testing.T) {
	// A timeout over the maximum is clamped, not rejected.
	ws := t.TempDir()
	res := runExec(t, testExec(), ws, `{"command":"true","timeout_s":99999}`)
	if res.IsError || !strings.HasSuffix(res.Text, "exit code: 0") {
		t.Errorf("clamped timeout = %q (err=%v)", res.Text, res.IsError)
	}
}
