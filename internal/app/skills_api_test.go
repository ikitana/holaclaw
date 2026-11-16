package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
	"holaclaw/internal/fsutil"
	"holaclaw/internal/skill"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
)

// teacherID is the user who taught "deploy" in apiApp.
const teacherID event.ID = "01J9Q4ABCDEFGHJKMNPQRSTVWZ"

// apiApp is skillApp with a store as the event sink and a teacher named
// Angel, the agent skill "deploy" saved twice by Angel (one version in
// history), and "gone" in the trash.
func apiApp(t *testing.T) (*App, *store.SQLite) {
	t.Helper()
	ctx := context.Background()
	a, _ := skillApp(t)
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a.Store, a.Events = st, st
	if err := st.CreateIdentity(ctx, store.Identity{ID: teacherID, Name: "Angel", Role: store.RoleOwner, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	w := skill.Writer{Root: a.SkillRoots("main").Agent}
	for _, step := range []string{"First way.", "Second way."} {
		if _, err := w.Save("deploy", learnedBytes(t, "deploy", step)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Save("gone", learnedBytes(t, "gone", "Old.")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Forget("gone"); err != nil {
		t.Fatal(err)
	}
	return a, st
}

func learnedBytes(t *testing.T, name, step string) []byte {
	t.Helper()
	b, err := skill.Render(skill.Draft{
		Name: name, Title: "Title", When: "When doing " + name + ".", Steps: []string{step, "Check it."},
	}, skill.Learned{
		On: time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC), By: teacherID,
		From: "01J9Q4ABCDEFGHJKMNPQRSTVWX", Origin: skill.OriginCorrection,
	}, skill.Requires{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// skillEvents returns the stored events of type typ, newest first.
func skillEvents(t *testing.T, st *store.SQLite, typ event.Type) []map[string]any {
	t.Helper()
	evs, err := st.ListEventsAfter(context.Background(), "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, e := range slices.Backward(evs) {
		if e.Type != typ {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(e.Attrs, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func TestListSkills(t *testing.T) {
	a, _ := apiApp(t)
	got, err := a.ListSkills(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	var rows []string
	by := map[string]gen.Skill{}
	for _, s := range got {
		k := string(s.Level) + ":" + s.Name
		if s.Trashed != nil && *s.Trashed {
			k = "trash:" + k
		}
		rows = append(rows, k)
		by[k] = s
		if s.Body != nil || s.Versions != nil {
			t.Errorf("%s: body or versions in the list", k)
		}
	}
	want := "agent:broken agent:deploy agent:forgets agent:mine agent:off agent:plain agent:with-exec " +
		"agent:with-files agent:with-search trash:agent:gone"
	if strings.Join(rows, " ") != want {
		t.Fatalf("rows =\n%s\nwant\n%s", strings.Join(rows, " "), want)
	}
	for k, w := range map[string]struct {
		ok     bool
		hidden *skillHidden
	}{
		"agent:plain":      {true, nil},
		"agent:with-exec":  {false, &skillHidden{Reason: gen.SkillHiddenReasonDeniedTool, Subject: ptrTo("exec"), Message: ptrTo("tool exec is denied for this role")}},
		"agent:forgets":    {false, &skillHidden{Reason: gen.SkillHiddenReasonDeniedTool, Subject: ptrTo("memory.forget"), Message: ptrTo("tool memory.forget is denied for this role")}},
		"agent:off":        {false, &skillHidden{Reason: gen.SkillHiddenReasonDisabled, Message: ptrTo("disabled")}},
		"agent:broken":     {false, &skillHidden{Reason: gen.SkillHiddenReasonInvalid, Code: ptrTo(string(skill.CodeNoFrontmatter))}},
		"agent:mine":       {true, nil},
		"agent:deploy":     {true, nil},
		"trash:agent:gone": {false, &skillHidden{Reason: gen.SkillHiddenReasonTrashed, Message: ptrTo("in the trash")}},
	} {
		s := by[k]
		if s.Eligible != w.ok || !sameHidden(s.Hidden, w.hidden) {
			t.Errorf("%s: eligible %v hidden %s, want %v %s", k, s.Eligible, hiddenJSON(s.Hidden), w.ok, hiddenJSON(w.hidden))
		}
	}
	if b := by["agent:broken"].Hidden; b == nil || str(b.Message) == "" {
		t.Errorf("broken: no message: %s", hiddenJSON(b))
	}
	d := by["agent:deploy"]
	if d.Learned == nil || d.Learned.Origin != gen.Correction || d.Learned.On.Format(time.DateOnly) != "2026-09-12" ||
		d.Learned.From != "01J9Q4ABCDEFGHJKMNPQRSTVWX" || d.Learned.By != string(teacherID) || str(d.Learned.ByName) != "Angel" {
		t.Errorf("learned = %+v", d.Learned)
	}
	if g := by["trash:agent:gone"]; g.Learned == nil || str(g.Learned.ByName) != "Angel" {
		t.Errorf("trashed learned = %+v", g.Learned)
	}
	// A teacher the store does not know: the id, no name.
	if m := by["agent:mine"]; m.Learned == nil || m.Learned.By != "01J9Q4ABCDEFGHJKMNPQRSTVWX" || m.Learned.ByName != nil {
		t.Errorf("unknown teacher = %+v", m.Learned)
	}
	if p := by["agent:plain"]; p.Learned != nil {
		t.Errorf("hand-written skill has learned: %+v", p.Learned)
	}
	if f := by["agent:with-files"]; f.Requires == nil || f.Requires.Tools == nil || (*f.Requires.Tools)[0] != "files" {
		t.Errorf("requires = %+v", f.Requires)
	}
	// The live number: deploy was saved twice, the others once; a row that
	// did not load has none.
	for k, want := range map[string]int{"agent:deploy": 2, "agent:plain": 1, "trash:agent:gone": 1} {
		if v := by[k].Version; v == nil || *v != want {
			t.Errorf("%s: version %v, want %d", k, v, want)
		}
	}
	if v := by["agent:broken"].Version; v != nil {
		t.Errorf("problem row: version %d", *v)
	}
	if _, err := a.ListSkills(context.Background(), "nobody"); !errors.Is(err, skill.ErrNotFound) {
		t.Errorf("unknown agent: %v", err)
	}
}

func TestListSkillsTrashedProblem(t *testing.T) {
	a, _ := apiApp(t)
	dir := filepath.Join(a.SkillRoots("main").Agent, skill.TrashDir, "bad")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := a.ListSkills(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	last := got[len(got)-2] // the trash group, by name: bad, gone
	if last.Name != "bad" || last.Trashed == nil || last.Level != gen.SkillLevelAgent || last.Eligible ||
		!sameHidden(last.Hidden, &skillHidden{Reason: gen.SkillHiddenReasonInvalid, Code: ptrTo(string(skill.CodeNoFrontmatter))}) {
		t.Fatalf("last = %+v", last)
	}
}

func TestGetSkill(t *testing.T) {
	ctx := context.Background()
	a, _ := apiApp(t)
	s, err := a.GetSkill(ctx, "main", "plain")
	if err != nil || s.Level != gen.SkillLevelAgent || !strings.Contains(str(s.Body), "# plain") ||
		s.Versions == nil || len(*s.Versions) != 0 || s.Learned != nil || s.Version == nil || *s.Version != 1 {
		t.Fatalf("hand-written = %+v, %v", s, err)
	}
	s, err = a.GetSkill(ctx, "main", "deploy")
	if err != nil || !strings.Contains(str(s.Body), "Second way.") || s.Versions == nil || len(*s.Versions) != 1 ||
		s.Trashed != nil || s.Learned == nil || str(s.Learned.ByName) != "Angel" || s.Version == nil || *s.Version != 2 {
		t.Fatalf("saved = %+v, %v", s, err)
	}
	s, err = a.GetSkill(ctx, "main", "gone")
	if err != nil || s.Trashed == nil || s.Eligible || !sameHidden(s.Hidden, hiddenTrashed()) || s.Versions == nil || len(*s.Versions) != 0 ||
		s.Version == nil || *s.Version != 1 {
		t.Fatalf("trashed = %+v, %v", s, err)
	}
	if _, err := a.GetSkill(ctx, "main", "missing"); !errors.Is(err, skill.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := a.GetSkill(ctx, "nobody", "plain"); !errors.Is(err, skill.ErrNotFound) {
		t.Errorf("unknown agent: %v", err)
	}
	// A history/ that cannot be read fails the get.
	if err := os.WriteFile(filepath.Join(a.SkillRoots("main").Agent, "plain", "history"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetSkill(ctx, "main", "plain"); err == nil {
		t.Error("history a file: no error")
	}
}

func TestGetSkillVersionAndRaw(t *testing.T) {
	ctx := context.Background()
	a, _ := apiApp(t)
	live, err := a.GetSkill(ctx, "main", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if raw := str(live.Raw); !strings.HasPrefix(raw, "---\n") || !strings.Contains(raw, "Second way.") {
		t.Fatalf("raw = %q", raw)
	}
	vs := *live.Versions
	if len(vs) != 1 || str(vs[0].Trace) != "01J9Q4ABCDEFGHJKMNPQRSTVWX" || vs[0].On == nil ||
		str(vs[0].By) != string(teacherID) || str(vs[0].ByName) != "Angel" {
		t.Fatalf("versions = %+v", vs)
	}
	// One version: its file, on the live skill's row.
	v, err := a.GetSkillVersion(ctx, "main", "deploy", vs[0].Version)
	if err != nil || !strings.Contains(str(v.Body), "First way.") || !strings.Contains(str(v.Raw), "First way.") || v.Learned == nil {
		t.Fatalf("version = %+v, %v", v, err)
	}
	if v.Versions == nil || len(*v.Versions) != 1 || v.Eligible != live.Eligible || v.Name != "deploy" {
		t.Errorf("a version's row lost the live list or the eligibility: %+v", v)
	}
	if v.Version == nil || *v.Version != vs[0].Version {
		t.Errorf("a version's row: version %v, want %d", v.Version, vs[0].Version)
	}
	// The live number is the live skill, as GetSkill has it.
	if *live.Version != 2 {
		t.Fatalf("live version = %d", *live.Version)
	}
	if l, err := a.GetSkillVersion(ctx, "main", "deploy", *live.Version); err != nil || str(l.Raw) != str(live.Raw) ||
		*l.Version != *live.Version || len(*l.Versions) != 1 {
		t.Fatalf("live by number = %+v, %v", l, err)
	}
	// Past the live number, and below 1, there is nothing.
	for _, bad := range []int{0, -1, 3, 99} {
		if _, err := a.GetSkillVersion(ctx, "main", "deploy", bad); !errors.Is(err, skill.ErrNotFound) {
			t.Errorf("version %d: %v", bad, err)
		}
	}
	// A trashed skill is read by number too.
	if g, err := a.GetSkillVersion(ctx, "main", "gone", 1); err != nil || g.Trashed == nil || !strings.Contains(str(g.Body), "Old.") {
		t.Errorf("trashed live = %+v, %v", g, err)
	}
	if _, err := a.GetSkillVersion(ctx, "main", "missing", 1); !errors.Is(err, skill.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	// A version a person wrote has no learned block: the name alone.
	plain := filepath.Join(a.SkillRoots("main").Agent, "plain")
	before, err := os.ReadFile(filepath.Join(plain, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (skill.Writer{Root: a.SkillRoots("main").Agent}).Save("plain", before); err != nil {
		t.Fatal(err)
	}
	p, err := a.GetSkill(ctx, "main", "plain")
	if err != nil || str(p.Raw) != string(before) || p.Versions == nil || len(*p.Versions) != 1 {
		t.Fatalf("hand-written = %+v, %v", p, err)
	}
	if r := (*p.Versions)[0]; r.Trace != nil || r.By != nil || r.On != nil {
		t.Errorf("a person's version = %+v", r)
	}
	// A bundled skill has a raw file and no versions; a version there is not found.
	old := skill.Bundled
	t.Cleanup(func() { skill.Bundled = old })
	raw := "---\nname: summarize\ndescription: When summarizing.\n---\n# Summarize\n"
	skill.Bundled = fstest.MapFS{"summarize/SKILL.md": {Data: []byte(raw)}}
	if b, err := a.GetSkill(ctx, "main", "summarize"); err != nil || str(b.Raw) != raw || b.Versions != nil || b.Version != nil {
		t.Fatalf("bundled = %+v, %v", b, err)
	}
	for _, n := range []int{1, vs[0].Version} {
		if _, err := a.GetSkillVersion(ctx, "main", "summarize", n); !errors.Is(err, skill.ErrNotFound) {
			t.Errorf("a bundled version %d: %v", n, err)
		}
	}
	// A version that no longer parses is listed by number and shown raw.
	dir := filepath.Join(a.SkillRoots("main").Agent, "deploy", "history")
	if err := os.WriteFile(filepath.Join(dir, strconv.Itoa(vs[0].Version), "SKILL.md"), []byte("broken\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	live, err = a.GetSkill(ctx, "main", "deploy")
	if err != nil || len(*live.Versions) != 1 || (*live.Versions)[0].Trace != nil {
		t.Fatalf("after breaking the version: %+v, %v", live, err)
	}
	v, err = a.GetSkillVersion(ctx, "main", "deploy", vs[0].Version)
	if h := v.Hidden; err != nil || str(v.Raw) != "broken\n" || h == nil || h.Reason != gen.SkillHiddenReasonInvalid || str(h.Code) != string(skill.CodeNoFrontmatter) ||
		v.Version == nil || *v.Version != vs[0].Version {
		t.Fatalf("broken version = %+v, %v", v, err)
	}
}

// Remove on a live skill trashes it; on a trashed one it deletes for good.
func TestRemoveSkill(t *testing.T) {
	ctx := context.Background()
	a, st := apiApp(t)
	root := a.SkillRoots("main").Agent
	if err := a.RemoveSkill(ctx, "main", "deploy", "U9"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, skill.TrashDir, "deploy", "SKILL.md")); err != nil {
		t.Fatalf("not trashed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "deploy")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("still live: %v", err)
	}
	evs := skillEvents(t, st, event.SkillForgotten)
	if len(evs) != 1 || evs[0]["who"] != "U9" || evs[0]["agent"] != "main" || evs[0]["name"] != "deploy" ||
		evs[0]["versions"] != float64(1) {
		t.Fatalf("events = %v", evs)
	}
	if _, ok := evs[0]["space"]; ok {
		t.Errorf("space attr: %v", evs[0])
	}
	// Already in the trash: gone for good, no event.
	if err := a.RemoveSkill(ctx, "main", "deploy", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, skill.TrashDir, "deploy")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("still there: %v", err)
	}
	if evs := skillEvents(t, st, event.SkillForgotten); len(evs) != 1 {
		t.Fatalf("events = %v", evs)
	}
	for _, c := range []struct {
		name string
		want error
	}{
		{"deploy", skill.ErrNotFound},
		{"missing", skill.ErrNotFound},
		{"../x", skill.ErrBadName},
	} {
		if err := a.RemoveSkill(ctx, "main", c.name, "admin"); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	if err := a.RemoveSkill(ctx, "nobody", "deploy", "admin"); !errors.Is(err, skill.ErrNotFound) {
		t.Errorf("unknown agent: %v", err)
	}
}

func TestRestoreSkill(t *testing.T) {
	ctx := context.Background()
	a, st := apiApp(t)
	s, err := a.RestoreSkill(ctx, "main", "gone", "admin")
	if err != nil || s.Trashed != nil || s.Name != "gone" || !s.Eligible {
		t.Fatalf("restored = %+v, %v", s, err)
	}
	evs := skillEvents(t, st, event.SkillRestored)
	if len(evs) != 1 || evs[0]["who"] != "admin" || evs[0]["agent"] != "main" || evs[0]["name"] != "gone" {
		t.Fatalf("events = %v", evs)
	}
	if _, ok := evs[0]["space"]; ok {
		t.Errorf("space attr: %v", evs[0])
	}
	// Over a live skill: conflict, both kept.
	if err := a.RemoveSkill(ctx, "main", "deploy", "admin"); err != nil {
		t.Fatal(err)
	}
	w := skill.Writer{Root: a.SkillRoots("main").Agent}
	if _, err := w.Save("deploy", learnedBytes(t, "deploy", "Third way.")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RestoreSkill(ctx, "main", "deploy", "admin"); !errors.Is(err, skill.ErrExists) {
		t.Fatalf("over live: %v", err)
	}
	if s, _ := a.GetSkill(ctx, "main", "deploy"); !strings.Contains(str(s.Body), "Third way.") {
		t.Errorf("live replaced: %+v", s)
	}
	if _, err := a.RestoreSkill(ctx, "main", "missing", "admin"); !errors.Is(err, skill.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if evs := skillEvents(t, st, event.SkillRestored); len(evs) != 1 {
		t.Errorf("events = %v", evs)
	}
}

func TestRevertSkill(t *testing.T) {
	ctx := context.Background()
	a, st := apiApp(t)
	before, err := a.GetSkill(ctx, "main", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	// No version named, or the live number: a no-op that returns the live
	// skill, and no event.
	for _, c := range []struct {
		name string
		to   *int
	}{{"deploy", nil}, {"plain", nil}, {"deploy", ptrTo(2)}, {"plain", ptrTo(1)}} {
		s, err := a.RevertSkill(ctx, "main", c.name, c.to, "U9")
		if err != nil || s.Name != c.name || s.Body == nil || s.Version == nil {
			t.Fatalf("%s: no-op = %+v, %v", c.name, s, err)
		}
	}
	if s, _ := a.GetSkill(ctx, "main", "deploy"); str(s.Body) != str(before.Body) || len(*s.Versions) != len(*before.Versions) || *s.Version != 2 {
		t.Fatalf("no-op wrote: %+v", s)
	}
	if evs := skillEvents(t, st, event.SkillReverted); len(evs) != 0 {
		t.Fatalf("no-op events = %v", evs)
	}
	// The newest earlier version.
	s, err := a.RevertSkill(ctx, "main", "deploy", &(*before.Versions)[0].Version, "U9")
	if err != nil || !strings.Contains(str(s.Body), "First way.") || len(*s.Versions) != 2 || *s.Version != 3 {
		t.Fatalf("reverted = %+v, %v", s, err)
	}
	evs := skillEvents(t, st, event.SkillReverted)
	if len(evs) != 1 || evs[0]["to"] != float64((*before.Versions)[0].Version) || evs[0]["from"] != float64(2) || evs[0]["version"] != float64(3) || evs[0]["who"] != "U9" ||
		evs[0]["agent"] != "main" || evs[0]["name"] != "deploy" {
		t.Fatalf("events = %v", evs)
	}
	if _, ok := evs[0]["space"]; ok {
		t.Errorf("space attr: %v", evs[0])
	}
	// The one the first revert retired.
	from := int(evs[0]["from"].(float64))
	s, err = a.RevertSkill(ctx, "main", "deploy", &from, "admin")
	if err != nil || !strings.Contains(str(s.Body), "Second way.") {
		t.Fatalf("named = %+v, %v", s, err)
	}
	if evs := skillEvents(t, st, event.SkillReverted); len(evs) != 2 || evs[0]["to"] != float64(from) {
		t.Fatalf("events = %v", evs)
	}
	for _, c := range []struct {
		agent, name string
		to          *int
		want        error
	}{
		{"main", "deploy", ptrTo(99), skill.ErrNotFound},
		{"main", "deploy", ptrTo(5), skill.ErrNotFound}, // one past the live number
		{"main", "deploy", ptrTo(0), skill.ErrNotFound},
		{"main", "deploy", ptrTo(-1), skill.ErrNotFound},
		{"main", "gone", ptrTo(1), skill.ErrNotFound},
		{"main", "plain", ptrTo(2), skill.ErrNotFound},
		{"main", "missing", ptrTo(1), skill.ErrNotFound},
		{"main", "gone", nil, skill.ErrNotFound},
		{"main", "missing", nil, skill.ErrNotFound},
		{"main", "../x", nil, skill.ErrBadName},
		{"main", "../x", ptrTo(1), skill.ErrBadName},
		{"nobody", "deploy", nil, skill.ErrNotFound},
		{"nobody", "deploy", ptrTo(1), skill.ErrNotFound},
	} {
		if _, err := a.RevertSkill(ctx, c.agent, c.name, c.to, "admin"); !errors.Is(err, c.want) {
			t.Errorf("%v: %v", c, err)
		}
	}
	if evs := skillEvents(t, st, event.SkillReverted); len(evs) != 2 {
		t.Fatalf("events after failures = %v", evs)
	}
}

// A bundled skill is never written: remove and revert refuse it, and an
// agent skill that shadows it is written as usual.
func TestSkillAPIBundled(t *testing.T) {
	ctx := context.Background()
	a, _ := apiApp(t)
	old := skill.Bundled
	t.Cleanup(func() { skill.Bundled = old })
	skill.Bundled = fstest.MapFS{
		"summarize/SKILL.md": {Data: []byte("---\nname: summarize\ndescription: When summarizing.\n---\n# Summarize\n")},
	}
	s, err := a.GetSkill(ctx, "main", "summarize")
	if err != nil || s.Level != gen.SkillLevelBundled || s.Versions != nil || s.Version != nil {
		t.Fatalf("bundled = %+v, %v", s, err)
	}
	list, err := a.ListSkills(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range list {
		if r.Name == "summarize" && r.Version != nil {
			t.Errorf("bundled row: version %d", *r.Version)
		}
	}
	if err := a.RemoveSkill(ctx, "main", "summarize", "admin"); !errors.Is(err, skill.ErrBundled) {
		t.Errorf("remove: %v", err)
	}
	for _, to := range []*int{nil, ptrTo(0), ptrTo(1), ptrTo(2)} {
		if _, err := a.RevertSkill(ctx, "main", "summarize", to, "admin"); !errors.Is(err, skill.ErrBundled) {
			t.Errorf("revert to %v: %v", to, err)
		}
	}
	if _, err := a.RestoreSkill(ctx, "main", "summarize", "admin"); !errors.Is(err, skill.ErrNotFound) {
		t.Errorf("restore: %v", err)
	}
}

// A write from the API drops every cached run set.
func TestSkillAPIWriteDropsCache(t *testing.T) {
	ctx := context.Background()
	a, _ := apiApp(t)
	a.RunSkills(skillCall("T1"))
	if len(a.skills.runs) != 1 {
		t.Fatal("not cached")
	}
	if err := a.RemoveSkill(ctx, "main", "deploy", "admin"); err != nil {
		t.Fatal(err)
	}
	if len(a.skills.runs) != 0 || len(a.skills.order) != 0 {
		t.Fatalf("cache kept: %d", len(a.skills.runs))
	}
	if got := names(a.RunSkills(skillCall("T1"))); strings.Contains(got, "deploy") {
		t.Fatalf("still eligible: %s", got)
	}
}

func skillCall(trace event.ID) tool.Call {
	return tool.Call{Agent: "main", Role: "owner", RunKind: tool.RunInteractive, TraceID: trace}
}

// sameHidden compares two Hiddens field by field; a want without a message
// matches any.
func sameHidden(got, want *skillHidden) bool {
	if got == nil || want == nil {
		return got == want
	}
	return got.Reason == want.Reason && str(got.Subject) == str(want.Subject) && str(got.Code) == str(want.Code) &&
		(want.Message == nil || str(got.Message) == str(want.Message))
}

func hiddenJSON(h *skillHidden) string {
	b, _ := json.Marshal(h)
	return string(b)
}

// Each eligibility reason reaches the wire typed, with its subject and the
// reason in words; every kind Eligible returns is one the spec knows.
func TestListSkillsHiddenReasons(t *testing.T) {
	a, _ := apiApp(t)
	root := a.SkillRoots("main").Agent
	writeSkill(t, root, "needs-browser", "metadata:\n  holaclaw:\n    version: 1\n    requires:\n      tools: [browser]\n")
	writeSkill(t, root, "needs-gh", "metadata:\n  holaclaw:\n    version: 1\n    requires:\n      bins: [gh]\n")
	got, err := a.ListSkills(context.Background(), "main")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]gen.Skill{}
	for _, s := range got {
		by[s.Name] = s
	}
	for name, want := range map[string]*skillHidden{
		"needs-browser": {Reason: gen.SkillHiddenReasonMissingTool, Subject: ptrTo("browser"), Message: ptrTo("requires tool browser, not in the agent's tools")},
		"needs-gh":      {Reason: gen.SkillHiddenReasonNeedsBinary, Subject: ptrTo("gh"), Message: ptrTo("requires binary gh (binaries are not supported yet)")},
	} {
		if s := by[name]; s.Eligible || !sameHidden(s.Hidden, want) {
			t.Errorf("%s: eligible %v hidden %s, want %s", name, s.Eligible, hiddenJSON(s.Hidden), hiddenJSON(want))
		}
	}
	for _, k := range []skill.ReasonKind{skill.ReasonMissingTool, skill.ReasonDeniedTool, skill.ReasonNeedsBinary, skill.ReasonDisabled} {
		if !gen.SkillHiddenReason(k).Valid() {
			t.Errorf("%q is not in the spec's enum", k)
		}
	}
	if h := hiddenOf(true, skill.Reason{}); h != nil {
		t.Errorf("eligible has hidden %s", hiddenJSON(h))
	}
}

// installSrc is a local skill folder <tmp>/<name> whose body is body.
func installSrc(t *testing.T, name, body string) string {
	t.Helper()
	dir := t.TempDir()
	writeSkill(t, dir, name, "")
	p := filepath.Join(dir, name)
	if body != "" {
		if err := os.WriteFile(filepath.Join(p, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: When shipping.\n---\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

// upload sends the folder dir to a as the CLI does, and returns the id.
func upload(t *testing.T, a *App, dir string) *string {
	t.Helper()
	var buf bytes.Buffer
	if err := fsutil.WriteTarGz(&buf, dir, fsutil.CopyOptions{}); err != nil {
		t.Fatal(err)
	}
	u, err := a.UploadSkill(context.Background(), &buf, dir, "admin")
	if err != nil {
		t.Fatal(err)
	}
	return &u.Id
}

// noStages fails when a stage is left under the tmp root.
func noStages(t *testing.T, a *App) {
	t.Helper()
	if m, _ := filepath.Glob(filepath.Join(a.SkillTmp(), "skill-stage-*")); len(m) != 0 {
		t.Errorf("stages left: %v", m)
	}
}

// uploads lists the uploads under the tmp root.
func uploads(t *testing.T, a *App) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(a.SkillTmp(), "skill-upload-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestInstallSkill(t *testing.T) {
	ctx := context.Background()
	a, st := apiApp(t)
	src := installSrc(t, "ship", "")
	out, err := a.InstallSkill(ctx, gen.SkillInstall{Agent: "main", Upload: upload(t, a, src)}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Installed || !strings.HasPrefix(out.Hash, "sha256:") || len(out.Findings) != 0 || out.Findings == nil ||
		out.Commit != nil || out.Skill.Name != "ship" || out.Skill.Level != gen.SkillLevelAgent || !strings.Contains(str(out.Skill.Body), "# ship") {
		t.Fatalf("out = %+v", out)
	}
	dir := filepath.Join(a.Config.Dir, "agents", "main", "skills", "ship")
	o, ok, err := skill.ReadOrigin(dir)
	if err != nil || !ok || o.Source != src || o.Hash != out.Hash || o.Ref != "" || o.Installed.IsZero() {
		t.Fatalf("origin = %+v, %v, %v", o, ok, err)
	}
	noStages(t, a)
	if left := uploads(t, a); len(left) != 0 {
		t.Errorf("upload kept after the install: %v", left)
	}
	list, err := a.ListSkills(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range list {
		if s.Name == "ship" && s.Level == gen.SkillLevelAgent && s.Hidden == nil {
			found = true
		}
	}
	if !found {
		t.Errorf("ship not listed: %+v", list)
	}
	evs := skillEvents(t, st, event.SkillInstalled)
	if len(evs) != 1 || evs[0]["who"] != "admin" || evs[0]["agent"] != "main" || evs[0]["name"] != "ship" ||
		evs[0]["source"] != src || evs[0]["hash"] != out.Hash || evs[0]["findings"] != float64(0) {
		t.Fatalf("events = %v", evs)
	}
	if _, ok := evs[0]["ref"]; ok {
		t.Errorf("ref on a local install: %v", evs[0])
	}

	// Again: the folder exists.
	if _, err := a.InstallSkill(ctx, gen.SkillInstall{Agent: "main", Upload: upload(t, a, src)}, "admin"); !errors.Is(err, skill.ErrExists) {
		t.Errorf("again: %v", err)
	}
	noStages(t, a)
	// Under another name.
	out, err = a.InstallSkill(ctx, gen.SkillInstall{Agent: "main", Upload: upload(t, a, src), Name: ptrTo("ship-too")}, "admin")
	if err != nil || out.Skill.Name != "ship-too" {
		t.Fatalf("as = %+v, %v", out, err)
	}
	b, err := os.ReadFile(filepath.Join(a.Config.Dir, "agents", "main", "skills", "ship-too", "SKILL.md"))
	if err != nil || !strings.Contains(string(b), "name: ship-too\n") {
		t.Errorf("renamed SKILL.md = %q, %v", b, err)
	}
	if evs := skillEvents(t, st, event.SkillInstalled); len(evs) != 2 {
		t.Errorf("events = %v", evs)
	}
}

func TestInstallSkillPreview(t *testing.T) {
	ctx := context.Background()
	a, st := apiApp(t)
	src := installSrc(t, "ship", "Run it\u200b first.\n")
	in := gen.SkillInstall{Agent: "main", Upload: upload(t, a, src), Preview: ptrTo(true)}
	prev, err := a.InstallSkill(ctx, in, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if prev.Installed || len(prev.Findings) != 1 || prev.Findings[0].Code != skill.CodeScanUnicode ||
		prev.Findings[0].File != "SKILL.md" || prev.Findings[0].Line != 5 || prev.Findings[0].Message == "" {
		t.Fatalf("preview = %+v", prev)
	}
	dir := filepath.Join(a.SkillRoots("main").Agent, "ship")
	installed := func() bool { _, err := os.Stat(dir); return err == nil }
	if installed() || len(skillEvents(t, st, event.SkillInstalled)) != 0 {
		t.Fatal("preview installed")
	}
	noStages(t, a)

	in.Preview, in.Hash = nil, &prev.Hash
	if _, err := a.InstallSkill(ctx, in, "admin"); !errors.Is(err, skill.ErrUnsafe) || installed() {
		t.Fatalf("unforced: %v", err)
	}
	noStages(t, a)
	in.Force, in.Hash = ptrTo(true), ptrTo("sha256:0000")
	if _, err := a.InstallSkill(ctx, in, "admin"); !errors.Is(err, skill.ErrChanged) || installed() {
		t.Fatalf("changed: %v", err)
	}
	noStages(t, a)
	// The preview and the refused installs keep the upload for the next try.
	if n := len(uploads(t, a)); n != 1 {
		t.Fatalf("%d uploads", n)
	}
	in.Hash = &prev.Hash
	out, err := a.InstallSkill(ctx, in, "admin")
	if err != nil || !out.Installed || !installed() || len(out.Findings) != 1 {
		t.Fatalf("forced = %+v, %v", out, err)
	}
	if n := len(uploads(t, a)); n != 0 {
		t.Errorf("%d uploads after the install", n)
	}
	if evs := skillEvents(t, st, event.SkillInstalled); len(evs) != 1 || evs[0]["findings"] != float64(1) {
		t.Errorf("events = %v", evs)
	}
}

func TestInstallSkillRefuses(t *testing.T) {
	ctx := context.Background()
	a, st := apiApp(t)
	broken := filepath.Join(t.TempDir(), "broken")
	if err := os.MkdirAll(broken, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "SKILL.md"), []byte("---\nname: broken\n---\nno description\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		in   gen.SkillInstall
		want error
	}{
		{gen.SkillInstall{Agent: "main", Upload: upload(t, a, broken)}, skill.ErrBadSource},
		{gen.SkillInstall{Agent: "nobody", Upload: upload(t, a, installSrc(t, "ship", ""))}, skill.ErrNotFound},
		{gen.SkillInstall{Agent: "main", Upload: ptrTo("01ARZ3NDEKTSV4RRFFQ69G5FAV")}, skill.ErrNotFound},
		{gen.SkillInstall{Agent: "main", Git: &gen.SkillGitSource{Url: "ssh://h/x", Ref: ptrTo("-x")}}, skill.ErrBadSource},
		{gen.SkillInstall{Agent: "main"}, skill.ErrBadSource},
		{gen.SkillInstall{Agent: "main", Upload: upload(t, a, installSrc(t, "ship", "")), Name: ptrTo("Bad Name")}, skill.ErrBadName},
		{gen.SkillInstall{Agent: "main", Upload: upload(t, a, installSrc(t, "plain", ""))}, skill.ErrExists},
	} {
		if _, err := a.InstallSkill(ctx, c.in, "admin"); !errors.Is(err, c.want) {
			t.Errorf("%+v: %v, want %v", c.in, err, c.want)
		}
	}
	noStages(t, a)
	if evs := skillEvents(t, st, event.SkillInstalled); len(evs) != 0 {
		t.Errorf("events = %v", evs)
	}
}

// An install drops every cached run set.
func TestInstallSkillDropsCache(t *testing.T) {
	ctx := context.Background()
	a, _ := apiApp(t)
	a.RunSkills(skillCall("T1"))
	if len(a.skills.runs) != 1 {
		t.Fatal("not cached")
	}
	if _, err := a.InstallSkill(ctx, gen.SkillInstall{Agent: "main", Upload: upload(t, a, installSrc(t, "ship", ""))}, "admin"); err != nil {
		t.Fatal(err)
	}
	if len(a.skills.runs) != 0 || len(a.skills.order) != 0 {
		t.Fatalf("cache kept: %d", len(a.skills.runs))
	}
	if got := names(a.RunSkills(skillCall("T1"))); !strings.Contains(got, "ship") {
		t.Fatalf("not eligible: %s", got)
	}
}

// collectionSrc is a local collection <tmp>/repo with alpha (clean) and beta
// (a zero-width space, SCN003) under prefix ("skills/" or "").
func collectionSrc(t *testing.T, prefix string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, filepath.Join(dir, prefix), "alpha", "")
	writeSkill(t, filepath.Join(dir, prefix), "beta", "")
	beta := "---\nname: beta\ndescription: When the task is about beta.\n---\nRun it\u200b first.\n"
	if err := os.WriteFile(filepath.Join(dir, prefix, "beta", "SKILL.md"), []byte(beta), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInstallSkillCollection(t *testing.T) {
	ctx := context.Background()
	a, st := apiApp(t)
	src := collectionSrc(t, "skills/")
	root := a.SkillRoots("main").Agent
	installed := func(name string) bool { _, err := os.Stat(filepath.Join(root, name)); return err == nil }

	in := gen.SkillInstall{Agent: "main", Upload: upload(t, a, src), Preview: ptrTo(true)}
	prev, err := a.InstallSkill(ctx, in, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if prev.Skill != nil || prev.Installed || prev.Findings == nil || len(prev.Findings) != 0 || prev.Path != nil ||
		!strings.HasPrefix(prev.Hash, "sha256:") || prev.Skills == nil || len(*prev.Skills) != 2 {
		t.Fatalf("preview = %+v", prev)
	}
	es := *prev.Skills
	for i, want := range []string{"alpha", "beta"} {
		e := es[i]
		if str(e.Path) != "skills/"+want || e.Skill == nil || e.Skill.Name != want || e.Skill.Body != nil ||
			e.Installed || !strings.HasPrefix(e.Hash, "sha256:") {
			t.Errorf("entry %d = %+v", i, e)
		}
	}
	if len(es[0].Findings) != 0 || len(es[1].Findings) != 1 || es[1].Findings[0].Code != skill.CodeScanUnicode {
		t.Errorf("findings = %+v, %+v", es[0].Findings, es[1].Findings)
	}
	sum := sha256.Sum256([]byte(es[0].Hash + "\n" + es[1].Hash))
	if prev.Hash != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("hash = %s", prev.Hash)
	}
	if installed("alpha") || installed("beta") || len(skillEvents(t, st, event.SkillInstalled)) != 0 {
		t.Fatal("preview installed")
	}
	noStages(t, a)

	in.Preview = nil
	for _, c := range []struct {
		in   gen.SkillInstall
		want error
	}{
		{in, skill.ErrCollection},
		{gen.SkillInstall{Agent: "main", Upload: upload(t, a, src), All: ptrTo(true)}, skill.ErrUnsafe},
		{gen.SkillInstall{Agent: "main", Upload: upload(t, a, src), All: ptrTo(true), Force: ptrTo(true), Name: ptrTo("x")}, skill.ErrCollection},
		{gen.SkillInstall{Agent: "main", Upload: upload(t, a, src), All: ptrTo(true), Force: ptrTo(true), Hash: ptrTo("sha256:0000")}, skill.ErrChanged},
		{gen.SkillInstall{Agent: "main", Upload: upload(t, a, src), All: ptrTo(true), Force: ptrTo(true), Hash: &es[0].Hash}, skill.ErrChanged},
		{gen.SkillInstall{Agent: "main", Upload: upload(t, a, t.TempDir()), All: ptrTo(true)}, skill.ErrBadSource},
	} {
		if _, err := a.InstallSkill(ctx, c.in, "admin"); !errors.Is(err, c.want) {
			t.Errorf("%+v: %v, want %v", c.in, err, c.want)
		}
		if installed("alpha") || installed("beta") {
			t.Fatalf("%+v installed", c.in)
		}
		noStages(t, a)
	}
	if evs := skillEvents(t, st, event.SkillInstalled); len(evs) != 0 {
		t.Fatalf("events = %v", evs)
	}

	out, err := a.InstallSkill(ctx, gen.SkillInstall{Agent: "main", Upload: upload(t, a, src), All: ptrTo(true), Force: ptrTo(true), Hash: &prev.Hash}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Installed || out.Hash != prev.Hash || out.Skills == nil || len(*out.Skills) != 2 {
		t.Fatalf("out = %+v", out)
	}
	for _, e := range *out.Skills {
		if !e.Installed {
			t.Errorf("entry not installed: %+v", e)
		}
	}
	noStages(t, a)
	evs := skillEvents(t, st, event.SkillInstalled)
	if len(evs) != 2 {
		t.Fatalf("events = %v", evs)
	}
	sources := map[string]bool{}
	for _, e := range evs {
		sources[e["source"].(string)] = true
	}
	for i, name := range []string{"alpha", "beta"} {
		want := filepath.Join(src, "skills", name)
		if !sources[want] {
			t.Errorf("no event with source %s: %v", want, evs)
		}
		o, ok, err := skill.ReadOrigin(filepath.Join(root, name))
		if err != nil || !ok || o.Source != want || o.Hash != es[i].Hash {
			t.Errorf("origin of %s = %+v, %v, %v", name, o, ok, err)
		}
	}
	list, err := a.ListSkills(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, s := range list {
		if (s.Name == "alpha" || s.Name == "beta") && s.Level == gen.SkillLevelAgent && s.Hidden == nil {
			found++
		}
	}
	if found != 2 {
		t.Errorf("listed %d of 2: %+v", found, list)
	}
}

// A batch never half-lands: one name taken refuses every skill.
func TestInstallSkillCollectionExists(t *testing.T) {
	ctx := context.Background()
	a, st := apiApp(t)
	root := a.SkillRoots("main").Agent
	writeSkill(t, root, "beta", "")
	_, err := a.InstallSkill(ctx, gen.SkillInstall{Agent: "main", Upload: upload(t, a, collectionSrc(t, "skills/")), All: ptrTo(true), Force: ptrTo(true)}, "admin")
	if !errors.Is(err, skill.ErrExists) || !strings.Contains(err.Error(), "beta") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "alpha")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("alpha installed: %v", err)
	}
	noStages(t, a)
	if evs := skillEvents(t, st, event.SkillInstalled); len(evs) != 0 {
		t.Errorf("events = %v", evs)
	}
}

// Skill folders directly under the source, no skills/.
func TestInstallSkillCollectionDirect(t *testing.T) {
	ctx := context.Background()
	a, _ := apiApp(t)
	src := collectionSrc(t, "")
	out, err := a.InstallSkill(ctx, gen.SkillInstall{Agent: "main", Upload: upload(t, a, src), All: ptrTo(true), Force: ptrTo(true)}, "admin")
	if err != nil || !out.Installed || out.Skills == nil || len(*out.Skills) != 2 || str((*out.Skills)[0].Path) != "alpha" {
		t.Fatalf("out = %+v, %v", out, err)
	}
	o, ok, err := skill.ReadOrigin(filepath.Join(a.SkillRoots("main").Agent, "beta"))
	if err != nil || !ok || o.Source != filepath.Join(src, "beta") {
		t.Errorf("origin = %+v, %v, %v", o, ok, err)
	}
	noStages(t, a)
	if left := uploads(t, a); len(left) != 0 {
		t.Errorf("upload kept after the install: %v", left)
	}
}

// One skill of a collection that does not parse fails the preview and the
// install alike, naming its folder, and leaves nothing staged.
func TestInstallSkillCollectionBroken(t *testing.T) {
	ctx := context.Background()
	a, st := apiApp(t)
	src := collectionSrc(t, "")
	if err := os.WriteFile(filepath.Join(src, "beta", "SKILL.md"), []byte("---\nname: beta\n---\nno description\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := upload(t, a, src)
	for _, in := range []gen.SkillInstall{
		{Agent: "main", Upload: id, Preview: ptrTo(true)},
		{Agent: "main", Upload: id, All: ptrTo(true), Force: ptrTo(true)},
	} {
		if _, err := a.InstallSkill(ctx, in, "admin"); !errors.Is(err, skill.ErrBadSource) || !strings.HasPrefix(err.Error(), "beta: ") {
			t.Errorf("%+v: %v", in, err)
		}
		noStages(t, a)
	}
	if _, err := os.Stat(filepath.Join(a.SkillRoots("main").Agent, "alpha")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("alpha installed: %v", err)
	}
	if evs := skillEvents(t, st, event.SkillInstalled); len(evs) != 0 {
		t.Errorf("events = %v", evs)
	}
}

// gitCollection is a git repository holding collectionSrc(t, "skills/"),
// tagged v1; it returns its file url and the commit.
func gitCollection(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := collectionSrc(t, "skills/")
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	run("add", ".")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "one")
	run("tag", "v1")
	return "file://" + dir, run("rev-parse", "HEAD")
}

// A git source without a ref installs from the repository's default
// branch, here trunk, and records no ref.
func TestInstallSkillGitDefaultBranch(t *testing.T) {
	ctx := context.Background()
	a, st := apiApp(t)
	url, _ := gitCollection(t)
	git := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		cmd.Dir = strings.TrimPrefix(url, "file://")
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("branch", "-m", "trunk")
	git("commit", "-q", "--allow-empty", "-m", "two")
	commit := git("rev-parse", "HEAD")
	root := a.SkillRoots("main").Agent

	in := gen.SkillInstall{Agent: "main", Git: &gen.SkillGitSource{Url: url}, All: ptrTo(true), Force: ptrTo(true)}
	out, err := a.InstallSkill(ctx, in, "admin")
	if err != nil || !out.Installed || str(out.Commit) != commit {
		t.Fatalf("install = %+v, %v; want commit %s", out, err, commit)
	}
	o, ok, err := skill.ReadOrigin(filepath.Join(root, "beta"))
	want := skill.Installed{Source: url, Path: "skills/beta", Commit: commit, Hash: o.Hash, Installed: o.Installed}
	if err != nil || !ok || o != want {
		t.Fatalf("origin = %+v, %v, %v; want %+v", o, ok, err, want)
	}
	for _, ev := range skillEvents(t, st, event.SkillInstalled) {
		if _, ok := ev["ref"]; ok || ev["commit"] != commit {
			t.Errorf("event = %v", ev)
		}
	}
}

func TestInstallSkillGit(t *testing.T) {
	ctx := context.Background()
	a, st := apiApp(t)
	url, commit := gitCollection(t)
	root := a.SkillRoots("main").Agent

	out, err := a.InstallSkill(ctx, gen.SkillInstall{Agent: "main", Git: &gen.SkillGitSource{Url: url, Ref: ptrTo("v1"), Path: ptrTo("skills/alpha")}}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Installed || out.Skill == nil || out.Skill.Name != "alpha" || str(out.Commit) != commit {
		t.Fatalf("out = %+v", out)
	}
	o, ok, err := skill.ReadOrigin(filepath.Join(root, "alpha"))
	want := skill.Installed{Source: url, Ref: "v1", Path: "skills/alpha", Commit: commit, Hash: out.Hash, Installed: o.Installed}
	if err != nil || !ok || o != want {
		t.Fatalf("origin = %+v, %v, %v; want %+v", o, ok, err, want)
	}
	evs := skillEvents(t, st, event.SkillInstalled)
	if len(evs) != 1 || evs[0]["source"] != url || evs[0]["ref"] != "v1" || evs[0]["path"] != "skills/alpha" || evs[0]["commit"] != commit {
		t.Fatalf("events = %v", evs)
	}

	// The whole collection: alpha exists, so nothing lands.
	in := gen.SkillInstall{Agent: "main", Git: &gen.SkillGitSource{Url: url, Ref: ptrTo("v1")}, All: ptrTo(true), Force: ptrTo(true)}
	if _, err := a.InstallSkill(ctx, in, "admin"); !errors.Is(err, skill.ErrExists) {
		t.Fatalf("collection over alpha: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "alpha")); err != nil {
		t.Fatal(err)
	}
	if out, err = a.InstallSkill(ctx, in, "admin"); err != nil || !out.Installed || str(out.Commit) != commit {
		t.Fatalf("collection = %+v, %v", out, err)
	}
	o, _, err = skill.ReadOrigin(filepath.Join(root, "beta"))
	if err != nil || o.Source != url || o.Path != "skills/beta" || o.Ref != "v1" {
		t.Errorf("origin of beta = %+v, %v", o, err)
	}
	if m, _ := filepath.Glob(filepath.Join(a.SkillTmp(), "skill-*")); len(m) != 0 {
		t.Errorf("checkouts left: %v", m)
	}
}
