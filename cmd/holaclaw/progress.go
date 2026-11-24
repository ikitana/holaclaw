package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// progress is what `chat` shows while a turn runs: one short line per trace
// event on stderr, so a two-minute browser task is not a silent cursor. The
// reply keeps stdout to itself.
//
// Only attributes are printed — never an event's text or delta, and never
// anything a page wrote: browser events carry no page content by design, and
// the one string a tool can put in a line (its error) is capped.
type progress struct {
	mu    sync.Mutex
	w     io.Writer
	t     *theme
	off   bool
	tty   bool // a spinner is only worth drawing where it can be erased
	start time.Time
	last  string // the line just printed, so a repeat is not printed twice
	stats turnStats

	// verbosity is 0 by default (model steps and blocking events only), up to
	// 3 for -vvv (also the full text of every tool result, printed
	// separately — see printToolOutputs in chatplain.go).
	verbosity int

	// The spinner: one goroutine drawing a frame in the prompt area, and the
	// width of the frame that is on screen right now, so it can be wiped.
	tick  time.Duration
	stopc chan struct{}
	donec chan struct{}
	shown int
}

// turnStats is what one turn cost, summed from the events as they arrive:
// the info line under the reply is written from this and nothing else.
type turnStats struct {
	model      string
	fallback   string // the model a fallback moved to, if one did
	prompt     int    // prompt tokens
	completion int    // completion tokens
	toolCalls  int
}

func newProgress(w io.Writer, quiet bool, verbosity int) *progress {
	return &progress{
		w: w, t: newTheme(w), off: quiet, tty: isTerminal(w),
		start: time.Now(), tick: 100 * time.Millisecond, verbosity: verbosity,
	}
}

// begin restarts the clock: the number on a line is seconds since the turn
// was sent, not since the process started. It also drops the last turn's
// totals.
func (p *progress) begin() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.start, p.last, p.stats = time.Now(), "", turnStats{}
}

// event prints one line for ev, or nothing when its type has no line. Every
// event counts towards the turn's totals, quiet or not.
func (p *progress) event(ev gen.Event) {
	if p == nil {
		return
	}
	p.count(ev)
	if p.off {
		return
	}
	line := progressLine(ev, p.verbosity)
	if line == "" {
		return
	}
	// The first line of a turn is the answer the spinner was standing in for.
	p.stopSpin()
	p.mu.Lock()
	defer p.mu.Unlock()
	if line == p.last {
		// The gate decides the same thing at several steps of one call; the
		// terminal only needs to be told once.
		return
	}
	p.last = line
	d := ev.Time.Sub(p.start)
	if ev.Time.IsZero() || d < 0 {
		d = time.Since(p.start)
	}
	p.eraseLocked()
	_, _ = fmt.Fprintf(p.w, "%s %s\n",
		p.t.style(styleDim, fmt.Sprintf("· %6s", fmt.Sprintf("%.1fs", d.Seconds()))),
		p.t.colorLine(eventStyle(ev), line))
	for _, extra := range progressExtra(ev, p.verbosity) {
		_, _ = fmt.Fprintf(p.w, "%s\n", p.t.style(styleDim, progressIndent+extra))
	}
}

// progressIndent lines a sub-line up under the text of the line above it.
const progressIndent = "          "

// count folds one event into the turn's totals.
func (p *progress) count(ev gen.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	countEvent(&p.stats, ev)
}

// countEvent is the summing itself, without the lock, so the full-screen UI
// keeps the same running totals off the same events.
func countEvent(st *turnStats, ev gen.Event) {
	switch event.Type(ev.Type) {
	case event.RunStarted, event.ModelRequest, event.ModelResponse:
		if m := evAttr(ev, "model"); m != "" {
			st.model = m
		}
	case event.ModelFallback:
		if to := evAttr(ev, "to"); to != "" {
			st.fallback = to
			st.model = to
		}
	case event.ToolCallEvent:
		st.toolCalls++
	}
	switch event.Type(ev.Type) {
	case event.ModelResponse:
		if n, ok := evNum(ev, "prompt_tokens"); ok {
			st.prompt += int(n)
		}
		if n, ok := evNum(ev, "completion_tokens"); ok {
			st.completion += int(n)
		}
	case event.RunFinished:
		// The run's own totals are authoritative when they arrive.
		if n, ok := evNum(ev, "prompt_tokens"); ok && int(n) > 0 {
			st.prompt = int(n)
		}
		if n, ok := evNum(ev, "completion_tokens"); ok && int(n) > 0 {
			st.completion = int(n)
		}
	}
}

// --- the info line ------------------------------------------------------------

// info is the one dim line under a reply: what the turn cost. It is empty
// under --quiet, where the reply is all anybody asked for.
func (p *progress) info(trace string) string {
	if p == nil || p.off {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.t.style(styleDim, infoLine(p.stats, time.Since(p.start), trace))
}

// infoLine is the format, apart from the colour: elapsed, model, tokens, tool
// calls, and the trace id when it was asked for. A field nobody reported is
// left out rather than printed as a zero.
func infoLine(st turnStats, d time.Duration, trace string) string {
	parts := []string{fmt.Sprintf("%.1fs", d.Seconds())}
	if st.model != "" {
		parts = append(parts, st.model)
	}
	if st.prompt > 0 || st.completion > 0 {
		parts = append(parts, fmt.Sprintf("%s→%s tokens", tokensShort(st.prompt), tokensShort(st.completion)))
	}
	switch st.toolCalls {
	case 0:
	case 1:
		parts = append(parts, "1 tool call")
	default:
		parts = append(parts, fmt.Sprintf("%d tool calls", st.toolCalls))
	}
	if st.fallback != "" {
		parts = append(parts, "via fallback "+st.fallback)
	}
	if trace != "" {
		parts = append(parts, "trace "+trace)
	}
	return "· " + strings.Join(parts, " · ")
}

// tokensShort keeps a token count to four characters or so.
func tokensShort(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// --- the spinner ---------------------------------------------------------------

// spin starts the "still working" frame in the prompt area. It draws only on
// a terminal: anywhere else the bytes would end up in a file. Calling it
// twice is harmless.
func (p *progress) spin() {
	if p == nil || p.off || !p.tty {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopc != nil {
		return
	}
	stop, done := make(chan struct{}), make(chan struct{})
	p.stopc, p.donec = stop, done
	go p.spinning(stop, done)
}

// spinning draws one frame per tick under the same lock every line takes, so
// a progress line and a frame can never interleave.
func (p *progress) spinning(stop, done chan struct{}) {
	defer close(done)
	tk := time.NewTicker(p.tick)
	defer tk.Stop()
	for i := 0; ; i++ {
		p.mu.Lock()
		p.eraseLocked()
		frame := string(spinnerFrames[i%len(spinnerFrames)]) + " thinking…"
		p.shown = len([]rune(frame))
		_, _ = io.WriteString(p.w, p.t.style(styleDim, frame))
		p.mu.Unlock()
		select {
		case <-stop:
			return
		case <-tk.C:
		}
	}
}

var spinnerFrames = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// stopSpin erases the frame and stops drawing. It is what every path out of a
// turn calls, including the ones that end in an error.
func (p *progress) stopSpin() {
	if p == nil {
		return
	}
	p.mu.Lock()
	stop, done := p.stopc, p.donec
	p.stopc, p.donec = nil, nil
	p.mu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.eraseLocked()
}

// eraseLocked wipes the frame on screen, if there is one: back to the margin,
// blanks over it, back again. Plain spaces rather than an erase escape, so a
// terminal that knows nothing else still ends up clean.
func (p *progress) eraseLocked() {
	if p.shown == 0 {
		return
	}
	_, _ = io.WriteString(p.w, "\r"+strings.Repeat(" ", p.shown)+"\r")
	p.shown = 0
}

// verbosityFloor is the -v level an event type first appears at. 0 (the
// default, no flag): the model's own steps and anything that blocks or
// otherwise needs a person. 1 (-v): what tools and browser tasks ran, and
// how they came out. 2 (-vv): the arguments and moves behind that — tool
// arguments and every browser move/step. A type absent from this map is
// always shown at every level (progressLine may still say "" for other
// reasons, e.g. PolicyDecision's allow case). A browser run waiting for the
// browser is a step, but it holds the turn up like anything that blocks, so it
// is shown at every level too (queuedStep).
var verbosityFloor = map[event.Type]int{
	event.GuardDecision:       1,
	event.ToolCallEvent:       1,
	event.ToolResult:          1,
	event.BrowserTaskStarted:  1,
	event.BrowserTaskFinished: 1,
	event.BrowserDownload:     1,
	event.BrowserMove:         2,
	event.BrowserScreenshot:   2,
	event.BrowserStep:         2,
}

// progressLine is the whole vocabulary: one event in, one line or "" out.
// An attribute that is missing is left out of the line rather than guessed.
func progressLine(ev gen.Event, verbosity int) string {
	if floor, ok := verbosityFloor[event.Type(ev.Type)]; ok && verbosity < floor && !queuedStep(ev) {
		return ""
	}
	switch event.Type(ev.Type) {
	case event.ModelRequest:
		s := "model"
		if n, ok := evNum(ev, "step"); ok {
			s += fmt.Sprintf(" step %d", int(n))
		}
		if m := evAttr(ev, "model"); m != "" {
			s += " → " + m
		}
		if id := evAttr(ev, "model_id"); id != "" && id != evAttr(ev, "model") {
			s += " (" + id + ")"
		}
		return s

	case event.ModelFallback:
		s := "model fallback"
		from, to := evAttr(ev, "from"), evAttr(ev, "to")
		switch {
		case from != "" && to != "":
			s += " " + from + " → " + to
		case to != "":
			s += " → " + to
		}
		return s + paren(evAttr(ev, "reason"))

	case event.ModelUsageLimit:
		s := "model usage limit"
		if pr := evAttr(ev, "provider"); pr != "" {
			s += " " + pr
		}
		if m := evAttr(ev, "model"); m != "" {
			s += " (" + m + ")"
		}
		if t := evClock(ev, "reset_at"); t != "" {
			s += ", resets " + t
		}
		return s

	case event.RunStepLimit:
		s := "run: step limit"
		if n, ok := evNum(ev, "steps"); ok {
			s += fmt.Sprintf(", %d steps", int(n))
		}
		return s + paren(evAttr(ev, "reason"))

	case event.RunLoopDetected:
		s := "run: loop detected"
		if t := evAttr(ev, "tool"); t != "" {
			s += " " + t
		}
		if n, ok := evNum(ev, "count"); ok {
			s += fmt.Sprintf(" ×%d", int(n))
		}
		return s

	case event.GuardDecision:
		s := "guard"
		if d := evAttr(ev, "decision"); d != "" {
			s += " " + d
		}
		var in []string
		if r := evAttr(ev, "reason"); r != "" {
			in = append(in, r)
		}
		if n, ok := evNum(ev, "score"); ok {
			in = append(in, fmt.Sprintf("%.2f", n))
		}
		return s + paren(strings.Join(in, " "))

	case event.PolicyDecision:
		// Only the two decisions that change what happens are worth a line.
		d := evAttr(ev, "decision")
		if d != "deny" && d != "ask" {
			return ""
		}
		s := "policy " + d
		if r := evAttr(ev, "reason"); r != "" {
			s += ": " + r
		}
		return s

	case event.ToolCallEvent:
		s := "tool " + evAttr(ev, "tool")
		if verbosity >= 2 {
			if a := argSummary(evAttr(ev, "args")); a != "" {
				s += "  " + a
			}
		}
		return s

	case event.ToolResult:
		s := "tool " + evAttr(ev, "tool") + " → "
		if evBool(ev, "is_error") {
			s += "error"
		} else {
			s += "ok"
		}
		if ev.DurationNs != nil {
			s += fmt.Sprintf(" in %.1fs", time.Duration(*ev.DurationNs).Seconds())
		}
		if evBool(ev, "is_error") {
			if e := evAttr(ev, "error"); e != "" {
				s += ": " + clip80(e)
			}
		}
		if evBool(ev, "tainted") {
			s += " (tainted)"
		}
		return s

	case event.BrowserTaskStarted:
		s := "browser: task started"
		if d := evAttr(ev, "decider"); d != "" {
			s += " (decider: " + d + ")"
		}
		if n, ok := evNum(ev, "queued_ms"); ok && n > 0 {
			s += fmt.Sprintf(" after %.1fs in the queue", time.Duration(n*float64(time.Millisecond)).Seconds())
		}
		return s

	case event.BrowserMove:
		// The one text call of a browser turn (M11): what the model decided to
		// do on the page it had just read. The moves themselves are the block
		// under the line.
		s := "browser: move"
		if n, ok := evNum(ev, "n"); ok {
			s += fmt.Sprintf(" %d", int(n))
		}
		if n := len(moveList(ev)); n > 0 {
			s += fmt.Sprintf(", %d queued", n)
		}
		if evBool(ev, "done") {
			s += ", done"
		}
		if b := evAttr(ev, "blocked"); b != "" {
			s += ", needs a person: " + b
		}
		if n, ok := evNum(ev, "dropped"); ok && n > 0 {
			s += fmt.Sprintf(", %d dropped", int(n))
		}
		s += moveCost(ev)
		return s

	case event.BrowserStep:
		// A run that has not started yet: the agent's browser is busy with
		// another of its runs — another session's, or a background task's.
		if queuedStep(ev) {
			return "browser: waiting for another run of this agent to give the browser back"
		}
		s := "browser: step"
		if n, ok := evNum(ev, "n"); ok {
			s += fmt.Sprintf(" %d", int(n))
		}
		if a := evAttr(ev, "action"); a != "" {
			s += " " + a
		}
		// The target names the element the way the move event does — ref and
		// accessible name; an older event carries the ref alone.
		if target := evAttr(ev, "target"); target != "" {
			s += " " + target
		} else if ref := evAttr(ev, "ref"); ref != "" {
			s += " " + ref
		}
		if role := evAttr(ev, "role"); role != "" {
			s += " " + role
		}
		// A wait carries what it waited for and whether the page got there,
		// where a move carries its scores.
		if u := evAttr(ev, "until"); u != "" {
			what := u
			if u == "quiet" {
				what = "settle"
			}
			s += " for the page to " + what
			if quiet, has := evHasBool(ev, "quiet"); has && !quiet {
				s += " · gave up"
			}
		}
		// The two gate events of the recovery branch: the run admitting it is
		// going nowhere, and what it decided to do about it. Both are fixed
		// words of internal/browser.
		if h := evAttr(ev, "held"); h != "" {
			s += " held: " + h
			if why := evAttr(ev, "why"); why != "" {
				s += " · " + why
			}
		}
		if strat := evAttr(ev, "strategy"); strat != "" {
			s += " · " + strat
		}
		// A box that only opens a list: the move said type and the browser
		// clicked it, which is the step a person reading this has to see.
		if evBool(ev, "opened") {
			s += " · opened"
		}
		// Controls the move left on the screen that the page called nothing at
		// all — a calendar of day cells: the reason whatever was queued behind
		// this step is not going to run.
		if evBool(ev, "panel") {
			s += " · panel"
		}
		// A box that answered what was typed with its own list of suggestions:
		// the browser chose one, which is part of the same step.
		if evBool(ev, "picked") {
			s += " · picked"
			if target := evAttr(ev, "pick_target"); target != "" {
				s += " " + target
			}
		}
		if n, ok := evNum(ev, "effect_p"); ok {
			s += fmt.Sprintf(" · effect %.2f", n)
		}
		if n, ok := evNum(ev, "goal_p"); ok {
			s += fmt.Sprintf(" · goal %.2f", n)
		}
		if k := evAttr(ev, "risk"); k != "" && k != "none" {
			s += " · risk " + k
		}
		if n, ok := evNum(ev, "rung"); ok && n > 0 {
			s += fmt.Sprintf(" · rung %d", int(n))
		}
		if n, ok := evNum(ev, "took_ms"); ok {
			s += fmt.Sprintf(" · %dms", int64(n))
		}
		if u := evAttr(ev, "url"); u != "" {
			s += " → " + clipURLCLI(u)
		}
		return s

	case event.BrowserBlocked:
		return "browser: blocked" + paren(evAttr(ev, "why"))

	case event.BrowserTakeoverRequest:
		s := "browser: takeover requested" + paren(evAttr(ev, "why")) +
			" — open /v1/browser/viewer to drive it"
		if t := evClock(ev, "expires_at"); t != "" {
			s += ", expires " + t
		}
		return s

	case event.BrowserTakeoverStarted:
		return "browser: takeover started"

	case event.BrowserTakeoverEnded:
		return "browser: takeover ended"

	case event.BrowserScreenshot:
		s := "browser: screenshot" + paren(evAttr(ev, "why"))
		if n, ok := evNum(ev, "size"); ok {
			s += " · " + bytesShort(int64(n))
		}
		if n, ok := evNum(ev, "took_ms"); ok {
			s += fmt.Sprintf(" · %dms", int64(n))
		}
		if evBool(ev, "attached") {
			s += " · attached"
		}
		return s

	case event.BrowserDownload:
		s := "browser: download"
		if n := evAttr(ev, "name"); n != "" {
			s += " " + n
		}
		if n, ok := evNum(ev, "size"); ok {
			s += " (" + bytesShort(int64(n)) + ")"
		}
		return s

	case event.BrowserTaskFinished:
		outcome := evAttr(ev, "outcome")
		if outcome == "" {
			outcome = "finished"
		}
		s := "browser: " + outcome
		if n, ok := evNum(ev, "took_ms"); ok {
			s += " in " + secsShort(int64(n))
		}
		var tail []string
		if n, ok := evNum(ev, "steps"); ok {
			tail = append(tail, fmt.Sprintf("%d steps", int(n)))
		}
		if n, ok := evNum(ev, "generative_calls"); ok {
			tail = append(tail, fmt.Sprintf("%d model calls", int(n)))
		}
		if n, ok := evNum(ev, "decision_calls"); ok {
			tail = append(tail, fmt.Sprintf("%d decisions", int(n)))
		}
		if n, ok := evNum(ev, "waits"); ok && n > 0 {
			tail = append(tail, fmt.Sprintf("%d waits", int(n)))
		}
		// How often the run noticed it was stuck and did something about it.
		if n, ok := evNum(ev, "recoveries"); ok && n > 0 {
			tail = append(tail, fmt.Sprintf("%d recoveries", int(n)))
		}
		if n := evCost(ev); n != nil && *n > 0 {
			tail = append(tail, dollars(n.Decimal()))
		}
		if h := rungHistogram(ev); h != "" {
			tail = append(tail, "rungs "+h)
		}
		if n, ok := evNum(ev, "extract_took_ms"); ok && n > 0 {
			tail = append(tail, "extract "+secsShort(int64(n)))
		}
		if len(tail) > 0 {
			s += " — " + strings.Join(tail, ", ")
		}
		return s
	}
	return ""
}

// moveCost is the " · Xs · P→O tokens" tail of a browser.move line: every
// move call's duration and token counts are traced, and the line shows them.
func moveCost(ev gen.Event) string {
	var s string
	if n, ok := evNum(ev, "took_ms"); ok {
		s += " · " + secsShort(int64(n))
	}
	prompt, hasPrompt := evNum(ev, "prompt_tokens")
	output, hasOutput := evNum(ev, event.AttrCompletionTokens)
	if !hasOutput {
		// A browser.move written before the rename.
		output, hasOutput = evNum(ev, event.AttrOutputTokens)
	}
	if hasPrompt || hasOutput {
		s += fmt.Sprintf(" · %d→%d tokens", int(prompt), int(output))
	}
	return s
}

// rungHistogram is the ladder's non-zero entries (M11: index 2 a move the
// model grounded itself, 3 an element the decision model bound, 5 vision,
// 6 a person, 7 failure), as "2:5 3:1".
func rungHistogram(ev gen.Event) string {
	if ev.Attrs == nil {
		return ""
	}
	raw, ok := (*ev.Attrs)["rungs"]
	if !ok {
		return ""
	}
	arr, ok := raw.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for i, v := range arr {
		n, ok := toNum(v)
		if !ok || n <= 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d:%d", i, int(n)))
	}
	return strings.Join(parts, " ")
}

// --- the moves --------------------------------------------------------------

// queuedStep reports whether ev is a browser run waiting for another run of the
// same agent to give the browser back.
func queuedStep(ev gen.Event) bool {
	return event.Type(ev.Type) == event.BrowserStep && evAttr(ev, "held") == "queued"
}

// progressExtra is the indented block under a line, or nothing. A browser move
// call has one — the moves it decided on, which is what a person watching a
// browser task wants to read — and so does a finished browser task, which names
// where it ended up. Both follow their line's own -v floor.
func progressExtra(ev gen.Event, verbosity int) []string {
	if floor, ok := verbosityFloor[event.Type(ev.Type)]; ok && verbosity < floor {
		return nil
	}
	switch event.Type(ev.Type) {
	case event.BrowserTaskFinished:
		if line := endedAtLine(ev); line != "" {
			return []string{line}
		}
		return nil
	case event.BrowserMove:
		moves := moveList(ev)
		out := make([]string, 0, len(moves))
		for i, m := range moves {
			out = append(out, fmt.Sprintf("%d. %s", i+1, moveLine(m)))
		}
		return out
	}
	return nil
}

// cliMaxURLLen is how much of a URL the CLI shows before clipping it — the
// trace and the tool result carry it in full; only the terminal line is
// shortened.
const cliMaxURLLen = 120

// clipURLCLI truncates a URL for the terminal; nowhere else.
func clipURLCLI(u string) string { return clip(u, cliMaxURLLen) }

// endedAtLine is the indented line under a finished browser task: where it
// ended up, done or failed alike. "" when neither attribute came through.
func endedAtLine(ev gen.Event) string {
	url, title := evAttr(ev, "url"), evAttr(ev, "title")
	if url == "" && title == "" {
		return ""
	}
	s := "browser: ended at "
	if title != "" {
		s += clip80(flat(title)) + " — "
	}
	return s + clipURLCLI(url)
}

// move is one queued move as internal/browser traces it: what it does, what it
// acts on, and what it expects. A typed value is never among them.
type move struct {
	Action string `json:"action"`
	Target string `json:"target"`
	Expect string `json:"expect"`
}

// moveList reads the moves attribute back. Anything that is not the shape this
// expects is simply not shown.
func moveList(ev gen.Event) []move {
	if ev.Attrs == nil {
		return nil
	}
	raw, ok := (*ev.Attrs)["moves"]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var out []move
	if err := json.Unmarshal(b, &out); err != nil {
		return nil
	}
	return out
}

// moveLine is one move in words: what it does, to what, and what it should
// produce.
func moveLine(m move) string {
	body := m.Action
	if body == "" {
		body = "move"
	}
	if m.Target != "" {
		body += " " + clip80(flat(m.Target))
	}
	if m.Expect != "" {
		return pad(body, 47) + " expect: " + clip80(flat(m.Expect))
	}
	return body
}

// pad widens s to n columns, counting runes: an arrow or an accent is one
// column and not three.
func pad(s string, n int) string {
	if w := len([]rune(s)); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// --- attributes -------------------------------------------------------------

// evNum reads one numeric attribute. JSON numbers arrive as float64 here.
func evNum(ev gen.Event, key string) (float64, bool) {
	if ev.Attrs == nil {
		return 0, false
	}
	return toNum((*ev.Attrs)[key])
}

// evCost is the event's cost, under either name (event.CostOf); nil when it
// reported none.
func evCost(ev gen.Event) *provider.NanoUSD {
	if ev.Attrs == nil {
		return nil
	}
	return event.CostOf(*ev.Attrs)
}

// toNum is evNum's type switch, shared with a numeric value read out of an
// array attribute (the rungs histogram) rather than off the top-level map.
func toNum(v any) (float64, bool) {
	switch v := v.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

func evBool(ev gen.Event, key string) bool {
	v, _ := evHasBool(ev, key)
	return v
}

// evHasBool is evBool plus whether the key was present at all, for an
// attribute where absent and false must read differently (browser.replan's
// "ok").
func evHasBool(ev gen.Event, key string) (bool, bool) {
	if ev.Attrs == nil {
		return false, false
	}
	v, ok := (*ev.Attrs)[key].(bool)
	return v, ok
}

// evClock reads an RFC3339 timestamp attribute as a local wall clock, which
// is what someone reading "expires" in a terminal needs.
func evClock(ev gen.Event, key string) string {
	t, err := time.Parse(time.RFC3339, evAttr(ev, key))
	if err != nil {
		return ""
	}
	return t.Local().Format("15:04:05")
}

// argSummary is the one glimpse of a tool call's arguments: its first string
// field (the intent of a browser task, the path of a write), else the raw
// JSON, capped either way.
func argSummary(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return ""
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	tok, err := dec.Token()
	if d, ok := tok.(json.Delim); err == nil && ok && d == '{' {
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				break
			}
			key, _ := kt.(string)
			vt, err := dec.Token()
			if err != nil {
				break
			}
			switch v := vt.(type) {
			case string:
				if strings.TrimSpace(v) != "" {
					return clip80(key + ": " + flat(v))
				}
			case json.Delim:
				if skipNested(dec) != nil {
					return clip80(flat(raw))
				}
			}
		}
	}
	return clip80(flat(raw))
}

// skipNested consumes the rest of an object or array whose opening delimiter
// was just read.
func skipNested(dec *json.Decoder) error {
	for depth := 1; depth > 0; {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else {
				depth--
			}
		}
	}
	return nil
}

// --- small formats ----------------------------------------------------------

func paren(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

// flat folds an argument onto one line; clip80 keeps it short enough to read
// at a glance.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip80(s string) string { return clip(s, 80) }

// secsShort prints a duration the way someone waiting reads it.
func secsShort(ms int64) string {
	s := float64(ms) / 1000
	if s >= 10 {
		return fmt.Sprintf("%.0fs", s)
	}
	return fmt.Sprintf("%.1fs", s)
}

func bytesShort(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// genEvent is one internal event in the API's shape, so the in-process mode
// and the attached one hand their frontends the same type.
func genEvent(e event.Event) gen.Event {
	ev := gen.Event{
		Schema: e.Schema, Id: string(e.ID), SpanId: string(e.SpanID),
		Type: string(e.Type), Time: e.Time, TraceId: string(e.TraceID),
	}
	if e.SessionID != "" {
		ev.SessionId = ptrOf(string(e.SessionID))
	}
	if len(e.Attrs) > 0 {
		var m map[string]any
		if err := json.Unmarshal(e.Attrs, &m); err == nil {
			ev.Attrs = &m
		}
	}
	if e.Duration > 0 {
		ns := int64(e.Duration)
		ev.DurationNs = &ns
	}
	return ev
}
