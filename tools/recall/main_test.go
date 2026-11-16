package main

import (
	"bytes"
	"strings"
	"testing"
)

// An unknown flag is a usage error: exit 2.
func TestUsageError(t *testing.T) {
	var out bytes.Buffer
	code := execute([]string{"--nope", "testdata/pages.txt"}, &out, &out)
	if code != 2 {
		t.Fatalf("code = %d, want 2; output: %s", code, out.String())
	}
}

// The wrong number of positional arguments is also a usage error.
func TestWrongArgCount(t *testing.T) {
	var out bytes.Buffer
	code := execute(nil, &out, &out)
	if code != 2 {
		t.Fatalf("code = %d, want 2; output: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "usage:") {
		t.Fatalf("output = %q, want a usage message", out.String())
	}
}

// --help exits 0 and prints the flags, without opening a browser.
func TestHelp(t *testing.T) {
	var out bytes.Buffer
	code := execute([]string{"--help"}, &out, &out)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "--cap") {
		t.Fatalf("help output = %q, want a mention of --cap", out.String())
	}
}
