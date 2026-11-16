package main

import (
	"fmt"
	"strings"
	"time"

	bkey "github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/lipgloss"

	"holaclaw/internal/api/gen"
)

// How the memory browser is drawn. It borrows the chat's styles rather than
// inventing a second palette: the two are the same program to whoever is
// looking at them, and a pane should not change colour because of which
// command opened it.

// --- the keyboard --------------------------------------------------------------

// memKeyMap is the browser's whole keyboard, in one place so the bar under it
// and the Update switch can never disagree.
type memKeyMap struct {
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
	Search   bkey.Binding
	Notes    bkey.Binding
	Recall   bkey.Binding
	Both     bkey.Binding
	Add      bkey.Binding
	Save     bkey.Binding
	Forget   bkey.Binding
	Reload   bkey.Binding
	Help     bkey.Binding
	Close    bkey.Binding
	Quit     bkey.Binding
}

func (k memKeyMap) ShortHelp() []bkey.Binding {
	return []bkey.Binding{k.Right, k.Down, k.Open, k.Search, k.Add, k.Forget, k.Help, k.Quit}
}

func (k memKeyMap) FullHelp() [][]bkey.Binding {
	return [][]bkey.Binding{
		{k.Left, k.Right, k.NextPane, k.PrevPane},
		{k.Up, k.Down, k.PageUp, k.PageDown, k.HalfUp, k.HalfDown, k.Top, k.Bottom},
		{k.Open, k.Search, k.Notes, k.Recall, k.Both},
		{k.Add, k.Save, k.Forget, k.Reload, k.Close, k.Quit},
	}
}

func newMemKeyMap() memKeyMap {
	return memKeyMap{
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
		Search:   bkey.NewBinding(bkey.WithKeys("/"), bkey.WithHelp("/", "search")),
		Notes:    bkey.NewBinding(bkey.WithKeys("n"), bkey.WithHelp("n", "notes only")),
		Recall:   bkey.NewBinding(bkey.WithKeys("r"), bkey.WithHelp("r", "recall only")),
		Both:     bkey.NewBinding(bkey.WithKeys("b"), bkey.WithHelp("b", "both layers")),
		Add:      bkey.NewBinding(bkey.WithKeys("a"), bkey.WithHelp("a", "add a note")),
		Save:     bkey.NewBinding(bkey.WithKeys("ctrl+s"), bkey.WithHelp("ctrl+s", "save the note")),
		Forget:   bkey.NewBinding(bkey.WithKeys("d"), bkey.WithHelp("d", "forget")),
		Reload:   bkey.NewBinding(bkey.WithKeys("R"), bkey.WithHelp("R", "reload")),
		Help:     bkey.NewBinding(bkey.WithKeys("?"), bkey.WithHelp("?", "help")),
		Close:    bkey.NewBinding(bkey.WithKeys("esc"), bkey.WithHelp("esc", "close")),
		Quit:     bkey.NewBinding(bkey.WithKeys("q", "ctrl+c"), bkey.WithHelp("q", "quit")),
	}
}

// --- layout ----------------------------------------------------------------------

// The fixed rows: the header, the status line under it, and the help bar at
// the foot. Everything between them is the three columns.
const (
	memHeaderRows = 2
	memHelpRows   = 1
	memChromeRows = memHeaderRows + memHelpRows

	// The row a column's first entry lands on: past the chrome, the pane's
	// top border and its heading.
	memRowTop = memHeaderRows + 2
)

// layout sizes the three columns for the current window. It is the only place
// that decides how wide anything is, so a click and a row can never disagree.
func (m *memoryUI) layout() {
	m.width, m.height = max(m.width, minCols), max(m.height, minRows)
	m.bodyH = max(m.height-memChromeRows, 5)

	// One agent is a label, not a choice, so the column shrinks to what it
	// takes to read the name and the other two get the space.
	agentsW := min(max(m.width/6, 16), 24)
	if len(m.agents) <= 1 {
		agentsW = 14
	}
	partsW := min(max(m.width/3, 26), 44)
	if m.width-agentsW-partsW < 28 {
		partsW = max(m.width-agentsW-28, 18)
	}
	m.agentsW, m.partsW = agentsW, partsW
	m.itemsW = max(m.width-agentsW-partsW, 16)

	m.search.Width = max(m.itemsW-6, 8)

	w := max(min(m.width-8, 76), 24)
	m.detail.Width = w - 4
	m.detail.Height = max(m.bodyH-6, 3)
	m.noteIn.SetWidth(w - 6)
	m.about.Width = w - 8
}

// rowsPerPane is how many entries a column shows. The memories column is the
// tightest of the three — a search box and a "latest 100" footer both cost it
// a row — and every column is measured by it, so one page key means the same
// thing wherever the focus is.
func (m *memoryUI) rowsPerPane() int {
	return max(m.bodyH-3-m.itemsExtraRows(), 1)
}

// itemsExtraRows is what the memories column spends on something other than a
// memory.
func (m *memoryUI) itemsExtraRows() int {
	n := 0
	if m.typing || m.query != "" {
		n++
	}
	if len(m.items) >= memoryLimit {
		n++
	}
	return n
}

// paneAt is which column a pointer column is over.
func (m *memoryUI) paneAt(x int) memPane {
	switch {
	case x < m.agentsW && len(m.agents) > 1:
		return paneAgents
	case x < m.agentsW+m.partsW:
		return paneParts
	}
	return paneItems
}

// rowAt turns a screen row into an entry of one column, or says the pointer
// was not on one.
func (m *memoryUI) rowAt(p memPane, y int) (int, bool) {
	top := memRowTop
	if p == paneItems && (m.typing || m.query != "") {
		top++
	}
	row := y - top
	if row < 0 || row >= m.rowsPerPane() {
		return 0, false
	}
	i := m.cursorOf(p).off + row
	if i >= m.rows(p) {
		return 0, false
	}
	return i, true
}

// --- the view ----------------------------------------------------------------------

func (m *memoryUI) View() string {
	if m.quitting {
		return ""
	}
	return strings.Join([]string{
		m.memHeaderView(),
		m.memStatusView(),
		m.memBodyView(),
		m.help.View(m.keys),
	}, "\n")
}

// memHeaderView names where in the memory somebody is: the daemon, then the
// path they have walked down, then what is at the end of it.
func (m *memoryUI) memHeaderView() string {
	s := m.styles
	left := s.header.Render("memory") + s.dim.Render(" · "+m.where)
	if a := m.agent(); a != "" {
		left += s.dim.Render(" · ") + s.accent.Render(a)
		if p := m.partition(); p != "" {
			left += s.dim.Render(" › ") + s.accent.Render(p)
		}
	}
	right := ""
	switch {
	case m.query != "":
		right = s.dim.Render(fmt.Sprintf("search %q", m.query))
	case m.partCur.at < len(m.parts):
		p := m.parts[m.partCur.at]
		right = s.dim.Render(fmt.Sprintf("%d notes · %d recall", p.Notes, p.Recall))
	}
	return memRow(m.width, left, right)
}

// memStatusView is the one line that answers back: what a request said, or
// which layer the memories are filtered to.
func (m *memoryUI) memStatusView() string {
	s := m.styles
	if m.status != "" {
		if m.isError {
			return truncate(s.errText.Render(m.status), m.width)
		}
		return truncate(s.dim.Render(m.status), m.width)
	}
	layer := "notes and recall"
	if m.layer != "" {
		layer = m.layer + " only"
	}
	return memRow(m.width, s.dim.Render(layer), s.dim.Render("? for keys"))
}

// memRow puts two pieces on one line, one at each margin.
func memRow(width int, left, right string) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		return truncate(left, width)
	}
	return left + strings.Repeat(" ", gap) + right
}

// memBodyView is the three columns, or whatever modal has taken them over.
func (m *memoryUI) memBodyView() string {
	switch {
	case m.showHelp:
		return m.memCentered(helpPanel(m.styles, m.help, m.keys))
	case m.confirm != nil:
		return m.memCentered(m.memConfirmView())
	case m.adding:
		return m.memCentered(m.memAddView())
	case m.showDet:
		return m.memCentered(m.memDetailView())
	}
	return lipgloss.JoinHorizontal(lipgloss.Top,
		paneBox(m.styles, m.agentsPaneView(), m.agentsW, m.bodyH, m.focus == paneAgents),
		paneBox(m.styles, m.partsPaneView(), m.partsW, m.bodyH, m.focus == paneParts),
		paneBox(m.styles, m.itemsPaneView(), m.itemsW, m.bodyH, m.focus == paneItems),
	)
}

func (m *memoryUI) memCentered(content string) string {
	return lipgloss.Place(m.width, m.bodyH, lipgloss.Center, lipgloss.Center, content)
}

// --- the columns ---------------------------------------------------------------------

func (m *memoryUI) agentsPaneView() string {
	w := max(m.agentsW-2, 4)
	rows := make([]string, 0, len(m.agents))
	for i, a := range m.agents {
		rows = append(rows, markRow(m.styles, truncate(a.Name, w-2), i == m.agentCur.at, m.focus == paneAgents))
	}
	return browsePane(m.styles, "agents", "", rows, m.agentCur, m.rowsPerPane(), false)
}

func (m *memoryUI) partsPaneView() string {
	w := max(m.partsW-2, 6)
	if m.loadParts {
		return browsePane(m.styles, "partitions", "", nil, m.partCur, m.rowsPerPane(), true)
	}
	rows := make([]string, 0, len(m.parts))
	for i, p := range m.parts {
		kind, label := splitPartition(p.Partition)
		if p.Name != nil && *p.Name != "" {
			label = *p.Name // the identity behind it; requests keep the raw partition
		}
		badge := ""
		if kind != "" {
			badge = m.styles.dim.Render(fmt.Sprintf("%-5s", kind)) + " "
		}
		tail := m.styles.dim.Render(fmt.Sprintf("%d/%d %s", p.Notes, p.Recall, memAge(derefTime(p.LastWritten))))
		room := max(w-2-lipgloss.Width(badge)-lipgloss.Width(tail)-1, 4)
		body := memRow(w-2, badge+truncate(label, room), tail)
		rows = append(rows, markRow(m.styles, body, i == m.partCur.at, m.focus == paneParts))
	}
	return browsePane(m.styles, "partitions", "", rows, m.partCur, m.rowsPerPane(), false)
}

func (m *memoryUI) itemsPaneView() string {
	w := max(m.itemsW-2, 8)
	head := ""
	if m.typing || m.query != "" {
		head = truncate(m.search.View(), w)
		if !m.typing {
			head = m.styles.dim.Render(truncate("/ "+m.query, w))
		}
	}
	if m.loadIts {
		return browsePane(m.styles, "memories", head, nil, m.itemCur, m.rowsPerPane(), true)
	}
	selected := m.partition()
	rows := make([]string, 0, len(m.items))
	for i, it := range m.items {
		rows = append(rows, markRow(m.styles, m.itemRowView(it, selected, w-2), i == m.itemCur.at, m.focus == paneItems))
	}
	foot := ""
	if len(m.items) >= memoryLimit {
		foot = m.styles.dim.Render(truncate("latest 100 · type / to search", w))
	}
	return browsePane(m.styles, "memories", head, rows, m.itemCur, m.rowsPerPane(), false) + foot
}

// itemRowView is one memory on one line: what layer it is, how old, whether
// it came from somewhere else, and as much of its first line as fits.
func (m *memoryUI) itemRowView(it gen.MemoryItem, selected string, w int) string {
	s := m.styles
	badge := s.accent.Render("note  ")
	if it.Layer == gen.MemoryItemLayer("recall") {
		badge = s.dim.Render("recall")
	}
	head := badge + " " + s.dim.Render(fmt.Sprintf("%4s", memAge(it.CreatedAt)))
	if it.Partition != selected {
		// The store hands back shared notes of other partitions too; saying
		// so is the difference between "the agent knows this here" and "the
		// agent knows this everywhere".
		head += " " + s.dim.Render("shared")
	}
	if m.query != "" && it.Score != nil {
		head += " " + s.dim.Render(fmt.Sprintf("%.2f", *it.Score))
	}
	room := max(w-lipgloss.Width(head)-1, 4)
	return head + " " + truncate(oneLine(it.Text), room)
}

// splitPartition takes the kind the daemon writes into a partition name apart
// from the rest of it, so a column can show `dm` as a badge and the
// conversation as the label.
func splitPartition(name string) (kind, label string) {
	head, rest, ok := strings.Cut(name, "/")
	if !ok {
		return "", name
	}
	switch head {
	case "dm", "room", "task":
		return head, rest
	}
	return "", name
}

// derefTime reads an optional timestamp; a partition nothing was ever
// written into has none.
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// memAge is how long ago something was written, short enough for a column.
func memAge(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// --- the modals -------------------------------------------------------------------

// memDetailView is the whole of one memory, which no column is wide enough
// for.
func (m *memoryUI) memDetailView() string {
	w := max(min(m.width-8, 76), 24)
	return m.styles.modal.Width(w).Render(
		m.styles.modalTitle.Render("memory") + "\n" + m.detail.View() + "\n" +
			m.styles.helpKey.Render("[esc] close"))
}

// memDetailText is what the panel holds: the text as it was written, then
// everything recorded about it.
func memDetailText(it gen.MemoryItem, s tuiStyles) string {
	var b strings.Builder
	b.WriteString(it.Text + "\n\n")
	kv := func(k, v string) {
		if v == "" {
			return
		}
		b.WriteString(s.detailKey.Render(fmt.Sprintf("%-11s", k)) + v + "\n")
	}
	kv("id", it.Id)
	kv("layer", string(it.Layer))
	kv("visibility", string(it.Visibility))
	kv("partition", it.Partition)
	kv("about", deref(it.About))
	kv("user", deref(it.UserId))
	kv("trace", deref(it.TraceId))
	kv("created", stamp(it.CreatedAt))
	return b.String()
}

// memConfirmView is the one question the browser asks: forgetting is the only
// thing here that cannot be undone.
func (m *memoryUI) memConfirmView() string {
	it := *m.confirm
	w := max(min(m.width-8, 64), 24)
	return m.styles.modal.Width(w).Render(
		m.styles.modalTitle.Render("forget "+it.Id+"? y/n") + "\n\n" +
			wrap(oneLine(it.Text), w-4) + "\n\n" +
			m.styles.helpKey.Render("[y] forget   [n] keep"))
}

// memAddView writes into the agent and partition already selected, which is
// why it asks for so little.
func (m *memoryUI) memAddView() string {
	s := m.styles
	w := max(min(m.width-8, 76), 24)
	field := func(label string, on bool) string {
		if on {
			return s.detailKey.Render("› " + label)
		}
		return s.dim.Render("  " + label)
	}
	vis := "origin — only this partition"
	if m.shared {
		vis = "shared — every partition of the agent"
	}
	var b strings.Builder
	b.WriteString(s.modalTitle.Render("add a note") + "\n")
	b.WriteString(s.dim.Render(m.agent()+" › "+m.partition()) + "\n\n")
	b.WriteString(field("text", m.addOn == addText) + "\n" + m.noteIn.View() + "\n\n")
	b.WriteString(field("about", m.addOn == addAbout) + "\n" + m.about.View() + "\n\n")
	b.WriteString(field("visibility", m.addOn == addVisibility) + "  " + s.dim.Render(vis) + "\n\n")
	b.WriteString(s.helpKey.Render("[tab] field   [space] toggle   [ctrl+s] save   [esc] cancel"))
	return s.modal.Width(w).Render(b.String())
}
