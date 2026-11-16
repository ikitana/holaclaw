// Package tools holds the built-in tools: files.read / files.write /
// files.list, fetch, attach and exec. Every tool here assumes the policy gate
// already authorised the call (docs/security.md); what these tools enforce is
// containment — the workspace jail (os.Root), SSRF-safe dialling, a scrubbed
// exec environment — and the small-model rules of docs/context.md: flat parameters, short
// descriptions, capped results, bad input reported as a Result, never as a
// Go error.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"holaclaw/internal/tool"
)

// BlobPutter stores an attachment body and returns its content address. The
// blob store is adapted to this interface by the daemon.
type BlobPutter interface {
	PutBlob(ctx context.Context, r io.Reader, mime string) (id string, size int64, err error)
}

// Deps is the wiring the built-in tools need.
type Deps struct {
	// Blobs is required by `attach`.
	Blobs BlobPutter
	// FetchAllowPrivate disables fetch's private-address guard.
	//
	// TESTS ONLY. It exists so unit tests can reach an httptest server on
	// loopback. Setting it in production re-opens the whole SSRF surface:
	// metadata services, loopback admin ports, the LAN.
	FetchAllowPrivate bool
	// ExecPath overrides the fixed PATH handed to exec children. Empty means
	// DefaultExecPath.
	ExecPath string
}

// Register adds every built-in tool to r.
func Register(r *tool.Registry, d Deps) error {
	all := []tool.Tool{
		&filesRead{},
		&filesWrite{},
		&filesList{},
		newFetch(d.FetchAllowPrivate),
		&attachTool{blobs: d.Blobs},
		newExec(d.ExecPath),
	}
	for _, t := range all {
		if err := r.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// capText truncates s to at most maxBytes on a rune boundary and appends a
// marker saying how much was dropped. Every tool result goes through it.
func capText(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[truncated %d bytes]", len(s)-cut)
}

// errf returns a one-line, actionable failure the model can recover from.
func errf(format string, a ...any) tool.Result {
	return tool.Result{Text: fmt.Sprintf(format, a...), IsError: true}
}

// decodeArgs unmarshals a call's arguments into dst. An empty body is an
// empty object.
func decodeArgs(raw json.RawMessage, dst any) error {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

// openWorkspace opens the agent workspace as an os.Root, creating it if
// missing. Every filesystem operation of every tool goes through the
// returned Root, so "..", absolute paths and symlinks pointing outside are
// refused by the kernel-checked walk, not by string matching.
func openWorkspace(c tool.Call) (*os.Root, error) {
	dir, err := ensureWorkspace(c)
	if err != nil {
		return nil, err
	}
	return os.OpenRoot(dir)
}

// ensureWorkspace validates the workspace path and creates the directory.
func ensureWorkspace(c tool.Call) (string, error) {
	if !filepath.IsAbs(c.Workspace) {
		return "", fmt.Errorf("no workspace configured for this agent")
	}
	if err := os.MkdirAll(c.Workspace, 0o750); err != nil {
		return "", fmt.Errorf("workspace is unusable: %w", err)
	}
	return c.Workspace, nil
}

// relPath validates a model-supplied path before it reaches the Root: it must
// be relative and free of "..". The Root enforces the same thing; this only
// turns the common mistakes into a readable message.
func relPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be relative to the workspace, not %q", p)
	}
	c := filepath.Clean(p)
	if c == ".." || strings.HasPrefix(c, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path must stay inside the workspace, not %q", p)
	}
	return c, nil
}

// jailErr renders a Root failure without leaking absolute host paths.
func jailErr(op, p string, err error) tool.Result {
	if os.IsNotExist(err) {
		return errf("%s: %s: no such file in the workspace", op, p)
	}
	if os.IsPermission(err) {
		return errf("%s: %s: permission denied", op, p)
	}
	return errf("%s: %s: refused (outside the workspace or unreadable)", op, p)
}
