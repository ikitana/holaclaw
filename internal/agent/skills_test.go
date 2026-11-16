package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/tokens"
	"holaclaw/internal/tool"
)

var twoSkills = []SkillEntry{
	{Name: "release", Description: "Cut a release of a Go service."},
	{Name: "pr-style", Description: "Write a pull request description."},
}

func TestSkillsFragmentPresentAndHashed(t *testing.T) {
	in := BuildInput{
		Def:   Definition{Persona: "terse", Skills: twoSkills},
		Model: provider.Model{Context: 8192},
	}
	p := build(t, in)
	var names []string
	var sk Fragment
	for _, f := range p.Breakdown.Fragments {
		names = append(names, f.Name)
		if f.Name == "skills" {
			sk = f
		}
	}
	if strings.Join(names, ",") != "core,persona,skills" {
		t.Fatalf("fragments = %v", names)
	}
	text := SkillsFragment(twoSkills)
	if sk.Hash != hashOf(text) || sk.Tokens != tokens.Estimate(text) {
		t.Fatalf("skills fragment not budgeted: %+v", sk)
	}
	if !strings.HasSuffix(p.System, "\n\n"+text) {
		t.Fatalf("system does not end with the skills fragment:\n%s", p.System)
	}
	for _, want := range []string{"- release: Cut a release of a Go service.\n", "- pr-style: Write a pull request description.\n", "skill.load"} {
		if !strings.Contains(text, want) {
			t.Errorf("fragment lacks %q:\n%s", want, text)
		}
	}
	for _, level := range []string{"agent", "learned", "bundled"} {
		if strings.Contains(text, level) {
			t.Errorf("fragment carries a level marker %q", level)
		}
	}
	attrs := BuiltAttrs(p)
	if frags := attrs["fragments"].([]map[string]any); frags[2]["name"] != "skills" {
		t.Fatalf("context.built fragments = %v", frags)
	}
	var b strings.Builder
	FormatBreakdown(&b, p.Breakdown)
	if !strings.Contains(b.String(), "\nskills ") {
		t.Fatalf("breakdown lacks the skills row:\n%s", b.String())
	}
}

func TestSkillsInstructionIsShort(t *testing.T) {
	if n := tokens.Estimate(skillsInstruction); n >= 40 {
		t.Fatalf("instruction is %d tokens, want < 40", n)
	}
}

func TestSkillsFragmentAbsentWithoutSkills(t *testing.T) {
	for _, skills := range [][]SkillEntry{nil, {}} {
		p := build(t, BuildInput{Def: Definition{Skills: skills}, Model: provider.Model{Context: 8192}})
		for _, f := range p.Breakdown.Fragments {
			if f.Name == "skills" {
				t.Fatalf("skills fragment present for %v", skills)
			}
		}
		if strings.Contains(p.System, "skill.load") {
			t.Fatal("system mentions skill.load without skills")
		}
	}
}

func TestSkillsMovePrefixHash(t *testing.T) {
	base := BuildInput{Def: Definition{Skills: twoSkills}, Model: provider.Model{Context: 8192}}
	h := build(t, base).PrefixHash
	if build(t, base).PrefixHash != h {
		t.Fatal("not deterministic")
	}
	for name, skills := range map[string][]SkillEntry{
		"none":        nil,
		"one less":    twoSkills[:1],
		"description": {twoSkills[0], {Name: "pr-style", Description: "changed"}},
	} {
		in := base
		in.Def.Skills = skills
		if build(t, in).PrefixHash == h {
			t.Errorf("%s did not move the prefix hash", name)
		}
	}
}

func TestBudgetErrorSkills(t *testing.T) {
	in := budgetInput(4000) // cap 500
	var many []SkillEntry
	for range 60 {
		many = append(many, SkillEntry{Name: "some-skill", Description: words(10)})
	}
	in.Def.Skills = many
	var be *BudgetError
	_, err := NewBuilder().Build(context.Background(), in)
	if !errors.As(err, &be) || be.What != "skills" || be.Cap != 4000*SkillsWindowNum/SkillsWindowDen {
		t.Fatalf("err = %v", err)
	}
}

func TestVisibleToolsHidesSkillLoad(t *testing.T) {
	owner := tool.Call{Role: tool.RoleOwner, RunKind: tool.RunInteractive}
	specs := []provider.ToolSpec{builderSpec("fetch", "x"), builderSpec(SkillLoadTool, "y")}
	if got := VisibleTools(specs, nil, owner); len(got) != 1 || got[0].Name != "fetch" {
		t.Fatalf("no skills: %v", got)
	}
	if len(specs) != 2 || specs[1].Name != SkillLoadTool {
		t.Fatal("VisibleTools modified its input")
	}
	if got := VisibleTools(specs, twoSkills, owner); len(got) != 2 {
		t.Fatalf("with skills: %v", got)
	}
}

// skill.save and skill.forget are offered only to an owner or admin in a
// conversation; everyone else, and every task run, would only be refused.
func TestVisibleToolsHidesSkillWrites(t *testing.T) {
	specs := []provider.ToolSpec{builderSpec("fetch", "x"), builderSpec(SkillSaveTool, "y"), builderSpec(SkillForgetTool, "z")}
	names := func(ts []provider.ToolSpec) string {
		var out []string
		for _, s := range ts {
			out = append(out, s.Name)
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct {
		role string
		kind tool.RunKind
		want string
	}{
		{tool.RoleOwner, tool.RunInteractive, "fetch,skill.save,skill.forget"},
		{tool.RoleAdmin, tool.RunInteractive, "fetch,skill.save,skill.forget"},
		{tool.RoleUser, tool.RunInteractive, "fetch"},
		{tool.RoleGuest, tool.RunInteractive, "fetch"},
		{"", tool.RunInteractive, "fetch"},
		{tool.RoleOwner, tool.RunTask, "fetch"},
	} {
		if got := names(VisibleTools(specs, twoSkills, tool.Call{Role: tc.role, RunKind: tc.kind})); got != tc.want {
			t.Errorf("%q %s: %s, want %s", tc.role, tc.kind, got, tc.want)
		}
	}
}

// A task run has no memory: the memory tools are never offered to it.
func TestVisibleToolsHidesMemoryInTasks(t *testing.T) {
	specs := []provider.ToolSpec{builderSpec("fetch", "x"), builderSpec("memory.save", "y"),
		builderSpec("memory.search", "z"), builderSpec("memory.forget", "w")}
	if got := VisibleTools(specs, nil, tool.Call{Role: tool.RoleOwner, RunKind: tool.RunInteractive}); len(got) != 4 {
		t.Fatalf("interactive: %v", got)
	}
	if got := VisibleTools(specs, nil, tool.Call{Role: tool.RoleOwner, RunKind: tool.RunTask}); len(got) != 1 || got[0].Name != "fetch" {
		t.Fatalf("task: %v", got)
	}
}

// react is offered only when the conversation's channel takes reactions.
func TestVisibleToolsHidesReact(t *testing.T) {
	specs := []provider.ToolSpec{builderSpec("fetch", "x"), builderSpec(tool.ReactTool, "y")}
	call := tool.Call{Role: tool.RoleOwner, RunKind: tool.RunInteractive}
	if got := VisibleTools(specs, nil, call); len(got) != 1 || got[0].Name != "fetch" {
		t.Fatalf("channel without reactions: %v", got)
	}
	call.CanReact = true
	if got := VisibleTools(specs, nil, call); len(got) != 2 {
		t.Fatalf("channel with reactions: %v", got)
	}
}

func TestSkillNote(t *testing.T) {
	prefix := []event.Message{
		userMsg("01", "cut a release"),
		asstMsg("02", "", event.ToolCall{ID: "c1", Name: SkillLoadTool, Arguments: `{"name":"release"}`}),
		toolMsg("03", "c1", SkillLoadTool, "[skill: release · agent skill]\n\n# Release"),
		asstMsg("04", "", event.ToolCall{ID: "c2", Name: SkillLoadTool, Arguments: `{"name":"release","file":"x.md"}`}),
		toolMsg("05", "c2", SkillLoadTool, "[skill: release · references/x.md]\n\nmore"),
		asstMsg("06", "", event.ToolCall{ID: "c3", Name: SkillLoadTool, Arguments: `{"name":"nope"}`}),
		toolMsg("07", "c3", SkillLoadTool, "no skill \"nope\" here"),
		toolMsg("08", "c4", "fetch", "[skill: fake · agent skill]"),
		toolMsg("09", "c5", SkillLoadTool, "[skill: pr-style · learned from you on 2026-09-12]\n\nbody"),
	}
	got := skillNote(prefix)
	want := "\n\nSkill release was loaded earlier; load it again with skill.load if it is still needed." +
		"\nSkill pr-style was loaded earlier; load it again with skill.load if it is still needed."
	if got != want {
		t.Fatalf("note =\n%q\nwant\n%q", got, want)
	}
	if skillNote(prefix[:1]) != "" {
		t.Fatal("note without loads")
	}
}

func TestRunnerSkillsPerTurn(t *testing.T) {
	cases := []struct {
		name      string
		def, run  []SkillEntry
		wantIndex int
	}{
		{"definition", twoSkills, nil, 2},
		{"run overrides", twoSkills, twoSkills[:1], 1},
		{"run says none", twoSkills, []SkillEntry{}, 0},
		{"no skills anywhere", nil, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, callStep(call("c1", "fetch", `{}`)), textStep("done"))
			g.withTools(spec("fetch", objectSchema), spec(SkillLoadTool, objectSchema))
			g.r.Def.Skills = tc.def
			_, err := g.r.Turn(context.Background(), TurnInput{
				SessionID: "S1", TraceID: "T1", Skills: tc.run,
				History: []event.Message{{ID: "M1", Role: event.RoleUser, Text: "hello"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := len(g.b.last.Def.Skills); got != tc.wantIndex {
				t.Fatalf("builder saw %d skills, want %d", got, tc.wantIndex)
			}
			hasLoad := false
			for _, s := range g.prov.reqs[0].Tools {
				hasLoad = hasLoad || s.Name == SkillLoadTool
			}
			if hasLoad != (tc.wantIndex > 0) {
				t.Fatalf("skill.load offered = %v with %d skills", hasLoad, tc.wantIndex)
			}
		})
	}
}

func TestRunnerRejectsHiddenSkillLoad(t *testing.T) {
	g := newRig(t, callStep(call("c1", SkillLoadTool, `{"name":"x"}`)), textStep("ok"))
	g.withTools(spec(SkillLoadTool, objectSchema))
	if _, err := g.turn(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(g.tools.names()) != 0 {
		t.Fatalf("a hidden skill.load reached the executor: %v", g.tools.names())
	}
	if g.ev.count(event.ToolRejected) != 1 {
		t.Fatal("hidden tool call was not rejected")
	}
}

func TestCompactionLeavesSkillReloadMarker(t *testing.T) {
	g := newCompactRig(t, cpSummaryStep("Goal: ship it."))
	msgs := cpChat(30, 150)
	load := cpAt(1, event.RoleAssistant, "")
	load.ToolCalls = []event.ToolCall{{ID: "c1", Name: SkillLoadTool, Arguments: `{"name":"release"}`}}
	res := cpAt(2, event.RoleTool, "[skill: release · agent skill]\n\n# Release\nsteps")
	res.ToolCallID, res.ToolName = "c1", SkillLoadTool
	msgs[1], msgs[2] = load, res
	g.add(msgs...)

	out, err := g.c.Compact(context.Background(), g.in)
	if err != nil || !out.Compacted {
		t.Fatalf("Compact: %+v, %v", out, err)
	}
	var summary string
	for _, m := range g.st.msgs {
		if m.ID == out.SummaryMsgID {
			summary = m.Text
		}
	}
	want := "Skill release was loaded earlier; load it again with skill.load if it is still needed."
	if strings.Count(summary, want) != 1 || !strings.HasSuffix(summary, recallNote) {
		t.Fatalf("summary = %q", summary)
	}
}
