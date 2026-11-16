package browser

import (
	"context"
	"encoding/json"
	"strings"

	"holaclaw/internal/browser/proto"
	"holaclaw/internal/provider"
)

// The decision model's two jobs in M11 (docs/browser.md): before a move, what
// it would commit — and which element it means, when the move named one in
// words rather than by ref; after it, whether the page changed the way the
// move said it would and whether the request is satisfied now. Both are
// answered against closed sets, with the guard's discipline
// (internal/guard/decide.go): an answer that is not a member of the set the
// executor sent is not an answer, and nothing is acted on it.

// Question names. Each call asks only the ones it has the material for.
const (
	QuestionElement = "element"
	QuestionRisk    = "risk"
	QuestionEffect  = "effect"
	QuestionGoal    = "goal"
	QuestionLoad    = "loading"
	QuestionPick    = "pick"
	// QuestionStrategy is the recovery branch's own question: the run is in a
	// dead end and what to do about it is a choice among the few things this
	// package can do, not a fact about the page (recover.go).
	QuestionStrategy = "strategy"
)

// RefNone is the element answer that means "none of the candidates fits". It
// is offered as a choice so a model that sees nothing usable can say so
// instead of picking the nearest thing.
const RefNone = "none"

// bound is what the pre-move call answers: the element the move acts on, and
// what carrying it out would commit.
type bound struct {
	Ref     string  // candidate ref, "" when none was chosen
	Role    string  // role of the chosen candidate, for the trace
	Element float64 // confidence in Ref
	Risk    RiskKind
	// Invalid marks a risk answer the executor could not read against its own
	// closed set. It is not a weak answer but no answer at all: nothing is
	// carried out on it, because an unreadable reply must never be the thing
	// that decides a purchase is not one.
	Invalid bool
}

// verdict is what the post-move call answers: the scores that steer the loop.
// Load is read before the other two — a page caught mid-render has not failed
// to change, it has not finished changing.
type verdict struct {
	Effect float64
	Goal   float64
	Load   float64
}

// riskQuestion is asked before every move, on the move's own words and the
// page it is about to act on.
func riskQuestion() provider.Question {
	return provider.Question{
		Type:         provider.QuestionChoice,
		Instructions: riskInstructions,
		Criteria: map[string]string{
			string(RiskNone):     riskNoneDesc,
			string(RiskPurchase): riskPurchaseDesc,
			string(RiskSend):     riskSendDesc,
			string(RiskDelete):   riskDeleteDesc,
			string(RiskAccount):  riskAccountDesc,
		},
	}
}

// elementQuestion offers the shortlist, in shortlist order — the refs are
// assigned in it (r1…rN), which is the only way the ranking survives a map of
// criteria.
func elementQuestion(cands []proto.Candidate) provider.Question {
	crit := map[string]string{RefNone: elementNone}
	for _, c := range cands {
		crit[c.Ref] = candidateCriteria(c, cands)
	}
	return provider.Question{
		Type:         provider.QuestionChoice,
		Instructions: elementInstructions,
		Criteria:     crit,
	}
}

// bind is the pre-move call: the risk of this move, and the element it means
// when it gave words instead of a ref. cands is nil when the move already
// carries a ref of this observation, and then no element question is asked;
// the risk question is asked only of a move that commits something, because a
// move that scrolls, reads or types into a box commits nothing by definition
// and asking costs a round trip that would otherwise not happen at all.
//
// A move that needs neither — a type with a ref and no submit is the common
// one — is bound without any call: the answer was already known.
func (e *Executor) bind(ctx context.Context, r *run, m Move, obs proto.Observation, cands []proto.Candidate) (bound, error) {
	qs := map[string]provider.Question{}
	risky := commits(m)
	if risky {
		qs[QuestionRisk] = riskQuestion()
	}
	if len(cands) > 0 {
		qs[QuestionElement] = elementQuestion(cands)
	}
	if len(qs) == 0 {
		return bound{Risk: RiskNone}, nil
	}
	var page strings.Builder
	page.WriteString(renderRefs(obs))
	page.WriteString("the move: " + describeMove(m) + "\n")
	if m.Expect != "" {
		page.WriteString("what it should produce: " + oneLine(m.Expect, maxExpectBytes) + "\n")
	}
	state := pageRule + "\n\nrequest: " + oneLine(r.task.Intent, 4000) + "\n\n" + fencePage(page.String())

	ans, err := e.ask(ctx, r, state, qs)
	if err != nil {
		return bound{}, err
	}
	b := bound{Risk: RiskNone}
	if risky {
		// An unreadable answer to a question that was asked is no answer at
		// all: nothing is carried out on it.
		if a, ok := ans[QuestionRisk]; ok && a.Choice != "" {
			k, valid := riskOf(a.Choice)
			b.Risk, b.Invalid = k, !valid
		} else {
			b.Invalid = true
		}
	}
	if len(cands) == 0 {
		return b, nil
	}
	a := ans[QuestionElement]
	if a.Choice == RefNone {
		return b, nil
	}
	c, ok := candidateByRef(cands, a.Choice)
	if !ok {
		// Not one of the refs we offered: no element and no confidence, so the
		// move is dropped. The risk answer beside it still stands.
		return b, nil
	}
	b.Ref, b.Role = c.Ref, c.Role
	b.Element = clamp01(a.Confidence)
	if p, has := a.Probabilities[a.Choice]; has {
		b.Element = clamp01(p)
	}
	if b.Invalid {
		b.Element = 0
	}
	return b, nil
}

// verify is the post-move call: one round trip, two score questions. The effect
// question is answered on the diff — what changed is the whole of what an
// expectation is judged against, and it is a fraction of the state — and the
// goal question on a trimmed view of the page as it stands now, because
// "is the request satisfied" is about the page and not about the change.
func (e *Executor) verify(ctx context.Context, r *run, m Move, d diff, after proto.Observation) (verdict, error) {
	var page strings.Builder
	page.WriteString("the move just made: " + describeMove(m) + "\n")
	if m.Expect != "" {
		page.WriteString("expected: " + oneLine(m.Expect, maxExpectBytes) + "\n")
	}
	page.WriteString("\nwhat changed:\n" + d.render())
	page.WriteString("\nthe page now:\n" + verifyPage(after))
	state := pageRule + "\n\nrequest: " + oneLine(r.task.Intent, 4000) + "\n\n" + fencePage(page.String())

	qs := map[string]provider.Question{
		QuestionEffect: {
			Type:         provider.QuestionNoul,
			Instructions: effectInstructions,
			Criteria:     map[string]string{"true": effectTrue, "false": effectFalse},
		},
		QuestionGoal: {
			Type:         provider.QuestionNoul,
			Instructions: goalInstructions,
			Criteria:     map[string]string{"true": goalTrue, "false": goalFalse},
		},
	}
	if d.Nothing {
		// The load question is only worth its tokens where it can change the
		// answer: a page that moved was not caught mid-render, whatever else is
		// wrong with what it did. A page where nothing changed at all is the one
		// case the browser's own wait cannot tell from a site that is slower
		// than the loop.
		qs[QuestionLoad] = provider.Question{
			Type:         provider.QuestionNoul,
			Instructions: loadInstructions,
			Criteria:     map[string]string{"true": loadTrue, "false": loadFalse},
		}
	}
	ans, err := e.ask(ctx, r, state, qs)
	if err != nil {
		return verdict{}, err
	}
	v := verdict{
		Effect: clamp01(ans[QuestionEffect].Noul),
		Goal:   clamp01(ans[QuestionGoal].Noul),
	}
	if d.Nothing {
		v.Load = clamp01(ans[QuestionLoad].Noul)
	}
	return v, nil
}

// pickQuestion offers the suggestions a box opened under itself, in the order
// the page lists them, with RefNone for a list that holds nothing the request
// means. all is the whole shortlist, which each option's own "in" line is
// resolved against: the box an option belongs to is not itself an option.
func pickQuestion(opts, all []proto.Candidate) provider.Question {
	crit := map[string]string{RefNone: pickNone}
	for _, c := range opts {
		crit[c.Ref] = candidateCriteria(c, all)
	}
	return provider.Question{
		Type:         provider.QuestionChoice,
		Instructions: pickInstructions,
		Criteria:     crit,
	}
}

// pick is the one round trip of the type-and-pick step: the page answered what
// was typed with its own suggestions, and which of them the request means is a
// choice among things offered rather than a fact about the page. It returns
// RefNone when nothing was chosen or the answer was not one of the refs
// offered, which the caller reads as "the list held nothing we asked for".
func (e *Executor) pick(ctx context.Context, r *run, obs proto.Observation, box proto.Candidate, opts []proto.Candidate) (string, error) {
	var page strings.Builder
	page.WriteString(renderRefs(obs))
	page.WriteString("the field: " + oneLine(box.Role, 40) + " " + quote(box.Name) + "\n")
	page.WriteString(renderAlerts(obs, ""))
	state := pageRule + "\n\nrequest: " + oneLine(r.task.Intent, 4000) + "\n\n" + fencePage(page.String())

	ans, err := e.ask(ctx, r, state, map[string]provider.Question{QuestionPick: pickQuestion(opts, obs.Candidates)})
	if err != nil {
		return "", err
	}
	choice := ans[QuestionPick].Choice
	if _, ok := candidateByRef(opts, choice); !ok {
		return RefNone, nil
	}
	return choice, nil
}

// ask is one decision round trip: the decision model, or — with no `decide`
// role — the chat model through a strict JSON prompt. Either way the answers
// go through the same validation, because the closed sets are what makes a
// model's text safe to act on and the backend does not change that.
func (e *Executor) ask(ctx context.Context, r *run, state string, qs map[string]provider.Question) (map[string]provider.Answer, error) {
	if e.o.Decider == nil {
		return e.askByChat(ctx, r, state)
	}
	resp, err := e.o.Decider.Decide(ctx, provider.DecisionRequest{
		Model: e.o.DecideModel, State: state, Questions: qs,
	})
	r.stats.DecisionCalls++
	if resp != nil {
		r.count(resp.Usage) // an invalid reply comes with its usage too
	}
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return map[string]provider.Answer{}, nil
	}
	return resp.Answers, nil
}

// chatAnswer is the JSON the fallback rung asks for: every question this
// package has, answered in one object. What the state gave no material for
// comes back at zero and is read as no answer.
type chatAnswer struct {
	Element    string  `json:"element"`
	Confidence float64 `json:"element_confidence"`
	Risk       string  `json:"risk"`
	Effect     float64 `json:"effect"`
	Goal       float64 `json:"goal"`
	Load       float64 `json:"loading"`
	// Pick is the suggestion the request means, for the one call that offers a
	// box's own list. "" is no answer, which the caller reads as none of them.
	Pick string `json:"pick"`
	// Strategy is what a stuck run should do next, for the one call that asks.
	Strategy string `json:"strategy"`
}

// askByChat is the no-decide-role fallback.
func (e *Executor) askByChat(ctx context.Context, r *run, state string) (map[string]provider.Answer, error) {
	resp, err := e.chat(ctx, e.o.ChatModel, e.o.Chat, decideFallbackSystem, decideReserve, provider.ReasoningNone,
		provider.TextMessage(provider.RoleUser, state))
	r.stats.DecisionCalls++
	if err != nil {
		return nil, err
	}
	r.count(resp.Usage)
	var a chatAnswer
	if err := json.Unmarshal([]byte(jsonObject(resp.Text)), &a); err != nil {
		return map[string]provider.Answer{}, nil
	}
	out := map[string]provider.Answer{
		QuestionElement:  {Type: provider.QuestionChoice, Choice: a.Element, Confidence: a.Confidence},
		QuestionRisk:     {Type: provider.QuestionChoice, Choice: a.Risk},
		QuestionEffect:   {Type: provider.QuestionNoul, Noul: a.Effect},
		QuestionGoal:     {Type: provider.QuestionNoul, Noul: a.Goal},
		QuestionLoad:     {Type: provider.QuestionNoul, Noul: a.Load},
		QuestionPick:     {Type: provider.QuestionChoice, Choice: a.Pick},
		QuestionStrategy: {Type: provider.QuestionChoice, Choice: a.Strategy},
	}
	return out, nil
}

func candidateByRef(cands []proto.Candidate, ref string) (proto.Candidate, bool) {
	for _, c := range cands {
		if c.Ref == ref {
			return c, true
		}
	}
	return proto.Candidate{}, false
}

// riskOf reports whether choice is one of this package's risk kinds.
func riskOf(choice string) (RiskKind, bool) {
	switch RiskKind(choice) {
	case RiskNone, RiskPurchase, RiskSend, RiskDelete, RiskAccount:
		return RiskKind(choice), true
	}
	return RiskNone, false
}

func clamp01(f float64) float64 {
	switch {
	case !(f > 0): // also catches NaN
		return 0
	case f > 1:
		return 1
	}
	return f
}
