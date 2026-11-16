package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"holaclaw/internal/browser/proto"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// The one text call of M11: the model is shown the page it is on — url, title,
// the shortlist with its refs, an excerpt of the text — and what has already
// been tried, and answers with up to MaxMovesPerCall moves or "done". It
// replaces the blind plan and its re-plans: a plan written before any page was
// read was the thing that completed no task at all.
//
// Nothing the call returns drives the browser as it stands. An action outside
// the closed set, a ref the observation never offered, a navigate that is not
// http(s) — each is dropped here, and a queue that ends up empty sends the run
// back to this call rather than to a guess.

// moveCall is one answer, validated.
type moveCall struct {
	Done    bool
	Moves   []Move
	Note    string
	Blocked Blocker
}

// rawMove is the wire shape. It is deliberately separate from Move: what comes
// back is text, and only what survives validation becomes a Move.
type rawMove struct {
	Action   string `json:"action"`
	Ref      string `json:"ref"`
	Describe string `json:"describe"`
	Value    string `json:"value"`
	Submit   bool   `json:"submit"`
	URL      string `json:"url"`
	Expect   string `json:"expect"`
}

type rawMoveCall struct {
	Done    bool      `json:"done"`
	Moves   []rawMove `json:"moves"`
	Note    string    `json:"note"`
	Blocked string    `json:"blocked"`
}

// Bounds on what one answer may say. The note and the expectations are
// page-derived text: they are capped where they are read, not where they are
// rendered.
const (
	maxNoteBytes   = 300
	maxExpectBytes = 200
	maxMoveHistory = 6
)

// moveAttrs is the browser.move event. The moves are the model's own words
// about a page it had just read, so — like the final title and URL — they are
// page-derived on purpose: a person reading a trace has to be able to see what
// the browser thought it was doing. A typed value never appears: not here, not
// in a log line, not in an approval.
type moveAttrs struct {
	N       int         `json:"n"`
	Moves   []movedAttr `json:"moves,omitempty"`
	Done    bool        `json:"done"`
	Blocked string      `json:"blocked,omitempty"`
	// Dropped counts the moves validation threw away, Retried says the answer
	// took a second attempt.
	Dropped int  `json:"dropped,omitempty"`
	Retried bool `json:"retried,omitempty"`

	TookMS int64 `json:"took_ms"`
	// The call's usage, both attempts summed. It is already in the task's
	// browser.task.finished, which is the event a trace's total counts.
	*event.UsageAttrs
}

// movedAttr is one queued move in the trace: what it does, what it acts on,
// and what it expects. Never what it types.
type movedAttr struct {
	Action string `json:"action"`
	Target string `json:"target,omitempty"`
	Expect string `json:"expect,omitempty"`
}

// tracedMoves renders a queue for the trace, naming each target by the ref and
// the element's own accessible name so a line reads as something that happened
// on a page.
func tracedMoves(ms []Move, obs proto.Observation) []movedAttr {
	out := make([]movedAttr, 0, len(ms))
	for _, m := range ms {
		out = append(out, movedAttr{
			Action: string(m.Action),
			Target: moveTarget(m, obs),
			Expect: oneLine(m.Expect, maxExpectBytes),
		})
	}
	return out
}

// moveTarget is what a move acts on in one short string: the ref and the
// element's name when the ref is one of the page's, the words the model used
// otherwise, and the URL for a navigate.
func moveTarget(m Move, obs proto.Observation) string {
	if m.Action == MoveNavigate {
		return oneLine(m.URL, 300)
	}
	if m.Ref != "" {
		if c, ok := candidateByRef(obs.Candidates, m.Ref); ok && c.Name != "" {
			return m.Ref + " " + quote(c.Name)
		}
		return m.Ref
	}
	if m.Describe != "" {
		return quote(oneLine(m.Describe, 200))
	}
	return ""
}

// tried is one move the run has already made, as the history renders it: what
// was done, what it was to produce, and what came of it. The words are fixed
// or page-derived, never a typed value.
type tried struct {
	what, expect, effect string
}

// The fixed verdicts of the history.
const (
	effectWorked   = "it did"
	effectNoChange = "it did not: nothing on the page changed at all"
	effectNoEffect = "it did not: the page changed, but not in that way"
	effectNoAct    = "the browser could not carry it out"
	effectNoAnswer = "the page was reached but nothing could be read off it"
	effectPartial  = "the page holds only part of what was asked for: the rest may be further down, " +
		"behind a \"show more\", or on another page"
	effectNoMove = "nothing on this page could be acted on"
	effectPicked = "it did, and one of the page's own suggestions was picked"
	effectNoPick = "it did not: the box offered a list and nothing in it matched the request"
	// effectOpenedDialog is a move that worked and left a dialog over the page.
	// Nothing else can be done until what it asks for has been answered in it,
	// so the history says so rather than letting the next turn queue a submit
	// behind a date that was never chosen.
	effectOpenedDialog = "it did, and the page opened a dialog: what it asks for has to be " +
		"chosen or confirmed in it before anything else"
	// effectOpenedPanel is the same verdict for the panel a page puts up
	// without calling it anything: a calendar made of day cells, a filter
	// drawer. No dialog is named and no list of options appears — the whole of
	// what happened is that controls the page did not have are on it now.
	effectOpenedPanel = "it did, and the page put new controls on the screen: what it asks for has to be " +
		"chosen among them before anything else"
	// effectNotTyped is a box that took none of the characters and opened
	// nothing when it was clicked either: the value is not in the form, and
	// the move call has to find another way into the field.
	effectNotTyped = "it did not: the box did not take what was typed and did not open anything"
)

// note books one attempt in the history, keeping the last maxMoveHistory.
func (r *run) note(t tried) {
	r.history = append(r.history, t)
	if len(r.history) > maxMoveHistory {
		r.history = r.history[len(r.history)-maxMoveHistory:]
	}
}

// renderHistory is what the move call is told about its own past turns. It
// goes inside the fence with the page: the element names in it are the page's.
func renderHistory(h []tried) string {
	if len(h) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("already tried, oldest first:\n")
	for i, t := range h {
		fmt.Fprintf(&b, "  %d. %s", i+1, t.what)
		if t.expect != "" {
			b.WriteString(" — expected: " + oneLine(t.expect, maxExpectBytes))
		}
		if t.effect != "" {
			// A line the run wrote about itself rather than about a move — the
			// recovery branch's "the run is going in circles" — has no verdict
			// to report, and an empty one would read as a missing word.
			b.WriteString(" — " + t.effect)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// move is the call. The request is the user's own text and goes in unwrapped;
// the page, the history and the last note are page-derived and go in fenced.
func (e *Executor) move(ctx context.Context, r *run, obs proto.Observation) (moveCall, error) {
	start := e.now()
	defer func() { r.stats.MoveTook += e.now().Sub(start) }()
	r.moveCalls++
	r.stats.MoveCalls++

	model, prov := e.movePair()
	msgs := []provider.Message{provider.TextMessage(provider.RoleUser, e.moveRequest(r, obs))}
	var (
		mc      moveCall
		dropped int
		err     error
		used    event.Usage
	)
	for attempt := 0; attempt < 2; attempt++ {
		var resp *provider.Response
		resp, err = e.moveOnce(ctx, model, prov, msgs)
		if err != nil {
			if timedOut(ctx, err) {
				e.log(r).Warn("browser: move call timed out", "timeout", e.o.PlanTimeout)
				break
			}
			return moveCall{}, err
		}
		r.stats.GenerativeCall++
		r.count(resp.Usage)
		used.Add(event.Call(resp.Usage))
		mc, dropped, err = parseMove(resp.Text, obs)
		if err == nil {
			e.emit(ctx, r, event.BrowserMove, moveAttrs{
				N: r.moveCalls, Moves: tracedMoves(mc.Moves, obs), Done: mc.Done,
				Blocked: blockedWord(mc.Blocked), Dropped: dropped, Retried: attempt > 0,
				TookMS: e.now().Sub(start).Milliseconds(), UsageAttrs: used.Attrs(),
			})
			r.lastNote = mc.Note
			return mc, nil
		}
		msgs = append(msgs,
			provider.TextMessage(provider.RoleAssistant, resp.Text),
			provider.TextMessage(provider.RoleUser, fmt.Sprintf(moveRetryFmt, err)))
	}
	// Two unreadable answers are not a task failure: the caller counts them and
	// climbs to vision or a person. What the trace says is that the call
	// produced nothing.
	e.log(r).Warn("browser: the move answer could not be read", "err", errText(err))
	e.emit(ctx, r, event.BrowserMove, moveAttrs{
		N: r.moveCalls, Dropped: dropped, Retried: true,
		TookMS: e.now().Sub(start).Milliseconds(), UsageAttrs: used.Attrs(),
	})
	return moveCall{Blocked: BlockNone}, nil
}

// needsPerson reports whether the answer says this page cannot be passed
// without somebody at the keyboard.
func (mc moveCall) needsPerson() bool {
	switch mc.Blocked {
	case BlockLogin, BlockCaptcha, Block2FA:
		return true
	}
	return false
}

// moveOnce is one attempt, bounded by its own timeout: a route that hangs
// costs the attempt and not the whole task.
func (e *Executor) moveOnce(ctx context.Context, m provider.Model, p provider.Provider, msgs []provider.Message) (*provider.Response, error) {
	cctx, cancel := context.WithTimeout(ctx, e.o.PlanTimeout)
	defer cancel()
	return e.chat(cctx, m, p, moveSystem, moveReserve, provider.ReasoningLow, msgs...)
}

// moveRequest is the user message: the request in the user's own words, then
// everything the page and the run's own past said, fenced together.
func (e *Executor) moveRequest(r *run, obs proto.Observation) string {
	var page strings.Builder
	page.WriteString(renderObservation(obs))
	if h := renderHistory(r.history); h != "" {
		page.WriteString("\n" + h)
	}
	if len(r.avoid) > 0 {
		// What a recovery learnt: the elements a move has already failed on
		// twice. Their names are the page's own words, which is why the line
		// goes inside the fence with the rest of the page.
		page.WriteString("\ndo not try again: " + strings.Join(r.avoid, ", ") + "\n")
	}
	if r.lastNote != "" {
		page.WriteString("\nyour note last turn: " + oneLine(r.lastNote, maxNoteBytes) + "\n")
	}
	return "request: " + oneLine(r.task.Intent, 4000) + "\n\n" + fencePage(page.String())
}

// movePair is the model and provider the move call uses: the `plan` role when
// it is wired, the `chat` role otherwise (docs/browser.md).
func (e *Executor) movePair() (provider.Model, provider.Provider) {
	if e.o.Plan != nil {
		return e.o.PlanModel, e.o.Plan
	}
	return e.o.ChatModel, e.o.Chat
}

// parseMove reads one answer against the closed sets and the observation it
// was made on. The error is quoted back to the model, so it says what is wrong
// in the model's own terms; dropped is how many moves were thrown away, which
// is not an error as long as one survives.
func parseMove(text string, obs proto.Observation) (moveCall, int, error) {
	obj := jsonObject(text)
	if obj == "" {
		return moveCall{}, 0, errors.New("the answer held no JSON object")
	}
	var raw rawMoveCall
	if err := json.Unmarshal([]byte(obj), &raw); err != nil {
		return moveCall{}, 0, fmt.Errorf("the JSON did not parse (%v)", err)
	}
	mc := moveCall{Done: raw.Done, Note: oneLine(raw.Note, maxNoteBytes), Blocked: blockerOf(raw.Blocked)}
	dropped := 0
	for _, rm := range raw.Moves {
		if len(mc.Moves) == MaxMovesPerCall {
			dropped++
			continue
		}
		m, ok := validMove(rm, obs)
		if !ok {
			dropped++
			continue
		}
		mc.Moves = append(mc.Moves, m)
	}
	// An answer with nothing usable in it, where something was meant: the retry
	// is worth spending, and the error says what was wrong with it. An answer
	// that simply had no move to offer is not an error at all — the loop counts
	// those and climbs to the late rungs.
	if len(mc.Moves) == 0 && dropped > 0 && !mc.Done && mc.Blocked == BlockNone {
		return moveCall{Blocked: BlockNone}, dropped, errors.New(
			"none of those moves could be used: an action outside the set, a ref this page does not " +
				"have, a type with nothing to type, or a navigate without an http url")
	}
	return mc, dropped, nil
}

// validMove is the whole of what makes one move actable: a known action, a ref
// of this observation or words to bind instead, something to type for a type,
// an http URL for a navigate.
func validMove(rm rawMove, obs proto.Observation) (Move, bool) {
	m := Move{
		Action:   MoveAction(strings.ToLower(strings.TrimSpace(rm.Action))),
		Ref:      strings.TrimSpace(rm.Ref),
		Describe: oneLine(rm.Describe, 200),
		Value:    rm.Value,
		Submit:   rm.Submit,
		URL:      strings.TrimSpace(rm.URL),
		Expect:   oneLine(rm.Expect, maxExpectBytes),
	}
	// A ref we never offered is not a ref. The words beside it may still name
	// the element, and the decision model binds those to a candidate.
	if m.Ref != "" {
		if _, ok := candidateByRef(obs.Candidates, m.Ref); !ok {
			m.Ref = ""
		}
	}
	switch m.Action {
	case MoveClick, MoveScroll:
		// Scrolling needs no element; clicking does.
		return m, m.Action == MoveScroll || m.Ref != "" || m.Describe != ""
	case MoveType, MoveSelect:
		return m, strings.TrimSpace(m.Value) != "" && (m.Ref != "" || m.Describe != "")
	case MoveKey:
		// A key press is a word of a closed set, not text: anything else
		// written there is not a key and the move is not one either. The
		// canonical spelling is what survives, so nothing the page or the
		// model wrote reaches the browser as it was written.
		key, known := keyName(m.Value)
		m.Value = key
		return m, known && (m.Ref != "" || m.Describe != "")
	case MoveBack, MoveWait:
		return m, true
	case MoveNavigate:
		// The allow-list itself is the sidecar's, enforced on every navigation
		// and redirect; the scheme is the only thing checked here.
		return m, isHTTPURL(m.URL)
	}
	return Move{}, false
}

// moveKeys is the closed set of keys a move may press: the ones a page's own
// instructions ever name — a date field that says to use the arrow keys, a
// panel that closes on Escape. A key move carries no text and never a typed
// value, which is what makes the key itself safe to write down.
var moveKeys = []string{
	"Enter", "Escape", "Tab", "ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight",
	"PageUp", "PageDown", "Home", "End", "Space", "Backspace",
}

// keyName maps what a move call wrote onto one of the set, ignoring case.
// Anything outside it is no key at all.
func keyName(s string) (string, bool) {
	s = strings.TrimSpace(s)
	for _, k := range moveKeys {
		if strings.EqualFold(k, s) {
			return k, true
		}
	}
	return "", false
}

// blockerOf reads the move call's own blocked word: anything outside the set
// is no blocker, which is the safe reading — a person is asked for by a page
// that plainly needs one, not by a word we could not place.
func blockerOf(s string) Blocker {
	switch Blocker(strings.ToLower(strings.TrimSpace(s))) {
	case BlockLogin:
		return BlockLogin
	case BlockCaptcha:
		return BlockCaptcha
	case Block2FA:
		return Block2FA
	}
	return BlockNone
}

// blockedWord is the trace word for a blocker: "" for none, so the attribute
// is simply absent on the ordinary case.
func blockedWord(b Blocker) string {
	if b == BlockNone {
		return ""
	}
	return string(b)
}

// needsElement reports whether a move has to be bound to an element of the
// live page before it can be carried out.
func needsElement(m Move) bool {
	switch m.Action {
	case MoveClick, MoveType, MoveSelect, MoveKey:
		return true
	}
	return false
}

// actionFor maps one move and the ref it was bound to onto the closed action
// set of the sidecar. A select is typed into the control: nothing a model says
// picks an option by index, and the native popup is keyboard-driven.
func actionFor(m Move, ref string) proto.Action {
	switch m.Action {
	case MoveNavigate:
		return proto.Action{Kind: proto.ActNavigate, URL: m.URL}
	case MoveType, MoveSelect:
		return proto.Action{Kind: proto.ActType, Ref: ref, Text: m.Value, Clear: true, Submit: m.Submit}
	case MoveKey:
		// The value of a key move is the key's name, already checked against
		// moveKeys: it is a key press and never characters.
		return proto.Action{Kind: proto.ActKey, Ref: ref, Key: m.Value}
	case MoveScroll:
		return proto.Action{Kind: proto.ActScroll, Ref: ref, DeltaY: scrollDelta}
	case MoveBack:
		return proto.Action{Kind: proto.ActBack}
	case MoveWait:
		return proto.Action{Kind: proto.ActWait}
	}
	return proto.Action{Kind: proto.ActClick, Ref: ref}
}

// moveHints are the words the sidecar ranks candidates by: what the move says
// about the element it wants. A typed value is not among them — it is the
// user's own text and the ranking does not need it.
func moveHints(m Move) []string {
	out := make([]string, 0, 8)
	for _, w := range strings.Fields(strings.ToLower(m.Describe)) {
		w = strings.Trim(w, `.,:;"'()[]{}!?`)
		if len(w) < 3 || isStopWord(w) {
			continue
		}
		out = append(out, w)
		if len(out) == cap(out) {
			break
		}
	}
	return out
}

func isStopWord(w string) bool {
	switch w {
	case "the", "and", "for", "with", "into", "that", "this", "your", "from":
		return true
	}
	return false
}

// timedOut reports whether err is a call's own timeout firing, as opposed to
// the run's deadline or the caller going away — those are real failures.
func timedOut(ctx context.Context, err error) bool {
	return errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil
}

// errText is an error's message, or a word when there is no error: a log line
// about an unreadable answer must not panic on a nil one.
func errText(err error) string {
	if err == nil {
		return "no answer"
	}
	return err.Error()
}

// isHTTPURL is the only scheme check the executor makes; the allow-list is
// enforced in the sidecar, on every navigation and redirect.
func isHTTPURL(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// jsonObject extracts the outermost {...} of a model answer, tolerating a
// markdown fence or a sentence around it.
func jsonObject(text string) string {
	s := strings.TrimSpace(text)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			return s[i : j+1]
		}
	}
	return ""
}
