package main

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/help"
	bkey "github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
)

// The memory browser: `holaclaw memory` with nothing after it. Everything the
// subcommands make you spell out — which agent, which partition — is a column
// here instead, because nobody remembers that a conversation's memory lives
// under `dm/U08F3…`. Three panes, left to right: the agents, the partitions of
// the one selected, and what is in the one selected. Picking moves right;
// nothing is ever typed that could have been picked.
//
// The subcommands are untouched: a script still calls `memory search` with
// both flags, and this is only the way a person reads the same API.

// memoryLimit is the spec's maximum for one memory search. The browser always
// asks for it: a pane that could show more than it was given would be lying
// about what the agent remembers.
const memoryLimit = 100

// memoryAPI is the slice of the daemon this browser needs. It is an interface
// so the model can be driven in a test without a server: every call below is
// one request, and nothing else reaches the network from here.
type memoryAPI interface {
	ListAgents(ctx context.Context) ([]gen.AgentInfo, error)
	ListMemoryPartitions(ctx context.Context, agent string) ([]gen.MemoryPartition, error)
	SearchMemory(ctx context.Context, params gen.SearchMemoryParams) ([]gen.MemoryItem, error)
	WriteMemory(ctx context.Context, in gen.MemoryWrite) (gen.MemoryItem, error)
	ForgetMemory(ctx context.Context, id, agent, partition string) error
}

// memPane is which column the keyboard is talking to.
type memPane int

const (
	paneAgents memPane = iota
	paneParts
	paneItems
)

// addField is which line of the add-note modal has the cursor.
type addField int

const (
	addText addField = iota
	addAbout
	addVisibility
)

// --- what a request answers with ------------------------------------------------

// Every list request carries the sequence number it was issued under, and an
// answer whose number is behind the model's is dropped. Moving the cursor
// down a long partition list fires a request per row; without this a slow
// early answer would land last and leave the pane showing somebody else's
// memories.
type (
	memAgentsMsg struct {
		agents []gen.AgentInfo
		err    error
	}
	memPartsMsg struct {
		seq   int
		parts []gen.MemoryPartition
		err   error
	}
	memItemsMsg struct {
		seq   int
		items []gen.MemoryItem
		err   error
	}
	memSavedMsg struct {
		id  string
		err error
	}
	memForgotMsg struct {
		id  string
		err error
	}
)

// --- the model ----------------------------------------------------------------------

type memoryUI struct {
	ctx   context.Context
	api   memoryAPI
	where string // the daemon address, for the header

	keys   memKeyMap
	styles tuiStyles
	help   help.Model
	search textinput.Model
	detail viewport.Model
	noteIn textarea.Model
	about  textinput.Model

	agents []gen.AgentInfo
	parts  []gen.MemoryPartition
	items  []gen.MemoryItem

	agentCur, partCur, itemCur cursor

	partSeq, itemSeq   int
	loadParts, loadIts bool

	focus    memPane
	layer    string // "", "note" or "recall"
	query    string // the search that produced the current list, "" for the recent one
	typing   bool   // the search box has the keyboard
	showHelp bool
	showDet  bool
	confirm  *gen.MemoryItem // the forget modal, or nil
	adding   bool
	addOn    addField
	shared   bool

	status  string
	isError bool

	width, height int
	bodyH         int
	agentsW       int
	partsW        int
	itemsW        int
	quitting      bool
}

// runMemoryTUI is the whole browser.
func runMemoryTUI(ctx context.Context, c *cli, mapi memoryAPI, where string) error {
	return runFullScreen(ctx, c, newMemoryUI(ctx, mapi, where))
}

func newMemoryUI(ctx context.Context, mapi memoryAPI, where string) *memoryUI {
	st := newTUIStyles()

	q := textinput.New()
	q.Prompt = "/ "
	q.Placeholder = "search this partition"

	note := textarea.New()
	note.Placeholder = "what should the agent remember?"
	note.ShowLineNumbers = false
	note.SetHeight(4)
	note.Prompt = ""

	about := textinput.New()
	about.Prompt = ""
	about.Placeholder = "who it is about (optional)"

	h := help.New()
	h.Styles.ShortKey, h.Styles.FullKey = st.helpKey, st.helpKey
	h.Styles.ShortDesc, h.Styles.FullDesc = st.dim, st.dim

	m := &memoryUI{
		ctx: ctx, api: mapi, where: where,
		keys: newMemKeyMap(), styles: st, help: h,
		search: q, noteIn: note, about: about,
	}
	// A size arrives a frame later; until then the panes are laid out for a
	// plausible terminal so the first frame is not drawn at zero columns.
	m.width, m.height = 100, 30
	m.layout()
	return m
}

func (m *memoryUI) Init() tea.Cmd { return m.loadAgents() }

// --- requests -------------------------------------------------------------------

func (m *memoryUI) loadAgents() tea.Cmd {
	ctx, mapi := m.ctx, m.api
	return func() tea.Msg {
		agents, err := mapi.ListAgents(ctx)
		return memAgentsMsg{agents: agents, err: err}
	}
}

// loadPartitions asks for the selected agent's partitions under a fresh
// sequence number, which is what makes every earlier answer stale.
func (m *memoryUI) loadPartitions() tea.Cmd {
	agent := m.agent()
	if agent == "" {
		return nil
	}
	m.partSeq++
	m.loadParts = true
	seq, ctx, mapi := m.partSeq, m.ctx, m.api
	return func() tea.Msg {
		parts, err := mapi.ListMemoryPartitions(ctx, agent)
		return memPartsMsg{seq: seq, parts: parts, err: err}
	}
}

func (m *memoryUI) loadItems() tea.Cmd {
	agent, partition := m.agent(), m.partition()
	if agent == "" || partition == "" {
		m.items, m.itemCur = nil, cursor{}
		return nil
	}
	m.itemSeq++
	m.loadIts = true
	params := gen.SearchMemoryParams{Agent: agent, Partition: partition, Limit: ptrOf(memoryLimit)}
	if m.query != "" {
		params.Q = ptrOf(m.query)
	}
	if m.layer != "" {
		params.Layer = ptrOf(gen.SearchMemoryParamsLayer(m.layer))
	}
	seq, ctx, mapi := m.itemSeq, m.ctx, m.api
	return func() tea.Msg {
		items, err := mapi.SearchMemory(ctx, params)
		return memItemsMsg{seq: seq, items: items, err: err}
	}
}

// reload is `R`: everything again, from the agent list down.
func (m *memoryUI) reload() tea.Cmd { return m.loadAgents() }

// --- update ---------------------------------------------------------------------

func (m *memoryUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case tea.KeyMsg:
		return m, m.onKey(msg)

	case tea.MouseMsg:
		return m, m.onMouse(msg)

	case memAgentsMsg:
		return m, m.onAgents(msg)

	case memPartsMsg:
		return m, m.onParts(msg)

	case memItemsMsg:
		return m, m.onItems(msg)

	case memSavedMsg:
		if msg.err != nil {
			m.fail("save", msg.err)
			return m, nil
		}
		m.say("saved " + msg.id)
		return m, m.loadItems()

	case memForgotMsg:
		if msg.err != nil {
			m.fail("forget", msg.err)
			return m, nil
		}
		m.say("forgot " + msg.id)
		return m, m.loadItems()
	}
	return m, nil
}

func (m *memoryUI) onAgents(msg memAgentsMsg) tea.Cmd {
	if msg.err != nil {
		m.fail("agents", msg.err)
		return nil
	}
	m.agents = msg.agents
	sort.Slice(m.agents, func(i, j int) bool { return m.agents[i].Name < m.agents[j].Name })
	m.agentCur.clamp(len(m.agents), m.rowsPerPane())
	// One agent is the common case, and there is nothing to choose there: the
	// column shrinks to a label and the keyboard starts where the choosing
	// actually happens.
	if len(m.agents) == 1 && m.focus == paneAgents {
		m.focus = paneParts
	}
	m.layout()
	return m.loadPartitions()
}

func (m *memoryUI) onParts(msg memPartsMsg) tea.Cmd {
	if msg.seq != m.partSeq {
		return nil
	}
	m.loadParts = false
	if msg.err != nil {
		m.parts, m.items = nil, nil
		m.failPartitions(msg.err)
		return nil
	}
	m.parts = msg.parts
	m.partCur = cursor{}
	m.clear()
	return m.loadItems()
}

func (m *memoryUI) onItems(msg memItemsMsg) tea.Cmd {
	if msg.seq != m.itemSeq {
		return nil
	}
	m.loadIts = false
	if msg.err != nil {
		m.items = nil
		m.fail("memories", msg.err)
		return nil
	}
	m.items = msg.items
	m.itemCur = cursor{}
	return nil
}

// failPartitions says the one thing a plain error would hide: a daemon that
// predates this endpoint, or one running without a memory backend, answers
// not_implemented — and then there is nothing to browse and nothing broken.
func (m *memoryUI) failPartitions(err error) {
	var ce *api.ClientError
	if errors.As(err, &ce) && ce.Status == http.StatusNotImplemented {
		m.status, m.isError = "this daemon cannot list partitions — use `memory search --partition …`", true
		return
	}
	m.fail("partitions", err)
}

func (m *memoryUI) fail(what string, err error) {
	m.status, m.isError = what+": "+err.Error(), true
}

func (m *memoryUI) say(s string) { m.status, m.isError = s, false }

func (m *memoryUI) clear() { m.status, m.isError = "", false }

// --- selection --------------------------------------------------------------------

func (m *memoryUI) agent() string {
	if m.agentCur.at >= len(m.agents) {
		return ""
	}
	return m.agents[m.agentCur.at].Name
}

func (m *memoryUI) partition() string {
	if m.partCur.at >= len(m.parts) {
		return ""
	}
	return m.parts[m.partCur.at].Partition
}

func (m *memoryUI) item() (gen.MemoryItem, bool) {
	if m.itemCur.at >= len(m.items) {
		return gen.MemoryItem{}, false
	}
	return m.items[m.itemCur.at], true
}

// rows is how many entries the focused pane holds, and cursorOf which cursor
// walks them, so one set of motion keys drives all three.
func (m *memoryUI) rows(p memPane) int {
	switch p {
	case paneAgents:
		return len(m.agents)
	case paneParts:
		return len(m.parts)
	}
	return len(m.items)
}

func (m *memoryUI) cursorOf(p memPane) *cursor {
	switch p {
	case paneAgents:
		return &m.agentCur
	case paneParts:
		return &m.partCur
	}
	return &m.itemCur
}

// --- keys ---------------------------------------------------------------------------

func (m *memoryUI) onKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case m.confirm != nil:
		return m.confirmKey(msg)
	case m.adding:
		return m.addKey(msg)
	case m.showDet:
		return m.detailKey(msg)
	case m.typing:
		return m.searchKey(msg)
	case m.showHelp:
		// The help is a reference, not a mode: anything shuts it.
		m.showHelp = false
		return nil
	}
	return m.browseKey(msg)
}

func (m *memoryUI) browseKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case bkey.Matches(msg, m.keys.Quit):
		m.quitting = true
		return tea.Quit
	case bkey.Matches(msg, m.keys.Help):
		m.showHelp = true
		return nil
	case bkey.Matches(msg, m.keys.Reload):
		m.say("reloading…")
		return m.reload()
	case bkey.Matches(msg, m.keys.Search):
		m.typing = true
		m.focus = paneItems
		m.search.SetValue(m.query)
		m.search.CursorEnd()
		m.search.Focus()
		return textinput.Blink
	case bkey.Matches(msg, m.keys.Close):
		// Escape out of a search first; with none running there is nothing
		// left to back out of.
		if m.query != "" {
			m.query = ""
			m.search.SetValue("")
			m.clear()
			return m.loadItems()
		}
		return nil
	case bkey.Matches(msg, m.keys.Notes):
		return m.setLayer("note")
	case bkey.Matches(msg, m.keys.Recall):
		return m.setLayer("recall")
	case bkey.Matches(msg, m.keys.Both):
		return m.setLayer("")
	case bkey.Matches(msg, m.keys.Add):
		return m.openAdd()
	case bkey.Matches(msg, m.keys.Forget):
		return m.openConfirm()
	case bkey.Matches(msg, m.keys.Open):
		return m.openDetail()
	case bkey.Matches(msg, m.keys.NextPane):
		m.movePane(1)
		return nil
	case bkey.Matches(msg, m.keys.PrevPane):
		m.movePane(-1)
		return nil
	case bkey.Matches(msg, m.keys.Left):
		m.movePane(-1)
		return nil
	case bkey.Matches(msg, m.keys.Right):
		m.movePane(1)
		return nil
	case bkey.Matches(msg, m.keys.Up):
		return m.moveCursor(-1)
	case bkey.Matches(msg, m.keys.Down):
		return m.moveCursor(1)
	case bkey.Matches(msg, m.keys.PageUp):
		return m.moveCursor(-m.rowsPerPane())
	case bkey.Matches(msg, m.keys.PageDown):
		return m.moveCursor(m.rowsPerPane())
	case bkey.Matches(msg, m.keys.HalfUp):
		return m.moveCursor(-halfPage(m.rowsPerPane()))
	case bkey.Matches(msg, m.keys.HalfDown):
		return m.moveCursor(halfPage(m.rowsPerPane()))
	case bkey.Matches(msg, m.keys.Top):
		return m.moveCursor(-len(m.items) - len(m.parts) - len(m.agents))
	case bkey.Matches(msg, m.keys.Bottom):
		return m.moveCursor(len(m.items) + len(m.parts) + len(m.agents))
	}
	return nil
}

// setLayer narrows the memories pane to one layer, or back to both.
func (m *memoryUI) setLayer(l string) tea.Cmd {
	if m.layer == l {
		return nil
	}
	m.layer = l
	return m.loadItems()
}

// movePane walks the focus across the columns. The agents column is skipped
// when there is only one agent: there is nothing to select in it.
func (m *memoryUI) movePane(d int) {
	first := paneAgents
	if len(m.agents) <= 1 {
		first = paneParts
	}
	order := []memPane{}
	for p := first; p <= paneItems; p++ {
		order = append(order, p)
	}
	at := 0
	for i, p := range order {
		if p == m.focus {
			at = i
		}
	}
	m.focus = order[((at+d)%len(order)+len(order))%len(order)]
}

// moveCursor walks the focused pane and reloads whatever hangs off it: a new
// agent has other partitions, a new partition other memories.
func (m *memoryUI) moveCursor(d int) tea.Cmd {
	c := m.cursorOf(m.focus)
	before := c.at
	c.move(d, m.rows(m.focus), m.rowsPerPane())
	if c.at == before {
		return nil
	}
	switch m.focus {
	case paneAgents:
		return m.loadPartitions()
	case paneParts:
		return m.loadItems()
	}
	return nil
}

// searchKey is the box above the memories: enter runs it, escape drops it and
// puts the recent list back.
func (m *memoryUI) searchKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		m.typing = false
		m.search.Blur()
		m.query = strings.TrimSpace(m.search.Value())
		m.clear()
		return m.loadItems()
	case "esc":
		m.typing = false
		m.search.Blur()
		m.search.SetValue("")
		if m.query == "" {
			return nil
		}
		m.query = ""
		return m.loadItems()
	}
	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	return cmd
}

// openDetail shows the whole of one memory: no pane is wide enough for a note
// somebody wrote in sentences.
func (m *memoryUI) openDetail() tea.Cmd {
	if m.focus != paneItems {
		// In the left-hand columns enter has already happened: moving the
		// cursor there loaded what it points at.
		return nil
	}
	it, ok := m.item()
	if !ok {
		return nil
	}
	m.showDet = true
	m.layout()
	m.detail.SetContent(fold(memDetailText(it, m.styles), max(m.detail.Width, 10)))
	m.detail.GotoTop()
	return nil
}

func (m *memoryUI) detailKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case bkey.Matches(msg, m.keys.Close), bkey.Matches(msg, m.keys.Open):
		m.showDet = false
		return nil
	case bkey.Matches(msg, m.keys.Quit):
		m.quitting = true
		return tea.Quit
	}
	if scrollKey(&m.detail, msg) {
		return nil
	}
	var cmd tea.Cmd
	m.detail, cmd = m.detail.Update(msg)
	return cmd
}

// openConfirm is `d`: forgetting is the one thing here that cannot be undone,
// so it is asked for rather than done.
func (m *memoryUI) openConfirm() tea.Cmd {
	it, ok := m.item()
	if !ok {
		return nil
	}
	m.confirm = &it
	return nil
}

func (m *memoryUI) confirmKey(msg tea.KeyMsg) tea.Cmd {
	it := *m.confirm
	switch strings.ToLower(msg.String()) {
	case "y":
		m.confirm = nil
		ctx, mapi, agent, partition := m.ctx, m.api, m.agent(), m.partition()
		return func() tea.Msg {
			return memForgotMsg{id: it.Id, err: mapi.ForgetMemory(ctx, it.Id, agent, partition)}
		}
	case "n", "esc", "ctrl+c":
		m.confirm = nil
	}
	return nil
}

// openAdd writes into whatever is selected, which is the whole point of the
// columns: the agent and the partition are already answered.
func (m *memoryUI) openAdd() tea.Cmd {
	if m.agent() == "" || m.partition() == "" {
		return nil
	}
	m.adding, m.addOn, m.shared = true, addText, false
	m.noteIn.Reset()
	m.about.SetValue("")
	m.noteIn.Focus()
	m.about.Blur()
	return textarea.Blink
}

func (m *memoryUI) addKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case bkey.Matches(msg, m.keys.Close):
		m.closeAdd()
		return nil
	case bkey.Matches(msg, m.keys.Save):
		return m.saveNote()
	case bkey.Matches(msg, m.keys.NextPane):
		m.addFocus(1)
		return nil
	case bkey.Matches(msg, m.keys.PrevPane):
		m.addFocus(-1)
		return nil
	}
	switch m.addOn {
	case addAbout:
		// A single line: enter has nothing to do there but finish.
		if msg.String() == "enter" {
			return m.saveNote()
		}
		var cmd tea.Cmd
		m.about, cmd = m.about.Update(msg)
		return cmd
	case addVisibility:
		if s := msg.String(); s == " " || s == "enter" {
			m.shared = !m.shared
		}
		return nil
	}
	var cmd tea.Cmd
	m.noteIn, cmd = m.noteIn.Update(msg)
	return cmd
}

func (m *memoryUI) addFocus(d int) {
	m.addOn = addField(((int(m.addOn)+d)%3 + 3) % 3)
	m.noteIn.Blur()
	m.about.Blur()
	switch m.addOn {
	case addText:
		m.noteIn.Focus()
	case addAbout:
		m.about.Focus()
	}
}

func (m *memoryUI) closeAdd() {
	m.adding = false
	m.noteIn.Blur()
	m.about.Blur()
}

// saveNote writes the note the modal holds. Visibility is only sent when
// somebody asked for `shared`, so the API's own default stands otherwise.
func (m *memoryUI) saveNote() tea.Cmd {
	text := strings.TrimSpace(m.noteIn.Value())
	if text == "" {
		m.status, m.isError = "a note needs some text", true
		return nil
	}
	in := gen.MemoryWrite{Agent: m.agent(), Partition: m.partition(), Text: text}
	if about := strings.TrimSpace(m.about.Value()); about != "" {
		in.About = &about
	}
	if m.shared {
		in.Visibility = ptrOf(gen.MemoryWriteVisibility("shared"))
	}
	m.closeAdd()
	ctx, mapi := m.ctx, m.api
	return func() tea.Msg {
		item, err := mapi.WriteMemory(ctx, in)
		return memSavedMsg{id: item.Id, err: err}
	}
}

// --- mouse ------------------------------------------------------------------------

// onMouse makes the columns clickable: a click selects the row under the
// pointer in the column it landed in, and the wheel walks that column.
func (m *memoryUI) onMouse(msg tea.MouseMsg) tea.Cmd {
	if m.confirm != nil || m.adding || m.showHelp || m.showDet {
		return nil
	}
	pane := m.paneAt(msg.X)
	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		m.focus = pane
		d := -memWheelRows
		if msg.Button == tea.MouseButtonWheelDown {
			d = memWheelRows
		}
		return m.moveCursor(d)
	case tea.MouseButtonLeft:
		// The release of a click must not be taken for a second click.
		if msg.Action != tea.MouseActionPress {
			return nil
		}
		m.focus = pane
		i, ok := m.rowAt(pane, msg.Y)
		if !ok {
			return nil
		}
		c := m.cursorOf(pane)
		return m.moveCursor(i - c.at)
	}
	return nil
}

// memWheelRows is how far one notch of the wheel walks a column.
const memWheelRows = 3

// --- the command ---------------------------------------------------------------------

// cmdMemoryBrowse is `holaclaw memory` with no subcommand. It needs a terminal
// at both ends, and says so rather than drawing a screen nobody can read.
func cmdMemoryBrowse(ctx context.Context, c *cli, cn *conn) error {
	need := "the memory browser needs a terminal; `memory search|add|forget` are the ones for scripts"
	return browseDaemon(ctx, c, cn, need, func(client *api.Client, where string) error {
		return runMemoryTUI(ctx, c, client, where)
	})
}
