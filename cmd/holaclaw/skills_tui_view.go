package main

import (
	"fmt"
	"strconv"
	"strings"

	bkey "github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/lipgloss"

	"holaclaw/internal/api/gen"
)

// How the skills explorer is drawn: the memory browser's columns and modals,
// the trace browser's panel under them, in the chat's styles. A third screen
// with its own look would be a third program to learn.

// --- the keyboard --------------------------------------------------------------

// skKeyMap is the explorer's whole keyboard, in one place so the bar under it
// and the Update switch can never disagree. The motion keys are the other two
// browsers'; the letters are the actions docs/skills.md §7 names.
type skKeyMap struct {
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
	Revert   bkey.Binding
	Remove   bkey.Binding
	Restore  bkey.Binding
	Raw      bkey.Binding
	Reload   bkey.Binding
	Help     bkey.Binding
	Close    bkey.Binding
	Quit     bkey.Binding
}

func (k skKeyMap) ShortHelp() []bkey.Binding {
	return []bkey.Binding{k.Right, k.Down, k.Open, k.Filter, k.Remove, k.Restore, k.Help, k.Quit}
}

func (k skKeyMap) FullHelp() [][]bkey.Binding {
	return [][]bkey.Binding{
		{k.Left, k.Right, k.NextPane, k.PrevPane},
		{k.Up, k.Down, k.PageUp, k.PageDown, k.HalfUp, k.HalfDown, k.Top, k.Bottom},
		{k.Open, k.Filter, k.Revert, k.Raw},
		{k.Remove, k.Restore, k.Reload, k.Close, k.Quit},
	}
}

func newSkKeyMap() skKeyMap {
	return skKeyMap{
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
		Open:     bkey.NewBinding(bkey.WithKeys("enter", "o"), bkey.WithHelp("enter", "open")),
		Filter:   bkey.NewBinding(bkey.WithKeys("/"), bkey.WithHelp("/", "filter")),
		Revert:   bkey.NewBinding(bkey.WithKeys("v"), bkey.WithHelp("v", "revert to version")),
		Remove:   bkey.NewBinding(bkey.WithKeys("d"), bkey.WithHelp("d", "remove")),
		Restore:  bkey.NewBinding(bkey.WithKeys("r"), bkey.WithHelp("r", "restore")),
		Raw:      bkey.NewBinding(bkey.WithKeys("J"), bkey.WithHelp("J", "raw file")),
		Reload:   bkey.NewBinding(bkey.WithKeys("R"), bkey.WithHelp("R", "reload")),
		Help:     bkey.NewBinding(bkey.WithKeys("?"), bkey.WithHelp("?", "help")),
		Close:    bkey.NewBinding(bkey.WithKeys("esc"), bkey.WithHelp("esc", "back")),
		Quit:     bkey.NewBinding(bkey.WithKeys("q", "ctrl+c"), bkey.WithHelp("q", "quit")),
	}
}

// --- layout ----------------------------------------------------------------------

// The fixed rows: the header, the status line under it, and the help bar at
// the foot. Everything between them is the three columns, and the panel when
// one is open.
const (
	skHeaderRows = 2
	skHelpRows   = 1
	skChromeRows = skHeaderRows + skHelpRows

	// The row a column's first entry lands on: past the chrome, the pane's
	// top border and its heading.
	skRowTop = skHeaderRows + 2

	// How much of the body the panel takes when it is open; the columns keep
	// the rest, so the skill it is about stays in sight.
	skDetailShare = 2

	// What a modal spends on something other than its text: the border,
	// the padding, the title, the keys and the blank lines between.
	skModalChrome = 8
)

// layout sizes everything for the current window. It is the only place that
// decides how wide anything is, so a click and a row can never disagree.
func (m *skillsUI) layout() {
	m.width, m.height = max(m.width, minCols), max(m.height, minRows)
	body := m.fullBody()
	if m.showDet {
		m.detail.Height = max(body/skDetailShare-3, 3)
		m.bodyH = max(body-m.detail.Height-4, 4)
	} else {
		m.bodyH = body
	}

	// One agent is a label, not a choice, so the column shrinks to what it
	// takes to read the name and the other two get the space.
	agentsW := min(max(m.width/6, 16), 24)
	if len(m.agents) <= 1 {
		agentsW = 14
	}
	levelsW := min(max(m.width/4, 24), 36)
	if m.width-agentsW-levelsW < 30 {
		levelsW = max(m.width-agentsW-30, 18)
	}
	m.agentsW, m.levelsW = agentsW, levelsW
	m.skillW = max(m.width-agentsW-levelsW, 16)

	m.filter.Width = max(m.skillW-6, 8)
	m.detail.Width = max(m.width-4, 16)
	m.modal.Width = m.modalW() - 6
	m.fitModal()
}

// fullBody is the rows between the status line and the help bar, which a
// modal has to itself.
func (m *skillsUI) fullBody() int { return max(m.height-skChromeRows, 6) }

func (m *skillsUI) modalW() int { return max(min(m.width-8, 90), 24) }

// fitModal fills the modal's text and makes it as tall as that text, up to
// what the window has room for; a longer text scrolls.
func (m *skillsUI) fitModal() {
	if m.confirm == nil {
		return
	}
	text := fold(m.confirm.body, max(m.modal.Width, 10))
	m.modal.SetContent(text)
	m.modal.Height = min(max(strings.Count(text, "\n")+1, 1), max(m.fullBody()-skModalChrome, 3))
	m.modal.GotoTop()
}

// rowsFor is how many entries one column shows: the body, less its borders
// and heading, less the filter line over the skills.
func (m *skillsUI) rowsFor(p skPane) int {
	return max(m.bodyH-3-m.headRows(p), 1)
}

func (m *skillsUI) headRows(p skPane) int {
	if p == skPaneSkills && (m.typing || m.query != "") {
		return 1
	}
	return 0
}

// paneAt is which column a pointer column is over.
func (m *skillsUI) paneAt(x int) skPane {
	switch {
	case x < m.agentsW && len(m.agents) > 1:
		return skPaneAgents
	case x < m.agentsW+m.levelsW:
		return skPaneLevels
	}
	return skPaneSkills
}

// rowAt turns a screen row into an entry of one column, or says the pointer
// was not on one.
func (m *skillsUI) rowAt(p skPane, y int) (int, bool) {
	row := y - skRowTop - m.headRows(p)
	if row < 0 || row >= m.rowsFor(p) {
		return 0, false
	}
	i := m.cursorOf(p).off + row
	if i >= m.rows(p) {
		return 0, false
	}
	return i, true
}

// --- the view ----------------------------------------------------------------------

func (m *skillsUI) View() string {
	if m.quitting {
		return ""
	}
	parts := []string{m.skHeaderView(), m.skStatusView(), m.skBodyView()}
	if m.showDet && !m.overlaid() {
		parts = append(parts, m.skDetailView())
	}
	return strings.Join(append(parts, m.help.View(m.keys)), "\n")
}

// overlaid says a modal has the body, panel included.
func (m *skillsUI) overlaid() bool { return m.showHelp || m.confirm != nil }

// skHeaderView names where somebody is: the daemon, the agent, the level.
func (m *skillsUI) skHeaderView() string {
	s := m.styles
	left := s.header.Render("holaclaw") + s.dim.Render(" · skills · "+m.where)
	if a := m.agent(); a != "" {
		left += s.dim.Render(" · ") + s.accent.Render(a)
		if l, ok := m.level(); ok {
			left += s.dim.Render(" › ") + s.accent.Render(l.name)
		}
	}
	eligible := 0
	for _, sk := range m.shown {
		if sk.Eligible && !isTrashed(sk) {
			eligible++
		}
	}
	right := s.dim.Render(fmt.Sprintf("%d skills · %d eligible", len(m.shown), eligible))
	return memRow(m.width, left, right)
}

// skStatusView is the one line that answers back: what a request said, or
// where the selected skill lives and why the model does or does not see it.
func (m *skillsUI) skStatusView() string {
	s := m.styles
	if m.status != "" {
		if m.isError {
			return truncate(s.errText.Render(m.status), m.width)
		}
		return truncate(s.dim.Render(m.status), m.width)
	}
	right := s.dim.Render("? for keys")
	sk, ok := m.skill()
	if !ok {
		return memRow(m.width, "", right)
	}
	return memRow(m.width, s.dim.Render(truncate(skillStatus(m.agent(), sk), max(m.width-12, 8))), right)
}

// skillStatus is the status line for one skill: its place, then the one
// thing about it the index line cannot say — whether the model sees it, and
// the loader's reason when it does not — and who taught it.
func skillStatus(agent string, sk gen.Skill) string {
	line := agent + " / " + levelOf(sk) + " / " + sk.Name
	switch h := hiddenText(sk); {
	case isTrashed(sk):
		line += " · in the trash"
	case h != "":
		line += " · hidden: " + h
	case sk.Eligible:
		line += " · eligible"
	default:
		line += " · hidden"
	}
	if l := sk.Learned; l != nil {
		line += " · taught by " + oneLine(author(sk)) + " on " + l.On.Format("2006-01-02")
	}
	return line
}

// skBodyView is the three columns, or whatever modal has taken them over.
func (m *skillsUI) skBodyView() string {
	switch {
	case m.showHelp:
		return m.skCentered(helpPanel(m.styles, m.help, m.keys))
	case m.confirm != nil:
		return m.skCentered(m.skConfirmView())
	}
	return lipgloss.JoinHorizontal(lipgloss.Top,
		paneBox(m.styles, m.skAgentsPaneView(), m.agentsW, m.bodyH, m.focus == skPaneAgents),
		paneBox(m.styles, m.levelsPaneView(), m.levelsW, m.bodyH, m.focus == skPaneLevels),
		paneBox(m.styles, m.skillsPaneView(), m.skillW, m.bodyH, m.focus == skPaneSkills),
	)
}

// skCentered puts a modal in the middle of everything under the status line,
// the panel's rows included, so the screen keeps its height.
func (m *skillsUI) skCentered(content string) string {
	return lipgloss.Place(m.width, m.fullBody(), lipgloss.Center, lipgloss.Center, content)
}

// --- the columns ---------------------------------------------------------------------

func (m *skillsUI) skAgentsPaneView() string {
	w := max(m.agentsW-2, 4)
	rows := make([]string, 0, len(m.agents))
	for i, a := range m.agents {
		rows = append(rows, markRow(m.styles, truncate(a.Name, w-2), i == m.agentCur.at, m.focus == skPaneAgents))
	}
	return m.skPaneView(skPaneAgents, "agents", w, rows, m.agentCur, false)
}

func (m *skillsUI) levelsPaneView() string {
	w := max(m.levelsW-2, 6)
	if m.loadList {
		return m.skPaneView(skPaneLevels, "levels", w, nil, m.levelCur, true)
	}
	rows := make([]string, 0, len(m.levels))
	for i, l := range m.levels {
		rows = append(rows, markRow(m.styles, m.levelRowView(l, w-2), i == m.levelCur.at, m.focus == skPaneLevels))
	}
	return m.skPaneView(skPaneLevels, "levels", w, rows, m.levelCur, false)
}

// levelRowView is one level on one line: its name and how many skills it
// holds.
func (m *skillsUI) levelRowView(l skLevel, w int) string {
	return memRow(w, truncate(l.name, max(w-6, 4)), fmt.Sprintf("(%d)", l.n))
}

func (m *skillsUI) skillsPaneView() string {
	w := max(m.skillW-2, 8)
	if m.loadList {
		return m.skPaneView(skPaneSkills, "skills", w, nil, m.skillCur, true)
	}
	rows := make([]string, 0, len(m.shown))
	for i, sk := range m.shown {
		rows = append(rows, markRow(m.styles, m.skillRowView(sk, w-2), i == m.skillCur.at, m.focus == skPaneSkills))
	}
	return m.skPaneView(skPaneSkills, "skills", w, rows, m.skillCur, false)
}

// skillRowView is one skill as the model's index has it — `name:
// description`, the dash left off — behind a dot that says whether the model
// sees that line at all.
func (m *skillsUI) skillRowView(sk gen.Skill, w int) string {
	s := m.styles
	if isTrashed(sk) {
		tail := s.dim.Render("trash")
		line := truncate("○ "+sk.Name+": "+oneLine(sk.Description), max(w-6, 4))
		return memRow(w, s.dim.Render(line), tail)
	}
	dot := s.dim.Render("○")
	if sk.Eligible {
		dot = s.okText.Render("●")
	}
	name := truncate(sk.Name, max(w-4, 2))
	room := w - 2 - lipgloss.Width(name) - 2
	line := dot + " " + s.accent.Render(name)
	if room > 1 {
		line += ": " + truncate(oneLine(sk.Description), room)
	}
	return line
}

// skPaneView is a column: its heading, the filter box over the skills, and
// the rows that fit under it.
func (m *skillsUI) skPaneView(p skPane, title string, w int, rows []string, cur cursor, loading bool) string {
	head := ""
	if p == skPaneSkills {
		switch {
		case m.typing:
			head = truncate(m.filter.View(), w)
		case m.query != "":
			head = m.styles.dim.Render(truncate("/ "+m.query, w))
		}
	}
	return browsePane(m.styles, title, head, rows, cur, m.rowsFor(p), loading)
}

// --- the panel ------------------------------------------------------------------

// skDetailView is the whole of one skill, under the columns rather than over
// them: which skill it is stays on screen above it.
func (m *skillsUI) skDetailView() string {
	s := m.styles
	name := m.det.live.Name
	if name == "" {
		if sk, ok := m.skill(); ok {
			name = sk.Name
		}
	}
	switch {
	case m.det.old != nil:
		name += " @ version " + strconv.Itoa(deref(m.det.old.Version))
	case m.det.live.Version != nil:
		name += " · version " + strconv.Itoa(*m.det.live.Version)
	}
	title := s.paneTitle.Render(name)
	if m.det.raw {
		title += s.dim.Render(" · raw")
	}
	keys := s.helpKey.Render("[enter] version   [v] revert   [J] raw   [↑↓] walk   [esc] back")
	return paneBox(s, title+"\n"+m.detail.View()+"\n"+keys, m.width, m.detail.Height+4, true)
}

// skillFacts is the frontmatter as the loader parsed it, in the order a
// person asks about it: what it is, where, whether it is seen, what it needs,
// where it came from, what else is in the folder.
func skillFacts(s gen.Skill) [][2]string {
	var out [][2]string
	kv := func(k, v string) {
		if v != "" {
			out = append(out, [2]string{k, v})
		}
	}
	kv("name", s.Name)
	kv("description", s.Description)
	kv("level", string(s.Level))
	if isTrashed(s) {
		kv("trashed", "yes")
	}
	if s.Eligible {
		kv("eligible", "yes")
	} else {
		kv("eligible", "no")
	}
	kv("hidden", hiddenText(s))
	if r := s.Requires; r != nil {
		kv("requires tools", strings.Join(deref(r.Tools), ", "))
		kv("requires bins", strings.Join(deref(r.Bins), ", "))
	}
	if l := s.Learned; l != nil {
		kv("learned on", l.On.Format("2006-01-02"))
		kv("learned by", author(s))
		kv("learned from", l.From)
		kv("origin", string(l.Origin))
	}
	if s.References != nil {
		for i, r := range *s.References {
			k := ""
			if i == 0 {
				k = "references"
			}
			out = append(out, [2]string{k, r})
		}
	}
	kv("scripts", strings.Join(deref(s.Scripts), ", "))
	return out
}

// skFactW is the width of the key column of the facts.
const skFactW = 15

// skillDetailText is the panel's text and the lines of it the cursor can
// stand on: the facts and the body of the skill shown (or its raw file), then
// the live skill's versions. Every cursor line is one row by construction, so
// a target's line is its row in the viewport.
func skillDetailText(d skDetail, w int, s tuiStyles) (string, []skTarget) {
	if !d.loaded {
		return s.dim.Render("loading…"), nil
	}
	shown := d.live
	if d.old != nil {
		shown = *d.old
	}
	var lines []string
	add := func(text string) { lines = append(lines, strings.Split(text, "\n")...) }
	heading := func(h string) { lines = append(lines, "", s.detailKey.Render(h)) }

	if d.raw {
		raw := deref(shown.Raw)
		if raw == "" {
			raw = "the daemon sent no raw file"
		}
		add(fold(strings.TrimSuffix(raw, "\n"), w))
	} else {
		for _, f := range skillFacts(shown) {
			val := strings.Split(wrap(f[1], max(w-skFactW, 8)), "\n")
			for i, v := range val {
				k := ""
				if i == 0 {
					k = f[0]
				}
				lines = append(lines, s.detailKey.Render(fmt.Sprintf("%-*s", skFactW, k))+v)
			}
		}
		heading("body")
		add(fold(strings.TrimSuffix(deref(shown.Body), "\n"), w))
	}

	var targets []skTarget
	target := func(t skTarget, text string) {
		mark := "  "
		if len(targets) == d.cur {
			mark = s.accent.Render("›") + " "
		}
		t.line = len(lines)
		targets = append(targets, t)
		lines = append(lines, mark+truncate(text, max(w-2, 4)))
	}
	if d.live.Level == gen.SkillLevelAgent {
		heading("versions")
		if d.live.Versions == nil || len(*d.live.Versions) == 0 {
			lines = append(lines, s.dim.Render("none"))
		} else {
			for i := range *d.live.Versions {
				v := &(*d.live.Versions)[i]
				target(skTarget{version: v}, versionLine(*v))
			}
		}
	}
	return strings.Join(lines, "\n"), targets
}

// versionLine is one file of an agent skill's history, with who taught it
// and the trace that wrote it; a version a person wrote, or whose block does
// not parse, is its number alone.
func versionLine(v gen.SkillVersion) string {
	line := strconv.Itoa(v.Version)
	if v.On != nil {
		line += "  " + v.On.Format("2006-01-02")
	}
	if by := versionBy(v); by != "" {
		line += "  by " + oneLine(by)
	}
	if v.Trace != nil {
		line += "  trace " + *v.Trace
	}
	return line
}

// --- the modals -------------------------------------------------------------------

// skConfirmView is every question the explorer asks, with what it is about
// scrolling under the title.
func (m *skillsUI) skConfirmView() string {
	c := m.confirm
	keys := "[y] yes   [n] no   [↑↓] scroll"
	body := m.modal.View()
	return m.styles.modal.Width(m.modalW()).Render(
		m.styles.modalTitle.Render(truncate(c.title, m.modalW()-6)) + "\n\n" + body + "\n\n" +
			m.styles.helpKey.Render(keys))
}
