package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/help"
	bkey "github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"holaclaw/internal/api"
)

// What the three browsers — memory, trace and skills — draw and run the same
// way: the columns, their cursors, the help panel and the program around
// them. The layouts, keymaps and panels stay with each screen.

// cursor is one pane's selection and how far it has been scrolled. The panes
// are plain row lists rather than bubbles lists: a row here is one line by
// construction, which is what lets a click be turned straight back into the
// row somebody aimed at.
type cursor struct{ at, off int }

// move walks the cursor by d over n rows and keeps it inside the h rows on
// screen, so the pane follows it rather than the other way round.
func (c *cursor) move(d, n, h int) {
	if n == 0 {
		c.at, c.off = 0, 0
		return
	}
	c.at = min(max(c.at+d, 0), n-1)
	c.clamp(n, h)
}

// halfPage is how far ctrl+d and ctrl+u walk a pane h rows high: never
// nothing, or a short pane would not move at all.
func halfPage(h int) int { return max(h/2, 1) }

func (c *cursor) clamp(n, h int) {
	h = max(h, 1)
	c.at = min(max(c.at, 0), max(n-1, 0))
	c.off = min(max(c.off, c.at-h+1), c.at)
	c.off = min(max(c.off, 0), max(n-h, 0))
}

// The motion keys every pane shares, each spelt three ways: the dedicated
// key, vim's and emacs'. A keyboard without pgup still gets through a long
// list a screen at a time, and the arrows still walk it a row at a time.
var (
	motionUp       = bkey.NewBinding(bkey.WithKeys("up", "k", "ctrl+p"), bkey.WithHelp("↑/k/ctrl+p", "up"))
	motionDown     = bkey.NewBinding(bkey.WithKeys("down", "j", "ctrl+n"), bkey.WithHelp("↓/j/ctrl+n", "down"))
	motionPageUp   = bkey.NewBinding(bkey.WithKeys("pgup", "ctrl+b", "alt+v"), bkey.WithHelp("pgup/ctrl+b/alt+v", "page up"))
	motionPageDown = bkey.NewBinding(bkey.WithKeys("pgdown", "ctrl+f", "ctrl+v"), bkey.WithHelp("pgdn/ctrl+f/ctrl+v", "page down"))
	motionHalfUp   = bkey.NewBinding(bkey.WithKeys("ctrl+u"), bkey.WithHelp("ctrl+u", "half page up"))
	motionHalfDown = bkey.NewBinding(bkey.WithKeys("ctrl+d"), bkey.WithHelp("ctrl+d", "half page down"))
	motionTop      = bkey.NewBinding(bkey.WithKeys("g", "home", "alt+<"), bkey.WithHelp("g/alt+<", "first"))
	motionBottom   = bkey.NewBinding(bkey.WithKeys("G", "end", "alt+>"), bkey.WithHelp("G/alt+>", "last"))
)

// scrollKey moves a text panel on the shared motion keys, and reports
// whether msg was one. The viewport knows the dedicated keys but neither
// emacs' nor the two ends, so the panels ask here before handing it on.
func scrollKey(vp *viewport.Model, msg tea.KeyMsg) bool {
	switch {
	case bkey.Matches(msg, motionUp):
		vp.ScrollUp(1)
	case bkey.Matches(msg, motionDown):
		vp.ScrollDown(1)
	case bkey.Matches(msg, motionPageUp):
		vp.PageUp()
	case bkey.Matches(msg, motionPageDown):
		vp.PageDown()
	case bkey.Matches(msg, motionHalfUp):
		vp.HalfPageUp()
	case bkey.Matches(msg, motionHalfDown):
		vp.HalfPageDown()
	case bkey.Matches(msg, motionTop):
		vp.GotoTop()
	case bkey.Matches(msg, motionBottom):
		vp.GotoBottom()
	default:
		return false
	}
	return true
}

// browsePane is a column: its heading, head over the rows when there is one
// (a filter line, say), and the h rows from cur's scroll that fit under it.
func browsePane(s tuiStyles, title, head string, rows []string, cur cursor, h int, loading bool) string {
	var b strings.Builder
	b.WriteString(s.paneTitle.Render(title) + "\n")
	if head != "" {
		b.WriteString(head + "\n")
	}
	switch {
	case loading:
		b.WriteString(s.dim.Render(" loading…"))
	case len(rows) == 0:
		b.WriteString(s.dim.Render(" nothing here"))
	default:
		end := min(cur.off+h, len(rows))
		b.WriteString(strings.Join(rows[cur.off:end], "\n"))
	}
	b.WriteString("\n")
	return b.String()
}

// markRow marks the selected row. The mark is dimmed when the column does not
// have the keyboard, so three columns are never three cursors.
func markRow(s tuiStyles, body string, selected, focused bool) string {
	if !selected {
		return " " + body
	}
	if !focused {
		return s.dim.Render("›") + body
	}
	return s.accent.Render("›") + s.selected.Render(body)
}

// helpPanel is every key of keys in a modal, for `?`.
func helpPanel(s tuiStyles, h help.Model, keys help.KeyMap) string {
	h.ShowAll = true
	return s.modal.Render(s.modalTitle.Render("keys") + "\n" + h.View(keys))
}

// paneBox frames one pane at an exact size, so the panes beside each other
// always add up to the window. Every full-screen UI draws its columns with
// it, which is what keeps them looking like one program.
func paneBox(s tuiStyles, content string, w, h int, active bool) string {
	box := s.pane
	if active {
		box = s.paneActive
	}
	return box.Width(max(w-2, 1)).Height(max(h-2, 1)).MaxWidth(w).MaxHeight(h).Render(content)
}

// runFullScreen runs a browser on the whole terminal, mouse included, until
// it quits or ctx ends.
func runFullScreen(ctx context.Context, c *cli, model tea.Model) error {
	p := tea.NewProgram(model,
		tea.WithContext(ctx),
		tea.WithInput(c.stdin),
		tea.WithOutput(c.stdout),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	_, err := p.Run()
	return err
}

// browseDaemon is how every browser command ends. A browser needs a terminal
// at both ends, and without one need is said rather than a screen drawn that
// nobody can read; with one, run gets the daemon and where it is.
func browseDaemon(ctx context.Context, c *cli, cn *conn, need string, run func(client *api.Client, where string) error) error {
	if !isTerminal(c.stdout) || !isTerminalReader(c.stdin) {
		_, _ = fmt.Fprintln(c.stderr, "holaclaw: "+need)
		return exitError(2)
	}
	client, err := cn.dial(ctx)
	if err != nil {
		return err
	}
	where := *cn.addr
	if where == "" {
		where = "config socket"
	}
	return run(client, where)
}
