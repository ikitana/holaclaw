package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

const (
	// DefaultExecPath is the PATH every exec child gets; nothing is inherited
	// from the daemon (docs/security.md, process layer).
	DefaultExecPath = "/usr/local/bin:/usr/bin:/bin"

	execDefaultTimeout = 30
	execMaxTimeout     = 300
	// maxExecOutput is the cap on combined stdout+stderr in the result.
	maxExecOutput = 16 << 10
	// execWaitDelay bounds how long Wait blocks on a pipe still held open by
	// a process that survived the kill.
	execWaitDelay = 3 * time.Second
)

// ExecCommand returns the shell command of an exec call. The policy gate uses
// it to match a call against command allow-lists without re-parsing schemas.
func ExecCommand(args json.RawMessage) (string, error) {
	var a struct {
		Command string `json:"command"`
	}
	if err := decodeArgs(args, &a); err != nil {
		return "", fmt.Errorf("exec: bad arguments: %w", err)
	}
	cmd := strings.TrimSpace(a.Command)
	if cmd == "" {
		return "", errors.New("exec: command is required")
	}
	return cmd, nil
}

// execTool runs a shell command in the workspace with a scrubbed environment.
type execTool struct {
	path string
}

func newExec(path string) *execTool {
	if path == "" {
		path = DefaultExecPath
	}
	return &execTool{path: path}
}

func (*execTool) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "exec",
		Description: "Run a shell command in the workspace directory. timeout_s defaults to 30, max 300. Output is capped.",
		Parameters: object{
			Properties: map[string]prop{"command": {Type: tString}, "timeout_s": {Type: tInteger}},
			Required:   []string{"command"},
		}.raw(),
	}
}

func (*execTool) Risk() tool.Risk { return tool.RiskExec }

func (t *execTool) Run(ctx context.Context, c tool.Call) (tool.Result, error) {
	command, err := ExecCommand(c.Args)
	if err != nil {
		return errf("%v", err), nil
	}
	var a struct {
		TimeoutS int `json:"timeout_s"`
	}
	if err := decodeArgs(c.Args, &a); err != nil {
		return errf("exec: bad arguments: %v", err), nil
	}
	secs := a.TimeoutS
	if secs <= 0 {
		secs = execDefaultTimeout
	}
	if secs > execMaxTimeout {
		secs = execMaxTimeout
	}
	dir, err := ensureWorkspace(c)
	if err != nil {
		return errf("%v", err), nil
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return errf("exec: cannot open %s: %v", os.DevNull, err), nil
	}
	defer devnull.Close() //nolint:errcheck // read-only

	ctx, cancel := context.WithTimeout(ctx, time.Duration(secs)*time.Second)
	defer cancel()

	out := &capWriter{max: maxExecOutput}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	// Scrubbed environment: nothing from the daemon's process reaches here.
	cmd.Env = []string{
		"PATH=" + t.path,
		"HOME=" + dir,
		"LANG=C.UTF-8",
		"TERM=dumb",
	}
	cmd.Stdin = devnull
	cmd.Stdout = out
	cmd.Stderr = out
	// Own process group, so a timeout kills the shell and everything it
	// started, not just the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd.Process.Pid) }
	cmd.WaitDelay = execWaitDelay

	if err := cmd.Start(); err != nil {
		return errf("exec: cannot start: %v", err), nil
	}
	pid := cmd.Process.Pid
	waitErr := cmd.Wait()
	_ = killGroup(pid) // reap anything that outlived the shell

	code := cmd.ProcessState.ExitCode()
	var b strings.Builder
	if s := out.String(); s != "" {
		b.WriteString(s)
		if !strings.HasSuffix(s, "\n") {
			b.WriteByte('\n')
		}
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		fmt.Fprintf(&b, "[timed out after %ds; process group killed]\n", secs)
	case ctx.Err() != nil:
		b.WriteString("[cancelled; process group killed]\n")
	case waitErr != nil && code < 0:
		fmt.Fprintf(&b, "[killed: %v]\n", waitErr)
	}
	fmt.Fprintf(&b, "exit code: %d", code)
	return tool.Result{Text: b.String()}, nil
}

// killGroup SIGKILLs a whole process group.
func killGroup(pid int) error {
	if pid <= 0 {
		return nil
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// capWriter keeps the head and the tail of a stream, bounded by max, so a
// chatty command cannot exhaust memory. It is written from the copy
// goroutines of os/exec, hence the mutex.
type capWriter struct {
	mu    sync.Mutex
	max   int
	head  []byte
	tail  []byte
	total int
}

func (w *capWriter) headMax() int { return w.max / 2 }
func (w *capWriter) tailMax() int { return w.max - w.max/2 }

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := len(p)
	w.total += written
	if n := w.headMax() - len(w.head); n > 0 {
		if n > len(p) {
			n = len(p)
		}
		w.head = append(w.head, p[:n]...)
		p = p[n:]
	}
	if len(p) > 0 {
		w.tail = append(w.tail, p...)
		if len(w.tail) > 2*w.tailMax() {
			w.tail = append([]byte(nil), w.tail[len(w.tail)-w.tailMax():]...)
		}
	}
	return written, nil
}

// String returns head + tail with a marker for what was dropped in between.
func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	tail := w.tail
	if len(tail) > w.tailMax() {
		tail = tail[len(tail)-w.tailMax():]
	}
	omitted := w.total - len(w.head) - len(tail)
	if omitted <= 0 {
		return string(w.head) + string(tail)
	}
	return fmt.Sprintf("%s\n[... %d bytes omitted ...]\n%s", w.head, omitted, tail)
}
