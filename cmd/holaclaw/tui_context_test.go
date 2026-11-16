package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"holaclaw/internal/event"
)

// builtEvent is a context.built as the builder reports it, with the numbers
// already decoded from JSON the way both modes hand them over.
func builtEvent(window, system, tools, history, volatile, reserve int, compact bool) eventMsg {
	return eventMsg{progEvent(event.ContextBuilt, map[string]any{
		"window": float64(window), "system": float64(system), "tool_schemas": float64(tools),
		"history": float64(history), "volatile": float64(volatile), "reply_reserve": float64(reserve),
		"total": float64(system + tools + history + volatile), "needs_compaction": compact,
	})}
}

// Connector tools are tools on the bar: context.built reports them apart.
func TestContextRowCountsConnectors(t *testing.T) {
	ev := progEvent(event.ContextBuilt, map[string]any{
		"window": float64(100_000), "system": float64(2000), "tool_schemas": float64(3000),
		"connectors": float64(4000), "total": float64(9000),
	})
	u, ok := contextFrom(ev)
	if !ok || u.tools != 7000 {
		t.Fatalf("context = %+v", u)
	}
}

// Before any prompt is built the row says so rather than drawing an empty bar
// that reads as a conversation with nothing in it.
func TestContextRowBeforeThePrompt(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	if got := stripStyle(m.contextView()); !strings.Contains(got, "no prompt built yet") {
		t.Errorf("row before the first prompt: %q", got)
	}
}

// A context.built fills the row with the builder's numbers, and the next one
// replaces them.
func TestContextRowFollowsContextBuilt(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	send(m, builtEvent(100_000, 2000, 3000, 5000, 0, 1024, false))
	row := stripStyle(m.contextView())
	for _, want := range []string{"10%", "10.0k / 100.0k", "system 2.0k", "tools 3.0k", "history 5.0k"} {
		if !strings.Contains(row, want) {
			t.Errorf("row lacks %q:\n%s", want, row)
		}
	}
	send(m, builtEvent(100_000, 2000, 3000, 45000, 0, 1024, false))
	if row := stripStyle(m.contextView()); !strings.Contains(row, "50%") {
		t.Errorf("the second prompt did not replace the first:\n%s", row)
	}
	if !strings.Contains(stripStyle(m.View()), "50% · 50.0k / 100.0k") {
		t.Error("the row is not on screen")
	}
}

// The bar is as full as the prompt: filled cells for what was sent, the
// reserve at the end, and a tick where compaction starts.
func TestContextBarShares(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	u := contextUse{window: 1000, system: 100, tools: 100, history: 300, reserve: 100, total: 500}
	bar := stripStyle(m.contextBar(u, 100))
	if n := strings.Count(bar, "█"); n != 50 {
		t.Errorf("filled cells = %d, want 50:\n%s", n, bar)
	}
	if n := strings.Count(bar, "▒"); n != 10 {
		t.Errorf("reserve cells = %d, want 10:\n%s", n, bar)
	}
	// 200 fixed + 70% of the 700 left after the reserve = 690.
	if i := strings.Index(bar, "┊"); len([]rune(bar[:max(i, 0)])) != 69 {
		t.Errorf("compaction tick at %d, want cell 69:\n%s", len([]rune(bar[:max(i, 0)])), bar)
	}
	if got := len([]rune(bar)); got != 100 {
		t.Errorf("bar is %d cells, want 100", got)
	}

	// Past the threshold there is no tick left to draw, and the row says
	// what the builder asked for.
	send(m, builtEvent(1000, 100, 100, 600, 0, 100, true))
	if row := stripStyle(m.contextView()); strings.Contains(row, "┊") || !strings.Contains(row, "compacting next turn") {
		t.Errorf("row past the threshold:\n%s", row)
	}
	send(m, eventMsg{progEvent(event.ContextCompacted, nil)})
	if row := stripStyle(m.contextView()); !strings.Contains(row, "compacted") {
		t.Errorf("row after a compaction:\n%s", row)
	}
}

// The row is one line at any width: the breakdown gives way first, the
// percentage never does.
func TestContextRowFitsANarrowWindow(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	send(m, builtEvent(128_000, 3000, 4000, 20000, 500, 4096, false))
	for _, w := range []int{40, 80, 120, 200} {
		m.Update(tea.WindowSizeMsg{Width: w, Height: 40})
		row := m.contextView()
		if strings.Contains(row, "\n") || lipgloss.Width(row) > w {
			t.Errorf("at %d columns the row is %d wide:\n%s", w, lipgloss.Width(row), row)
		}
		if !strings.Contains(stripStyle(row), "21%") {
			t.Errorf("at %d columns the percentage fell off:\n%s", w, stripStyle(row))
		}
		if got := lipgloss.Height(m.View()); got != m.height {
			t.Errorf("at %d columns the screen is %d rows, want %d", w, got, m.height)
		}
	}
}

// A new conversation starts with an empty row: the last one's prompt is not
// this one's.
func TestContextRowClearsOnReset(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	send(m, builtEvent(1000, 100, 100, 300, 0, 100, false))
	send(m, resetMsg{})
	if m.ctxUse != nil {
		t.Error("the context row survived a new conversation")
	}
}
