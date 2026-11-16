package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGenerateDeterministic runs the full generator twice into separate temp
// directories and checks every file is byte-identical. Skipped under -short:
// it builds the holaclaw binary twice.
func TestGenerateDeterministic(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the holaclaw binary")
	}
	a := t.TempDir()
	b := t.TempDir()
	if err := Generate(a); err != nil {
		t.Fatal(err)
	}
	if err := Generate(b); err != nil {
		t.Fatal(err)
	}
	compareTrees(t, a, b)
}

// TestGenerateMatchesCommitted regenerates into a temp directory and diffs
// against the committed docs/reference: `just docs-check` runs the same
// check without touching the working tree. Skipped under -short.
func TestGenerateMatchesCommitted(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the holaclaw binary")
	}
	tmp := t.TempDir()
	if err := Generate(tmp); err != nil {
		t.Fatal(err)
	}
	compareTrees(t, tmp, inRepo("docs/reference"))
}

func compareTrees(t *testing.T, a, b string) {
	t.Helper()
	entries, err := os.ReadDir(a)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	other, err := os.ReadDir(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range other {
		names[e.Name()] = true
	}
	for name := range names {
		wantBytes, err1 := os.ReadFile(filepath.Join(a, name))
		gotBytes, err2 := os.ReadFile(filepath.Join(b, name))
		if err1 != nil || err2 != nil {
			t.Errorf("%s: present in one directory but not the other (%v / %v)", name, err1, err2)
			continue
		}
		if string(wantBytes) != string(gotBytes) {
			t.Errorf("%s differs between %s and %s", name, a, b)
		}
	}
}
