package main

import (
	"os"
	"strings"
	"testing"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
)

// env builds a lookup over a fixed map, so a colour decision is stated rather
// than inherited from whatever ran the tests.
func env(kv map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := kv[k]
		return v, ok
	}
}

func TestColorRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		tty  bool
		env  map[string]string
		want bool
	}{
		{"a terminal", true, nil, true},
		{"a pipe", false, nil, false},
		{"NO_COLOR", true, map[string]string{"NO_COLOR": "1"}, false},
		{"NO_COLOR empty is not set", true, map[string]string{"NO_COLOR": ""}, true},
		{"TERM=dumb", true, map[string]string{"TERM": "dumb"}, false},
		{"TERM=xterm", true, map[string]string{"TERM": "xterm-256color"}, true},
		{"NO_COLOR on a pipe", false, map[string]string{"NO_COLOR": "1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := colorRules(tc.tty, env(tc.env)); got != tc.want {
				t.Errorf("colorRules(%v, %v) = %v, want %v", tc.tty, tc.env, got, tc.want)
			}
		})
	}
}

func TestStyleIsANoOpWithoutColors(t *testing.T) {
	off := &theme{on: false}
	if got := off.style(styleRed, "boom"); got != "boom" {
		t.Errorf("style with colours off = %q", got)
	}
	if got := off.colorLine(styleGreen, "tool files.write"); got != "tool files.write" {
		t.Errorf("colorLine with colours off = %q", got)
	}
	if got := (*theme)(nil).style(styleDim, "x"); got != "x" {
		t.Errorf("style on a nil theme = %q", got)
	}

	on := &theme{on: true}
	got := on.style(styleRed, "boom")
	if !strings.HasPrefix(got, "\x1b[31m") || !strings.HasSuffix(got, reset) {
		t.Errorf("style with colours on = %q", got)
	}
	if on.style(styleDim, "") != "" {
		t.Error("an empty string came back with escapes around it")
	}
	// The leading word carries the colour, the rest is dim.
	line := on.colorLine(styleGreen, "tool files.write")
	if !strings.HasPrefix(line, "\x1b[32mtool"+reset+" ") || !strings.Contains(line, "\x1b[2mfiles.write") {
		t.Errorf("colorLine = %q", line)
	}
}

func TestEventStyle(t *testing.T) {
	for _, tc := range []struct {
		ev   gen.Event
		want styleKind
	}{
		{progEvent(event.ToolCallEvent, map[string]any{"tool": "files.write"}), styleGreen},
		{progEvent(event.ToolResult, map[string]any{"is_error": true}), styleRed},
		{progEvent(event.ToolResult, map[string]any{"is_error": false}), styleGreen},
		{progEvent(event.GuardDecision, nil), styleYellow},
		{progEvent(event.PolicyDecision, nil), styleYellow},
		{progEvent(event.BrowserStep, nil), styleCyan},
		{progEvent(event.RunAborted, nil), styleRed},
		{progEvent(event.ModelRequest, nil), styleDim},
	} {
		if got := eventStyle(tc.ev); got != tc.want {
			t.Errorf("eventStyle(%s) = %s, want %s", tc.ev.Type, got, tc.want)
		}
	}
}

// A buffer is never a terminal; a pty master always is. Both matter: the
// first keeps every test and every redirected run plain.
func TestIsTerminal(t *testing.T) {
	if isTerminal(&strings.Builder{}) {
		t.Error("a strings.Builder claims to be a terminal")
	}
	pty, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	defer func() { _ = pty.Close() }()
	if !isTerminal(pty) {
		t.Error("a pty master is not recognised as a terminal")
	}
}
