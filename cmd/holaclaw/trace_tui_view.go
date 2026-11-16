package main

import (
	"fmt"
	"strings"
	"time"

	bkey "github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/lipgloss"
	"github.com/shopspring/decimal"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// How the trace browser is drawn. Like the memory browser it borrows the
// chat's styles rather than inventing a third palette, and it draws its event
// rows with the chat's own delegate: the same event is the same line in both
// places, which is the whole reason for building this out of that.

// --- the keyboard --------------------------------------------------------------

// traceKeyMap is the browser's whole keyboard, in one place so the bar under
// it and the Update switch can never disagree. It is the memory browser's
// keyboard minus the keys memory needs and traces do not, plus `J`.
type traceKeyMap struct {
	Up       bkey.Binding
	Down     bkey.Binding
	Left     bkey.Binding
	Right    bkey.Binding
	NextPane bkey.Binding
	PrevPane bkey.Binding
	PageUp   bkey.Binding
	PageDown bkey.Binding
	HalfUp   bkey.Binding
	HalfDown bkey.Binding
	Top      bkey.Binding
	Bottom   bkey.Binding
	Open     bkey.Binding
	Filter   bkey.Binding
	Messages bkey.Binding
	Raw      bkey.Binding
	Reload   bkey.Binding
	Help     bkey.Binding
	Close    bkey.Binding
	Quit     bkey.Binding
}

func (k traceKeyMap) ShortHelp() []bkey.Binding {
	return []bkey.Binding{k.Right, k.Down, k.Open, k.Filter, k.Messages, k.Raw, k.Help, k.Quit}
}

func (k traceKeyMap) FullHelp() [][]bkey.Binding {
	return [][]bkey.Binding{
		{k.Left, k.Right, k.NextPane, k.PrevPane},
		{k.Up, k.Down, k.PageUp, k.PageDown, k.HalfUp, k.HalfDown, k.Top, k.Bottom},
		{k.Open, k.Filter, k.Messages, k.Raw},
		{k.Reload, k.Close, k.Quit},
	}
}

func newTraceKeyMap() traceKeyMap {
	return traceKeyMap{
		Up:       motionUp,
		Down:     motionDown,
		Left:     bkey.NewBinding(bkey.WithKeys("left", "h"), bkey.WithHelp("←/h", "pane left")),
		Right:    bkey.NewBinding(bkey.WithKeys("right", "l"), bkey.WithHelp("→/l", "pane right")),
		NextPane: bkey.NewBinding(bkey.WithKeys("tab"), bkey.WithHelp("tab", "next pane")),
		PrevPane: bkey.NewBinding(bkey.WithKeys("shift+tab"), bkey.WithHelp("shift+tab", "previous pane")),
		PageUp:   motionPageUp,
		PageDown: motionPageDown,
		HalfUp:   motionHalfUp,
		HalfDown: motionHalfDown,
		Top:      motionTop,
		Bottom:   motionBottom,
		Open:     bkey.NewBinding(bkey.WithKeys("enter"), bkey.WithHelp("enter", "open")),
		Filter:   bkey.NewBinding(bkey.WithKeys("/"), bkey.WithHelp("/", "filter")),
		Messages: bkey.NewBinding(bkey.WithKeys("m"), bkey.WithHelp("m", "messages")),
		Raw:      bkey.NewBinding(bkey.WithKeys("J"), bkey.WithHelp("J", "raw JSON")),
		Reload:   bkey.NewBinding(bkey.WithKeys("R"), bkey.WithHelp("R", "reload")),
		Help:     bkey.NewBinding(bkey.WithKeys("?"), bkey.WithHelp("?", "help")),
		Close:    bkey.NewBinding(bkey.WithKeys("esc"), bkey.WithHelp("esc", "back")),
		Quit:     bkey.NewBinding(bkey.WithKeys("q", "ctrl+c"), bkey.WithHelp("q", "quit")),
	}
}

// --- layout ----------------------------------------------------------------------

// The fixed rows: the header, the status line under it, and the help bar at
// the foot. Everything between them is the three columns, and the details
// panel when one is open.
const (
	trHeaderRows = 2
	trHelpRows   = 1
	trChromeRows = trHeaderRows + trHelpRows

	// The row a column's first entry lands on: past the chrome, the pane's
	// top border and its heading.
	trRowTop = trHeaderRows + 2

	// The lines the events column spends on the selected trace's totals.
	trTotalsRows = 2

	// How much of the body the details panel takes when it is open. The
	// columns keep the rest, so the event a panel is about stays in sight.
	trDetailShare = 2
)

// layout sizes the columns for the current window. It is the only place that
// decides how wide anything is, so a click and a row can never disagree.
func (m *traceUI) layout() {
	m.width, m.height = max(m.width, minCols), max(m.height, minRows)
	body := max(m.height-trChromeRows, 6)
	if m.showDet {
		m.detail.Height = max(body/trDetailShare-3, 3)
		m.bodyH = max(body-m.detail.Height-4, 4)
	} else {
		m.bodyH = body
	}

	sessW := min(max(m.width/4, 22), 36)
	tracesW := min(max(m.width/3, 26), 52)
	if m.width-sessW-tracesW < 30 {
		tracesW = max(m.width-sessW-30, 18)
	}
	m.sessW, m.tracesW = sessW, tracesW
	m.evsW = max(m.width-sessW-tracesW, 16)

	m.filter.Width = max(m.colW(m.focus)-6, 8)
	m.detail.Width = max(m.width-4, 16)
}

// colW is the whole width of one column, border included.
func (m *traceUI) colW(p trPane) int {
	switch p {
	case paneSessions:
		return m.sessW
	case paneTraces:
		return m.tracesW
	}
	return m.evsW
}

// rowsPerPane is how many entries the focused column shows, which is what a
// page key moves by: a screenful of whatever that column holds.
func (m *traceUI) rowsPerPane() int { return m.rowsFor(m.focus) }

// rowsFor is how many entries one column shows: the body, less its borders
// and heading, less whatever that column spends on something other than an
// entry — its filter line, and the events column's totals strip — divided by
// the lines one entry takes.
func (m *traceUI) rowsFor(p trPane) int {
	return max((m.bodyH-3-m.headRows(p))/trEntryLines(p), 1)
}

// trEntryLines is how many screen lines one entry of a column takes. A
// session and a trace are two — what it is, and under it what its model
// calls cost — and an event is one, the chat's own line.
func trEntryLines(p trPane) int {
	if p == paneEvents {
		return 1
	}
	return 2
}

// headRows is what sits between a column's heading and its first entry.
func (m *traceUI) headRows(p trPane) int {
	n := 0
	if (p == m.focus && m.typing) || m.queries[p] != "" {
		n++
	}
	if p == paneEvents && m.totalsShown() {
		n += trTotalsRows
	}
	return n
}

// totalsShown says whether the events column is carrying the cost strip. It
// hangs off the loaded trace rather than the focus, so walking the columns
// never moves the rows under the pointer.
func (m *traceUI) totalsShown() bool { return len(m.events) > 0 }

// paneAt is which column a pointer column is over.
func (m *traceUI) paneAt(x int) trPane {
	switch {
	case x < m.sessW:
		return paneSessions
	case x < m.sessW+m.tracesW:
		return paneTraces
	}
	return paneEvents
}

// rowAt turns a screen row into an entry of one column, or says the pointer
// was not on one.
func (m *traceUI) rowAt(p trPane, y int) (int, bool) {
	top := trRowTop + m.headRows(p)
	line := y - top
	if line < 0 {
		return 0, false
	}
	// Either line of a two-line entry is that entry.
	row := line / trEntryLines(p)
	if row >= m.rowsFor(p) {
		return 0, false
	}
	i := m.cursorOf(p).off + row
	if i >= m.rows(p) {
		return 0, false
	}
	return i, true
}

// --- the view ----------------------------------------------------------------------

func (m *traceUI) View() string {
	if m.quitting {
		return ""
	}
	parts := []string{m.trHeaderView(), m.trStatusView(), m.trBodyView()}
	if m.showDet && !m.showHelp {
		parts = append(parts, m.trDetailView())
	}
	return strings.Join(append(parts, m.help.View(m.keys)), "\n")
}

// trHeaderView names where somebody is: the daemon, then the session and the
// trace they have walked down to.
func (m *traceUI) trHeaderView() string {
	s := m.styles
	left := s.header.Render("holaclaw") + s.dim.Render(" · traces · "+m.where)
	if sess, ok := m.session(); ok {
		left += s.dim.Render(" · ") + s.accent.Render(sessionLabel(sess))
	}
	right := ""
	if t, ok := m.trace(); ok {
		right = s.dim.Render(fmt.Sprintf("%d ev · %d tools", t.Events, t.ToolCalls))
	}
	return memRow(m.width, left, right)
}

// trStatusView is the one line that answers back: what a request said, or the
// counts and the id of the selected trace — which is what makes this browser
// a way of finding the argument for `trace <id>`.
func (m *traceUI) trStatusView() string {
	s := m.styles
	if m.status != "" {
		if m.isError {
			return truncate(s.errText.Render(m.status), m.width)
		}
		return truncate(s.dim.Render(m.status), m.width)
	}
	events, msgs := 0, 0
	for _, r := range m.evShown {
		if r.isMsg {
			msgs++
			continue
		}
		events++
	}
	left := s.dim.Render(fmt.Sprintf("%d sessions · %d traces · %d events · %d messages",
		len(m.sessShown), len(m.traceShown), events, msgs))
	right := s.dim.Render("? for keys")
	if id := m.traceID(); id != "" {
		right = s.dim.Render("trace ") + s.accent.Render(id)
	}
	return memRow(m.width, left, right)
}

// trBodyView is the three columns, or the help over them.
func (m *traceUI) trBodyView() string {
	if m.showHelp {
		return lipgloss.Place(m.width, m.bodyH, lipgloss.Center, lipgloss.Center, helpPanel(m.styles, m.help, m.keys))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top,
		paneBox(m.styles, m.sessionsPaneView(), m.sessW, m.bodyH, m.focus == paneSessions),
		paneBox(m.styles, m.tracesPaneView(), m.tracesW, m.bodyH, m.focus == paneTraces),
		paneBox(m.styles, m.eventsPaneView(), m.evsW, m.bodyH, m.focus == paneEvents),
	)
}

// --- the columns ---------------------------------------------------------------------

func (m *traceUI) sessionsPaneView() string {
	w := max(m.sessW-2, 6)
	rows := make([]string, 0, len(m.sessShown))
	for i, sess := range m.sessShown {
		tail := m.styles.dim.Render(memAge(sess.UpdatedAt))
		room := max(w-2-lipgloss.Width(tail)-1, 4)
		body := memRow(w-2, truncate(sessionLabel(sess), room), tail)
		rows = append(rows, m.trEntryView(body, m.sessionUsageView(sess, w-2), i == m.sessCur.at, m.focus == paneSessions))
	}
	return m.trPaneView(paneSessions, "sessions", w, rows, m.sessCur, false)
}

// sessionLabel is what a session is called in a column: the agent, the kind
// of conversation and whatever name it has — the title when somebody gave it
// one, the routing key when nobody did.
func sessionLabel(s gen.Session) string {
	name := deref(s.Title)
	if name == "" {
		name = s.Key
	}
	return s.Agent + " · " + string(s.Kind) + " · " + oneLine(name)
}

// sessionUsageView is the second line of a session: what all of its model
// calls cost. A daemon whose store cannot total leaves the numbers out, and
// the line is blank rather than a zero nobody measured.
func (m *traceUI) sessionUsageView(s gen.Session, w int) string {
	if s.PromptTokens == nil && s.CompletionTokens == nil && s.Cost == nil {
		return ""
	}
	line := usageLine(deref(s.PromptTokens), deref(s.CompletionTokens), s.Cost, deref(s.UnpricedCalls))
	return m.styles.dim.Render(truncate(line, w))
}

// usageLine is tokens and dollars the way every list row and the totals strip
// say them: "12.3k→1.1k tokens · $0.0042". The dollars are there only when
// some call reported a price, a free model's $0 included, and carry a "+"
// when other calls reported none: the sum is then only part of the cost.
// No call priced, no dollars — unknown is not free.
func usageLine(prompt, completion int, cost *decimal.Decimal, unpriced int) string {
	s := tokensShort(prompt) + "→" + tokensShort(completion) + " tokens"
	if cost != nil {
		s += " · " + dollars(*cost)
		if unpriced > 0 {
			s += "+"
		}
	}
	return s
}

// dollars is a cost to four decimals, "$0.0042"; a priced cost too small to
// show at four is shown whole ("$0.000012") rather than as a free $0.0000.
func dollars(d decimal.Decimal) string {
	s := d.StringFixed(4)
	if d.IsPositive() && d.Round(4).IsZero() {
		s = d.String()
	}
	return "$" + s
}

// nanoDecimal is a summed cost as the API's decimal, nil staying nil.
func nanoDecimal(n *provider.NanoUSD) *decimal.Decimal {
	if n == nil {
		return nil
	}
	d := n.Decimal()
	return &d
}

func (m *traceUI) tracesPaneView() string {
	w := max(m.tracesW-2, 8)
	if m.loadTraces {
		return m.trPaneView(paneTraces, "traces", w, nil, m.traceCur, true)
	}
	rows := make([]string, 0, len(m.traceShown))
	for i, t := range m.traceShown {
		usage := m.styles.dim.Render(truncate(usageLine(t.PromptTokens, t.CompletionTokens, t.Cost, t.UnpricedCalls), w-2))
		rows = append(rows, m.trEntryView(m.traceRowView(t, w-2), usage, i == m.traceCur.at, m.focus == paneTraces))
	}
	return m.trPaneView(paneTraces, "traces", w, rows, m.traceCur, false)
}

// traceRowView is the first line of a run's entry: when it started, what was
// asked, how much it took and how it ended. What it cost is the line under it.
func (m *traceUI) traceRowView(t gen.TraceSummary, w int) string {
	s := m.styles
	head := s.dim.Render(t.StartedAt.Local().Format("15:04:05"))
	word, style := traceOutcome(deref(t.LastType), s)
	tail := s.dim.Render(fmt.Sprintf("%dev %dt %s", t.Events, t.ToolCalls, shortDur(t.EndedAt.Sub(t.StartedAt))))
	if word != "" {
		tail += " " + style.Render(word)
	}
	what := oneLine(deref(t.Preview))
	if what == "" {
		// Nothing was typed into this trace — a scheduled task, a channel
		// message with only an attachment — so what it did stands in for it.
		what = deref(t.LastType)
	}
	room := max(w-lipgloss.Width(head)-lipgloss.Width(tail)-2, 4)
	return memRow(w, head+" "+truncate(what, room), tail)
}

// traceOutcome is the word at the right end of a trace row: how the run
// ended, or that it has not. A trace whose last event is not a terminal one
// is still being written — the browser is often opened on a run in flight.
func traceOutcome(last string, s tuiStyles) (string, lipgloss.Style) {
	switch event.Type(last) {
	case "":
		return "", s.dim
	case event.Reply, event.RunFinished:
		return "reply", s.okText
	case event.RunAborted:
		return "abort", s.errText
	case event.ModelError, event.ToolRejected, event.RunLoopDetected, event.RunStepLimit, event.ModelUsageLimit:
		return "error", s.errText
	}
	return "running", lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
}

// eventsPaneView draws the events with the chat's own delegate, so a line
// here is the line the activity pane would have shown for the same event.
func (m *traceUI) eventsPaneView() string {
	w := max(m.evsW-2, 8)
	if m.loadEvs {
		return m.trPaneView(paneEvents, "events", w, nil, m.evCur, true)
	}
	rows := make([]string, 0, len(m.evShown))
	d := activityDelegate{styles: m.styles, width: w - 1}
	// The delegate takes the selection off a list, so one is built to hold
	// it rather than the rendering being written a second time here.
	items := make([]list.Item, 0, len(m.evShown))
	for _, r := range m.evShown {
		items = append(items, r.item())
	}
	holder := list.New(items, d, max(w, 1), max(m.rowsFor(paneEvents), 1))
	holder.Select(m.evCur.at)
	for i, r := range m.evShown {
		selected, focused := i == m.evCur.at, m.focus == paneEvents
		if r.isMsg {
			rows = append(rows, m.messageRowView(r.msg, selected, focused, w-1))
			continue
		}
		var b strings.Builder
		d.Render(&b, holder, i, items[i])
		row := b.String()
		if !selected || !focused {
			// The delegate marks its own selection; a column that does not
			// have the keyboard must not look as though it does.
			row = stripSelected(row, selected, m.styles)
		}
		rows = append(rows, row)
	}
	return m.trPaneView(paneEvents, "events", w, rows, m.evCur, false)
}

// messageRowView is one transcript entry on one row, in the column's own
// shape — the same elapsed gutter the events use — and in the colour the chat
// gives that role, so what was said reads differently from what was done.
func (m *traceUI) messageRowView(t traceMsg, selected, focused bool, w int) string {
	s := m.styles
	at := s.dim.Render(fmt.Sprintf("%6.1fs", t.since.Seconds()))
	style := s.plain
	switch t.msg.Role {
	case gen.MessageRoleUser:
		style = s.user
	case gen.MessageRoleTool, gen.MessageRoleSystem:
		style = s.dim
	}
	body := style.Render(truncate(t.line, max(w-9, 4)))
	row := at + " " + body
	switch {
	case selected && focused:
		return s.accent.Render("›") + s.selected.Render(row)
	case selected:
		return s.dim.Render("›") + row
	}
	return " " + row
}

// totalsView is the strip over the events: where the trace's wall time went,
// what it spent in tokens and what the upstreams billed for them.
// It is two lines whether or not the numbers are there, so the rows under it
// never move.
func (m *traceUI) totalsView(w int) string {
	s := m.styles
	t := m.totals
	first := fmt.Sprintf("wall %s · model %s · tool %s",
		shortDur(t.Wall), shortDur(t.Model), shortDur(t.Tool))
	second := fmt.Sprintf("%d model · %d tools", t.ModelCalls, t.ToolCalls)
	if t.Prompt > 0 || t.Completion > 0 || t.Cost != nil {
		second += " · " + usageLine(t.Prompt, t.Completion, nanoDecimal(t.Cost), t.Unpriced)
	}
	return s.dim.Render(truncate(first, w)) + "\n" + s.dim.Render(truncate(second, w))
}

// shortDur is a duration in the width a column can spare.
func shortDur(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

// stripSelected dims the delegate's cursor mark for an unfocused column: the
// row is still the selected one, it is simply not the one being driven.
func stripSelected(row string, selected bool, s tuiStyles) string {
	if !selected {
		return row
	}
	return s.dim.Render("›") + strings.TrimPrefix(stripMark(row), "›")
}

// stripMark drops the leading cursor mark of a rendered row, whatever styling
// the delegate wrapped it in.
func stripMark(row string) string {
	if i := strings.IndexRune(row, '›'); i >= 0 {
		return row[i:]
	}
	return row
}

// trPaneView is a column: its heading, the filter box when this column has
// one, and the rows that fit under it.
func (m *traceUI) trPaneView(p trPane, title string, w int, rows []string, cur cursor, loading bool) string {
	var head []string
	if h := m.filterHead(p, w); h != "" {
		head = append(head, h)
	}
	if p == paneEvents && m.totalsShown() {
		head = append(head, m.totalsView(w))
	}
	return browsePane(m.styles, title, strings.Join(head, "\n"), rows, cur, m.rowsFor(p), loading)
}

// filterHead is the `/` line over a column that is being narrowed.
func (m *traceUI) filterHead(p trPane, w int) string {
	if p == m.focus && m.typing {
		return truncate(m.filter.View(), w)
	}
	if q := m.queries[p]; q != "" {
		return m.styles.dim.Render(truncate("/ "+q, w))
	}
	return ""
}

// trEntryView is a two-line entry: the row as markRow marks it, and under it
// the second line, highlighted with it when the column has the keyboard but
// never carrying a mark of its own — one entry, one cursor.
func (m *traceUI) trEntryView(first, second string, selected, focused bool) string {
	under := " " + second
	if selected && focused {
		under = " " + m.styles.selected.Render(second)
	}
	return markRow(m.styles, first, selected, focused) + "\n" + under
}

// --- the details panel ----------------------------------------------------------

// trDetailView is the whole of one event, under the columns rather than over
// them: what it is about is still on screen above it.
func (m *traceUI) trDetailView() string {
	s := m.styles
	title := s.paneTitle.Render("details")
	if m.showRaw {
		title = s.paneTitle.Render("details") + s.dim.Render(" · raw")
	}
	keys := s.helpKey.Render("[J] raw   [↑↓] walk   [esc] close")
	return paneBox(s, title+"\n"+m.detail.View()+"\n"+keys, m.width, m.detail.Height+4, true)
}

// traceDetailText is detailText with the one line a browser cannot mean: the
// chat dates an event against the turn it belonged to, and there are no turns
// here, only the trace the event is in. Everything else — the attributes, the
// narrowed tool output — is the chat's, unchanged.
func traceDetailText(a activity, out []toolOutput, s tuiStyles) string {
	body := strings.Replace(detailText(a, out, s),
		fmt.Sprintf("  ·  +%.1fs into turn %d", a.since.Seconds(), a.turn),
		fmt.Sprintf("  ·  +%.1fs into the trace", a.since.Seconds()), 1)
	if extra := eventCostLines(a.ev, s); extra != "" {
		// After the header block — the time, the duration, the trace — and
		// before the sentence, which is where the rest of the numbers are.
		body = strings.Replace(body, "\n\n", "\n"+extra+"\n", 1)
	}
	return body
}

// eventCostLines is what an event cost, for the details panel: the tokens and
// the dollars of any event that records model calls — a response, a browser
// task or move, a guard verdict, a scored tool result, a compaction, a media
// call, an embedding, a run's total. The browser's old output_tokens name is
// read when completion_tokens is absent, so older traces still show both
// halves. The cached and reasoning shares are shown when reported, and the
// cost only when reported: a reported $0 is a free call, a missing one an
// unpriced call.
func eventCostLines(ev gen.Event, s tuiStyles) string {
	var lines []string
	prompt, _ := evNum(ev, event.AttrPromptTokens)
	completion, ok := evNum(ev, event.AttrCompletionTokens)
	if !ok {
		completion, _ = evNum(ev, event.AttrOutputTokens)
	}
	if prompt > 0 || completion > 0 {
		line := fmt.Sprintf("tokens %s→%s", tokensShort(int(prompt)), tokensShort(int(completion)))
		var parts []string
		if n, ok := evNum(ev, event.AttrCachedTokens); ok {
			parts = append(parts, "cached "+tokensShort(int(n)))
		}
		if n, ok := evNum(ev, event.AttrReasoningTokens); ok {
			parts = append(parts, "reasoning "+tokensShort(int(n)))
		}
		if len(parts) > 0 {
			line += " (" + strings.Join(parts, ", ") + ")"
		}
		lines = append(lines, s.dim.Render(line))
	}
	if c := evCost(ev); c != nil {
		cost := "cost " + dollars(c.Decimal())
		if n, ok := evNum(ev, event.AttrUnpricedCalls); ok && n > 0 {
			cost += "+"
		}
		lines = append(lines, s.dim.Render(cost))
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n")
}
