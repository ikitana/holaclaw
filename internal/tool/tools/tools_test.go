package tools

import (
	"context"
	"io"
	"strings"
	"testing"

	"holaclaw/internal/tool"
)

// fakeBlobs is a BlobPutter that keeps bodies in memory.
type fakeBlobs struct {
	mime string
	body []byte
	err  error
}

func (f *fakeBlobs) PutBlob(_ context.Context, r io.Reader, mime string) (string, int64, error) {
	if f.err != nil {
		return "", 0, f.err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", 0, err
	}
	f.body, f.mime = b, mime
	return strings.Repeat("ab", 32), int64(len(b)), nil
}

func registry(t *testing.T, d Deps) *tool.Registry {
	t.Helper()
	r := tool.NewRegistry()
	if err := Register(r, d); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return r
}

// TestSchemaSizes registers every built-in tool in a real registry (which
// rejects anything over tool.MaxSchemaTokens) and logs the budget.
func TestSchemaSizes(t *testing.T) {
	r := registry(t, Deps{Blobs: &fakeBlobs{}})
	want := []string{"attach", "exec", "fetch", "files.list", "files.read", "files.write"}
	got := r.Names()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("registered %v, want %v", got, want)
	}
	total := 0
	for _, n := range got {
		tl, _ := r.Get(n)
		size := tool.SchemaTokens(tl.Spec())
		total += size
		t.Logf("%-12s %3d tokens", n, size)
		if size > tool.MaxSchemaTokens {
			t.Errorf("%s: %d tokens, cap is %d", n, size, tool.MaxSchemaTokens)
		}
	}
	t.Logf("%-12s %3d tokens", "TOTAL", total)
}

func TestRegisterTwiceFails(t *testing.T) {
	r := registry(t, Deps{})
	if err := Register(r, Deps{}); err == nil {
		t.Fatal("second Register succeeded, want duplicate error")
	}
}

func TestCapText(t *testing.T) {
	if got := capText("hello", 16); got != "hello" {
		t.Errorf("short text was changed: %q", got)
	}
	got := capText("héllo world", 4)
	if !strings.HasPrefix(got, "hé") || strings.Contains(got, "l world") {
		t.Errorf("capText = %q, want a truncated prefix", got)
	}
	if !strings.Contains(got, "[truncated ") {
		t.Errorf("capText = %q, want a truncation marker", got)
	}
	// The cut must land on a rune boundary: "é" is two bytes at offset 1..2.
	if !strings.HasPrefix(got, "hé") {
		t.Errorf("capText split a rune: %q", got)
	}
}

func TestRelPathRefusesEscapes(t *testing.T) {
	for _, p := range []string{"", "   ", "/etc/passwd", "../x", "..", "a/../../b"} {
		if got, err := relPath(p); err == nil {
			t.Errorf("relPath(%q) = %q, want an error", p, got)
		}
	}
	if got, err := relPath("./a/b.txt"); err != nil || got != "a/b.txt" {
		t.Errorf("relPath(./a/b.txt) = %q, %v", got, err)
	}
}
