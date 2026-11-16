package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
	"unicode/utf8"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/skill"
	"holaclaw/internal/tokens"
	"holaclaw/internal/tool"
)

// skill.load reads a skill body or one of its references into the
// conversation (docs/skills.md §4). Which skills exist for the call is the
// daemon's decision, made from the trusted Call: the model names a skill, it
// never widens the set. A body is the operator's or the person's own text, so
// the result is never tainted (docs/security.md). Sizes are the skill
// package's to bound: Parse refuses a body over skill.MaxBody and Reference a
// file over skill.MaxReference, so a load is never truncated.

// SkillSource is the daemon side of the skill tools. Every answer is decided
// from the trusted Call: its agent, sender role and run kind. Skills follow
// the agent, never a person, so the conversation plays no part.
type SkillSource interface {
	// Skills returns the skills eligible for this call. The daemon caches
	// per run, so the tool and the prompt index agree.
	Skills(ctx context.Context, c tool.Call) ([]skill.Skill, error)
	// Teacher resolves the identity that taught a skill (Learned.By) to a
	// display name, keeping store access out of this package; an empty id,
	// or one Teacher cannot resolve, gives an empty name.
	Teacher(ctx context.Context, id event.ID) (string, error)
	// Writer returns the writer over the call's agent skills root.
	// It fails when the call's agent is not configured.
	Writer(c tool.Call) (skill.Writer, error)
}

// RegisterSkill adds skill.load, skill.save and skill.forget. sink may be
// nil; it receives skill.loaded, skill.saved and skill.forgotten.
func RegisterSkill(r *tool.Registry, src SkillSource, sink event.Sink) error {
	if src == nil {
		return errors.New("tools: RegisterSkill needs a skill source")
	}
	if sink == nil {
		sink = event.Discard
	}
	for _, t := range []tool.Tool{
		&skillLoad{src: src, sink: sink},
		&skillSave{src: src, sink: sink, now: time.Now},
		&skillForget{src: src, sink: sink},
	} {
		if err := r.Register(t); err != nil {
			return err
		}
	}
	return nil
}

type skillLoad struct {
	src  SkillSource
	sink event.Sink
}

func (*skillLoad) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "skill.load",
		Description: "Load a skill from the skills list by name. file reads one of its references.",
		Parameters: object{
			Properties: map[string]prop{"name": {Type: tString}, "file": {Type: tString}},
			Required:   []string{"name"},
		}.raw(),
	}
}

func (*skillLoad) Risk() tool.Risk { return tool.RiskRead }

func (t *skillLoad) Run(ctx context.Context, c tool.Call) (tool.Result, error) {
	var a struct {
		Name string `json:"name"`
		File string `json:"file"`
	}
	if err := decodeArgs(c.Args, &a); err != nil {
		return errf("bad arguments: %v", err), nil
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return errf("name is required: one of the skills listed in the prompt"), nil
	}
	skills, err := t.src.Skills(ctx, c)
	if err != nil {
		return errf("skills are unavailable right now; continue without one"), nil
	}
	sk, found := findSkill(skills, name)
	if !found {
		return errf("no skill %q here; use a name from the skills list", name), nil
	}

	var teacher string
	if sk.Learned != nil {
		if teacher, err = t.src.Teacher(ctx, sk.Learned.By); err != nil {
			teacher = "" // unresolved: the provenance line falls back to the date only
		}
	}
	provenance := skill.Provenance(sk, teacher)

	var text string
	file := strings.TrimPrefix(strings.TrimSpace(a.File), "references/")
	if file == "" {
		text = provenance + "\n\n" + sk.Body
	} else {
		b, err := sk.Reference(file)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return errf("skill %s has no file references/%s", name, file), nil
		case errors.Is(err, skill.ErrBadReference):
			return errf("file must be a relative path under references/, e.g. \"checklist.md\""), nil
		case errors.Is(err, skill.ErrTooLarge):
			return errf("references/%s is over %d KiB, too large to load", file, skill.MaxReference>>10), nil
		case err != nil:
			return errf("skill.load failed: cannot read references/%s", file), nil
		}
		if !utf8.Valid(b) {
			return errf("references/%s is not text", file), nil
		}
		text = "[skill: " + name + " · references/" + file + "]\n\n" + string(b)
	}

	attrs := map[string]any{
		"agent":       c.Agent,
		"name":        sk.Name,
		"level":       string(sk.Level),
		"body_tokens": tokens.Estimate(sk.Body),
		"provenance":  provenance,
	}
	if file != "" {
		attrs["file"] = file
		attrs["body_tokens"] = tokens.Estimate(text)
	}
	t.sink.Emit(ctx, event.New(event.SkillLoaded, c.TraceID, c.SessionID, "", attrs))
	return tool.Result{Text: text}, nil
}

// skill.save and skill.forget write the agent's skills (docs/skills.md §5).
// The model supplies the draft only: the teacher is the call's user, the
// provenance its trace, the date the tool's clock, and the daemon renders,
// lints and versions the file. A skill changes the agent for everyone it
// serves, so only an owner or admin writes one; the role on the call is the
// sender's, the strictest one in a coalesced turn. A task run never writes:
// it has no person to correct it, so a discovery there is offered, not saved.
// The loop enforces "last call of a turn".

// skillSave renders and writes one agent skill.
type skillSave struct {
	src  SkillSource
	sink event.Sink
	now  func() time.Time
}

func (*skillSave) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "skill.save",
		Description: "Save a procedure the person corrected or asked to keep, as the last call of the turn.",
		// Every field is needed, but a "required" list and string types do not
		// fit under tool.MaxSchemaTokens: Run reports every missing or wrong
		// field in one error instead.
		Parameters: object{Properties: map[string]prop{
			"name":     {},
			"title":    {},
			"when":     {},
			"steps":    {Type: tArray, Items: &prop{}},
			"pitfalls": {Type: tArray, Items: &prop{}},
			"origin":   {Enum: []string{"correction", "explicit"}},
		}}.raw(),
	}
}

func (*skillSave) Risk() tool.Risk { return tool.RiskWrite }

func (t *skillSave) Run(ctx context.Context, c tool.Call) (tool.Result, error) {
	var a struct {
		Name     string   `json:"name"`
		Title    string   `json:"title"`
		When     string   `json:"when"`
		Steps    []string `json:"steps"`
		Pitfalls []string `json:"pitfalls"`
		Origin   string   `json:"origin"`
	}
	if err := decodeArgs(c.Args, &a); err != nil {
		return errf("bad arguments: %v", err), nil
	}
	d := skill.Draft{
		Name:     strings.TrimSpace(a.Name),
		Title:    strings.TrimSpace(a.Title),
		When:     strings.TrimSpace(a.When),
		Steps:    trimAll(a.Steps),
		Pitfalls: trimAll(a.Pitfalls),
	}
	if r, ok := refuseSkillWrite(c, "teach"); !ok {
		return r, nil
	}
	var lines []string
	origin := skill.Origin(strings.TrimSpace(a.Origin))
	if origin != skill.OriginCorrection && origin != skill.OriginExplicit {
		lines = append(lines, `origin: must be "correction" (the person said how it should be done) or "explicit" (they asked to keep it)`)
	}
	for _, e := range skill.Lint(d) {
		lines = append(lines, e.Error())
	}
	if len(lines) > 0 {
		return errf("skill not saved; fix every problem and call skill.save again:\n%s", strings.Join(lines, "\n")), nil
	}

	// A bundled skill of the same name is shadowed, never written. An agent
	// skill is replaced, its current file kept in history, and its requires
	// carry over unchanged: saving never widens them (docs/skills.md §8).
	w, err := t.src.Writer(c)
	if err != nil {
		return errf("skill.save failed: %v", err), nil
	}
	cur, exists, err := w.Current(d.Name)
	switch {
	case errors.Is(err, skill.ErrUnsafePath):
		return errf("the folder of skill %q is not safe to write (a symlink); a person must fix it", d.Name), nil
	case err != nil && !exists:
		return errf("skill.save failed: %v", err), nil
	case err != nil:
		return errf("skill %q cannot be read, so it cannot be replaced; a person must fix it", d.Name), nil
	}

	l := skill.Learned{On: t.now().UTC().Truncate(time.Second), By: c.UserID, From: c.TraceID, Origin: origin}
	version, err := w.Next(d.Name)
	if err != nil {
		return errf("skill.save failed: %v", err), nil
	}
	b, err := skill.Render(d, l, cur.Requires, version)
	if err != nil {
		return errf("skill.save failed: %v", err), nil
	}
	saved, err := w.Save(d.Name, b)
	if err != nil {
		return errf("skill.save failed: %v", err), nil
	}
	_, body, _ := strings.Cut(strings.TrimPrefix(string(b), "---\n"), "\n---\n")
	t.sink.Emit(ctx, event.New(event.SkillSaved, c.TraceID, c.SessionID, "", map[string]any{
		"agent":       c.Agent,
		"name":        d.Name,
		"by":          string(c.UserID),
		"origin":      string(origin),
		"version":     saved.Version,
		"replaced":    saved.Replaced,
		"versions":    saved.Versions,
		"body_tokens": tokens.Estimate(body),
	}))
	how := fmt.Sprintf("version %d (new)", saved.Version)
	if saved.Replaced != 0 {
		how = fmt.Sprintf("version %d (replaced version %d)", saved.Version, saved.Replaced)
	}
	return tool.Result{Text: "saved skill " + d.Name + " " + how + "\nloaded from now on when it matches"}, nil
}

// skillForget moves one agent skill, whole, to the trash.
type skillForget struct {
	src  SkillSource
	sink event.Sink
}

func (*skillForget) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "skill.forget",
		Description: "Forget a skill by name when the person asks.",
		Parameters: object{
			Properties: map[string]prop{"name": {Type: tString}},
			Required:   []string{"name"},
		}.raw(),
	}
}

func (*skillForget) Risk() tool.Risk { return tool.RiskWrite }

func (t *skillForget) Run(ctx context.Context, c tool.Call) (tool.Result, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := decodeArgs(c.Args, &a); err != nil {
		return errf("bad arguments: %v", err), nil
	}
	name := strings.TrimSpace(a.Name)
	if name == "" {
		return errf("name is required"), nil
	}
	if r, ok := refuseSkillWrite(c, "forget"); !ok {
		return r, nil
	}
	w, err := t.src.Writer(c)
	if err != nil {
		return errf("skill.forget failed: %v", err), nil
	}
	versions, err := w.Forget(name)
	switch {
	case errors.Is(err, skill.ErrNotFound), errors.Is(err, skill.ErrBadName):
		// No agent skill. What the person asked for holds, the skill is not
		// used, unless a bundled one of that name still is.
		skills, serr := t.src.Skills(ctx, c)
		if serr != nil {
			return errf("skills are unavailable right now; nothing was forgotten"), nil
		}
		if sk, ok := findSkill(skills, name); ok && sk.Level == skill.LevelBundled {
			return errf("skill %q is bundled with holaclaw; it cannot be forgotten", name), nil
		}
		return tool.Result{Text: fmt.Sprintf("no skill %q; nothing to forget", name)}, nil
	case errors.Is(err, skill.ErrUnsafePath):
		return errf("the folder of skill %q is not safe to write (a symlink); a person must fix it", name), nil
	case err != nil:
		return errf("skill.forget failed: %v", err), nil
	}
	t.sink.Emit(ctx, event.New(event.SkillForgotten, c.TraceID, c.SessionID, "", map[string]any{
		"agent":    c.Agent,
		"name":     name,
		"versions": versions,
	}))
	return tool.Result{Text: "forgotten skill " + name}, nil
}

// refuseSkillWrite applies the rules every skill write shares: no task run,
// and a sender who is an owner or admin. verb names the act in the error.
func refuseSkillWrite(c tool.Call, verb string) (tool.Result, bool) {
	switch {
	case c.RunKind == tool.RunTask:
		return errf("skills are saved and forgotten in a conversation, not a task"), false
	case c.Role != tool.RoleOwner && c.Role != tool.RoleAdmin:
		return errf("only an owner or admin can %s a skill", verb), false
	}
	return tool.Result{}, true
}

// findSkill returns the skill named name in an eligible set.
func findSkill(skills []skill.Skill, name string) (skill.Skill, bool) {
	for _, s := range skills {
		if s.Name == name {
			return s, true
		}
	}
	return skill.Skill{}, false
}

// trimAll trims every string; nil stays nil.
func trimAll(ss []string) []string {
	if ss == nil {
		return nil
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.TrimSpace(s)
	}
	return out
}
