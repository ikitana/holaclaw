package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"holaclaw/internal/agent"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
)

// The context row: how full the model's window was when the last prompt went
// out, drawn as one bar under the prompt box. It is read off context.built —
// the builder's own accounting, the numbers `holaclaw prompt-size` prints — so
// it is what the model was actually sent, not an estimate made here. Both
// modes forward that event, so the row fills the same way attached or not.

// contextUse is one context.built, in the parts the bar is drawn from.
type contextUse struct {
	window, system, tools, history, volatile, reserve, total int

	compact   bool // the builder asked for compaction after this prompt
	compacted bool // a compaction ran since this prompt was built
}

// contextFrom reads a context.built event. One without a window says nothing
// a bar can be drawn from.
func contextFrom(ev gen.Event) (contextUse, bool) {
	n := func(k string) int {
		v, _ := evNum(ev, k)
		return int(v)
	}
	u := contextUse{
		window: n("window"), system: n("system"), tools: n("tool_schemas") + n("connectors"),
		history: n("history"), volatile: n("volatile"), reserve: n("reply_reserve"),
		total: n("total"),
	}
	if ev.Attrs != nil {
		u.compact, _ = (*ev.Attrs)["needs_compaction"].(bool)
	}
	return u, u.window > 0
}

// noteContext is what the row learns from one event.
func (m *chatUI) noteContext(ev gen.Event) {
	switch event.Type(ev.Type) {
	case event.ContextBuilt:
		if u, ok := contextFrom(ev); ok {
			m.ctxUse = &u
		}
	case event.ContextCompacted:
		if m.ctxUse != nil {
			m.ctxUse.compacted = true
		}
	}
}

// compactAt is the token count at which history crosses the builder's
// compaction threshold: CompactionPct of what the window leaves for it.
func (u contextUse) compactAt() int {
	fixed := u.system + u.tools + u.volatile
	available := u.window - fixed - u.reserve
	if available <= 0 {
		return 0
	}
	return fixed + available*agent.CompactionPct/100
}

// contextView is the row: a label, the bar, and the numbers. The bar is
// stacked in the order the prompt is — system, tool schemas, history, the
// volatile tail — then the free space, with the reply reserve at the far end
// because it is never the context's to fill. A tick in the free space marks
// where compaction starts. In a narrow window the breakdown goes first, then
// the bar shrinks; the percentage stays.
func (m *chatUI) contextView() string {
	label := m.styles.dim.Render("context ")
	u := m.ctxUse
	if u == nil {
		return clipStyled(label+m.styles.dim.Render("· no prompt built yet"), m.width)
	}

	pct := u.total * 100 / u.window
	sum := fmt.Sprintf(" %d%% · %s / %s", pct, tokensShort(u.total), tokensShort(u.window))
	sumStyle := m.styles.dim
	switch {
	case u.compacted:
		sum += " · compacted"
	case u.compact:
		sum += " · compacting next turn"
		sumStyle = m.styles.modalTitle
	}
	legend := "  " + m.contextLegend(*u)

	const minBar = 10
	room := m.width - lipgloss.Width(label) - lipgloss.Width(sum)
	if room-lipgloss.Width(legend) < minBar*2 {
		legend = ""
	}
	w := max(room-lipgloss.Width(legend), minBar)
	return clipStyled(label+m.contextBar(*u, w)+sumStyle.Render(sum)+legend, m.width)
}

// contextSegment is one stretch of the bar.
type contextSegment struct {
	name   string
	tokens int
	style  lipgloss.Style
}

func (m *chatUI) contextSegments(u contextUse) []contextSegment {
	return []contextSegment{
		{"system", u.system, lipgloss.NewStyle().Foreground(lipgloss.Color("5"))},
		{"tools", u.tools, lipgloss.NewStyle().Foreground(lipgloss.Color("4"))},
		{"history", u.history, m.styles.accent},
		{"tail", u.volatile, lipgloss.NewStyle().Foreground(lipgloss.Color("3"))},
	}
}

// contextBar draws w cells for the window. A cell belongs to the segment its
// start falls in, so the stretches add up to the bar without a gap however
// the rounding goes.
func (m *chatUI) contextBar(u contextUse, w int) string {
	segs := m.contextSegments(u)
	cell := func(tokens int) int { return min(tokens*w/u.window, w) }
	mark := -1
	if at := u.compactAt(); at > u.total {
		mark = cell(at)
	}
	reserveFrom := cell(u.window - u.reserve)

	var b strings.Builder
	edge, used := 0, 0
	for _, s := range segs {
		used += s.tokens
		end := cell(used)
		if end > edge {
			b.WriteString(s.style.Render(strings.Repeat("█", end-edge)))
			edge = end
		}
	}
	for i := edge; i < w; i++ {
		switch {
		case i == mark && i < reserveFrom:
			b.WriteString(m.styles.modalTitle.Render("┊"))
		case i >= reserveFrom:
			b.WriteString(m.styles.dim.Render("▒"))
		default:
			b.WriteString(m.styles.dim.Render("░"))
		}
	}
	return b.String()
}

// contextLegend names the segments, in their colours, with what each holds.
func (m *chatUI) contextLegend(u contextUse) string {
	var parts []string
	for _, s := range m.contextSegments(u) {
		parts = append(parts, s.style.Render("■")+m.styles.dim.Render(" "+s.name+" "+tokensShort(s.tokens)))
	}
	return strings.Join(parts, " ")
}
