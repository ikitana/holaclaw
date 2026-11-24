package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
)

// progEvent builds the one shape the formatter reads.
func progEvent(t event.Type, attrs map[string]any) gen.Event {
	ev := gen.Event{Type: string(t)}
	if attrs != nil {
		ev.Attrs = &attrs
	}
	return ev
}

func withDuration(ev gen.Event, d time.Duration) gen.Event {
	ns := int64(d)
	ev.DurationNs = &ns
	return ev
}

// clockOf is the wall clock the formatter prints for a timestamp attribute.
func clockOf(t *testing.T, stamp string) string {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		t.Fatalf("parse %s: %v", stamp, err)
	}
	return ts.Local().Format("15:04:05")
}

func TestProgressLine(t *testing.T) {
	const expires = "2026-09-19T09:05:14Z"
	long := strings.Repeat("a", 100)

	for _, tc := range []struct {
		name string
		ev   gen.Event
		want string
	}{
		{"model request", progEvent(event.ModelRequest, map[string]any{
			"step": 1, "model": "cloud", "model_id": "deepseek/deepseek-v4.1-flash",
		}), "model step 1 → cloud (deepseek/deepseek-v4.1-flash)"},
		{"model request without an id", progEvent(event.ModelRequest, map[string]any{
			"step": 2, "model": "cloud",
		}), "model step 2 → cloud"},
		{"model fallback", progEvent(event.ModelFallback, map[string]any{
			"from": "cloud", "to": "local", "reason": "rate_limit",
		}), "model fallback cloud → local (rate_limit)"},
		{"model usage limit", progEvent(event.ModelUsageLimit, map[string]any{
			"provider": "openrouter", "model": "cloud", "reset_at": expires,
		}), "model usage limit openrouter (cloud), resets " + clockOf(t, expires)},
		{"run step limit", progEvent(event.RunStepLimit, map[string]any{
			"steps": 12, "limit": 12, "reason": "max_steps",
		}), "run: step limit, 12 steps (max_steps)"},
		{"run loop detected", progEvent(event.RunLoopDetected, map[string]any{
			"tool": "browser.do", "count": 3,
		}), "run: loop detected browser.do ×3"},

		{"guard decision", progEvent(event.GuardDecision, map[string]any{
			"decision": "allow", "reason": "benign", "score": 0.93,
		}), "guard allow (benign 0.93)"},
		{"guard decision without a reason", progEvent(event.GuardDecision, map[string]any{
			"decision": "deny", "score": 0.12,
		}), "guard deny (0.12)"},
		{"policy ask", progEvent(event.PolicyDecision, map[string]any{
			"decision": "ask", "tool": "files.write", "reason": "write",
		}), "policy ask: write"},
		{"policy deny", progEvent(event.PolicyDecision, map[string]any{
			"decision": "deny", "reason": "class",
		}), "policy deny: class"},
		{"policy allow says nothing", progEvent(event.PolicyDecision, map[string]any{
			"decision": "allow", "reason": "class",
		}), ""},

		{"tool call", progEvent(event.ToolCallEvent, map[string]any{
			"tool": "browser.do", "args": `{"intent":"Go to Google Flights","url":"https://x"}`,
		}), "tool browser.do  intent: Go to Google Flights"},
		{"tool call skips a non-string field", progEvent(event.ToolCallEvent, map[string]any{
			"tool": "memory.search", "args": `{"limit":5,"query":"hola"}`,
		}), "tool memory.search  query: hola"},
		{"tool call without a string field", progEvent(event.ToolCallEvent, map[string]any{
			"tool": "memory.search", "args": `{"limit":5}`,
		}), `tool memory.search  {"limit":5}`},
		{"tool call with no arguments", progEvent(event.ToolCallEvent, map[string]any{
			"tool": "status", "args": "{}",
		}), "tool status"},
		{"tool call arguments are capped", progEvent(event.ToolCallEvent, map[string]any{
			"tool": "browser.do", "args": `{"intent":"` + long + `"}`,
		}), "tool browser.do  intent: " + strings.Repeat("a", 72) + "…"},
		{"tool result", withDuration(progEvent(event.ToolResult, map[string]any{
			"tool": "browser.do", "is_error": false, "tainted": true,
		}), 65*time.Second), "tool browser.do → ok in 65.0s (tainted)"},
		{"tool result error", withDuration(progEvent(event.ToolResult, map[string]any{
			"tool": "files.write", "is_error": true, "error": "permission denied",
		}), 1200*time.Millisecond), "tool files.write → error in 1.2s: permission denied"},

		{"browser task started", progEvent(event.BrowserTaskStarted, map[string]any{
			"agent": "main", "detached": false, "decider": "decision",
		}), "browser: task started (decider: decision)"},
		{"browser task started after a queue", progEvent(event.BrowserTaskStarted, map[string]any{
			"agent": "main", "detached": false, "decider": "chat", "queued_ms": 56000,
		}), "browser: task started (decider: chat) after 56.0s in the queue"},
		{"browser run queued", progEvent(event.BrowserStep, map[string]any{
			"held": "queued", "why": "another_run",
		}), "browser: waiting for another run of this agent to give the browser back"},
		{"browser move", progEvent(event.BrowserMove, map[string]any{
			"n": 1, "moves": []any{map[string]any{"action": "click", "target": `r5 "Buscar"`}},
		}), "browser: move 1, 1 queued"},
		{"browser move with cost", progEvent(event.BrowserMove, map[string]any{
			"n": 2, "moves": []any{
				map[string]any{"action": "type", "target": `r1 "Origen"`},
				map[string]any{"action": "click", "target": `r5 "Buscar"`},
			},
			"took_ms": 2100, "prompt_tokens": 300, "output_tokens": 412,
		}), "browser: move 2, 2 queued · 2.1s · 300→412 tokens"},
		{"browser move done", progEvent(event.BrowserMove, map[string]any{
			"n": 3, "done": true, "took_ms": 900,
		}), "browser: move 3, done · 0.9s"},
		{"browser move blocked", progEvent(event.BrowserMove, map[string]any{
			"n": 4, "blocked": "login", "dropped": 2,
		}), "browser: move 4, needs a person: login, 2 dropped"},
		{"browser step", progEvent(event.BrowserStep, map[string]any{
			"n": 1, "action": "click", "ref": "r5", "role": "button",
			"effect_p": 0.91, "goal_p": 0.12, "took_ms": 480,
		}), "browser: step 1 click r5 button · effect 0.91 · goal 0.12 · 480ms"},
		{"browser step that changed page", progEvent(event.BrowserStep, map[string]any{
			"n": 2, "action": "type", "ref": "r1", "effect_p": 0.8, "goal_p": 0.9,
			"risk": "none", "rung": 3, "took_ms": 120, "url": "https://x.example/results",
		}), "browser: step 2 type r1 · effect 0.80 · goal 0.90 · rung 3 · 120ms → https://x.example/results"},
		{"browser step with a risk", progEvent(event.BrowserStep, map[string]any{
			"n": 3, "action": "click", "ref": "r9", "effect_p": 0.5, "goal_p": 0.1,
			"risk": "purchase", "took_ms": 700,
		}), "browser: step 3 click r9 · effect 0.50 · goal 0.10 · risk purchase · 700ms"},
		{"browser blocked", progEvent(event.BrowserBlocked, map[string]any{
			"why": "approval_refused",
		}), "browser: blocked (approval_refused)"},
		{"browser blocked by a wall", progEvent(event.BrowserBlocked, map[string]any{
			"why": "login",
		}), "browser: blocked (login)"},
		{"browser takeover requested", progEvent(event.BrowserTakeoverRequest, map[string]any{
			"agent": "main", "why": "error", "expires_at": expires,
		}), "browser: takeover requested (error) — open /v1/browser/viewer to drive it, expires " + clockOf(t, expires)},
		{"browser takeover started", progEvent(event.BrowserTakeoverStarted, map[string]any{
			"agent": "main", "user_id": "u1",
		}), "browser: takeover started"},
		{"browser takeover ended", progEvent(event.BrowserTakeoverEnded, map[string]any{
			"agent": "main", "user_id": "u1",
		}), "browser: takeover ended"},
		{"browser screenshot", progEvent(event.BrowserScreenshot, map[string]any{
			"why": "return", "size": 2048, "took_ms": 120, "url": "https://shop.example/", "attached": true,
		}), "browser: screenshot (return) · 2.0 kB · 120ms · attached"},
		{"browser screenshot not attached", progEvent(event.BrowserScreenshot, map[string]any{
			"why": "look", "size": 2048, "took_ms": 80, "attached": false,
		}), "browser: screenshot (look) · 2.0 kB · 80ms"},
		{"browser download", progEvent(event.BrowserDownload, map[string]any{
			"agent": "main", "name": "tickets.pdf", "size": 2048,
		}), "browser: download tickets.pdf (2.0 kB)"},
		{"browser task finished", progEvent(event.BrowserTaskFinished, map[string]any{
			"outcome": "done", "steps": 6, "generative_calls": 3, "decision_calls": 9,
			"replans": 1, "prompt_tokens": 900, "output_tokens": 120,
			"cost_nanousd": 1600000, "answer": true, "took_ms": 65000,
		}), "browser: done in 65s — 6 steps, 3 model calls, 9 decisions, $0.0016"},
		{"browser task finished before cost_nanousd", progEvent(event.BrowserTaskFinished, map[string]any{
			"outcome": "done", "steps": 6, "generative_calls": 3, "decision_calls": 9, "cost": 0.0016, "took_ms": 65000,
		}), "browser: done in 65s — 6 steps, 3 model calls, 9 decisions, $0.0016"},
		{"browser task finished without a cost", progEvent(event.BrowserTaskFinished, map[string]any{
			"outcome": "blocked", "steps": 2, "generative_calls": 1, "decision_calls": 2, "took_ms": 4200,
		}), "browser: blocked in 4.2s — 2 steps, 1 model calls, 2 decisions"},
		{"browser task finished with the rung histogram", progEvent(event.BrowserTaskFinished, map[string]any{
			"outcome": "done", "steps": 6, "generative_calls": 2, "decision_calls": 6, "took_ms": 18000,
			"rungs": []any{0, 0, 5, 1, 0, 0, 0, 0}, "extract_took_ms": 2100,
		}), "browser: done in 18s — 6 steps, 2 model calls, 6 decisions, rungs 2:5 3:1, extract 2.1s"},

		// Nothing a model or a page wrote ever reaches a line.
		{"tokens say nothing", progEvent(event.ModelToken, map[string]any{"delta": "hola"}), ""},
		{"a reply says nothing", progEvent(event.Reply, map[string]any{"text_bytes": 12}), ""},
		{"an unknown type says nothing", progEvent(event.ChannelReceived, map[string]any{"channel": "cli"}), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := progressLine(tc.ev, 2); got != tc.want {
				t.Errorf("progressLine =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// TestProgressLineWithoutAttributes is the missing-attribute path: a line is
// still printed, with the parts it has.
func TestProgressLineWithoutAttributes(t *testing.T) {
	for _, tc := range []struct {
		ev   gen.Event
		want string
	}{
		{progEvent(event.ModelRequest, nil), "model"},
		{progEvent(event.BrowserMove, nil), "browser: move"},
		{progEvent(event.BrowserStep, nil), "browser: step"},
		{progEvent(event.BrowserTaskFinished, nil), "browser: finished"},
		{progEvent(event.ToolResult, nil), "tool  → ok"},
	} {
		if got := progressLine(tc.ev, 2); got != tc.want {
			t.Errorf("progressLine(%s) = %q, want %q", tc.ev.Type, got, tc.want)
		}
	}
}

// A browser run waiting for another run of its agent holds the turn up, so it
// is shown with no -v at all; the steps of a run that is going are not.
func TestProgressShowsAQueuedRunByDefault(t *testing.T) {
	queued := progEvent(event.BrowserStep, map[string]any{"held": "queued", "why": "another_run"})
	if got := progressLine(queued, 0); got == "" {
		t.Error("a queued run is hidden at the default verbosity")
	}
	step := progEvent(event.BrowserStep, map[string]any{"n": 1, "action": "click", "ref": "r5"})
	if got := progressLine(step, 0); got != "" {
		t.Errorf("an ordinary step is shown at the default verbosity: %q", got)
	}
}

// TestProgressLayout pins the shape of a printed line: the dot, the elapsed
// seconds right-aligned in six columns, then the line.
func TestProgressLayout(t *testing.T) {
	var out strings.Builder
	p := newProgress(&out, false, 0)
	p.start = time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)

	for step, at := range []time.Duration{600 * time.Millisecond, 5200 * time.Millisecond, 70100 * time.Millisecond} {
		ev := progEvent(event.ModelRequest, map[string]any{"step": step + 1, "model": "cloud"})
		ev.Time = p.start.Add(at)
		p.event(ev)
	}
	want := "·   0.6s model step 1 → cloud\n" +
		"·   5.2s model step 2 → cloud\n" +
		"·  70.1s model step 3 → cloud\n"
	if out.String() != want {
		t.Errorf("progress =\n%q\nwant\n%q", out.String(), want)
	}
}

// TestProgressSkipsARepeatedLine: the gate decides the same thing at several
// steps of one call, and the terminal is told once.
func TestProgressSkipsARepeatedLine(t *testing.T) {
	var out strings.Builder
	p := newProgress(&out, false, 0)
	p.start = time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		ev := progEvent(event.PolicyDecision, map[string]any{"decision": "ask", "reason": "write"})
		ev.Time = p.start.Add(time.Second)
		p.event(ev)
	}
	if want := "·   1.0s policy ask: write\n"; out.String() != want {
		t.Errorf("progress = %q, want %q", out.String(), want)
	}
}

// TestProgressQuietPrintsNothing is the -q contract at the formatter level;
// the command-level one is TestChatQuietPrintsNothingButTheReply.
func TestProgressQuietPrintsNothing(t *testing.T) {
	var out strings.Builder
	p := newProgress(&out, true, 3)
	ev := progEvent(event.BrowserStep, map[string]any{"n": 1, "kind": "open"})
	ev.Time = p.start.Add(time.Second)
	p.event(ev)
	if out.String() != "" {
		t.Errorf("quiet progress wrote %q", out.String())
	}
}

// --- the move block ---------------------------------------------------------

// moveEvent is a browser.move as internal/browser writes it, queue and all.
func moveEvent(moves []any) gen.Event {
	return progEvent(event.BrowserMove, map[string]any{"n": 1, "moves": moves})
}

func TestProgressMoveBlock(t *testing.T) {
	ev := moveEvent([]any{
		map[string]any{
			"action": "navigate", "target": "https://www.google.com/travel/flights",
			"expect": "the flights page is open",
		},
		map[string]any{
			"action": "type", "target": `r1 "Where from?"`,
			"expect": "a list of airports drops down",
		},
	})
	if got := progressLine(ev, 2); got != "browser: move 1, 2 queued" {
		t.Errorf("move line = %q", got)
	}
	want := []string{
		"1. " + pad("navigate https://www.google.com/travel/flights", 47) + " expect: the flights page is open",
		"2. " + pad(`type r1 "Where from?"`, 47) + " expect: a list of airports drops down",
	}
	got := progressExtra(ev, 2)
	if len(got) != len(want) {
		t.Fatalf("move block =\n%q\nwant\n%q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("move %d = %q, want %q", i+1, got[i], want[i])
		}
	}
}

// A move event with nothing queued — done, or a page nothing could be done on
// — has no block under it.
func TestProgressMoveWithoutAQueue(t *testing.T) {
	if got := progressExtra(progEvent(event.BrowserMove, map[string]any{"n": 2, "done": true}), 2); len(got) != 0 {
		t.Errorf("progressExtra = %q, want nothing", got)
	}
	if got := progressExtra(progEvent(event.BrowserStep, map[string]any{"n": 2}), 2); got != nil {
		t.Errorf("a step printed a block: %q", got)
	}
}

// The block is printed under the line it belongs to, indented.
func TestProgressPrintsTheMoveBlock(t *testing.T) {
	var out strings.Builder
	p := newProgress(&out, false, 2)
	p.start = time.Date(2026, 9, 19, 9, 0, 0, 0, time.UTC)
	ev := moveEvent([]any{map[string]any{"action": "click", "target": `r2 "Search"`}})
	ev.Time = p.start.Add(2 * time.Second)
	p.event(ev)

	want := "·   2.0s browser: move 1, 1 queued\n" +
		progressIndent + `1. click r2 "Search"` + "\n"
	if out.String() != want {
		t.Errorf("progress =\n%q\nwant\n%q", out.String(), want)
	}
}

// TestProgressEndedAtLine: where a browser task ended up, under the finished
// line — the title and the URL, which is what a person wants to know when a
// task failed.
func TestProgressEndedAtLine(t *testing.T) {
	ev := progEvent(event.BrowserTaskFinished, map[string]any{
		"outcome": "failed", "took_ms": 1000,
		"title": "Results", "url": "https://x.example/results",
	})
	got := progressExtra(ev, 2)
	if len(got) != 1 || got[0] != "browser: ended at Results — https://x.example/results" {
		t.Fatalf("ended-at block = %q", got)
	}
	if got := progressExtra(progEvent(event.BrowserTaskFinished, map[string]any{"outcome": "failed"}), 2); got != nil {
		t.Errorf("a finished task with nowhere to name printed %q", got)
	}
}

// --- the info line ------------------------------------------------------------

func TestInfoLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		st    turnStats
		d     time.Duration
		trace string
		want  string
	}{
		{"the usual turn", turnStats{model: "cloud", prompt: 1600, completion: 218, toolCalls: 1},
			12400 * time.Millisecond, "", "· 12.4s · cloud · 1.6k→218 tokens · 1 tool call"},
		{"several tools", turnStats{model: "cloud", prompt: 120, completion: 8, toolCalls: 3},
			2 * time.Second, "", "· 2.0s · cloud · 120→8 tokens · 3 tool calls"},
		{"no tools and no tokens", turnStats{model: "local"}, 900 * time.Millisecond, "",
			"· 0.9s · local"},
		{"a fallback says so", turnStats{model: "local", fallback: "local", prompt: 1000, completion: 2000},
			3 * time.Second, "", "· 3.0s · local · 1.0k→2.0k tokens · via fallback local"},
		{"with a trace", turnStats{model: "cloud", prompt: 10, completion: 20, toolCalls: 1},
			time.Second, "01ARZ3", "· 1.0s · cloud · 10→20 tokens · 1 tool call · trace 01ARZ3"},
		{"nothing known at all", turnStats{}, time.Second, "", "· 1.0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := infoLine(tc.st, tc.d, tc.trace); got != tc.want {
				t.Errorf("infoLine =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// The totals are summed from the events, quiet or not — and --quiet still
// prints no line.
func TestProgressCountsATurn(t *testing.T) {
	var out strings.Builder
	p := newProgress(&out, false, 2)
	for _, ev := range []gen.Event{
		progEvent(event.RunStarted, map[string]any{"model": "cloud"}),
		progEvent(event.ToolCallEvent, map[string]any{"tool": "browser.do"}),
		progEvent(event.ModelResponse, map[string]any{
			"model": "cloud", "prompt_tokens": 1000, "completion_tokens": 100,
		}),
		progEvent(event.ModelResponse, map[string]any{
			"model": "cloud", "prompt_tokens": 600, "completion_tokens": 118,
		}),
	} {
		p.event(ev)
	}
	if p.stats != (turnStats{model: "cloud", prompt: 1600, completion: 218, toolCalls: 1}) {
		t.Errorf("stats = %+v", p.stats)
	}
	// run.finished carries the run's own totals and wins.
	p.event(progEvent(event.RunFinished, map[string]any{
		"prompt_tokens": 1700, "completion_tokens": 220,
	}))
	if p.stats.prompt != 1700 || p.stats.completion != 220 {
		t.Errorf("stats after run.finished = %+v", p.stats)
	}
	if line := p.info(""); !strings.Contains(line, "1.7k→220 tokens") {
		t.Errorf("info = %q", line)
	}

	q := newProgress(&strings.Builder{}, true, 2)
	q.event(progEvent(event.ToolCallEvent, map[string]any{"tool": "files.write"}))
	if q.stats.toolCalls != 1 {
		t.Error("quiet stopped the totals being counted")
	}
	if got := q.info(""); got != "" {
		t.Errorf("quiet info = %q", got)
	}
}

// --- the spinner ----------------------------------------------------------------

// syncBuf is a writer two goroutines share: the spinner draws while the test
// reads.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The spinner lives in the prompt area and gets out of the way: a progress
// line is never printed over a frame.
func TestSpinnerErasesItselfBeforeAProgressLine(t *testing.T) {
	out := &syncBuf{}
	p := newProgress(out, false, 2)
	p.tty, p.tick = true, time.Millisecond // a terminal, simulated
	p.begin()
	p.spin()

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), "thinking…") {
		if time.Now().After(deadline) {
			t.Fatalf("no spinner frame was drawn: %q", out.String())
		}
		time.Sleep(time.Millisecond)
	}
	p.event(progEvent(event.ToolCallEvent, map[string]any{"tool": "files.write"}))
	p.stopSpin()

	got := out.String()
	erase := "\r" + strings.Repeat(" ", len([]rune("⠋ thinking…"))) + "\r"
	i, j := strings.Index(got, erase), strings.Index(got, "tool files.write")
	if i < 0 {
		t.Fatalf("the frame was never erased: %q", got)
	}
	if j < 0 || i > j {
		t.Errorf("the line was printed before the erase: %q", got)
	}
	if !strings.HasSuffix(got, "tool files.write\n") {
		t.Errorf("a frame outlived the line: %q", got)
	}
}

// Nothing is drawn where nobody can erase it: a pipe, a file, --quiet.
func TestSpinnerOnlyDrawsOnATerminal(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tty, quiet bool
	}{
		{"a pipe", false, false},
		{"quiet on a terminal", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &syncBuf{}
			p := newProgress(out, tc.quiet, 0)
			p.tty, p.tick = tc.tty, time.Millisecond
			p.spin()
			time.Sleep(10 * time.Millisecond)
			p.stopSpin()
			if got := out.String(); got != "" {
				t.Errorf("the spinner wrote %q", got)
			}
		})
	}
}
