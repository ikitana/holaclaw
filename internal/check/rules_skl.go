package check

import (
	"context"
	"fmt"
	"path"
	"path/filepath"

	"holaclaw/internal/config"
	"holaclaw/internal/skill"
)

// The skill rules (docs/skills.md §3, §6, §8). Each names the skill and why
// it is hidden or suspect. They load every agent's two roots the way the
// daemon does at prompt build; Load is a read-only walk, cheap enough to
// repeat per rule. SKL003, SKL004 and SKL007 belong to the exec sidecar and
// are not implemented yet; SKL005 (unused skill) is deferred.

// agentSkills is one agent's loaded roots.
type agentSkills struct {
	agent string
	set   skill.Set
	probs []skill.Problem
}

// loadSkills loads every configured agent's skills, in agent order; nil when
// there is nothing to look at.
func loadSkills(in *Input) []agentSkills {
	if in.Config == nil || in.SkillRoots == nil {
		return nil
	}
	var out []agentSkills
	for _, name := range sortedKeys(in.Config.Agents) {
		set, probs := skill.Load(in.SkillRoots(name))
		out = append(out, agentSkills{agent: name, set: set, probs: probs})
	}
	return out
}

// skillFile is where a finding about sk points: bundled/<name>, or an agent
// skill relative to the config directory.
func skillFile(in *Input, sk skill.Skill) string {
	if sk.Level == skill.LevelBundled {
		return path.Join("bundled", sk.Name)
	}
	return relFile(in.ConfigDir, sk.Dir)
}

// skillLabel is "name (bundled)" or "name (agent)".
func skillLabel(sk skill.Skill) string {
	return fmt.Sprintf("%s (%s)", sk.Name, sk.Level)
}

// disabled reports whether the agent hides sk on purpose (skills.disabled).
func disabled(in *Input, agent string, sk skill.Skill) bool {
	return contains(in.Config.Agents[agent].Skills.Disabled, sk.Name)
}

// sklProblemsRule passes every skill.Problem through with its own SKL1xx
// code: a folder that does not parse is never in the index. The second rule,
// after CFG, that produces more than one id.
func sklProblemsRule() Rule {
	return Rule{
		ID:      "SKL1xx",
		Summary: "a skill folder that does not parse (hidden)",
		Run: func(_ context.Context, in *Input) []Finding {
			var out []Finding
			for _, as := range loadSkills(in) {
				for _, p := range as.probs {
					out = append(out, Finding{
						ID:       string(p.Code),
						Severity: SeverityWarning,
						File:     relFile(in.ConfigDir, p.Dir),
						Message:  fmt.Sprintf("%s: skill folder %s: %s: hidden", as.agent, path.Base(filepath.ToSlash(p.Dir)), p.Msg),
					})
				}
			}
			return out
		},
	}
}

// agentToolNames is what eligibility compares requires.tools against: the
// agent's tool entries plus every tool name they expand to, and the entries
// the daemon adds on its own (image.generate with an image role, the skill
// group), mirroring app.toolEntries.
func agentToolNames(in *Input, agent string, a config.Agent) []string {
	names := append([]string{}, a.Tools...)
	if a.Models.Image != "" {
		names = append(names, "image.generate")
	}
	names = append(names, "skill")
	if in.Specs != nil {
		for _, s := range in.Specs(agent) {
			names = append(names, s.Name)
		}
	}
	return names
}

// skl001Rule: a skill requires a tool the agent does not list, so the
// daemon keeps it out of the index (§3 rule 1).
func skl001Rule() Rule {
	return Rule{
		ID:      "SKL001",
		Summary: "skill requires a tool the agent does not list (hidden)",
		Run: func(_ context.Context, in *Input) []Finding {
			var out []Finding
			for _, as := range loadSkills(in) {
				tools := agentToolNames(in, as.agent, in.Config.Agents[as.agent])
				for _, sk := range as.set.All() {
					if disabled(in, as.agent, sk) {
						continue
					}
					for _, t := range sk.Requires.Tools {
						if contains(tools, t) {
							continue
						}
						out = append(out, Finding{
							ID:       "SKL001",
							Severity: SeverityWarning,
							File:     skillFile(in, sk),
							Key:      "metadata.holaclaw.requires.tools",
							Message:  fmt.Sprintf("skill %s requires tool %s, which agent %s does not list: hidden", skillLabel(sk), t, as.agent),
							Fix:      fmt.Sprintf(`add "%s" to tools in agents/%s/agent.toml, or add "%s" to skills.disabled`, t, as.agent, sk.Name),
						})
						break
					}
				}
			}
			return out
		},
	}
}

// skl002Rule: a skill requires a binary. Until the exec sidecar lands (§8)
// no binary is ever realised, so the skill is always hidden.
func skl002Rule() Rule {
	return Rule{
		ID:      "SKL002",
		Summary: "skill requires a binary: not supported yet (hidden)",
		Run: func(_ context.Context, in *Input) []Finding {
			var out []Finding
			for _, as := range loadSkills(in) {
				for _, sk := range as.set.All() {
					if len(sk.Requires.Bins) == 0 || disabled(in, as.agent, sk) {
						continue
					}
					out = append(out, Finding{
						ID:       "SKL002",
						Severity: SeverityWarning,
						File:     skillFile(in, sk),
						Key:      "metadata.holaclaw.requires.bins",
						Message:  fmt.Sprintf("skill %s requires binary %s: binaries are not supported yet, hidden", skillLabel(sk), sk.Requires.Bins[0].Name),
						Fix:      fmt.Sprintf(`remove the skill, or add "%s" to skills.disabled until binaries land`, sk.Name),
					})
				}
			}
			return out
		},
	}
}

// skl006Rule: an installed agent skill whose files no longer hash to what
// install recorded — edited by hand, or tampered with. Hand-written skills
// have no origin record and are never checked.
func skl006Rule() Rule {
	return Rule{
		ID:      "SKL006",
		Summary: "installed skill differs from its recorded hash",
		Run: func(_ context.Context, in *Input) []Finding {
			var out []Finding
			for _, as := range loadSkills(in) {
				for _, sk := range as.set.All() {
					if sk.Level != skill.LevelAgent {
						continue
					}
					f := Finding{ID: "SKL006", Severity: SeverityWarning, File: skillFile(in, sk)}
					o, ok, err := skill.ReadOrigin(sk.Dir)
					if err != nil {
						f.Message = fmt.Sprintf("skill %s: %s unreadable: %v", sk.Name, skill.OriginFile, err)
						out = append(out, f)
						continue
					}
					if !ok {
						continue
					}
					h, err := skill.Hash(sk.Dir)
					if err != nil {
						f.Message = fmt.Sprintf("skill %s: cannot hash its files: %v", sk.Name, err)
						out = append(out, f)
						continue
					}
					if h == o.Hash {
						continue
					}
					f.Message = fmt.Sprintf("skill %s was installed from %s but its files differ from the recorded hash", sk.Name, o.Source)
					f.Fix = fmt.Sprintf("rm -r %s && holaclaw skills install %s --agent %s", sk.Dir, o.Source, as.agent)
					out = append(out, f)
				}
			}
			return out
		},
	}
}
