package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	bkey "github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// The trace browser: `holaclaw trace` with nothing after it. `trace <id>` has
// always assumed somebody already had the id — off a `chat --trace` line, or
// out of the database — and then printed the whole thing at once. Here the id
// is the thing being looked for: the sessions on the left, the traces of the
// one selected in the middle, its events on the right, and the whole of an
// event underneath. It is the memory browser's shape, and an event is drawn
// by the same code the chat's activity pane uses, so a run looks the same
// wherever it is read.
//
// `trace <id>` (with --json, and the offline store fallback) is untouched:
// that is the one for scripts.

// traceAPI is the slice of the daemon this browser needs. An interface so the
// model can be driven in a test without a server; nothing else here reaches
// the network.
type traceAPI interface {
	ListSessions(ctx context.Context) ([]gen.Session, error)
	ListTraces(ctx context.Context, sessionID string) ([]gen.TraceSummary, error)
	GetTrace(ctx context.Context, id string) (gen.Trace, error)
}

// trPane is which column the keyboard is talking to.
type trPane int

const (
	paneSessions trPane = iota
	paneTraces
	paneEvents
)

// --- what a request answers with --------------------------------------------------

// The traces and the events carry the sequence number they were asked under,
// and an answer behind the model's is dropped. Walking a session list fires a
// request per row, and without this a slow early answer would land last and
// leave a column showing another session's run.
type (
	trSessionsMsg struct {
		sessions []gen.Session
		err      error
	}
	trTracesMsg struct {
		seq    int
		traces []gen.TraceSummary
		err    error
	}
	trEventsMsg struct {
		seq   int
		trace gen.Trace
		err   error
	}
)

// --- the model ----------------------------------------------------------------------

type traceUI struct {
	ctx   context.Context
	api   traceAPI
	where string // the daemon address, for the header

	keys   traceKeyMap
	styles tuiStyles
	help   help.Model
	filter textinput.Model
	detail viewport.Model

	// What the daemon answered, and what is left of it after the column's
	// filter. The browser filters what it already holds rather than asking
	// again: none of these three endpoints takes a query.
	sessions []gen.Session
	traces   []gen.TraceSummary
	events   []traceRow   // the loaded trace, events and messages in time order
	outs     []toolOutput // the tool results of the loaded trace
	totals   traceSummaryTotals

	sessShown  []gen.Session
	traceShown []gen.TraceSummary
	evShown    []traceRow

	sessCur, traceCur, evCur cursor
	queries                  [3]string

	traceSeq, evSeq      int
	loadTraces, loadEvs  bool
	focus                trPane
	typing               bool
	showHelp             bool
	showDet              bool
	showRaw              bool
	showMsgs             bool // the transcript rows, on unless `m` says otherwise
	status               string
	isError              bool
	width, height, bodyH int
	sessW, tracesW, evsW int
	quitting             bool
}

// runTraceTUI is the whole browser.
func runTraceTUI(ctx context.Context, c *cli, tapi traceAPI, where string) error {
	return runFullScreen(ctx, c, newTraceUI(ctx, tapi, where))
}

func newTraceUI(ctx context.Context, tapi traceAPI, where string) *traceUI {
	st := newTUIStyles()

	q := textinput.New()
	q.Prompt = "/ "
	q.Placeholder = "filter this column"

	h := help.New()
	h.Styles.ShortKey, h.Styles.FullKey = st.helpKey, st.helpKey
	h.Styles.ShortDesc, h.Styles.FullDesc = st.dim, st.dim

	m := &traceUI{
		ctx: ctx, api: tapi, where: where,
		keys: newTraceKeyMap(), styles: st, help: h, filter: q,
		// The messages are on by default: what was said is the half of a run
		// somebody came to read, and the events are the machinery under it.
		showMsgs: true,
	}
	// A size arrives a frame later; until then the columns are laid out for a
	// plausible terminal so the first frame is not drawn at zero columns.
	m.width, m.height = 100, 30
	m.layout()
	return m
}

func (m *traceUI) Init() tea.Cmd { return m.loadSessions() }

// --- requests -------------------------------------------------------------------

func (m *traceUI) loadSessions() tea.Cmd {
	ctx, tapi := m.ctx, m.api
	return func() tea.Msg {
		sessions, err := tapi.ListSessions(ctx)
		return trSessionsMsg{sessions: sessions, err: err}
	}
}

// loadTraceList asks for the selected session's traces under a fresh sequence
// number, which is what makes every earlier answer stale.
func (m *traceUI) loadTraceList() tea.Cmd {
	id := m.sessionID()
	if id == "" {
		m.traces, m.traceShown, m.traceCur = nil, nil, cursor{}
		return nil
	}
	m.traceSeq++
	m.loadTraces = true
	seq, ctx, tapi := m.traceSeq, m.ctx, m.api
	return func() tea.Msg {
		traces, err := tapi.ListTraces(ctx, id)
		return trTracesMsg{seq: seq, traces: traces, err: err}
	}
}

func (m *traceUI) loadEvents() tea.Cmd {
	id := m.traceID()
	if id == "" {
		m.events, m.evShown, m.outs, m.evCur = nil, nil, nil, cursor{}
		return nil
	}
	m.evSeq++
	m.loadEvs = true
	seq, ctx, tapi := m.evSeq, m.ctx, m.api
	return func() tea.Msg {
		tr, err := tapi.GetTrace(ctx, id)
		return trEventsMsg{seq: seq, trace: tr, err: err}
	}
}

// reload is `R`: the focused column again, and everything that hangs off it.
func (m *traceUI) reload() tea.Cmd {
	switch m.focus {
	case paneSessions:
		return m.loadSessions()
	case paneTraces:
		return m.loadTraceList()
	}
	return m.loadEvents()
}

// --- update ---------------------------------------------------------------------

func (m *traceUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case tea.KeyMsg:
		return m, m.onKey(msg)

	case tea.MouseMsg:
		return m, m.onMouse(msg)

	case trSessionsMsg:
		return m, m.onSessions(msg)

	case trTracesMsg:
		return m, m.onTraces(msg)

	case trEventsMsg:
		return m, m.onEvents(msg)
	}
	return m, nil
}

func (m *traceUI) onSessions(msg trSessionsMsg) tea.Cmd {
	if msg.err != nil {
		m.fail("sessions", msg.err)
		return nil
	}
	m.sessions = msg.sessions
	// Newest first: the conversation somebody is here about is almost always
	// the one they were just having.
	sort.SliceStable(m.sessions, func(i, j int) bool {
		return m.sessions[i].UpdatedAt.After(m.sessions[j].UpdatedAt)
	})
	m.applyFilters()
	m.sessCur.clamp(len(m.sessShown), m.rowsPerPane())
	return m.loadTraceList()
}

func (m *traceUI) onTraces(msg trTracesMsg) tea.Cmd {
	if msg.seq != m.traceSeq {
		return nil
	}
	m.loadTraces = false
	if msg.err != nil {
		m.traces, m.traceShown, m.events, m.evShown = nil, nil, nil, nil
		m.failTraces(msg.err)
		return nil
	}
	m.traces = msg.traces
	m.traceCur = cursor{}
	m.applyFilters()
	m.clear()
	return m.loadEvents()
}

func (m *traceUI) onEvents(msg trEventsMsg) tea.Cmd {
	if msg.seq != m.evSeq {
		return nil
	}
	m.loadEvs = false
	if msg.err != nil {
		m.events, m.evShown, m.outs, m.totals = nil, nil, nil, traceSummaryTotals{}
		m.fail("trace", msg.err)
		return nil
	}
	m.events, m.outs = traceRows(msg.trace), traceOutputs(msg.trace)
	m.totals = traceTotals(msg.trace.Events)
	m.evCur = cursor{}
	m.applyFilters()
	return nil
}

// traceMsg is one transcript entry as a row of the events column: what was
// said, between the machinery that said it.
type traceMsg struct {
	msg   gen.Message
	since time.Duration // since the trace started
	line  string        // the sentence the row shows
}

// FilterValue lets the column's `/` reach what was said, not only the event
// types around it.
func (t traceMsg) FilterValue() string {
	return string(t.msg.Role) + " " + t.line + " " + deref(t.msg.Text)
}

// traceRow is one line of the events column. A trace is two interleaved
// stories — what the machinery did and what was said — and reading one
// without the other is what made `trace <id>` hard to follow, so the column
// carries both and this is the little sum type that lets it.
type traceRow struct {
	at    time.Time // the sort key: an event's time, a message's created_at
	order int       // ties: a message sits after the event that produced it
	ev    activity
	msg   traceMsg
	isMsg bool
}

// item hands the row to the list the delegate reads its selection off. An
// event goes in as the activity the chat's delegate already knows how to
// draw; a message as itself.
func (r traceRow) item() list.Item {
	if r.isMsg {
		return r.msg
	}
	return r.ev
}

func (r traceRow) FilterValue() string {
	if r.isMsg {
		return r.msg.FilterValue()
	}
	return r.ev.FilterValue()
}

// traceRows folds a trace into the column's rows: every event, every message,
// in the order they happened. A message is keyed to the event that carries
// its id, so a reply sits under the `reply` event that produced it however
// the two timestamps rounded.
func traceRows(tr gen.Trace) []traceRow {
	if len(tr.Events) == 0 && len(tr.Messages) == 0 {
		return nil
	}
	start := traceStart(tr)
	rows := make([]traceRow, 0, len(tr.Events)+len(tr.Messages))
	// Which event announced which message, so the transcript lines up with
	// the machinery rather than with the clock.
	producer := map[string]int{}
	for i, ev := range tr.Events {
		if ev.MessageId != nil && *ev.MessageId != "" {
			if _, seen := producer[*ev.MessageId]; !seen {
				producer[*ev.MessageId] = i
			}
		}
		rows = append(rows, traceRow{at: ev.Time, order: 2 * i, ev: newActivity(ev, 0, ev.Time.Sub(start))})
	}
	for j, msg := range tr.Messages {
		at, order := msg.CreatedAt, 2*len(tr.Events)+j
		if i, ok := producer[msg.Id]; ok {
			at, order = tr.Events[i].Time, 2*i+1
		}
		rows = append(rows, traceRow{
			at: at, order: order, isMsg: true,
			msg: traceMsg{msg: msg, since: at.Sub(start), line: messageLine(msg)},
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].at.Equal(rows[j].at) {
			return rows[i].order < rows[j].order
		}
		return rows[i].at.Before(rows[j].at)
	})
	return rows
}

// traceStart is the instant everything else is timed from: the first event,
// or the first message in a trace that somehow has none.
func traceStart(tr gen.Trace) time.Time {
	if len(tr.Events) > 0 {
		return tr.Events[0].Time
	}
	if len(tr.Messages) > 0 {
		return tr.Messages[0].CreatedAt
	}
	return time.Time{}
}

// messageLine is one transcript entry on one line: who said it, marked the
// way the chat marks it, and as much of it as the column has room for.
func messageLine(msg gen.Message) string {
	text := oneLine(deref(msg.Text))
	switch msg.Role {
	case gen.MessageRoleUser:
		return "› " + text
	case gen.MessageRoleAssistant:
		return "‹ " + text
	case gen.MessageRoleTool:
		name := deref(msg.ToolName)
		if name == "" {
			name = "tool"
		}
		return "⚙ " + name + " result"
	}
	return "· system"
}

// traceSummaryTotals is what one trace cost: where its wall time went, how
// many tokens it spent and what the upstreams billed for them.
type traceSummaryTotals struct {
	Wall       time.Duration // first event to last
	Model      time.Duration // summed model.response durations
	Tool       time.Duration // summed tool.result durations
	Prompt     int
	Completion int
	ModelCalls int
	ToolCalls  int
	// Cost is what the upstreams reported, summed with the tokens
	// under the one rule (event.UsageTypes): every model call — the chat's,
	// the browser's, the guard's, the taint scorer's, a compaction's, a
	// media call's, an image, an embedding — counted once. It is nil when no
	// call reported a price; Unpriced counts the calls that did not, so a
	// cost with Unpriced > 0 covers only part of the trace.
	Cost     *provider.NanoUSD
	Unpriced int
}

// traceTotals sums a trace. The tokens and the cost follow event.UsageOf —
// the rule the store's trace and session sums follow in SQL — so the strip
// over a trace's events and its row in the traces column say the same thing.
func traceTotals(events []gen.Event) traceSummaryTotals {
	var out traceSummaryTotals
	if len(events) == 0 {
		return out
	}
	var u event.Usage
	first, last := events[0].Time, events[0].Time
	for _, ev := range events {
		if ev.Attrs != nil {
			u.Add(event.UsageOf(event.Type(ev.Type), *ev.Attrs))
		}
		if ev.Time.Before(first) {
			first = ev.Time
		}
		if ev.Time.After(last) {
			last = ev.Time
		}
		d := time.Duration(0)
		if ev.DurationNs != nil {
			d = time.Duration(*ev.DurationNs)
		}
		switch event.Type(ev.Type) {
		case event.ModelResponse:
			out.ModelCalls++
			out.Model += d
		case event.ToolCallEvent:
			out.ToolCalls++
		case event.ToolResult:
			out.Tool += d
		}
	}
	out.Wall = last.Sub(first)
	out.Prompt, out.Completion, out.Cost, out.Unpriced = u.PromptTokens, u.CompletionTokens, u.Cost, u.UnpricedCalls
	return out
}

// traceOutputs is every tool result the trace holds, keyed by the call it
// answers so outputsFor can narrow it down to the selected event.
func traceOutputs(tr gen.Trace) []toolOutput {
	var out []toolOutput
	for _, msg := range tr.Messages {
		if msg.Role != gen.MessageRoleTool || msg.Text == nil {
			continue
		}
		o := toolOutput{text: *msg.Text}
		if msg.ToolName != nil {
			o.name = *msg.ToolName
		}
		if msg.ToolCallId != nil {
			o.callID = *msg.ToolCallId
		}
		out = append(out, o)
	}
	return out
}

// failTraces says the one thing a plain error would hide: a daemon that
// predates this endpoint, or one whose store cannot enumerate traces, answers
// not_implemented — and then there is nothing to browse and nothing broken.
func (m *traceUI) failTraces(err error) {
	var ce *api.ClientError
	if errors.As(err, &ce) && ce.Status == http.StatusNotImplemented {
		m.status, m.isError = "this daemon cannot list traces — use `trace <trace-id>`", true
		return
	}
	m.fail("traces", err)
}

func (m *traceUI) fail(what string, err error) {
	m.status, m.isError = what+": "+err.Error(), true
}

func (m *traceUI) say(s string) { m.status, m.isError = s, false }

func (m *traceUI) clear() { m.status, m.isError = "", false }

// --- selection --------------------------------------------------------------------

func (m *traceUI) session() (gen.Session, bool) {
	if m.sessCur.at >= len(m.sessShown) {
		return gen.Session{}, false
	}
	return m.sessShown[m.sessCur.at], true
}

func (m *traceUI) sessionID() string {
	s, ok := m.session()
	if !ok {
		return ""
	}
	return s.Id
}

func (m *traceUI) trace() (gen.TraceSummary, bool) {
	if m.traceCur.at >= len(m.traceShown) {
		return gen.TraceSummary{}, false
	}
	return m.traceShown[m.traceCur.at], true
}

func (m *traceUI) traceID() string {
	t, ok := m.trace()
	if !ok {
		return ""
	}
	return t.TraceId
}

// row is whatever the events column has selected, event or message.
func (m *traceUI) row() (traceRow, bool) {
	if m.evCur.at >= len(m.evShown) {
		return traceRow{}, false
	}
	return m.evShown[m.evCur.at], true
}

// rows is how many entries a column holds, and cursorOf which cursor walks
// them, so one set of motion keys drives all three.
func (m *traceUI) rows(p trPane) int {
	switch p {
	case paneSessions:
		return len(m.sessShown)
	case paneTraces:
		return len(m.traceShown)
	}
	return len(m.evShown)
}

func (m *traceUI) cursorOf(p trPane) *cursor {
	switch p {
	case paneSessions:
		return &m.sessCur
	case paneTraces:
		return &m.traceCur
	}
	return &m.evCur
}

// --- filtering --------------------------------------------------------------------

// applyFilters narrows each column to its own `/` text. Nothing is asked of
// the daemon again: all three lists are already here, and a filter that went
// over the wire would make a keystroke cost a request.
func (m *traceUI) applyFilters() {
	m.sessShown = m.sessions[:0:0]
	for _, s := range m.sessions {
		if matches(m.queries[paneSessions], sessionFilterText(s)) {
			m.sessShown = append(m.sessShown, s)
		}
	}
	m.traceShown = m.traces[:0:0]
	for _, t := range m.traces {
		if matches(m.queries[paneTraces], traceFilterText(t)) {
			m.traceShown = append(m.traceShown, t)
		}
	}
	m.evShown = m.events[:0:0]
	for _, r := range m.events {
		if r.isMsg && !m.showMsgs {
			continue
		}
		if matches(m.queries[paneEvents], r.FilterValue()) {
			m.evShown = append(m.evShown, r)
		}
	}
	m.sessCur.clamp(len(m.sessShown), m.rowsFor(paneSessions))
	m.traceCur.clamp(len(m.traceShown), m.rowsFor(paneTraces))
	m.evCur.clamp(len(m.evShown), m.rowsFor(paneEvents))
}

func matches(q, text string) bool {
	return q == "" || strings.Contains(strings.ToLower(text), strings.ToLower(q))
}

func sessionFilterText(s gen.Session) string {
	return s.Agent + " " + s.Key + " " + deref(s.Title) + " " + string(s.Kind)
}

func traceFilterText(t gen.TraceSummary) string {
	return t.TraceId + " " + deref(t.Preview) + " " + deref(t.LastType)
}

// --- keys ---------------------------------------------------------------------------

func (m *traceUI) onKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case m.typing:
		return m.filterKey(msg)
	case m.showDet:
		return m.detailKey(msg)
	case m.showHelp:
		// The help is a reference, not a mode: anything shuts it.
		m.showHelp = false
		return nil
	}
	return m.browseKey(msg)
}

func (m *traceUI) browseKey(msg tea.KeyMsg) tea.Cmd {
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
	case bkey.Matches(msg, m.keys.Filter):
		m.typing = true
		m.filter.SetValue(m.queries[m.focus])
		m.filter.CursorEnd()
		m.filter.Focus()
		return textinput.Blink
	case bkey.Matches(msg, m.keys.Close):
		// Escape drops the column's filter first, then backs out a column:
		// the way out is the way in, one step at a time.
		if m.queries[m.focus] != "" {
			m.setQuery("")
			return nil
		}
		if m.focus > paneSessions {
			m.focus--
		}
		return nil
	case bkey.Matches(msg, m.keys.Open):
		return m.open()
	case bkey.Matches(msg, m.keys.Messages):
		return m.toggleMessages()
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
		return m.moveCursor(-m.rows(m.focus))
	case bkey.Matches(msg, m.keys.Bottom):
		return m.moveCursor(m.rows(m.focus))
	}
	return nil
}

// open is enter: in the left columns it moves right, where the cursor has
// already loaded what it points at; on an event it opens the whole of it.
func (m *traceUI) open() tea.Cmd {
	if m.focus < paneEvents {
		m.focus++
		return nil
	}
	return m.openDetail()
}

// toggleMessages is `m`: the transcript rows out of the way, for when the
// question is what the machinery did and the text is in the way of reading
// it — and back, because that is usually the question after.
func (m *traceUI) toggleMessages() tea.Cmd {
	m.showMsgs = !m.showMsgs
	m.applyFilters()
	if m.showMsgs {
		m.say("messages shown")
	} else {
		m.say("messages hidden")
	}
	if r, ok := m.row(); ok && m.showDet {
		m.refreshDetail(r)
	}
	return nil
}

// movePane walks the focus across the three columns.
func (m *traceUI) movePane(d int) {
	m.focus = trPane(((int(m.focus)+d)%3 + 3) % 3)
}

// moveCursor walks the focused column and reloads whatever hangs off it: a
// new session has other traces, a new trace other events.
func (m *traceUI) moveCursor(d int) tea.Cmd {
	c := m.cursorOf(m.focus)
	before := c.at
	c.move(d, m.rows(m.focus), m.rowsPerPane())
	if c.at == before {
		return nil
	}
	switch m.focus {
	case paneSessions:
		return m.loadTraceList()
	case paneTraces:
		return m.loadEvents()
	}
	return nil
}

// filterKey is the box above the focused column: enter keeps it, escape drops
// it.
func (m *traceUI) filterKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "enter":
		m.typing = false
		m.filter.Blur()
		m.setQuery(strings.TrimSpace(m.filter.Value()))
		return nil
	case "esc":
		m.typing = false
		m.filter.Blur()
		m.filter.SetValue("")
		m.setQuery("")
		return nil
	}
	var cmd tea.Cmd
	m.filter, cmd = m.filter.Update(msg)
	return cmd
}

// setQuery narrows the focused column and reloads what hangs off it, because
// the row the cursor lands on after a filter is rarely the one it was on.
func (m *traceUI) setQuery(q string) {
	if m.queries[m.focus] == q {
		return
	}
	m.queries[m.focus] = q
	m.applyFilters()
	m.clear()
}

// openDetail shows the whole of one event: the sentence, every attribute and
// — for a tool call — the response of that call alone.
func (m *traceUI) openDetail() tea.Cmd {
	r, ok := m.row()
	if !ok {
		return nil
	}
	m.showDet, m.showRaw = true, false
	m.layout()
	m.refreshDetail(r)
	return nil
}

// refreshDetail fills the panel from one row. Only a tool line has a response
// behind it: asking outputsFor about any other event would hand back every
// tool result of the trace, because there is no call id on it to narrow by.
func (m *traceUI) refreshDetail(r traceRow) {
	var body string
	switch {
	case r.isMsg && m.showRaw:
		body = rawJSON(r.msg.msg)
	case r.isMsg:
		body = messageDetailText(r.msg, m.styles)
	case m.showRaw:
		body = rawJSON(r.ev.ev)
	default:
		var out []toolOutput
		if wantsToolOutput(r.ev.ev) {
			out = outputsFor(r.ev.ev, m.outs)
		}
		body = traceDetailText(r.ev, out, m.styles)
	}
	m.detail.SetContent(fold(body, max(m.detail.Width, 10)))
	m.detail.GotoTop()
}

func (m *traceUI) detailKey(msg tea.KeyMsg) tea.Cmd {
	switch {
	case bkey.Matches(msg, m.keys.Close), bkey.Matches(msg, m.keys.Open):
		m.showDet = false
		m.layout()
		return nil
	case bkey.Matches(msg, m.keys.Raw):
		m.showRaw = !m.showRaw
		if r, ok := m.row(); ok {
			m.refreshDetail(r)
		}
		return nil
	case bkey.Matches(msg, m.keys.Quit):
		m.quitting = true
		return tea.Quit
	case bkey.Matches(msg, m.keys.Up), bkey.Matches(msg, m.keys.Down):
		// The cursor keeps walking the events with the panel open, so
		// reading down a trace is one key rather than open-close-open.
		cmd := m.moveCursor(map[bool]int{true: -1, false: 1}[bkey.Matches(msg, m.keys.Up)])
		if r, ok := m.row(); ok {
			m.refreshDetail(r)
		}
		return cmd
	}
	if scrollKey(&m.detail, msg) {
		return nil
	}
	var cmd tea.Cmd
	m.detail, cmd = m.detail.Update(msg)
	return cmd
}

// taintLoud is the score from which a message is called out in red rather
// than dimmed: at half the scale the scorer already thinks the text is
// trying to steer the run.
const taintLoud = 0.5

// messageDetailText is the whole of one transcript entry: who wrote it and
// when, what the loop recorded about it — the call it answers, the calls it
// asked for, whether it came from somewhere untrusted — and then the text,
// whole, because that is the thing a column can never show.
func messageDetailText(t traceMsg, s tuiStyles) string {
	msg := t.msg
	var b strings.Builder
	b.WriteString(s.detailKey.Render(string(msg.Role)+" message") + "\n")
	b.WriteString(s.dim.Render(msg.CreatedAt.Local().Format("15:04:05.000")+
		fmt.Sprintf("  ·  +%.1fs into the trace", t.since.Seconds())) + "\n")
	kv := func(k, v string) {
		if v == "" {
			return
		}
		b.WriteString(s.dim.Render(fmt.Sprintf("%-11s", k)+v) + "\n")
	}
	kv("id", msg.Id)
	kv("sender", deref(msg.Sender))
	kv("reply to", deref(msg.ReplyTo))
	kv("tool call", deref(msg.ToolCallId))
	kv("tool", deref(msg.ToolName))
	if msg.Tainted != nil && *msg.Tainted {
		// Untrusted text is the one thing about a message that changes how it
		// must be read, so it is said in the error colour rather than dimmed.
		b.WriteString(s.errText.Render("tainted — this text came from an untrusted source") + "\n")
	}
	if msg.TaintScore != nil {
		// The score is the half that matters: tainted says where the text
		// came from, this says how much of it reads like an instruction, and
		// a high one is worth the red.
		line := fmt.Sprintf("taint %.2f", *msg.TaintScore)
		if *msg.TaintScore >= taintLoud {
			b.WriteString(s.errText.Render(line) + "\n")
		} else {
			b.WriteString(s.dim.Render(line) + "\n")
		}
	}
	if msg.ToolCalls != nil && len(*msg.ToolCalls) > 0 {
		b.WriteString("\n" + s.detailKey.Render("tool calls") + "\n")
		for _, c := range *msg.ToolCalls {
			b.WriteString("— " + c.Name + " (" + c.Id + ")\n")
			if args := prettyJSON(c.Arguments); args != "" {
				b.WriteString(args + "\n")
			}
		}
	}
	if msg.Attachments != nil && len(*msg.Attachments) > 0 {
		b.WriteString("\n" + s.detailKey.Render("attachments") + "\n")
		for _, a := range *msg.Attachments {
			fmt.Fprintf(&b, "— %s  %s  %s\n", a.Name, a.Mime, bytesShort(a.Size))
		}
	}
	if text := deref(msg.Text); text != "" {
		b.WriteString("\n" + s.detailKey.Render("text") + "\n" + text + "\n")
	}
	return b.String()
}

// rawJSON is `J`: the row as the daemon stored it — an event or a message —
// for when the sentence is not what is in doubt.
func rawJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("this cannot be shown as JSON: %v", err)
	}
	return string(b)
}

// --- mouse ------------------------------------------------------------------------

// onMouse makes the columns clickable: a click selects the row under the
// pointer in the column it landed in — and opens it when that column is the
// events — and the wheel walks the column under the pointer.
func (m *traceUI) onMouse(msg tea.MouseMsg) tea.Cmd {
	if m.showHelp {
		return nil
	}
	if m.showDet {
		// With the panel open the pointer is reading it, not the columns.
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(msg)
		return cmd
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
		cmd := m.moveCursor(i - m.cursorOf(pane).at)
		if pane == paneEvents {
			return tea.Batch(cmd, m.openDetail())
		}
		return cmd
	}
	return nil
}

// --- the command ---------------------------------------------------------------------

// cmdTraceBrowse is `holaclaw trace` with no id. Like the memory browser it
// needs a terminal at both ends, and says so rather than drawing a screen
// nobody can read.
func cmdTraceBrowse(ctx context.Context, c *cli, cn *conn) error {
	need := "the trace browser needs a terminal; `trace <trace-id>` is the one for scripts"
	return browseDaemon(ctx, c, cn, need, func(client *api.Client, where string) error {
		return runTraceTUI(ctx, c, client, where)
	})
}
