package main

import (
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/termutil"
)

// The terminal side of `chat`: is anybody looking, and what colour is a line.
// Nothing here writes; it only decides. A stream that is not a terminal — a
// pipe, a test buffer, a log file — gets the same bytes it always got.

// styleKind names one role in the chat's palette rather than a colour, so the
// escape codes live in one table and a caller says what a line *is*.
type styleKind string

const (
	styleDim      styleKind = "dim"
	styleBold     styleKind = "bold"
	styleCyan     styleKind = "cyan"
	styleGreen    styleKind = "green"
	styleYellow   styleKind = "yellow"
	styleRed      styleKind = "red"
	stylePrompt   styleKind = "prompt"   // the "> " at the start of a line
	styleApproval styleKind = "approval" // a question that stops the turn
)

// codes is the whole palette: SGR parameters, reset excluded.
var codes = map[styleKind]string{
	styleDim:      "2",
	styleBold:     "1",
	styleCyan:     "36",
	styleGreen:    "32",
	styleYellow:   "33",
	styleRed:      "31",
	stylePrompt:   "1;36",
	styleApproval: "1;33",
}

const reset = "\x1b[0m"

// theme is the answer to "may I colour this?" for one run.
type theme struct{ on bool }

// newTheme decides for a stream: colours only for a terminal that has not
// asked to be left alone.
func newTheme(w io.Writer) *theme {
	return &theme{on: colorsEnabled(w, os.LookupEnv)}
}

// colorsEnabled is newTheme's rule, with the environment passed in so a test
// can state it. NO_COLOR (set and not empty) and TERM=dumb both win over a
// real terminal; everything else follows the stream.
func colorsEnabled(w io.Writer, lookup func(string) (string, bool)) bool {
	return colorRules(isTerminal(w), lookup)
}

// colorRules is the decision itself, with the terminal already answered for.
func colorRules(tty bool, lookup func(string) (string, bool)) bool {
	if v, ok := lookup("NO_COLOR"); ok && v != "" {
		return false
	}
	if v, _ := lookup("TERM"); v == "dumb" {
		return false
	}
	return tty
}

// style wraps text for one role, or returns it untouched when colours are off
// — so a caller never has to ask twice.
func (t *theme) style(k styleKind, text string) string {
	if t == nil || !t.on || text == "" {
		return text
	}
	c, ok := codes[k]
	if !ok {
		return text
	}
	return "\x1b[" + c + "m" + text + reset
}

// isTerminal reports whether w is a terminal. A writer that cannot name a
// file descriptor (a buffer, a pipe wrapper) never is, which is what keeps
// the tests and every redirected run plain.
func isTerminal(w io.Writer) bool {
	f, ok := w.(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	return termutil.IsTerminal(int(f.Fd()))
}

// terminalWidth is the column count of fd's window, or 0 when it cannot be
// read — a caller then falls back to behaving as if the line never wraps.
func terminalWidth(fd int) int {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 {
		return 0
	}
	return int(ws.Col)
}

// --- colouring progress ----------------------------------------------------

// eventStyle is the colour of a progress line's first word: what kind of
// thing is happening, at a glance down the left edge.
func eventStyle(ev gen.Event) styleKind {
	switch event.Type(ev.Type) {
	case event.ToolCallEvent:
		return styleGreen
	case event.ToolResult:
		if evBool(ev, "is_error") {
			return styleRed
		}
		return styleGreen
	case event.GuardDecision, event.PolicyDecision, event.ModelUsageLimit,
		event.ModelFallback, event.RunStepLimit, event.RunLoopDetected:
		return styleYellow
	case event.ModelError, event.RunAborted, event.BrowserBlocked:
		return styleRed
	case event.BrowserTaskStarted, event.BrowserMove, event.BrowserScreenshot, event.BrowserStep,
		event.BrowserTakeoverRequest, event.BrowserTakeoverStarted, event.BrowserTakeoverEnded,
		event.BrowserDownload, event.BrowserTaskFinished:
		return styleCyan
	}
	return styleDim
}

// colorLine paints one progress line: the leading word in the event's colour,
// the rest dim, so the line reads as one thing and still stands out.
func (t *theme) colorLine(k styleKind, line string) string {
	if t == nil || !t.on {
		return line
	}
	head, rest, found := strings.Cut(line, " ")
	if !found {
		return t.style(k, line)
	}
	return t.style(k, head) + " " + t.style(styleDim, rest)
}
