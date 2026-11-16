package agent

import (
	"slices"
	"strings"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/skill"
	"holaclaw/internal/tool"
)

// SkillLoadTool is the one tool that reads a skill body (docs/skills.md §4).
// It is hidden from a run whose index is empty: a tool with nothing to load
// only costs prompt.
const SkillLoadTool = "skill.load"

// SkillSaveTool saves a skill for the agent. The loop runs it only as the
// last call of a response and ends the turn after a successful save (docs/skills.md §5).
const SkillSaveTool = "skill.save"

// noteSaveNotLast is the tool error for a skill.save followed by other calls.
const noteSaveNotLast = "skill.save must be the last call of the turn: finish the task, then save"

// skillsHeading and skillsInstruction frame the index. The instruction is
// capped at 40 tokens by a test: the rule is the whole point, not prose.
const (
	skillsHeading     = "Skills (procedures you can load):"
	skillsInstruction = "If one clearly matches the task, load it with skill.load before acting. " +
		"At most one up front. If none matches, load none."
)

// SkillsFragment renders the `skills` system fragment: the heading, one
// "- name: description" line per entry with no level marker, the instruction.
// It changes only when the skill set does, so it belongs to the stable prefix.
func SkillsFragment(entries []SkillEntry) string {
	sks := make([]skill.Skill, 0, len(entries))
	for _, e := range entries {
		sks = append(sks, skill.Skill{Name: e.Name, Description: e.Description})
	}
	return skillsHeading + "\n" + skill.Index(sks) + skillsInstruction
}

// SkillForgetTool moves an agent skill to the trash.
const SkillForgetTool = "skill.forget"

// memoryTools is the name prefix of the memory tools, which a task run never
// gets: tasks have no memory (docs/memory.md §7).
const memoryTools = "memory."

// VisibleTools drops skill.load when the run has no skill to load, and
// skill.save and skill.forget unless the sender is an owner or admin in a
// conversation: those tools refuse anyone else, so the model is never offered
// a call that can only fail. The memory tools go the same way in a task run,
// and react unless the channel takes reactions (c.CanReact). c is the run's
// call template. The loop, the compactor's budget and `prompt-size` all apply
// it, so the three agree on the prefix.
func VisibleTools(specs []provider.ToolSpec, skills []SkillEntry, c tool.Call) []provider.ToolSpec {
	task := c.RunKind == tool.RunTask
	canWrite := (c.Role == tool.RoleOwner || c.Role == tool.RoleAdmin) && !task
	return slices.DeleteFunc(slices.Clone(specs), func(s provider.ToolSpec) bool {
		if task && strings.HasPrefix(s.Name, memoryTools) {
			return true
		}
		switch s.Name {
		case SkillLoadTool:
			return len(skills) == 0
		case SkillSaveTool, SkillForgetTool:
			return !canWrite
		case tool.ReactTool:
			return !c.CanReact
		}
		return false
	})
}

// skillNote closes a summary whose prefix loaded skills: a loaded body is
// gone from the prompt once summarised, so the model is told to reload it
// rather than work from the summary's paraphrase. Names come from the
// provenance line that opens every successful skill.load result.
func skillNote(prefix []event.Message) string {
	var names []string
	for _, m := range prefix {
		if m.Role != event.RoleTool || m.ToolName != SkillLoadTool {
			continue
		}
		rest, ok := strings.CutPrefix(m.Text, "[skill: ")
		if !ok {
			continue // an error result: nothing was loaded
		}
		name, _, ok := strings.Cut(rest, " ")
		if ok && skill.ValidName(name) && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	var b strings.Builder
	for _, n := range names {
		b.WriteString("\nSkill " + n + " was loaded earlier; load it again with skill.load if it is still needed.")
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n" + b.String()
}
