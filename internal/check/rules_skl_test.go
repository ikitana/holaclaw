package check

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/config"
	"holaclaw/internal/provider"
	"holaclaw/internal/skill"
)

// sklNow is the install time of every SKL test origin.
var sklNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

// skillMD is a SKILL.md with the given frontmatter extra lines.
func skillMD(name, extra string) string {
	return "---\nname: " + name + "\ndescription: When testing " + name + ".\n" + extra + "---\n# " + name + "\n\nbody\n"
}

// requiresTools is the metadata block requiring tools.
func requiresTools(tools ...string) string {
	return "metadata:\n  holaclaw:\n    version: 1\n    requires:\n      tools: [" + strings.Join(tools, ", ") + "]\n"
}

// sklEnv is a config dir and data dir with the SKL rules switched on.
type sklEnv struct {
	cfgDir, dataDir string
	in              *Input
}

func newSklEnv(t *testing.T) *sklEnv {
	t.Helper()
	e := &sklEnv{cfgDir: t.TempDir(), dataDir: t.TempDir()}
	e.in = &Input{
		ConfigDir: e.cfgDir,
		Config:    minimalConfig(),
		DataDir:   e.dataDir,
		SkillRoots: func(agent string) skill.Roots {
			return skill.Roots{
				Bundled: skill.Bundled,
				Agent:   filepath.Join(e.cfgDir, "agents", agent, "skills"),
			}
		},
	}
	return e
}

// agentSkill writes agents/main/skills/<name>/SKILL.md and returns the folder.
func (e *sklEnv) agentSkill(t *testing.T, name, extra string) string {
	t.Helper()
	rel := filepath.Join("agents", "main", "skills", name, "SKILL.md")
	if err := writeFileErr(e.cfgDir, rel, skillMD(name, extra)); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(e.cfgDir, "agents", "main", "skills", name)
}

// writeOrigin records dir as installed from src at when, with its current hash.
func writeOrigin(t *testing.T, dir, src string, when time.Time) {
	t.Helper()
	h, err := skill.Hash(dir)
	if err != nil {
		t.Fatal(err)
	}
	body := "source = \"" + src + "\"\nhash = \"" + h + "\"\ninstalled = " + when.Format(time.RFC3339) + "\n"
	if err := writeFileErr(dir, skill.OriginFile, body); err != nil {
		t.Fatal(err)
	}
}

func runRule(r Rule, in *Input) []Finding { return r.Run(context.Background(), in) }

func TestSKL001(t *testing.T) {
	t.Run("fires and names the tool", func(t *testing.T) {
		e := newSklEnv(t)
		e.agentSkill(t, "release", requiresTools("memory", "exec"))
		fs := runRule(skl001Rule(), e.in)
		if len(fs) != 1 {
			t.Fatalf("findings = %+v, want 1", fs)
		}
		f := fs[0]
		want := "skill release (agent) requires tool exec, which agent main does not list: hidden"
		if f.ID != "SKL001" || f.Severity != SeverityWarning || f.Message != want {
			t.Errorf("finding = %+v", f)
		}
		if f.File != filepath.Join("agents", "main", "skills", "release") || f.Key != "metadata.holaclaw.requires.tools" {
			t.Errorf("file/key = %q %q", f.File, f.Key)
		}
		if f.Fix != `add "exec" to tools in agents/main/agent.toml, or add "release" to skills.disabled` {
			t.Errorf("fix = %q", f.Fix)
		}
	})
	t.Run("one finding per skill", func(t *testing.T) {
		e := newSklEnv(t)
		e.agentSkill(t, "release", requiresTools("exec", "files"))
		if fs := runRule(skl001Rule(), e.in); len(fs) != 1 || !strings.Contains(fs[0].Message, "tool exec,") {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("tool listed", func(t *testing.T) {
		e := newSklEnv(t)
		e.agentSkill(t, "notes", requiresTools("memory"))
		if fs := runRule(skl001Rule(), e.in); len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("tool from a group and the implicit ones", func(t *testing.T) {
		e := newSklEnv(t)
		e.agentSkill(t, "notes", requiresTools("memory.save", "skill", "image.generate"))
		a := e.in.Config.Agents["main"]
		a.Models.Image = "pic"
		e.in.Config.Agents["main"] = a
		if fs := runRule(skl001Rule(), e.in); len(fs) != 1 || !strings.Contains(fs[0].Message, "memory.save") {
			t.Fatalf("without specs: findings = %+v", fs)
		}
		e.in.Specs = specsNamed("memory.save", "memory.search")
		if fs := runRule(skl001Rule(), e.in); len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		e := newSklEnv(t)
		e.agentSkill(t, "release", requiresTools("exec"))
		a := e.in.Config.Agents["main"]
		a.Skills = config.AgentSkills{Disabled: []string{"release"}}
		e.in.Config.Agents["main"] = a
		if fs := runRule(skl001Rule(), e.in); len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
}

func TestSKL002(t *testing.T) {
	e := newSklEnv(t)
	e.agentSkill(t, "prs", "metadata:\n  holaclaw:\n    version: 1\n    requires:\n      bins: [gh, {name: rg, pkg: ripgrep}]\n")
	e.agentSkill(t, "plain", "")
	fs := runRule(skl002Rule(), e.in)
	if len(fs) != 1 {
		t.Fatalf("findings = %+v, want 1", fs)
	}
	f := fs[0]
	if f.ID != "SKL002" || f.Key != "metadata.holaclaw.requires.bins" ||
		f.Message != "skill prs (agent) requires binary gh: binaries are not supported yet, hidden" ||
		f.Fix != `remove the skill, or add "prs" to skills.disabled until binaries land` {
		t.Errorf("finding = %+v", f)
	}

	a := e.in.Config.Agents["main"]
	a.Skills.Disabled = []string{"prs"}
	e.in.Config.Agents["main"] = a
	if fs := runRule(skl002Rule(), e.in); len(fs) != 0 {
		t.Fatalf("disabled: findings = %+v, want none", fs)
	}
}

func TestSKL006(t *testing.T) {
	t.Run("edited after install", func(t *testing.T) {
		e := newSklEnv(t)
		dir := e.agentSkill(t, "inst", "")
		writeOrigin(t, dir, "/src/inst", sklNow)
		if fs := runRule(skl006Rule(), e.in); len(fs) != 0 {
			t.Fatalf("unchanged: findings = %+v, want none", fs)
		}
		if err := writeFileErr(dir, "references/extra.md", "added by hand\n"); err != nil {
			t.Fatal(err)
		}
		fs := runRule(skl006Rule(), e.in)
		if len(fs) != 1 {
			t.Fatalf("findings = %+v, want 1", fs)
		}
		f := fs[0]
		if f.Message != "skill inst was installed from /src/inst but its files differ from the recorded hash" ||
			f.Fix != "rm -r "+dir+" && holaclaw skills install /src/inst --agent main" ||
			f.File != filepath.Join("agents", "main", "skills", "inst") || f.Key != "" {
			t.Errorf("finding = %+v", f)
		}
	})
	t.Run("hand-written skill", func(t *testing.T) {
		e := newSklEnv(t)
		e.agentSkill(t, "mine", "")
		if fs := runRule(skl006Rule(), e.in); len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("unreadable origin", func(t *testing.T) {
		e := newSklEnv(t)
		dir := e.agentSkill(t, "inst", "")
		if err := writeFileErr(dir, skill.OriginFile, "source = [\n"); err != nil {
			t.Fatal(err)
		}
		fs := runRule(skl006Rule(), e.in)
		if len(fs) != 1 || !strings.HasPrefix(fs[0].Message, "skill inst: .holaclaw/origin.toml unreadable: ") || fs[0].Fix != "" {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("unhashable files", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads everything")
		}
		e := newSklEnv(t)
		dir := e.agentSkill(t, "inst", "")
		writeOrigin(t, dir, "/src/inst", sklNow)
		secret := filepath.Join(dir, "references", "locked.md")
		if err := writeFileErr(dir, "references/locked.md", "x"); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(secret, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(secret, 0o644) })
		fs := runRule(skl006Rule(), e.in)
		if len(fs) != 1 || !strings.HasPrefix(fs[0].Message, "skill inst: cannot hash its files: ") || fs[0].Fix != "" {
			t.Fatalf("findings = %+v", fs)
		}
	})
}

func TestSKLProblems(t *testing.T) {
	e := newSklEnv(t)
	if err := writeFileErr(e.cfgDir, "agents/main/skills/broken/SKILL.md", "no frontmatter here\n"); err != nil {
		t.Fatal(err)
	}
	fs := runRule(sklProblemsRule(), e.in)
	if len(fs) != 1 {
		t.Fatalf("findings = %+v, want 1", fs)
	}
	f := fs[0]
	if f.ID != string(skill.CodeNoFrontmatter) || f.Severity != SeverityWarning ||
		f.File != filepath.Join("agents", "main", "skills", "broken") ||
		!strings.HasPrefix(f.Message, "main: skill folder broken: ") || !strings.HasSuffix(f.Message, ": hidden") {
		t.Errorf("finding = %+v", f)
	}
}

func TestSKLOff(t *testing.T) {
	e := newSklEnv(t)
	e.agentSkill(t, "release", requiresTools("exec"))
	e.in.SkillRoots = nil
	if fs := Run(context.Background(), e.in, []string{"SKL"}, nil); len(fs) != 0 {
		t.Fatalf("no roots: findings = %+v, want none", fs)
	}
	e = newSklEnv(t)
	e.agentSkill(t, "release", requiresTools("exec"))
	e.in.Config = nil
	if fs := Run(context.Background(), e.in, []string{"SKL"}, nil); len(fs) != 0 {
		t.Fatalf("no config: findings = %+v, want none", fs)
	}
}

// TestSKLRun runs every SKL rule through Run: rule order, --only selection,
// and nothing written under the config directory.
func TestSKLRun(t *testing.T) {
	e := newSklEnv(t)
	inst := e.agentSkill(t, "inst", "")
	writeOrigin(t, inst, "/src/inst", sklNow)
	if err := writeFileErr(inst, "SKILL.md", skillMD("inst", "")+"edited\n"); err != nil {
		t.Fatal(err)
	}
	e.agentSkill(t, "prs", "metadata:\n  holaclaw:\n    version: 1\n    requires:\n      tools: [exec]\n      bins: [gh]\n")
	if err := writeFileErr(e.cfgDir, "agents/main/skills/broken/SKILL.md", "---\nname: [\n---\n"); err != nil {
		t.Fatal(err)
	}
	before := hashTree(t, e.cfgDir)

	fs := Run(context.Background(), e.in, []string{"SKL"}, nil)
	var ids []string
	for _, f := range fs {
		ids = append(ids, f.ID)
	}
	want := []string{string(skill.CodeSyntax), "SKL001", "SKL002", "SKL006"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("ids = %v, want %v\n%+v", ids, want, fs)
	}
	if fs := Run(context.Background(), e.in, []string{"SKL006"}, nil); len(fs) != 1 {
		t.Errorf("--only SKL006: %+v", fs)
	}
	if fs := Run(context.Background(), e.in, []string{"SKL"}, []string{"SKL1"}); len(fs) != 3 {
		t.Errorf("--skip SKL1: %+v", fs)
	}
	if after := hashTree(t, e.cfgDir); after != before {
		t.Error("a check run changed the config directory")
	}
}

// specsNamed is a Specs func returning schemas with these names.
func specsNamed(names ...string) func(string) []provider.ToolSpec {
	return func(string) []provider.ToolSpec {
		out := make([]provider.ToolSpec, len(names))
		for i, n := range names {
			out[i] = provider.ToolSpec{Name: n}
		}
		return out
	}
}
