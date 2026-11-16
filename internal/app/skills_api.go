package app

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/skill"
	"holaclaw/internal/tool"
)

// skillHidden is the wire form of gen.Skill.Hidden.
type skillHidden = struct {
	Code    *string               `json:"code,omitempty"`
	Message *string               `json:"message,omitempty"`
	Reason  gen.SkillHiddenReason `json:"reason"`
	Subject *string               `json:"subject,omitempty"`
}

// hiddenTrashed is the Hidden of a skill in the trash.
func hiddenTrashed() *skillHidden {
	return &skillHidden{Reason: gen.SkillHiddenReasonTrashed, Message: ptrTo("in the trash")}
}

// hiddenOf is the Hidden of a loaded skill from Eligible: nil when ok.
func hiddenOf(ok bool, why skill.Reason) *skillHidden {
	if ok {
		return nil
	}
	return &skillHidden{Reason: gen.SkillHiddenReason(why.Kind), Subject: optString(why.Subject), Message: ptrTo(why.String())}
}

// ListSkills implements api.SkillBackend: every skill of the agent as the
// loader sees it (a bundled one an agent skill shadows included), a row per
// load problem, and the skills in the trash. Eligibility is the owner's in a
// direct, interactive run. Sorted bundled, agent, trashed, each group by
// name.
func (a *App) ListSkills(ctx context.Context, agentName string) ([]gen.Skill, error) {
	if err := a.knownSkillAgent(agentName); err != nil {
		return nil, err
	}
	roots := a.SkillRoots(agentName)
	set, probs := skill.Load(roots)
	trashed, tprobs := skill.Trashed(roots.Agent)
	env := a.ownerEnv(agentName)
	names := &teachers{a: a}
	var out []gen.Skill
	for _, sk := range set.All() {
		out = append(out, withLive(toGenSkill(ctx, names, sk, hiddenOf(skill.Eligible(sk, env))), sk))
	}
	for _, sk := range trashed {
		g := withLive(toGenSkill(ctx, names, sk, hiddenTrashed()), sk)
		g.Trashed = ptrTo(true)
		out = append(out, g)
	}
	for _, p := range append(probs, tprobs...) {
		out = append(out, problemSkill(roots, p))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if gi, gj := skillGroup(out[i]), skillGroup(out[j]); gi != gj {
			return gi < gj
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// GetSkill implements api.SkillBackend: the agent skill named id, else
// the bundled one it would shadow, else the one in the trash, as it is now.
// Body and Raw are filled, and Version and Versions for an agent skill.
func (a *App) GetSkill(ctx context.Context, agentName, id string) (gen.Skill, error) {
	out, found, err := a.findSkill(ctx, agentName, id)
	if err != nil {
		return gen.Skill{}, err
	}
	return withFile(out, found)
}

// GetSkillVersion implements api.SkillBackend: GetSkill for version of an
// agent skill. The live number is the skill as it is now; one in history/
// fills Body, Raw and Learned from that version, with Version set to it and
// Versions still the full list. Any other number, and any version of a
// bundled skill, is skill.ErrNotFound.
func (a *App) GetSkillVersion(ctx context.Context, agentName, id string, version int) (gen.Skill, error) {
	out, found, err := a.findSkill(ctx, agentName, id)
	if err != nil {
		return gen.Skill{}, err
	}
	if found.Level == skill.LevelBundled {
		return gen.Skill{}, fmt.Errorf("skill %s: a bundled skill has no versions: %w", id, skill.ErrNotFound)
	}
	if version == deref(out.Version) {
		return withFile(out, found)
	}
	// One earlier version: its file and what its frontmatter says, on the
	// live skill's row. A version that no longer parses still has a file.
	vsk, raw, err := skill.ReadVersion(found.Dir, version)
	var p skill.Problem
	switch {
	case errors.Is(err, skill.ErrNotFound):
		return gen.Skill{}, err
	case errors.As(err, &p):
		out.Body, out.Raw, out.Learned = ptrTo(""), ptrTo(string(raw)), nil
		out.Hidden = &skillHidden{Reason: gen.SkillHiddenReasonInvalid, Code: ptrTo(string(p.Code)), Message: ptrTo(p.Msg)}
		out.Version = &version
		return out, nil
	case err != nil:
		return gen.Skill{}, err
	}
	vout := toGenSkill(ctx, &teachers{a: a}, vsk, out.Hidden) // the version's own references and scripts
	vout.Trashed, vout.Versions, vout.Version = out.Trashed, out.Versions, &version
	vout.Body, vout.Raw = ptrTo(vsk.Body), ptrTo(string(raw))
	return vout, nil
}

// findSkill is the row GetSkill starts from, without Body and Raw, and the
// skill it is of: for an agent skill, live or trashed, Version and Versions
// are filled.
func (a *App) findSkill(ctx context.Context, agentName, id string) (gen.Skill, skill.Skill, error) {
	if err := a.knownSkillAgent(agentName); err != nil {
		return gen.Skill{}, skill.Skill{}, err
	}
	roots := a.SkillRoots(agentName)
	set, _ := skill.Load(roots)
	names := &teachers{a: a}
	var out gen.Skill
	var found skill.Skill
	if sk, ok := set.Get(id); ok {
		out, found = toGenSkill(ctx, names, sk, hiddenOf(skill.Eligible(sk, a.ownerEnv(agentName)))), sk
		if sk.Level == skill.LevelBundled {
			return out, found, nil
		}
	} else {
		trashed, _ := skill.Trashed(roots.Agent)
		for _, sk := range trashed {
			if sk.Name == id {
				out, found = toGenSkill(ctx, names, sk, hiddenTrashed()), sk
				out.Trashed = ptrTo(true)
				break
			}
		}
		if found.Dir == "" {
			return gen.Skill{}, skill.Skill{}, fmt.Errorf("skill %s: %w", id, skill.ErrNotFound)
		}
	}
	live, err := skill.LiveVersion(found.Dir)
	if err != nil {
		return gen.Skill{}, skill.Skill{}, err
	}
	versions, err := versionRows(ctx, names, found.Dir)
	if err != nil {
		return gen.Skill{}, skill.Skill{}, err
	}
	out.Version, out.Versions = &live, &versions
	return out, found, nil
}

// withLive sets Version on the row of an agent skill to its live number. A
// history/ that cannot be read leaves it unset rather than failing the list;
// GetSkill reports the error.
func withLive(out gen.Skill, sk skill.Skill) gen.Skill {
	if sk.Level != skill.LevelAgent {
		return out
	}
	if n, err := skill.LiveVersion(sk.Dir); err == nil {
		out.Version = &n
	}
	return out
}

// withFile fills Body and Raw from the skill's file; a folder whose file
// vanished between the load and the read is not found.
func withFile(out gen.Skill, sk skill.Skill) (gen.Skill, error) {
	raw, err := sk.File()
	if err != nil {
		return gen.Skill{}, fmt.Errorf("skill %s: %w", sk.Name, err)
	}
	out.Body, out.Raw = ptrTo(sk.Body), ptrTo(string(raw))
	return out, nil
}

// versionRows lists an agent skill's versions with the learned block each
// carries: who taught it and the trace that saved it are what the explorer
// shows next to the version. A version a person wrote (install, a hand edit)
// or one that does not parse is listed by number alone.
func versionRows(ctx context.Context, names *teachers, dir string) ([]gen.SkillVersion, error) {
	ns, err := skill.ListVersions(dir)
	if err != nil {
		return nil, err
	}
	rows := make([]gen.SkillVersion, 0, len(ns))
	for _, n := range ns {
		row := gen.SkillVersion{Version: n}
		if vsk, _, err := skill.ReadVersion(dir, n); err == nil && vsk.Learned != nil {
			l := vsk.Learned
			row.On = ptrTo(l.On.UTC())
			row.Trace = ptrTo(string(l.From))
			row.By, row.ByName = ptrTo(string(l.By)), optString(names.name(ctx, l.By))
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// RemoveSkill implements api.SkillBackend. A live agent skill moves to the
// trash (skill.forgotten); one already there is deleted for good, with no
// event. A bundled skill is never written (skill.ErrBundled).
func (a *App) RemoveSkill(ctx context.Context, agentName, id string, who event.ID) error {
	if err := a.knownSkillAgent(agentName); err != nil {
		return err
	}
	roots := a.SkillRoots(agentName)
	w := skill.Writer{Root: roots.Agent}
	versions, err := w.Forget(id)
	if errors.Is(err, skill.ErrNotFound) {
		if err = w.Remove(id); errors.Is(err, skill.ErrNotFound) && onlyBundled(roots, id) {
			return fmt.Errorf("skill %s: %w", id, skill.ErrBundled)
		}
		return err
	}
	if err != nil {
		return err
	}
	a.forgetAll()
	a.emitSkill(ctx, event.SkillForgotten, map[string]any{
		"agent": agentName, "name": id, "versions": versions, "who": string(who),
	})
	return nil
}

// RestoreSkill implements api.SkillBackend: the skill leaves the trash with
// its history (skill.restored).
func (a *App) RestoreSkill(ctx context.Context, agentName, id string, who event.ID) (gen.Skill, error) {
	if err := a.knownSkillAgent(agentName); err != nil {
		return gen.Skill{}, err
	}
	if err := (skill.Writer{Root: a.SkillRoots(agentName).Agent}).Restore(id); err != nil {
		return gen.Skill{}, err
	}
	a.emitSkill(ctx, event.SkillRestored, map[string]any{
		"agent": agentName, "name": id, "who": string(who),
	})
	a.forgetAll()
	return a.GetSkill(ctx, agentName, id)
}

// RevertSkill implements api.SkillBackend: version to is copied into a new
// current version and the current one retires into history (skill.reverted,
// with the new version). A nil to, or the live number, is a no-op that
// returns the live skill. A name only bundled is skill.ErrBundled.
func (a *App) RevertSkill(ctx context.Context, agentName, id string, to *int, who event.ID) (gen.Skill, error) {
	if err := a.knownSkillAgent(agentName); err != nil {
		return gen.Skill{}, err
	}
	roots := a.SkillRoots(agentName)
	if to == nil {
		return a.liveAgentSkill(ctx, agentName, roots, id)
	}
	if skill.ValidName(id) {
		if n, err := skill.LiveVersion(filepath.Join(roots.Agent, id)); err == nil && n == *to {
			return a.liveAgentSkill(ctx, agentName, roots, id)
		}
	}
	r, err := skill.Writer{Root: roots.Agent}.Revert(id, *to)
	if errors.Is(err, skill.ErrNotFound) {
		if onlyBundled(roots, id) {
			return gen.Skill{}, fmt.Errorf("skill %s: %w", id, skill.ErrBundled)
		}
	}
	if err != nil {
		return gen.Skill{}, err
	}
	a.emitSkill(ctx, event.SkillReverted, map[string]any{
		"agent": agentName, "name": id, "from": r.From, "to": r.To, "version": r.Version, "who": string(who),
	})
	a.forgetAll()
	return a.GetSkill(ctx, agentName, id)
}

// liveAgentSkill is GetSkill for a live agent skill only: skill.ErrBadName,
// skill.ErrBundled for a name only bundled, skill.ErrNotFound otherwise.
func (a *App) liveAgentSkill(ctx context.Context, agentName string, roots skill.Roots, id string) (gen.Skill, error) {
	if !skill.ValidName(id) {
		return gen.Skill{}, fmt.Errorf("skill %q: %w", id, skill.ErrBadName)
	}
	set, _ := skill.Load(roots)
	switch sk, ok := set.Get(id); {
	case ok && sk.Level == skill.LevelAgent:
		return a.GetSkill(ctx, agentName, id)
	case ok:
		return gen.Skill{}, fmt.Errorf("skill %s: %w", id, skill.ErrBundled)
	default:
		return gen.Skill{}, fmt.Errorf("skill %s: %w", id, skill.ErrNotFound)
	}
}

// onlyBundled reports whether name resolves to a bundled skill: no live
// agent skill of that name shadows it.
func onlyBundled(roots skill.Roots, name string) bool {
	set, _ := skill.Load(roots)
	sk, ok := set.Get(name)
	return ok && sk.Level == skill.LevelBundled
}

// knownSkillAgent is skill.ErrNotFound for an agent the config does not have.
func (a *App) knownSkillAgent(agentName string) error {
	if _, ok := a.Config.Agents[agentName]; !ok {
		return fmt.Errorf("agent %q: %w", agentName, skill.ErrNotFound)
	}
	return nil
}

// ownerEnv is the eligibility env of the agent's owner in a direct,
// interactive run.
func (a *App) ownerEnv(agentName string) skill.Env {
	return a.skillEnv(tool.Call{Agent: agentName, Role: tool.RoleOwner, RunKind: tool.RunInteractive})
}

// teachers resolves Learned.By to a display name through App.Teacher, once
// per id; an id it cannot resolve has no name.
type teachers struct {
	a    *App
	seen map[event.ID]string
}

func (t *teachers) name(ctx context.Context, id event.ID) string {
	if n, ok := t.seen[id]; ok {
		return n
	}
	n, err := t.a.Teacher(ctx, id)
	if err != nil {
		n = ""
	}
	if t.seen == nil {
		t.seen = map[event.ID]string{}
	}
	t.seen[id] = n
	return n
}

// emitSkill records a person's skill write: no trace, no session.
func (a *App) emitSkill(ctx context.Context, t event.Type, attrs map[string]any) {
	sinkProxy{a}.Emit(ctx, event.New(t, "", "", "", attrs))
}

// forgetAll drops every cached run set: a write from the API belongs to no
// run, and any cached set may hold the old file.
func (a *App) forgetAll() {
	s := &a.skills
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs, s.order = nil, nil
}

// toGenSkill renders a loaded skill on the wire, without Body or Versions;
// names resolves who taught a saved one. A nil hidden is eligible.
func toGenSkill(ctx context.Context, names *teachers, sk skill.Skill, hidden *skillHidden) gen.Skill {
	out := gen.Skill{
		Name:        sk.Name,
		Description: sk.Description,
		Level:       gen.SkillLevel(sk.Level),
		Eligible:    hidden == nil,
		Hidden:      hidden,
	}
	if l := sk.Learned; l != nil {
		out.Learned = &struct {
			By     string                 `json:"by"`
			ByName *string                `json:"by_name,omitempty"`
			From   string                 `json:"from"`
			On     time.Time              `json:"on"`
			Origin gen.SkillLearnedOrigin `json:"origin"`
		}{
			By: string(l.By), ByName: optString(names.name(ctx, l.By)), From: string(l.From),
			On: l.On.UTC(), Origin: gen.SkillLearnedOrigin(l.Origin),
		}
	}
	if len(sk.Requires.Tools) > 0 || len(sk.Requires.Bins) > 0 {
		req := &struct {
			Bins  *[]string `json:"bins,omitempty"`
			Tools *[]string `json:"tools,omitempty"`
		}{}
		if len(sk.Requires.Tools) > 0 {
			req.Tools = ptrTo(append([]string(nil), sk.Requires.Tools...))
		}
		if len(sk.Requires.Bins) > 0 {
			bins := make([]string, 0, len(sk.Requires.Bins))
			for _, b := range sk.Requires.Bins {
				bins = append(bins, b.Name)
			}
			req.Bins = &bins
		}
		out.Requires = req
	}
	if len(sk.References) > 0 {
		out.References = ptrTo(append([]string(nil), sk.References...))
	}
	if len(sk.Scripts) > 0 {
		out.Scripts = ptrTo(append([]string(nil), sk.Scripts...))
	}
	return out
}

// problemSkill is the row of a folder that did not load: its base name, the
// level (and trash) its directory says, and the problem as Hidden.
func problemSkill(roots skill.Roots, p skill.Problem) gen.Skill {
	out := gen.Skill{
		Hidden: &skillHidden{Reason: gen.SkillHiddenReasonInvalid, Code: ptrTo(string(p.Code)), Message: optString(p.Msg)},
		Name:   path.Base(filepath.ToSlash(p.Dir)),
	}
	switch rel, err := filepath.Rel(absDir(roots.Agent), p.Dir); {
	case strings.HasPrefix(p.Dir, "bundled/"):
		out.Level = gen.SkillLevelBundled
	case roots.Agent != "" && err == nil && strings.HasPrefix(filepath.ToSlash(rel), skill.TrashDir+"/"):
		out.Level, out.Trashed = gen.SkillLevelAgent, ptrTo(true)
	default:
		out.Level = gen.SkillLevelAgent
	}
	return out
}

// skillGroup orders the list: bundled, agent, trashed.
func skillGroup(s gen.Skill) int {
	switch {
	case s.Trashed != nil && *s.Trashed:
		return 2
	case s.Level == gen.SkillLevelBundled:
		return 0
	default:
		return 1
	}
}

func absDir(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func ptrTo[T any](v T) *T { return &v }

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// InstallSkill implements api.SkillBackend (docs/skills.md §7). The source
// is resolved (a git checkout, or an upload) and is either one skill or a
// collection; a skill is a collection of one. Each skill folder is staged
// under the tmp root, scanned, hashed and parsed, and a preview stops there.
// Otherwise the checks run once, before any skill lands: a collection needs
// all, a preview's hash must still match, findings need force, and no name
// may be taken. Then every stage is renamed into place (skill.installed
// each) and a used upload is removed. Every stage not installed is removed
// on every path.
func (a *App) InstallSkill(ctx context.Context, in gen.SkillInstall, who event.ID) (gen.SkillInstalled, error) {
	if err := a.knownSkillAgent(in.Agent); err != nil {
		return gen.SkillInstalled{}, err
	}
	src, err := a.installSource(ctx, in)
	if err != nil {
		return gen.SkillInstalled{}, err
	}
	defer src.close()
	single := hasSkillFile(src.dir)
	items, err := installItems(src.dir, single, in.Name)
	if err != nil {
		return gen.SkillInstalled{}, err
	}
	stages := make([]staged, 0, len(items))
	// Install removes each stage it lands; this is for every other one.
	defer func() {
		for _, st := range stages {
			_ = skill.RemoveStage(st.dir)
		}
	}()
	for _, it := range items {
		st, err := a.stageOne(ctx, in.Agent, filepath.Join(src.dir, filepath.FromSlash(it.path)), it.name)
		if err != nil && !single {
			err = fmt.Errorf("%s: %w", it.path, err)
		}
		if err != nil {
			return gen.SkillInstalled{}, err
		}
		st.path = it.path
		stages = append(stages, st)
	}
	out := installAnswer(stages, single, src.origin.Commit)
	if in.Preview != nil && *in.Preview {
		return out, nil
	}
	if err := a.installChecks(in, stages, single, out.Hash); err != nil {
		return gen.SkillInstalled{}, err
	}
	// Every check has run, so an Install failing here is the filesystem or a
	// race; the skills installed before it stay, and the answer is the error.
	defer a.forgetAll()
	for i, st := range stages {
		if err := a.installStaged(ctx, in.Agent, st, subOrigin(src.origin, st.path), who); err != nil {
			return gen.SkillInstalled{}, err
		}
		if !single {
			(*out.Skills)[i].Installed = true
		}
	}
	src.installed()
	out.Installed = true
	return out, nil
}

// installSource is where an install reads from: dir, the skill or the
// collection, and origin, what origin.toml records of it (Source, and Ref,
// Path and Commit for git). close releases it, and installed says the
// install used it.
type installSource struct {
	dir       string
	origin    skill.Installed
	close     func()
	installed func()
}

// subOrigin is the origin of the skill folder p ("" for the source itself)
// of a source whose origin is o: for git (it has a commit), p joins the
// path in the repository; for an upload, the folder the CLI read.
func subOrigin(o skill.Installed, p string) skill.Installed {
	out := o
	switch {
	case p == "":
	case o.Commit != "":
		out.Path = path.Join(o.Path, p)
	default:
		out.Source = filepath.Join(o.Source, filepath.FromSlash(p))
	}
	return out
}

// installSource resolves in's source: a git checkout under the tmp root,
// removed on close, or an upload, removed once installed. The one place the
// two differ.
func (a *App) installSource(ctx context.Context, in gen.SkillInstall) (installSource, error) {
	switch {
	case in.Git != nil:
		g, err := skill.GitSource{URL: in.Git.Url, Ref: deref(in.Git.Ref), Path: deref(in.Git.Path)}.Validate()
		if err != nil {
			return installSource{}, err
		}
		f, err := skill.Fetch(ctx, a.SkillTmp(), g)
		if err != nil {
			return installSource{}, err
		}
		return installSource{
			dir:    f.Dir,
			origin: skill.Installed{Source: g.URL, Ref: g.Ref, Path: g.Path, Commit: f.Commit},
			close:  f.Close, installed: func() {},
		}, nil
	case in.Upload != nil:
		u, err := skill.OpenUpload(a.SkillTmp(), *in.Upload)
		if err != nil {
			return installSource{}, err
		}
		return installSource{
			dir:    u.Dir,
			origin: skill.Installed{Source: cmp.Or(u.Path, filepath.Base(u.Dir))},
			close:  func() {}, installed: func() { _ = skill.RemoveUpload(a.SkillTmp(), u.ID) },
		}, nil
	}
	return installSource{}, fmt.Errorf("%w: no git and no upload", skill.ErrBadSource)
}

// hasSkillFile reports whether dir holds a SKILL.md: a skill, not a
// collection.
func hasSkillFile(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "SKILL.md"))
	return err == nil
}

// installItem is one skill folder of a source: its slash path inside it
// ("" for a single skill) and the name it installs as.
type installItem struct{ path, name string }

// installItems lists the skills of the source at dir: itself, named name or
// after its folder, or the skill folders skill.Candidates finds, each under
// its own name (name is then ErrCollection).
func installItems(dir string, single bool, name *string) ([]installItem, error) {
	if single {
		n := cmp.Or(deref(name), filepath.Base(dir))
		if !skill.ValidName(n) {
			return nil, fmt.Errorf("%w: %q", skill.ErrBadName, n)
		}
		return []installItem{{name: n}}, nil
	}
	paths, err := skill.Candidates(dir)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("%w: no SKILL.md and no skill folders under it", skill.ErrBadSource)
	}
	if name != nil {
		return nil, fmt.Errorf("%w: a collection installs each skill under its own name", skill.ErrCollection)
	}
	items := make([]installItem, 0, len(paths))
	for _, p := range paths {
		items = append(items, installItem{path: p, name: path.Base(p)})
	}
	return items, nil
}

// installAnswer is the answer over the stages. One skill: its Skill (with
// the body), Findings and Hash. A collection: an entry per skill (path,
// skill without body, findings, hash), with the hash over their hashes and
// no top-level findings. Installed is left false.
func installAnswer(stages []staged, single bool, commit string) gen.SkillInstalled {
	if single {
		out := stages[0].out
		out.Skill.Body = ptrTo(stages[0].body)
		out.Commit = optString(commit)
		return out
	}
	entries := make([]gen.SkillInstalled, 0, len(stages))
	h := sha256.New()
	for i, st := range stages {
		e := st.out
		e.Path = ptrTo(st.path)
		entries = append(entries, e)
		if i > 0 {
			h.Write([]byte("\n"))
		}
		h.Write([]byte(e.Hash))
	}
	return gen.SkillInstalled{
		Findings: []gen.ScanFinding{}, Hash: "sha256:" + hex.EncodeToString(h.Sum(nil)),
		Commit: optString(commit), Skills: &entries,
	}
}

// installChecks refuses an install as a whole, before any skill lands: a
// collection without all, a hash other than the preview's, findings without
// force, and a name already in the skills root.
func (a *App) installChecks(in gen.SkillInstall, stages []staged, single bool, hash string) error {
	if !single && (in.All == nil || !*in.All) {
		return fmt.Errorf("%w: the source holds %d skills; pass all, or name one skill", skill.ErrCollection, len(stages))
	}
	if in.Hash != nil && *in.Hash != hash {
		return fmt.Errorf("%w: %s, previewed %s", skill.ErrChanged, hash, *in.Hash)
	}
	findings := 0
	for _, st := range stages {
		findings += len(st.out.Findings)
	}
	if findings > 0 && (in.Force == nil || !*in.Force) {
		return fmt.Errorf("%w: %d findings", skill.ErrUnsafe, findings)
	}
	// Candidates lists the folders of one directory, so the names differ.
	root := a.SkillRoots(in.Agent).Agent
	for _, st := range stages {
		target := filepath.Join(root, st.name)
		if _, err := os.Lstat(target); err == nil {
			return fmt.Errorf("skill: install %s: %w", target, skill.ErrExists)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// UploadSkill implements api.SkillBackend: the tar r is kept under the tmp
// root for an install to name (skill.SaveUpload).
func (a *App) UploadSkill(_ context.Context, r io.Reader, path string, _ event.ID) (gen.SkillUpload, error) {
	u, err := skill.SaveUpload(a.SkillTmp(), r, path)
	if err != nil {
		return gen.SkillUpload{}, err
	}
	return gen.SkillUpload{Id: u.ID, ExpiresAt: u.Expires}, nil
}

// deref is *p, or the zero value for nil.
func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// staged is one skill folder staged, scanned, hashed and parsed, not yet
// installed: path is its folder in the source ("" for the source itself).
// out has Skill (without Body), Findings and Hash.
type staged struct {
	dir, path, name, body string
	out                   gen.SkillInstalled
}

// stageOne stages the folder from as name under the tmp root, then scans,
// hashes and parses the stage. On error nothing is left staged;
// otherwise the caller removes the stage (skill.RemoveStage) on every path
// that does not install it.
func (a *App) stageOne(ctx context.Context, agentName, from, name string) (st staged, err error) {
	dir, err := skill.Stage(from, a.SkillTmp(), name)
	if err != nil {
		return staged{}, err
	}
	defer func() {
		if err != nil {
			_ = skill.RemoveStage(dir)
		}
	}()
	findings, err := skill.Scan(dir)
	if err != nil {
		return staged{}, err
	}
	hash, err := skill.Hash(dir)
	if err != nil {
		return staged{}, err
	}
	set, probs := skill.Load(skill.Roots{Agent: filepath.Dir(dir)})
	if len(probs) > 0 {
		return staged{}, fmt.Errorf("%w: %v", skill.ErrBadSource, probs[0])
	}
	sk, ok := set.Get(name)
	if !ok {
		return staged{}, fmt.Errorf("%w: no skill %s in the source", skill.ErrBadSource, name)
	}
	g := toGenSkill(ctx, &teachers{a: a}, sk, hiddenOf(skill.Eligible(sk, a.ownerEnv(agentName))))
	out := gen.SkillInstalled{Skill: &g, Findings: make([]gen.ScanFinding, 0, len(findings)), Hash: hash}
	for _, fd := range findings {
		out.Findings = append(out.Findings, gen.ScanFinding{Code: fd.Code, File: fd.File, Line: fd.Line, Message: fd.Msg})
	}
	return staged{dir: dir, name: name, body: sk.Body, out: out}, nil
}

// installStaged renames st into place with its origin (src with st's hash)
// and records skill.installed. The caller drops the run cache.
func (a *App) installStaged(ctx context.Context, agentName string, st staged, src skill.Installed, who event.ID) error {
	src.Hash, src.Installed = st.out.Hash, time.Now().UTC()
	if _, err := skill.Install(st.dir, a.SkillRoots(agentName).Agent, src); err != nil {
		return err
	}
	attrs := map[string]any{
		"agent": agentName, "name": st.name, "source": src.Source, "hash": st.out.Hash,
		"findings": len(st.out.Findings), "who": string(who),
	}
	for k, v := range map[string]string{"ref": src.Ref, "path": src.Path, "commit": src.Commit} {
		if v != "" {
			attrs[k] = v
		}
	}
	a.emitSkill(ctx, event.SkillInstalled, attrs)
	return nil
}
