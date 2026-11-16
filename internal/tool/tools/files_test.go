package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holaclaw/internal/tool"
)

// run executes one registered tool with the given workspace and arguments.
func run(t *testing.T, r *tool.Registry, name, ws string, args string) tool.Result {
	t.Helper()
	tl, ok := r.Get(name)
	if !ok {
		t.Fatalf("tool %s is not registered", name)
	}
	res, err := tl.Run(context.Background(), tool.Call{
		CallID: "c1", Name: name, Args: json.RawMessage(args), Workspace: ws,
	})
	if err != nil {
		t.Fatalf("%s returned a Go error: %v", name, err)
	}
	return res
}

func TestFilesWriteReadList(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "agent") // missing: the tools create it
	r := registry(t, Deps{Blobs: &fakeBlobs{}})

	res := run(t, r, "files.write", ws, `{"path":"notes/day.md","content":"hello\n"}`)
	if res.IsError {
		t.Fatalf("write: %s", res.Text)
	}
	if got, err := os.ReadFile(filepath.Join(ws, "notes/day.md")); err != nil || string(got) != "hello\n" {
		t.Fatalf("file on disk = %q, %v", got, err)
	}

	res = run(t, r, "files.write", ws, `{"path":"notes/day.md","content":"world\n","append":true}`)
	if res.IsError || !strings.Contains(res.Text, "appended") {
		t.Fatalf("append: %s", res.Text)
	}
	res = run(t, r, "files.read", ws, `{"path":"notes/day.md"}`)
	if res.IsError || res.Text != "hello\nworld\n" {
		t.Fatalf("read after append = %q (err=%v)", res.Text, res.IsError)
	}

	res = run(t, r, "files.list", ws, `{}`)
	if res.IsError || !strings.Contains(res.Text, "notes/") {
		t.Fatalf("list root = %q", res.Text)
	}
	res = run(t, r, "files.list", ws, `{"path":"notes"}`)
	if res.IsError || !strings.Contains(res.Text, "day.md\t12") {
		t.Fatalf("list notes = %q", res.Text)
	}
	res = run(t, r, "files.list", ws, `{"path":"nope"}`)
	if !res.IsError {
		t.Fatalf("list of a missing dir succeeded: %q", res.Text)
	}
}

func TestFilesReadOffsetLimitCapBinary(t *testing.T) {
	ws := t.TempDir()
	r := registry(t, Deps{})
	var lines strings.Builder
	for i := 1; i <= 10; i++ {
		fmt.Fprintf(&lines, "line%d\n", i)
	}
	if err := os.WriteFile(filepath.Join(ws, "f.txt"), []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	res := run(t, r, "files.read", ws, `{"path":"f.txt","offset":3,"limit":2}`)
	if res.Text != "line3\nline4\n" {
		t.Errorf("offset/limit = %q", res.Text)
	}
	res = run(t, r, "files.read", ws, `{"path":"f.txt","offset":99}`)
	if res.Text != "" {
		t.Errorf("offset past the end = %q", res.Text)
	}

	big := strings.Repeat("x", maxReadBytes+1000)
	if err := os.WriteFile(filepath.Join(ws, "big.txt"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	res = run(t, r, "files.read", ws, `{"path":"big.txt"}`)
	if !strings.Contains(res.Text, "[truncated 1000 bytes]") || len(res.Text) > maxReadBytes+64 {
		t.Errorf("cap: len=%d tail=%q", len(res.Text), res.Text[len(res.Text)-30:])
	}

	if err := os.WriteFile(filepath.Join(ws, "b.bin"), []byte{'a', 0, 'b'}, 0o600); err != nil {
		t.Fatal(err)
	}
	res = run(t, r, "files.read", ws, `{"path":"b.bin"}`)
	if !res.IsError || !strings.Contains(res.Text, "attach") {
		t.Errorf("binary read = %q (err=%v), want a refusal pointing at attach", res.Text, res.IsError)
	}

	res = run(t, r, "files.read", ws, `{"path":"."}`)
	if !res.IsError || !strings.Contains(res.Text, "files.list") {
		t.Errorf("read of a directory = %q", res.Text)
	}
	res = run(t, r, "files.read", ws, `{"path":"missing.txt"}`)
	if !res.IsError {
		t.Errorf("read of a missing file succeeded")
	}
	res = run(t, r, "files.read", ws, `not json`)
	if !res.IsError {
		t.Errorf("bad JSON arguments accepted")
	}
}

func TestFilesWriteLimits(t *testing.T) {
	ws := t.TempDir()
	r := registry(t, Deps{})
	big := strings.Repeat("x", maxWriteBytes+1)
	args, err := json.Marshal(map[string]any{"path": "big.txt", "content": big})
	if err != nil {
		t.Fatal(err)
	}
	res := run(t, r, "files.write", ws, string(args))
	if !res.IsError || !strings.Contains(res.Text, "cap is") {
		t.Errorf("oversize write = %q (err=%v)", res.Text, res.IsError)
	}
	if _, err := os.Stat(filepath.Join(ws, "big.txt")); !os.IsNotExist(err) {
		t.Errorf("oversize write created the file anyway")
	}
	if res := run(t, r, "files.write", ws, `{"path":"a.txt"}`); res.IsError {
		t.Errorf("empty content should write an empty file: %s", res.Text)
	}
}

func TestFilesListCap(t *testing.T) {
	ws := t.TempDir()
	r := registry(t, Deps{})
	for i := range maxListEntries + 25 {
		if err := os.WriteFile(filepath.Join(ws, fmt.Sprintf("f%03d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	res := run(t, r, "files.list", ws, `{}`)
	if n := strings.Count(res.Text, "\n"); n != maxListEntries+1 {
		t.Errorf("listed %d lines, want %d entries plus the notice", n, maxListEntries)
	}
	if !strings.Contains(res.Text, "[25 more entries not listed]") {
		t.Errorf("missing overflow notice: %q", res.Text[len(res.Text)-60:])
	}
}

// TestJailEscapes is the containment test: every tool that touches the
// filesystem must refuse to leave the workspace, whatever shape the escape
// takes, and must not touch the file outside.
func TestJailEscapes(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	outside := filepath.Join(base, "secret.txt")
	if err := os.MkdirAll(ws, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("classified\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink to a file outside, and a symlinked parent directory.
	if err := os.Symlink(outside, filepath.Join(ws, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(base, filepath.Join(ws, "up")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(ws, "etc")); err != nil {
		t.Fatal(err)
	}

	escapes := []string{
		"../secret.txt",
		"../../etc/passwd",
		"/etc/passwd",
		"/tmp/x",
		"link.txt",
		"up/secret.txt",
		"etc/passwd",
		"./up/../../secret.txt",
	}
	r := registry(t, Deps{Blobs: &fakeBlobs{}})
	for _, p := range escapes {
		args, err := json.Marshal(map[string]any{"path": p, "content": "pwned"})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"files.read", "files.write", "files.list", "attach"} {
			res := run(t, r, name, ws, string(args))
			if !res.IsError {
				t.Errorf("%s(%q) succeeded: %q", name, p, res.Text)
			}
		}
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "classified\n" {
		t.Fatalf("the file outside the workspace was modified: %q, %v", got, err)
	}
}

func TestNoWorkspace(t *testing.T) {
	r := registry(t, Deps{Blobs: &fakeBlobs{}})
	for _, name := range []string{"files.read", "files.write", "files.list", "attach", "exec"} {
		res := run(t, r, name, "relative/path", `{"path":"a.txt","content":"x","command":"true"}`)
		if !res.IsError || !strings.Contains(res.Text, "workspace") {
			t.Errorf("%s with a relative workspace = %q (err=%v)", name, res.Text, res.IsError)
		}
	}
}
