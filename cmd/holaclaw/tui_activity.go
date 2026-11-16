package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
)

// The activity pane: every trace event of the chat, one line each, and the
// panel behind them. This is what replaces -vvv — instead of dumping every
// tool response under the reply, the whole response is one keystroke (or one
// click) away from the line that produced it.

// activity is one event as the pane shows it.
type activity struct {
	ev    gen.Event
	turn  int           // which turn it belonged to, 1-based
	since time.Duration // since that turn was sent
	line  string        // the same sentence the line-by-line UI would print
	extra []string      // the block under it, e.g. a browser move queue
}

// newActivity folds one event into a pane entry. Everything is kept, at the
// most detailed level, because the pane is where detail belongs now.
func newActivity(ev gen.Event, turn int, since time.Duration) activity {
	line := progressLine(ev, 3)
	if line == "" {
		// A type with no sentence of its own still belongs on the timeline.
		line = string(ev.Type)
	}
	return activity{ev: ev, turn: turn, since: since, line: line, extra: progressExtra(ev, 3)}
}

// FilterValue lets the pane's `/` search reach the words of a line.
func (a activity) FilterValue() string { return string(a.ev.Type) + " " + a.line }

// activityDelegate draws one entry per row: a dim elapsed column, then the
// sentence in the colour of the thing that happened. One row per item is what
// makes a click land on the entry somebody aimed at.
type activityDelegate struct {
	styles tuiStyles
	width  int
}

func (d activityDelegate) Height() int                         { return 1 }
func (d activityDelegate) Spacing() int                        { return 0 }
func (d activityDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }

func (d activityDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	a, ok := item.(activity)
	if !ok {
		return
	}
	at := d.styles.dim.Render(fmt.Sprintf("%6.1fs", a.since.Seconds()))
	body := lipgloss.NewStyle().Foreground(d.styles.eventColor(a.ev)).Render(truncate(a.line, d.width-9))
	row := at + " " + body
	if index == m.Index() {
		row = d.styles.selected.Render("›" + at + " " + body)
	} else {
		row = " " + row
	}
	_, _ = io.WriteString(w, row)
}

// --- the details panel -----------------------------------------------------------

// detailText is everything known about one event, for the panel: the sentence
// the line shows, the block under it, every attribute, and — for a tool call —
// the tool's whole response, which no line ever carries.
//
// Only lines that are short by construction are styled. The long ones are
// left plain, because the panel folds them to its width and a fold through
// the middle of an escape sequence paints the rest of the pane.
func detailText(a activity, out []toolOutput, s tuiStyles) string {
	var b strings.Builder
	b.WriteString(s.detailKey.Render(string(a.ev.Type)) + "\n")
	b.WriteString(s.dim.Render(a.ev.Time.Local().Format("15:04:05.000")+
		fmt.Sprintf("  ·  +%.1fs into turn %d", a.since.Seconds(), a.turn)) + "\n")
	if a.ev.DurationNs != nil {
		b.WriteString(s.dim.Render("took "+time.Duration(*a.ev.DurationNs).String()) + "\n")
	}
	if a.ev.TraceId != "" {
		b.WriteString(s.dim.Render("trace "+a.ev.TraceId) + "\n")
	}
	b.WriteString("\n" + a.line + "\n")
	for _, e := range a.extra {
		b.WriteString("  " + e + "\n")
	}
	if attrs := attrLines(a.ev); len(attrs) > 0 {
		b.WriteString("\n" + s.detailKey.Render("attributes") + "\n")
		for _, l := range attrs {
			b.WriteString(l + "\n")
		}
	}
	if len(out) > 0 {
		b.WriteString("\n" + s.detailKey.Render("tool output") + "\n")
		for _, o := range out {
			name := o.name
			if name == "" {
				name = "tool"
			}
			b.WriteString(s.dim.Render("—") + " " + name + "\n")
			b.WriteString(strings.TrimRight(o.text, "\n") + "\n")
		}
	}
	return b.String()
}

// attrLines renders an event's attributes in a stable order, a scalar on its
// key's line and anything nested indented under it.
func attrLines(ev gen.Event) []string {
	if ev.Attrs == nil || len(*ev.Attrs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(*ev.Attrs))
	for k := range *ev.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		v := (*ev.Attrs)[k]
		switch v.(type) {
		case map[string]any, []any:
			b, err := json.MarshalIndent(v, "  ", "  ")
			if err != nil {
				continue
			}
			out = append(out, "  "+k+":")
			for _, l := range strings.Split(string(b), "\n") {
				out = append(out, "  "+l)
			}
		default:
			out = append(out, fmt.Sprintf("  %s: %v", k, v))
		}
	}
	return out
}

// outputsFor is the part of a trace's tool output that belongs to one event:
// the result of the call the line is about, and not of the other calls of the
// same turn. The cache is kept per trace because that is what the engine can
// fetch in one go, so the narrowing happens here, when the panel is drawn.
//
// An event without a call id is one from before the loop carried one, and
// there is nothing better to show it than everything the trace answered.
func outputsFor(ev gen.Event, out []toolOutput) []toolOutput {
	id := evAttr(ev, "call_id")
	if id == "" {
		return out
	}
	var mine []toolOutput
	for _, o := range out {
		if o.callID == id {
			mine = append(mine, o)
		}
	}
	return mine
}

// wantsToolOutput reports whether opening this entry should fetch the tool
// responses of its trace. Only a tool line has one behind it.
func wantsToolOutput(ev gen.Event) bool {
	switch event.Type(ev.Type) {
	case event.ToolCallEvent, event.ToolResult:
		return true
	}
	return false
}

// truncate cuts a line to n runes, the last an ellipsis, counting runes
// rather than bytes so an arrow or an accent counts once and not three
// times.
func truncate(s string, n int) string {
	if n <= 1 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
