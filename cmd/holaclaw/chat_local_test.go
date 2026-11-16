package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/provider"
	"holaclaw/internal/testmodel"
)

// The in-process mode of `chat` runs the same policy gate the daemon does: a
// tool call that policy denies never reaches the tool, and an `ask` waits for
// a human at the terminal. These tests drive it with no daemon listening, so
// `chat` falls back to the local loop.

// localConfig writes a config directory whose socket nothing is listening on,
// so `chat` takes the in-process path.
func localConfig(t *testing.T, tools, policy string, script ...testmodel.Step) string {
	t.Helper()
	e := &daemonEnv{dir: t.TempDir(), socket: shortSocket(t), model: testmodel.New(t, script...)}
	writeDaemonConfig(t, e, tools, policy)
	return e.dir
}

// workspaceFile is where the files tool writes inside a local run.
func workspaceFile(dir, name string) string {
	return filepath.Join(dir, "data", "workspace", "main", name)
}

func TestChatLocalDeniesAnExecCallByPolicy(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran.txt")
	dir := localConfig(t, `["exec"]`, `exec = "deny"`,
		testmodel.Step{ToolCalls: []provider.ToolCall{
			{ID: "c1", Name: "exec", Arguments: `{"command":"touch ` + marker + `"}`},
		}},
		testmodel.Step{Text: "I could not run it"},
	)

	code, out, errOut := runCLI(t, "", "chat", "--config", dir, "-m", "run the thing")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "not running") {
		t.Fatalf("it did not take the in-process path: %q", errOut)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the denied command ran anyway")
	}
	if !strings.Contains(out, "I could not run it") {
		t.Errorf("stdout = %q", out)
	}
	// The gate is what answered, and it told the model so.
	if !strings.Contains(errOut, "allow ") && strings.Contains(errOut, "?") {
		t.Errorf("a deny must not prompt: %q", errOut)
	}
}

func TestChatLocalAsksBeforeWritingAndHonoursTheAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answer  string
		written bool
	}{
		{"yes", "y", true},
		{"no", "n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := localConfig(t, `["files"]`, `write = "ask"`,
				testmodel.Step{ToolCalls: []provider.ToolCall{
					{ID: "c1", Name: "files.write", Arguments: `{"path":"asked.txt","content":"ok"}`},
				}},
				testmodel.Step{Text: "that is done"},
			)
			// One line for the turn, one for the approval, one to leave.
			stdin := "write asked.txt\n" + tc.answer + "\n/quit\n"
			code, _, errOut := runCLI(t, stdin, "chat", "--config", dir)
			if code != 0 {
				t.Fatalf("exit = %d\n%s", code, errOut)
			}
			if !strings.Contains(errOut, "[y]es once") || !strings.Contains(errOut, "[s]ession") {
				t.Errorf("no approval prompt on stderr: %q", errOut)
			}
			if !strings.Contains(errOut, "nothing is persisted") {
				t.Errorf("the prompt did not say choices are not persisted: %q", errOut)
			}
			_, err := os.Stat(workspaceFile(dir, "asked.txt"))
			if tc.written && err != nil {
				t.Errorf("the approved write did not happen: %v", err)
			}
			if !tc.written && err == nil {
				t.Error("the refused write happened anyway")
			}
		})
	}
}

// A session choice covers the rest of the run without asking again.
func TestChatLocalSessionChoiceIsNotAskedTwice(t *testing.T) {
	dir := localConfig(t, `["files"]`, `write = "ask"`,
		testmodel.Step{ToolCalls: []provider.ToolCall{
			{ID: "c1", Name: "files.write", Arguments: `{"path":"one.txt","content":"1"}`},
		}},
		testmodel.Step{Text: "first done"},
		testmodel.Step{ToolCalls: []provider.ToolCall{
			{ID: "c2", Name: "files.write", Arguments: `{"path":"two.txt","content":"2"}`},
		}},
		testmodel.Step{Text: "second done"},
	)
	stdin := "write one\ns\nwrite two\n/quit\n"
	code, _, errOut := runCLI(t, stdin, "chat", "--config", dir)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	if n := strings.Count(errOut, "[y]es once"); n != 1 {
		t.Errorf("asked %d times, want 1:\n%s", n, errOut)
	}
	for _, name := range []string{"one.txt", "two.txt"} {
		if _, err := os.Stat(workspaceFile(dir, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}
}

// -m has nobody to ask, so an `ask` is a deny — it must never fall through to
// running the tool.
func TestChatLocalOneShotDeniesAnAsk(t *testing.T) {
	dir := localConfig(t, `["files"]`, `write = "ask"`,
		testmodel.Step{ToolCalls: []provider.ToolCall{
			{ID: "c1", Name: "files.write", Arguments: `{"path":"asked.txt","content":"ok"}`},
		}},
		testmodel.Step{Text: "I was not allowed"},
	)
	code, out, errOut := runCLI(t, "", "chat", "--config", dir, "-m", "write asked.txt")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "nobody to ask") {
		t.Errorf("stderr = %q", errOut)
	}
	if _, err := os.Stat(workspaceFile(dir, "asked.txt")); err == nil {
		t.Error("a one-shot run wrote the file without an approval")
	}
	if !strings.Contains(out, "I was not allowed") {
		t.Errorf("stdout = %q", out)
	}
}

// The in-process mode starts a new conversation unless asked to resume one
// (#82), and --continue goes to the one used last, not the one created last.
func TestChatLocalResumeAndContinue(t *testing.T) {
	m := testmodel.New(t,
		testmodel.Step{Text: "one"}, testmodel.Step{Text: "two"},
		testmodel.Step{Text: "resumed"}, testmodel.Step{Text: "continued"})
	e := &daemonEnv{dir: t.TempDir(), socket: shortSocket(t), model: m}
	writeDaemonConfig(t, e, "", "")
	_, _, errOut := runCLI(t, "in the first one\n/quit\n", "chat", "--config", e.dir)
	first := chatConversation(t, errOut)
	for _, args := range [][]string{
		{"-m", "in the second one"},
		{"--resume", first, "-m", "back to the first one"},
		{"--continue", "-m", "where am I"},
	} {
		if code, _, errOut := runCLI(t, "", append([]string{"chat", "--config", e.dir}, args...)...); code != 0 {
			t.Fatalf("%v: exit = %d\n%s", args, code, errOut)
		}
	}
	reqs := m.Requests()
	if len(reqs) != 4 {
		t.Fatalf("model saw %d requests, want 4", len(reqs))
	}
	if got := reqs[1].Text(); strings.Contains(got, "in the first one") {
		t.Errorf("a chat with no flags joined the previous conversation:\n%s", got)
	}
	if got := reqs[2].Text(); !strings.Contains(got, "in the first one") || strings.Contains(got, "in the second one") {
		t.Errorf("--resume %s did not join it:\n%s", first, got)
	}
	// The first conversation was used last, so that is where --continue goes.
	if got := reqs[3].Text(); !strings.Contains(got, "back to the first one") {
		t.Errorf("--continue did not resume the most recent conversation:\n%s", got)
	}
}

func TestChatLocalResumeRefusesAnUnknownConversation(t *testing.T) {
	dir := localConfig(t, "", "")
	code, _, errOut := runCLI(t, "", "chat", "--config", dir, "--resume", "nope", "-m", "hola")
	if code == 0 || !strings.Contains(errOut, `no conversation "nope"`) {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
}

func TestChooseConversation(t *testing.T) {
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	convs := []conversationInfo{{name: "a", at: at.Add(2 * time.Hour)}, {name: "b", at: at}, {name: "c", at: at.Add(2 * time.Hour)}}
	list := func() ([]conversationInfo, error) { return convs, nil }
	none := func() ([]conversationInfo, error) { return nil, nil }
	broken := func() ([]conversationInfo, error) { return nil, errors.New("store is gone") }

	pick := func([]conversationInfo) (string, error) { return "", errors.New("asked") }

	if got, err := chooseConversation(chatOptions{}, broken, pick); err != nil || !strings.HasPrefix(got, conversationBase+"-") {
		t.Errorf("no flags = %q, %v; want a new conversation without listing", got, err)
	}
	if got, err := chooseConversation(chatOptions{resume: "b"}, list, pick); err != nil || got != "b" {
		t.Errorf("--resume b = %q, %v", got, err)
	}
	if _, err := chooseConversation(chatOptions{agent: "main", resume: "z"}, list, pick); err == nil {
		t.Error("--resume of an unknown conversation did not fail")
	}
	// a and c tie; c is the newer session.
	if got, err := chooseConversation(chatOptions{cont: true}, list, pick); err != nil || got != "c" {
		t.Errorf("--continue = %q, %v; want c", got, err)
	}
	if got, err := chooseConversation(chatOptions{cont: true}, none, pick); err != nil || !strings.HasPrefix(got, conversationBase+"-") {
		t.Errorf("--continue with nothing to continue = %q, %v; want a new conversation", got, err)
	}
	if _, err := chooseConversation(chatOptions{cont: true}, broken, pick); err == nil {
		t.Error("a failed listing did not fail --continue")
	}
	// --resume with no conversation asks, newest first.
	var offered []string
	got, err := chooseConversation(chatOptions{resume: pickConversation}, list, func(cs []conversationInfo) (string, error) {
		for _, c := range cs {
			offered = append(offered, c.name)
		}
		return cs[len(cs)-1].name, nil
	})
	if err != nil || got != "b" || strings.Join(offered, ",") != "c,a,b" {
		t.Errorf("--resume = %q, %v after offering %v; want b after c,a,b", got, err, offered)
	}
	if _, err := chooseConversation(chatOptions{agent: "main", resume: pickConversation}, none, pick); err == nil ||
		!strings.Contains(err.Error(), "no conversation to resume") {
		t.Errorf("--resume with nothing to resume = %v", err)
	}
}

func TestConversationSuffixDoesNotStack(t *testing.T) {
	first := conversationSuffix("")
	if !strings.HasPrefix(first, conversationBase+"-") {
		t.Fatalf("conversationSuffix(\"\") = %q", first)
	}
	second := conversationSuffix(first)
	if second == first || strings.Count(second, "-") != 1 || !strings.HasPrefix(second, conversationBase+"-") {
		t.Errorf("conversationSuffix(%q) = %q, want a sibling", first, second)
	}
	// A name that only looks like it has a suffix keeps it.
	if got := conversationSuffix("side-quest"); !strings.HasPrefix(got, "side-quest-") {
		t.Errorf("conversationSuffix(side-quest) = %q", got)
	}
}

func TestAskConversation(t *testing.T) {
	at := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	var convs []conversationInfo
	for i := range pickMax + 2 {
		name := fmt.Sprintf("cli-%02d", i)
		convs = append(convs, conversationInfo{name: name, at: at, first: func() string { return "hola\n  from " + name }})
	}
	ask := func(stdin string) (string, string, error) {
		var errOut strings.Builder
		got, err := askConversation(&cli{stdin: strings.NewReader(stdin), stderr: &errOut}, "main", convs)
		return got, errOut.String(), err
	}

	got, shown, err := ask("\n")
	if err != nil || got != "cli-00" {
		t.Errorf("empty answer = %q, %v; want the first", got, err)
	}
	if !strings.Contains(shown, "hola from cli-03") || strings.Contains(shown, "cli-10") {
		t.Errorf("listing:\n%s", shown)
	}
	for in, want := range map[string]string{"3\n": "cli-02", " 10 \n": "cli-09", "cli-11\n": "cli-11", "2": "cli-01"} {
		if got, _, err := ask(in); err != nil || got != want {
			t.Errorf("answer %q = %q, %v; want %s", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0\n", "11\n", "nope\n"} {
		if got, _, err := ask(in); err == nil {
			t.Errorf("answer %q = %q, want an error", in, got)
		}
	}
}

// --resume without a conversation lists the recent ones and resumes the one
// chosen, leaving the rest of stdin to the chat.
func TestChatLocalResumePicks(t *testing.T) {
	m := testmodel.New(t,
		testmodel.Step{Text: "one"}, testmodel.Step{Text: "two"}, testmodel.Step{Text: "picked"})
	e := &daemonEnv{dir: t.TempDir(), socket: shortSocket(t), model: m}
	writeDaemonConfig(t, e, "", "")
	for _, msg := range []string{"in the first one", "in the second one"} {
		if code, _, errOut := runCLI(t, "", "chat", "--config", e.dir, "-m", msg); code != 0 {
			t.Fatalf("%s: exit = %d\n%s", msg, code, errOut)
		}
	}
	code, _, errOut := runCLI(t, "2\nback to the first one\n/quit\n", "chat", "--config", e.dir, "--resume", "--plain")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "1  ") || !strings.Contains(errOut, "in the second one") ||
		strings.Index(errOut, "in the second one") > strings.Index(errOut, "in the first one") {
		t.Errorf("the picker did not list the conversations newest first:\n%s", errOut)
	}
	reqs := m.Requests()
	if len(reqs) != 3 {
		t.Fatalf("model saw %d requests, want 3", len(reqs))
	}
	if got := reqs[2].Text(); !strings.Contains(got, "in the first one") || strings.Contains(got, "in the second one") ||
		!strings.Contains(got, "back to the first one") {
		t.Errorf("the chat did not resume the conversation picked:\n%s", got)
	}
}

func TestChatLocalResumePickFailures(t *testing.T) {
	dir := localConfig(t, "", "", testmodel.Step{Text: "one"})
	code, _, errOut := runCLI(t, "", "chat", "--config", dir, "-r", "-m", "hola")
	if code == 0 || !strings.Contains(errOut, "no conversation to resume") {
		t.Errorf("nothing to resume: exit = %d, stderr = %q", code, errOut)
	}
	if code, _, errOut := runCLI(t, "", "chat", "--config", dir, "-m", "hola"); code != 0 {
		t.Fatalf("exit = %d\n%s", code, errOut)
	}
	code, _, errOut = runCLI(t, "", "chat", "--config", dir, "-r", "-m", "hola")
	if code == 0 || !strings.Contains(errOut, "no conversation chosen") {
		t.Errorf("no answer: exit = %d, stderr = %q", code, errOut)
	}
	// An argument is only ever the conversation of --resume.
	code, _, errOut = runCLI(t, "", "chat", "--config", dir, "stray", "-m", "hola")
	if code != 2 || !strings.Contains(errOut, `unknown command "stray"`) {
		t.Errorf("stray argument: exit = %d, stderr = %q", code, errOut)
	}
}
