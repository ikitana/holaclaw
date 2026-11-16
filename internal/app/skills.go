package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"holaclaw/internal/agent"
	"holaclaw/internal/event"
	"holaclaw/internal/skill"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
)

// maxSkillRuns bounds the per-run cache: runs in flight at once, with room.
const maxSkillRuns = 64

// skillState caches the eligible skills of recent runs, so the index the
// prompt shows and the set skill.load serves are the same walk, and remembers
// which load problems were already logged.
type skillState struct {
	mu     sync.Mutex
	runs   map[string][]skill.Skill
	order  []string
	warned map[string]bool
}

// SkillRoots are the two skill roots of one agent (docs/skills.md §1, §2):
// bundled ones ship with holaclaw, agent ones live in the config directory.
// Skills follow the agent, never a person.
func (a *App) SkillRoots(agentName string) skill.Roots {
	return skill.Roots{
		Bundled: skill.Bundled,
		Agent:   filepath.Join(a.Config.Dir, "agents", agentName, "skills"),
	}
}

// SkillTmp is where an install fetches, unpacks and stages: inside the
// config directory, so a staged folder renames into agents/ on the same
// filesystem.
func (a *App) SkillTmp() string { return filepath.Join(a.Config.Dir, "tmp") }

// RunSkills returns the skills eligible for one run, decided from the
// trusted call template: its agent, sender role and run kind. The roots are
// walked once per run, at prompt build; skill.load reads the same cached set
// through Skills. Skills follow the agent, so the memory partition of the
// conversation plays no part in the key.
func (a *App) RunSkills(c tool.Call) []skill.Skill {
	key := strings.Join([]string{string(c.TraceID), c.Agent, c.Role, string(c.RunKind)}, "\x00")
	s := &a.skills
	s.mu.Lock()
	if got, ok := s.runs[key]; ok && c.TraceID != "" {
		s.mu.Unlock()
		return got
	}
	s.mu.Unlock()

	set, probs := skill.Load(a.SkillRoots(c.Agent))
	a.warnSkillProblems(c.Agent, probs)
	env := a.skillEnv(c)
	var out []skill.Skill
	for _, sk := range set.Resolve() {
		if ok, _ := skill.Eligible(sk, env); ok {
			out = append(out, sk)
		}
	}
	if out == nil {
		out = []skill.Skill{} // non-nil: "none", not "unset"
	}
	if c.TraceID == "" {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs == nil {
		s.runs = map[string][]skill.Skill{}
	}
	if _, ok := s.runs[key]; !ok {
		s.order = append(s.order, key)
		if len(s.order) > maxSkillRuns {
			delete(s.runs, s.order[0])
			s.order = s.order[1:]
		}
	}
	s.runs[key] = out
	return out
}

// Skills implements tools.SkillSource.
func (a *App) Skills(_ context.Context, c tool.Call) ([]skill.Skill, error) {
	return a.RunSkills(c), nil
}

// Teacher implements tools.SkillSource: the display name of the identity
// that taught a skill the agent saved, resolved from the store and sanitised
// the same way a group sender label is (docs/skills.md §4). An empty id, an
// unknown user or an empty stored name all give an empty result, which
// leaves the name out of the provenance line.
func (a *App) Teacher(ctx context.Context, id event.ID) (string, error) {
	if id == "" || a.Store == nil {
		return "", nil
	}
	i, err := a.Store.GetIdentity(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if i.Name == "" {
		return "", nil
	}
	return agent.SanitizeSender(i.Name), nil
}

// SkillEntries is the prompt index of an eligible set.
func SkillEntries(sks []skill.Skill) []agent.SkillEntry {
	out := make([]agent.SkillEntry, 0, len(sks))
	for _, sk := range sks {
		out = append(out, agent.SkillEntry{Name: sk.Name, Description: sk.Description})
	}
	return out
}

// skillEnv is what eligibility needs for one call. Tools holds the agent's
// entries as written (groups included) plus every concrete tool they
// select; Denied holds each selected tool whose class the policy denies for
// this sender and run kind, or that a per-tool override denies, plus any
// group with a denied member.
func (a *App) skillEnv(c tool.Call) skill.Env {
	ac := a.Config.Agents[c.Agent]
	env := skill.Env{Disabled: ac.Skills.Disabled}
	entries, err := a.toolEntries(c.Agent)
	if err != nil {
		return env
	}
	env.Tools = slices.Clone(entries)
	exec, err := a.Registry.Select(entries)
	if err != nil {
		return env
	}
	lk, _ := exec.(tool.Lookuper)
	pol := a.Policy(c.Agent)
	for _, s := range exec.Specs() {
		env.Tools = append(env.Tools, s.Name)
		if lk == nil {
			continue
		}
		t, ok := lk.Lookup(s.Name)
		if !ok || !deniedFor(pol, c, s.Name, t.Risk()) {
			continue
		}
		env.Denied = append(env.Denied, s.Name)
		if g, _, ok := strings.Cut(s.Name, "."); ok && !slices.Contains(env.Denied, g) {
			env.Denied = append(env.Denied, g)
		}
	}
	return env
}

// deniedFor mirrors the gate's static steps (class by origin, then the
// per-tool override) for a Deny only: an Ask can still be approved, so it
// never hides a skill.
func deniedFor(p tool.Policy, c tool.Call, name string, risk tool.Risk) bool {
	guest, task := c.Role == tool.RoleGuest, c.RunKind == tool.RunTask
	var d tool.Decision
	switch {
	case guest && task:
		d = p.Guest.For(risk)
		if p.Task.For(risk) == tool.Deny {
			d = tool.Deny
		}
	case guest:
		d = p.Guest.For(risk)
	case task:
		d = p.Task.For(risk)
	default:
		d = p.Default.For(risk)
	}
	if d == tool.Deny {
		return true
	}
	return p.Tools[name] == tool.Deny
}

// warnSkillProblems logs each load problem once per process, with its code.
// The model never sees them.
func (a *App) warnSkillProblems(agentName string, probs []skill.Problem) {
	s := &a.skills
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.warned == nil {
		s.warned = map[string]bool{}
	}
	log := a.Logger
	if log == nil {
		log = slog.Default()
	}
	for _, p := range probs {
		key := agentName + "\x00" + p.Error()
		if s.warned[key] {
			continue
		}
		s.warned[key] = true
		log.Warn("skill not loaded", "agent", agentName, "code", p.Code, "dir", p.Dir, "problem", p.Msg)
	}
}

// PromptSkills is the index `prompt-size` shows: the agent's owner in a
// direct, interactive run, the widest static view.
func (a *App) PromptSkills(agentName string) []agent.SkillEntry {
	return SkillEntries(a.RunSkills(tool.Call{Agent: agentName, Role: tool.RoleOwner, RunKind: tool.RunInteractive}))
}

// Writer implements tools.SkillSource: the writer over the agent skills
// root, the only place under agents/ the daemon writes. It is called before a
// write, so it also drops this run's cached eligible set: a skill.load later
// in the run walks the roots again and sees the new file. The prompt index of
// the turn is fixed at its start and does not change. A call whose agent is
// not configured gets an error, never a writer over a made-up folder.
func (a *App) Writer(c tool.Call) (skill.Writer, error) {
	if _, ok := a.Config.Agents[c.Agent]; !ok {
		return skill.Writer{}, fmt.Errorf("skills: agent %q is not configured", c.Agent)
	}
	a.forgetRun(c.TraceID)
	return skill.Writer{Root: a.SkillRoots(c.Agent).Agent}, nil
}

// forgetRun drops every cached set of one run.
func (a *App) forgetRun(traceID event.ID) {
	s := &a.skills
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := string(traceID) + "\x00"
	s.order = slices.DeleteFunc(s.order, func(k string) bool {
		if strings.HasPrefix(k, prefix) {
			delete(s.runs, k)
			return true
		}
		return false
	})
}
