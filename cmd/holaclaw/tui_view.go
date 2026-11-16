package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"

	"holaclaw/internal/api/gen"
)

// How the full-screen chat is drawn. The palette is the terminal's own ANSI
// colours rather than hex, for the same reason term.go's is: whatever theme
// somebody has chosen, this follows it.

type tuiStyles struct {
	plain      lipgloss.Style
	dim        lipgloss.Style
	accent     lipgloss.Style
	user       lipgloss.Style
	errText    lipgloss.Style
	okText     lipgloss.Style
	pane       lipgloss.Style
	paneActive lipgloss.Style
	paneTitle  lipgloss.Style
	selected   lipgloss.Style
	detailKey  lipgloss.Style
	header     lipgloss.Style
	headerKey  lipgloss.Style
	helpKey    lipgloss.Style
	modal      lipgloss.Style
	modalTitle lipgloss.Style
}

func newTUIStyles() tuiStyles {
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	accent := lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	border := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8"))
	return tuiStyles{
		plain:      lipgloss.NewStyle(),
		dim:        dim,
		accent:     accent,
		user:       lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true),
		errText:    lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		okText:     lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		pane:       border,
		paneActive: border.BorderForeground(lipgloss.Color("6")),
		paneTitle:  lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true).Padding(0, 0, 0, 1),
		selected:   lipgloss.NewStyle().Bold(true),
		detailKey:  lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true),
		header:     lipgloss.NewStyle().Bold(true),
		headerKey:  dim,
		helpKey:    lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
		modal:      lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("3")).Padding(1, 2),
		modalTitle: lipgloss.NewStyle().Foreground(lipgloss.Color("3")).Bold(true),
	}
}

// eventColor is term.go's progress palette, in lipgloss terms, so a line in
// the activity pane is the colour it would have been on a plain terminal.
func (s tuiStyles) eventColor(ev gen.Event) lipgloss.TerminalColor {
	switch eventStyle(ev) {
	case styleGreen:
		return lipgloss.Color("2")
	case styleYellow:
		return lipgloss.Color("3")
	case styleRed:
		return lipgloss.Color("1")
	case styleCyan:
		return lipgloss.Color("6")
	}
	return lipgloss.Color("8")
}

// --- layout ------------------------------------------------------------------------

// The fixed rows around the body: the header, the status line, the prompt box
// (a three-line textarea inside a border), the context row and the help bar.
const (
	headerRows  = 2
	inputRows   = 5
	contextRows = 1
	helpRows    = 1
	chromeRows  = headerRows + inputRows + contextRows + helpRows

	// The smallest window the layout is drawn for. Below this it is drawn
	// for this size anyway and the terminal clips it, which is still better
	// than panes that disagree about where they are.
	minCols = 40
	minRows = 12

	// The column each scrollable pane keeps for its bar, and the row the
	// details pane keeps for its heading.
	scrollbarCols   = 1
	detailTitleRows = 1
)

// layout recomputes every pane for the current window. It is the only place
// that decides how wide anything is.
func (m *chatUI) layout() {
	// A size Bubble Tea could not measure — 0 rows or columns, which some
	// terminals report before they settle — is normalised rather than
	// skipped. Leaving early would keep the panes at the last size while the
	// window is drawn at the new one, so they would be drawn to one width and
	// clicked at another.
	m.width, m.height = max(m.width, minCols), max(m.height, minRows)

	bodyH := max(m.height-chromeRows, 3)
	actsW := 0
	if m.showActs {
		actsW = min(max(m.width/3, 32), 52)
		if m.width-actsW < 40 {
			// A narrow window is the transcript's before it is anything else.
			actsW = 0
		}
	}
	bodyW := m.width - actsW

	m.body.Width, m.body.Height = max(bodyW-2, 10), max(bodyH-2, 1)
	m.input.SetWidth(max(m.width-4, 10))

	m.actsW, m.bodyH = actsW, bodyH
	if actsW > 0 {
		actsH := m.actsHeight()
		// A column down the right edge of each pane is the scrollbar's, and
		// the details pane spends a row of its own on its heading. Leaving
		// either out of the arithmetic is what pushes a bottom border off the
		// box.
		inner := max(actsW-2-scrollbarCols, 10)
		m.acts.SetSize(inner, max(actsH-2, 1))
		m.acts.SetDelegate(activityDelegate{styles: m.styles, width: inner})
		m.detail.Width, m.detail.Height = inner, max(bodyH-actsH-2-detailTitleRows, 1)
		// Where the first activity row lands on screen, so a click can be
		// turned back into the entry somebody aimed at.
		m.actsTop = headerRows + 1 + lipgloss.Height(m.acts.Styles.TitleBar.Render(""))
	}

	m.pal.SetSize(max(min(m.width-8, 60)-2, 10), max(bodyH-2, 1))
	m.bar.Width = max(min(m.width-8, 60)-6, 10)

	if r, err := glamour.NewTermRenderer(
		glamour.WithStyles(m.mdStyle),
		glamour.WithWordWrap(m.body.Width),
	); err == nil {
		m.md = r
	}
	m.rerender()
}

// settleTerminal answers, once and *before* the program takes the terminal
// over, the one question the drawing code would otherwise ask it mid-session:
// is the background dark? Both lipgloss (for its adaptive colours) and
// glamour (for its "auto" style) ask by writing an escape sequence and reading
// the answer back. Asked once Bubble Tea is reading keys, that answer is
// swallowed by Bubble Tea instead — so the ask waits out its whole timeout,
// which is the pause before the chat appears, and whatever of the reply
// Bubble Tea did read is typed into the prompt as a line of escape codes.
//
// It returns the style a reply is rendered with. The margin and the blank
// line glamour wraps a document in are dropped: the transcript already puts
// one line between blocks, and two read as a gap somebody left on purpose.
func settleTerminal(w io.Writer) ansi.StyleConfig {
	cfg := styles.NoTTYStyleConfig
	if isTerminal(w) {
		// One ask, through lipgloss, which caches the answer on the
		// renderer everything else here draws with.
		dark := lipgloss.HasDarkBackground()
		cfg = styles.LightStyleConfig
		if dark {
			cfg = styles.DarkStyleConfig
		}
	}
	zero := uint(0)
	cfg.Document.Margin = &zero
	cfg.Document.BlockPrefix, cfg.Document.BlockSuffix = "", ""
	return cfg
}

// actsLeft is the first column of the activity pane.
func (m *chatUI) actsLeft() int { return m.width - m.actsW }

// actsRowAt turns a screen row into an entry of the activity pane.
func (m *chatUI) actsRowAt(y int) (int, bool) {
	if m.actsW == 0 {
		return 0, false
	}
	row := y - m.actsTop
	if row < 0 {
		return 0, false
	}
	start, end := m.acts.Paginator.GetSliceBounds(len(m.acts.VisibleItems()))
	i := start + row
	if i >= end {
		return 0, false
	}
	return i, true
}

// --- the transcript ------------------------------------------------------------------

// renderMessage lays one block out for the current width. An agent's reply is
// markdown unless somebody asked for it raw; everything else is one styled
// paragraph. A block never carries its own blank line: refreshBody is what
// puts exactly one between two of them.
func (m *chatUI) renderMessage(msg uiMessage) string {
	w := max(m.body.Width, 10)
	switch msg.role {
	case roleUser:
		out := m.styles.user.Render("› " + wrap(msg.text, w-2))
		if len(msg.reactions) > 0 {
			out += "\n  " + strings.Join(msg.reactions, " ")
		}
		return out
	case roleSystem:
		return m.styles.dim.Render("· " + wrap(msg.text, w-2))
	case roleError:
		return m.styles.errText.Render("! " + wrap(msg.text, w-2))
	}
	if m.raw || m.md == nil {
		return wrap(msg.text, w)
	}
	out, err := m.md.Render(msg.text)
	if err != nil {
		return wrap(msg.text, w)
	}
	return strings.Trim(out, "\n")
}

// rerender lays the whole transcript out again, after a resize or a switch
// between markdown and raw.
func (m *chatUI) rerender() {
	m.rendered = m.rendered[:0]
	for _, msg := range m.msgs {
		m.rendered = append(m.rendered, m.renderMessage(msg))
	}
	m.refreshBody()
}

// refreshBody puts the transcript — plus whatever of a reply has streamed in
// so far — into the viewport, following the bottom unless somebody scrolled
// away from it.
func (m *chatUI) refreshBody() {
	atBottom := m.body.AtBottom()
	blocks := m.rendered
	if s := m.stream.String(); s != "" {
		// A reply is laid out raw while it arrives: re-rendering markdown on
		// every delta would cost more than it shows.
		blocks = append(append([]string{}, blocks...), wrap(s, max(m.body.Width, 10)))
	}
	m.body.SetContent(strings.Join(blocks, "\n\n"))
	if atBottom || m.busy {
		m.body.GotoBottom()
	}
}

// refreshDetail fills the panel from whatever the activity pane has selected.
func (m *chatUI) refreshDetail() {
	a, ok := m.acts.SelectedItem().(activity)
	if !ok {
		m.detail.SetContent("")
		return
	}
	var out []toolOutput
	if wantsToolOutput(a.ev) {
		out = outputsFor(a.ev, m.outputs[a.ev.TraceId])
	}
	m.detail.SetContent(fold(detailText(a, out, m.styles), max(m.detail.Width, 10)))
	m.detail.GotoTop()
}

// --- the view -------------------------------------------------------------------------

func (m *chatUI) View() string {
	if m.quitting {
		return ""
	}
	return strings.Join([]string{
		m.headerView(),
		m.statusView(),
		m.bodyView(),
		m.inputView(),
		m.contextView(),
		m.helpView(),
	}, "\n")
}

// headerView names the chat on the left — who is being talked to — and on the
// right puts the mascot: a face that moves on every tick, so a glance at the
// top of the screen says the chat is alive before the word beside it says
// what it is doing.
func (m *chatUI) headerView() string {
	left := m.styles.header.Render("holaclaw") + m.styles.dim.Render(" · ") +
		m.styles.accent.Render(m.info.agent) + m.styles.dim.Render(" · "+m.info.conversation)
	return m.rowView(left, m.mascotView())
}

// statusView is the second header row: where the turns run and on what model,
// with the clock, then what the turn has cost so far.
func (m *chatUI) statusView() string {
	where := "local"
	if m.info.attached {
		where = "daemon"
	}
	left := where
	if model := m.model(); model != "" {
		left = model + " · " + where
	}
	if m.busy || m.elapsed > 0 {
		left += fmt.Sprintf(" · %.1fs", m.elapsed.Seconds())
	}
	trace := ""
	if m.showTrace {
		trace = m.lastTrace
	}
	right := m.styles.dim.Render(strings.TrimPrefix(infoLine(m.stats, m.elapsed, trace), "· "))
	if !m.busy && m.stats.model == "" {
		right = ""
	}
	return m.rowView(m.styles.dim.Render(left), right)
}

// uiState is what the header says the chat is doing. The two states nobody
// has to infer — an approval on screen, an abort asked for — are read off the
// model here rather than tracked twice; the rest is whatever the events said.
func (m *chatUI) uiState() uiState {
	switch {
	case m.approval != nil:
		return stateWaiting
	case m.aborting:
		return stateAborting
	case !m.busy:
		return stateIdle
	}
	if m.state == stateIdle {
		// Busy with nothing traced yet: a model is being asked.
		return stateThinking
	}
	return m.state
}

// mascotView is the face and its word, in the colour of the state.
func (m *chatUI) mascotView() string {
	st := m.uiState()
	return m.styles.dim.Render(mascotFrame(st, m.frame)) + " " + m.stateStyle(st).Render(st.word())
}

// stateStyle is the palette of the states: the same colours the activity pane
// gives the events they come from.
func (m *chatUI) stateStyle(st uiState) lipgloss.Style {
	switch st {
	case stateThinking:
		return m.styles.accent
	case stateWorking:
		return m.styles.okText
	case stateWaiting:
		return m.styles.modalTitle
	case stateAborting:
		return m.styles.errText
	}
	return m.styles.dim
}

// rowView puts two pieces on one line, one at each margin. In a narrow window
// the left gives way: the right of these rows is the state and what the turn
// is costing, which is the half somebody is watching change.
func (m *chatUI) rowView(left, right string) string {
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap >= 1 {
		return left + strings.Repeat(" ", gap) + right
	}
	room := m.width - lipgloss.Width(right) - 1
	if room < 4 {
		return clipStyled(right, m.width)
	}
	return clipStyled(left, room) + " " + right
}

// clipStyled cuts a styled string to a width. lipgloss does the counting, because
// the escape sequences in it are not columns and must not be cut through.
func clipStyled(s string, w int) string {
	return lipgloss.NewStyle().MaxWidth(max(w, 0)).Render(s)
}

// bodyView is the middle of the screen: the transcript, the panes beside it,
// or whatever modal has taken it over.
func (m *chatUI) bodyView() string {
	switch {
	case m.approval != nil:
		return m.centered(m.approvalView())
	case m.showPal:
		return m.centered(m.paletteView())
	case m.showHelp:
		return m.centered(m.helpPanelView())
	}
	transcript := m.paneBox(m.body.View(), m.width-m.actsW, m.bodyH, m.focus == focusTranscript)
	if m.actsW == 0 {
		return transcript
	}
	actsH := m.actsHeight()
	col := m.paneBox(m.actsView(), m.actsW, actsH, m.focus == focusActivity)
	if m.showDetail {
		col = lipgloss.JoinVertical(lipgloss.Left, col,
			m.paneBox(m.detailView(), m.actsW, m.bodyH-actsH, m.focus == focusDetail))
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, transcript, col)
}

// actsView is the activity list with its bar beside it. The bar starts below
// the pane's heading, so it tracks the rows rather than the whole box.
func (m *chatUI) actsView() string {
	above := max(m.actsTop-headerRows-1, 0)
	total := len(m.acts.VisibleItems())
	bar := m.styles.scrollbar(m.acts.Paginator.PerPage, total, m.acts.Paginator.PerPage,
		ratio(m.acts.Index(), total-1))
	if bar != "" {
		bar = strings.Repeat("\n", above) + bar
	}
	// The list does not pad its rows out to its width, so without this the
	// bar would sit against the longest entry instead of the pane edge.
	body := lipgloss.NewStyle().Width(m.acts.Width()).Render(m.acts.View())
	return lipgloss.JoinHorizontal(lipgloss.Top, body, bar)
}

// detailView is the heading — with the close box at its right end — then the
// panel and its bar.
func (m *chatUI) detailView() string {
	bar := m.styles.scrollbar(m.detail.Height, m.detail.TotalLineCount(), m.detail.Height,
		ratio(m.detail.YOffset, m.detail.TotalLineCount()-m.detail.Height))
	body := lipgloss.JoinHorizontal(lipgloss.Top, m.detail.View(), bar)
	title := m.styles.paneTitle.Render("details")
	shut := m.styles.dim.Render(detailCloseMark)
	if m.focus == focusDetail {
		shut = m.styles.accent.Render(detailCloseMark)
	}
	gap := max(m.detail.Width+scrollbarCols-lipgloss.Width(title)-1, 1)
	return title + strings.Repeat(" ", gap) + shut + "\n" + body
}

// detailCloseMark is the close box of the details panel, and detailCloseAt
// says where on screen it is: the last column inside the panel's border, on
// the heading row.
const detailCloseMark = "✕"

func (m *chatUI) detailCloseAt(x, y int) bool {
	return m.showDetail &&
		y == headerRows+m.actsHeight()+detailTitleRows &&
		x >= m.width-2-scrollbarCols
}

// closeDetail shuts the panel and leaves the focus on the list it belongs to.
func (m *chatUI) closeDetail() {
	m.showDetail = false
	if m.focus == focusDetail {
		m.focus = focusActivity
	}
	m.layout()
}

// scrollbar is the one-column track down a pane's right edge: h rows of it,
// with a thumb covering the share of `total` rows that the `window` on screen
// is, sitting `at` of the way down — 0 at the top, 1 at the bottom. It is
// blank when everything already fits: a bar that cannot move says nothing,
// and a blank column keeps the pane the same width either way.
//
// The position comes in as a fraction because the two panes measure it
// differently — a viewport by the row it is scrolled to, a list by the entry
// the cursor is on, since a list moves a page at a time and a bar that only
// twitched once a page looks broken.
func (s tuiStyles) scrollbar(h, total, window int, at float64) string {
	if h <= 0 {
		return ""
	}
	if total <= window || total <= 0 {
		return strings.TrimSuffix(strings.Repeat(" \n", h), "\n")
	}
	size := min(max(h*window/total, 1), h)
	span := h - size
	top := min(max(int(at*float64(span)+0.5), 0), span)
	rows := make([]string, h)
	for i := range rows {
		if i >= top && i < top+size {
			rows[i] = s.accent.Render("┃")
			continue
		}
		rows[i] = s.dim.Render("│")
	}
	return strings.Join(rows, "\n")
}

// ratio is n of the way through den, as a fraction, with an empty range
// reading as the top.
func ratio(n, den int) float64 {
	if den <= 0 {
		return 0
	}
	return float64(n) / float64(den)
}

// paneBox frames one pane at an exact size. The arithmetic itself lives in
// tui_browse.go, where the browsers draw their columns with it: two programs
// that frame a pane differently look like two programs.
func (m *chatUI) paneBox(content string, w, h int, active bool) string {
	return paneBox(m.styles, content, w, h, active)
}

// centered puts a modal in the middle of the body area.
func (m *chatUI) centered(content string) string {
	return lipgloss.Place(m.width, m.bodyH, lipgloss.Center, lipgloss.Center, content)
}

// inputView is the prompt box. It carries the focus hint, because the one
// thing somebody needs to know at a glance is whether typing goes here.
func (m *chatUI) inputView() string {
	return m.inputStyle().Width(max(m.width-2, 1)).Render(m.input.View())
}

// inputStyle is the frame around the prompt: the active border is the whole
// of that hint, which is why it is its own function — a test can ask which
// frame the box is wearing without a terminal that shows colour.
func (m *chatUI) inputStyle() lipgloss.Style {
	if m.focus == focusInput {
		return m.styles.paneActive
	}
	return m.styles.pane
}

func (m *chatUI) helpView() string {
	return m.help.View(m.keys)
}

// helpPanelView is the full keyboard, as f1 and /help show it.
func (m *chatUI) helpPanelView() string {
	m.help.ShowAll = true
	body := m.help.View(m.keys)
	m.help.ShowAll = false
	var cmds []string
	for _, c := range tuiCommands {
		cmds = append(cmds, m.styles.helpKey.Render(c.Title())+m.styles.dim.Render("  "+c.desc))
	}
	return m.styles.modal.Render(
		m.styles.modalTitle.Render("keys") + "\n" + body + "\n\n" +
			m.styles.modalTitle.Render("commands") + "\n" + strings.Join(cmds, "\n"))
}

func (m *chatUI) paletteView() string {
	return m.styles.modal.Render(m.pal.View())
}

// approvalView is the modal a turn stops on: what is being asked for, the
// arguments in full, and how long there is to answer.
func (m *chatUI) approvalView() string {
	a := *m.approval
	w := max(min(m.width-8, 76), 20)
	var b strings.Builder
	b.WriteString(m.styles.modalTitle.Render("approval") + "\n\n")
	b.WriteString(m.styles.header.Render(wrap(a.summary, w)) + "\n")
	if a.tool != "" {
		b.WriteString(m.styles.dim.Render("tool "+a.tool) + "\n")
	}
	if args := prettyJSON(a.args); args != "" {
		b.WriteString("\n" + m.styles.dim.Render(fold(args, w)) + "\n")
	}
	if !a.expires.IsZero() {
		left := time.Until(a.expires)
		total := a.expires.Sub(m.approvalSeen)
		if left > 0 && total > 0 {
			b.WriteString("\n" + m.bar.ViewAs(left.Seconds()/total.Seconds()) +
				m.styles.dim.Render(fmt.Sprintf(" %.0fs left", left.Seconds())) + "\n")
		}
	}
	keys := "[y] once   [s] session   [a] always   [n] no"
	if a.local {
		// Nothing is persisted in the in-process mode, so there is no always.
		b.WriteString("\n" + m.styles.dim.Render("this choice lasts for this run only — nothing is persisted") + "\n")
		keys = "[y] once   [s] session   [n] no"
	}
	b.WriteString("\n" + m.styles.helpKey.Render(keys))
	return m.styles.modal.Width(w).Render(b.String())
}

// --- text -------------------------------------------------------------------------

// wrap folds one paragraph to a width, leaving existing line breaks alone.
func wrap(s string, w int) string {
	if w < 4 {
		return s
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		out = append(out, wrapLine(line, w))
	}
	return strings.Join(out, "\n")
}

// fold breaks text that is already laid out — a JSON dump, a tool response —
// to a width. Nothing is cut: the panel is where the whole of a thing is read,
// so a long line is carried on to the next row instead. A carried row keeps
// the indent of the line it came from, which is what holds a dump's shape
// together.
//
// A line carrying an escape sequence is left alone: folding one through the
// middle would paint the rest of the pane. detailText only styles lines that
// are short by construction, so none of those needs folding anyway.
func fold(s string, w int) string {
	if w < 8 {
		return s
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		out = append(out, foldLine(line, w)...)
	}
	return strings.Join(out, "\n")
}

func foldLine(line string, w int) []string {
	r := []rune(line)
	if len(r) <= w || strings.ContainsRune(line, 0x1b) {
		return []string{line}
	}
	indent := ""
	for _, c := range r {
		if c != ' ' && c != '\t' {
			break
		}
		indent += string(c)
	}
	if len([]rune(indent)) > w/2 {
		indent = ""
	}
	var out []string
	for first := true; len(r) > 0; first = false {
		prefix, room := "", w
		if !first {
			prefix, room = indent, max(w-len([]rune(indent)), 1)
		}
		n := min(room, len(r))
		out = append(out, prefix+string(r[:n]))
		r = r[n:]
	}
	return out
}

// wrapLine breaks one line on spaces, keeping a word whole unless it is
// longer than the whole width.
func wrapLine(line string, w int) string {
	words := strings.Fields(line)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	col := 0
	for i, word := range words {
		n := len([]rune(word))
		switch {
		case i == 0:
		case col+1+n <= w:
			b.WriteString(" ")
			col++
		default:
			b.WriteString("\n")
			col = 0
		}
		b.WriteString(word)
		col += n
	}
	return b.String()
}

// prettyJSON indents a tool call's arguments, or hands back whatever it was
// given when that is not JSON at all.
func prettyJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return raw
	}
	return string(b)
}
