package main

import (
	"bytes"
	"strings"
	"testing"
)

// An unknown flag is a usage error: exit 2, and the message says so.
func TestUsageError(t *testing.T) {
	var out bytes.Buffer
	code := execute([]string{"--nope"}, &out, &out)
	if code != 2 {
		t.Fatalf("code = %d, want 2; output: %s", code, out.String())
	}
}

// Missing --url/--intent is also a usage error, not a run failure.
func TestMissingRequiredFlags(t *testing.T) {
	var out bytes.Buffer
	code := execute(nil, &out, &out)
	if code != 2 {
		t.Fatalf("code = %d, want 2; output: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "--url") {
		t.Fatalf("output = %q, want a mention of --url", out.String())
	}
}

// --help exits 0 and prints the flags, without opening a browser.
func TestHelp(t *testing.T) {
	var out bytes.Buffer
	code := execute([]string{"--help"}, &out, &out)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "--intent") {
		t.Fatalf("help output = %q, want a mention of --intent", out.String())
	}
}
