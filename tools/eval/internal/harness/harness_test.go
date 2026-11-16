package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/provider"
	"holaclaw/tools/eval/internal/record"
	"holaclaw/tools/eval/internal/scenario"
)

// scripted is a fake model: it calls the first offered tool once, then
// answers from the tool result. No network, no recordings needed.
type scripted struct {
	calls int
	err   error
}

func (s *scripted) Name() string { return "scripted" }

func (s *scripted) Chat(_ context.Context, req provider.Request) (provider.Stream, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	last := ""
	for _, m := range req.Messages {
		if m.Role == provider.RoleTool {
			last = m.Text()
		}
	}
	resp := &provider.Response{Finish: provider.FinishStop, Usage: provider.Usage{PromptTokens: 10, CompletionTokens: 3}}
	switch {
	case len(req.Tools) > 0 && last == "":
		resp.Finish = provider.FinishToolCalls
		resp.ToolCalls = []provider.ToolCall{{ID: "call-1", Name: req.Tools[0].Name, Arguments: `{"city":"Mirefield"}`}}
	case last != "":
		resp.Text = "Tool says: " + last
	default:
		resp.Text = "The answer is 42."
	}
	return &stream{resp: resp}, nil
}

type stream struct {
	resp *provider.Response
	i    int
}

func (s *stream) Next() (provider.Chunk, error) {
	switch s.i {
	case 0:
		s.i++
		if s.resp.Text != "" {
			return provider.Chunk{Kind: provider.ChunkText, Text: s.resp.Text}, nil
		}
		fallthrough
	case 1:
		s.i = 2
		return provider.Chunk{Kind: provider.ChunkDone, Response: s.resp}, nil
	default:
		return provider.Chunk{}, io.EOF
	}
}

func (s *stream) Close() error { return nil }

const chatScenario = `
name = "chat"
max_steps = 4
[[turns]]
user = "Hello there, what is the answer?"
  [turns.expect]
  no_tool_calls = true
  reply_contains = ["42"]
  max_steps = 1
  stop = "final"
`

const toolyScenario = `
name = "tooly"
max_steps = 4
[[tools]]
name = "weather"
description = "Current weather for a city."
parameters = '''{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}'''
  [[tools.responses]]
  text = "light rain, 11 C"
[[turns]]
user = "What is the weather in Mirefield?"
  [turns.expect]
  reply_contains = ["rain"]
  max_steps = 3
  max_prompt_tokens = 4000
  [[turns.expect.tool_calls]]
  name = "weather"
  args = { city = "Mirefield" }
`

const failingScenario = `
name = "zfails"
max_steps = 4
[[turns]]
user = "Hello there, what is the answer?"
  [turns.expect]
  reply_contains = ["banana"]
`

// scenarioDir writes the given scenario bodies and loads them.
func scenarioDir(t *testing.T, bodies ...string) (string, []scenario.Scenario) {
	t.Helper()
	dir := t.TempDir()
	for i, b := range bodies {
		name := filepath.Join(dir, string(rune('a'+i))+".toml")
		if err := os.WriteFile(name, []byte(b), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ss, err := scenario.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, ss
}

func target(name string, p provider.Provider) Target {
	return Target{
		Name: name,
		Spec: func(window int) provider.Model {
			return provider.Model{Name: name, Provider: "test", ID: "fake", Context: window, Tools: true}
		},
		New: func() (provider.Provider, error) { return p, nil },
	}
}

func byName(t *testing.T, r Report, scenarioName string) RunResult {
	t.Helper()
	for _, run := range r.Runs {
		if run.Scenario == scenarioName {
			return run
		}
	}
	t.Fatalf("no run for scenario %q", scenarioName)
	return RunResult{}
}

func TestRunLiveWithScriptedModel(t *testing.T) {
	_, ss := scenarioDir(t, chatScenario, toolyScenario, failingScenario)
	fake := &scripted{}
	rep, err := Run(context.Background(), Options{
		Targets:   []Target{target("fake", fake)},
		Scenarios: ss,
		Mode:      record.ModeLive,
		RecordDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rep.Runs) != 3 {
		t.Fatalf("got %d runs, want 3", len(rep.Runs))
	}
	if rep.Failed() != 1 {
		t.Fatalf("failed = %d, want 1", rep.Failed())
	}

	chat := byName(t, rep, "chat")
	if chat.Status != StatusPass || chat.Steps != 1 || chat.PromptTokens == 0 {
		t.Fatalf("chat run = %+v", chat)
	}
	if chat.Usage.PromptTokens != 10 {
		t.Fatalf("chat usage = %+v", chat.Usage)
	}

	tooly := byName(t, rep, "tooly")
	if tooly.Status != StatusPass {
		t.Fatalf("tooly run = %+v", tooly)
	}
	if got := tooly.Turns[0].Calls; len(got) != 1 || got[0] != "weather" {
		t.Fatalf("tooly calls = %v", got)
	}
	if tooly.Steps != 2 {
		t.Fatalf("tooly steps = %d, want 2 (call, then answer)", tooly.Steps)
	}

	bad := byName(t, rep, "zfails")
	if bad.Status != StatusFail || len(bad.Turns[0].Failures) != 1 {
		t.Fatalf("zfails run = %+v", bad)
	}

	var out bytes.Buffer
	WriteTable(&out, rep)
	table := out.String()
	for _, want := range []string{"chat", "tooly", "zfails", "1/1", "0/1", "failures (1)", "banana"} {
		if !strings.Contains(table, want) {
			t.Fatalf("table lacks %q:\n%s", want, table)
		}
	}

	var js bytes.Buffer
	if err := WriteJSON(&js, rep); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(js.Bytes(), &back); err != nil {
		t.Fatalf("json report: %v", err)
	}
	if len(back.Runs) != 3 || back.Mode != string(record.ModeLive) {
		t.Fatalf("decoded report = %+v", back)
	}
}

func TestRecordThenReplayRoundTrip(t *testing.T) {
	dir, ss := scenarioDir(t, chatScenario, toolyScenario)
	recDir := t.TempDir()
	fake := &scripted{}
	opts := Options{
		Targets:   []Target{target("fake", fake)},
		Scenarios: ss,
		Mode:      record.ModeRecord,
		RecordDir: recDir,
	}
	rec, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Failed() != 0 {
		t.Fatalf("record run failed: %+v", rec.Runs)
	}
	for _, name := range []string{"chat", "tooly"} {
		if _, err := os.Stat(record.Path(recDir, "fake", name, 1)); err != nil {
			t.Fatalf("no recording for %s: %v", name, err)
		}
	}
	recorded := fake.calls
	if recorded != 3 {
		t.Fatalf("model calls = %d, want 3", recorded)
	}

	// Replay never opens a provider: New fails loudly if it is used.
	replayTarget := target("fake", fake)
	replayTarget.New = func() (provider.Provider, error) { return nil, errors.New("replay must not open a provider") }
	opts.Mode, opts.Targets = record.ModeReplay, []Target{replayTarget}
	rep, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed() != 0 {
		t.Fatalf("replay failed: %+v", rep.Runs)
	}
	if fake.calls != recorded {
		t.Fatalf("replay called the model %d extra times", fake.calls-recorded)
	}
	if got, want := byName(t, rep, "tooly").Turns[0].Reply, byName(t, rec, "tooly").Turns[0].Reply; got != want {
		t.Fatalf("replayed reply %q, recorded %q", got, want)
	}

	// A changed prompt must not silently replay the old response.
	changed := strings.Replace(chatScenario, "what is the answer?", "what is the other answer?", 1)
	if err := os.WriteFile(filepath.Join(dir, "a.toml"), []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	opts.Scenarios, err = scenario.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	rep, err = Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	chat := byName(t, rep, "chat")
	if chat.Status != StatusFail || !strings.Contains(chat.Reason, "does not match this prompt") {
		t.Fatalf("changed prompt gave %+v, want a digest mismatch failure", chat)
	}
}

func TestReplayWithoutRecording(t *testing.T) {
	_, ss := scenarioDir(t, chatScenario)
	rep, err := Run(context.Background(), Options{
		Targets:   []Target{target("fake", &scripted{})},
		Scenarios: ss,
		Mode:      record.ModeReplay,
		RecordDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if run := rep.Runs[0]; run.Status != StatusFail || !strings.Contains(run.Reason, "no recording") {
		t.Fatalf("run = %+v, want a missing-recording failure", run)
	}
}

func TestSkippedTarget(t *testing.T) {
	_, ss := scenarioDir(t, chatScenario)
	tg := target("nokey", &scripted{})
	tg.Skip = "no API key: OPENROUTER_KEY is not set"
	rep, err := Run(context.Background(), Options{Targets: []Target{tg}, Scenarios: ss, Mode: record.ModeLive})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed() != 0 || rep.Runs[0].Status != StatusSkip {
		t.Fatalf("run = %+v, want a skip", rep.Runs[0])
	}
	var out bytes.Buffer
	WriteTable(&out, rep)
	if !strings.Contains(out.String(), "OPENROUTER_KEY") {
		t.Fatalf("table lacks the skip reason:\n%s", out.String())
	}
}

func TestRateLimitedRunIsRetriedThenSkipped(t *testing.T) {
	_, ss := scenarioDir(t, chatScenario)
	fake := &scripted{err: &provider.Error{Kind: provider.ErrRateLimit, Provider: "fake", RetryAfter: time.Second}}
	var waits []time.Duration
	rep, err := Run(context.Background(), Options{
		Targets:   []Target{target("fake", fake)},
		Scenarios: ss,
		Mode:      record.ModeLive,
		RecordDir: t.TempDir(),
		Sleep:     func(d time.Duration) { waits = append(waits, d) },
	})
	if err != nil {
		t.Fatal(err)
	}
	run := rep.Runs[0]
	if run.Status != StatusSkip || run.Reason != "skipped: rate limited" {
		t.Fatalf("run = %+v", run)
	}
	if len(waits) != maxRateLimitRetries {
		t.Fatalf("waited %v, want %d retries", waits, maxRateLimitRetries)
	}
	for _, w := range waits {
		if w != time.Second {
			t.Fatalf("wait %v, want the provider RetryAfter", w)
		}
	}
}

func TestRunRejectsEmptyInput(t *testing.T) {
	if _, err := Run(context.Background(), Options{}); err == nil {
		t.Fatal("want an error without targets")
	}
	if _, err := Run(context.Background(), Options{Targets: []Target{target("fake", &scripted{})}}); err == nil {
		t.Fatal("want an error without scenarios")
	}
}

func TestMultiTurnHistoryCarriesOver(t *testing.T) {
	const twoTurns = `
name = "memory"
max_steps = 4
[[turns]]
user = "First question, please answer."
  [turns.expect]
  no_tool_calls = true
[[turns]]
user = "Second question, please answer."
  [turns.expect]
  no_tool_calls = true
`
	_, ss := scenarioDir(t, twoTurns)
	fake := &recordingProvider{}
	rep, err := Run(context.Background(), Options{
		Targets:   []Target{target("fake", fake)},
		Scenarios: ss,
		Mode:      record.ModeLive,
		RecordDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Failed() != 0 {
		t.Fatalf("runs = %+v", rep.Runs)
	}
	if len(fake.reqs) != 2 {
		t.Fatalf("got %d model calls, want 2", len(fake.reqs))
	}
	// The second prompt carries the first turn and its reply.
	if got := len(fake.reqs[1].Messages); got != 3 {
		t.Fatalf("second prompt has %d messages, want 3", got)
	}
	if !strings.Contains(fake.reqs[1].Messages[0].Text(), "First question") {
		t.Fatalf("second prompt lost the first turn: %+v", fake.reqs[1].Messages)
	}
}

// recordingProvider answers plainly and keeps every request.
type recordingProvider struct{ reqs []provider.Request }

func (p *recordingProvider) Name() string { return "recording" }

func (p *recordingProvider) Chat(_ context.Context, req provider.Request) (provider.Stream, error) {
	p.reqs = append(p.reqs, req)
	return &stream{resp: &provider.Response{Text: "Noted.", Finish: provider.FinishStop}}, nil
}

// capture answers every call with text and keeps the requests.
type capture struct{ reqs []provider.Request }

func (c *capture) Name() string { return "capture" }

func (c *capture) Chat(_ context.Context, req provider.Request) (provider.Stream, error) {
	c.reqs = append(c.reqs, req)
	return &stream{resp: &provider.Response{Text: "ok", Finish: provider.FinishStop}}, nil
}

// A scenario's skills reach the prompt as the skills fragment, and skill.load
// is offered only when there are skills to load.
func TestScenarioSkillsReachThePrompt(t *testing.T) {
	const load = `
[[tools]]
name = "skill.load"
description = "Load a skill."
parameters = '''{"type":"object","properties":{"name":{"type":"string"}}}'''
  [[tools.responses]]
  text = "[skill: notes · agent skill]"
[[turns]]
user = "hi"
`
	for _, tc := range []struct {
		name, body string
		offered    bool
	}{
		{"with", "name = \"with\"\n[[skills]]\nname = \"notes\"\ndescription = \"When taking notes.\"\n" + load, true},
		{"without", "name = \"without\"\n" + load, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ss := scenarioDir(t, tc.body)
			c := &capture{}
			if _, err := Run(context.Background(), Options{
				Targets: []Target{target("fake", c)}, Scenarios: ss,
				Mode: record.ModeLive, RecordDir: t.TempDir(),
			}); err != nil {
				t.Fatal(err)
			}
			req := c.reqs[0]
			if got := strings.Contains(req.System, "- notes: When taking notes."); got != tc.offered {
				t.Fatalf("index in system = %v:\n%s", got, req.System)
			}
			if got := len(req.Tools) == 1 && req.Tools[0].Name == "skill.load"; got != tc.offered {
				t.Fatalf("tools = %v", req.Tools)
			}
		})
	}
}
