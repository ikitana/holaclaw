package browser

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"holaclaw/internal/browser/proto"
	"holaclaw/internal/provider"
)

// The last rung, outside the ladder: the page is read once more and turned
// into the answer the agent asked for — what the request asked for, in the
// user's own words, since M11 has no plan to say it otherwise. The rung is in
// two halves. A value that is *on* the page — a price, a name, a time, a code, a link — is picked out of
// the page's own short spans by the decision model, which costs 0.3–0.7 s and
// nothing; only prose, arithmetic or a multi-field answer reaches the 5–15 s
// chat call behind it.
//
// Either way what comes back is page-derived, which is why the tool marks the
// turn tainted (docs/security.md). A selected span is the page's own bytes,
// so it may well carry an injected sentence: it is an answer shown to a
// person, never an instruction — exactly like the chat extraction's.

// The selection call's questions. They are answered against one state in one
// round trip, like the step call's.
const (
	QuestionSpan  = "span"
	QuestionCover = "covered"
	QuestionMany  = "many"
)

// What the selection rung is bounded by.
const (
	// maxSpanBytes and maxSpans bound what the choice is offered: short
	// spans, and no more than the endpoint's 255 options minus the "none".
	// maxPageSpans is what the page is cut into before that shortlist is
	// chosen out of it — high enough that a whole results page is considered,
	// bounded so a pathological page cannot be unbounded work.
	maxSpanBytes = 120
	maxSpans     = 200
	maxPageSpans = 5000
	// selectionFloor is how sure the model must be that the span it chose
	// holds the whole answer. Below it the chat call reads the page instead.
	selectionFloor = 0.7
	// manyFloor is when a request is read as asking for more than one item.
	manyFloor = 0.5

	// What the finished event reports as extract_by.
	extractBySelection = "selection"
	extractByChat      = "chat"
	extractByScreen    = "screen"

	// thinTextBytes is how little text a page has to render before the answer
	// is looked for on the screen rather than in the text. A page whose
	// content is a picture — an image URL, a chart, a scanned menu — has a
	// URL, a title and almost nothing else, and the two text rungs can only
	// answer "not found" on it: the selection rung has no spans to offer and
	// the chat rung is shown an empty page.
	thinTextBytes = 80

	// What the finished event reports as screen_why: which of the three
	// triggers asked for the screenshot. Fixed words, like every other reason
	// on the trace.
	screenWhyThin     = "thin"
	screenWhyLook     = "look"
	screenWhyNotFound = "not_found"
)

// extract reads the answer off the final page: by selection when the page
// says it in one span (or a few), by one chat call otherwise.
func (e *Executor) extract(ctx context.Context, r *run) (string, error) {
	start := e.now()
	defer func() { r.stats.ExtractTook += e.now().Sub(start) }()

	obs, err := e.observe(ctx, r, Move{}, extractTextBytes, "", 0)
	if err != nil {
		return "", err
	}
	if obs.Truncated {
		// Worth a line of its own: an answer read off a page that was cut
		// short is the one case where the rung can be working perfectly and
		// still be short of what was asked for.
		e.log(r).Info("browser: the page was longer than could be read; the answer comes from its beginning only")
	}
	// What is to be read off the page is the request itself: the user's own
	// words, which is the one thing in a run no page has touched.
	request := r.task.Intent

	answer, many, ok := e.selectAnswer(ctx, r, request, obs)
	if ok {
		r.extractBy = extractBySelection
		return capAnswer(answer, e.o.MaxAnswerBytes), nil
	}

	// A page with nothing to read is read off the screen instead, when there
	// is a model that can look at one. Without such a model nothing below
	// changes and the chat rung reads the page as it always did.
	screened := false
	if thinPage(obs) {
		sr := e.readScreen(ctx, r, obs, many, screenWhyThin)
		screened = sr.ran
		if sr.ran {
			// The page has no text: whatever the screen said, including the
			// sentinel, is the whole of what could be read off it.
			return sr.answer, sr.err
		}
	}
	r.extractBy = extractByChat

	msg := "request: " + oneLine(r.task.Intent, 4000) +
		"\nread off the page: " + oneLine(request, 500) + "\n"
	if obs.Truncated {
		// The page was longer than the budget, so what follows is its
		// beginning and not the whole of it. Saying so is the difference
		// between an answer that is short and an answer that is wrong: a
		// partial list presented as complete reads as the complete one.
		msg += truncatedNote + "\n"
	}
	// The whole page, as the sidecar capped it: at most proto.MaxTextBytes,
	// which every chat model here reads in one call. It was once cut down to
	// the lines the request matched, and that cut the rows of a ranking out
	// from between the heading and the footer — a list is exactly the shape
	// whose items do not contain the request's words.
	msg += "\n" + fencePage(renderObservation(obs))

	system := extractSystem
	if many {
		system = extractListSystem
	}
	resp, err := e.chat(ctx, e.o.ChatModel, e.o.Chat, system, extractReserve, provider.ReasoningNone,
		provider.TextMessage(provider.RoleUser, msg))
	if err != nil {
		return "", fmt.Errorf("browser: extraction: %w", err)
	}
	r.stats.GenerativeCall++
	r.count(resp.Usage)
	r.extractPrompt += resp.Usage.PromptTokens
	r.extractOutput += resp.Usage.CompletionTokens
	text := strings.TrimSpace(resp.Text)
	if wantsLook(text) {
		// The model that read the text says what was asked for is in a picture
		// on the page — a chart, a photo, a scanned table — which is the one
		// thing the text it was given does not contain. It is the reading
		// model's call, not a word list's: it can say so for a request that
		// never mentions a picture.
		if !screened {
			sr := e.readScreen(ctx, r, obs, many, screenWhyLook)
			if sr.found {
				return sr.answer, sr.err
			}
			if !sr.ran {
				e.log(r).Info("browser: the page wanted a look and no model can look")
			}
		}
		// Either nothing could look, or what looked did not see it. Both come
		// to the sentinel's own answer: not there, and the run may try again.
		e.log(r).Info("browser: the page did not hold what was asked for")
		return "", nil
	}
	if notFound(text) {
		// The text did not hold it; what is on the screen still might — a
		// figure inside a chart, a dish on a scanned menu. The page is looked
		// at once, at most once per extraction, before the run is told it was
		// not there.
		if !screened {
			if sr := e.readScreen(ctx, r, obs, many, screenWhyNotFound); sr.found {
				return sr.answer, sr.err
			}
		}
		// The rung was asked to say this when the page does not hold what was
		// asked for, so it is the one answer that is not one. Returning it as
		// an answer is how a run ends "done" with "not found" in its result,
		// and how it skips the retry it is owed: an empty answer puts the
		// failure in the history and goes round again while there is budget.
		e.log(r).Info("browser: the page did not hold what was asked for")
		return "", nil
	}
	if rest, ok := partial(text); ok {
		// The page holds part of it. Once per page the run is sent back to
		// the move call with that in its history — the rest is often a
		// scroll or a "show more" away. A page that says "part" twice is a
		// page whose part is the answer: what was read is returned, with the
		// caveat carried in the notes, rather than the run circling on it.
		fp := fingerprintOf(obs)
		if r.partialAt == fp {
			e.log(r).Info("browser: the page holds only part of what was asked for, twice; answering with what it has")
			r.partialNotes = notesPartial
			return capAnswer(rest, e.o.MaxAnswerBytes), nil
		}
		r.partialAt = fp
		e.log(r).Info("browser: the page holds only part of what was asked for")
		return "", errPartial
	}
	return capAnswer(text, e.o.MaxAnswerBytes), nil
}

// thinPage reports whether the page rendered too little text for the text
// rungs to work on: what is left of it once the whitespace is discounted is
// shorter than thinTextBytes. An image, a chart or a canvas is this shape.
func thinPage(obs proto.Observation) bool {
	n := 0
	for _, f := range strings.Fields(obs.Text) {
		if n += len(f); n >= thinTextBytes {
			return false
		}
	}
	return true
}

// reader is the model that looks at the screenshot for the extraction: the
// `vision` role when there is one, else the `chat` model when it takes image
// parts. The `look` rung keeps to the `vision` role alone — it is a recovery
// the loop can pay for per step, this is one call at the end of a run.
func (e *Executor) reader() (provider.Model, provider.Provider, bool) {
	switch {
	case e.o.Vision != nil:
		return e.o.VisionModel, e.o.Vision, true
	case e.o.ChatModel.Vision:
		return e.o.ChatModel, e.o.Chat, true
	}
	return provider.Model{}, nil, false
}

// screenRead is what one screen read came to. ran says a model looked at the
// page at all — false for no reader, no screenshot or a call that failed, and
// then the caller carries on exactly as it would have without this rung.
// found says the look produced something: an answer, or the partial signal.
// A screen that answered the sentinel is ran without found.
type screenRead struct {
	answer string
	err    error
	ran    bool
	found  bool
}

// readScreen is the extraction's third way of reading the answer: one
// screenshot and one call to a model that can look at it. why is the fixed
// word the trace carries for what asked for the look.
//
// What comes back is page-derived exactly as the chat rung's answer is: the
// screenshot is the page's own pixels, so the tool marks the turn tainted and
// the answer is something shown to a person, never an instruction.
func (e *Executor) readScreen(ctx context.Context, r *run, obs proto.Observation, many bool, why string) screenRead {
	model, prov, ok := e.reader()
	if !ok {
		return screenRead{}
	}
	shot, err := e.o.Sidecar.Screenshot(ctx, r.task.Agent)
	if err != nil || len(shot) == 0 {
		e.log(r).Info("browser: the page could not be read off the screen")
		return screenRead{}
	}
	system := screenExtractSystem
	if many {
		system = screenExtractListSystem
	}
	// Only the request and the two page-derived values the agent is told
	// about anyway reach the text part: what is to be read is in the image.
	msg := "request: " + oneLine(r.task.Intent, 4000) + "\n\n" +
		fencePage("url: "+oneLine(obs.URL, 300)+"\ntitle: "+oneLine(obs.Title, proto.MaxNameBytes)+"\n")
	resp, err := e.chat(ctx, model, prov, system, extractReserve, provider.ReasoningNone,
		provider.Message{Role: provider.RoleUser, Parts: []provider.Part{
			{Image: &provider.Image{MIME: "image/jpeg", Data: shot}},
			{Text: msg},
		}})
	if err != nil {
		e.log(r).Info("browser: the page could not be read off the screen")
		return screenRead{}
	}
	r.stats.GenerativeCall++
	r.stats.ScreenReads++
	r.count(resp.Usage)
	r.extractPrompt += resp.Usage.PromptTokens
	r.extractOutput += resp.Usage.CompletionTokens
	r.extractBy = extractByScreen
	r.screenWhy = why

	text := strings.TrimSpace(resp.Text)
	if notFound(text) {
		e.log(r).Info("browser: the screen did not show what was asked for")
		return screenRead{ran: true}
	}
	if rest, ok := partial(text); ok {
		// The same two-strikes discipline as the chat rung's: once per page
		// the run goes back for the rest, and a page that says "part" twice is
		// answered with the part it has.
		fp := fingerprintOf(obs)
		if r.partialAt == fp {
			e.log(r).Info("browser: the page holds only part of what was asked for, twice; answering with what it has")
			r.partialNotes = notesPartial
			return screenRead{answer: capAnswer(rest, e.o.MaxAnswerBytes), ran: true, found: true}
		}
		r.partialAt = fp
		e.log(r).Info("browser: the page holds only part of what was asked for")
		return screenRead{err: errPartial, ran: true, found: true}
	}
	return screenRead{answer: capAnswer(text, e.o.MaxAnswerBytes), ran: true, found: true}
}

// errPartial is extract's way of saying the page had part of the answer and
// the run should try for the rest. It is a signal, not a failure: reach turns
// it into a history line and carries on.
var errPartial = errors.New("browser: the page holds only part of what was asked for")

// notesPartial is what a result carries when the run settled for the part a
// page had, in fixed words.
const notesPartial = "the page showed only part of what was asked for"

// partial reports whether an extraction began with the partial sentinel, and
// returns what followed it: what the page did show.
func partial(answer string) (string, bool) {
	s := strings.TrimSpace(answer)
	low := strings.ToLower(s)
	const word = "partial"
	if !strings.HasPrefix(low, word) {
		return "", false
	}
	// The word, not a word that begins with it: "Partially refundable" is an
	// answer about a fare.
	if len(low) > len(word) {
		if c := low[len(word)]; c >= 'a' && c <= 'z' {
			return "", false
		}
	}
	rest := strings.TrimLeft(s[len(word):], ` .:;,-—–"'“”‘’`+"\n\t")
	if rest == "" {
		return "", true
	}
	return rest, true
}

// notFoundAnswers are the ways the extraction says "the page does not hold
// it". The prompts ask for the first one exactly; the second is what a model
// reading a Spanish page answers often enough to be worth knowing, and a
// sentinel nobody recognises is worse than a list that is slightly long.
var notFoundAnswers = []string{"not found", "no encontrado"}

// notFound reports whether an extraction answered with the sentinel rather
// than with something off the page.
func notFound(answer string) bool { return sentinel(answer, notFoundAnswers) }

// lookAnswers are the ways the text rung says "what was asked for is in a
// picture on this page, not in the text you gave me". The second is what a
// model reading a Spanish page answers, the same allowance the not-found
// sentinel makes.
var lookAnswers = []string{"look", "mirar"}

// wantsLook reports whether the text rung asked for the page to be looked at
// rather than read.
func wantsLook(answer string) bool { return sentinel(answer, lookAnswers) }

// sentinel reports whether an answer is one of the fixed words the extraction
// prompts ask for, rather than something off the page. The comparison is made
// on the answer stripped of the punctuation and quotes a model wraps a short
// reply in.
//
// A model asked for one word often gives the word and explains itself as
// well, before it or after it: "not found. The page has no image URL…", "…the
// page gives no such URL. Not found." Both are the sentinel, and taking
// either for an answer is how a run ends "done" with the model's apology as
// its result — measured, on a page whose answer was in a picture. The
// explanation has to be joined to the sentinel by punctuation, which is what
// separates it from "not found in stock, but 12 EUR online": an answer that
// happens to begin with the words. Words in the middle of a sentence are
// never the sentinel — a real answer may well contain them.
func sentinel(answer string, words []string) bool {
	s := strings.ToLower(strings.TrimSpace(answer))
	s = strings.Trim(s, ` .!,;:"'“”‘’`)
	s = strings.Join(strings.Fields(s), " ")
	for _, w := range words {
		switch {
		case s == w:
			return true
		case strings.HasPrefix(s, w):
			rest := strings.TrimLeft(s[len(w):], " ")
			if r, _ := utf8.DecodeRuneInString(rest); strings.ContainsRune(sentinelJoins, r) {
				return true
			}
		}
		if strings.HasSuffix(s, w) {
			before := strings.TrimRight(s[:len(s)-len(w)], " ")
			if r, _ := utf8.DecodeLastRuneInString(before); strings.ContainsRune(sentinelJoins, r) {
				return true
			}
		}
	}
	return false
}

// sentinelJoins are the marks an explanation is hung off a sentinel with.
const sentinelJoins = `.,:;!—–-…()`

// selectAnswer is the selection rung: one call, three questions — which span
// answers the request, is the answer wholly inside what was chosen, and does
// the request ask for more than one thing. ok is false whenever the chat call
// should read the page instead: no decision model, no spans, no span chosen,
// a cover answer under the floor, or a call that failed. A failure here is
// never the task's failure — there is a rung behind it.
//
// many is the answer to the third question, and it is returned even when ok
// is false: the chat rung behind this one is then told to list rather than to
// answer in one line, which is the difference between every capacity and the
// first of them. It is false whenever the question was not reached.
func (e *Executor) selectAnswer(ctx context.Context, r *run, request string, obs proto.Observation) (answer string, many, ok bool) {
	if e.o.Decider == nil {
		return "", false, false
	}
	// The spans the request's own words appear in, not the first maxSpans of
	// the page: on a results page those are the cookie banner and the nav.
	spans := rankSpans(spansOf(obs.Text), requestTerms(request+" "+r.task.Intent), maxSpans)
	if len(spans) == 0 {
		return "", false, false
	}
	keys := spanKeys(len(spans))
	crit := map[string]string{RefNone: spanNone}
	for i, key := range keys {
		crit[key] = quote(neutralise(spans[i]))
	}
	resp, err := e.o.Decider.Decide(ctx, provider.DecisionRequest{
		Model: e.o.DecideModel,
		State: selectionState(r.task, request, obs, spans, keys),
		Questions: map[string]provider.Question{
			QuestionSpan: {
				Type:         provider.QuestionChoice,
				Instructions: spanInstructions,
				Criteria:     crit,
			},
			QuestionCover: {
				Type:         provider.QuestionNoul,
				Instructions: coverInstructions,
				Criteria:     map[string]string{"true": coverTrue, "false": coverFalse},
			},
			QuestionMany: {
				Type:         provider.QuestionNoul,
				Instructions: manyInstructions,
				Criteria:     map[string]string{"true": manyTrue, "false": manyFalse},
			},
		},
	})
	r.stats.DecisionCalls++
	if resp != nil {
		r.count(resp.Usage) // an invalid reply comes with its usage too
	}
	if err != nil || resp == nil {
		e.log(r).Info("browser: the answer could not be selected off the page; reading it instead")
		return "", false, false
	}

	many = clamp01(resp.Answers[QuestionMany].Noul) >= manyFloor
	chosen := resp.Answers[QuestionSpan]
	idx, found := spanIndex(chosen.Choice, len(spans))
	if !found {
		return "", many, false
	}
	if clamp01(resp.Answers[QuestionCover].Noul) < selectionFloor {
		// The span was named but does not hold the whole answer: prose, or
		// something that has to be put together out of several places.
		return "", many, false
	}
	if many {
		// Several things were asked for. A span is one thing, and the
		// distribution over spans is not a list — a table row is often cut
		// into four spans, and the items of a ranking share their mass with
		// the page around them. The chat rung behind this one is told to
		// list, and reads the page itself.
		return "", true, false
	}
	return spans[idx], false, true
}

// spanKeys are the names the spans are offered under: s1…sN, in page order.
func spanKeys(n int) []string {
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, "s"+strconv.Itoa(i))
	}
	return out
}

// spanIndex reads a chosen key back as a position, and reports false for
// "none" and for anything we never offered — the same discipline as the
// element answer's.
func spanIndex(choice string, n int) (int, bool) {
	if !strings.HasPrefix(choice, "s") {
		return 0, false
	}
	i, err := strconv.Atoi(choice[1:])
	if err != nil || i < 1 || i > n {
		return 0, false
	}
	return i - 1, true
}

// selectionState is what the selection call judges: the request in the
// user's own words, what is to be read off the page, and the spans — which
// came off the page, so they go inside the fence like everything else it
// said. The criteria of the span question carry the same text, neutralised
// the same way.
func selectionState(t Task, request string, obs proto.Observation, spans, keys []string) string {
	var page strings.Builder
	fmt.Fprintf(&page, "url: %s\n", oneLine(obs.URL, 300))
	fmt.Fprintf(&page, "title: %s\n", oneLine(obs.Title, proto.MaxNameBytes))
	page.WriteString("spans:\n")
	for i, key := range keys {
		page.WriteString("  " + key + ": " + spans[i] + "\n")
	}
	return pageRule + "\n\nrequest: " + oneLine(t.Intent, 4000) +
		"\nread off the page: " + oneLine(request, 500) +
		"\n\n" + fencePage(page.String())
}

// spansOf cuts the page text into the short pieces the choice is offered:
// lines, then table cells, then sentences, each collapsed to one line and
// capped. Duplicates and one-character scraps are dropped, and the whole is
// bounded by maxSpans — the endpoint takes 255 options and a shortlist of
// spans is not made better by being longer.
func spansOf(text string) []string {
	out := make([]string, 0, 32)
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		for _, cell := range strings.FieldsFunc(line, isCellBreak) {
			for _, s := range sentences(cell) {
				s = capAnswer(strings.Join(strings.Fields(s), " "), maxSpanBytes)
				if utf8.RuneCountInString(s) < 2 || seen[s] {
					continue
				}
				seen[s] = true
				out = append(out, s)
				if len(out) == maxPageSpans {
					return out
				}
			}
		}
	}
	return out
}

func isCellBreak(r rune) bool { return r == '\t' || r == '|' }

// sentences splits one line where a sentence visibly ends. It is deliberately
// crude: a span that is still too long is capped, and a price cut in half by
// a full stop is still a span the model can decline to choose.
func sentences(s string) []string {
	var out []string
	start := 0
	for i := 0; i+1 < len(s); i++ {
		if strings.IndexByte(".!?;", s[i]) < 0 || s[i+1] != ' ' {
			continue
		}
		out = append(out, s[start:i+1])
		start = i + 2
	}
	return append(out, s[start:])
}

// neutralise defuses a fence marker the page wrote, for page text rendered
// somewhere a fence cannot go — the criteria of the span question.
func neutralise(s string) string {
	return pageFenceTag.ReplaceAllString(s, "[page]")
}

// capAnswer truncates on a rune boundary: the answer is page-derived and a
// page decides how long it is.
func capAnswer(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
