package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
)

// The full-screen chat is a plain Bubble Tea model, so every one of these
// drives it the way the runtime would — a window size, then messages — and
// reads what it drew. No terminal is involved.

// fakeEngine is a chatEngine that does whatever the test says.
type fakeEngine struct {
	inf      engineInfo
	onRun    func(sink chatSink) turnResult
	hist     []histMsg
	outputs  []toolOutput
	aborted  int
	resetTo  string
	lastSent string
}

func (f *fakeEngine) info() engineInfo { return f.inf }

func (f *fakeEngine) run(_ context.Context, text string, sink chatSink) (turnResult, error) {
	f.lastSent = text
	if f.onRun == nil {
		sink.reply(replyInfo{text: "ok"})
		return turnResult{traceID: "t1"}, nil
	}
	return f.onRun(sink), nil
}

func (f *fakeEngine) abort(context.Context) error { f.aborted++; return nil }

func (f *fakeEngine) history(context.Context) ([]histMsg, error) { return f.hist, nil }

func (f *fakeEngine) toolOutputs(context.Context, string) []toolOutput { return f.outputs }

func (f *fakeEngine) reset(_ context.Context, c string) error { f.resetTo = c; return nil }

func (f *fakeEngine) close() error { return nil }

// newTestUI is a model already sized, the way the runtime hands one a window
// before anything else happens.
func newTestUI(t *testing.T, eng chatEngine) *chatUI {
	t.Helper()
	m := newChatUI(context.Background(), eng, chatOptions{agent: "main", conversation: "cli"},
		&tuiSink{}, settleTerminal(io.Discard))
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return m
}

// send drives one message through Update and drops the command, for the cases
// that only care about what ended up on screen.
func send(m *chatUI, msg tea.Msg) {
	m.Update(msg)
}

// key is one keystroke as the runtime would deliver it.
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEscape}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "ctrl+o":
		return tea.KeyMsg{Type: tea.KeyCtrlO}
	case "ctrl+p":
		return tea.KeyMsg{Type: tea.KeyCtrlP}
	case "f1":
		return tea.KeyMsg{Type: tea.KeyF1}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestFullScreenNeedsATerminalAndSomebodyAtIt(t *testing.T) {
	// The streams a test or a pipe gives are never terminals, so every case
	// here has to come out false for a reason of its own.
	c := &cli{stdin: strings.NewReader(""), stdout: &strings.Builder{}, stderr: &strings.Builder{}}
	for _, tc := range []struct {
		name string
		o    chatOptions
	}{
		{"one shot", chatOptions{once: "hi"}},
		{"asked for plain", chatOptions{plain: true}},
		{"piped", chatOptions{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if fullScreen(c, tc.o) {
				t.Error("the full-screen UI was chosen without a terminal")
			}
		})
	}
	t.Setenv("TERM", "dumb")
	if fullScreen(c, chatOptions{}) {
		t.Error("a dumb terminal must get the line-by-line UI")
	}
}

func TestSlashCommand(t *testing.T) {
	for _, tc := range []struct {
		line, name, arg string
		ok              bool
	}{
		{"/quit", "quit", "", true},
		{"/new  side quest ", "new", "side quest", true},
		{"/NEW", "new", "", true},
		{"hello", "", "", false},
		{"/", "", "", false},
		{" /help", "help", "", true},
	} {
		name, arg, ok := slashCommand(tc.line)
		if ok != tc.ok || name != tc.name || arg != tc.arg {
			t.Errorf("slashCommand(%q) = %q, %q, %v; want %q, %q, %v",
				tc.line, name, arg, ok, tc.name, tc.arg, tc.ok)
		}
	}
}

func TestHeaderNamesTheChat(t *testing.T) {
	m := newTestUI(t, &fakeEngine{inf: engineInfo{
		agent: "main", conversation: "cli", model: "cloud", attached: true,
	}})
	view := m.View()
	for _, want := range []string{"holaclaw", "main", "cli", "cloud", "daemon"} {
		if !strings.Contains(view, want) {
			t.Errorf("the header does not name %q:\n%s", want, view)
		}
	}
}

func TestSendingPutsTheMessageAndTheReplyOnScreen(t *testing.T) {
	eng := &fakeEngine{}
	m := newTestUI(t, eng)
	m.input.SetValue("find me a flight")
	send(m, keyMsg("enter"))
	if eng.lastSent != "" {
		// run happens on the returned command, not inline.
		t.Fatalf("the turn ran before its command did")
	}
	if !strings.Contains(m.View(), "find me a flight") {
		t.Errorf("what was typed is not in the transcript:\n%s", m.View())
	}
	if !m.busy {
		t.Error("the chat is not marked busy while a turn runs")
	}
	send(m, replyMsg{reply: replyInfo{text: "one stop, 149€"}})
	send(m, doneMsg{res: turnResult{traceID: "t1"}})
	if m.busy {
		t.Error("the chat is still busy after the turn finished")
	}
	if !strings.Contains(m.View(), "149") {
		t.Errorf("the reply is not in the transcript:\n%s", m.View())
	}
}

// The agent's reaction is drawn under the message it reacted to, and a turn
// that only reacted adds no empty reply block.
func TestReactionIsDrawnUnderTheMessage(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	m.input.SetValue("deployed?")
	send(m, keyMsg("enter"))
	attrs := map[string]any{"emoji": "👍"}
	send(m, eventMsg{gen.Event{Type: string(event.ChannelReacted), Attrs: &attrs}})
	send(m, replyMsg{})
	send(m, doneMsg{res: turnResult{traceID: "t1"}})
	if len(m.msgs) != 1 || len(m.msgs[0].reactions) != 1 || m.msgs[0].reactions[0] != "👍" {
		t.Fatalf("transcript = %+v", m.msgs)
	}
	v := m.View()
	if i, j := strings.Index(v, "deployed?"), strings.Index(v, "👍"); i < 0 || j < i {
		t.Errorf("the reaction is not under the message:\n%s", v)
	}

	// Nothing to react to yet: ignored.
	m = newTestUI(t, &fakeEngine{})
	m.react("👍")
	if len(m.msgs) != 0 {
		t.Fatalf("a reaction made a block: %+v", m.msgs)
	}
}

// The plain UI prints the reaction to stdout as a line of the answer, even
// with --quiet; other events stay progress on stderr.
func TestPlainReactionLine(t *testing.T) {
	var out, errOut strings.Builder
	c := &cli{stdin: strings.NewReader(""), stdout: &out, stderr: &errOut}
	p := newPlainSink(c, chatOptions{quiet: true}, false)
	defer p.close()
	attrs := map[string]any{"emoji": "🎉"}
	p.event(gen.Event{Type: string(event.ChannelReacted), Attrs: &attrs})
	p.event(gen.Event{Type: string(event.ChannelReacted)})
	if out.String() != "  ↳ 🎉\n" {
		t.Fatalf("stdout = %q", out.String())
	}
}

func TestStreamedTokensAppearBeforeTheReply(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	m.busy = true
	send(m, tokenMsg("one "))
	send(m, tokenMsg("stop"))
	if !strings.Contains(m.View(), "one stop") {
		t.Errorf("streamed text is not on screen:\n%s", m.View())
	}
	// The reply carries no text when it was streamed: what is on screen is
	// what must be kept.
	send(m, replyMsg{})
	send(m, doneMsg{})
	if n := strings.Count(m.View(), "one stop"); n != 1 {
		t.Errorf("the streamed reply was kept %d times, want 1:\n%s", n, m.View())
	}
}

// twoCalls is a turn with two tool calls behind one trace, which is what the
// engine hands back for either of their lines.
func twoCalls() []toolOutput {
	return []toolOutput{
		{name: "files.read", callID: "c1", text: "the departures board"},
		{name: "browser.do", callID: "c2", text: "one stop, LHR→CDG, 149€"},
	}
}

// panelText is everything the details panel holds, top and bottom: only the
// first screenful of it is ever in the view.
func panelText(m *chatUI) string {
	m.detail.GotoBottom()
	s := m.detail.View()
	m.detail.GotoTop()
	return s + m.detail.View()
}

func TestActivityPaneAndDetailsReplaceTheDump(t *testing.T) {
	eng := &fakeEngine{outputs: twoCalls()}
	m := newTestUI(t, eng)
	m.busy, m.started = true, time.Now()
	send(m, eventMsg{ev: gen.Event{
		Type: string(event.ToolResult), TraceId: "t1", Time: time.Now(),
		Attrs: &map[string]any{"tool": "browser.do", "call_id": "c2", "is_error": false, "tainted": true},
	}})
	if n := len(m.acts.Items()); n != 1 {
		t.Fatalf("activity items = %d, want 1", n)
	}
	if !strings.Contains(m.View(), "browser.do") {
		t.Errorf("the activity pane does not show the call:\n%s", m.View())
	}

	// Opening the entry asks for the tool's whole response and shows it.
	cmd := m.openDetail()
	if cmd == nil {
		t.Fatal("opening a tool line did not ask for its output")
	}
	msg, ok := cmd().(outputsMsg)
	if !ok {
		t.Fatalf("openDetail sent %T, want outputsMsg", cmd())
	}
	send(m, msg)
	if !strings.Contains(m.View(), "details") {
		t.Errorf("the details panel is not on screen:\n%s", m.View())
	}
	panel := panelText(m)
	for _, want := range []string{"149€", "tainted"} {
		if !strings.Contains(panel, want) {
			t.Errorf("the details panel is missing %q:\n%s", want, panel)
		}
	}
	// And only that call's output: the other call of the same turn is
	// somebody else's line.
	if strings.Contains(panel, "departures board") {
		t.Errorf("the panel dumped another call's output:\n%s", panel)
	}

	// The other line shows the other output, out of the same cache.
	send(m, eventMsg{ev: gen.Event{
		Type: string(event.ToolResult), TraceId: "t1", Time: time.Now(),
		Attrs: &map[string]any{"tool": "files.read", "call_id": "c1"},
	}})
	m.acts.Select(1)
	m.openDetail()
	panel = panelText(m)
	if !strings.Contains(panel, "departures board") || strings.Contains(panel, "149€") {
		t.Errorf("the second line does not show its own output alone:\n%s", panel)
	}

	// An event from before the loop carried a call id has nothing to match
	// on, so it falls back to everything the trace answered.
	send(m, eventMsg{ev: gen.Event{
		Type: string(event.ToolResult), TraceId: "t1", Time: time.Now(),
		Attrs: &map[string]any{"tool": "files.read"},
	}})
	m.acts.Select(2)
	m.openDetail()
	panel = panelText(m)
	if !strings.Contains(panel, "departures board") || !strings.Contains(panel, "149€") {
		t.Errorf("an event without a call id lost the trace's output:\n%s", panel)
	}
}

// lateOutputEngine answers for the earlier call of the turn but not yet for
// the one being looked at, the way the store does while the turn is still
// running: the tool.result event is emitted before the tool message is
// appended.
type lateOutputEngine struct {
	fakeEngine
	asked int
}

func (e *lateOutputEngine) toolOutputs(context.Context, string) []toolOutput {
	e.asked++
	if e.asked == 1 {
		return twoCalls()[:1]
	}
	return twoCalls()
}

func TestDetailsAskAgainWhenTheOutputWasNotStoredYet(t *testing.T) {
	eng := &lateOutputEngine{}
	m := newTestUI(t, eng)
	m.busy, m.started = true, time.Now()
	send(m, eventMsg{ev: gen.Event{
		Type: string(event.ToolResult), TraceId: "t1", Time: time.Now(),
		Attrs: &map[string]any{"tool": "browser.do", "call_id": "c2"},
	}})

	// Opened while the turn runs: the trace answers for another call, which
	// is not an answer for this one and must not stand in for one.
	cmd := m.openDetail()
	if cmd == nil {
		t.Fatal("opening a tool line did not ask for its output")
	}
	send(m, cmd())
	if strings.Contains(panelText(m), "departures board") {
		t.Error("the panel showed the other call's output while its own was missing")
	}

	// Opened again once the message is in the store: the panel fills.
	cmd = m.openDetail()
	if cmd == nil {
		t.Fatal("the empty answer was cached: the second open asked for nothing")
	}
	send(m, cmd())
	if panel := panelText(m); !strings.Contains(panel, "149€") {
		t.Errorf("the details panel never shows the tool output:\n%s", panel)
	}
}

func TestClickSelectsTheActivityUnderThePointer(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	m.busy, m.started = true, time.Now()
	for i := range 3 {
		send(m, eventMsg{ev: gen.Event{
			Type: string(event.ToolCallEvent), TraceId: "t1", Time: time.Now(),
			Attrs: &map[string]any{"tool": "files.write", "args": `{"path":"note` + string(rune('1'+i)) + `.txt"}`},
		}})
	}
	i, ok := m.actsRowAt(m.actsTop + 1)
	if !ok || i != 1 {
		t.Fatalf("the second row maps to %d (ok=%v), want 1", i, ok)
	}
	if _, ok := m.actsRowAt(m.actsTop - 1); ok {
		t.Error("a row above the pane was taken for an entry")
	}
	if _, ok := m.actsRowAt(m.actsTop + 99); ok {
		t.Error("a row past the last entry was taken for one")
	}
}

func TestApprovalModalAnswersTheTurn(t *testing.T) {
	for _, tc := range []struct {
		name, press string
		approve     bool
		choice      string
	}{
		{"once", "y", true, choiceOnce},
		{"session", "s", true, choiceSession},
		{"always", "a", true, choiceAlways},
		{"no", "n", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestUI(t, &fakeEngine{})
			reply := make(chan approvalAnswer, 1)
			send(m, approvalMsg{reply: reply, req: approvalReq{
				tool: "files.write", summary: `write "notes.txt"`,
				args: `{"path":"notes.txt"}`, expires: time.Now().Add(time.Minute),
			}})
			view := m.View()
			for _, want := range []string{"approval", "notes.txt", "[y] once", "[n] no"} {
				if !strings.Contains(view, want) {
					t.Errorf("the modal is missing %q:\n%s", want, view)
				}
			}
			send(m, keyMsg(tc.press))
			select {
			case got := <-reply:
				if got.approve != tc.approve || got.choice != tc.choice {
					t.Errorf("answer = %+v, want approve=%v choice=%q", got, tc.approve, tc.choice)
				}
			default:
				t.Fatal("the turn was never answered")
			}
			if m.approval != nil {
				t.Error("the modal stayed open after an answer")
			}
		})
	}
}

// Local mode persists nothing, so "always" is not on offer and the key does
// nothing rather than quietly granting a session.
func TestLocalApprovalHasNoAlways(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	reply := make(chan approvalAnswer, 1)
	send(m, approvalMsg{reply: reply, req: approvalReq{
		tool: "files.write", summary: "write notes.txt", local: true,
	}})
	if strings.Contains(m.View(), "[a] always") {
		t.Error("local mode offered an always")
	}
	if !strings.Contains(m.View(), "nothing is persisted") {
		t.Errorf("local mode did not say choices are not persisted:\n%s", m.View())
	}
	send(m, keyMsg("a"))
	if m.approval == nil {
		t.Error("`a` resolved an approval that has no always")
	}
}

// A once-only approval (a browser step) is yes or no: the keys that would
// keep a grant are neither shown nor taken.
func TestOnceOnlyApprovalHasNoSessionOrAlways(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	reply := make(chan approvalAnswer, 1)
	send(m, approvalMsg{reply: reply, req: approvalReq{
		tool: "browser.do", summary: "browser: a move classified send: click on g4r2", onceOnly: true,
	}})
	if v := m.View(); strings.Contains(v, "[s] session") || strings.Contains(v, "[a] always") || !strings.Contains(v, "[y] once") {
		t.Errorf("the modal offered more than once:\n%s", v)
	}
	send(m, keyMsg("s"))
	send(m, keyMsg("a"))
	if m.approval == nil {
		t.Fatal("`s` or `a` resolved a once-only approval")
	}
	send(m, keyMsg("y"))
	if ans := <-reply; !ans.approve || ans.choice != choiceOnce {
		t.Errorf("answer = %+v", ans)
	}
}

func TestPlainOnceOnlyApproval(t *testing.T) {
	for said, want := range map[string]approvalAnswer{
		"y\n": {approve: true, choice: choiceOnce},
		"s\n": {},
		"a\n": {},
		"n\n": {},
	} {
		var errOut strings.Builder
		c := &cli{stdin: strings.NewReader(said), stdout: &strings.Builder{}, stderr: &errOut}
		p := newPlainSink(c, chatOptions{quiet: true}, true)
		got := p.approve(context.Background(), approvalReq{summary: "a step", onceOnly: true})
		p.close()
		if got != want {
			t.Errorf("%q: answer = %+v, want %+v", said, got, want)
		}
		if o := errOut.String(); strings.Contains(o, "[s]ession") || strings.Contains(o, "[a]lways") || !strings.Contains(o, "[y]es once / [n]o:") {
			t.Errorf("prompt = %q", o)
		}
	}
}

func TestCommandsDoWhatTheySay(t *testing.T) {
	eng := &fakeEngine{inf: engineInfo{agent: "main", conversation: "cli"}}
	m := newTestUI(t, eng)
	m.say(roleAgent, "something")

	m.command("clear", "")
	if len(m.msgs) != 0 {
		t.Error("/clear left the transcript alone")
	}
	m.command("activity", "")
	if m.showActs {
		t.Error("/activity did not hide the pane")
	}
	m.command("raw", "")
	if !m.raw {
		t.Error("/raw did not turn markdown off")
	}
	m.command("trace", "")
	if !m.showTrace {
		t.Error("/trace did not turn trace ids on")
	}
	m.command("info", "")
	if !strings.Contains(m.View(), "agent main") {
		t.Errorf("/info said nothing useful:\n%s", m.View())
	}
	if cmd := m.command("abort", ""); cmd != nil {
		cmd()
	}
	m.command("nope", "")
	if !strings.Contains(m.View(), "no such command") {
		t.Errorf("an unknown command was not reported:\n%s", m.View())
	}
	// The keys are a panel over the body, so it goes last: nothing else is
	// readable while it is up.
	m.command("help", "")
	if !m.showHelp {
		t.Error("/help did not open the keys")
	}
}

func TestF1OpensTheHelpFromThePrompt(t *testing.T) {
	m := newTestUI(t, &fakeEngine{inf: engineInfo{agent: "main", conversation: "cli"}})
	if m.focus != focusInput {
		t.Fatalf("the chat did not start on the prompt: focus %v", m.focus)
	}
	send(m, keyMsg("?"))
	if m.showHelp {
		t.Error("`?` in the prompt opened the help instead of being typed")
	}
	if m.input.Value() != "?" {
		t.Errorf("`?` did not reach the prompt: %q", m.input.Value())
	}
	send(m, keyMsg("f1"))
	if !m.showHelp {
		t.Error("f1 in the prompt did not open the help")
	}
}

func TestNewConversationStartsAFreshOne(t *testing.T) {
	eng := &fakeEngine{inf: engineInfo{agent: "main", conversation: "cli"}}
	m := newTestUI(t, eng)
	m.say(roleAgent, "old talk")
	cmd := m.command("new", "side-quest")
	if cmd == nil {
		t.Fatal("/new did nothing")
	}
	msg := cmd()
	if eng.resetTo != "side-quest" {
		t.Errorf("the engine moved to %q, want side-quest", eng.resetTo)
	}
	eng.inf.conversation = "side-quest"
	send(m, msg)
	if len(m.msgs) != 1 || !strings.Contains(m.msgs[0].text, "side-quest") {
		t.Errorf("the transcript was not started over: %+v", m.msgs)
	}
}

// Ctrl-C is the abort key while a turn runs, and only then the way out.
func TestCtrlCAbortsThenQuits(t *testing.T) {
	eng := &fakeEngine{}
	m := newTestUI(t, eng)
	m.busy = true
	if cmd := m.onKey(keyMsg("ctrl+c")); cmd != nil {
		cmd()
	}
	if eng.aborted != 1 {
		t.Fatalf("aborts = %d, want 1", eng.aborted)
	}
	if m.quitting {
		t.Fatal("the first ctrl+c left the chat")
	}
	m.onKey(keyMsg("ctrl+c"))
	if !m.quitting {
		t.Error("the second ctrl+c did not leave")
	}
}

func TestHistoryIsOnScreenWhenAConversationResumes(t *testing.T) {
	eng := &fakeEngine{hist: []histMsg{
		{role: "user", text: "what is the build doing"},
		{role: "tool", text: "exit 0", toolName: "exec"},
		{role: "assistant", text: "it is green"},
	}}
	m := newTestUI(t, eng)
	send(m, historyMsg{msgs: eng.hist})
	view := m.View()
	if !strings.Contains(view, "what is the build doing") || !strings.Contains(view, "it is green") {
		t.Errorf("the conversation did not come back:\n%s", view)
	}
	if strings.Contains(view, "exit 0") {
		t.Error("a tool message was put in the transcript; it belongs to the activity pane")
	}
}

func TestPaletteFillsThePrompt(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	m.onKey(keyMsg("ctrl+p"))
	if !m.showPal {
		t.Fatal("ctrl+p did not open the palette")
	}
	if !strings.Contains(m.View(), "/new") {
		t.Errorf("the palette is empty:\n%s", m.View())
	}
	// "new" takes an argument, so picking it leaves the prompt ready to type
	// into rather than running anything.
	m.pal.Select(0)
	m.paletteKey(keyMsg("enter"))
	if m.showPal {
		t.Error("the palette stayed open after a pick")
	}
	if got := m.input.Value(); got != "/new " {
		t.Errorf("prompt = %q, want %q", got, "/new ")
	}
}

func TestPrettyJSONAndWrapping(t *testing.T) {
	if got := prettyJSON(`{"path":"a.txt"}`); !strings.Contains(got, "\"path\": \"a.txt\"") {
		t.Errorf("prettyJSON = %q", got)
	}
	if got := prettyJSON("not json"); got != "not json" {
		t.Errorf("prettyJSON left %q", got)
	}
	if got := prettyJSON("{}"); got != "" {
		t.Errorf("prettyJSON of nothing = %q", got)
	}
	if got := wrap("one two three four", 9); got != "one two\nthree\nfour" {
		t.Errorf("wrap = %q", got)
	}
	if got := truncate("abcdef", 4); got != "abc…" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("ab", 4); got != "ab" {
		t.Errorf("truncate shortened a short line: %q", got)
	}
}

// A row opens on one click, not two: the press does the work and the release
// that follows it must change nothing.
func TestOneClickOpensAnActivity(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	m.busy, m.started = true, time.Now()
	for range 4 {
		send(m, eventMsg{ev: gen.Event{
			Type: string(event.BrowserStep), TraceId: "t1", Time: time.Now(),
			Attrs: &map[string]any{"n": 1, "action": "click", "ref": "r5"},
		}})
	}
	press := tea.MouseMsg{
		X: m.actsLeft() + 2, Y: m.actsTop + 2,
		Button: tea.MouseButtonLeft, Action: tea.MouseActionPress,
	}
	m.Update(press)
	if !m.showDetail {
		t.Fatal("one click did not open the details panel")
	}
	if m.acts.Index() != 2 {
		t.Errorf("selected %d, want the row under the pointer (2)", m.acts.Index())
	}
	if m.focus != focusActivity {
		t.Error("the click did not move the focus to the pane")
	}
	// The release of the same click must not count as a second one.
	before := m.acts.Index()
	release := press
	release.Action = tea.MouseActionRelease
	m.Update(release)
	if m.acts.Index() != before {
		t.Errorf("the release moved the selection to %d", m.acts.Index())
	}
}

// The details panel scrolls: under the pointer with the wheel, and with the
// keys once tab has reached it.
func TestDetailsPanelScrolls(t *testing.T) {
	long := strings.Repeat("a line of a very long tool response\n", 200)
	m := newTestUI(t, &fakeEngine{outputs: []toolOutput{{name: "exec", text: long}}})
	m.busy, m.started = true, time.Now()
	send(m, eventMsg{ev: gen.Event{
		Type: string(event.ToolResult), TraceId: "t1", Time: time.Now(),
		Attrs: &map[string]any{"tool": "exec", "is_error": false},
	}})
	if cmd := m.openDetail(); cmd != nil {
		send(m, cmd())
	}
	if m.detail.AtBottom() {
		t.Fatal("the panel has nothing to scroll; the test cannot say anything")
	}

	// The wheel, over the panel rather than over the list above it.
	y := headerRows + m.actsHeight() + 1
	if pane := m.paneAt(m.actsLeft()+2, y); pane != focusDetail {
		t.Fatalf("the point below the activity pane is %v, want the details panel", pane)
	}
	m.Update(tea.MouseMsg{X: m.actsLeft() + 2, Y: y, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	if m.detail.YOffset == 0 {
		t.Error("the wheel did not scroll the details panel")
	}

	// And the keys, once the focus is on it.
	m.focus = focusDetail
	at := m.detail.YOffset
	m.onKey(tea.KeyMsg{Type: tea.KeyPgDown})
	if m.detail.YOffset <= at {
		t.Error("page down did not scroll the details panel")
	}
}

// Tab reaches every pane that is open, and only those.
func TestTabCyclesTheOpenPanes(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	want := []focusArea{focusTranscript, focusActivity, focusInput}
	for _, w := range want {
		m.cycleFocus()
		if m.focus != w {
			t.Fatalf("focus = %v, want %v", m.focus, w)
		}
	}
	m.showActs = false
	m.layout()
	m.cycleFocus()
	if m.focus != focusTranscript {
		t.Fatalf("focus = %v, want the transcript", m.focus)
	}
	m.cycleFocus()
	if m.focus != focusInput {
		t.Errorf("focus = %v; a hidden pane must not be in the cycle", m.focus)
	}
}

// One blank line between blocks, never two, and the trace id stays out of
// what was said.
func TestTranscriptSpacingAndNoTraceInTheText(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	m.say(roleUser, "hello")
	m.say(roleAgent, "hi there")
	if strings.Contains(m.body.View(), "\n\n\n") {
		t.Errorf("a gap of more than one blank line:\n%q", m.body.View())
	}
	m.showTrace = true
	send(m, doneMsg{res: turnResult{traceID: "01J0TRACE"}})
	for _, msg := range m.msgs {
		if strings.Contains(msg.text, "01J0TRACE") {
			t.Errorf("the trace id was put in the transcript: %q", msg.text)
		}
	}
	if m.lastTrace != "01J0TRACE" {
		t.Errorf("lastTrace = %q; the status line has nothing to show", m.lastTrace)
	}
}

// Every pane is drawn at exactly the size the layout gave it — a box one row
// too tall loses its bottom border to the clip, which is how the details
// panel lost its own.
func TestPanesAreDrawnAtTheirExactSize(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	m.busy, m.started = true, time.Now()
	for range 60 {
		send(m, eventMsg{ev: gen.Event{
			Type: string(event.ToolResult), TraceId: "t1", Time: time.Now(),
			Attrs: &map[string]any{"tool": "exec"},
		}})
	}
	for _, open := range []bool{false, true} {
		if open {
			m.openDetail()
			m.detail.SetContent(strings.Repeat("a long detail line\n", 200))
		}
		body := m.bodyView()
		if h := lipgloss.Height(body); h != m.bodyH {
			t.Errorf("details open=%v: body is %d rows, want %d", open, h, m.bodyH)
		}
		if w := lipgloss.Width(body); w != m.width {
			t.Errorf("details open=%v: body is %d columns, want %d", open, w, m.width)
		}
		// Two closed boxes on the right when the panel is open, one when it
		// is not: a missing bottom border shows up as a missing corner.
		want := 2
		if open {
			want = 3
		}
		if n := strings.Count(body, "╰"); n != want {
			t.Errorf("details open=%v: %d bottom-left corners, want %d:\n%s", open, n, want, body)
		}
	}
}

// The bar is drawn only when there is something to scroll, and its thumb
// moves with the offset.
func TestScrollbar(t *testing.T) {
	s := newTUIStyles()
	if got := s.scrollbar(4, 4, 4, 0); strings.ContainsAny(got, "│┃") {
		t.Errorf("a bar was drawn for a pane that fits: %q", got)
	}
	if got := s.scrollbar(0, 100, 10, 0); got != "" {
		t.Errorf("a bar was drawn with no rows: %q", got)
	}
	top := strings.Split(s.scrollbar(6, 60, 6, 0), "\n")
	if !strings.Contains(top[0], "┃") {
		t.Errorf("at the top the thumb is not on the first row: %q", top)
	}
	bottom := strings.Split(s.scrollbar(6, 60, 6, 1), "\n")
	if !strings.Contains(bottom[len(bottom)-1], "┃") {
		t.Errorf("at the bottom the thumb is not on the last row: %q", bottom)
	}
	// Halfway down is neither end.
	mid := strings.Split(s.scrollbar(6, 60, 6, 0.5), "\n")
	if strings.Contains(mid[0], "┃") || strings.Contains(mid[len(mid)-1], "┃") {
		t.Errorf("halfway down the thumb sat at an end: %q", mid)
	}
	if n := len(bottom); n != 6 {
		t.Errorf("the bar is %d rows, want 6", n)
	}
}

// The panel is where the whole of a thing is read, so nothing in it is cut.
func TestDetailsFoldsRatherThanCuts(t *testing.T) {
	long := strings.Repeat("abcdefghij", 12) // 120 columns, wider than any panel
	m := newTestUI(t, &fakeEngine{outputs: []toolOutput{{name: "exec", text: long}}})
	m.busy, m.started = true, time.Now()
	send(m, eventMsg{ev: gen.Event{
		Type: string(event.ToolResult), TraceId: "t1", Time: time.Now(),
		Attrs: &map[string]any{"tool": "exec"},
	}})
	if cmd := m.openDetail(); cmd != nil {
		send(m, cmd())
	}
	panel := stripStyle(m.detail.View())
	if strings.Contains(panel, "…") {
		t.Errorf("the panel cut a line:\n%s", panel)
	}
	// A line wider than the panel is carried on to the next row, so the one
	// long line is on screen as several.
	if n := strings.Count(panel, "abcdefghij"); n < 2 {
		t.Errorf("the long line was not carried on (%d rows):\n%s", n, panel)
	}
	if !strings.Contains(fold(long, m.detail.Width), "\n") {
		t.Error("fold left the line whole at the panel width")
	}
}

func TestFold(t *testing.T) {
	if got := fold("abcdefghijklmnopqr", 8); got != "abcdefgh\nijklmnop\nqr" {
		t.Errorf("fold = %q", got)
	}
	// A carried row keeps the indent, so a dump holds its shape.
	if got := fold("    abcdefgh", 8); got != "    abcd\n    efgh" {
		t.Errorf("indented fold = %q", got)
	}
	if got := fold("short", 20); got != "short" {
		t.Errorf("fold touched a line that fits: %q", got)
	}
	// A styled line is left whole: folding through an escape would paint the
	// rest of the pane.
	styled := "\x1b[36m" + strings.Repeat("x", 40) + "\x1b[0m"
	if got := fold(styled, 10); got != styled {
		t.Errorf("fold broke a styled line: %q", got)
	}
}

// The panel has a close box, and clicking it shuts the panel without
// disturbing what the list has selected.
func TestDetailsCloseBox(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	m.busy, m.started = true, time.Now()
	for range 5 {
		send(m, eventMsg{ev: gen.Event{
			Type: string(event.ToolCallEvent), TraceId: "t1", Time: time.Now(),
			Attrs: &map[string]any{"tool": "exec"},
		}})
	}
	m.acts.Select(3)
	m.openDetail()
	if !strings.Contains(stripStyle(m.bodyView()), detailCloseMark) {
		t.Fatalf("no close box on the panel:\n%s", m.bodyView())
	}
	x, y := m.width-2, headerRows+m.actsHeight()+detailTitleRows
	if !m.detailCloseAt(x, y) {
		t.Fatalf("the close box is not where the panel drew it (%d,%d)", x, y)
	}
	m.Update(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.showDetail {
		t.Error("clicking the close box left the panel open")
	}
	if m.acts.Index() != 3 {
		t.Errorf("closing moved the selection to %d", m.acts.Index())
	}
	if m.focus != focusActivity {
		t.Errorf("focus = %v after closing, want the list", m.focus)
	}
}

// Clicking the prompt box is how most people put the keyboard back where
// they type, and the chrome around it is not a pane at all.
func TestClickingTheComposerFocusesIt(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	m.busy, m.started = true, time.Now()
	for range 40 {
		send(m, eventMsg{ev: gen.Event{
			Type: string(event.ToolCallEvent), TraceId: "t1", Time: time.Now(),
			Attrs: &map[string]any{"tool": "exec"},
		}})
	}
	for range 40 {
		m.say(roleAgent, "a line of the conversation")
	}

	click := func(x, y int) tea.Cmd {
		var cmd tea.Cmd
		_, cmd = m.Update(tea.MouseMsg{
			X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress,
		})
		return cmd
	}
	inputRow := headerRows + m.bodyH + 1
	if pane := m.paneAt(4, inputRow); pane != focusInput {
		t.Fatalf("a row in the prompt box is %v, want the prompt", pane)
	}

	// Away from the prompt first, so the click has something to undo.
	click(4, headerRows+1)
	if m.focus != focusTranscript || m.input.Focused() {
		t.Fatalf("a click in the transcript left the focus at %v (input focused=%v)",
			m.focus, m.input.Focused())
	}
	if click(4, inputRow) == nil {
		t.Error("the click on the prompt did not start the cursor blinking")
	}
	if m.focus != focusInput || !m.input.Focused() {
		t.Fatalf("the click on the prompt left the focus at %v (input focused=%v)",
			m.focus, m.input.Focused())
	}
	// The box says where typing goes, so it wears the active border. The
	// colour is what says it, and a test has no terminal to show colour in,
	// so the frame itself is what is asked.
	if got := m.inputStyle().GetBorderTopForeground(); got != m.styles.paneActive.GetBorderTopForeground() {
		t.Errorf("the focused prompt box is not drawn with the active border (%v)", got)
	}
	// A click back in the transcript takes the keyboard away again.
	click(4, headerRows+1)
	if m.focus != focusTranscript || m.input.Focused() {
		t.Errorf("the prompt kept the keyboard after a click elsewhere (focus %v)", m.focus)
	}
	if got := m.inputStyle().GetBorderTopForeground(); got != m.styles.pane.GetBorderTopForeground() {
		t.Errorf("the blurred prompt box kept the active border (%v)", got)
	}
	click(4, inputRow)

	// The header and the help bar are not panes: a click on them changes
	// nothing at all.
	for _, y := range []int{0, headerRows - 1, headerRows + m.bodyH + inputRows} {
		if pane := m.paneAt(4, y); pane != focusNone {
			t.Errorf("row %d is %v, want no pane", y, pane)
		}
		click(4, y)
		if m.focus != focusInput || !m.input.Focused() {
			t.Errorf("a click on row %d moved the focus to %v", y, m.focus)
		}
	}

	// And the wheel over the prompt box leaves the transcript where it was.
	m.body.GotoTop()
	m.Update(tea.MouseMsg{
		X: 4, Y: inputRow, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress,
	})
	if m.body.YOffset != 0 {
		t.Errorf("the wheel over the prompt scrolled the transcript to %d", m.body.YOffset)
	}
}

// The mascot is animated, so every state has frames of its own and they
// change from one tick to the next.
func TestMascotFrames(t *testing.T) {
	for _, st := range []uiState{stateIdle, stateThinking, stateWorking, stateWaiting, stateAborting} {
		if st.word() == "" {
			t.Errorf("state %d has no word", st)
		}
		first := mascotFrame(st, 0)
		if first == "" {
			t.Fatalf("state %q has no frame", st.word())
		}
		if mascotFrame(st, 1) == first {
			t.Errorf("state %q does not animate: %q twice", st.word(), first)
		}
		if got := mascotFrame(st, len(mascotFrames[st])); got != first {
			t.Errorf("state %q did not cycle back: %q, want %q", st.word(), got, first)
		}
		// A counter that has gone negative still lands on a frame.
		if got := mascotFrame(st, -1); got == "" {
			t.Errorf("state %q has no frame at a negative tick", st.word())
		}
	}
	// A state nobody drew frames for still says something.
	if got := mascotFrame(uiState(99), 0); got == "" {
		t.Error("an unknown state drew nothing")
	}
}

// The word beside the mascot follows the turn: what the model is doing, what
// a tool is doing, and who is being waited on.
func TestHeaderSaysWhatTheChatIsDoing(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	state := func() string { return stripStyle(strings.SplitN(m.View(), "\n", 2)[0]) }
	if !strings.Contains(state(), "ready") {
		t.Fatalf("an idle chat does not say ready:\n%s", state())
	}

	m.input.SetValue("find me a flight")
	m.submit("find me a flight")
	ev := func(typ event.Type, attrs map[string]any) {
		send(m, eventMsg{ev: gen.Event{Type: string(typ), TraceId: "t1", Time: time.Now(), Attrs: &attrs}})
	}
	ev(event.ModelRequest, map[string]any{"model": "cloud"})
	if !strings.Contains(state(), "thinking") {
		t.Errorf("a model request does not read as thinking:\n%s", state())
	}
	ev(event.ToolCallEvent, map[string]any{"tool": "browser.do", "call_id": "c1"})
	if !strings.Contains(state(), "working") {
		t.Errorf("a tool call does not read as working:\n%s", state())
	}

	// An approval is on the turn's way, so it is the state until it is
	// answered.
	reply := make(chan approvalAnswer, 1)
	send(m, approvalMsg{reply: reply, req: approvalReq{tool: "browser.do", summary: "open a page"}})
	if !strings.Contains(state(), "waiting") {
		t.Errorf("a pending approval does not read as waiting:\n%s", state())
	}
	send(m, keyMsg("y"))
	if !strings.Contains(state(), "working") {
		t.Errorf("after an approval the call is not what is running:\n%s", state())
	}

	// A browser step is inside that call, so it is work too.
	ev(event.BrowserStep, map[string]any{"n": 1, "action": "click"})
	if !strings.Contains(state(), "working") {
		t.Errorf("a browser step does not read as working:\n%s", state())
	}
	ev(event.ToolResult, map[string]any{"tool": "browser.do", "call_id": "c1"})
	if !strings.Contains(state(), "thinking") {
		t.Errorf("after a result the model is not the one composing:\n%s", state())
	}

	if cmd := m.abortTurn(); cmd != nil {
		cmd()
	}
	if !strings.Contains(state(), "aborting") {
		t.Errorf("an abort does not say so:\n%s", state())
	}
	send(m, doneMsg{res: turnResult{traceID: "t1"}})
	if !strings.Contains(state(), "ready") {
		t.Errorf("a finished turn does not go back to ready:\n%s", state())
	}
}

// The two header rows are one line each at any width, and the state stays on
// screen when there is not room for everything.
func TestHeaderFitsANarrowWindow(t *testing.T) {
	m := newTestUI(t, &fakeEngine{inf: engineInfo{
		agent: "a-long-agent-name", conversation: "a-long-conversation-name",
		model: "a-very-long-model-name", attached: true,
	}})
	for _, w := range []int{40, 80, 120} {
		m.Update(tea.WindowSizeMsg{Width: w, Height: 40})
		m.busy, m.elapsed = true, 1234*time.Millisecond
		for _, row := range []string{m.headerView(), m.statusView()} {
			if strings.Contains(row, "\n") {
				t.Errorf("at %d columns a header row wrapped:\n%s", w, row)
			}
			if got := lipgloss.Width(row); got > w {
				t.Errorf("at %d columns a header row is %d wide:\n%s", w, got, row)
			}
		}
		if !strings.Contains(stripStyle(m.headerView()), "thinking") {
			t.Errorf("at %d columns the state fell off the header:\n%s", w, m.headerView())
		}
	}
}

// stripStyle drops the SGR escapes, so a test can read what a pane says.
func stripStyle(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < '@' || s[j] > '~') {
				j++
			}
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// The wheel scrolls the activity pane. bubbles' list ignores mouse messages
// altogether, so this only works because the wheel is turned into cursor
// moves — which is exactly what was missing when the bar appeared but nothing
// under it moved.
func TestWheelScrollsTheActivityPane(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	m.busy, m.started = true, time.Now()
	for range 80 {
		send(m, eventMsg{ev: gen.Event{
			Type: string(event.ToolCallEvent), TraceId: "t1", Time: time.Now(),
			Attrs: &map[string]any{"tool": "exec"},
		}})
	}
	m.acts.Select(0)
	at, thumb := m.acts.Index(), thumbRow(m.actsView())
	if thumb < 0 {
		t.Fatal("no scrollbar thumb on a list that does not fit")
	}

	wheel := func(b tea.MouseButton) {
		m.Update(tea.MouseMsg{
			X: m.actsLeft() + 2, Y: m.actsTop + 1,
			Button: b, Action: tea.MouseActionPress,
		})
	}
	wheel(tea.MouseButtonWheelDown)
	if m.acts.Index() <= at {
		t.Fatalf("the wheel did not move the list: still at %d", m.acts.Index())
	}
	// The bar tracks the cursor, not the page, so it moves on every notch
	// rather than staying put until the list turns over.
	if got := thumbRow(m.actsView()); got <= thumb {
		t.Errorf("the thumb stayed at row %d after scrolling down", got)
	}
	down := m.acts.Index()
	wheel(tea.MouseButtonWheelUp)
	if m.acts.Index() >= down {
		t.Errorf("the wheel up did not go back: %d then %d", down, m.acts.Index())
	}

	// And it carries on past the ends rather than running off them.
	for range 100 {
		wheel(tea.MouseButtonWheelDown)
	}
	if got := m.acts.Index(); got != len(m.acts.Items())-1 {
		t.Errorf("scrolling to the end left the cursor at %d of %d", got, len(m.acts.Items()))
	}
	for range 100 {
		wheel(tea.MouseButtonWheelUp)
	}
	if got := m.acts.Index(); got != 0 {
		t.Errorf("scrolling back to the start left the cursor at %d", got)
	}
}

// thumbRow is which row of a rendered pane carries the scrollbar thumb, or -1
// when the bar is not drawn.
func thumbRow(pane string) int {
	for i, line := range strings.Split(stripStyle(pane), "\n") {
		if strings.Contains(line, "┃") {
			return i
		}
	}
	return -1
}

// The transcript pages on vim's and emacs' keys once tab has reached it; the
// prompt keeps them for editing, and ctrl+p stays the palette.
func TestTranscriptPagesOnVimAndEmacsKeys(t *testing.T) {
	m := newTestUI(t, &fakeEngine{})
	m.body.SetContent(strings.Repeat("a line of the transcript\n", 300))
	m.body.GotoTop()

	m.input.SetValue("ab")
	m.onKey(memKeyMsg("ctrl+f"))
	if m.body.YOffset != 0 {
		t.Fatal("ctrl+f at the prompt scrolled the transcript")
	}
	m.onKey(memKeyMsg("ctrl+b"))
	m.onKey(memKeyMsg("x"))
	if got := m.input.Value(); got != "axb" {
		t.Fatalf("the prompt holds %q, want its editing keys to have moved the cursor: \"axb\"", got)
	}

	m.focus = focusTranscript
	m.onKey(memKeyMsg("ctrl+v"))
	if m.body.YOffset != m.body.Height {
		t.Fatalf("ctrl+v scrolled the transcript to %d, want a page: %d", m.body.YOffset, m.body.Height)
	}
	m.onKey(memKeyMsg("alt+v"))
	if m.body.YOffset != 0 {
		t.Fatalf("alt+v scrolled the transcript to %d, want back to the top", m.body.YOffset)
	}
	m.onKey(memKeyMsg("alt+>"))
	if !m.body.AtBottom() {
		t.Fatal("alt+> did not reach the end of the transcript")
	}
	m.onKey(memKeyMsg("ctrl+p"))
	if !m.showPal {
		t.Fatal("ctrl+p on the transcript did not open the palette")
	}
}
