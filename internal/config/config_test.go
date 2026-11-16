package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/provider"
)

// dir returns a fixture config directory.
func dir(name string) string { return filepath.Join("..", "..", "testdata", "config", name) }

// codes loads a fixture expected to fail and returns the problem set.
func codes(t *testing.T, name string) (Problems, *Config) {
	t.Helper()
	cfg, err := Load(dir(name))
	if err == nil {
		t.Fatalf("%s: expected problems, got none", name)
	}
	var ps Problems
	if !errors.As(err, &ps) {
		t.Fatalf("%s: error is %T, want Problems", name, err)
	}
	return ps, cfg
}

// has reports whether the problem list holds a code, and returns that problem.
func has(ps Problems, code string) (Problem, bool) {
	for _, p := range ps {
		if p.Code == code {
			return p, true
		}
	}
	return Problem{}, false
}

func codeSet(ps Problems) map[string]int {
	out := map[string]int{}
	for _, p := range ps {
		out[p.Code]++
	}
	return out
}

func TestLoadMinimal(t *testing.T) {
	cfg, err := Load(dir("minimal"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Version != CurrentVersion {
		t.Errorf("version = %d, want %d", cfg.Version, CurrentVersion)
	}
	if got := cfg.Models["gemma"].Context; got != 8192 {
		t.Errorf("context = %d, want 8192", got)
	}
	if len(cfg.Channels) != 0 {
		t.Errorf("channels = %v, want none when channels.toml is absent", cfg.Channels)
	}
	// Defaults applied where the file is silent.
	if got := cfg.Providers["ollama"].Auth; got != "none" {
		t.Errorf("auth default = %q, want \"none\"", got)
	}
	if got := cfg.Retention.EventsDays; got != 30 {
		t.Errorf("events_days default = %d, want 30", got)
	}
	a := cfg.Agents["main"]
	if a.Policy.Read != "allow" || a.Policy.Write != "allow" || a.Policy.External != "allow" || a.Policy.Exec != "deny" {
		t.Errorf("policy defaults = %+v", a.Policy.Classes)
	}
	if a.Policy.Guest != (Classes{Read: "allow", Write: "deny", External: "deny", Exec: "deny"}) {
		t.Errorf("guest defaults = %+v", a.Policy.Guest)
	}
	if a.Policy.Task != a.Policy.Classes {
		t.Errorf("task policy = %+v, want the agent policy", a.Policy.Task)
	}
	if a.Limits.MaxSteps != 0 || a.Limits.ReplyReserve != 0 {
		t.Errorf("limits = %+v, want zero so the library default applies", a.Limits)
	}
	if a.Policy.GuardMode != "recommend" || a.Policy.GuardThreshold != 0.7 {
		t.Errorf("guard defaults = %q / %v", a.Policy.GuardMode, a.Policy.GuardThreshold)
	}
	// images_per_day is the one limit with a non-zero default: 0 is a real
	// value (the tool refuses every call), so absence cannot mean 0.
	if a.Limits.ImagesPerDay != 20 {
		t.Errorf("images_per_day default = %d, want 20", a.Limits.ImagesPerDay)
	}
	if !strings.Contains(a.Prompt, "Hola") {
		t.Errorf("prompt.md not loaded: %q", a.Prompt)
	}
	if cfg.Browser.Enabled {
		t.Errorf("browser = %+v, want Enabled false when [browser] is absent", cfg.Browser)
	}
	if a.Models.Decide != "" || len(a.Browser.Allow) != 0 {
		t.Errorf("decide role / browser allow should be zero by default: %q / %v", a.Models.Decide, a.Browser.Allow)
	}
}

func TestLoadFull(t *testing.T) {
	cfg, err := Load(dir("full"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Server.Listen; got != "127.0.0.1:8899" {
		t.Errorf("listen = %q", got)
	}
	if got := cfg.Server.SecretsDir; got != "/etc/holaclaw-secrets" {
		t.Errorf("secrets_dir = %q", got)
	}
	m := cfg.Models["gemma"]
	if m.Timeout != 120*time.Second {
		t.Errorf("timeout = %v, want 2m", m.Timeout)
	}
	if m.SchemaProfile != "llamacpp" || m.Reasoning != "none" || !m.Tools || len(m.ThinkingTags) != 2 {
		t.Errorf("model profile = %+v", m)
	}
	ch := cfg.Channels["slack-ops"]
	if ch.Access != (Access{DM: "open", Rooms: "open", Reply: "always", ReplyTo: "always"}) {
		t.Errorf("access = %+v", ch.Access)
	}
	if got := cfg.Channels["tg-main"].Access.ReplyTo; got != "rooms" {
		t.Errorf("reply_to default = %q, want rooms", got)
	}
	a := cfg.Agents["main"]
	if a.Models.Embed != "embed" || a.Models.Fallback != "gpt" {
		t.Errorf("roles = %+v", a.Models)
	}
	if a.Policy.Tools["files.write"] != "ask" || !a.Policy.TaintEscalates || len(a.Policy.ExecAllow) != 2 {
		t.Errorf("policy = %+v", a.Policy)
	}
	if a.Policy.Task.External != "ask" {
		t.Errorf("task policy = %+v", a.Policy.Task)
	}
	if got := a.Policy.Decision("exec"); got != "ask" {
		t.Errorf("Decision(exec) = %q", got)
	}

	// Converters.
	def := cfg.Definition("main")
	// The fixture has a [browser] table, so the agent gets browser.do too.
	if def.Name != "main" || def.MaxSteps != 10 || def.ReplyReserve != 1024 || len(def.Tools) != 4 || def.Tools[3] != "browser.do" {
		t.Errorf("definition = %+v", def)
	}
	if def.Core != "" {
		t.Errorf("Core = %q, want empty: the core fragment belongs to the agent package", def.Core)
	}
	if !strings.Contains(def.Persona, "Be brief") {
		t.Errorf("persona = %q", def.Persona)
	}
	if got := cfg.Definition("nope"); got.Name != "" {
		t.Errorf("unknown agent = %+v, want zero Definition", got)
	}
	// Model APIs, their roles and the guard knobs (M6 / M7).
	if got := cfg.Models["gemma"].API; got != "chat" {
		t.Errorf("gemma api = %q, want \"chat\" filled in for a silent model", got)
	}
	jev := cfg.Models["jev"]
	if jev.API != "decisions" || jev.URL != "https://openrouter.ai/api/alpha/decisions" || jev.Context != 0 {
		t.Errorf("decisions model = %+v", jev)
	}
	if got := cfg.Models["muse"].API; got != "images" {
		t.Errorf("muse api = %q", got)
	}
	if a.Models.Guard != "jev" || a.Models.Image != "muse" {
		t.Errorf("roles = %+v", a.Models)
	}
	if a.Policy.GuardMode != "auto" || a.Policy.GuardThreshold != 0.9 {
		t.Errorf("guard knobs = %q / %v", a.Policy.GuardMode, a.Policy.GuardThreshold)
	}
	if a.Limits.ImagesPerDay != 5 {
		t.Errorf("images_per_day = %d, want 5", a.Limits.ImagesPerDay)
	}
	if a.Models.Decide != "jev" {
		t.Errorf("decide role = %q, want %q", a.Models.Decide, "jev")
	}
	if len(a.Browser.Allow) != 2 || a.Browser.Allow[0] != "example.com" {
		t.Errorf("browser allow = %v", a.Browser.Allow)
	}
	if len(a.Skills.Disabled) != 1 || a.Skills.Disabled[0] != "release" {
		t.Errorf("skills.disabled = %v", a.Skills.Disabled)
	}
	if !cfg.Browser.Enabled || cfg.Browser.Socket != "/run/holaclaw-browser/browser.sock" ||
		cfg.Browser.Binary != "/usr/bin/chromium" || !cfg.Browser.Headless {
		t.Errorf("browser = %+v", cfg.Browser)
	}

	pm, err := cfg.Model("gemma")
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	if pm.ID != "gemma4:e2b" || pm.Provider != "ollama" || pm.Timeout != 120*time.Second {
		t.Errorf("provider.Model = %+v", pm)
	}
	if pm.Reasoning != provider.ReasoningNone {
		t.Errorf("provider.Model reasoning = %q, want none", pm.Reasoning)
	}
	if pm.API != provider.APIChat || pm.URL != "" {
		t.Errorf("provider.Model api = %q, url = %q", pm.API, pm.URL)
	}
	pj, err := cfg.Model("jev")
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	if pj.API != provider.APIDecisions || pj.URL != jev.URL {
		t.Errorf("provider.Model = %+v", pj)
	}
	if _, err := cfg.Model("nope"); err == nil {
		t.Error("Model(nope) = nil error, want one")
	}
}

func TestUnknownKeyWithSuggestion(t *testing.T) {
	ps, _ := codes(t, "bad-unknown-key")
	p, ok := has(ps, CodeUnknownKey)
	if !ok {
		t.Fatalf("no %s in %v", CodeUnknownKey, ps.Codes())
	}
	if p.Key != "server.sockett" {
		t.Errorf("key = %q", p.Key)
	}
	if !strings.Contains(p.Hint, `"socket"`) {
		t.Errorf("hint = %q, want a did-you-mean for socket", p.Hint)
	}
}

func TestWrongFile(t *testing.T) {
	ps, _ := codes(t, "bad-wrong-file")
	var seen []string
	for _, p := range ps {
		if p.Code != CodeWrongFile {
			t.Errorf("unexpected %s at %s: %s", p.Code, p.File, p.Message)
			continue
		}
		seen = append(seen, p.File+":"+p.Key+":"+p.Message)
	}
	if len(seen) != 2 {
		t.Fatalf("wrong-file problems = %v", seen)
	}
	if !strings.Contains(seen[0], FileAgent) || !strings.Contains(seen[0], FileConfig) {
		t.Errorf("agent.toml problem should name config.toml: %q", seen[0])
	}
	if !strings.Contains(seen[1], FileChannels) {
		t.Errorf("config.toml problem should name channels.toml: %q", seen[1])
	}
}

func TestTypesEnumsAndDurations(t *testing.T) {
	ps, _ := codes(t, "bad-types")
	want := map[string]int{CodeWrongType: 2, CodeBadEnum: 2, CodeBadValue: 1}
	got := codeSet(ps)
	for c, n := range want {
		if got[c] != n {
			t.Errorf("%s count = %d, want %d (all: %v)", c, got[c], n, ps.Codes())
		}
	}
	hints := map[string]string{}
	for _, p := range ps {
		if p.Code == CodeBadEnum {
			hints[p.Key] = p.Hint
		}
	}
	if h := hints["providers.ollama.auth"]; !strings.Contains(h, `"subscription"`) {
		t.Errorf("auth enum hint = %q, want the allowed values", h)
	}
	if h := hints["models.gemma.reasoning"]; !strings.Contains(h, `"none"`) || !strings.Contains(h, `"xhigh"`) {
		t.Errorf("reasoning enum hint = %q, want the allowed values", h)
	}
	if p, _ := has(ps, CodeBadValue); p.Key != "models.gemma.timeout" {
		t.Errorf("bad duration reported at %q", p.Key)
	}
}

func TestMissingRequired(t *testing.T) {
	ps, _ := codes(t, "bad-missing-required")
	if got := codeSet(ps); got[CodeMissingRequired] != 3 {
		t.Fatalf("missing-required count = %d, want 3 (all: %v)", got[CodeMissingRequired], ps.Codes())
	}
}

func TestDanglingReferences(t *testing.T) {
	ps, _ := codes(t, "bad-dangling-ref")
	var keys []string
	for _, p := range ps {
		if p.Code != CodeDanglingRef {
			t.Errorf("unexpected %s: %s", p.Code, p.Message)
			continue
		}
		keys = append(keys, p.File+":"+p.Key)
	}
	want := []string{"agents/main/agent.toml:models.chat", "channels.toml:channels.tg.agent", "config.toml:models.gemma.provider"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("dangling refs = %v, want %v", keys, want)
	}
	if p, _ := has(ps, CodeDanglingRef); !strings.Contains(p.Hint, `"gemma"`) {
		t.Errorf("hint = %q, want a did-you-mean for gemma", p.Hint)
	}
}

func TestVersionProblems(t *testing.T) {
	ps, _ := codes(t, "bad-version")
	if _, ok := has(ps, CodeBadVersion); !ok {
		t.Fatalf("no %s in %v", CodeBadVersion, ps.Codes())
	}
	for _, p := range ps {
		if p.Code != CodeBadVersion {
			t.Errorf("unexpected %s: %s", p.Code, p.Message)
		}
	}

	ps, _ = codes(t, "bad-version-mismatch")
	p, ok := has(ps, CodeVersionMismatch)
	if !ok {
		t.Fatalf("no %s in %v", CodeVersionMismatch, ps.Codes())
	}
	if p.File != "agents/main/agent.toml" {
		t.Errorf("mismatch reported on %q", p.File)
	}
}

func TestMissingVersion(t *testing.T) {
	d := t.TempDir()
	copyDir(t, dir("minimal"), d)
	write(t, filepath.Join(d, FileConfig), "[models.x]\nprovider = \"p\"\nid = \"i\"\ncontext = 1\n[providers.p]\ntype = \"codex\"\n")
	cfg, err := Load(d)
	if err == nil {
		t.Fatal("expected problems")
	}
	var ps Problems
	_ = errors.As(err, &ps)
	if p, ok := has(ps, CodeBadVersion); !ok || !strings.Contains(p.Message, "missing") {
		t.Fatalf("problems = %v", ps)
	}
	if cfg == nil {
		t.Fatal("Load returned no config; callers report against a partial one")
	}
}

func TestLiteralSecret(t *testing.T) {
	ps, _ := codes(t, "bad-literal-secret")
	p, ok := has(ps, CodeLiteralSecret)
	if !ok {
		t.Fatalf("no %s in %v", CodeLiteralSecret, ps.Codes())
	}
	if p.Key != "providers.openrouter.api_key" {
		t.Errorf("key = %q", p.Key)
	}
	if !strings.Contains(p.Hint, FileAuth) {
		t.Errorf("hint = %q, want it to point at %s", p.Hint, FileAuth)
	}
	if strings.Contains(p.Message, "sk-or") {
		t.Errorf("message leaks the secret: %q", p.Message)
	}
}

func TestMalformedTOML(t *testing.T) {
	ps, _ := codes(t, "bad-toml")
	if _, ok := has(ps, CodeBadValue); !ok {
		t.Fatalf("no %s in %v", CodeBadValue, ps.Codes())
	}
}

func TestMissingFiles(t *testing.T) {
	ps, _ := codes(t, "bad-no-agents")
	if p, ok := has(ps, CodeMissingFile); !ok || p.File != "agents" {
		t.Fatalf("problems = %v", ps)
	}
	ps, _ = codes(t, "bad-no-prompt")
	if p, ok := has(ps, CodeMissingFile); !ok || p.File != "agents/main/prompt.md" {
		t.Fatalf("problems = %v", ps)
	}

	// A directory without config.toml at all.
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("empty directory loaded without problems")
	}
}

func TestValidateUnknownTool(t *testing.T) {
	cfg, err := Load(dir("full"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := Validate(cfg, []string{"memory.save", "memory.search", "files.read", "files.write", "fetch", "browser.do"}); err != nil {
		t.Fatalf("validate: %v", err)
	}
	err = Validate(cfg, []string{"memory.save", "files.read", "fetc"})
	var ps Problems
	if !errors.As(err, &ps) {
		t.Fatalf("error = %v, want Problems", err)
	}
	p, ok := has(ps, CodeUnknownTool)
	if !ok {
		t.Fatalf("no %s in %v", CodeUnknownTool, ps.Codes())
	}
	if !strings.Contains(p.Hint, `"fetc"`) {
		t.Errorf("hint = %q, want a did-you-mean", p.Hint)
	}
	// Both the tools list and the per-tool policy overrides are checked.
	var keys []string
	for _, q := range ps {
		keys = append(keys, q.Key)
	}
	if !strings.Contains(strings.Join(keys, ","), "policy.tools.files.write") {
		t.Errorf("per-tool overrides not checked: %v", keys)
	}
}

// TestModelAPIKeys covers the per-model half of M6 / M7: `url` is required and
// must be absolute for a decisions model, and `context` is required only for a
// chat model.
func TestModelAPIKeys(t *testing.T) {
	ps, _ := codes(t, "bad-model-api")
	want := []struct{ code, key, msg string }{
		{CodeBadValue, "models.jev-relative.url", "absolute http(s) URL"},
		{CodeMissingRequired, "models.jev.url", "url"},
	}
	if len(ps) != len(want) {
		t.Fatalf("problems = %v, want exactly %d", ps, len(want))
	}
	for i, w := range want {
		got := ps[i]
		if got.Code != w.code || got.Key != w.key || !strings.Contains(got.Message, w.msg) {
			t.Errorf("problem %d = %s, want %s at %q mentioning %q", i, got, w.code, w.key, w.msg)
		}
	}
}

// TestContextRequiredForChatOnly pins the condition from both sides.
func TestContextRequiredForChatOnly(t *testing.T) {
	for _, tc := range []struct {
		name    string
		model   string
		problem bool
	}{
		{"absent api", "provider = \"p\"\nid = \"i\"\n", true},
		{"explicit chat", "provider = \"p\"\nid = \"i\"\napi = \"chat\"\n", true},
		{"decisions", "provider = \"p\"\nid = \"i\"\napi = \"decisions\"\nurl = \"https://x.dev/d\"\n", false},
		{"images", "provider = \"p\"\nid = \"i\"\napi = \"images\"\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			copyDir(t, dir("minimal"), d)
			write(t, filepath.Join(d, FileConfig),
				"version = 1\n[providers.p]\ntype = \"codex\"\n[models.gemma]\ncontext = 8192\nprovider = \"p\"\nid = \"i\"\n[models.x]\n"+tc.model)
			_, err := Load(d)
			var ps Problems
			_ = errors.As(err, &ps)
			p, found := has(ps, CodeMissingRequired)
			if found != tc.problem {
				t.Fatalf("problems = %v, want missing-required: %v", ps, tc.problem)
			}
			if found && p.Key != "models.x.context" {
				t.Errorf("missing-required at %q", p.Key)
			}
		})
	}
}

// TestRoleAPIMismatch: a role only accepts the wire APIs it can use (docs/models.md).
func TestRoleAPIMismatch(t *testing.T) {
	ps, _ := codes(t, "bad-role-api")
	want := map[string]string{
		"models.guard":     "muse",
		"models.image":     "gemma",
		"models.summarize": "jev",
		"models.decide":    "gemma",
	}
	if len(ps) != len(want) {
		t.Fatalf("problems = %v, want exactly %d", ps, len(want))
	}
	for _, p := range ps {
		model, ok := want[p.Key]
		if !ok {
			t.Errorf("unexpected problem %s", p)
			continue
		}
		if p.Code != CodeBadEnum {
			t.Errorf("%s: code = %s, want %s", p.Key, p.Code, CodeBadEnum)
		}
		// The message names the role, the model and the api it speaks.
		role := strings.TrimPrefix(p.Key, "models.")
		for _, s := range []string{role, model} {
			if !strings.Contains(p.Message, s) {
				t.Errorf("%s: message %q does not name %q", p.Key, p.Message, s)
			}
		}
		if !strings.Contains(p.Hint, "api") {
			t.Errorf("%s: hint = %q", p.Key, p.Hint)
		}
	}
	// A decision model in the guard role is the recommended setup, not a problem.
	if _, err := Load(dir("full")); err != nil {
		t.Fatalf("full fixture: %v", err)
	}
}

// TestGuardModeAndLimits: auto with nothing to decide with is refused, a score
// outside (0, 1] is a bad value, and a negative image budget is too.
func TestGuardModeAndLimits(t *testing.T) {
	ps, _ := codes(t, "bad-guard-mode")
	want := []struct{ code, key string }{
		{CodeBadValue, "limits.images_per_day"},
		{CodeMissingRequired, "models.guard"},
		{CodeBadValue, "policy.guard_threshold"},
	}
	if len(ps) != len(want) {
		t.Fatalf("problems = %v, want exactly %d", ps, len(want))
	}
	for i, w := range want {
		if ps[i].Code != w.code || ps[i].Key != w.key {
			t.Errorf("problem %d = %s, want %s at %q", i, ps[i], w.code, w.key)
		}
	}
	if p, _ := has(ps, CodeMissingRequired); !strings.Contains(p.Message, "auto") {
		t.Errorf("guard-mode message = %q, want it to name the mode", p.Message)
	}
}

// TestGuardThresholdValues: a TOML float or the integer 1, nothing else.
func TestGuardThresholdValues(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  float64 // 0 = expect a problem
	}{
		{"0.5", 0.5},
		{"1", 1},
		{"1.0", 1},
		{"0", 0},
		{"-0.2", 0},
		{"2", 0},
		{`"0.5"`, 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			d := t.TempDir()
			copyDir(t, dir("minimal"), d)
			write(t, filepath.Join(d, "agents", "main", FileAgent),
				"version = 1\ntools = [\"memory\"]\n[models]\nchat = \"gemma\"\n[policy]\nguard_threshold = "+tc.value+"\n")
			cfg, err := Load(d)
			if tc.want == 0 {
				if err == nil {
					t.Fatalf("guard_threshold = %s loaded without a problem", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := cfg.Agents["main"].Policy.GuardThreshold; got != tc.want {
				t.Errorf("guard_threshold = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTaintThresholdValues: the score at or above which untrusted content
// escalates policy (docs/security.md). Same shape as guard_threshold — a float in (0, 1],
// unset means the default 0.5.
func TestTaintThresholdValues(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  float64 // 0 = expect a problem
	}{
		{"", 0.5}, // unset: the default
		{"0.3", 0.3},
		{"1", 1},
		{"0", 0},
		{"-0.1", 0},
		{"1.5", 0},
		{`"0.3"`, 0},
	} {
		t.Run("threshold="+tc.value, func(t *testing.T) {
			d := t.TempDir()
			copyDir(t, dir("minimal"), d)
			body := "version = 1\ntools = [\"memory\"]\n[models]\nchat = \"gemma\"\n[policy]\n"
			if tc.value != "" {
				body += "taint_threshold = " + tc.value + "\n"
			}
			write(t, filepath.Join(d, "agents", "main", FileAgent), body)
			cfg, err := Load(d)
			if tc.want == 0 {
				if err == nil {
					t.Fatalf("taint_threshold = %s loaded without a problem", tc.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := cfg.Agents["main"].Policy.TaintThreshold; got != tc.want {
				t.Errorf("taint_threshold = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSecretsDirMustBeAbsolute: unlike data_dir, never relative to the config
// directory.
func TestSecretsDirMustBeAbsolute(t *testing.T) {
	d := t.TempDir()
	copyDir(t, dir("minimal"), d)
	body, err := os.ReadFile(filepath.Join(d, FileConfig))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(d, FileConfig), string(body)+"\n[server]\nsecrets_dir = \"secrets\"\n")
	_, err = Load(d)
	var ps Problems
	if !errors.As(err, &ps) || len(ps) != 1 || ps[0].Code != CodeBadValue || ps[0].Key != "server.secrets_dir" {
		t.Fatalf("err = %v, want one %s at server.secrets_dir", err, CodeBadValue)
	}
	if !strings.Contains(ps[0].Hint, "/etc/holaclaw-secrets") {
		t.Errorf("hint = %q, want the default as the example", ps[0].Hint)
	}
}

// TestImagesPerDayZero: 0 is a configured refusal, not "use the default".
func TestImagesPerDayZero(t *testing.T) {
	d := t.TempDir()
	copyDir(t, dir("minimal"), d)
	write(t, filepath.Join(d, "agents", "main", FileAgent),
		"version = 1\ntools = [\"memory\"]\n[models]\nchat = \"gemma\"\n[limits]\nimages_per_day = 0\n")
	cfg, err := Load(d)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Agents["main"].Limits.ImagesPerDay; got != 0 {
		t.Errorf("images_per_day = %d, want the configured 0", got)
	}
}

// TestBackups: absent is one backup a day and 7 kept, keep = 0 is a
// configured "take none", and a negative count or a bad interval is a problem.
func TestBackups(t *testing.T) {
	cfg, err := Load(dir("minimal"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Backups.Keep; got != 7 {
		t.Errorf("backups.keep default = %d, want 7", got)
	}
	if got := cfg.Backups.Every; got != 24*time.Hour {
		t.Errorf("backups.every default = %v, want 24h", got)
	}

	base, err := os.ReadFile(filepath.Join(dir("minimal"), FileConfig))
	if err != nil {
		t.Fatal(err)
	}
	for value, want := range map[string]int{"0": 0, "3": 3} {
		d := t.TempDir()
		copyDir(t, dir("minimal"), d)
		write(t, filepath.Join(d, FileConfig), string(base)+"\n[backups]\nkeep = "+value+"\n")
		cfg, err := Load(d)
		if err != nil {
			t.Fatalf("keep = %s: %v", value, err)
		}
		if got := cfg.Backups.Keep; got != want {
			t.Errorf("backups.keep = %d, want the configured %d", got, want)
		}
	}

	d := t.TempDir()
	copyDir(t, dir("minimal"), d)
	write(t, filepath.Join(d, FileConfig), string(base)+"\n[backups]\nevery = \"6h\"\nkeep = 2\n")
	if cfg, err = Load(d); err != nil || cfg.Backups.Every != 6*time.Hour || cfg.Backups.Keep != 2 {
		t.Errorf("backups = %+v, %v, want every 6h and keep 2", cfg, err)
	}
	for _, bad := range []string{"keep = -1", "every = \"daily\"", "every = 24"} {
		d := t.TempDir()
		copyDir(t, dir("minimal"), d)
		write(t, filepath.Join(d, FileConfig), string(base)+"\n[backups]\n"+bad+"\n")
		if _, err := Load(d); err == nil {
			t.Errorf("[backups] %s loaded", bad)
		}
	}
}

// TestBrowserConfig: socket and binary must be absolute paths (docs/browser.md); socket has a default.
func TestBrowserConfig(t *testing.T) {
	ps, cfg := codes(t, "bad-browser-config")
	want := []struct{ code, key string }{
		{CodeBadValue, "browser.binary"},
	}
	if cfg.Browser.Socket != DefaultBrowserSocket || !cfg.Browser.Enabled {
		t.Errorf("browser = %+v, want it on with the default socket", cfg.Browser)
	}
	if len(ps) != len(want) {
		t.Fatalf("problems = %v, want exactly %d", ps, len(want))
	}
	for i, w := range want {
		if ps[i].Code != w.code || ps[i].Key != w.key {
			t.Errorf("problem %d = %s, want %s at %q", i, ps[i], w.code, w.key)
		}
	}
}

// TestBrowserSocketRelative pins the bad-value message for a relative socket.
func TestBrowserSocketRelative(t *testing.T) {
	d := t.TempDir()
	copyDir(t, dir("minimal"), d)
	write(t, filepath.Join(d, FileConfig),
		"version = 1\n[providers.p]\ntype = \"codex\"\n[models.gemma]\nprovider = \"p\"\nid = \"i\"\ncontext = 1\n"+
			"[browser]\nsocket = \"run/browser.sock\"\n")
	_, err := Load(d)
	var ps Problems
	if !errors.As(err, &ps) {
		t.Fatalf("error = %v, want Problems", err)
	}
	p, ok := has(ps, CodeBadValue)
	if !ok || p.Key != "browser.socket" || !strings.Contains(p.Message, "absolute path") {
		t.Fatalf("problems = %v", ps)
	}
}

// TestBrowserAllowHosts: every kind of invalid entry v1 rejects, one bad
// value per index so the message names the entry.
func TestBrowserAllowHosts(t *testing.T) {
	ps, _ := codes(t, "bad-browser-allow")
	if got := codeSet(ps); got[CodeBadValue] != 6 {
		t.Fatalf("bad-value count = %d, want 6 (all: %v)", got[CodeBadValue], ps.Codes())
	}
	for i := 0; i < 6; i++ {
		key := fmt.Sprintf("browser.allow[%d]", i)
		if p, ok := has(ps, CodeBadValue); !ok || !containsKey(ps, key) {
			t.Errorf("no problem at %q: %v", key, p)
		}
	}
}

// TestSkillsDisabledNames: an entry that is not a skill name is refused.
func TestSkillsDisabledNames(t *testing.T) {
	ps, cfg := codes(t, "bad-skills-disabled")
	if got := codeSet(ps); got[CodeBadValue] != 2 || len(ps) != 2 {
		t.Fatalf("problems = %v", ps)
	}
	for _, key := range []string{"skills.disabled[1]", "skills.disabled[2]"} {
		if !containsKey(ps, key) {
			t.Errorf("no problem at %q: %v", key, ps)
		}
	}
	if d := cfg.Agents["main"].Skills.Disabled; len(d) != 3 {
		t.Errorf("disabled = %v", d)
	}
}

func containsKey(ps Problems, key string) bool {
	for _, p := range ps {
		if p.Key == key {
			return true
		}
	}
	return false
}

// TestValidHost pins the accepted and rejected shapes directly.
func TestValidHost(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"example.com", true},
		{"sub.example.com", true},
		{"a-b.example.com", true},
		{"https://example.com", false},
		{"example.com/path", false},
		{"example.com:8080", false},
		{"EXAMPLE.com", false},
		{"exa mple.com", false},
		{"192.168.0.1", false},
		{"::1", false},
		{"", false},
		{"-example.com", false},
		{"example-.com", false},
		{"*.example.com", false},
	} {
		if got := validHost(tc.host); got != tc.want {
			t.Errorf("validHost(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// TestBrowserToolWithoutTable: the cross-file rule of docs/browser.md — an agent that
// lists the browser tool needs config.toml to have a [browser] table. It
// fires from the agent's own `tools` list, without the tool being registered.
func TestBrowserToolWithoutTable(t *testing.T) {
	ps, _ := codes(t, "bad-browser-tool")
	p, ok := has(ps, CodeMissingRequired)
	if !ok {
		t.Fatalf("no %s in %v", CodeMissingRequired, ps.Codes())
	}
	if p.File != "agents/main/agent.toml" {
		t.Errorf("file = %q", p.File)
	}
	for _, want := range []string{"main", "browser", "[browser]", "$HOLACLAW_BROWSER_ENABLED"} {
		if !strings.Contains(p.Message+p.Hint, want) {
			t.Errorf("message/hint = %q / %q, want it to mention %q", p.Message, p.Hint, want)
		}
	}

	// The full fixture lists no browser tool and has a [browser] table: no
	// problem. Once "browser.do" lands in the registry, Validate accepts it
	// independently of this rule (it never depends on the registry).
	cfg, err := Load(dir("full"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := Validate(cfg, []string{"memory.save", "memory.search", "files.read", "files.write", "fetch", "browser.do"}); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestResolveEnv(t *testing.T) {
	t.Setenv("HOLACLAW_TEST_KEY", "s3cret")
	got, err := ResolveEnv("env:HOLACLAW_TEST_KEY")
	if err != nil || got != "s3cret" {
		t.Fatalf("ResolveEnv = %q, %v", got, err)
	}
	if got, err := ResolveEnv("plain"); err != nil || got != "plain" {
		t.Errorf("plain value = %q, %v", got, err)
	}
	if _, err := ResolveEnv("env:HOLACLAW_TEST_UNSET"); err == nil {
		t.Error("unset variable returned no error")
	}
	if _, err := ResolveEnv("env:"); err == nil {
		t.Error("empty reference returned no error")
	}
}

func TestProblemsFormatting(t *testing.T) {
	ps := Problems{{Code: CodeUnknownKey, Severity: SeverityError, File: FileConfig, Key: "a.b", Message: "m", Hint: "h"}}
	s := ps.Error()
	for _, want := range []string{CodeUnknownKey, FileConfig, "a.b", "m", "h"} {
		if !strings.Contains(s, want) {
			t.Errorf("%q missing from %q", want, s)
		}
	}
	if !ps.HasErrors() {
		t.Error("HasErrors = false")
	}
	if (Problems{{Severity: SeverityWarning}}).HasErrors() {
		t.Error("a warning counts as an error")
	}
}

// --- schema -----------------------------------------------------------------

// KeyCount is tracked in CI (ARCHITECTURE.md §1): every new config key has to
// justify itself, so this number only moves with a deliberate change.
//
// 67 -> 66 (M11): browser.calibrate removed with the calibration log — the
// floors it was to fit are gone with the gates that had them, and browser.step
// events already carry the two scores that remain.
//
// 68 was reached with policy.taint_threshold (docs/security.md graded taint).
//
// 68 -> 69 (M12, 2026-09-24): skills.disabled, the one agent key skills add
// (docs/skills.md §3).
//
// 69 -> 70 (2026-10-06): server.secrets_dir, so auth.toml can live outside
// the config directory (default /etc/holaclaw-secrets).
//
// 70 -> 72 (2026-10-07): backups.every and backups.keep, how often the
// daemon takes an automatic backup and how many of them it keeps (default
// 24h and 7; keep = 0 takes none), issue #35.
//
// 72 -> 79 (2026-10-15): connectors.toml (version, connectors.<id>.url,
// .auth, .scopes, .client_id, .trusted) and connectors in agent.toml, the
// seven keys of docs/connectors.md §4, issue #67.
//
// 79 -> 80 (2026-10-19): channels.<name>.access.reply_to, whether an answer
// is a native reply to the message that asked (rooms | always | off,
// default rooms), issue #93.
//
// 80 -> 81 (2026-10-23): connectors.<id>.allow_insecure_transport, plain
// http to any host or an unverified certificate for one connector's MCP
// endpoint (docs/connectors.md §11), for a server on the operator's LAN.
//
// 81 -> 82 (2026-10-26): models.<name>.reasoning, the reasoning effort of
// every call to the model that sets none (none | low); the only way to turn
// a thinking model's reasoning off on Ollama, issue #127.
//
// 82 -> 83 (2026-10-30): server.allow_insecure_transport, an off-loopback
// listen over plain http, for a daemon behind a TLS-terminating proxy or on
// a trusted LAN; still refused without a token.
//
// 83 -> 85 (2026-11-04): browser.enabled in config.toml and in agent.toml,
// issue #146: the browser is turned on by a switch (or
// $HOLACLAW_BROWSER_ENABLED) and every agent gets browser.do unless it opts
// out, replacing the sed the container and install.sh ran on agent.toml.
//
// 85 -> 86 (2026-11-11): server.public_url, issue #154: the address devices
// reach the daemon at, which a pairing link carries; behind a container's
// port mapping or a TLS proxy the daemon cannot work it out from listen.
const wantKeyCount = 86

func TestKeyCountPinned(t *testing.T) {
	if got := KeyCount(); got != wantKeyCount {
		t.Fatalf("KeyCount = %d, want %d — CI tracks this number; changing it needs a justified new key", got, wantKeyCount)
	}
}

func TestSchemaIsValidJSONAndComplete(t *testing.T) {
	total := 0
	for _, file := range SchemaFiles() {
		b, err := Schema(file)
		if err != nil {
			t.Fatalf("schema %s: %v", file, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("schema %s is not valid JSON: %v", file, err)
		}
		if doc["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
			t.Errorf("schema %s: $schema = %v", file, doc["$schema"])
		}
		if doc["additionalProperties"] != false {
			t.Errorf("schema %s: additionalProperties = %v, want false", file, doc["additionalProperties"])
		}
		fs, _ := schemaFor(file)
		total += countJSONLeaves(t, file, doc, fs.fields)
	}
	if total != KeyCount() {
		t.Fatalf("schema exports %d keys, KeyCount = %d", total, KeyCount())
	}
	if _, err := Schema("nope.toml"); err == nil {
		t.Error("Schema(nope.toml) = nil error")
	}
}

// countJSONLeaves walks the exported schema next to the description and counts
// the leaves, proving every described key reaches the JSON Schema.
func countJSONLeaves(t *testing.T, file string, doc map[string]any, fs []field) int {
	t.Helper()
	props, _ := doc["properties"].(map[string]any)
	n := 0
	for _, f := range fs {
		sub, ok := props[f.name].(map[string]any)
		if !ok {
			t.Fatalf("schema %s: key %q missing", file, f.name)
		}
		switch f.kind {
		case kTable:
			n += countJSONLeaves(t, file, sub, f.fields)
		case kMapTable:
			inner, ok := sub["additionalProperties"].(map[string]any)
			if !ok {
				t.Fatalf("schema %s: %q has no value schema", file, f.name)
			}
			n += countJSONLeaves(t, file, inner, f.fields)
		default:
			n++
		}
	}
	return n
}

func TestSuggest(t *testing.T) {
	cands := []string{"socket", "listen", "data_dir"}
	if got := suggest("sockett", cands); got != "socket" {
		t.Errorf("suggest(sockett) = %q", got)
	}
	if got := suggest("banana", cands); got != "" {
		t.Errorf("suggest(banana) = %q, want no suggestion", got)
	}
}

// --- helpers ----------------------------------------------------------------

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyDir(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// An agent name becomes a directory and a URL path segment: a closed alphabet.
func TestValidAgentName(t *testing.T) {
	for name, want := range map[string]bool{
		"main": true, "a": true, "ops-2": true, "x_y": true, "0day": true,
		"": false, "Main": false, "../x": false, "a/b": false, ".hidden": false, "-x": false,
		"a b": false, "ü": false, strings.Repeat("a", 64): false, strings.Repeat("a", 63): true,
	} {
		if got := ValidAgentName(name); got != want {
			t.Errorf("ValidAgentName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestLoadRejectsBadAgentName(t *testing.T) {
	dir := t.TempDir()
	copyDir(t, filepath.Join("..", "..", "testdata", "config", "minimal"), dir)
	if err := os.Rename(filepath.Join(dir, "agents", "main"), filepath.Join(dir, "agents", "Bad Name")); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "is not valid") {
		t.Fatalf("err = %v", err)
	}
}

// TestEnvOverrides: an EnvOverrides variable wins over config.toml, and
// Sources says where each value came from.
func TestEnvOverrides(t *testing.T) {
	base, err := os.ReadFile(filepath.Join(dir("minimal"), FileConfig))
	if err != nil {
		t.Fatal(err)
	}
	d := t.TempDir()
	copyDir(t, dir("minimal"), d)
	write(t, filepath.Join(d, FileConfig), string(base)+"\n[server]\nlisten = \"127.0.0.1:1\"\n")

	cfg, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"server.listen": FileConfig, "server.allow_insecure_transport": "default", "browser.enabled": "default"}
	for k, v := range want {
		if cfg.Sources[k] != v {
			t.Errorf("source of %s = %q, want %q", k, cfg.Sources[k], v)
		}
	}
	if cfg.Value("server.listen") != `"127.0.0.1:1"` || cfg.Value("browser.enabled") != "false" {
		t.Errorf("values = %s %s", cfg.Value("server.listen"), cfg.Value("browser.enabled"))
	}

	t.Setenv("HOLACLAW_LISTEN", "0.0.0.0:8484")
	t.Setenv("HOLACLAW_ALLOW_INSECURE_TRANSPORT", "true")
	t.Setenv("HOLACLAW_BROWSER_ENABLED", "true")
	t.Setenv("HOLACLAW_PUBLIC_URL", "https://claw.example/")
	if cfg, err = Load(d); err != nil {
		t.Fatal(err)
	}
	if cfg.Server.PublicURL != "https://claw.example" || cfg.Value("server.public_url") != `"https://claw.example"` {
		t.Errorf("public url = %q, want the env value without its trailing slash", cfg.Server.PublicURL)
	}
	if cfg.Server.Listen != "0.0.0.0:8484" || !cfg.Server.AllowInsecureTransport || !cfg.Browser.Enabled {
		t.Errorf("config = %+v %+v, want the env values", cfg.Server, cfg.Browser)
	}
	for _, o := range EnvOverrides {
		if cfg.Sources[o.Key] != "env" {
			t.Errorf("source of %s = %q, want env", o.Key, cfg.Sources[o.Key])
		}
	}
	if cfg.Browser.Socket != DefaultBrowserSocket {
		t.Errorf("socket = %q, want the default", cfg.Browser.Socket)
	}
	if got := cfg.Agents["main"].Tools; !slices.Equal(got, []string{"memory", "browser.do"}) {
		t.Errorf("tools = %v, want browser.do granted", got)
	}

	t.Setenv("HOLACLAW_BROWSER_ENABLED", "maybe")
	_, err = Load(d)
	var problems Problems
	if !errors.As(err, &problems) {
		t.Fatalf("a bad boolean loaded: %v", err)
	}
	if p, ok := has(problems, CodeBadValue); !ok || p.Key != "browser.enabled" || !strings.Contains(p.Message, "HOLACLAW_BROWSER_ENABLED") {
		t.Errorf("problems = %v", problems)
	}

	t.Setenv("HOLACLAW_BROWSER_ENABLED", "true")
	t.Setenv("HOLACLAW_PUBLIC_URL", "claw.example")
	_, err = Load(d)
	if !errors.As(err, &problems) {
		t.Fatalf("a public url that is no URL loaded: %v", err)
	}
	if p, ok := has(problems, CodeBadValue); !ok || p.Key != "server.public_url" || !strings.Contains(p.Message, "HOLACLAW_PUBLIC_URL") {
		t.Errorf("problems = %v", problems)
	}
}

// TestBrowserEnabled: browser.enabled switches a present [browser] table off,
// $HOLACLAW_BROWSER_ENABLED wins over it, and an agent opts out with its own
// browser.enabled.
func TestBrowserEnabled(t *testing.T) {
	base, err := os.ReadFile(filepath.Join(dir("minimal"), FileConfig))
	if err != nil {
		t.Fatal(err)
	}
	agentBase, err := os.ReadFile(filepath.Join(dir("minimal"), "agents", "main", FileAgent))
	if err != nil {
		t.Fatal(err)
	}
	load := func(t *testing.T, cfgExtra, agentExtra string) (*Config, error) {
		t.Helper()
		d := t.TempDir()
		copyDir(t, dir("minimal"), d)
		write(t, filepath.Join(d, FileConfig), string(base)+cfgExtra)
		write(t, filepath.Join(d, "agents", "main", FileAgent), string(agentBase)+agentExtra)
		return Load(d)
	}

	cfg, err := load(t, "\n[browser]\nenabled = false\n", "")
	if err != nil || cfg.Browser.Enabled || slices.Contains(cfg.Agents["main"].Tools, "browser.do") {
		t.Errorf("enabled = false: %+v %v %v", cfg.Browser, cfg.Agents["main"].Tools, err)
	}
	t.Run("env wins", func(t *testing.T) {
		t.Setenv("HOLACLAW_BROWSER_ENABLED", "false")
		cfg, err := load(t, "\n[browser]\n", "")
		if err != nil || cfg.Browser.Enabled || cfg.Sources["browser.enabled"] != "env" {
			t.Errorf("env false over a table: %+v %v", cfg.Browser, err)
		}
	})

	cfg, err = load(t, "\n[browser]\n", "\n[browser]\nenabled = false\n")
	if err != nil || !cfg.Browser.Enabled || slices.Contains(cfg.Agents["main"].Tools, "browser.do") {
		t.Errorf("agent opt-out: %+v %v %v", cfg.Browser, cfg.Agents["main"].Tools, err)
	}

	d := t.TempDir()
	copyDir(t, dir("minimal"), d)
	write(t, filepath.Join(d, FileConfig), string(base)+"\n[browser]\n")
	write(t, filepath.Join(d, "agents", "main", FileAgent),
		"version = 1\ntools = [\"memory\", \"browser.do\"]\n\n[models]\nchat = \"gemma\"\n\n[browser]\nenabled = false\n")
	_, err = Load(d)
	var ps Problems
	if !errors.As(err, &ps) {
		t.Fatalf("browser.do with an opt-out loaded: %v", err)
	}
	if p, ok := has(ps, CodeBadValue); !ok || p.File != "agents/main/agent.toml" {
		t.Errorf("problems = %v", ps)
	}
}
