package browser

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"sort"
	"strings"
	"time"

	"holaclaw/internal/browser/proto"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// The loop (docs/browser.md, M11): observe, ask the text model for up to three
// moves on the page in front of it, then carry them out one at a time — each
// verified by the decision model against what the move said it would do, and
// against the request. A queued move that worked costs no text call at all; one
// that did not sends the run back to the move call with the failure in its
// history.
//
// Nothing a page says reaches a log line, and nothing but the model's own
// account of what it is doing — the moves, the final title and URL — reaches a
// trace event. A typed value reaches neither.

// stepAttrs is the browser.step event: one move carried out, as numbers and
// closed-set words. Ref is the sidecar's opaque handle and Role is checked
// against the set below, so no page text rides along.
type stepAttrs struct {
	N       int        `json:"n"`
	Action  MoveAction `json:"action"`
	Ref     string     `json:"ref,omitempty"`
	Role    string     `json:"role,omitempty"`
	EffectP float64    `json:"effect_p"`
	GoalP   float64    `json:"goal_p"`
	// LoadP is how sure the decision model was that the page was still coming
	// up when it judged the move; absent on a page that plainly was not.
	LoadP float64  `json:"load_p,omitempty"`
	Risk  RiskKind `json:"risk,omitempty"`
	Rung  int      `json:"rung"`
	// Target is the element the move acted on as the browser.move event names
	// it: the ref and the element's own accessible name. A ref alone says
	// nothing to a person reading a trace; the name is page-derived on
	// purpose, the same way the move's target is — the trace has to say what
	// the browser clicked.
	Target string `json:"target,omitempty"`
	// Picked says the box this move typed into answered with a list of its own
	// suggestions and the executor picked one; PickRef is the option it
	// clicked and PickTarget names it the way Target does.
	Picked     bool   `json:"picked,omitempty"`
	PickRef    string `json:"pick_ref,omitempty"`
	PickTarget string `json:"pick_target,omitempty"`
	// Opened says the move was carried out as a click because the element it
	// named takes no characters: a box that only opens a list. The action stays
	// what the move asked for — what a person reading the trace wants to see is
	// the move the run made and the one thing the browser did differently.
	Opened bool `json:"opened,omitempty"`
	// Panel says the move left controls on the screen the page had not shown
	// before and called nothing at all — a calendar of day cells, a filter
	// drawer. Like a dialog it is a page the rest of the queue was not decided
	// on, and a person reading the trace needs to see where the queue went.
	Panel bool `json:"panel,omitempty"`
	// URL is the page's own address, carried only when it changed since the
	// previous browser.step event: repeating it on every step of one page would
	// say nothing new.
	URL    string `json:"url,omitempty"`
	TookMS int64  `json:"took_ms"`
}

// waitAttrs is the browser.step event of a wait. A wait is the one step that
// does nothing to the page, so it carries no scores and no rung — there was no
// move to score — and says instead what was waited for and whether the page got
// there before the bound. It is deliberately the same event type as a move:
// what a person reading a trace wants is one timeline of what the browser did,
// and a wait that took four seconds is the most interesting line on it.
type waitAttrs struct {
	N      int    `json:"n"`
	Action string `json:"action"`
	Until  string `json:"until"`
	Quiet  bool   `json:"quiet"`
	TookMS int64  `json:"took_ms"`
}

// commits reports whether a move is one that hands something over: the point
// past which what was filled in cannot be taken back. A click is included
// wholesale — the button a run is about to press is a submit far more often
// than not, and the risk question is what stands between that and an approval
// nobody was asked for. Everything else — a scroll, a read, characters going
// into a box — commits nothing by definition and is not worth a round trip.
func commits(m Move) bool {
	switch m.Action {
	case MoveClick:
		return true
	case MoveType, MoveSelect:
		return m.Submit
	case MoveKey:
		// Enter is a submit by another name: the one key that hands a form
		// over, and the only way a key move can commit anything.
		return m.Value == "Enter"
	}
	return false
}

// fingerprint is the page as the loop sees it: where the browser is, what the
// page calls itself, and a hash of the shortlist's role, name and value tuples
// — refs are per observation, the elements behind them are not. Everything in
// it is page-derived, so it is compared and never rendered or emitted.
type fingerprint struct {
	url, title string
	cands      uint64
}

func fingerprintOf(o proto.Observation) fingerprint {
	h := fnv.New64a()
	for _, c := range o.Candidates {
		// Write never fails on a hash.
		_, _ = io.WriteString(h, c.Role+"\x00"+c.Name+"\x00"+c.Value+"\x00"+invalidMark(c)+"\n")
	}
	// What the page is saying is part of what the page is: a submit the site
	// rejected changes no address, no title and no control, and an error
	// message beside the field is the whole of what it did produce. Without
	// this it was a move that changed nothing at all.
	for _, a := range o.Alerts {
		_, _ = io.WriteString(h, "alert\x00"+a+"\n")
	}
	// A dialog is a page of its own laid over this one: a click that opened a
	// calendar changed no address, no title and often no control on the
	// shortlist, and the dialog is the whole of what it did.
	for _, d := range o.Dialogs {
		_, _ = io.WriteString(h, "dialog\x00"+d+"\n")
	}
	return fingerprint{url: o.URL, title: o.Title, cands: h.Sum64()}
}

func invalidMark(c proto.Candidate) string {
	if c.Invalid {
		return "invalid"
	}
	return ""
}

// maxDiffItems caps each list of a diff. What a move did shows in the first
// few changes; a page that replaced two hundred rows has said "it changed"
// after ten of them, and the rest is state a decision call is billed for.
const maxDiffItems = 10

// diff is what changed between the page a move was taken from and the page
// after it: the whole of what an expectation is judged against.
type diff struct {
	URLFrom, URLTo     string
	TitleFrom, TitleTo string
	Added, Removed     []string
	Changed            []string
	// Said and Unsaid are what the page started and stopped telling the user,
	// and Invalidated and Accepted the fields whose own verdict on what they
	// hold changed. A form the site rejected changes nothing else.
	Said, Unsaid          []string
	Invalidated, Accepted []string
	// Opened and Closed are the dialogs the page put over itself and took away
	// again, by name. A dialog that appeared is the one change that makes the
	// rest of a queue moot: it was decided on a page without it.
	Opened, Closed []string
	// New is how many controls the page put on the screen with this move, as
	// the sidecar marks them. The count is the whole of what says a panel
	// appeared on a page that named no dialog; the names are in Added already.
	New int
	// Nothing is true when url, title and shortlist are all identical: the page
	// did not move, whatever the move's own result said.
	Nothing bool
}

// diffOf compares two observations by role, name and value. Refs are per
// observation, so they are no use for it.
func diffOf(before, after proto.Observation) diff {
	d := diff{}
	if before.URL != after.URL {
		d.URLFrom, d.URLTo = before.URL, after.URL
	}
	if before.Title != after.Title {
		d.TitleFrom, d.TitleTo = before.Title, after.Title
	}
	d.Said, d.Unsaid = newIn(after.Alerts, before.Alerts), newIn(before.Alerts, after.Alerts)
	d.Opened, d.Closed = newIn(after.Dialogs, before.Dialogs), newIn(before.Dialogs, after.Dialogs)
	wasBad, nowBad := invalidByName(before.Candidates), invalidByName(after.Candidates)
	for _, k := range sortedNames(nowBad) {
		switch {
		case nowBad[k] == "invalid" && wasBad[k] != "invalid":
			d.Invalidated = append(d.Invalidated, k)
		case nowBad[k] != "invalid" && wasBad[k] == "invalid":
			d.Accepted = append(d.Accepted, k)
		}
	}
	was, now := byName(before.Candidates), byName(after.Candidates)
	for _, k := range sortedNames(now) {
		old, had := was[k]
		switch {
		case !had:
			d.Added = append(d.Added, k)
		case old != now[k]:
			d.Changed = append(d.Changed, k)
		}
	}
	for _, k := range sortedNames(was) {
		if _, still := now[k]; !still {
			d.Removed = append(d.Removed, k)
		}
	}
	d.New = countNew(after.Candidates)
	d.Nothing = fingerprintOf(before) == fingerprintOf(after)
	return d
}

// countNew is how many of a shortlist the sidecar says were not on the
// previous observation of the same tab. It is a count and never a rendering:
// the names of those elements are page content and go through the diff.
func countNew(cands []proto.Candidate) int {
	n := 0
	for _, c := range cands {
		if c.New {
			n++
		}
	}
	return n
}

// byName keys the shortlist by role and name, with the value as the payload:
// what a diff is about is which controls came and went and what is in them.
func byName(cands []proto.Candidate) map[string]string {
	out := make(map[string]string, len(cands))
	for _, c := range cands {
		out[oneLine(c.Role, 40)+" "+quote(c.Name)] = c.Value
	}
	return out
}

// invalidByName keys the shortlist the way byName does, with the page's own
// verdict on each field as the payload.
func invalidByName(cands []proto.Candidate) map[string]string {
	out := make(map[string]string, len(cands))
	for _, c := range cands {
		out[oneLine(c.Role, 40)+" "+quote(c.Name)] = invalidMark(c)
	}
	return out
}

// newIn is the entries of a that are not in b, in order.
func newIn(a, b []string) []string {
	had := make(map[string]bool, len(b))
	for _, s := range b {
		had[s] = true
	}
	var out []string
	for _, s := range a {
		if !had[s] {
			out = append(out, s)
		}
	}
	return out
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// render is the diff as the verify call reads it: page-derived throughout, so
// the caller fences it.
func (d diff) render() string {
	var b strings.Builder
	if d.URLTo != "" {
		fmt.Fprintf(&b, "  the address changed to %s\n", oneLine(d.URLTo, 300))
	}
	if d.TitleTo != "" {
		fmt.Fprintf(&b, "  the page is now called %s\n", quote(d.TitleTo))
	}
	for _, n := range capItems(d.Opened) {
		fmt.Fprintf(&b, "  a dialog opened: %s\n", quote(n))
	}
	for _, n := range capItems(d.Closed) {
		fmt.Fprintf(&b, "  a dialog closed: %s\n", quote(n))
	}
	for _, a := range capItems(d.Said) {
		fmt.Fprintf(&b, "  the page now says: %s\n", quote(a))
	}
	for _, a := range capItems(d.Unsaid) {
		fmt.Fprintf(&b, "  the page no longer says: %s\n", quote(a))
	}
	for _, k := range capItems(d.Invalidated) {
		fmt.Fprintf(&b, "  the field %s is marked invalid\n", k)
	}
	for _, k := range capItems(d.Accepted) {
		fmt.Fprintf(&b, "  the field %s is no longer marked invalid\n", k)
	}
	if d.New > 0 {
		// What the move put on the screen, counted: a page that opened a
		// month of day cells has said "a panel appeared" with the number
		// alone, and each of those cells is already named in Added.
		fmt.Fprintf(&b, "  %d new controls appeared\n", d.New)
	}
	writeList(&b, "appeared", d.Added)
	writeList(&b, "went away", d.Removed)
	writeList(&b, "hold something else now", d.Changed)
	if b.Len() == 0 {
		return "  nothing: the address, the title and every element are as they were.\n"
	}
	return b.String()
}

// capItems is maxDiffItems of a list: a page that marked forty fields invalid
// has said what it has to say in the first few of them.
func capItems(items []string) []string {
	if len(items) > maxDiffItems {
		return items[:maxDiffItems]
	}
	return items
}

func writeList(b *strings.Builder, what string, items []string) {
	if len(items) == 0 {
		return
	}
	shown := items
	if len(shown) > maxDiffItems {
		shown = shown[:maxDiffItems]
	}
	fmt.Fprintf(b, "  %d elements %s: %s", len(items), what, strings.Join(shown, ", "))
	if len(items) > len(shown) {
		fmt.Fprintf(b, ", and %d more", len(items)-len(shown))
	}
	b.WriteString("\n")
}

// loop is the whole of the executor's decision-making. It returns the Result
// the task ends with; done is true when the run reached the goal and its
// answer (when there was one to read) came back.
func (e *Executor) loop(ctx context.Context, r *run) (Result, error) {
	var (
		queue     []Move
		takeovers int
		// waits is how many times in a row a step has been held back for a
		// page that had not finished coming up; any settled page clears it.
		waits int
	)
	obs, err := e.observe(ctx, r, Move{}, moveTextBytes, "", 0)
	if ended(err) {
		return r.result(OutcomeFailed, notesTime), nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("browser: observe: %w", err)
	}
	// A start URL — one the agent may well have constructed — that rendered
	// nothing at all is a fail-fast, not a page to spend a budget on.
	if r.emptyCheckURL != "" && obs.Total == 0 && strings.TrimSpace(obs.Title) == "" {
		return r.result(OutcomeFailed, fmt.Sprintf(emptyPageFmt, r.emptyCheckURL)), nil
	}
	r.emptyCheckURL = ""

	// A page with no text is not a page to make moves on: it is a picture, and
	// the answer is either in it or nowhere. This is before the first move
	// call, so a direct image URL costs one screenshot and no move budget.
	if res, finished, err := e.screenIfThin(ctx, r, obs); err != nil {
		return Result{}, err
	} else if finished {
		return res, nil
	}

	for {
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return r.result(OutcomeFailed, notesTime), nil
			}
			return Result{}, err
		}
		if r.stats.Steps >= e.o.MaxSteps {
			return r.result(OutcomeFailed, notesSteps), nil
		}

		if len(queue) == 0 {
			if e.spent(r) {
				return r.result(OutcomeFailed, outOfBudgetNotes(r)), nil
			}
			mc, err := e.move(ctx, r, obs)
			if err != nil {
				return Result{}, fmt.Errorf("browser: move: %w", err)
			}
			if mc.needsPerson() {
				res, cont := e.person(ctx, r, mc.Blocked, &takeovers)
				if !cont {
					return res, nil
				}
				obs, err = e.reobserve(ctx, r, "", 0)
				if ended(err) {
					return r.result(OutcomeFailed, notesTime), nil
				}
				if err != nil {
					return Result{}, err
				}
				continue
			}
			if mc.Done {
				res, finished, err := e.reach(ctx, r)
				if err != nil {
					return Result{}, err
				}
				if finished {
					return res, nil
				}
				continue
			}
			if len(mc.Moves) == 0 {
				// A page the text model found nothing to do on. Once is a page
				// that may still be settling; twice is a reason to be stuck
				// like any other, answered in the recovery branch.
				r.track.noMove++
				r.note(tried{what: "looked at this page for something to do", effect: effectNoMove})
				if why, held := r.track.stuck(r.moveCalls); held {
					res, cont, err := e.recoverFrom(ctx, r, why, obs, &queue, &takeovers)
					if err != nil {
						return Result{}, err
					}
					if !cont {
						return res, nil
					}
				}
				if len(queue) == 0 {
					obs, err = e.reobserve(ctx, r, "", 0)
					if ended(err) {
						return r.result(OutcomeFailed, notesTime), nil
					}
					if err != nil {
						return Result{}, err
					}
					continue
				}
			} else {
				r.track.noMove = 0
				queue = mc.Moves
			}
		}

		m := queue[0]
		queue = queue[1:]
		start := e.now()
		rung := rungMove

		// The element. A move that named a ref of this observation is already
		// bound; one that only described an element is bound by the decision
		// model, over the candidates of this same observation.
		cands := []proto.Candidate(nil)
		if needsElement(m) && m.Ref == "" {
			cands = obs.Candidates
			rung = rungChoice
		}
		b, err := e.bind(ctx, r, m, obs, cands)
		if err != nil {
			return Result{}, fmt.Errorf("browser: decide: %w", err)
		}
		ref := m.Ref
		if len(cands) > 0 {
			ref = b.Ref
		}
		if b.Invalid || (needsElement(m) && (ref == "" || (len(cands) > 0 && b.Element < e.o.ConfidenceFloor))) {
			// No readable risk answer, or no element to act on: the move is
			// dropped and the rest of the queue with it, because a queue is a
			// sequence and its second move assumed the first.
			e.log(r).Info("browser: a move could not be bound to the page", "action", string(m.Action))
			r.note(tried{what: describeMove(m), expect: m.Expect, effect: effectNoAct})
			queue = nil
			continue
		}

		// Approvals, before anything is committed.
		if b.Risk != RiskNone {
			ok, err := e.o.Humans.Approve(ctx, r.task, b.Risk, riskSummary(m, b.Risk, ref))
			if err != nil || !ok {
				e.emitBlocked(ctx, r, whyApproval)
				return r.result(OutcomeBlocked, notesApprovalRefused), nil
			}
		}

		before := obs
		// Open-and-pick. A box the page says takes no characters — a div the
		// site calls a combobox, which only puts a list on the screen — is
		// opened rather than typed into: what a type move means on one of those
		// is "put the request's value in it", and the way to do that is to open
		// the list and take the option. The value the move carried is dropped
		// here; the pick below is what fills the box in.
		action := actionFor(m, ref)
		opened := opensRatherThanTakes(m, ref, before)
		if opened {
			action = proto.Action{Kind: proto.ActClick, Ref: ref}
			e.log(r).Info("browser: a box that only opens a list is clicked, not typed into")
		} else if action.Submit && answersWithAList(m, ref, before) {
			// Enter in a box that answers what is typed with a list of its
			// own is not a submit but a guess: it takes whatever the site
			// does with raw text, which on a flight search is a page about
			// somewhere else. The pick comes first; a move queued behind
			// this one can still submit once the box holds the page's own
			// value.
			action.Submit = false
			m.Submit = false
			e.log(r).Info("browser: Enter is not pressed in a box that opens a list; the pick comes first")
		}
		res, err := e.act(ctx, r, action)
		if ended(err) {
			return r.result(OutcomeFailed, notesTime), nil
		}
		if err != nil {
			return Result{}, fmt.Errorf("browser: act: %w", err)
		}
		if !res.OK {
			switch res.Code {
			case proto.ErrNotAllowed:
				e.emitBlocked(ctx, r, whyNotAllowed)
				return r.result(OutcomeBlocked, notesNotAllowed), nil
			case proto.ErrHeld:
				// Somebody is driving. Wait for the browser back and try the
				// same move again; nothing is observed meanwhile.
				queue = append([]Move{m}, queue...)
				if err := e.sleep(ctx, heldPoll); err != nil {
					return r.result(OutcomeFailed, notesTime), nil
				}
				continue
			default:
				r.note(tried{what: describeMove(m), expect: m.Expect, effect: effectNoAct})
				queue = nil
			}
		}

		after, err := e.reobserve(ctx, r, ref, wideAfter(ref, before, res))
		if ended(err) {
			return r.result(OutcomeFailed, notesTime), nil
		}
		if err != nil {
			return Result{}, err
		}
		// Whatever is still queued was decided on the page before this move:
		// its refs belong to that observation and not to this one.
		queue = rebind(queue, before, after)
		obs = after
		if !res.OK {
			continue
		}

		// Type-and-pick. A box that answers what is typed with its own list of
		// suggestions is not filled in until one of them is chosen: the
		// characters are in it, but the site takes the value on the choice.
		// The page says so itself — the box reports that it is expanded, or
		// that it opens a list and options have appeared under it — so the
		// executor finishes the job here rather than handing a half-filled
		// form back to the move call.
		// A dialog the move put over the page: a date picker, a passengers
		// panel, a cookie banner. What it asks for has to be answered in it
		// before anything else, so nothing that was queued behind the move
		// stands — and the pick step is not attempted either, because a
		// calendar's days are buttons and gridcells and not a list of options
		// to choose the request's value from. The move call sees the dialog's
		// own controls next turn and decides.
		// A panel the move put up without the page calling it anything is read
		// the same way: a calendar of day cells announces nothing at all, and
		// the queue behind the move was decided on a page without it.
		fx := effectsOf(m, ref, before, after, opened)

		// A box that took none of what was typed. The page says it is a field
		// and Chromium says characters may go into it, and yet the value is
		// exactly what it was: a readonly input driven by something else. It
		// is clicked once, and whatever that puts on the screen is what the
		// move call decides on next — the same treatment a box the page
		// admits takes no characters gets up front.
		clicked := false
		if !fx.busy() && !opened {
			if box, retry := retypeAsClick(m, ref, before, after, res); retry {
				e.log(r).Info("browser: the box did not take what was typed; clicking it instead")
				cres, err := e.act(ctx, r, proto.Action{Kind: proto.ActClick, Ref: box.Ref})
				if ended(err) {
					return r.result(OutcomeFailed, notesTime), nil
				}
				if err != nil {
					return Result{}, fmt.Errorf("browser: act: %w", err)
				}
				if cres.OK {
					clicked, opened = true, true
					next, err := e.reobserve(ctx, r, box.Ref, proto.MaxCandidates)
					if ended(err) {
						return r.result(OutcomeFailed, notesTime), nil
					}
					if err != nil {
						return Result{}, err
					}
					queue = rebind(queue, after, next)
					after, obs = next, next
					fx = effectsOf(m, ref, before, after, opened)
				}
			}
		}

		picked, pickRef, pickTarget := false, "", ""
		if fx.picking {
			next, q, opt, optTarget, worked, err := e.pickFrom(ctx, r, fx.box, before, after, queue)
			if ended(err) {
				return r.result(OutcomeFailed, notesTime), nil
			}
			if err != nil {
				return Result{}, err
			}
			obs, queue = next, q
			if !worked {
				r.note(tried{what: describeMove(m), expect: m.Expect, effect: effectNoPick})
				queue = nil
				continue
			}
			// The box holds one of the page's own values now, which is what the
			// move was for: the rest of the queue was decided on a page where
			// that was about to be true, and it still stands.
			after, picked, pickRef, pickTarget = next, true, opt, optTarget
			r.progress()
		}

		// One round trip, two questions: did the page change the way the move
		// said it would, and is the request satisfied now.
		d := diffOf(before, after)
		v, err := e.verify(ctx, r, m, d, after)
		if err != nil {
			return Result{}, fmt.Errorf("browser: verify: %w", err)
		}
		// A type the box took nothing of and a click that opened nothing
		// either: the field is as empty as it was and the value is nowhere.
		notTyped := clicked && d.Nothing
		if fx.dialog || fx.panel || notTyped {
			// Judged first, dropped afterwards: the verify call is what says
			// whether the move itself did what it was for, and only then is
			// the rest of the queue — decided on a page without this dialog,
			// or behind a field that was never filled in — thrown away.
			queue = nil
		}
		if rung >= 0 && rung < len(r.stats.Rungs) {
			r.stats.Rungs[rung]++
		}
		e.emit(ctx, r, event.BrowserStep, stepAttrs{
			N: r.stats.Steps, Action: m.Action, Ref: ref, Role: safeRole(b.Role),
			EffectP: v.Effect, GoalP: v.Goal, LoadP: v.Load, Risk: b.Risk, Rung: rung,
			Target: moveTarget(Move{Action: m.Action, Ref: ref, URL: m.URL}, before),
			Picked: picked, PickRef: pickRef, PickTarget: pickTarget, Opened: opened, Panel: fx.panel,
			URL: r.stepURL(after.URL), TookMS: e.now().Sub(start).Milliseconds(),
		})

		// A page caught mid-render is not a move that failed: the site is
		// simply slower than the loop. The queue stands, nothing goes into the
		// history as a failure, and the page is read again once it has settled
		// — the same gate the observation itself applies to a page that admits
		// it is loading, for the spinners and skeletons it cannot see.
		if v.Effect < effectFloor && v.Goal < goalFloor && v.Load >= loadFloor && waits < maxLoadWaits {
			waits++
			e.log(r).Info("browser: waiting for a page that had not finished coming up")
			next, err := e.settle(ctx, r, after, ref)
			if err != nil {
				return Result{}, err
			}
			queue = rebind(queue, after, next)
			obs = next
			continue
		}
		waits = 0

		// What the move did to the run's trajectory, as facts: the goal the
		// page now scores against anything it scored before, and whatever the
		// move took off the page that the site was saying. Both are progress;
		// the move's own effect score is not, because a control that opens and
		// closes scores well every time and gets nowhere.
		if r.track.scored(v, r.moveCalls) {
			r.progress()
		}
		if r.track.cleared(d) {
			r.progress()
		}
		if v.Effect < effectFloor && ref != "" {
			if c, ok := candidateByRef(before.Candidates, ref); ok {
				r.track.failedOn(c)
			}
		}

		effect := effectWorked
		switch {
		case picked:
			// The suggestion was taken, which is the move's own post-condition
			// whatever else the page did with it.
			effect = effectPicked
		case notTyped:
			// Said outright rather than as "nothing changed": what the move
			// call has to know is that this field does not take characters.
			effect = effectNotTyped
		case v.Effect < effectFloor:
			effect = effectNoChange
			if !d.Nothing {
				effect = effectNoEffect
			}
		case fx.dialog:
			// It worked, and it put something in front of the page that has to
			// be dealt with before anything else.
			effect = effectOpenedDialog
		case fx.panel:
			// The same, for a page that put its controls up and named none of
			// them: what it asks for is among them and nowhere else.
			effect = effectOpenedPanel
		}
		r.note(tried{what: describeMove(m), expect: m.Expect, effect: effect})

		// A move that went somewhere and landed on a page with no text: the
		// same picture the loop cannot work on, arrived at rather than started
		// from — a thumbnail clicked, a "view the full chart" link followed.
		if m.Action == MoveNavigate || m.Action == MoveClick {
			res, finished, err := e.screenIfThin(ctx, r, obs)
			if err != nil {
				return Result{}, err
			}
			if finished {
				return res, nil
			}
		}

		if v.Goal >= goalFloor {
			res, finished, err := e.reach(ctx, r)
			if err != nil {
				return Result{}, err
			}
			if finished {
				return res, nil
			}
			queue = nil
			continue
		}

		// A dead end, as a fact rather than as a model's opinion of itself:
		// the page keeps coming back, an element keeps failing, the goal has
		// not moved in three turns. The next move call on the same page with
		// the same history would answer the same way, so the run is asked
		// instead what else there is to do (recover.go).
		if why, held := r.track.stuck(r.moveCalls); held {
			res, cont, err := e.recoverFrom(ctx, r, why, obs, &queue, &takeovers)
			if err != nil {
				return Result{}, err
			}
			if !cont {
				return res, nil
			}
			obs, err = e.reobserve(ctx, r, "", 0)
			if ended(err) {
				return r.result(OutcomeFailed, notesTime), nil
			}
			if err != nil {
				return Result{}, err
			}
			continue
		}

		if v.Effect < effectFloor {
			// The move did not do what it was for: the queue behind it assumed
			// that it had, so it goes, and the move call is asked again with
			// the failure in its history.
			queue = nil
			continue
		}
		if d.URLTo != "" {
			// A new page: every ref the rest of the queue holds belongs to the
			// page before it.
			queue = nil
		}
	}
}

// rebind carries a queue from the page it was decided on to the page as it is
// now. A queued move holds a ref of the observation the move call was shown,
// and every action produces a new observation with new refs: the element the
// ref named may have moved in document order, or gone.
//
// So the element is found again by what it is — its role and its accessible
// name — rather than by the handle it had. A move whose element cannot be found
// again, or that now matches more than one, ends the queue there: the moves
// behind it assumed it, and clicking the wrong thing is worse than asking the
// move call again. A move with no ref is left alone — the decision model binds
// it against the current page when its turn comes.
func rebind(queue []Move, before, after proto.Observation) []Move {
	out := make([]Move, 0, len(queue))
	for _, m := range queue {
		if m.Ref == "" {
			out = append(out, m)
			continue
		}
		was, ok := candidateByRef(before.Candidates, m.Ref)
		if !ok {
			return out
		}
		now, ok := sameElement(after.Candidates, was)
		if !ok {
			return out
		}
		m.Ref = now.Ref
		out = append(out, m)
	}
	return out
}

// sameElement finds the one candidate of a fresh observation that is the
// element an older one named. Exactly one: a page with two buttons of the same
// name is a page whose queue is not worth guessing at.
func sameElement(cands []proto.Candidate, want proto.Candidate) (proto.Candidate, bool) {
	var hit proto.Candidate
	n := 0
	for _, c := range cands {
		if strings.EqualFold(c.Role, want.Role) && c.Name == want.Name {
			hit, n = c, n+1
		}
	}
	return hit, n == 1
}

// listBox is the trigger of the type-and-pick step: the box a type or select
// move just went into, as the page reports it afterwards, when the page is
// waiting for one of its own suggestions to be chosen. The page's own words
// are what decides — the box says it is expanded, or it says it opens a list
// and options have appeared under it. Nothing here guesses from the shape of
// the shortlist alone: a page that grew a list of its own accord is not a box
// that has to be picked from.
//
// A native <select> never reports itself expanded after being typed into, so
// the step does not trigger for one: typing already committed the option.
func listBox(m Move, ref string, before, after proto.Observation) (proto.Candidate, bool) {
	if ref == "" || (m.Action != MoveType && m.Action != MoveSelect) {
		return proto.Candidate{}, false
	}
	was, ok := candidateByRef(before.Candidates, ref)
	if !ok {
		return proto.Candidate{}, false
	}
	box, ok := sameElement(after.Candidates, was)
	if !ok {
		return proto.Candidate{}, false
	}
	if box.Expanded != nil && *box.Expanded {
		return box, true
	}
	// Options that say themselves which box they came out of: the page has
	// named the list, so nothing has to be inferred from what grew.
	if len(optionsIn(after.Candidates, box.Ref)) > len(optionsIn(before.Candidates, ref)) {
		return box, true
	}
	// Counted rather than compared by name: the names in such a list are the
	// page's own words about what was typed and change with every character.
	if box.HasPopup != "" && len(optionsOf(after.Candidates)) > len(optionsOf(before.Candidates)) {
		return box, true
	}
	return proto.Candidate{}, false
}

// opensRatherThanTakes reports whether a type or select move named a box that
// takes no characters at all: the page says it is not editable, and that it
// opens something or is one of the roles a site builds out of a div and a list.
// Typing into one of those puts the characters nowhere and leaves the form
// empty; clicking it is what a person does, and the pick step finishes the job.
func opensRatherThanTakes(m Move, ref string, obs proto.Observation) bool {
	if ref == "" || (m.Action != MoveType && m.Action != MoveSelect) {
		return false
	}
	c, ok := candidateByRef(obs.Candidates, ref)
	if !ok || c.Editable {
		return false
	}
	if c.HasPopup != "" {
		return true
	}
	// The roles a site builds a value out of a div and a list with, and the
	// one the renfe date field is: a readonly input the accessibility tree
	// still calls a textbox. Characters go nowhere in any of them — which is
	// exactly what the rendering already tells the model as `click to open`.
	switch strings.ToLower(strings.TrimSpace(c.Role)) {
	case "combobox", "listbox", "textbox", "searchbox":
		return true
	}
	return false
}

// afterEffects is what a page did with a move beyond the move's own result: a
// dialog it named, a list it opened to be picked from, a panel of controls it
// put up without calling it anything. Each of them is a page the rest of a
// queue was not decided on.
type afterEffects struct {
	dialog bool
	panel  bool
	// box is the box a pick is due from, and picking says there is one.
	box     proto.Candidate
	picking bool
}

// busy reports whether the page put something in front of the move. Nothing
// else is worth trying until it has been dealt with.
func (fx afterEffects) busy() bool { return fx.dialog || fx.panel || fx.picking }

// effectsOf reads the three off the pages either side of a move. opened says
// the executor carried the move out as a click that opens — an open-and-pick,
// or a type the box took nothing of — which is what makes a panel behind it
// the move's own doing.
func effectsOf(m Move, ref string, before, after proto.Observation, opened bool) afterEffects {
	fx := afterEffects{dialog: len(newIn(after.Dialogs, before.Dialogs)) > 0}
	if box, ok := listBox(m, ref, before, after); ok && !fx.dialog {
		fx.box, fx.picking = box, true
	}
	fx.panel = openedAPanel(m, ref, before, after, opened, fx)
	return fx
}

// openedAPanel is the dialog guard for what a page opens without admitting to
// a dialog at all. renfe's calendar is a div of day cells with no role and no
// name: nothing is announced, no list of options appears, the address does not
// change, and the whole of what happened is that controls the page did not
// have are on it now.
//
// It fires on a move that worked, left the address alone and was aimed at
// something that opens rather than takes — an element the page says is not
// editable, one that admits to a popup, a click on a control, or a move the
// executor itself turned into a click — after which the page grew controls
// that are not a list to pick from and named no dialog. Like a dialog, what
// the panel asks for comes before whatever was queued behind the move.
func openedAPanel(m Move, ref string, before, after proto.Observation, opened bool, fx afterEffects) bool {
	if fx.dialog || fx.picking || ref == "" || before.URL != after.URL {
		return false
	}
	if countNew(after.Candidates) == 0 {
		return false
	}
	c, ok := candidateByRef(before.Candidates, ref)
	if !ok {
		return false
	}
	if opened || !c.Editable || c.HasPopup != "" {
		return true
	}
	// A box characters do go into, clicked rather than typed into: the click
	// is what opens the panel beside it.
	if m.Action != MoveClick {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(c.Role)) {
	case "button", "textbox", "combobox":
		return true
	}
	return false
}

// wideAfter is the shortlist bound the observation right after a move asks
// for. A move on something that opens rather than takes — an element the page
// says is not editable, one that admits to a popup — is the one whose result
// is a screenful of controls that were not there before, and a month of day
// cells is forty of them: the default 25 would cut it in half and the move
// call that follows would be shown half a calendar. Everywhere else the
// default stands, because every question is billed for the candidates it is
// answered over — a navigation included, where the page that arrives is all
// new and none of it was opened.
func wideAfter(ref string, before proto.Observation, res proto.ActResult) int {
	if ref == "" || res.Navigated {
		return 0
	}
	c, ok := candidateByRef(before.Candidates, ref)
	if !ok {
		return 0
	}
	if !c.Editable || c.HasPopup != "" {
		return proto.MaxCandidates
	}
	return 0
}

// retypeAsClick reports whether a type left the box exactly as it found it —
// the characters went nowhere — and names the box as the page holds it now, to
// be clicked instead. A readonly field Chromium still calls editable is the
// case it was written for: the value does not move, the form stays empty, and
// what a person does is click the field and use what it puts on the screen.
//
// A select is not one of these: typing committed the option. Nor is a secret
// field, whose value is never populated, so "unchanged" says nothing about it.
// Nor is a move that navigated — the box is on another page now.
func retypeAsClick(m Move, ref string, before, after proto.Observation, res proto.ActResult) (proto.Candidate, bool) {
	if m.Action != MoveType || ref == "" || res.Navigated {
		return proto.Candidate{}, false
	}
	was, ok := candidateByRef(before.Candidates, ref)
	if !ok || was.Secret {
		return proto.Candidate{}, false
	}
	now, ok := sameElement(after.Candidates, was)
	if !ok || now.Value != was.Value {
		return proto.Candidate{}, false
	}
	return now, true
}

// answersWithAList reports whether a type or select move is aimed at a box the
// page says opens a list — the autocomplete whose value is one of its own
// suggestions and not the characters typed.
func answersWithAList(m Move, ref string, obs proto.Observation) bool {
	if ref == "" || (m.Action != MoveType && m.Action != MoveSelect) {
		return false
	}
	c, ok := candidateByRef(obs.Candidates, ref)
	if !ok {
		return false
	}
	return c.HasPopup != "" || (c.Expanded != nil && *c.Expanded)
}

// optionsIn is the options that belong to one box: those whose In chain leads
// to it, whether directly or through the listbox the box owns.
func optionsIn(cands []proto.Candidate, ref string) []proto.Candidate {
	if ref == "" {
		return nil
	}
	var out []proto.Candidate
	for _, c := range optionsOf(cands) {
		if belongsTo(cands, c, ref) {
			out = append(out, c)
		}
	}
	return out
}

// maxInDepth bounds the walk up an In chain: a page that made one a cycle, or
// nested its lists twenty deep, is not worth following.
const maxInDepth = 8

// belongsTo walks one candidate's In chain upwards and reports whether it
// reaches ref.
func belongsTo(cands []proto.Candidate, c proto.Candidate, ref string) bool {
	for i := 0; i < maxInDepth && c.In != ""; i++ {
		if c.In == ref {
			return true
		}
		next, ok := candidateByRef(cands, c.In)
		if !ok {
			return false
		}
		c = next
	}
	return false
}

// optionsOf is the candidates of a shortlist that are things to choose from.
func optionsOf(cands []proto.Candidate) []proto.Candidate {
	var out []proto.Candidate
	for _, c := range cands {
		switch strings.ToLower(strings.TrimSpace(c.Role)) {
		case "option", "menuitem", "treeitem":
			out = append(out, c)
		}
	}
	return out
}

// newOptions is the option-like candidates of after that before did not have,
// by role and name — the list a box just opened, as distinct from the menus
// the page always had.
func newOptions(before, after []proto.Candidate) []proto.Candidate {
	had := make(map[string]bool, len(before))
	for _, c := range optionsOf(before) {
		had[strings.ToLower(c.Role)+"\x00"+c.Name] = true
	}
	var out []proto.Candidate
	for _, c := range optionsOf(after) {
		if !had[strings.ToLower(c.Role)+"\x00"+c.Name] {
			out = append(out, c)
		}
	}
	return out
}

// pickFrom carries out the pick: let the list finish drawing itself, ask the
// decision model which of the page's own suggestions the request means, click
// it, and read the page again. It returns the page to carry on from, the queue
// carried across everything it did, the option's ref and its name for the
// trace, and whether the box took the value.
//
// before is the page the box was typed into, obs the page as it stands with
// the list open, and box the box on it. Every observation it makes is read
// around the box, so what the site says about the field arrives next to the
// field.
func (e *Executor) pickFrom(ctx context.Context, r *run, box proto.Candidate, before, obs proto.Observation, queue []Move) (proto.Observation, []Move, string, string, bool, error) {
	next, err := e.settle(ctx, r, obs, box.Ref)
	if err != nil {
		return obs, queue, "", "", false, err
	}
	queue = rebind(queue, obs, next)
	if b, ok := sameElement(next.Candidates, box); ok {
		box = b
	}
	// What is offered is the list this box opened, not every menu item on the
	// page: a site's navigation is made of options too, and a model offered the
	// whole page can pick a department instead of a station. Best of all is the page saying which box each option came out of: two
	// lists can be open at once — a trip type and a cabin class — and only one
	// of them is the box this move was about. Failing that the suggestions are
	// the option-like elements that were not there before; a page whose list was
	// already drawn — empty, waiting — is offered them all.
	opts := optionsIn(next.Candidates, box.Ref)
	if len(opts) == 0 {
		opts = newOptions(before.Candidates, next.Candidates)
	}
	if len(opts) == 0 {
		opts = optionsOf(next.Candidates)
	}
	if len(opts) == 0 {
		// The list went away while it was being waited for: nothing to pick,
		// and the box holds characters the site has not taken.
		return next, queue, "", "", false, nil
	}

	choice, err := e.pick(ctx, r, next, box, opts)
	if err != nil {
		return next, queue, "", "", false, fmt.Errorf("browser: decide: %w", err)
	}
	opt, ok := candidateByRef(opts, choice)
	if !ok {
		return next, queue, "", "", false, nil
	}
	res, err := e.act(ctx, r, proto.Action{Kind: proto.ActClick, Ref: opt.Ref})
	if err != nil {
		return next, queue, "", "", false, err
	}
	if !res.OK {
		return next, queue, "", "", false, nil
	}
	after, err := e.reobserve(ctx, r, box.Ref, 0)
	if err != nil {
		return next, queue, "", "", false, err
	}
	queue = rebind(queue, next, after)
	return after, queue, opt.Ref, moveTarget(Move{Action: MoveClick, Ref: opt.Ref}, next), tookTheValue(box, next, after), nil
}

// tookTheValue reports whether the box ended up holding one of the page's own
// values. The box itself is the evidence when it can be found again: the list
// is closed and it holds something. A page that replaced the box outright — a
// chip, a tag, a row in place of the field — is read off the list instead: the
// suggestions are gone and the browser is still on the same page, which is
// what choosing one looks like from outside.
func tookTheValue(box proto.Candidate, before, after proto.Observation) bool {
	if b, ok := sameElement(after.Candidates, box); ok {
		return (b.Expanded == nil || !*b.Expanded) && b.Value != ""
	}
	return len(optionsOf(after.Candidates)) == 0 && after.URL == before.URL
}

// reach is the end of a run that believes it is there: the answer read off the
// page, done reported. An extraction that came back empty is not a done task
// with nothing to say — it is the goal not having been reached, so it goes into
// the history and the loop carries on while there is budget for it.
func (e *Executor) reach(ctx context.Context, r *run) (Result, bool, error) {
	return e.reachAs(ctx, r, "read the answer off this page")
}

// reachAs is reach with the history line it leaves when the page did not
// answer: what was tried is not the same thing when the run went to the page's
// pixels before it ever made a move.
func (e *Executor) reachAs(ctx context.Context, r *run, what string) (Result, bool, error) {
	answer, err := e.extract(ctx, r)
	// The screenshot the extraction read, if it read one, is reported once the
	// run knows whether it is also the picture it returns.
	read, use := r.read, shotKept
	r.read = nil
	defer func() { e.emitShot(ctx, r, read, use) }()
	if ended(err) {
		return r.result(OutcomeFailed, notesTime), true, nil
	}
	if errors.Is(err, errPartial) {
		// Part of the answer is on the page: the move call is told so, and
		// what it does about it — scroll, "show more", another tab — is its
		// call. The move budget bounds it like anything else.
		r.note(tried{what: what, effect: effectPartial})
		if e.spent(r) {
			return r.result(OutcomeFailed, outOfBudgetNotes(r)), true, nil
		}
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if strings.TrimSpace(answer) == "" {
		r.note(tried{what: what, effect: effectNoAnswer})
		if e.spent(r) {
			return r.result(OutcomeFailed, outOfBudgetNotes(r)), true, nil
		}
		return Result{}, false, nil
	}
	res := r.result(OutcomeDone, r.partialNotes)
	res.Answer = answer
	res.Screenshot, use = e.picture(ctx, r, answer, read)
	return res, true, nil
}

// screenIfThin is the extraction offered a page the move loop cannot work
// on at all. A direct image URL — a chart, a scanned menu, a photograph — is
// observed as no text and one nameless control, and the loop's only exits are
// a move call saying "done" and a goal score: neither can happen on a page
// with nothing on it, so the run spends its budget clicking at a picture and
// dies stuck. So the page is read off the screen before any move, and if what
// is on it answers the request the run is over.
//
// Nothing happens without a model that can look at a page, which is what keeps
// this off every run that has no reader. One read per page: a zoom click that
// comes back to the same picture does not pay for a second screenshot.
func (e *Executor) screenIfThin(ctx context.Context, r *run, obs proto.Observation) (Result, bool, error) {
	// A page that is not a web page — the blank tab a run without a start URL
	// opens on — has nothing to read off the screen either.
	if _, _, ok := e.reader(); !ok || !isHTTPURL(obs.URL) || !thinPage(obs) {
		return Result{}, false, nil
	}
	fp := fingerprintOf(obs)
	if r.screenedAt == fp {
		return Result{}, false, nil
	}
	r.screenedAt = fp
	e.log(r).Info("browser: the page has no text to act on; reading it off the screen")
	return e.reachAs(ctx, r, notesTriedScreen)
}

// notesTriedScreen is what the history says when the screen was read before
// any move and did not hold the answer: fixed words, like every other line the
// move call is shown about itself.
const notesTriedScreen = "read this page off the screen"

// person is the takeover a move call asked for: a page that cannot be passed
// without somebody signing in, answering a captcha or reading a code. Detached
// runs fail fast — the Humans semantics of docs/browser.md: nobody watching, no waiting.
func (e *Executor) person(ctx context.Context, r *run, why Blocker, takeovers *int) (Result, bool) {
	e.emitBlocked(ctx, r, string(why))
	if r.task.Detached {
		return r.result(OutcomeBlocked, notesNeedsPerson(why, true)), false
	}
	*takeovers++
	if *takeovers > maxTakeovers {
		return r.result(OutcomeBlocked, notesNeedsPerson(why, false)), false
	}
	ok, err := e.o.Humans.Takeover(ctx, r.task, why)
	if err != nil || !ok {
		return r.result(OutcomeBlocked, notesNeedsPerson(why, false)), false
	}
	r.stats.Rungs[rungPerson]++
	return Result{}, true
}

// look is rung 5: one vision call for a description of what can be clicked —
// never coordinates, because nothing a model says may drive input — turned
// into a move whose element the decision model binds like any other.
func (e *Executor) look(ctx context.Context, r *run, obs proto.Observation) (Move, bool) {
	shot := e.shoot(ctx, r, ShotLook, obs)
	if shot == nil {
		return Move{}, false
	}
	e.emitShot(ctx, r, shot, shotKept)
	resp, err := e.chat(ctx, e.o.VisionModel, e.o.Vision,
		fmt.Sprintf(visionSystemFmt, oneLine(r.task.Intent, 200)), visionReserve, provider.ReasoningNone,
		provider.Message{Role: provider.RoleUser, Parts: []provider.Part{
			{Image: &provider.Image{MIME: "image/jpeg", Data: shot.data}},
			{Text: "which control is it?"},
		}})
	if err != nil {
		e.log(r).Info("browser: the vision rung could not look at the page")
		return Move{}, false
	}
	r.stats.GenerativeCall++
	r.count(resp.Usage)
	desc := oneLine(resp.Text, 200)
	if desc == "" || len(obs.Candidates) == 0 {
		return Move{}, false
	}
	return Move{Action: MoveClick, Describe: desc, Expect: visionExpect}, true
}

// visionExpect is what a move the vision rung invented is judged against. The
// description came off a screenshot, so it is page-derived and is not reused as
// an expectation; what is expected of it is fixed words.
const visionExpect = "the page moves on from what it was showing"

// waitReleased suspends the run while a person holds the agent's takeover lock
// (docs/browser.md: the executor makes no observation while a person is driving). It guards
// every observation and every action, so neither the page a person navigated to
// nor an action of ours can cross a takeover. An error only ever means the run
// is over, which is a Result, not a Go error.
func (e *Executor) waitReleased(ctx context.Context, r *run) error {
	if err := e.o.Humans.WaitReleased(ctx, r.task); err != nil {
		return fmt.Errorf("%w: %w", errRunEnded, err)
	}
	return nil
}

// reobserve is the observation after an action: the same shape as the one the
// move call was made on, because it is what the next one is made on. around is
// the ref of the element the action was done to, so a page too long to read
// whole is read where the action happened rather than from the top; "" when
// there was no element.
// max is the shortlist bound, 0 for the default: the one observation that asks
// for more is the one right after a move that opened something.
func (e *Executor) reobserve(ctx context.Context, r *run, around string, max int) (proto.Observation, error) {
	obs, err := e.observe(ctx, r, Move{}, moveTextBytes, around, max)
	if err != nil && !ended(err) {
		return proto.Observation{}, fmt.Errorf("browser: observe: %w", err)
	}
	return obs, err
}

// settle is what the run does when the decision model says the page was caught
// mid-render although Chromium considers it loaded — an XHR still out, a list
// still filling in. It asks the browser to wait for the page to change and then
// go quiet (proto.WaitChange), and reads it again. A page that never changes
// costs loadWait once and is then decided on as it stands.
func (e *Executor) settle(ctx context.Context, r *run, obs proto.Observation, around string) (proto.Observation, error) {
	if _, err := e.waitQuiet(ctx, r, proto.WaitChange, settleWait); err != nil {
		return obs, nil // the run is over; the caller's ctx check ends it
	}
	next, err := e.observeOnce(ctx, r, Move{}, moveTextBytes, around, 0)
	if ended(err) {
		return obs, nil
	}
	if err != nil {
		return proto.Observation{}, err
	}
	return next, nil
}

// waitQuiet hands the waiting to the browser: one proto.ActWait, which returns
// when Chromium's lifecycle events and the document's own quiescence say the
// page has stopped, or when loadWait has passed.
//
// It does not count against MaxSteps — nothing was done to the page, and a run
// that waited three times has not used up three of the actions it is allowed —
// but it is on the timeline like any other step, because a wait is the thing a
// person reading a slow run most needs to see.
func (e *Executor) waitQuiet(ctx context.Context, r *run, until proto.WaitUntil, bound time.Duration) (bool, error) {
	if err := e.waitReleased(ctx, r); err != nil {
		return false, err
	}
	if until == "" {
		until = proto.WaitQuiet
	}
	start := e.now()
	r.stats.Waits++
	res, err := e.o.Sidecar.Act(ctx, r.task.Agent, proto.Action{
		Kind: proto.ActWait, Until: until, TimeoutMS: int(bound.Milliseconds()),
	})
	if err != nil {
		return false, err
	}
	if res.URL != "" {
		r.url = res.URL
	}
	e.emit(ctx, r, event.BrowserStep, waitAttrs{
		N: r.stats.Steps, Action: string(MoveWait), Until: string(until),
		Quiet: res.Quiet, TookMS: e.now().Sub(start).Milliseconds(),
	})
	return res.Quiet, nil
}

// observe asks the sidecar for the page: the shortlist a move is chosen from
// (25 candidates, where recall stops improving — measured on real pages) and an
// excerpt of the text, because the move call reads the page and not only its
// controls.
//
// A page that says it is still loading is given until maxLoadWaits × loadPoll
// to finish before it is handed to anything that decides on it: a half-rendered
// page is where a move is chosen from controls that are about to move and where
// an answer is read off text that has not arrived. This is the cheap half of
// the gate — the sidecar's own lifecycle flag, no model involved; the load
// question of the post-move call is the half that catches what the flag cannot
// see.
func (e *Executor) observe(ctx context.Context, r *run, m Move, textBytes int, around string, max int) (proto.Observation, error) {
	for waits := 0; ; waits++ {
		obs, err := e.observeOnce(ctx, r, m, textBytes, around, max)
		if err != nil || !obs.Loading || waits == maxLoadWaits {
			return obs, err
		}
		if waits == 0 {
			e.log(r).Info("browser: the page says it is still loading; waiting for it")
		}
		if _, err := e.waitQuiet(ctx, r, proto.WaitQuiet, loadWait); err != nil {
			// The run is over. The page as it stands is still a valid
			// observation; the caller's own ctx check ends the run.
			return obs, nil
		}
	}
}

// observeOnce is one read of the page, with no waiting of its own.
func (e *Executor) observeOnce(ctx context.Context, r *run, m Move, textBytes int, around string, max int) (proto.Observation, error) {
	if err := e.waitReleased(ctx, r); err != nil {
		return proto.Observation{}, err
	}
	if max <= 0 {
		max = proto.DefaultMaxCandidates
	}
	obs, err := e.o.Sidecar.Observe(ctx, r.task.Agent, proto.ObserveRequest{
		Hints:         moveHints(m),
		MaxCandidates: max,
		TextBytes:     textBytes,
		TextAround:    around,
	})
	if err != nil {
		return proto.Observation{}, err
	}
	if obs.URL != "" {
		r.url = obs.URL
	}
	if obs.Title != "" {
		r.title = obs.Title
	}
	if len(obs.Candidates) > proto.MaxCandidates {
		obs.Candidates = obs.Candidates[:proto.MaxCandidates]
	}
	if r.firstURL == "" && isHTTPURL(obs.URL) {
		// Where a restart goes back to: the first web page this run ever
		// read, which is the start URL when the task gave one and the first
		// site it went to when it did not.
		r.firstURL = obs.URL
	}
	// Every read of the page goes through here, so this is where the run's own
	// trajectory is booked: a page it has never been at is progress, and the
	// fingerprints are what say it keeps coming back to the same one.
	if r.track.observed(obs, r.moveCalls) {
		r.progress()
	}
	return obs, nil
}

// act sends one action and books what it did: the step count, where the
// browser ended up, and any file it saved.
func (e *Executor) act(ctx context.Context, r *run, a proto.Action) (proto.ActResult, error) {
	if err := e.waitReleased(ctx, r); err != nil {
		return proto.ActResult{}, err
	}
	res, err := e.o.Sidecar.Act(ctx, r.task.Agent, a)
	r.stats.Steps++
	if err != nil {
		return proto.ActResult{}, err
	}
	if res.URL != "" {
		r.url = res.URL
	}
	r.downloads = append(r.downloads, res.Downloads...)
	return res, nil
}

// roles is the closed set a candidate's role is reported as in the trace.
// A role comes off the page like everything else, so anything outside the set
// is reported as "other" rather than passed through.
var roles = map[string]bool{
	"button": true, "link": true, "textbox": true, "searchbox": true, "combobox": true,
	"checkbox": true, "radio": true, "listbox": true, "option": true, "menuitem": true,
	"tab": true, "switch": true, "slider": true, "spinbutton": true, "textarea": true,
	"canvas": true, "image": true, "heading": true, "cell": true, "gridcell": true,
	"row": true, "dialog": true, "form": true, "generic": true,
}

func safeRole(role string) string {
	if role == "" {
		return ""
	}
	r := strings.ToLower(strings.TrimSpace(role))
	if roles[r] {
		return r
	}
	return "other"
}

// notesNeedsPerson is the one line a blocked task reports. Fixed words only.
func notesNeedsPerson(b Blocker, detached bool) string {
	what := map[Blocker]string{
		BlockLogin:   "a sign-in",
		BlockCaptcha: "a captcha",
		Block2FA:     "a verification code",
	}[b]
	if what == "" {
		what = "a person"
	}
	if detached {
		return "the site asked for " + what + " and this run has nobody watching"
	}
	return "the site asked for " + what + " and nobody took over in time"
}
