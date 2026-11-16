package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/config"
	"holaclaw/internal/event"
	"holaclaw/internal/skill"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
)

// The roles a tool.Call carries are the store's, spelled the same.
func TestToolRolesMatchStore(t *testing.T) {
	for got, want := range map[string]store.Role{
		tool.RoleOwner: store.RoleOwner, tool.RoleAdmin: store.RoleAdmin,
		tool.RoleUser: store.RoleUser, tool.RoleGuest: store.RoleGuest,
	} {
		if got != string(want) {
			t.Errorf("tool role %q, store role %q", got, want)
		}
	}
}

func writeSkill(t *testing.T, dir, name, frontmatter string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o750); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: When the task is about " + name + ".\n" + frontmatter + "---\n# " + name + "\n"
	if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// skillApp is an app over a temporary config and state directory holding:
// plain (no requirements), with-files (needs files), with-exec (needs exec,
// denied by the default policy), off (disabled), broken (unparsable) and a
// learned agent skill.
func skillApp(t *testing.T) (*App, *bytes.Buffer) {
	t.Helper()
	a := testApp(t, "")
	a.Config.Dir, a.DataDir = t.TempDir(), t.TempDir()
	reg, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	a.Registry = reg
	var logs bytes.Buffer
	a.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	a.Config.Agents["main"] = config.Agent{
		Name: "main", Models: config.Roles{Chat: "chat"},
		Tools: []string{"files", "exec", "memory"},
		Policy: config.Policy{
			Classes: config.Classes{Read: "allow", Write: "allow", External: "allow", Exec: "deny"},
			Guest:   config.Classes{Read: "allow", Write: "deny", External: "deny", Exec: "deny"},
			Task:    config.Classes{Read: "allow", Write: "allow", External: "allow", Exec: "deny"},
			Tools:   map[string]string{"memory.forget": "deny"},
		},
		Skills: config.AgentSkills{Disabled: []string{"off"}},
	}
	root := a.SkillRoots("main")
	writeSkill(t, root.Agent, "plain", "")
	writeSkill(t, root.Agent, "with-files", "metadata:\n  holaclaw:\n    version: 1\n    requires:\n      tools: [files]\n")
	writeSkill(t, root.Agent, "with-exec", "metadata:\n  holaclaw:\n    version: 1\n    requires:\n      tools: [exec]\n")
	writeSkill(t, root.Agent, "with-search", "metadata:\n  holaclaw:\n    version: 1\n    requires:\n      tools: [memory.search]\n")
	writeSkill(t, root.Agent, "forgets", "metadata:\n  holaclaw:\n    version: 1\n    requires:\n      tools: [memory.forget]\n")
	writeSkill(t, root.Agent, "off", "")
	if err := os.MkdirAll(filepath.Join(root.Agent, "broken"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root.Agent, "broken", "SKILL.md"), []byte("no frontmatter"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, root.Agent, "mine",
		"metadata:\n  holaclaw:\n    version: 1\n    learned:\n      on: 2026-09-12\n      by: 01J9Q4ABCDEFGHJKMNPQRSTVWX\n"+
			"      from: 01J9Q4ABCDEFGHJKMNPQRSTVWY\n      origin: explicit\n")
	return a, &logs
}

func names(sks []skill.Skill) string {
	var out []string
	for _, sk := range sks {
		out = append(out, sk.Name)
	}
	return strings.Join(out, ",")
}

func TestRunSkillsEligibility(t *testing.T) {
	a, _ := skillApp(t)
	owner := tool.Call{Agent: "main", Role: "owner", RunKind: tool.RunInteractive, Partition: "dm/U1"}
	for _, tc := range []struct {
		name string
		mut  func(*tool.Call)
		want string
	}{
		// with-exec: exec is deny by class; forgets: a per-tool deny; off: disabled.
		{"owner", func(*tool.Call) {}, "mine,plain,with-files,with-search"},
		// the memory partition plays no part: skills follow the agent.
		{"other partition, same role", func(c *tool.Call) { c.Partition = "dm/U2" }, "mine,plain,with-files,with-search"},
		// files.write is deny for guests, so the files group is.
		{"guest", func(c *tool.Call) { c.Role = "guest" }, "mine,plain,with-search"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := owner
			tc.mut(&c)
			if got := names(a.RunSkills(c)); got != tc.want {
				t.Fatalf("eligible = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestRunSkillsNoneIsNonNil(t *testing.T) {
	a, _ := skillApp(t)
	a.Config.Dir = t.TempDir()
	a.DataDir = t.TempDir()
	got := a.RunSkills(tool.Call{Agent: "main", Role: "owner"})
	if got == nil || len(got) != 0 {
		t.Fatalf("got %#v", got)
	}
	if e := SkillEntries(got); e == nil || len(e) != 0 {
		t.Fatalf("entries %#v", e)
	}
}

// The prompt index and skill.load read the same walk within a run; a new
// run walks again.
func TestRunSkillsCachedPerRun(t *testing.T) {
	a, _ := skillApp(t)
	c := tool.Call{Agent: "main", Role: "owner", Partition: "dm/U1", TraceID: "T1"}
	first := a.RunSkills(c)
	if err := os.RemoveAll(filepath.Join(a.SkillRoots("main").Agent, "plain")); err != nil {
		t.Fatal(err)
	}
	src, err := a.Skills(context.Background(), c)
	if err != nil || names(src) != names(first) {
		t.Fatalf("tool saw %s, index %s (err %v)", names(src), names(first), err)
	}
	c.TraceID = "T2"
	if got := names(a.RunSkills(c)); strings.Contains(got, "plain") {
		t.Fatalf("a new run did not walk again: %s", got)
	}
	for i := range maxSkillRuns + 5 {
		c.TraceID = event.ID(fmt.Sprintf("X%d", i))
		a.RunSkills(c)
	}
	if len(a.skills.runs) > maxSkillRuns || len(a.skills.order) != len(a.skills.runs) {
		t.Fatalf("cache holds %d runs, order %d", len(a.skills.runs), len(a.skills.order))
	}
}

func TestSkillProblemsWarnedOnce(t *testing.T) {
	a, logs := skillApp(t)
	for range 3 {
		a.RunSkills(tool.Call{Agent: "main", Role: "owner"})
	}
	if n := strings.Count(logs.String(), "skill not loaded"); n != 1 {
		t.Fatalf("%d warnings:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "code="+string(skill.CodeNoFrontmatter)) {
		t.Fatalf("warning lacks the code: %s", logs.String())
	}
}

func TestPromptSkills(t *testing.T) {
	a, _ := skillApp(t)
	var got []string
	for _, e := range a.PromptSkills("main") {
		got = append(got, e.Name)
		if e.Description == "" {
			t.Fatalf("entry %+v", e)
		}
	}
	// Skills follow the agent: the learned one shows in the offline view too.
	if strings.Join(got, ",") != "mine,plain,with-files,with-search" {
		t.Fatalf("prompt skills = %v", got)
	}
}

// The skill tools join every tool set that names none of them; an agent that
// lists skill.load alone gets exactly that.
func TestSkillToolsInEveryToolSet(t *testing.T) {
	a, _ := skillApp(t)
	for _, tc := range []struct {
		tools []string
		want  []string
		not   []string
	}{
		{nil, []string{"skill.load", "skill.save", "skill.forget"}, nil},
		{[]string{"files"}, []string{"skill.load", "skill.save", "skill.forget"}, nil},
		{[]string{"skill"}, []string{"skill.load", "skill.save", "skill.forget"}, nil},
		{[]string{"skill.load"}, []string{"skill.load"}, []string{"skill.save", "skill.forget"}},
	} {
		ac := a.Config.Agents["main"]
		ac.Tools = tc.tools
		a.Config.Agents["main"] = ac
		exec, err := a.Executor("main")
		if err != nil || exec == nil {
			t.Fatalf("%v: %v", tc.tools, err)
		}
		var got []string
		for _, s := range exec.Specs() {
			got = append(got, s.Name)
		}
		for _, w := range tc.want {
			if !slices.Contains(got, w) {
				t.Errorf("%v: tools %v lack %s", tc.tools, got, w)
			}
		}
		for _, n := range tc.not {
			if slices.Contains(got, n) {
				t.Errorf("%v: tools %v include %s", tc.tools, got, n)
			}
		}
	}
}

// A call whose agent is missing or not configured gets no writer, so nothing
// is written under a folder no agent owns.
func TestSkillWriterUnknownAgent(t *testing.T) {
	a, _ := skillApp(t)
	for _, agentName := range []string{"", "nobody"} {
		if w, err := a.Writer(tool.Call{Agent: agentName, Role: tool.RoleOwner}); err == nil {
			t.Errorf("agent %q: writer %+v, no error", agentName, w)
		}
	}
}

// A write drops the run's cached set, so skill.load later in the same run
// sees the new file.
func TestSkillWriterDropsRunCache(t *testing.T) {
	a, _ := skillApp(t)
	c := tool.Call{Agent: "main", Role: "owner", RunKind: tool.RunInteractive, Partition: "dm/U1", TraceID: "T1"}
	other := c
	other.TraceID = "T2"
	a.RunSkills(c)
	a.RunSkills(other)
	w, err := a.Writer(c)
	if err != nil || w.Root != a.SkillRoots("main").Agent {
		t.Fatalf("writer root %q, %v", w.Root, err)
	}
	b, err := skill.Render(skill.Draft{
		Name: "fresh", Title: "Fresh", When: "When something is fresh.", Steps: []string{"One.", "Two."},
	}, skill.Learned{On: time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC), By: "01J9Q4ABCDEFGHJKMNPQRSTVWX", From: "01J9Q4ABCDEFGHJKMNPQRSTVWX", Origin: skill.OriginExplicit}, skill.Requires{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Save("fresh", b); err != nil {
		t.Fatal(err)
	}
	got, err := a.Skills(context.Background(), c)
	if err != nil || !strings.Contains(names(got), "fresh") {
		t.Fatalf("after the save: %s (%v)", names(got), err)
	}
	if got := names(a.RunSkills(other)); strings.Contains(got, "fresh") {
		t.Fatalf("another run's cache was dropped: %s", got)
	}
	if len(a.skills.order) != len(a.skills.runs) {
		t.Fatalf("cache order %d, runs %d", len(a.skills.order), len(a.skills.runs))
	}
}
