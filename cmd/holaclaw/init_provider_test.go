package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holaclaw/internal/app"
	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
	"holaclaw/internal/store"
	"holaclaw/internal/testmodel"
)

// atTerminal makes init see a terminal on stdin, and counts the times it
// turned the echo off.
func atTerminal(t *testing.T) *int {
	t.Helper()
	isTerm, off := stdinIsTerminal, echoOff
	t.Cleanup(func() { stdinIsTerminal, echoOff = isTerm, off })
	n := 0
	stdinIsTerminal = func(io.Reader) bool { return true }
	echoOff = func(io.Reader) (func(), error) { n++; return func() {}, nil }
	return &n
}

// initIn runs init in a new config directory and returns it.
func initIn(t *testing.T, stdin string, args ...string) (dir string, code int, out, errOut string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "config")
	code, out, errOut = runCLI(t, stdin, append([]string{"init", "--config", dir}, args...)...)
	return dir, code, out, errOut
}

// storedKey is the api_key of a provider in the secrets of dir.
func storedKey(t *testing.T, dir, provider string) (string, bool) {
	t.Helper()
	f, err := secrets.Open(filepath.Join(dir, "secrets", config.FileAuth))
	if err != nil {
		t.Fatal(err)
	}
	return f.Get(secrets.Providers, provider, "api_key")
}

// Every provider, non-interactive: one provider, one model, the main agent on
// it, a config that loads, and the key from stdin where it takes one.
func TestInitProviders(t *testing.T) {
	cases := []struct {
		args                 []string
		stdin                string
		provider, typ, model string
		key                  string // "" = no key stored
		want, notWant        []string
	}{
		{args: []string{"--provider", "openai"}, stdin: "sk-openai\n",
			provider: "openai", typ: "openai-compatible", model: "gpt-6.1-sol", key: "sk-openai",
			want: []string{`base_url = "https://api.openai.com/v1"`, `auth     = "api-key"`}},
		{args: []string{"--provider", "openai-codex"}, stdin: "not a key",
			provider: "codex", typ: "codex", model: "gpt-6.1-sol",
			want: []string{`auth     = "subscription"`}, notWant: []string{"base_url"}},
		{args: []string{"--provider", "openrouter", "--model", "qwen/qwen3.8-27b"}, stdin: "sk-or",
			provider: "openrouter", typ: "openai-compatible", model: "qwen/qwen3.8-27b", key: "sk-or",
			want: []string{`base_url = "https://openrouter.ai/api/v1"`, `sort     = "throughput"`}},
		{args: []string{"--provider", "openai-compat", "--base-url", "http://host.docker.internal:11434/v1", "--model", "gemma4:e2b"},
			provider: "openai-compat", typ: "openai-compatible", model: "gemma4:e2b",
			want: []string{`base_url = "http://host.docker.internal:11434/v1"`}, notWant: []string{"auth"}},
		{args: []string{"--provider", "openai-compat", "--base-url", "https://llm.example/v1", "--model", "m"}, stdin: "k",
			provider: "openai-compat", typ: "openai-compatible", model: "m", key: "k",
			want: []string{`auth     = "api-key"`}},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			token := filepath.Join(t.TempDir(), "token")
			dir, code, out, errOut := initIn(t, tc.stdin, append(tc.args, "--token-file", token)...)
			if code != 0 {
				t.Fatalf("init: exit = %d\n%s", code, errOut)
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing with --token-file", out)
			}
			a, err := app.Static(dir)
			if err != nil {
				t.Fatalf("the config does not load: %v", err)
			}
			if len(a.Config.Providers) != 1 || len(a.Config.Models) != 1 {
				t.Fatalf("providers %v, models %v: want one of each", a.Config.Providers, a.Config.Models)
			}
			p, m := a.Config.Providers[tc.provider], a.Config.Models[initModelName]
			if p.Type != tc.typ || m.Provider != tc.provider || m.ID != tc.model {
				t.Errorf("provider %+v, model %+v", p, m)
			}
			if got := a.Config.Agents["main"].Models.Chat; got != initModelName {
				t.Errorf("chat role = %q", got)
			}
			body, err := os.ReadFile(filepath.Join(dir, config.FileConfig))
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(string(body), w) {
					t.Errorf("config.toml lacks %q:\n%s", w, body)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(string(body), w) {
					t.Errorf("config.toml has %q:\n%s", w, body)
				}
			}
			key, ok := storedKey(t, dir, tc.provider)
			if key != tc.key || ok != (tc.key != "") {
				t.Errorf("stored key = %q, %v; want %q", key, ok, tc.key)
			}
			if tc.provider == "codex" && !strings.Contains(errOut, "holaclaw login codex") {
				t.Errorf("stderr does not give the sign-in step: %q", errOut)
			}
			if strings.Contains(errOut, "auth set") {
				t.Errorf("stderr asks for a key init has or needs not: %q", errOut)
			}
			secret, err := os.ReadFile(token)
			if err != nil {
				t.Fatal(err)
			}
			if pr := ownerOf(t, dir, strings.TrimSpace(string(secret))); pr.Role != store.RoleOwner {
				t.Errorf("principal = %+v", pr)
			}
		})
	}
}

// With nothing on stdin, a key provider is written without a key, and stderr
// says how to store it; stdout stays empty.
func TestInitWithoutKey(t *testing.T) {
	dir, code, out, errOut := initIn(t, "", "--provider", "openrouter")
	if code != 0 {
		t.Fatalf("init: exit = %d\n%s", code, errOut)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing", out)
	}
	if !strings.Contains(errOut, `printf %s "$KEY" | holaclaw auth set providers.openrouter api_key`) {
		t.Errorf("stderr = %q, want the auth set command", errOut)
	}
	if _, ok := storedKey(t, dir, "openrouter"); ok {
		t.Error("a key was stored from an empty stdin")
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets", config.FileAuth)); err == nil {
		t.Error("auth.toml written with no key")
	}
}

// The flags init cannot do without, in non-interactive mode.
func TestInitNonInteractiveFailures(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{nil, "pass --provider openai|openai-codex|openrouter|openai-compat"},
		{[]string{"--provider", "ollama"}, `unknown provider "ollama"`},
		{[]string{"--provider", "openai-compat", "--model", "m"}, "needs --base-url"},
		{[]string{"--provider", "openai-compat", "--base-url", "http://h/v1"}, "needs --model"},
		{[]string{"--provider", "openai-compat", "--base-url", "h:11434", "--model", "m"}, "not an http or https URL"},
		{[]string{"--provider", "openrouter", "--base-url", "http://h/v1"}, "only for --provider openai-compat"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			dir, code, out, errOut := initIn(t, "", tc.args...)
			if code != 1 || out != "" || !strings.Contains(errOut, tc.want) {
				t.Fatalf("exit = %d, stdout %q, stderr %q; want %q", code, out, errOut, tc.want)
			}
			if _, err := os.Stat(dir); err == nil {
				t.Error("a failed init wrote the config directory")
			}
		})
	}
}

// --non-interactive at a terminal never prompts, and reads no key.
func TestInitForcedNonInteractive(t *testing.T) {
	off := atTerminal(t)
	if _, code, _, errOut := initIn(t, "3\n", "--non-interactive"); code != 1 || !strings.Contains(errOut, "pass --provider") {
		t.Fatalf("exit = %d, stderr %q", code, errOut)
	}
	dir, code, _, errOut := initIn(t, "sk-typed\n", "--non-interactive", "--provider", "openrouter")
	if code != 0 {
		t.Fatalf("init: exit = %d\n%s", code, errOut)
	}
	if _, ok := storedKey(t, dir, "openrouter"); ok || *off != 0 || strings.Contains(errOut, "API key for") {
		t.Errorf("init read a key at the terminal: stderr %q", errOut)
	}
}

// Interactive: the provider by number, the default model accepted, the key
// read with the echo off.
func TestInitInteractive(t *testing.T) {
	off := atTerminal(t)
	dir, code, _, errOut := initIn(t, "9\n3\n\nsk-typed\n")
	if code != 0 {
		t.Fatalf("init: exit = %d\n%s", code, errOut)
	}
	for _, w := range []string{"Which provider?", "4) openai-compat", "Model id [deepseek/deepseek-v4.1-flash]: ", "API key for openrouter"} {
		if !strings.Contains(errOut, w) {
			t.Errorf("stderr lacks %q:\n%s", w, errOut)
		}
	}
	if strings.Count(errOut, "Which provider?") != 2 {
		t.Errorf("an invalid answer was not asked again:\n%s", errOut)
	}
	if key, _ := storedKey(t, dir, "openrouter"); key != "sk-typed" || *off != 1 {
		t.Errorf("key = %q, echo off %d times", key, *off)
	}
	a, err := app.Static(dir)
	if err != nil {
		t.Fatal(err)
	}
	if id := a.Config.Models[initModelName].ID; id != "deepseek/deepseek-v4.1-flash" {
		t.Errorf("model = %q", id)
	}
}

// Interactive openai-compat asks for the base URL until it is one, and for
// the model; an empty key means no auth.
func TestInitInteractiveCompat(t *testing.T) {
	atTerminal(t)
	dir, code, _, errOut := initIn(t, "openai-compat\nlocalhost:11434\nhttp://localhost:11434/v1\n\ngemma4:e2b\n\n")
	if code != 0 {
		t.Fatalf("init: exit = %d\n%s", code, errOut)
	}
	if strings.Count(errOut, "Base URL") != 2 || strings.Count(errOut, "Model id: ") != 2 {
		t.Errorf("stderr:\n%s", errOut)
	}
	a, err := app.Static(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := a.Config.Providers["openai-compat"]
	if p.BaseURL != "http://localhost:11434/v1" || p.Auth != "none" || a.Config.Models[initModelName].ID != "gemma4:e2b" {
		t.Errorf("provider %+v, model %+v", p, a.Config.Models[initModelName])
	}
	if strings.Contains(errOut, "auth set") {
		t.Errorf("a keyless endpoint got the auth set step: %q", errOut)
	}
}

// Interactive codex asks for no key and never signs in.
func TestInitInteractiveCodex(t *testing.T) {
	off := atTerminal(t)
	_, code, _, errOut := initIn(t, "2\ngpt-6-luna\n")
	if code != 0 {
		t.Fatalf("init: exit = %d\n%s", code, errOut)
	}
	if *off != 0 || strings.Contains(errOut, "API key for") || !strings.Contains(errOut, "holaclaw login codex") {
		t.Errorf("stderr:\n%s", errOut)
	}
}

// The empty config a container starts with is replaced, keeping its data and
// secrets directories; a failed init puts it back.
func TestInitReplacesTheEmptyConfig(t *testing.T) {
	dir := t.TempDir()
	data, sec := filepath.Join(t.TempDir(), "data"), filepath.Join(t.TempDir(), "secrets")
	empty := "# no provider yet\nversion = 1\n\n[server]\ndata_dir = \"" + data + "\"\nsecrets_dir = \"" + sec + "\"\n"
	cfg := filepath.Join(dir, config.FileConfig)
	if err := os.WriteFile(cfg, []byte(empty), 0o640); err != nil {
		t.Fatal(err)
	}

	bad := filepath.Join(t.TempDir(), "missing", "token")
	if code, _, errOut := runCLI(t, "sk", "init", "--config", dir, "--provider", "openrouter", "--token-file", bad); code != 1 {
		t.Fatalf("init with an unwritable token: exit = %d\n%s", code, errOut)
	}
	if body, _ := os.ReadFile(cfg); string(body) != empty {
		t.Fatalf("a failed init left config.toml as %q", body)
	}
	if _, err := os.Stat(filepath.Join(dir, "agents")); err == nil {
		t.Fatal("a failed init left agents/ behind")
	}
	if _, err := os.Stat(filepath.Join(sec, config.FileAuth)); err == nil {
		t.Fatal("a failed init left the key behind")
	}

	code, _, errOut := runCLI(t, "sk", "init", "--config", dir, "--provider", "openrouter")
	if code != 0 {
		t.Fatalf("init: exit = %d\n%s", code, errOut)
	}
	a, err := app.Static(dir)
	if err != nil {
		t.Fatal(err)
	}
	if a.DataDir != data || a.Secrets.Path() != filepath.Join(sec, config.FileAuth) {
		t.Errorf("data %q, secrets %q: want the empty config's", a.DataDir, a.Secrets.Path())
	}
	if v, _ := a.Secrets.Get(secrets.Providers, "openrouter", "api_key"); v != "sk" {
		t.Errorf("key = %q", v)
	}
	if fi, err := os.Stat(cfg); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("config.toml mode = %v, %v; want the empty one's", fi.Mode(), err)
	}
	if pr := ownerAt(t, data); pr.Role != store.RoleOwner {
		t.Errorf("principal = %+v", pr)
	}

	// The second init says so before asking anything.
	atTerminal(t)
	code, _, errOut = runCLI(t, "", "init", "--config", dir)
	if code != 1 || !strings.Contains(errOut, "already initialised") || strings.Contains(errOut, "Which provider?") {
		t.Fatalf("second init: exit = %d, stderr %q", code, errOut)
	}
}

// Any other config is kept, and the provider flags say they are ignored.
func TestInitKeepsAConfigWithAProvider(t *testing.T) {
	dir := t.TempDir()
	e := &daemonEnv{dir: dir, socket: shortSocket(t), model: testmodel.New(t)}
	writeDaemonConfig(t, e, "", "")
	before, err := os.ReadFile(filepath.Join(dir, config.FileConfig))
	if err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runCLI(t, "sk", "init", "--config", dir, "--provider", "openrouter")
	if code != 0 {
		t.Fatalf("init: exit = %d\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "are ignored") {
		t.Errorf("stderr = %q, want the flags called ignored", errOut)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, config.FileConfig)); string(after) != string(before) {
		t.Errorf("config.toml changed:\n%s", after)
	}
}

func TestTOMLString(t *testing.T) {
	for in, want := range map[string]string{`a`: `"a"`, `a"b\c`: `"a\"b\\c"`, "x\ny": `"x\u000Ay"`, "ñ": `"ñ"`} {
		if got := tomlString(in); got != want {
			t.Errorf("tomlString(%q) = %s, want %s", in, got, want)
		}
	}
}

// `holaclaw config env` lists the keys the environment may set and where each
// value came from; `check` names the ones the environment set.
func TestConfigSources(t *testing.T) {
	dir, code, _, errOut := initIn(t, "", "--provider", "openai-codex")
	if code != 0 {
		t.Fatalf("init: exit = %d\n%s", code, errOut)
	}
	t.Setenv("HOLACLAW_LISTEN", "127.0.0.1:8484")
	code, out, errOut := runCLI(t, "", "config", "env", "--config", dir)
	if code != 0 {
		t.Fatalf("config: exit = %d\n%s", code, errOut)
	}
	for _, w := range []string{`server.listen                    "127.0.0.1:8484"  $HOLACLAW_LISTEN`, "browser.enabled                  false             default"} {
		if !strings.Contains(out, w) {
			t.Errorf("config output lacks %q:\n%s", w, out)
		}
	}
	_, out, _ = runCLI(t, "", "check", "--offline", "--config", dir)
	if !strings.Contains(out, `env: server.listen = "127.0.0.1:8484" from $HOLACLAW_LISTEN`) || strings.Contains(out, "browser.enabled") {
		t.Errorf("check output:\n%s", out)
	}
}
