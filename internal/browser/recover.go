package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"holaclaw/internal/browser/proto"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// What a stuck run does about it. Until this file the only re-plan was the
// next move call with six history lines, which is the same model looking at
// the same page and answering the same question: a run in a dead end stayed in
// it until the budget ran out.
//
// The choice itself is the decision model's, over a closed set of things this
// package knows how to carry out — the shape of every other judgement in the
// loop. What is generative here is only the one thing that has to be written:
// the address of a deep link.

// strategy is the closed set of ways out of a dead end.
type strategy string

const (
	stratRetryOther strategy = "retry_other" // something else on this page
	stratRestart    strategy = "restart"     // back to the start, another route
	stratDeeplink   strategy = "deeplink"    // put the search in the address
	stratLook       strategy = "look"        // a screenshot of what the text misses
	stratGiveUp     strategy = "give_up"     // nothing here will do it
	// stratPerson is never offered as a choice: it is what a give_up becomes
	// on a run somebody is watching, when they take the keyboard.
	stratPerson strategy = "person"
)

// maxRecoveries is how often one run may be pulled out of a dead end. A third
// reason to be stuck is a run that is not going to finish, and asking again
// costs a round trip to be told so.
const maxRecoveries = 2

// stuckAttrs is the gate event of the recovery branch: the run held, and why,
// in this package's own words. It is a browser.step because what a person
// reading a trace wants is one timeline of what the browser did, and the turn
// where a run admitted it was going nowhere is the line on it they want most.
type stuckAttrs struct {
	N      int    `json:"n"`
	Held   string `json:"held"`
	Why    string `json:"why"`
	TookMS int64  `json:"took_ms"`
}

// recoverAttrs is what was done about it: one word of the closed set above.
type recoverAttrs struct {
	N        int    `json:"n"`
	Action   string `json:"action"`
	Strategy string `json:"strategy"`
	TookMS   int64  `json:"took_ms"`
}

// recoverFrom is the whole of the branch: say the run is stuck, choose what to do
// about it, and do it. cont is false when the Result ends the run; queue is
// what the loop carries on with.
func (e *Executor) recoverFrom(ctx context.Context, r *run, why stuckReason, obs proto.Observation, queue *[]Move, takeovers *int) (Result, bool, error) {
	start := e.now()
	e.log(r).Info("browser: the run is stuck", "why", string(why))
	e.emit(ctx, r, event.BrowserStep, stuckAttrs{
		N: r.stats.Steps, Held: "stuck", Why: string(why),
		TookMS: e.now().Sub(start).Milliseconds(),
	})
	r.note(tried{what: stuckWords[why]})

	choice := stratGiveUp
	if r.stats.Recoveries < maxRecoveries {
		c, err := e.strategy(ctx, r, why, obs)
		if err != nil {
			return Result{}, false, fmt.Errorf("browser: decide: %w", err)
		}
		choice = c
	}
	r.stats.Recoveries++

	switch choice {
	case stratGiveUp:
		// The person rung sits above the failure rung (docs/browser.md): before a run
		// that somebody is watching is given up on, that somebody is offered
		// the keyboard once. A detached run has nobody to offer it to.
		if !r.task.Detached && *takeovers < maxTakeovers {
			*takeovers++
			e.emitBlocked(ctx, r, string(BlockError))
			if ok, err := e.o.Humans.Takeover(ctx, r.task, BlockError); err == nil && ok {
				r.stats.Rungs[rungPerson]++
				*queue = nil
				e.emitRecover(ctx, r, stratPerson, start)
				return Result{}, true, nil
			}
		}
		r.stats.Rungs[rungFailure]++
		e.emitRecover(ctx, r, choice, start)
		return r.result(OutcomeFailed, gaveUpNotes(r, why)), false, nil

	case stratRestart:
		res, cont, err := e.restart(ctx, r, why)
		if err != nil || !cont {
			return res, cont, err
		}
		*queue = nil
		e.emitRecover(ctx, r, choice, start)
		return Result{}, true, nil

	case stratDeeplink:
		res, cont, ok, err := e.deeplink(ctx, r, obs)
		if err != nil || !cont {
			return res, cont, err
		}
		if ok {
			*queue = nil
			e.emitRecover(ctx, r, choice, start)
			return Result{}, true, nil
		}
		// No address worth navigating to: what is left is this page.
		choice = stratRetryOther

	case stratLook:
		if m, ok := e.look(ctx, r, obs); ok {
			r.vision = true
			r.stats.Rungs[rungVision]++
			*queue = []Move{m}
			e.emitRecover(ctx, r, choice, start)
			return Result{}, true, nil
		}
		choice = stratRetryOther
	}

	// retry_other, and everything that fell through to it: the page stands,
	// the queue does not, and the next move call is told what has already
	// failed on it.
	r.avoid = r.track.avoid()
	*queue = nil
	e.emitRecover(ctx, r, choice, start)
	return Result{}, true, nil
}

func (e *Executor) emitRecover(ctx context.Context, r *run, s strategy, start time.Time) {
	e.emit(ctx, r, event.BrowserStep, recoverAttrs{
		N: r.stats.Steps, Action: "recover", Strategy: string(s),
		TookMS: e.now().Sub(start).Milliseconds(),
	})
}

// gaveUpNotes is the honest failure of a run that had nowhere left to go: the
// reason in fixed words, and where it ended, the same shape as a run that
// spent its budget.
func gaveUpNotes(r *run, why stuckReason) string {
	return fmt.Sprintf("gave up: %s — ended at %q — %s", stuckWords[why], r.title, r.url)
}

// strategy is the one decision round trip of the branch: which of the things
// this package can do is worth doing on this page. The criteria are fixed
// words; the state is the run's own history, the reason it is stuck, and the
// page as a decision is ever shown it — refs, dialogs and what it is saying.
func (e *Executor) strategy(ctx context.Context, r *run, why stuckReason, obs proto.Observation) (strategy, error) {
	crit := map[string]string{
		string(stratRetryOther): strategyRetryOther,
		string(stratDeeplink):   strategyDeeplink,
		string(stratGiveUp):     strategyGiveUp,
	}
	if r.firstURL != "" {
		// A run that has not been on a web page yet has no page to go back
		// to, and offering one would be offering a choice that does nothing.
		crit[string(stratRestart)] = strategyRestart
	}
	if e.o.Vision != nil && !r.vision {
		crit[string(stratLook)] = strategyLook
	}

	var page strings.Builder
	if h := renderHistory(r.history); h != "" {
		page.WriteString(h + "\n")
	}
	page.WriteString("why this run is stuck: " + stuckWords[why] + "\n")
	page.WriteString(renderRefs(obs))
	page.WriteString(renderDialogs(obs, ""))
	page.WriteString(renderAlerts(obs, ""))
	state := pageRule + "\n\nrequest: " + oneLine(r.task.Intent, 4000) + "\n\n" + fencePage(page.String())

	ans, err := e.ask(ctx, r, state, map[string]provider.Question{QuestionStrategy: {
		Type:         provider.QuestionChoice,
		Instructions: strategyInstructions,
		Criteria:     crit,
	}})
	if err != nil {
		return "", err
	}
	c := strings.TrimSpace(ans[QuestionStrategy].Choice)
	if _, offered := crit[c]; !offered {
		// Not one of the words we offered — including an answer that was not
		// readable at all. The safe reading is the strategy that changes
		// nothing about where the run is.
		return stratRetryOther, nil
	}
	return strategy(c), nil
}

// restart takes the run back to the page it started on and forgets the route
// it took from there: the history collapses to one line, the tracker to the
// addresses it has already been at, and the note the move call left itself
// goes — it was about a page that is two navigations ago.
func (e *Executor) restart(ctx context.Context, r *run, why stuckReason) (Result, bool, error) {
	res, cont, err := e.goTo(ctx, r, r.firstURL)
	if err != nil || !cont {
		return res, cont, err
	}
	r.history = []tried{{what: "a first attempt failed: " + stuckWords[why]}}
	r.lastNote, r.avoid = "", nil
	r.track.reset(true)
	return Result{}, true, nil
}

// deeplink is the one generative call of the branch: the address on this site
// that shows what the request asks for. What comes back is validated like any
// navigate move — http(s) here, the allow-list in the sidecar — and an answer
// that is not an address is counted as a dropped move and nothing else.
// ok is false when there was no address to go to.
func (e *Executor) deeplink(ctx context.Context, r *run, obs proto.Observation) (Result, bool, bool, error) {
	start := e.now()
	var page strings.Builder
	fmt.Fprintf(&page, "url: %s\n", oneLine(obs.URL, 300))
	fmt.Fprintf(&page, "title: %s\n", oneLine(obs.Title, proto.MaxNameBytes))
	msg := "request: " + oneLine(r.task.Intent, 4000) + "\n\n" + fencePage(page.String())

	model, prov := e.movePair()
	resp, err := e.chat(ctx, model, prov, deeplinkSystem, moveReserve, provider.ReasoningNone,
		provider.TextMessage(provider.RoleUser, msg))
	if err != nil {
		if !timedOut(ctx, err) {
			return Result{}, false, false, err
		}
		e.log(r).Warn("browser: the deep-link call timed out", "timeout", e.o.PlanTimeout)
		return Result{}, true, false, nil
	}
	r.stats.GenerativeCall++
	r.count(resp.Usage)

	u := deeplinkURL(resp.Text)
	if u == "" {
		// The model wrote something that is not an address on this site. It is
		// a move that was thrown away, and the trace says so in the same shape
		// as every other one.
		e.log(r).Info("browser: the deep-link answer was not an http address")
		e.emit(ctx, r, event.BrowserMove, moveAttrs{
			N: r.moveCalls, Dropped: 1, TookMS: e.now().Sub(start).Milliseconds(),
			UsageAttrs: event.Call(resp.Usage).Attrs(),
		})
		return Result{}, true, false, nil
	}
	res, cont, err := e.goTo(ctx, r, u)
	if err != nil || !cont {
		return res, cont, false, err
	}
	return Result{}, true, true, nil
}

// rawDeeplink is the wire shape of the deep-link answer.
type rawDeeplink struct {
	URL string `json:"url"`
}

// deeplinkURL reads the address out of the answer, or "" when there is none
// this package would navigate to.
func deeplinkURL(text string) string {
	obj := jsonObject(text)
	if obj == "" {
		return ""
	}
	var raw rawDeeplink
	if err := json.Unmarshal([]byte(obj), &raw); err != nil {
		return ""
	}
	u := strings.TrimSpace(raw.URL)
	if !isHTTPURL(u) {
		return ""
	}
	return u
}

// goTo navigates, with the two answers a navigation can end a run with: the
// deadline passing while a person held the browser, and the allow-list.
func (e *Executor) goTo(ctx context.Context, r *run, url string) (Result, bool, error) {
	res, err := e.act(ctx, r, proto.Action{Kind: proto.ActNavigate, URL: url})
	if ended(err) {
		return r.result(OutcomeFailed, notesTime), false, nil
	}
	if err != nil {
		return Result{}, false, fmt.Errorf("browser: act: %w", err)
	}
	if !res.OK && res.Code == proto.ErrNotAllowed {
		e.emitBlocked(ctx, r, whyNotAllowed)
		return r.result(OutcomeBlocked, notesNotAllowed), false, nil
	}
	return Result{}, true, nil
}
