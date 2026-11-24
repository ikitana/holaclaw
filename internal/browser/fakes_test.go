package browser

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"

	"holaclaw/internal/browser/proto"
	"holaclaw/internal/provider"
)

// The fakes every test in this package is built from: a sidecar serving
// scripted pages, a decision model with scripted answers, a chat provider
// with scripted text, and a Humans that records what it was asked.

// page is one scripted page of the fake sidecar.
type page struct {
	title  string
	cands  []proto.Candidate
	text   string
	alerts []string // what the page is telling the user, as it loads
	// dialogs are the dialogs the page already has open when it is reached,
	// and modal the one of them that is modal.
	dialogs []string
	modal   string
}

// behaviour is what one action does: where it leads, what it fails with, what
// it downloads, and what it leaves the page saying and showing.
type behaviour struct {
	to        string // navigate here afterwards
	code      proto.ErrorCode
	once      bool // the failure happens only the first time
	downloads []proto.Download
	used      bool
	// says is what the page tells the user afterwards — the site's own
	// complaint about what was just submitted, which on a real page is often
	// the whole of what a submit produced.
	says []string
	// expands is the box whose own suggestion list this action opens: an
	// autocomplete answering what was typed into it.
	expands string
	// picks is the box a click on this option fills in, with value what it
	// leaves in it; the suggestions go away with the choice, the way a real
	// combobox closes on one.
	picks, value string
	// opens is the dialog this action lays over the page, by name: a calendar
	// a date box puts up, a panel a button opens.
	opens string
	// reveals is the controls this action puts on the screen without the page
	// calling them anything: the day cells of a calendar built out of divs.
	// They arrive marked new, the way the sidecar marks whatever was not on
	// the previous observation of the same tab.
	reveals []proto.Candidate
	// keeps is a box that takes none of what is typed into it — a readonly
	// field the accessibility tree still calls editable. The characters go
	// nowhere, the value stays what it was, and nothing is revealed: only a
	// click on it does anything at all.
	keeps bool
	// toggle is the other shortlist this action swaps the page to and back
	// again: a menu that opens and closes, a calendar that comes and goes. Two
	// clicks leave the page exactly as it was, which is what a run going in
	// circles is made of.
	toggle []proto.Candidate
}

type fakeSidecar struct {
	mu sync.Mutex

	pages map[string]page
	url   string
	// home is the page a fresh tab opens on: the start page of most tests,
	// about:blank for the ones about a run that starts on nothing.
	home string
	// on maps a ref (or an action kind for ref-less actions) to a behaviour.
	on map[string]*behaviour
	// typed is what a type action left in an element, by url and ref.
	typed map[string]string
	// shot is what a screenshot returns, shotErr what it fails with, and
	// shots how many were taken.
	shot    []byte
	shotErr error
	shots   int
	// loading is how many of the next observations report a page that is
	// still coming up.
	loading int
	// after maps a ref to candidates the page grows once it has been acted on:
	// the suggestion list an autocomplete opens under a box that was typed in.
	after map[string][]proto.Candidate
	// reshuffle maps a ref to the whole shortlist the page shows once it has
	// been acted on, refs and all — what the real sidecar does when an element
	// enters document order and every ref after it is minted afresh.
	reshuffle map[string][]proto.Candidate
	// shown is that replacement, once it has happened.
	shown []proto.Candidate
	// grown is what those additions have added to the current page so far.
	grown []proto.Candidate
	// expanded is the aria-expanded state an action has left a box in, by ref,
	// and says what the page is telling the user right now.
	expanded map[string]bool
	says     []string
	// opened is the dialogs actions have laid over the current page, on top of
	// the ones the page itself came with.
	opened []string
	// fresh is the refs the last action put on the screen: they come back marked
	// new on the next observation and ordinary on the one after, the way the
	// sidecar compares a tab with its own previous observation.
	fresh map[string]bool
	// mistyped is every ref a type action reached that the page says takes no
	// characters. A real one would swallow them and leave the field empty; the
	// executor is supposed to click instead, so the list stays empty.
	mistyped []string

	opens    int
	freshes  int
	observes int
	acts     []proto.Action
	hints    [][]string
	textAsk  []int
	maxCands []int
	observed []string
	// around is the ref each observation asked to be read around, in order.
	around []string
}

func newSidecar(start string) *fakeSidecar {
	return &fakeSidecar{
		pages: map[string]page{}, on: map[string]*behaviour{},
		after: map[string][]proto.Candidate{}, reshuffle: map[string][]proto.Candidate{},
		expanded: map[string]bool{}, url: start, home: start,
	}
}

func (f *fakeSidecar) Open(_ context.Context, _ string, req proto.OpenRequest) (proto.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	if req.Fresh {
		// A new tab: the page the last run was on, and everything it had
		// typed, opened or revealed there, is gone.
		f.freshes++
		f.url = f.home
		f.typed, f.opened, f.shown, f.grown, f.says, f.fresh = nil, nil, nil, nil, nil, nil
		f.expanded = map[string]bool{}
	}
	return proto.Session{Agent: "muse"}, nil
}

// openCounts is how often the run opened the browser, and how many of those
// asked for a fresh tab.
func (f *fakeSidecar) openCounts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens, f.freshes
}

// observedURLs is the page every observation was made on, in order.
func (f *fakeSidecar) observedURLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.observed...)
}

func (f *fakeSidecar) Observe(_ context.Context, _ string, req proto.ObserveRequest) (proto.Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observes++
	f.hints = append(f.hints, req.Hints)
	f.textAsk = append(f.textAsk, req.TextBytes)
	f.maxCands = append(f.maxCands, req.MaxCandidates)
	f.observed = append(f.observed, f.url)
	f.around = append(f.around, req.TextAround)
	p := f.pages[f.url]
	base := p.cands
	if f.shown != nil {
		base = f.shown
	}
	cands := append(append([]proto.Candidate(nil), base...), f.grown...)
	// The shortlist is capped where the caller asked, like the real one: how
	// many candidates a question is answered over is the caller's to choose.
	if req.MaxCandidates > 0 && len(cands) > req.MaxCandidates {
		cands = cands[:req.MaxCandidates]
	}
	for i := range cands {
		if v, ok := f.typed[f.url+"\x00"+cands[i].Ref]; ok {
			cands[i].Value = v
		}
		if open, ok := f.expanded[cands[i].Ref]; ok {
			cands[i].Expanded = &open
		}
		if f.fresh[cands[i].Ref] {
			cands[i].New = true
		}
	}
	// New is "not on the previous observation of this tab": once it has been
	// reported, the element is part of the page like any other.
	f.fresh = nil
	alerts := p.alerts
	if f.says != nil {
		alerts = f.says
	}
	o := proto.Observation{
		URL: f.url, Title: p.title, Candidates: cands, Total: len(p.cands), Alerts: alerts,
		Dialogs: append(append([]string(nil), p.dialogs...), f.opened...), Modal: p.modal,
	}
	if f.loading > 0 {
		// The next f.loading observations are of a page that has not finished
		// coming up, the way a real one reports itself mid-navigation.
		f.loading--
		o.Loading = true
	}
	if req.TextBytes > 0 {
		o.Text = p.text
	}
	return o, nil
}

func (f *fakeSidecar) Act(_ context.Context, _ string, a proto.Action) (proto.ActResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acts = append(f.acts, a)
	key := a.Ref
	if key == "" {
		key = string(a.Kind)
	}
	b := f.on[key]
	if a.Kind == proto.ActNavigate {
		if b == nil || b.code == "" {
			f.url = a.URL
			// A fresh load: whatever was typed into that page is gone, and so
			// is anything it had open over it.
			f.opened = nil
			for k := range f.typed {
				if strings.HasPrefix(k, f.url+"\x00") {
					delete(f.typed, k)
				}
			}
			return proto.ActResult{OK: true, URL: f.url, Navigated: true}, nil
		}
	}
	if b != nil && b.code != "" && (!b.once || !b.used) {
		b.used = true
		return proto.ActResult{OK: false, Code: b.code, URL: f.url}, nil
	}
	f.recordType(a)
	if b == nil {
		return proto.ActResult{OK: true, URL: f.url}, nil
	}
	if b.to != "" {
		f.url, f.opened = b.to, nil
	}
	return proto.ActResult{OK: true, URL: f.url, Downloads: b.downloads}, nil
}

// recordType remembers what was typed where. A real page shows it as the
// element's value, and the executor's post-condition is a hash of the
// shortlist: a fake that swallowed the text would report every type step as
// having had no effect at all.
func (f *fakeSidecar) recordType(a proto.Action) {
	if a.Kind == proto.ActType && a.Ref != "" {
		if c, ok := f.candidate(a.Ref); ok && !c.Editable {
			f.mistyped = append(f.mistyped, a.Ref)
		}
	}
	if b := f.on[a.Ref]; b != nil && b.keeps && a.Kind == proto.ActType {
		// A box that takes none of what is typed does nothing whatever with
		// it: no value, nothing opened, nothing revealed. Clicking it is
		// another matter.
		return
	}
	if b := f.on[a.Ref]; b != nil {
		if b.opens != "" {
			f.opened = append(f.opened, b.opens)
		}
		if b.says != nil {
			f.says = b.says
		}
		if b.expands != "" {
			f.expanded[b.expands] = true
		}
		if b.picks != "" {
			// One of the page's own suggestions was chosen: the box takes the
			// value, the list closes and the suggestions go away.
			f.expanded[b.picks] = false
			if f.typed == nil {
				f.typed = map[string]string{}
			}
			f.typed[f.url+"\x00"+b.picks] = b.value
			f.grown = nil
		}
		if len(b.reveals) > 0 {
			f.grown = append(f.grown, b.reveals...)
			if f.fresh == nil {
				f.fresh = map[string]bool{}
			}
			for _, c := range b.reveals {
				f.fresh[c.Ref] = true
			}
		}
	}
	if b := f.on[a.Ref]; b != nil && len(b.toggle) > 0 {
		// Open, then closed again: the shortlist swaps to the other one and
		// back with every action on this element.
		if f.shown == nil {
			f.shown = b.toggle
		} else {
			f.shown = nil
		}
	}
	if extra, ok := f.after[a.Ref]; ok && a.Ref != "" {
		f.grown = append(f.grown, extra...)
	}
	if next, ok := f.reshuffle[a.Ref]; ok && a.Ref != "" {
		f.shown = next
	}
	if a.Kind == proto.ActType && a.Ref != "" {
		if f.typed == nil {
			f.typed = map[string]string{}
		}
		f.typed[f.url+"\x00"+a.Ref] = a.Text
	}
}

// candidate is the element a ref names on the page as it stands: the page's
// own shortlist, whatever replaced it, and whatever has grown on it.
func (f *fakeSidecar) candidate(ref string) (proto.Candidate, bool) {
	base := f.pages[f.url].cands
	if f.shown != nil {
		base = f.shown
	}
	for _, c := range append(append([]proto.Candidate(nil), base...), f.grown...) {
		if c.Ref == ref {
			return c, true
		}
	}
	return proto.Candidate{}, false
}

// typedIntoUneditable is every ref a type action reached that takes no
// characters.
func (f *fakeSidecar) typedIntoUneditable() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.mistyped...)
}

func (f *fakeSidecar) Screenshot(context.Context, string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shots++
	if f.shotErr != nil {
		return nil, f.shotErr
	}
	return f.shot, nil
}

// screenshots is how many times the page was photographed.
func (f *fakeSidecar) screenshots() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shots
}

func (f *fakeSidecar) Close(context.Context, string) error { return nil }

// observeCount is the observation counter read under the lock, for tests that
// watch a run from another goroutine.
func (f *fakeSidecar) observeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.observes
}

// caps is the shortlist bound every observation asked for, in order.
func (f *fakeSidecar) caps() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.maxCands...)
}

func (f *fakeSidecar) actions() []proto.Action {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proto.Action(nil), f.acts...)
}

// observations is how many times the page has been read.
func (f *fakeSidecar) observations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.observes
}

// answers is one scripted decision. Zero values mean "the default": pick the
// first candidate with high confidence, nothing is risky, the move did what it
// said it would, and the goal is not reached yet.
type answers struct {
	element string
	conf    float64
	risk    string
	// effect and goal are the two scores of the post-move call. effect is 0.9
	// when left at zero — the ordinary case is a move that worked — and goal is
	// taken as given, so a test that says nothing gets a run that carries on.
	effect float64
	goal   float64
	// load is the post-move call's "was the page caught mid-render" answer,
	// asked only where nothing changed at all; pick is which of a box's own
	// suggestions the request meant, for the one call that offers them.
	load float64
	pick string
	// strategy is what a stuck run is told to do about it, for the one call
	// the recovery branch makes. Unscripted is no answer at all, which the
	// executor reads as the strategy that changes nothing.
	strategy string
	probs    map[string]float64
	cost     provider.NanoUSD
	usage    provider.Usage
	set      bool
	// The selection rung's three answers: which span holds what was asked for,
	// whether it holds all of it, whether more than one item was asked for, and
	// the distribution a list answer is taken from. An unscripted call chooses
	// no span, which is the chat extraction's cue.
	span      string
	cover     float64
	many      float64
	spanProbs map[string]float64
	// picture is the answer to "should the answer come with a picture of the
	// page", and noPicture a reply that does not carry that answer at all.
	picture   float64
	noPicture bool
}

type fakeDecider struct {
	mu      sync.Mutex
	script  []answers
	calls   int
	states  []string
	qs      []map[string]provider.Question
	nilResp bool
	err     error
	// errResp comes back with err: a reply that failed validation, carrying
	// only its usage.
	errResp *provider.DecisionResponse
}

func (f *fakeDecider) Decide(_ context.Context, req provider.DecisionRequest) (*provider.DecisionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.states = append(f.states, req.State)
	f.qs = append(f.qs, req.Questions)
	if f.err != nil {
		return f.errResp, f.err
	}
	if f.nilResp {
		return nil, nil
	}
	var a answers
	if len(f.script) > 0 {
		a, f.script = f.script[0], f.script[1:]
		a.set = true
	}
	u := a.usage
	u.Cost = priced(a.cost)
	return &provider.DecisionResponse{
		Answers: buildAnswers(a, req.Questions),
		Usage:   u,
	}, nil
}

// decided is the state every decision call was made on, in order.
func (f *fakeDecider) decided() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.states...)
}

// asked is the last question of that name any call made, and whether one was
// made at all: a run makes calls of three shapes — pre-move, post-move and span
// selection — and only some of them carry a given question.
func (f *fakeDecider) asked(name string) (provider.Question, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.qs) - 1; i >= 0; i-- {
		if q, ok := f.qs[i][name]; ok {
			return q, true
		}
	}
	return provider.Question{}, false
}

// timesAsked is how many decision calls carried a question of that name: what
// a test about which question is worth its tokens asserts on.
func (f *fakeDecider) timesAsked(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, qs := range f.qs {
		if _, ok := qs[name]; ok {
			n++
		}
	}
	return n
}

// callCount is how many decision round trips the run made.
func (f *fakeDecider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// buildAnswers turns a scripted answers into the map a Decider returns,
// filling everything left at zero with the defaults above. Only the questions
// the call actually asked are answered: a fake that answers more than it was
// asked hides a caller reading an answer it never requested.
func buildAnswers(a answers, qs map[string]provider.Question) map[string]provider.Answer {
	if _, asked := qs[QuestionSpan]; asked {
		return map[string]provider.Answer{
			QuestionSpan:  {Type: provider.QuestionChoice, Choice: a.span, Probabilities: a.spanProbs},
			QuestionCover: {Type: provider.QuestionNoul, Noul: a.cover},
			QuestionMany:  {Type: provider.QuestionNoul, Noul: a.many},
		}
	}
	if _, asked := qs[QuestionPicture]; asked {
		if a.noPicture {
			return map[string]provider.Answer{}
		}
		return map[string]provider.Answer{
			QuestionPicture: {Type: provider.QuestionNoul, Noul: a.picture},
		}
	}
	if _, asked := qs[QuestionStrategy]; asked {
		return map[string]provider.Answer{
			QuestionStrategy: {Type: provider.QuestionChoice, Choice: a.strategy},
		}
	}
	if q, asked := qs[QuestionPick]; asked {
		ref := a.pick
		if ref == "" && !a.set {
			ref = firstRef(q)
		}
		return map[string]provider.Answer{
			QuestionPick: {Type: provider.QuestionChoice, Choice: ref},
		}
	}
	out := map[string]provider.Answer{}
	if _, asked := qs[QuestionRisk]; asked {
		risk := a.risk
		if risk == "" {
			risk = string(RiskNone)
		}
		out[QuestionRisk] = provider.Answer{Type: provider.QuestionChoice, Choice: risk}
	}
	if _, asked := qs[QuestionEffect]; asked {
		effect := a.effect
		if effect == 0 && !a.set {
			effect = 0.9
		}
		out[QuestionEffect] = provider.Answer{Type: provider.QuestionNoul, Noul: effect}
		out[QuestionGoal] = provider.Answer{Type: provider.QuestionNoul, Noul: a.goal}
	}
	if _, asked := qs[QuestionLoad]; asked {
		out[QuestionLoad] = provider.Answer{Type: provider.QuestionNoul, Noul: a.load}
	}
	if q, asked := qs[QuestionElement]; asked {
		ref, conf := a.element, a.conf
		if ref == "" && !a.set {
			ref, conf = firstRef(q), 0.9
		}
		if conf == 0 {
			conf = 0.9
		}
		out[QuestionElement] = provider.Answer{
			Type: provider.QuestionChoice, Choice: ref, Confidence: conf, Probabilities: a.probs,
		}
	}
	return out
}

// firstRef is the lowest-numbered ref the element question offers, so the
// default answer does not depend on map order.
func firstRef(q provider.Question) string {
	best := ""
	for ref := range q.Criteria {
		if ref == RefNone {
			continue
		}
		if best == "" || ref < best {
			best = ref
		}
	}
	return best
}

// fakeChat is a provider.Provider that answers with scripted text. A reply of
// cutOffReply stands for the answer a reasoning model gives when its whole
// reply budget went into thinking: finished "length", no text at all.
type fakeChat struct {
	mu      sync.Mutex
	replies []string
	reqs    []provider.Request
	err     error
	// cost is what every call reports in its usage, the way OpenRouter
	// prices a call; zero is an upstream that reports none.
	cost provider.NanoUSD
}

// priced is a scripted cost as the transports report it: zero is an upstream
// that reports no price (nil), anything else a price.
func priced(c provider.NanoUSD) *provider.NanoUSD {
	if c == 0 {
		return nil
	}
	return &c
}

// cutOffReply is the scripted truncation marker.
const cutOffReply = "\x00cut-off"

func (f *fakeChat) Name() string { return "fake" }

func (f *fakeChat) Chat(_ context.Context, req provider.Request) (provider.Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	text := "no script left"
	if len(f.replies) > 0 {
		text, f.replies = f.replies[0], f.replies[1:]
	}
	finish := provider.FinishStop
	usage := provider.Usage{PromptTokens: 10, CompletionTokens: 4, Cost: priced(f.cost)}
	if text == cutOffReply {
		text, finish = "", provider.FinishLength
		usage.CompletionTokens = req.MaxTokens
	}
	return &fakeStream{chunks: []provider.Chunk{{
		Kind: provider.ChunkDone,
		Response: &provider.Response{
			Text:   text,
			Finish: finish,
			Usage:  usage,
		},
	}}}, nil
}

func (f *fakeChat) requests() []provider.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]provider.Request(nil), f.reqs...)
}

type fakeStream struct {
	chunks []provider.Chunk
	i      int
}

func (s *fakeStream) Next() (provider.Chunk, error) {
	if s.i >= len(s.chunks) {
		return provider.Chunk{}, io.EOF
	}
	c := s.chunks[s.i]
	s.i++
	return c, nil
}

func (s *fakeStream) Close() error { return nil }

// fakeHumans records every approval and takeover it was asked for.
type fakeHumans struct {
	mu sync.Mutex

	approve  []bool // answers, in order; missing means false
	takeover []bool

	approvals  []string
	riskKinds  []RiskKind
	takeovers  []Blocker
	approveErr error

	// hold, when set, is a takeover lock a person is holding: WaitReleased
	// blocks on it until the channel is closed. waits counts how often the
	// executor asked.
	hold  chan struct{}
	waits int
}

func (f *fakeHumans) Approve(_ context.Context, _ Task, kind RiskKind, summary string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approvals = append(f.approvals, summary)
	f.riskKinds = append(f.riskKinds, kind)
	if f.approveErr != nil {
		return false, f.approveErr
	}
	return pop(&f.approve), nil
}

func (f *fakeHumans) WaitReleased(ctx context.Context, _ Task) error {
	f.mu.Lock()
	f.waits++
	hold := f.hold
	f.mu.Unlock()
	if hold == nil {
		return nil
	}
	select {
	case <-hold:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeHumans) waitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.waits
}

func (f *fakeHumans) Takeover(_ context.Context, _ Task, why Blocker) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.takeovers = append(f.takeovers, why)
	return pop(&f.takeover), nil
}

func (f *fakeHumans) counts() (approvals, takeovers int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.approvals), len(f.takeovers)
}

func pop(s *[]bool) bool {
	if len(*s) == 0 {
		return false
	}
	v := (*s)[0]
	*s = (*s)[1:]
	return v
}

// candidate is shorthand for a shortlist entry. A box a test builds is one
// characters go into, the way a real input is: the pages here are the ordinary
// case, and the box that only opens a list is menuBox below.
func candidate(ref, role, name string) proto.Candidate {
	c := proto.Candidate{Ref: ref, Role: role, Name: name}
	switch role {
	case "textbox", "searchbox", "combobox", "textarea":
		c.Editable = true
	}
	return c
}

// suggestBox is a box that answers what is typed into it with the page's own
// list of suggestions, and says so the way a real one does.
func suggestBox(ref, name string) proto.Candidate {
	return proto.Candidate{Ref: ref, Role: "combobox", Name: name, HasPopup: "listbox", Editable: true}
}

// menuBox is the other kind: a div the site calls a combobox, which takes no
// characters at all and only puts its list on the screen.
func menuBox(ref, name string) proto.Candidate {
	return proto.Candidate{Ref: ref, Role: "combobox", Name: name, HasPopup: "listbox"}
}

// readonlyBox is the field the renfe run failed on: an input the page marks
// readonly, which the accessibility tree still calls a textbox and which takes
// no characters at all, carrying the page's own sentence about how to drive it.
func readonlyBox(ref, name, desc string) proto.Candidate {
	return proto.Candidate{Ref: ref, Role: "textbox", Name: name, Description: desc}
}

// dayCells is what a calendar built out of divs puts on the screen: buttons
// with a number on them and nothing else to say for themselves.
func dayCells(refs ...string) []proto.Candidate {
	out := make([]proto.Candidate, 0, len(refs))
	for _, ref := range refs {
		out = append(out, candidate(ref, "button", "day "+ref))
	}
	return out
}

// optionIn is one of a list's options, saying which element it belongs to.
func optionIn(ref, name, in string) proto.Candidate {
	return proto.Candidate{Ref: ref, Role: "option", Name: name, In: in}
}

// aroundRefs is the ref each observation asked to be read around, in order.
func (f *fakeSidecar) aroundRefs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.around...)
}

// containsFold reports whether s holds sub, ignoring case.
func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

// moveJSON is what a move call answers with: the moves, done and blocked in the
// shape the parser reads. A test that wants an unreadable answer sends prose.
func moveJSON(t *testing.T, done bool, blocked string, ms ...Move) string {
	t.Helper()
	body := struct {
		Done    bool   `json:"done"`
		Moves   []Move `json:"moves"`
		Note    string `json:"note"`
		Blocked string `json:"blocked"`
	}{Done: done, Moves: ms, Blocked: blocked}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal moves: %v", err)
	}
	return string(b)
}

// click and typeInto are the two moves most tests queue.
func click(ref, expect string) Move {
	return Move{Action: MoveClick, Ref: ref, Expect: expect}
}

func typeInto(ref, value, expect string) Move {
	return Move{Action: MoveType, Ref: ref, Value: value, Expect: expect}
}
