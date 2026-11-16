package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/shopspring/decimal"

	"holaclaw/internal/api"
	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// The trace browser is a plain Bubble Tea model, so every one of these drives
// it the way the runtime would — a window size, then messages — and reads
// what it drew. No daemon and no terminal are involved: the model talks to a
// fake traceAPI that records what it was asked for.

// fakeTraces answers the three calls the browser makes and remembers each one.
type fakeTraces struct {
	sessions []gen.Session
	byTrace  map[string][]gen.TraceSummary
	traces   map[string]gen.Trace

	calls []string

	tracesErr error
	traceErr  error
}

func (f *fakeTraces) ListSessions(context.Context) ([]gen.Session, error) {
	f.calls = append(f.calls, "sessions")
	return f.sessions, nil
}

func (f *fakeTraces) ListTraces(_ context.Context, sessionID string) ([]gen.TraceSummary, error) {
	f.calls = append(f.calls, "traces:"+sessionID)
	if f.tracesErr != nil {
		return nil, f.tracesErr
	}
	return f.byTrace[sessionID], nil
}

func (f *fakeTraces) GetTrace(_ context.Context, id string) (gen.Trace, error) {
	f.calls = append(f.calls, "trace:"+id)
	if f.traceErr != nil {
		return gen.Trace{}, f.traceErr
	}
	return f.traces[id], nil
}

// t0 is the instant the fixture's older trace starts at, fixed so a row's
// clock is the same on every machine.
var t0 = time.Date(2026, 9, 22, 10, 0, 0, 0, time.Local)

func evAttrs(kv map[string]any) *map[string]any { return &kv }

func ns(d time.Duration) *int64 { n := int64(d); return &n }

// twoSessions is the fixture most of these open on: two sessions, two traces
// under the first, and one whole run — a question, two tool calls with their
// results, a model response with its tokens, a billed decision and a reply.
func twoSessions() *fakeTraces {
	return &fakeTraces{
		sessions: []gen.Session{
			{Id: "S1", Key: "tg/main/42", Agent: "main", Kind: "chat", Title: ptrOf("the deploy"), UpdatedAt: t0.Add(time.Hour),
				PromptTokens: ptrOf(12300), CompletionTokens: ptrOf(1100), Cost: usd("0.0042")},
			{Id: "S2", Key: "slack/eng", Agent: "scribe", Kind: "chat", UpdatedAt: t0.Add(-time.Hour)},
		},
		byTrace: map[string][]gen.TraceSummary{
			"S1": {
				{TraceId: "T2", SessionId: "S1", StartedAt: t0.Add(time.Hour), EndedAt: t0.Add(time.Hour + time.Second),
					Events: 2, ToolCalls: 0, LastType: ptrOf(string(event.ModelRequest)), Preview: ptrOf("and now the second question")},
				{TraceId: "T1", SessionId: "S1", StartedAt: t0, EndedAt: t0.Add(3 * time.Second),
					Events: 7, ToolCalls: 2, LastType: ptrOf(string(event.Reply)), Preview: ptrOf("how is the build"),
					PromptTokens: 1000, CompletionTokens: 200, Cost: usd("0.0031"), UnpricedCalls: 1},
			},
			"S2": {
				{TraceId: "T9", SessionId: "S2", StartedAt: t0, EndedAt: t0, Events: 1,
					LastType: ptrOf(string(event.RunAborted)), Preview: ptrOf("standup please")},
			},
		},
		traces: map[string]gen.Trace{
			"T2": {TraceId: "T2", Events: []gen.Event{
				{Id: "E1", TraceId: "T2", Type: string(event.RunStarted), Time: t0.Add(time.Hour)},
				{Id: "E2", TraceId: "T2", Type: string(event.ModelRequest), Time: t0.Add(time.Hour + time.Second)},
			}},
			"T1": {
				TraceId: "T1",
				Events: []gen.Event{
					{Id: "F0", TraceId: "T1", Type: string(event.ChannelReceived), Time: t0, MessageId: ptrOf("M0")},
					{Id: "F1", TraceId: "T1", Type: string(event.RunStarted), Time: t0},
					{Id: "F2", TraceId: "T1", Type: string(event.ToolCallEvent), Time: t0.Add(time.Second),
						Attrs: evAttrs(map[string]any{"tool": "browser", "call_id": "c1"})},
					{Id: "F3", TraceId: "T1", Type: string(event.ToolResult), Time: t0.Add(1500 * time.Millisecond),
						DurationNs: ns(500 * time.Millisecond), Attrs: evAttrs(map[string]any{"tool": "browser", "call_id": "c1"})},
					{Id: "F4", TraceId: "T1", Type: string(event.ToolCallEvent), Time: t0.Add(2 * time.Second),
						Attrs: evAttrs(map[string]any{"tool": "memory", "call_id": "c2"})},
					{Id: "F5", TraceId: "T1", Type: string(event.ModelResponse), Time: t0.Add(2500 * time.Millisecond),
						DurationNs: ns(1200 * time.Millisecond),
						Attrs:      evAttrs(map[string]any{"model": "gemma", "prompt_tokens": float64(1000), "completion_tokens": float64(200)})},
					{Id: "F6", TraceId: "T1", Type: string(event.GuardDecision), Time: t0.Add(2600 * time.Millisecond),
						Attrs: evAttrs(map[string]any{"cost": 0.0031, "verdict": "allow"})},
					{Id: "F7", TraceId: "T1", Type: string(event.Reply), Time: t0.Add(3 * time.Second), MessageId: ptrOf("MA")},
				},
				Messages: []gen.Message{
					{Id: "M0", TraceId: "T1", Role: gen.MessageRoleUser, Text: ptrOf("how is the build"),
						Sender: ptrOf("angel"), CreatedAt: t0},
					{Id: "M1", TraceId: "T1", Role: gen.MessageRoleTool, ToolCallId: ptrOf("c1"), Tainted: ptrOf(true),
						ToolName: ptrOf("browser"), Text: ptrOf("the page the browser fetched"), CreatedAt: t0.Add(1500 * time.Millisecond)},
					{Id: "M2", TraceId: "T1", Role: gen.MessageRoleTool, ToolCallId: ptrOf("c2"),
						ToolName: ptrOf("memory"), Text: ptrOf("what memory answered"), CreatedAt: t0.Add(2 * time.Second)},
					{Id: "MA", TraceId: "T1", Role: gen.MessageRoleAssistant, Text: ptrOf("the build is green"),
						CreatedAt: t0.Add(3 * time.Second), ToolCalls: &[]gen.ToolCall{
							{Id: "c1", Name: "browser", Arguments: `{"url":"https://ci.example"}`},
						}},
				},
			},
			"T9": {TraceId: "T9", Events: []gen.Event{
				{Id: "G1", TraceId: "T9", Type: string(event.RunAborted), Time: t0}}},
		},
	}
}

// openTraces builds a sized model and runs it through its opening requests,
// which is the state every test below starts from.
func openTraces(t *testing.T, f *fakeTraces) *traceUI {
	t.Helper()
	m := newTraceUI(context.Background(), f, "config socket")
	m.Update(tea.WindowSizeMsg{Width: 150, Height: 44})
	driveTraces(t, m, m.Init())
	return m
}

// openOnTheWholeRun walks to the trace with everything in it — T1 — which is
// where the message rows, the totals and the tool output live.
func openOnTheWholeRun(t *testing.T, f *fakeTraces) *traceUI {
	t.Helper()
	m := openTraces(t, f)
	m.focus = paneTraces
	pressTrace(t, m, "j")
	if m.traceID() != "T1" {
		t.Fatalf("selected %q, want the whole run T1", m.traceID())
	}
	m.focus = paneEvents
	return m
}

// driveTraces runs a command and everything it leads to, the way the runtime
// would.
func driveTraces(t *testing.T, m *traceUI, cmd tea.Cmd) {
	t.Helper()
	for range 20 {
		if cmd == nil {
			return
		}
		msg := cmd()
		if msg == nil {
			return
		}
		_, cmd = m.Update(msg)
	}
	t.Fatal("the browser kept issuing commands; something loops")
}

// pressTrace is one keystroke, followed by whatever it set off.
func pressTrace(t *testing.T, m *traceUI, s string) {
	t.Helper()
	_, cmd := m.Update(memKeyMsg(s))
	driveTraces(t, m, cmd)
}

func traceScreen(m *traceUI) string { return stripStyle(m.View()) }

// selectRow puts the events column's cursor on the first row whose text
// contains what, so a test names the row it means rather than counting.
func selectRow(t *testing.T, m *traceUI, what string) {
	t.Helper()
	for i, r := range m.evShown {
		line := r.ev.line
		if r.isMsg {
			line = r.msg.line
		}
		if strings.Contains(line, what) {
			m.evCur.at = i
			return
		}
	}
	t.Fatalf("no row about %q in %d rows", what, len(m.evShown))
}

func TestOpeningWalksSessionsThenTracesThenEvents(t *testing.T) {
	f := twoSessions()
	m := openTraces(t, f)

	want := []string{"sessions", "traces:S1", "trace:T2"}
	if strings.Join(f.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v, want %v", f.calls, want)
	}
	view := traceScreen(m)
	for _, s := range []string{"sessions", "traces", "events", "the deploy", "scribe",
		"how is the build", "run.started", "T2"} {
		if !strings.Contains(view, s) {
			t.Errorf("the screen does not mention %q:\n%s", s, view)
		}
	}
}

func TestMovingTheSessionCursorLoadsItsTraces(t *testing.T) {
	f := twoSessions()
	m := openTraces(t, f)
	pressTrace(t, m, "j")

	if m.sessionID() != "S2" {
		t.Fatalf("selected %q, want the second session", m.sessionID())
	}
	if last := f.calls[len(f.calls)-1]; last != "trace:T9" {
		t.Fatalf("last call = %q, want the trace of the session moved to (%v)", last, f.calls)
	}
	if !strings.Contains(traceScreen(m), "standup") {
		t.Errorf("the traces column still shows the old session:\n%s", traceScreen(m))
	}
}

func TestEnterWalksRightAndOpensTheEvent(t *testing.T) {
	f := twoSessions()
	m := openTraces(t, f)

	pressTrace(t, m, "enter") // sessions → traces
	if m.focus != paneTraces {
		t.Fatalf("focus = %v, want the traces column", m.focus)
	}
	pressTrace(t, m, "j") // the older trace, the one with the tools
	if m.traceID() != "T1" {
		t.Fatalf("selected %q, want T1", m.traceID())
	}
	pressTrace(t, m, "enter") // traces → events
	if m.focus != paneEvents {
		t.Fatalf("focus = %v, want the events column", m.focus)
	}
	selectRow(t, m, "tool browser")
	pressTrace(t, m, "enter")

	if !m.showDet {
		t.Fatal("enter on an event did not open the details")
	}
	view := traceScreen(m)
	if !strings.Contains(view, "the page the browser fetched") {
		t.Errorf("the details do not carry the output of the selected call:\n%s", view)
	}
	// The other call of the same trace belongs to the other row.
	if strings.Contains(view, "what memory answered") {
		t.Errorf("the details carry another call's output:\n%s", view)
	}
	// A browser has no turns to date an event against.
	if strings.Contains(view, "into turn") {
		t.Errorf("the details still talk about a turn:\n%s", view)
	}
	if !strings.Contains(view, "into the trace") {
		t.Errorf("the details do not date the event against the trace:\n%s", view)
	}

	// `J` swaps it for the event as the daemon stored it.
	pressTrace(t, m, "J")
	if raw := traceScreen(m); !strings.Contains(raw, `"call_id"`) || !strings.Contains(raw, "F2") {
		t.Errorf("`J` did not show the raw event:\n%s", raw)
	}
	pressTrace(t, m, "esc")
	if m.showDet {
		t.Fatal("escape did not close the details")
	}
}

// The bug this is here for: outputsFor falls back to the whole trace when the
// event has no call id, so every line used to show every tool response.
func TestOnlyAToolLineCarriesToolOutput(t *testing.T) {
	m := openOnTheWholeRun(t, twoSessions())

	selectRow(t, m, "model")
	pressTrace(t, m, "enter")
	view := traceScreen(m)
	if strings.Contains(view, "tool output") {
		t.Errorf("a model event shows a tool output section:\n%s", view)
	}
	for _, s := range []string{"the page the browser fetched", "what memory answered"} {
		if strings.Contains(view, s) {
			t.Errorf("a model event shows %q:\n%s", s, view)
		}
	}
	pressTrace(t, m, "esc")

	selectRow(t, m, "tool memory")
	pressTrace(t, m, "enter")
	view = traceScreen(m)
	if !strings.Contains(view, "what memory answered") {
		t.Errorf("the tool call does not show its own output:\n%s", view)
	}
	if strings.Contains(view, "the page the browser fetched") {
		t.Errorf("the tool call shows another call's output:\n%s", view)
	}
}

// --- the transcript rows -----------------------------------------------------

func TestTheMessagesAreInterleavedWithTheEvents(t *testing.T) {
	m := openOnTheWholeRun(t, twoSessions())

	var order []string
	for _, r := range m.evShown {
		if r.isMsg {
			order = append(order, "msg:"+string(r.msg.msg.Role))
			continue
		}
		order = append(order, r.ev.ev.Type)
	}
	want := strings.Join([]string{
		"channel.received", "msg:user", "run.started", "tool.call", "tool.result",
		"msg:tool", "tool.call", "msg:tool", "model.response", "guard.decision",
		"reply", "msg:assistant",
	}, ",")
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("rows =\n%s\nwant\n%s", got, want)
	}

	view := traceScreen(m)
	for _, s := range []string{"› how is the build", "‹ the build is green", "⚙ browser result"} {
		if !strings.Contains(view, s) {
			t.Errorf("the events column does not show %q:\n%s", s, view)
		}
	}
}

func TestTheMessageRowsCanBeHiddenAndBroughtBack(t *testing.T) {
	m := openOnTheWholeRun(t, twoSessions())
	with := len(m.evShown)

	pressTrace(t, m, "m")
	if m.showMsgs {
		t.Fatal("`m` did not hide the messages")
	}
	if len(m.evShown) != with-4 {
		t.Fatalf("%d rows with the messages hidden, want %d", len(m.evShown), with-4)
	}
	if strings.Contains(traceScreen(m), "› how is the build") {
		t.Errorf("a message row is still drawn:\n%s", traceScreen(m))
	}
	if !strings.Contains(m.status, "messages hidden") {
		t.Errorf("status = %q, want it to say what happened", m.status)
	}

	pressTrace(t, m, "m")
	if !m.showMsgs || len(m.evShown) != with {
		t.Fatalf("`m` did not bring them back: %d rows", len(m.evShown))
	}
}

func TestTheFilterReachesWhatWasSaid(t *testing.T) {
	m := openOnTheWholeRun(t, twoSessions())

	pressTrace(t, m, "/")
	pressTrace(t, m, "green")
	pressTrace(t, m, "enter")

	if len(m.evShown) != 1 || !m.evShown[0].isMsg {
		t.Fatalf("the filter kept %d rows, want the one message that says it", len(m.evShown))
	}
	if !strings.Contains(traceScreen(m), "the build is green") {
		t.Errorf("the message it matched is not drawn:\n%s", traceScreen(m))
	}
}

func TestAMessageOpensItsOwnDetails(t *testing.T) {
	m := openOnTheWholeRun(t, twoSessions())

	selectRow(t, m, "‹ the build is green")
	pressTrace(t, m, "enter")
	view := traceScreen(m)
	for _, s := range []string{"assistant message", "MA", "the build is green", "tool calls", "browser", "ci.example"} {
		if !strings.Contains(view, s) {
			t.Errorf("the message details do not show %q:\n%s", s, view)
		}
	}
	pressTrace(t, m, "esc")

	// A tool message says which call it answers, and that its text came from
	// somewhere untrusted.
	selectRow(t, m, "⚙ browser result")
	pressTrace(t, m, "enter")
	view = traceScreen(m)
	for _, s := range []string{"tool message", "c1", "tainted", "the page the browser fetched"} {
		if !strings.Contains(view, s) {
			t.Errorf("the tool message details do not show %q:\n%s", s, view)
		}
	}
	// `J` shows the message as it is stored.
	pressTrace(t, m, "J")
	if raw := traceScreen(m); !strings.Contains(raw, `"tool_call_id"`) {
		t.Errorf("`J` did not show the raw message:\n%s", raw)
	}
}

// --- the totals --------------------------------------------------------------

func TestTraceTotals(t *testing.T) {
	events := twoSessions().traces["T1"].Events
	got := traceTotals(events)
	if got.Wall != 3*time.Second {
		t.Errorf("wall = %s, want 3s", got.Wall)
	}
	if got.Model != 1200*time.Millisecond {
		t.Errorf("model = %s, want 1.2s", got.Model)
	}
	if got.Tool != 500*time.Millisecond {
		t.Errorf("tool = %s, want 500ms", got.Tool)
	}
	if got.Prompt != 1000 || got.Completion != 200 {
		t.Errorf("tokens = %d→%d, want 1000→200", got.Prompt, got.Completion)
	}
	if got.ModelCalls != 1 || got.ToolCalls != 2 {
		t.Errorf("calls = %d model / %d tool, want 1 / 2", got.ModelCalls, got.ToolCalls)
	}
	// The guard's decision is priced (a legacy USD cost); the model response
	// reported no price.
	if got.Cost == nil || *got.Cost != 3100000 || got.Unpriced != 1 {
		t.Errorf("cost = %v (%d unpriced), want the one billed decision and one unpriced call", got.Cost, got.Unpriced)
	}

	// Every model call counts once: the browser task's total with its chat
	// calls' cost, a compaction and a vision call add in; the move inside the
	// task, the run's own total and a cost-shaped key on an event that is not
	// a model call do not.
	events = append(events,
		gen.Event{Id: "F8", Type: string(event.BrowserMove), Time: t0.Add(3 * time.Second),
			Attrs: evAttrs(map[string]any{"prompt_tokens": float64(300), "completion_tokens": float64(30), "cost_nanousd": float64(300000)})},
		gen.Event{Id: "F9", Type: string(event.BrowserTaskFinished), Time: t0.Add(3 * time.Second),
			Attrs: evAttrs(map[string]any{"prompt_tokens": float64(900), "completion_tokens": float64(90), "cost": 0.0009})},
		gen.Event{Id: "FA", Type: string(event.ContextCompacted), Time: t0.Add(3 * time.Second),
			Attrs: evAttrs(map[string]any{"prompt_tokens": float64(2000), "completion_tokens": float64(100), "cost_nanousd": float64(2000000)})},
		gen.Event{Id: "FB", Type: string(event.MediaDescribed), Time: t0.Add(3 * time.Second),
			Attrs: evAttrs(map[string]any{"prompt_tokens": float64(500), "completion_tokens": float64(50), "cost_nanousd": float64(0)})},
		gen.Event{Id: "FC", Type: string(event.PolicyDecision), Time: t0.Add(3 * time.Second),
			Attrs: evAttrs(map[string]any{"cost_nanousd": float64(9)})},
		gen.Event{Id: "FD", Type: string(event.RunFinished), Time: t0.Add(4 * time.Second),
			Attrs: evAttrs(map[string]any{"prompt_tokens": float64(1000), "completion_tokens": float64(200), "cost_nanousd": float64(0)})})
	got = traceTotals(events)
	if got.Prompt != 1000+900+2000+500 || got.Completion != 200+90+100+50 {
		t.Errorf("tokens = %d→%d, want 4400→440", got.Prompt, got.Completion)
	}
	if got.Wall != 4*time.Second {
		t.Errorf("wall = %s, want 4s", got.Wall)
	}
	if want := provider.NanoUSD(3100000 + 900000 + 2000000); got.Cost == nil || *got.Cost != want || got.Unpriced != 1 {
		t.Errorf("cost = %v (%d unpriced), want %v", got.Cost, got.Unpriced, want)
	}

	// The strip and the store's sums are the same rule: whatever event.UsageOf
	// says of these events is what the strip says.
	var u event.Usage
	for _, ev := range events {
		if ev.Attrs != nil {
			u.Add(event.UsageOf(event.Type(ev.Type), *ev.Attrs))
		}
	}
	if u.PromptTokens != got.Prompt || u.CompletionTokens != got.Completion || *u.Cost != *got.Cost ||
		u.UnpricedCalls != got.Unpriced {
		t.Errorf("the strip says %+v, the rule %+v", got, u)
	}

	// Nothing in, nothing claimed.
	if zero := traceTotals(nil); zero != (traceSummaryTotals{}) {
		t.Errorf("an empty trace totals %+v", zero)
	}
}

func TestTheTotalsStripIsOverTheEvents(t *testing.T) {
	m := openOnTheWholeRun(t, twoSessions())
	view := traceScreen(m)
	for _, s := range []string{"wall 3.0s", "model 1.2s", "tool 0.5s", "1 model · 2 tools",
		"1 model · 2 tools · 1.0k→200 tokens · $0.0031+"} {
		if !strings.Contains(view, s) {
			t.Errorf("the totals strip does not say %q:\n%s", s, view)
		}
	}
	if strings.Contains(view, "decisions $") {
		t.Errorf("the strip still names the cost for the decisions alone:\n%s", view)
	}
	// The strip costs the events column two rows and nothing else: the screen
	// is still exactly the window.
	if lines := strings.Count(view, "\n") + 1; lines != m.height {
		t.Fatalf("the screen is %d rows tall, want the window's %d", lines, m.height)
	}
	// An event is one line and a session two, so the columns hold different
	// numbers of entries out of the same lines.
	if lines := m.bodyH - 3; m.rowsFor(paneEvents) != lines-trTotalsRows || m.rowsFor(paneSessions) != lines/2 {
		t.Errorf("the events column shows %d rows and the sessions one %d entries, out of %d lines",
			m.rowsFor(paneEvents), m.rowsFor(paneSessions), lines)
	}
}

func TestATraceRowCarriesItsWallTime(t *testing.T) {
	m := openTraces(t, twoSessions())
	if !strings.Contains(traceScreen(m), "7ev 2t 3.0s") {
		t.Errorf("the traces column does not carry the wall time:\n%s", traceScreen(m))
	}
	if got := shortDur(90 * time.Second); got != "1m30s" {
		t.Errorf("a long run reads %q", got)
	}
}

func TestAnEventsDetailsCarryItsTokensAndCost(t *testing.T) {
	m := openOnTheWholeRun(t, twoSessions())

	selectRow(t, m, "model")
	pressTrace(t, m, "enter")
	if view := traceScreen(m); !strings.Contains(view, "tokens 1.0k→200") {
		t.Errorf("the model response's details do not carry its tokens:\n%s", view)
	}
	pressTrace(t, m, "esc")

	selectRow(t, m, "guard")
	pressTrace(t, m, "enter")
	if view := traceScreen(m); !strings.Contains(view, "cost $0.0031") {
		t.Errorf("the billed decision's details do not carry its cost:\n%s", view)
	}
}

// --- the rest of the browser --------------------------------------------------

func TestAnAnswerFromAnEarlierRequestIsDroppedInTheTraceBrowser(t *testing.T) {
	m := openTraces(t, twoSessions())
	stale := trTracesMsg{seq: m.traceSeq - 1, traces: []gen.TraceSummary{
		{TraceId: "OLD", Preview: ptrOf("an answer nobody waited for")},
	}}
	m.Update(stale)
	if strings.Contains(traceScreen(m), "an answer nobody waited for") {
		t.Fatalf("a stale answer overwrote the column:\n%s", traceScreen(m))
	}

	staleEvents := trEventsMsg{seq: m.evSeq - 1, trace: gen.Trace{TraceId: "OLD", Events: []gen.Event{
		{Id: "X", Type: string(event.RunAborted), Time: t0},
	}}}
	m.Update(staleEvents)
	if strings.Contains(traceScreen(m), "run.aborted") {
		t.Fatalf("a stale trace overwrote the events:\n%s", traceScreen(m))
	}
	if !strings.Contains(traceScreen(m), "run.started") {
		t.Fatal("the current events are gone")
	}
}

func TestTheFilterNarrowsTheFocusedColumn(t *testing.T) {
	f := twoSessions()
	m := openTraces(t, f)

	pressTrace(t, m, "/")
	if !m.typing {
		t.Fatal("`/` did not open the filter box")
	}
	pressTrace(t, m, "scribe")
	pressTrace(t, m, "enter")

	if len(m.sessShown) != 1 || m.sessShown[0].Id != "S2" {
		t.Fatalf("the sessions column shows %+v, want the scribe one alone", m.sessShown)
	}
	if strings.Contains(traceScreen(m), "the deploy") {
		t.Errorf("the filtered-out session is still drawn:\n%s", traceScreen(m))
	}
	// Nothing was asked of the daemon again: the list was already here.
	if last := f.calls[len(f.calls)-1]; last != "trace:T2" {
		t.Errorf("the filter went over the wire: %v", f.calls)
	}

	pressTrace(t, m, "esc")
	if m.queries[paneSessions] != "" || len(m.sessShown) != 2 {
		t.Fatalf("escape left the filter running: %q, %d rows", m.queries[paneSessions], len(m.sessShown))
	}
}

func TestATraceRowSaysHowTheRunEnded(t *testing.T) {
	m := openTraces(t, twoSessions())
	view := traceScreen(m)
	// T1 replied, T2 is still being written.
	for _, s := range []string{"reply", "running"} {
		if !strings.Contains(view, s) {
			t.Errorf("the traces column does not say %q:\n%s", s, view)
		}
	}
	if word, _ := traceOutcome(string(event.RunAborted), newTUIStyles()); word != "abort" {
		t.Errorf("an aborted run reads %q", word)
	}
	if word, _ := traceOutcome("", newTUIStyles()); word != "" {
		t.Errorf("a trace with no last event reads %q", word)
	}
}

func TestTheStatusLineCarriesTheSelectedTraceId(t *testing.T) {
	m := openTraces(t, twoSessions())
	if !strings.Contains(traceScreen(m), "trace T2") {
		t.Errorf("the status line does not carry the id to pass to `trace <id>`:\n%s", traceScreen(m))
	}
}

func TestATraceThatCannotBeReadLandsInTheStatusLine(t *testing.T) {
	f := twoSessions()
	f.traceErr = errors.New("the database is locked")
	m := openTraces(t, f)

	if !m.isError || !strings.Contains(m.status, "the database is locked") {
		t.Fatalf("status = %q (error=%v), want the failure", m.status, m.isError)
	}
	if !strings.Contains(m.status, "trace:") {
		t.Errorf("the status line does not say what failed: %q", m.status)
	}
	// And the columns are still there to move around in.
	pressTrace(t, m, "j")
	if traceScreen(m) == "" {
		t.Fatal("the browser stopped drawing after an error")
	}
}

func TestADaemonThatCannotListTracesSaysSo(t *testing.T) {
	f := twoSessions()
	f.tracesErr = &api.ClientError{Status: http.StatusNotImplemented, Code: "not_implemented",
		Message: "trace listing is not available in this build"}
	m := openTraces(t, f)

	if !m.isError || !strings.Contains(m.status, "cannot list traces") {
		t.Fatalf("status = %q, want the not-implemented explanation", m.status)
	}
	if !strings.Contains(m.status, "trace <trace-id>") {
		t.Errorf("the message does not say what to do instead: %q", m.status)
	}
}

func TestTraceBrowserQuitLeaves(t *testing.T) {
	m := openTraces(t, twoSessions())
	_, cmd := m.Update(memKeyMsg("q"))
	if cmd == nil {
		t.Fatal("`q` issued no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("`q` did not quit")
	}
	if m.View() != "" {
		t.Error("the screen was not handed back on the way out")
	}
}

func TestAClickSelectsTheRowUnderThePointerAndTheWheelScrolls(t *testing.T) {
	f := twoSessions()
	m := openTraces(t, f)

	x := m.sessW + 2 // over the traces column
	// A trace is two lines: the third line under the heading is the second
	// entry's first.
	m.Update(tea.MouseMsg{X: x, Y: trRowTop + 2, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	if m.focus != paneTraces {
		t.Fatalf("focus = %v, want the traces column", m.focus)
	}
	if m.traceCur.at != 1 {
		t.Fatalf("selected row %d, want the second", m.traceCur.at)
	}

	// The wheel over a column focuses it and walks it; a column that is
	// already at its end stays there rather than wrapping.
	m.Update(tea.MouseMsg{X: 1, Y: trRowTop, Button: tea.MouseButtonWheelDown})
	if m.focus != paneSessions {
		t.Fatalf("focus = %v, want the sessions column", m.focus)
	}
	if m.sessCur.at != 1 {
		t.Fatalf("the wheel left the cursor at %d, want the last row", m.sessCur.at)
	}
}

func TestAClickOnAnEventOpensIt(t *testing.T) {
	f := twoSessions()
	m := openTraces(t, f)
	x := m.sessW + m.tracesW + 2
	// Past the heading and the totals strip: the second row of the events.
	y := trRowTop + m.headRows(paneEvents) + 1
	_, cmd := m.Update(tea.MouseMsg{X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	driveTraces(t, m, cmd)

	if m.focus != paneEvents {
		t.Fatalf("focus = %v, want the events column", m.focus)
	}
	if !m.showDet {
		t.Fatal("clicking an event did not open its details")
	}
	if m.evCur.at != 1 {
		t.Fatalf("the click landed on row %d, want the second", m.evCur.at)
	}
	if !strings.Contains(traceScreen(m), "model.request") {
		t.Errorf("the details are about another event:\n%s", traceScreen(m))
	}
}

func TestTheDetailsPanelFitsUnderTheColumns(t *testing.T) {
	m := openTraces(t, twoSessions())
	m.focus = paneEvents
	pressTrace(t, m, "enter")
	if lines := strings.Count(traceScreen(m), "\n") + 1; lines != m.height {
		t.Fatalf("the screen is %d rows tall, want the window's %d", lines, m.height)
	}
	if !strings.Contains(traceScreen(m), "details") {
		t.Fatal("the panel is not under the columns")
	}
}

// --- the command ---------------------------------------------------------------

func TestTraceBrowserNeedsATerminal(t *testing.T) {
	code, _, errOut := runCLI(t, "", "trace")
	if code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut, "needs a terminal") || !strings.Contains(errOut, "trace <trace-id>") {
		t.Fatalf("the hint does not point at the argument form:\n%s", errOut)
	}
}

func TestTraceUsageNamesTheOptionalId(t *testing.T) {
	code, out, _ := runCLI(t, "", "trace", "-h")
	if code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, s := range []string{"[<trace-id>]", "--addr"} {
		if !strings.Contains(out, s) {
			t.Errorf("`trace -h` does not mention %q:\n%s", s, out)
		}
	}
}

func TestTheStatusLineCountsEventsAndMessagesApart(t *testing.T) {
	m := openOnTheWholeRun(t, twoSessions())
	if !strings.Contains(traceScreen(m), "8 events · 4 messages") {
		t.Errorf("the status line does not count the two apart:\n%s", traceScreen(m))
	}
	// With the messages hidden the line is the toggle saying so; the counts
	// come back with the next thing that clears it.
	pressTrace(t, m, "m")
	m.clear()
	if !strings.Contains(traceScreen(m), "8 events · 0 messages") {
		t.Errorf("hiding the messages did not change the count:\n%s", traceScreen(m))
	}
}

func TestAMessageDetailShowsTheTaintScore(t *testing.T) {
	f := twoSessions()
	tr := f.traces["T1"]
	tr.Messages[1].TaintScore = ptrOf(float32(0.83))
	f.traces["T1"] = tr
	m := openOnTheWholeRun(t, f)

	selectRow(t, m, "⚙ browser result")
	pressTrace(t, m, "enter")
	if view := traceScreen(m); !strings.Contains(view, "taint 0.83") {
		t.Errorf("the details do not show the score next to the taint line:\n%s", view)
	}
	// A low score is dimmed rather than shouted, and an unscored message says
	// nothing at all.
	if !strings.Contains(stripStyle(messageDetailText(traceMsg{
		msg: gen.Message{Role: gen.MessageRoleTool, TaintScore: ptrOf(float32(0.12))}}, m.styles)), "taint 0.12") {
		t.Error("a low score is not shown")
	}
	if strings.Contains(messageDetailText(traceMsg{msg: gen.Message{Role: gen.MessageRoleUser}}, m.styles), "taint") {
		t.Error("an unscored message mentions a taint score")
	}
}

// --- the two-line entries -------------------------------------------------------

// A session and a trace are two lines: the second, dimmed, is what their
// model calls cost.
func TestSessionAndTraceEntriesCarryTheirUsage(t *testing.T) {
	m := openOnTheWholeRun(t, twoSessions())
	lines := strings.Split(traceScreen(m), "\n")
	under := func(what, want string) {
		t.Helper()
		for i, l := range lines {
			// Inside a column: the header names the selected session too.
			if strings.Contains(l, "│") && strings.Contains(l, what) && i+1 < len(lines) {
				if !strings.Contains(lines[i+1], want) {
					t.Errorf("the line under %q does not say %q:\n%s\n%s", what, want, l, lines[i+1])
				}
				return
			}
		}
		t.Errorf("no line says %q", what)
	}
	under("the deploy", "12.3k→1.1k tokens · $0.0042")
	// Part of the trace reported no price: the dollars are a floor, marked.
	under("how is the build", "1.0k→200 tokens · $0.0031+")
	// A trace with nothing reported says zero tokens and no dollars.
	under("and now", "0→0 tokens")
	if strings.Contains(traceScreen(m), "0→0 tokens · $") {
		t.Error("an unknown cost is drawn")
	}
}

func TestUsageLine(t *testing.T) {
	cases := []struct {
		p, c     int
		cost     *decimal.Decimal
		unpriced int
		want     string
	}{
		{12300, 1100, usd("0.0042"), 0, "12.3k→1.1k tokens · $0.0042"},
		{12300, 1100, usd("0.0042"), 3, "12.3k→1.1k tokens · $0.0042+"}, // some calls unpriced
		{500, 20, nil, 2, "500→20 tokens"},                              // none priced: unknown, not free
		{500, 20, usd("0"), 0, "500→20 tokens · $0.0000"},               // a free model
		{0, 0, nil, 0, "0→0 tokens"},
		{0, 0, usd("0.04"), 0, "0→0 tokens · $0.0400"},              // an image: dollars, no tokens
		{9, 1, usd("0.000012"), 0, "9→1 tokens · $0.000012"},        // too small for four decimals, not free
		{9, 1, usd("0.000000001"), 1, "9→1 tokens · $0.000000001+"}, // one nano-USD
		{9, 1, usd("0.00005"), 0, "9→1 tokens · $0.0001"},           // rounds half away from zero
		{9, 1, usd("0.12345"), 0, "9→1 tokens · $0.1235"},
	}
	for _, c := range cases {
		if got := usageLine(c.p, c.c, c.cost, c.unpriced); got != c.want {
			t.Errorf("usageLine(%d, %d, %v, %d) = %q, want %q", c.p, c.c, c.cost, c.unpriced, got, c.want)
		}
	}
	// A daemon that cannot total a session leaves its second line blank
	// rather than claiming it cost nothing.
	m := newTraceUI(context.Background(), &fakeTraces{}, "")
	if got := m.sessionUsageView(gen.Session{}, 40); got != "" {
		t.Errorf("an untotalled session reads %q", got)
	}
	if got := stripStyle(m.sessionUsageView(gen.Session{Cost: usd("0.5")}, 40)); got != "0→0 tokens · $0.5000" {
		t.Errorf("a partly totalled session reads %q", got)
	}
}

// usd is an API cost from its decimal string.
func usd(s string) *decimal.Decimal { return ptrOf(decimal.RequireFromString(s)) }

// manySessions is a column longer than the screen, every session costing
// something, so paging and the pointer have somewhere to go.
func manySessions(n int) *fakeTraces {
	f := &fakeTraces{byTrace: map[string][]gen.TraceSummary{}, traces: map[string]gen.Trace{}}
	for i := range n {
		f.sessions = append(f.sessions, gen.Session{
			Id: fmt.Sprintf("S%02d", i), Key: fmt.Sprintf("k%02d", i), Agent: "main", Kind: "chat",
			Title:        ptrOf(fmt.Sprintf("session %02d", i)),
			UpdatedAt:    t0.Add(-time.Duration(i) * time.Minute), // newest first keeps S00 on top
			PromptTokens: ptrOf(i * 100), CompletionTokens: ptrOf(i), Cost: usd("0"),
		})
	}
	return f
}

// The geometry of two-line entries: a page is half as many sessions as there
// are lines, the cursor never leaves the screen, and what is drawn fits the
// pane exactly.
func TestTwoLineEntriesPageAndScroll(t *testing.T) {
	m := openTraces(t, manySessions(60))
	per := m.rowsFor(paneSessions)
	if per != (m.bodyH-3)/2 {
		t.Fatalf("%d sessions per page out of %d lines", per, m.bodyH-3)
	}
	check := func(what string) {
		t.Helper()
		if lines := strings.Count(traceScreen(m), "\n") + 1; lines != m.height {
			t.Fatalf("%s: the screen is %d rows tall, want %d", what, lines, m.height)
		}
		c := m.sessCur
		if c.at < c.off || c.at >= c.off+per {
			t.Fatalf("%s: the cursor %d is off the screen (offset %d, %d per page)", what, c.at, c.off, per)
		}
		first := fmt.Sprintf("session %02d", c.off)
		last := fmt.Sprintf("session %02d", c.off+per-1)
		past := fmt.Sprintf("session %02d", c.off+per)
		view := traceScreen(m)
		if !strings.Contains(view, first) || !strings.Contains(view, last) || strings.Contains(view, past) {
			t.Fatalf("%s: the column does not show entries %d…%d exactly:\n%s", what, c.off, c.off+per-1, view)
		}
	}
	check("opened")

	pressTrace(t, m, "pgdown")
	if m.sessCur.at != per {
		t.Fatalf("a page down moved to %d, want %d", m.sessCur.at, per)
	}
	check("after a page down")

	pressTrace(t, m, "G")
	if m.sessCur.at != 59 || m.sessCur.off != 60-per {
		t.Fatalf("the end is at %d (offset %d), want 59 (offset %d)", m.sessCur.at, m.sessCur.off, 60-per)
	}
	check("at the end")

	pressTrace(t, m, "pgup")
	if m.sessCur.at != 59-per {
		t.Fatalf("a page up moved to %d, want %d", m.sessCur.at, 59-per)
	}
	check("after a page up")
}

// Either line of an entry is that entry, under any scroll offset, and the
// lines past the last whole entry are not.
func TestAPointerHitsBothLinesOfAnEntry(t *testing.T) {
	m := openTraces(t, manySessions(60))
	pressTrace(t, m, "G") // scrolled: the offset is not zero
	off := m.sessCur.off
	top := trRowTop + m.headRows(paneSessions)
	for _, c := range []struct {
		y    int
		want int
		ok   bool
	}{
		{top - 1, 0, false}, // the heading
		{top, off, true},
		{top + 1, off, true},
		{top + 2, off + 1, true},
		{top + 5, off + 2, true},
		{top + 2*m.rowsFor(paneSessions) - 1, off + m.rowsFor(paneSessions) - 1, true},
		{top + 2*m.rowsFor(paneSessions), 0, false}, // past the last entry
	} {
		i, ok := m.rowAt(paneSessions, c.y)
		if ok != c.ok || (ok && i != c.want) {
			t.Errorf("row at y=%d = %d, %v; want %d, %v", c.y, i, ok, c.want, c.ok)
		}
	}

	// And a click on an entry's second line selects it.
	pressTrace(t, m, "g")
	_, cmd := m.Update(tea.MouseMsg{X: 1, Y: top + 3, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	driveTraces(t, m, cmd)
	if m.sessCur.at != 1 {
		t.Fatalf("a click on the second entry's usage line selected %d", m.sessCur.at)
	}
	// Only a two-line column halves: the events column is one line an event.
	if trEntryLines(paneEvents) != 1 || trEntryLines(paneTraces) != 2 {
		t.Error("entry heights changed")
	}
}

// The selected entry is marked once, on its first line; the line under it is
// highlighted with it but carries no cursor of its own.
func TestASelectedEntryIsMarkedOnce(t *testing.T) {
	m := openTraces(t, twoSessions())
	got := stripStyle(m.trEntryView("first", "second", true, true))
	if got != "›first\n second" {
		t.Errorf("selected entry = %q", got)
	}
	if got := stripStyle(m.trEntryView("first", "second", false, true)); got != " first\n second" {
		t.Errorf("unselected entry = %q", got)
	}
	if n := strings.Count(stripStyle(m.sessionsPaneView()), "›"); n != 1 {
		t.Errorf("the sessions column carries %d marks, want 1", n)
	}
}

// Every event that records model calls shows its tokens and cost in the
// details panel, the browser's old output_tokens included. A reported zero
// cost is drawn (a free call); a missing one is not (an unpriced call); the
// cached and reasoning shares are drawn when reported.
func TestEventCostLinesForEveryModelCall(t *testing.T) {
	s := newTUIStyles()
	for _, c := range []struct {
		typ   event.Type
		attrs map[string]any
		want  []string
	}{
		{event.GuardDecision, map[string]any{"prompt_tokens": float64(50), "completion_tokens": float64(1), "cost": 0.0001},
			[]string{"tokens 50→1", "cost $0.0001"}},
		{event.BrowserTaskFinished, map[string]any{"prompt_tokens": float64(2500), "completion_tokens": float64(90), "cost_nanousd": float64(2000000)},
			[]string{"tokens 2.5k→90", "cost $0.0020"}},
		{event.BrowserTaskFinished, map[string]any{"prompt_tokens": float64(2500), "completion_tokens": float64(90), "cost_nanousd": float64(2000000),
			"unpriced_calls": float64(2)}, []string{"cost $0.0020+"}},
		{event.BrowserTaskFinished, map[string]any{"prompt_tokens": float64(10), "output_tokens": float64(5)},
			[]string{"tokens 10→5"}},
		{event.ContextCompacted, map[string]any{"prompt_tokens": float64(3000), "completion_tokens": float64(200), "cost_nanousd": float64(3000000)},
			[]string{"tokens 3.0k→200", "cost $0.0030"}},
		{event.MediaDescribed, map[string]any{"prompt_tokens": float64(700), "completion_tokens": float64(60), "cost_nanousd": float64(0)},
			[]string{"tokens 700→60", "cost $0.0000"}},
		{event.MemoryEmbedded, map[string]any{"prompt_tokens": float64(9), "completion_tokens": float64(0), "cost_nanousd": float64(1000)},
			[]string{"tokens 9→0", "cost $0.000001"}},
		{event.ModelResponse, map[string]any{"prompt_tokens": float64(2000), "completion_tokens": float64(300),
			"cached_tokens": float64(1500), "reasoning_tokens": float64(0)},
			[]string{"tokens 2.0k→300 (cached 1.5k, reasoning 0)"}},
	} {
		got := stripStyle(eventCostLines(gen.Event{Type: string(c.typ), Attrs: evAttrs(c.attrs)}, s))
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q does not say %q", c.typ, got, w)
			}
		}
		_, legacy := c.attrs["cost"]
		if _, priced := c.attrs["cost_nanousd"]; !priced && !legacy && strings.Contains(got, "cost") {
			t.Errorf("%s: a missing cost is drawn: %q", c.typ, got)
		}
	}
	if got := eventCostLines(gen.Event{Type: string(event.RunStarted)}, s); got != "" {
		t.Errorf("an event with no usage has cost lines: %q", got)
	}
}

// Every spelling of a motion moves a column the same way, and none of them
// goes past either end.
func TestEveryPagingSpellingMovesTheColumn(t *testing.T) {
	m := openTraces(t, manySessions(60))
	per := m.rowsFor(paneSessions)
	half := halfPage(m.rowsPerPane())
	for _, tc := range []struct {
		from string // the end the cursor starts on
		keys []string
		want int
	}{
		{"alt+<", []string{"pgdown", "ctrl+f", "ctrl+v"}, per},
		{"alt+>", []string{"pgup", "ctrl+b", "alt+v"}, 59 - per},
		{"alt+<", []string{"ctrl+d"}, half},
		{"alt+>", []string{"ctrl+u"}, 59 - half},
		{"alt+<", []string{"down", "j", "ctrl+n"}, 1},
		{"alt+>", []string{"up", "k", "ctrl+p"}, 58},
		{"alt+>", []string{"g", "home", "alt+<"}, 0},
		{"alt+<", []string{"G", "end", "alt+>"}, 59},
		// At an end, a motion past it stays there.
		{"alt+<", []string{"pgup", "ctrl+b", "alt+v", "ctrl+u", "ctrl+p"}, 0},
		{"alt+>", []string{"pgdown", "ctrl+f", "ctrl+v", "ctrl+d", "ctrl+n"}, 59},
	} {
		for _, k := range tc.keys {
			pressTrace(t, m, tc.from)
			pressTrace(t, m, k)
			if m.sessCur.at != tc.want {
				t.Errorf("%s from %s left the cursor on %d, want %d", k, tc.from, m.sessCur.at, tc.want)
			}
			if c := m.sessCur; c.at < c.off || c.at >= c.off+per {
				t.Errorf("%s from %s left the cursor %d off the screen (offset %d)", k, tc.from, c.at, c.off)
			}
		}
	}
}
