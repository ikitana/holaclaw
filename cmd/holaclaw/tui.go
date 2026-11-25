package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	bkey "github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	bprogress "github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/lipgloss"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
)

// The full-screen chat. It is one Bubble Tea program over the same chatEngine
// the line-by-line UI drives, so nothing about a turn changes here — only what
// a person sees of it: the reply in one pane, everything the agent did in
// another, and the whole of any one of those things in a panel when they ask
// for it. That panel is why this UI has no -vvv: a tool's entire response is
// a keystroke away from the line that produced it, instead of a wall of text
// under every reply.

// --- what the engine sends the program ------------------------------------------

type (
	tokenMsg string
	noteMsg  string
	eventMsg struct{ ev gen.Event }
	replyMsg struct{ reply replyInfo }
	tickMsg  time.Time
	doneMsg  struct {
		res turnResult
		err error
	}
	approvalMsg struct {
		req   approvalReq
		reply chan approvalAnswer
	}
	historyMsg struct{ msgs []histMsg }
	outputsMsg struct {
		traceID string
		out     []toolOutput
	}
	resetMsg struct{ err error }
)

// tuiSink is the chatSink half of the program: it turns everything a turn
// produces into a message on the Bubble Tea loop, which is the only goroutine
// allowed to touch the model.
type tuiSink struct{ p *tea.Program }

func (s *tuiSink) token(d string)     { s.p.Send(tokenMsg(d)) }
func (s *tuiSink) event(ev gen.Event) { s.p.Send(eventMsg{ev}) }
func (s *tuiSink) note(t string)      { s.p.Send(noteMsg(t)) }
func (s *tuiSink) reply(r replyInfo)  { s.p.Send(replyMsg{r}) }

// approve hands the request to the program and waits for the person. The turn
// is blocked here, which is exactly what an approval is.
func (s *tuiSink) approve(ctx context.Context, req approvalReq) approvalAnswer {
	ch := make(chan approvalAnswer, 1)
	s.p.Send(approvalMsg{req: req, reply: ch})
	select {
	case a := <-ch:
		return a
	case <-ctx.Done():
		return approvalAnswer{}
	}
}

// --- the transcript ---------------------------------------------------------------

type uiRole int

const (
	roleUser uiRole = iota
	roleAgent
	roleSystem
	roleError
)

// uiMessage is one block of the transcript.
type uiMessage struct {
	role uiRole
	text string
	// reactions are the agent's emoji on a user message (channel.reacted),
	// drawn under it the way a chat app does.
	reactions []string
}

// focusArea is which pane the keyboard is talking to.
type focusArea int

const (
	focusInput focusArea = iota
	focusTranscript
	focusActivity
	focusDetail

	// focusNone is not a pane at all: the header rows and the help bar, where
	// a pointer has landed on nothing anybody can talk to.
	focusNone focusArea = -1
)

// --- the model -------------------------------------------------------------------

type chatUI struct {
	ctx  context.Context
	eng  chatEngine
	opts chatOptions
	sink *tuiSink
	info engineInfo

	keys    keyMap
	styles  tuiStyles
	help    help.Model
	input   textarea.Model
	body    viewport.Model
	acts    list.Model
	detail  viewport.Model
	pal     list.Model
	spin    spinner.Model
	bar     bprogress.Model
	md      *glamour.TermRenderer
	mdStyle ansi.StyleConfig // settled before the program starts; see settleTerminal

	msgs     []uiMessage
	rendered []string // msgs, laid out for the current width and mode
	stream   strings.Builder

	busy      bool
	state     uiState // what the header says the chat is doing
	frame     int     // the mascot's animation counter, one per tick
	turn      int
	started   time.Time
	elapsed   time.Duration
	stats     turnStats
	lastTrace string
	outputs   map[string][]toolOutput
	ctxUse    *contextUse // the last context.built, nil before the first prompt

	approval     *approvalReq
	approvalSeen time.Time // when it was put on screen, for the countdown
	answer       chan approvalAnswer

	focus      focusArea
	showActs   bool
	showDetail bool
	showHelp   bool
	showPal    bool
	raw        bool
	showTrace  bool

	width, height int
	bodyH         int // rows the panes get, once the fixed chrome is taken out
	actsW         int // columns the activity pane gets, 0 when it is hidden
	actsTop       int // screen row of the pane's first entry, for a click
	aborting      bool
	quitting      bool
}

// runChatTUI is the whole full-screen frontend.
func runChatTUI(ctx context.Context, c *cli, eng chatEngine, o chatOptions) error {
	sink := &tuiSink{}
	// Whatever has to ask the terminal a question asks it here, while this
	// process is still the only thing reading stdin.
	m := newChatUI(ctx, eng, o, sink, settleTerminal(c.stdout))
	p := tea.NewProgram(m,
		tea.WithContext(ctx),
		tea.WithInput(c.stdin),
		tea.WithOutput(c.stdout),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	sink.p = p
	_, err := p.Run()
	return err
}

func newChatUI(ctx context.Context, eng chatEngine, o chatOptions, sink *tuiSink, md ansi.StyleConfig) *chatUI {
	st := newTUIStyles()

	ta := textarea.New()
	ta.Placeholder = "Ask " + o.agent + " something…  (enter sends, ctrl+j for a newline, /help)"
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(3)
	ta.Prompt = ""
	ta.FocusedStyle.CursorLine = st.plain
	// Enter is send here, so the textarea keeps the newline on a key of its
	// own rather than swallowing it.
	ta.KeyMap.InsertNewline = bkey.NewBinding(bkey.WithKeys("ctrl+j", "alt+enter"))
	ta.Focus()

	acts := list.New(nil, activityDelegate{styles: st, width: 40}, 0, 0)
	acts.Title = "activity"
	acts.SetShowStatusBar(false)
	acts.SetShowHelp(false)
	// The scrollbar says where in the list somebody is, so the dots under it
	// would say it twice and cost a row of entries.
	acts.SetShowPagination(false)
	acts.SetFilteringEnabled(true)
	acts.Styles.Title = st.paneTitle
	acts.Styles.TitleBar = lipgloss.NewStyle().Padding(0, 0, 1, 0)
	// The pane walks and pages on the keys every other pane does. ctrl+p and
	// ctrl+d stay the palette and quit: the screen takes those first.
	km := &acts.KeyMap
	km.CursorDown.SetKeys(append(km.CursorDown.Keys(), "ctrl+n")...)
	km.PrevPage.SetKeys(append(km.PrevPage.Keys(), "ctrl+b", "alt+v")...)
	km.NextPage.SetKeys(append(km.NextPage.Keys(), "ctrl+f", "ctrl+v")...)
	km.GoToStart.SetKeys(append(km.GoToStart.Keys(), "alt+<")...)
	km.GoToEnd.SetKeys(append(km.GoToEnd.Keys(), "alt+>")...)

	pal := list.New(commandItems(), list.NewDefaultDelegate(), 0, 0)
	pal.Title = "commands"
	pal.SetShowStatusBar(false)
	pal.SetShowHelp(false)
	pal.Styles.Title = st.paneTitle
	pal.Styles.TitleBar = lipgloss.NewStyle().Padding(0, 0, 1, 0)

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = st.dim

	h := help.New()
	h.Styles.ShortKey, h.Styles.FullKey = st.helpKey, st.helpKey
	h.Styles.ShortDesc, h.Styles.FullDesc = st.dim, st.dim

	m := &chatUI{
		ctx: ctx, eng: eng, opts: o, sink: sink, info: eng.info(),
		keys: newKeyMap(), styles: st, help: h, mdStyle: md,
		input: ta, acts: acts, pal: pal, spin: sp,
		bar:       bprogress.New(bprogress.WithDefaultGradient(), bprogress.WithoutPercentage()),
		showActs:  true,
		showTrace: o.showTrace,
		outputs:   map[string][]toolOutput{},
	}
	// The prompt is live before the first window size arrives, so somebody
	// can start typing the moment the chat opens rather than watching a
	// placeholder. The real size follows a frame later and re-lays it out.
	m.width, m.height = 80, 24
	m.layout()
	return m
}

// commandItems is the palette's contents as list items.
func commandItems() []list.Item {
	out := make([]list.Item, 0, len(tuiCommands))
	for _, c := range tuiCommands {
		out = append(out, c)
	}
	return out
}

func (m *chatUI) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.loadHistory())
}

// loadHistory puts whatever the conversation already held on screen, so
// resuming one does not open on an empty pane.
func (m *chatUI) loadHistory() tea.Cmd {
	return func() tea.Msg {
		msgs, err := m.eng.history(m.ctx)
		if err != nil {
			return historyMsg{}
		}
		return historyMsg{msgs: msgs}
	}
}

// --- update ------------------------------------------------------------------------

func (m *chatUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case tea.KeyMsg:
		return m, m.onKey(msg)

	case tea.MouseMsg:
		return m, m.onMouse(msg)

	case spinner.TickMsg:
		// The spinner is no longer drawn — the mascot is what moves now — but
		// its tick is still the fastest cadence the program has, so the face
		// is animated on it.
		m.frame++
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case tickMsg:
		m.frame++
		if !m.busy {
			return m, nil
		}
		m.elapsed = time.Since(m.started)
		return m, tickCmd()

	case tokenMsg:
		m.stream.WriteString(string(msg))
		m.refreshBody()
		return m, nil

	case eventMsg:
		if event.Type(msg.ev.Type) == event.ChannelReacted {
			m.react(evAttr(msg.ev, "emoji"))
		}
		countEvent(&m.stats, msg.ev)
		m.noteState(msg.ev)
		m.noteContext(msg.ev)
		m.addActivity(msg.ev)
		return m, nil

	case noteMsg:
		m.say(roleSystem, string(msg))
		return m, nil

	case replyMsg:
		text := m.stream.String()
		m.stream.Reset()
		if msg.reply.text != "" {
			text = msg.reply.text
		}
		for _, at := range msg.reply.attachments {
			text += fmt.Sprintf("\n\n_[attachment: %s (%s)]_", at.name, at.mime)
		}
		m.say(roleAgent, text)
		return m, nil

	case approvalMsg:
		req := msg.req
		m.approval, m.answer, m.approvalSeen = &req, msg.reply, time.Now()
		m.state = stateWaiting
		return m, nil

	case historyMsg:
		m.addHistory(msg.msgs)
		return m, nil

	case outputsMsg:
		m.outputs[msg.traceID] = msg.out
		m.refreshDetail()
		return m, nil

	case resetMsg:
		if msg.err != nil {
			m.say(roleError, msg.err.Error())
			return m, nil
		}
		m.info = m.eng.info()
		m.msgs, m.rendered = nil, nil
		m.acts.SetItems(nil)
		m.turn, m.stats, m.lastTrace, m.ctxUse = 0, turnStats{}, "", nil
		m.say(roleSystem, "new conversation: "+m.info.conversation)
		return m, nil

	case doneMsg:
		return m, m.onDone(msg)

	case bprogress.FrameMsg:
		bar, cmd := m.bar.Update(msg)
		m.bar = bar.(bprogress.Model)
		return m, cmd
	}
	return m, nil
}

// onDone closes a turn: the info line under the reply, and the prompt back.
func (m *chatUI) onDone(msg doneMsg) tea.Cmd {
	m.busy, m.aborting, m.state = false, false, stateIdle
	m.elapsed = time.Since(m.started)
	switch {
	case msg.err != nil:
		m.say(roleError, msg.err.Error())
	case msg.res.aborted:
		m.say(roleSystem, "aborted")
	}
	// The trace id belongs to the status line, not to what was said.
	m.lastTrace = msg.res.traceID
	m.input.Focus()
	m.focus = focusInput
	return textarea.Blink
}

// noteState is what the header learns from one event: a model composing, a
// tool in flight, and the way back once the tool answered. Nothing here can
// override an approval or an abort — those are decided by the thing itself,
// not by whatever the turn happened to trace on the way.
func (m *chatUI) noteState(ev gen.Event) {
	if !m.busy || m.approval != nil || m.aborting {
		return
	}
	switch event.Type(ev.Type) {
	case event.ModelRequest, event.ModelResponse, event.ToolResult:
		// A result is the end of the call it answered: whatever comes next,
		// the model is the one composing it.
		m.state = stateThinking
	case event.ToolCallEvent:
		m.state = stateWorking
	default:
		if strings.HasPrefix(ev.Type, "browser.") {
			// The browser loop runs inside a tool call and can take a while
			// without saying anything else; it is work all the same.
			m.state = stateWorking
		}
	}
}

// --- keys ---------------------------------------------------------------------------

func (m *chatUI) onKey(msg tea.KeyMsg) tea.Cmd {
	// A pending approval owns the keyboard: the turn is stopped on it and
	// nothing else is worth typing.
	if m.approval != nil {
		return m.approvalKey(msg)
	}
	// A search in progress owns escape — it is how somebody backs out of one
	// — so the pane is asked before anything global is.
	filtering := (m.focus == focusActivity && m.acts.FilterState() != list.Unfiltered) ||
		(m.showPal && m.pal.FilterState() != list.Unfiltered)
	switch {
	case bkey.Matches(msg, m.keys.Abort):
		return m.interrupt()
	case bkey.Matches(msg, m.keys.Quit):
		m.quitting = true
		return tea.Quit
	case bkey.Matches(msg, m.keys.Close) && !filtering:
		return m.closeOverlay()
	}
	if m.showPal {
		return m.paletteKey(msg)
	}
	if m.showHelp {
		// Anything closes the full help; it is a reference, not a mode.
		m.showHelp = false
		return nil
	}
	switch {
	case bkey.Matches(msg, m.keys.Palette):
		m.showPal = true
		m.pal.ResetFilter()
		return nil
	case bkey.Matches(msg, m.keys.Activity):
		m.showActs = !m.showActs
		if !m.showActs {
			m.showDetail = false
			if m.focus == focusActivity {
				m.focus = focusInput
			}
		}
		m.layout()
		return nil
	case bkey.Matches(msg, m.keys.Raw):
		m.raw = !m.raw
		m.rerender()
		return nil
	case bkey.Matches(msg, m.keys.Trace):
		m.showTrace = !m.showTrace
		return nil
	case bkey.Matches(msg, m.keys.Clear):
		m.msgs, m.rendered = nil, nil
		m.refreshBody()
		return nil
	case bkey.Matches(msg, m.keys.Focus):
		m.cycleFocus()
		return nil
	}
	// f1 opens the help from anywhere, as in vim and emacs; `?` is a character
	// somebody may well want to type, so it only does where it cannot be part
	// of a message.
	if bkey.Matches(msg, m.keys.Help) && (msg.Type == tea.KeyF1 || m.focus != focusInput) {
		m.showHelp = true
		return nil
	}
	switch m.focus {
	case focusInput:
		return m.inputKey(msg)
	case focusTranscript:
		if scrollKey(&m.body, msg) {
			return nil
		}
		var cmd tea.Cmd
		m.body, cmd = m.body.Update(msg)
		return cmd
	case focusActivity:
		return m.activityKey(msg)
	case focusDetail:
		if scrollKey(&m.detail, msg) {
			return nil
		}
		var cmd tea.Cmd
		m.detail, cmd = m.detail.Update(msg)
		return cmd
	}
	return nil
}

// inputKey is the prompt: enter sends or runs a command, everything else is
// the textarea's.
func (m *chatUI) inputKey(msg tea.KeyMsg) tea.Cmd {
	if bkey.Matches(msg, m.keys.Send) {
		return m.submit(strings.TrimSpace(m.input.Value()))
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return cmd
}

// activityKey drives the pane: the list's own motions and filter, plus enter
// to open what is selected.
func (m *chatUI) activityKey(msg tea.KeyMsg) tea.Cmd {
	if bkey.Matches(msg, m.keys.Detail) && m.acts.FilterState() != list.Filtering {
		return m.openDetail()
	}
	var cmd tea.Cmd
	m.acts, cmd = m.acts.Update(msg)
	return cmd
}

// paletteKey picks a command out of the palette and puts it at the prompt,
// ready to be sent or to have an argument typed after it.
func (m *chatUI) paletteKey(msg tea.KeyMsg) tea.Cmd {
	if msg.String() == "enter" && m.pal.FilterState() != list.Filtering {
		if c, ok := m.pal.SelectedItem().(tuiCommand); ok {
			m.showPal = false
			if c.arg == "" {
				return m.submit("/" + c.name)
			}
			m.input.SetValue("/" + c.name + " ")
			m.input.CursorEnd()
			m.focus = focusInput
			m.input.Focus()
			return textarea.Blink
		}
	}
	var cmd tea.Cmd
	m.pal, cmd = m.pal.Update(msg)
	return cmd
}

// approvalKey is the modal: one letter, or escape to refuse.
func (m *chatUI) approvalKey(msg tea.KeyMsg) tea.Cmd {
	local := m.approval.local
	var ans approvalAnswer
	switch strings.ToLower(msg.String()) {
	case "y":
		ans = approvalAnswer{approve: true, choice: choiceOnce}
	case "s":
		if m.approval.onceOnly {
			return nil
		}
		ans = approvalAnswer{approve: true, choice: choiceSession}
	case "a":
		if local || m.approval.onceOnly {
			// There is nowhere to persist an "always" in the in-process mode.
			return nil
		}
		ans = approvalAnswer{approve: true, choice: choiceAlways}
	case "n", "esc", "ctrl+c":
		ans = approvalAnswer{}
	default:
		return nil
	}
	m.answer <- ans
	m.say(roleSystem, approvalSaid(*m.approval, ans))
	m.approval, m.answer = nil, nil
	if m.busy {
		// The answer goes straight back to the gate, so the call it was
		// about is what happens next.
		m.state = stateWorking
	}
	return nil
}

// approvalSaid is the line left in the transcript, so a decision is still
// visible after the modal is gone.
func approvalSaid(req approvalReq, ans approvalAnswer) string {
	if !ans.approve {
		return "denied " + req.summary
	}
	return "allowed " + req.summary + " (" + ans.choice + ")"
}

// interrupt is Ctrl-C: the first one stops the turn, the second leaves. With
// nothing running there is nothing to stop, so it is the way out straight
// away.
func (m *chatUI) interrupt() tea.Cmd {
	if !m.busy || m.aborting {
		m.quitting = true
		return tea.Quit
	}
	return m.abortTurn()
}

// abortTurn asks the engine to stop the running turn. /abort is this and
// nothing else: it never leaves the chat, however often it is used.
func (m *chatUI) abortTurn() tea.Cmd {
	if !m.busy {
		m.say(roleSystem, "nothing is running")
		return nil
	}
	m.aborting, m.state = true, stateAborting
	eng, ctx := m.eng, m.ctx
	return func() tea.Msg {
		if err := eng.abort(ctx); err != nil {
			return noteMsg("abort: " + err.Error())
		}
		return nil
	}
}

// closeOverlay is escape: shut whatever is open, else go back to the prompt.
func (m *chatUI) closeOverlay() tea.Cmd {
	switch {
	case m.showPal:
		m.showPal = false
	case m.showHelp:
		m.showHelp = false
	case m.showDetail:
		m.closeDetail()
	default:
		m.focus = focusInput
		m.input.Focus()
		return textarea.Blink
	}
	return nil
}

// cycleFocus is tab: the prompt, the transcript, then whichever panes are
// open, and back to the prompt.
func (m *chatUI) cycleFocus() {
	order := []focusArea{focusInput, focusTranscript}
	if m.actsW > 0 {
		order = append(order, focusActivity)
		if m.showDetail {
			order = append(order, focusDetail)
		}
	}
	next := order[0]
	for i, f := range order {
		if f == m.focus {
			next = order[(i+1)%len(order)]
			break
		}
	}
	m.focus = next
	if next == focusInput {
		m.input.Focus()
		return
	}
	m.input.Blur()
}

// --- mouse -------------------------------------------------------------------------

// paneAt is which pane a pointer position is over. Every mouse decision goes
// through it, so a click and a wheel can never disagree about where they are.
func (m *chatUI) paneAt(x, y int) focusArea {
	switch {
	case y < headerRows:
		// The name of the chat and the state line: nothing to aim at.
		return focusNone
	case y >= headerRows+m.bodyH+inputRows:
		// The context row and the help bar, under the prompt box.
		return focusNone
	case y >= headerRows+m.bodyH:
		return focusInput
	}
	if m.actsW == 0 || x < m.actsLeft() {
		return focusTranscript
	}
	if m.showDetail && y >= headerRows+m.actsHeight() {
		return focusDetail
	}
	return focusActivity
}

// actsHeight is how many rows the activity pane has, which is half the body
// once the details panel is under it.
func (m *chatUI) actsHeight() int {
	if m.showDetail {
		return max(m.bodyH/2, 5)
	}
	return m.bodyH
}

// onMouse is what makes the panes clickable: the wheel scrolls whatever is
// under the pointer, and one click on an activity row opens it.
func (m *chatUI) onMouse(msg tea.MouseMsg) tea.Cmd {
	if m.approval != nil || m.showPal || m.showHelp {
		return nil
	}
	pane := m.paneAt(msg.X, msg.Y)
	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		return m.scroll(pane, msg)
	case tea.MouseButtonLeft:
		// A click is the press; the release that follows it must not be
		// taken for a second one.
		if msg.Action != tea.MouseActionPress {
			return nil
		}
		return m.click(pane, msg.X, msg.Y)
	}
	return nil
}

// click moves the focus where the pointer is, opens the activity row it
// landed on, and shuts the details panel when it landed on the close box. A
// click on the prompt box is how most people expect to start typing, so it
// hands the keyboard back rather than doing nothing.
func (m *chatUI) click(pane focusArea, x, y int) tea.Cmd {
	if pane == focusNone {
		return nil
	}
	if pane == focusInput {
		m.focus = focusInput
		m.input.Focus()
		return textarea.Blink
	}
	m.input.Blur()
	if m.detailCloseAt(x, y) {
		// The pointer was in the right-hand column, so the list is where the
		// focus belongs once the panel is gone.
		m.focus = focusActivity
		m.closeDetail()
		return nil
	}
	m.focus = pane
	if pane != focusActivity {
		return nil
	}
	i, ok := m.actsRowAt(y)
	if !ok {
		return nil
	}
	m.acts.Select(i)
	return m.openDetail()
}

// wheelRows is how far one notch of the wheel moves a pane.
const wheelRows = 3

// scroll sends the wheel to the pane under the pointer. The two viewports
// handle it themselves; the list does not — bubbles' list ignores mouse
// messages entirely — so the wheel is turned into cursor moves here, which is
// also what carries it onto the next page.
func (m *chatUI) scroll(pane focusArea, msg tea.MouseMsg) tea.Cmd {
	var cmd tea.Cmd
	switch pane {
	case focusActivity:
		for range wheelRows {
			if msg.Button == tea.MouseButtonWheelUp {
				m.acts.CursorUp()
				continue
			}
			m.acts.CursorDown()
		}
		if m.showDetail {
			// The panel is showing whatever the list has selected, so it
			// follows the wheel rather than going stale.
			return m.openDetail()
		}
	case focusDetail:
		m.detail, cmd = m.detail.Update(msg)
	case focusTranscript:
		m.body, cmd = m.body.Update(msg)
	default:
		// Over the prompt box or the chrome: the transcript is somewhere
		// else, so the wheel there moves nothing.
		return nil
	}
	return cmd
}

// --- turns -------------------------------------------------------------------------

// submit is what enter does: a slash command, or a message to the agent.
func (m *chatUI) submit(text string) tea.Cmd {
	if text == "" {
		return nil
	}
	if name, arg, ok := slashCommand(text); ok {
		m.input.Reset()
		return m.command(name, arg)
	}
	if m.busy {
		m.say(roleSystem, "still working on the last turn — ctrl+c aborts it")
		return nil
	}
	m.input.Reset()
	m.say(roleUser, text)
	m.busy, m.aborting, m.state = true, false, stateThinking
	m.turn++
	m.started, m.elapsed, m.stats = time.Now(), 0, turnStats{}
	eng, ctx, sink := m.eng, m.ctx, m.sink
	return tea.Batch(m.spin.Tick, tickCmd(), func() tea.Msg {
		res, err := eng.run(ctx, text, sink)
		return doneMsg{res: res, err: err}
	})
}

// command runs one slash command. An unknown one is said so rather than sent
// to the agent, which is never what somebody who typed a slash meant.
func (m *chatUI) command(name, arg string) tea.Cmd {
	switch name {
	case "quit", "exit":
		m.quitting = true
		return tea.Quit
	case "help":
		m.showHelp = true
	case "clear":
		m.msgs, m.rendered = nil, nil
		m.refreshBody()
	case "activity":
		m.showActs = !m.showActs
		m.layout()
	case "details", "detail":
		if !m.showDetail {
			return m.openDetail()
		}
		m.closeDetail()
	case "raw":
		m.raw = !m.raw
		m.rerender()
	case "trace":
		m.showTrace = !m.showTrace
		m.say(roleSystem, "trace ids "+onOff(m.showTrace))
	case "abort":
		return m.abortTurn()
	case "info":
		m.say(roleSystem, m.infoText())
	case "new":
		conversation := arg
		if conversation == "" {
			conversation = conversationSuffix(m.opts.conversation)
		}
		eng, ctx := m.eng, m.ctx
		return func() tea.Msg { return resetMsg{err: eng.reset(ctx, conversation)} }
	default:
		m.say(roleSystem, "no such command: /"+name)
	}
	return nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// infoText is what /info answers: where this chat is pointed.
func (m *chatUI) infoText() string {
	where := "in this process"
	if m.info.attached {
		where = "through the daemon"
	}
	s := fmt.Sprintf("agent %s · conversation %s · %s", m.info.agent, m.info.conversation, where)
	if model := m.model(); model != "" {
		s += " · model " + model
	}
	if m.info.sessionID != "" {
		s += " · session " + m.info.sessionID
	}
	return s
}

// model is the model to name: the one a turn actually ran on, else whatever
// the engine knew at the start.
func (m *chatUI) model() string {
	if m.stats.model != "" {
		return m.stats.model
	}
	return m.info.model
}

// openDetail shows the selected activity, fetching the tool response behind
// it when there is one.
func (m *chatUI) openDetail() tea.Cmd {
	a, ok := m.acts.SelectedItem().(activity)
	if !ok {
		return nil
	}
	m.showDetail = true
	m.layout()
	m.refreshDetail()
	if !wantsToolOutput(a.ev) {
		return nil
	}
	// An empty answer is never kept: the tool.result event is emitted before
	// the tool message is appended, so a detail opened while the turn is still
	// running finds nothing behind it, and caching that nothing would leave
	// the panel blank for every tool line of the turn from then on.
	// What counts as an answer is this call's own output: a trace whose other
	// calls are already cached still has nothing behind this line.
	if len(outputsFor(a.ev, m.outputs[a.ev.TraceId])) > 0 {
		return nil
	}
	eng, ctx, trace := m.eng, m.ctx, a.ev.TraceId
	return func() tea.Msg {
		return outputsMsg{traceID: trace, out: eng.toolOutputs(ctx, trace)}
	}
}

// addActivity appends one event to the pane, following the bottom unless the
// person has scrolled away from it.
func (m *chatUI) addActivity(ev gen.Event) {
	since := time.Since(m.started)
	if d := ev.Time.Sub(m.started); !ev.Time.IsZero() && d > 0 {
		since = d
	}
	items := m.acts.Items()
	atEnd := len(items) == 0 || m.acts.Index() >= len(items)-1
	items = append(items, newActivity(ev, m.turn, since))
	m.acts.SetItems(items)
	if atEnd && m.acts.FilterState() != list.Filtering {
		m.acts.Select(len(items) - 1)
	}
}

// addHistory puts a resumed conversation on screen. Tool messages are left
// out: they belong to the activity pane, not to what was said.
func (m *chatUI) addHistory(msgs []histMsg) {
	for _, h := range msgs {
		if h.text == "" {
			continue
		}
		switch h.role {
		case "user":
			m.say(roleUser, h.text)
		case "assistant":
			m.say(roleAgent, h.text)
		}
	}
}

// say appends one block to the transcript and scrolls to it.
func (m *chatUI) say(role uiRole, text string) {
	if text == "" {
		return
	}
	msg := uiMessage{role: role, text: text}
	m.msgs = append(m.msgs, msg)
	m.rendered = append(m.rendered, m.renderMessage(msg))
	m.refreshBody()
}

// react puts the agent's emoji under the message it answers. A reaction is
// always on the turn's own message, which is the last one the user sent.
func (m *chatUI) react(emoji string) {
	if emoji == "" {
		return
	}
	for i := len(m.msgs) - 1; i >= 0; i-- {
		if m.msgs[i].role == roleUser {
			m.msgs[i].reactions = append(m.msgs[i].reactions, emoji)
			m.rendered[i] = m.renderMessage(m.msgs[i])
			m.refreshBody()
			return
		}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(200*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}
