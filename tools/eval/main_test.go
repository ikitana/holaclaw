package main

import (
	"bytes"
	"strings"
	"testing"
)

// An unknown flag on the root command is a usage error: exit 2.
func TestUsageError(t *testing.T) {
	var out bytes.Buffer
	code := execute([]string{"--nope"}, &out, &out)
	if code != 2 {
		t.Fatalf("code = %d, want 2; output: %s", code, out.String())
	}
}

// An unknown flag on a child command is also a usage error.
func TestSubcommandUsageError(t *testing.T) {
	for _, sub := range []string{"guard", "taint", "browser"} {
		var out bytes.Buffer
		code := execute([]string{sub, "--nope"}, &out, &out)
		if code != 2 {
			t.Fatalf("eval %s --nope: code = %d, want 2; output: %s", sub, code, out.String())
		}
	}
}

// A stray positional argument is a usage error too: none of these commands
// takes one.
func TestUnexpectedArgument(t *testing.T) {
	var out bytes.Buffer
	code := execute([]string{"nonsense"}, &out, &out)
	if code != 2 {
		t.Fatalf("code = %d, want 2; output: %s", code, out.String())
	}
}

// --help on the root command exits 0 and lists the three subcommands.
func TestHelp(t *testing.T) {
	var out bytes.Buffer
	code := execute([]string{"--help"}, &out, &out)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	for _, want := range []string{"guard", "taint", "browser", "--scenarios"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help output = %q, want a mention of %q", out.String(), want)
		}
	}
}

// --help on each subcommand exits 0 and shows that subcommand's own flags.
func TestSubcommandHelp(t *testing.T) {
	cases := map[string]string{
		"guard":   "--reference",
		"taint":   "--threshold",
		"browser": "--tasks-file",
	}
	for sub, want := range cases {
		var out bytes.Buffer
		code := execute([]string{sub, "--help"}, &out, &out)
		if code != 0 {
			t.Fatalf("eval %s --help: code = %d, want 0; output: %s", sub, code, out.String())
		}
		if !strings.Contains(out.String(), want) {
			t.Fatalf("eval %s --help output = %q, want a mention of %q", sub, out.String(), want)
		}
	}
}
