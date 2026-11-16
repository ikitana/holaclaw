//go:build linux

package browsereval

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/browser/chromium"
	"holaclaw/internal/provider"
	"holaclaw/tools/eval/internal/record"
	"holaclaw/tools/eval/internal/sites"
)

// This is the runner driven end to end: the real page engine on a real
// Chromium, the real synthetic sites, the real browser executor — and a
// scripted model in place of the live one, because live and record modes spend
// the owner's credits and are never run from a test.
//
// What it proves: the plan → decide → act → extract path reaches a verdict,
// the cookie banner is dismissed before the answer is read, the injection page
// is answered without the decoy being followed (sites.Leaked stays false), and
// the baseline loop has the shape it claims — one chat call per step, over the
// same engine, scored by the same verdicts.

func testEnv(t *testing.T) *Env {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping: -short")
	}
	if _, err := chromium.Find(""); err != nil {
		t.Skipf("skipping: no browser: %v", err)
	}
	zero := 0.0
	env, err := NewEnv(EnvOptions{Headless: true, Pacing: &zero})
	if err != nil {
		t.Fatalf("NewEnv: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		env.Close(ctx)
	})
	return env
}

func pickTasks(t *testing.T, names ...string) []Task {
	t.Helper()
	all, err := Load(tasksFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tasks, err := Select(all, names)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	return tasks
}

// TestExecutorEndToEnd drives the cookies and inject tasks through the real
// executor with a scripted plan and scripted step decisions.
func TestExecutorEndToEnd(t *testing.T) {
	env := testEnv(t)
	script := &scriptedChat{}
	rep, err := Run(context.Background(), Options{
		Targets: []Target{{
			Label:    "scripted",
			Model:    "scripted",
			ChatSpec: emptySpec,
			NewChat:  func() (provider.Provider, error) { return script, nil },
		}},
		Tasks:    pickTasks(t, "cookies", "inject"),
		Sidecar:  env.Engine,
		StartURL: env.StartURL,
		Host:     env.Host,
		Mode:     record.ModeLive,
		Timeout:  90 * time.Second,
		Browser:  env.Browser.String(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Results) != 2 {
		t.Fatalf("%d results", len(rep.Results))
	}
	for _, res := range rep.Results {
		if res.Status != StatusPass {
			t.Errorf("%s: %s %v (outcome %s, error %s)", res.Task, res.Status, res.Verdict.Reasons, res.Outcome, res.Error)
		}
		if res.Metrics.GenerativeCalls < 2 { // at least the plan and the extraction
			t.Errorf("%s: %d generative calls", res.Task, res.Metrics.GenerativeCalls)
		}
		// The cookies task has a banner to get out of the way, so it carries out
		// a move and the decision model verifies it. The inject task's answer is
		// on the page the run opens, which under M11 is one move call and no
		// move at all.
		if res.Task == "cookies" {
			if res.Metrics.DecisionCalls == 0 {
				t.Errorf("%s: no decision calls", res.Task)
			}
			if res.Metrics.Rungs[2] == 0 {
				t.Errorf("%s: no move was carried out: %v", res.Task, res.Metrics.Rungs)
			}
		}
	}
	// The injection defence, measured where it matters: the decoy page records
	// every hit, and judge() fails the run when it was hit at all.
	if sites.Leaked(runID("scripted", "inject", 1)) {
		t.Error("the injected instruction was followed")
	}
	if rep.Tasks[0].Passed != 1 || rep.Failed() != 0 {
		t.Errorf("summary: %+v", rep.Tasks)
	}
}

// TestBaselineEndToEnd drives the same cookies task with the naive per-click
// loop: one chat call per step, the whole observation in each prompt.
func TestBaselineEndToEnd(t *testing.T) {
	env := testEnv(t)
	script := &baselineChat{}
	rep, err := Run(context.Background(), Options{
		Targets: []Target{{
			Label:    "scripted-baseline",
			Model:    "scripted",
			ChatSpec: emptySpec,
			NewChat:  func() (provider.Provider, error) { return script, nil },
			Baseline: true,
		}},
		Tasks:    pickTasks(t, "cookies"),
		Sidecar:  env.Engine,
		StartURL: env.StartURL,
		Host:     env.Host,
		Mode:     record.ModeLive,
		Timeout:  90 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	res := rep.Results[0]
	if res.Status != StatusPass {
		t.Fatalf("baseline: %s %v (outcome %s, error %s)", res.Status, res.Verdict.Reasons, res.Outcome, res.Error)
	}
	// The shape of the loop: a chat call for every step it took, and no
	// decision calls at all — that is what the baseline column means.
	if res.Metrics.DecisionCalls != 0 {
		t.Errorf("baseline made %d decision calls", res.Metrics.DecisionCalls)
	}
	if res.Metrics.GenerativeCalls != script.calls() {
		t.Errorf("counted %d generative calls, the model saw %d", res.Metrics.GenerativeCalls, script.calls())
	}
	if res.Metrics.GenerativeCalls < 2 || res.Metrics.Steps < 2 {
		t.Errorf("baseline metrics %+v", res.Metrics)
	}
	if !script.sawText {
		t.Error("the baseline prompt carried no page text")
	}
}

func emptySpec() provider.Model {
	return provider.Model{Name: "scripted", Provider: "eval", ID: "scripted", Context: 32768}
}

// --- the scripted models -------------------------------------------------

// candidateLine matches one line of the executor's rendered shortlist:
// `  ref: role "name"`.
var candidateLine = regexp.MustCompile(`(?m)^\s+(\S+): (\S+) "([^"]*)"`)

// refFor returns the ref of the first candidate whose name holds want.
func refFor(state, want string) string {
	for _, m := range candidateLine.FindAllStringSubmatch(state, -1) {
		if strings.Contains(strings.ToLower(m[3]), strings.ToLower(want)) {
			return m[1]
		}
	}
	return ""
}

// scriptedChat answers the executor's three kinds of chat call by reading the
// prompt it was given: the moves for the page in front of it, the narrow
// questions about one move, and the final extraction (M11).
type scriptedChat struct {
	mu sync.Mutex
	n  int
}

func (s *scriptedChat) Name() string { return "scripted" }

func (s *scriptedChat) Chat(_ context.Context, req provider.Request) (provider.Stream, error) {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	last := ""
	for _, m := range req.Messages {
		if t := m.Text(); t != "" {
			last = t
		}
	}
	return &oneShot{text: s.answer(req.System, last)}, nil
}

func (s *scriptedChat) answer(system, state string) string {
	switch {
	case strings.Contains(system, "narrow questions"): // one move, before or after
		return s.decide(state)
	case strings.Contains(system, "Answer the request from the page content"):
		return extractFrom(state)
	default: // the move call
		return s.move(state)
	}
}

// move is the one text call: the consent banner is the only thing this model
// ever clicks, and a page that already shows what was asked for is done.
func (s *scriptedChat) move(state string) string {
	switch {
	case answerOn(state):
		return `{"done":true,"moves":[]}`
	case refFor(state, "accept") != "":
		return fmt.Sprintf(`{"done":false,"moves":[{"action":"click","ref":%q,`+
			`"expect":"the banner is gone and the page can be read"}]}`, refFor(state, "accept"))
	default:
		return `{"done":false,"moves":[{"action":"wait","expect":"the page has settled"}]}`
	}
}

// decide answers whichever of the narrow questions the state gave material for:
// nothing a move here commits, the banner dismissal is what it says it is, and
// the goal is reached once the answer is on the page.
func (s *scriptedChat) decide(state string) string {
	goal := 0.0
	if answerOn(state) {
		goal = 1.0
	}
	return fmt.Sprintf(`{"element":%q,"element_confidence":0.95,"risk":"none",`+
		`"effect":0.9,"goal":%.1f}`, refFor(state, "accept"), goal)
}

// answerOn reports whether what the task asks for is visible in the rendered
// page text of the state.
func answerOn(state string) bool {
	return strings.Contains(state, "+1-555-0142") || strings.Contains(state, "9am-5pm")
}

var phone = regexp.MustCompile(`\+1-555-0142`)
var hours = regexp.MustCompile(`Mon-Fri, 9am-5pm ET\.`)

func extractFrom(state string) string {
	if m := phone.FindString(state); m != "" {
		return m
	}
	if m := hours.FindString(state); m != "" {
		return m
	}
	return "not found"
}

// baselineChat is the naive loop's model: click the consent button, then read
// the phone number off the page and say done.
type baselineChat struct {
	mu      sync.Mutex
	n       int
	sawText bool
}

func (b *baselineChat) Name() string { return "scripted" }

func (b *baselineChat) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
}

func (b *baselineChat) Chat(_ context.Context, req provider.Request) (provider.Stream, error) {
	last := ""
	for _, m := range req.Messages {
		if t := m.Text(); t != "" {
			last = t
		}
	}
	b.mu.Lock()
	b.n++
	if strings.Contains(last, "page text:") && len(strings.TrimSpace(last)) > len("page text:") {
		b.sawText = true
	}
	b.mu.Unlock()

	if m := phone.FindString(last); m != "" {
		return &oneShot{text: fmt.Sprintf(`{"action":"done","answer":%q}`, m)}, nil
	}
	ref := baselineRefFor(last, "accept")
	if ref == "" {
		return &oneShot{text: `{"action":"scroll"}`}, nil
	}
	return &oneShot{text: fmt.Sprintf(`{"action":"click","ref":%q}`, ref)}, nil
}

// baselineRefFor reads the baseline prompt's own element list:
// `- ref e1: button "Accept all"`.
var baselineLine = regexp.MustCompile(`(?m)^- ref (\S+): (\S+) "([^"]*)"`)

func baselineRefFor(prompt, want string) string {
	for _, m := range baselineLine.FindAllStringSubmatch(prompt, -1) {
		if strings.Contains(strings.ToLower(m[3]), strings.ToLower(want)) {
			return m[1]
		}
	}
	return ""
}

// oneShot is a provider.Stream that answers with one text response.
type oneShot struct {
	text string
	i    int
}

func (s *oneShot) Next() (provider.Chunk, error) {
	switch s.i {
	case 0:
		s.i++
		return provider.Chunk{Kind: provider.ChunkText, Text: s.text}, nil
	case 1:
		s.i++
		return provider.Chunk{Kind: provider.ChunkDone, Response: &provider.Response{
			Text:   s.text,
			Finish: provider.FinishStop,
			Usage:  provider.Usage{PromptTokens: 100, CompletionTokens: 20},
		}}, nil
	default:
		return provider.Chunk{}, io.EOF
	}
}

func (s *oneShot) Close() error { return nil }
