package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/skill"
	"holaclaw/internal/tokens"
	"holaclaw/internal/tool"
)

// fakeSkills returns a fixed eligible set, or an error, and records the call
// it was asked about. teachers maps an identity id to the name Teacher
// resolves it to; a missing entry gives an empty name, as an unknown id does.
type fakeSkills struct {
	skills     []skill.Skill
	err        error
	got        tool.Call
	teachers   map[event.ID]string
	teacherErr error
	root       string // agent skills root the writer writes under
	writerErr  error  // Writer fails, as for an agent that is not configured
}

func (f *fakeSkills) Skills(_ context.Context, c tool.Call) ([]skill.Skill, error) {
	f.got = c
	return f.skills, f.err
}

func (f *fakeSkills) Teacher(_ context.Context, id event.ID) (string, error) {
	if f.teacherErr != nil {
		return "", f.teacherErr
	}
	return f.teachers[id], nil
}

func (f *fakeSkills) Writer(tool.Call) (skill.Writer, error) {
	return skill.Writer{Root: f.root}, f.writerErr
}

type skillRig struct {
	exec   tool.Executor
	src    *fakeSkills
	events []event.Event
}

// newSkillRig loads the repository fixtures and offers the ones that parse.
func newSkillRig(t *testing.T) *skillRig {
	t.Helper()
	set, _ := skill.Load(skill.Roots{Agent: filepath.Join("..", "..", "..", "testdata", "skills", "agent")})
	g := &skillRig{src: &fakeSkills{skills: set.Resolve()}}
	if len(g.src.skills) == 0 {
		t.Fatal("no fixture skills loaded")
	}
	r := tool.NewRegistry()
	sink := event.SinkFunc(func(_ context.Context, e event.Event) { g.events = append(g.events, e) })
	if err := RegisterSkill(r, g.src, sink); err != nil {
		t.Fatal(err)
	}
	ex, err := r.Select([]string{"skill.load"})
	if err != nil {
		t.Fatal(err)
	}
	g.exec = ex
	return g
}

func (g *skillRig) load(t *testing.T, args string) tool.Result {
	t.Helper()
	res, err := g.exec.Execute(context.Background(), tool.Call{
		Name: "skill.load", Args: json.RawMessage(args), Agent: "main",
		Partition: "dm/U1", Role: "owner", TraceID: "T1", SessionID: "S1",
	})
	if err != nil {
		t.Fatalf("Go error: %v", err)
	}
	return res
}

func TestSkillLoadBody(t *testing.T) {
	g := newSkillRig(t)
	res := g.load(t, `{"name":"pr-style"}`)
	if res.IsError || res.Tainted {
		t.Fatalf("res = %+v", res)
	}
	if !strings.HasPrefix(res.Text, "[skill: pr-style · agent skill]\n\n# Pr style") {
		t.Fatalf("text = %q", res.Text)
	}
	if g.src.got.Partition != "dm/U1" || g.src.got.Role != "owner" {
		t.Fatalf("source was not given the trusted call: %+v", g.src.got)
	}
	if len(g.events) != 1 {
		t.Fatalf("%d events", len(g.events))
	}
	e := g.events[0]
	if e.Type != event.SkillLoaded || e.TraceID != "T1" || e.SessionID != "S1" {
		t.Fatalf("event = %+v", e)
	}
	var attrs map[string]any
	if err := json.Unmarshal(e.Attrs, &attrs); err != nil {
		t.Fatal(err)
	}
	var sk skill.Skill
	for _, s := range g.src.skills {
		if s.Name == "pr-style" {
			sk = s
		}
	}
	want := map[string]any{
		"agent": "main", "name": "pr-style", "level": "agent",
		"body_tokens": float64(tokens.Estimate(sk.Body)), "provenance": "[skill: pr-style · agent skill]",
	}
	for k, v := range want {
		if attrs[k] != v {
			t.Errorf("attr %s = %v, want %v", k, attrs[k], v)
		}
	}
	if _, ok := attrs["file"]; ok {
		t.Error("file attr without a file")
	}
}

func TestSkillLoadReference(t *testing.T) {
	g := newSkillRig(t)
	for _, file := range []string{"checklist.md", "references/checklist.md"} {
		res := g.load(t, `{"name":"pr-style","file":"`+file+`"}`)
		if res.IsError || res.Text != "[skill: pr-style · references/checklist.md]\n\n# Checklist\n\n- tests pass\n" {
			t.Fatalf("%s: res = %+v", file, res)
		}
	}
	var attrs map[string]any
	_ = json.Unmarshal(g.events[0].Attrs, &attrs)
	if attrs["file"] != "checklist.md" {
		t.Fatalf("attrs = %v", attrs)
	}
}

func TestSkillLoadErrors(t *testing.T) {
	g := newSkillRig(t)
	for _, tc := range []struct{ args, want string }{
		{`{}`, "name is required"},
		{`{"name":"  "}`, "name is required"},
		{`{"name":7}`, "bad arguments"},
		{`{"name":"nope"}`, `no skill "nope"`},
		{`{"name":"broken"}`, `no skill "broken"`}, // failed to load: never offered
		{`{"name":"pr-style","file":"missing.md"}`, "has no file references/missing.md"},
		{`{"name":"pr-style","file":"../SKILL.md"}`, "relative path under references/"},
		{`{"name":"pr-style","file":"/etc/passwd"}`, "relative path under references/"},
		{`{"name":"summarize","file":"x.md"}`, "has no file"},
	} {
		res := g.load(t, tc.args)
		if !res.IsError || !strings.Contains(res.Text, tc.want) {
			t.Errorf("%s: res = %+v, want error containing %q", tc.args, res, tc.want)
		}
	}
	if len(g.events) != 0 {
		t.Fatalf("errors emitted %d skill.loaded events", len(g.events))
	}

	g.src.err = errors.New("disk on fire")
	if res := g.load(t, `{"name":"pr-style"}`); !res.IsError || strings.Contains(res.Text, "fire") {
		t.Fatalf("source error: %+v", res)
	}
}

// An ineligible skill is simply absent from the source: the tool cannot load
// it even though it exists on disk.
func TestSkillLoadIneligible(t *testing.T) {
	g := newSkillRig(t)
	g.src.skills = nil
	if res := g.load(t, `{"name":"pr-style"}`); !res.IsError {
		t.Fatalf("res = %+v", res)
	}
}

func TestSkillLoadReferenceNotText(t *testing.T) {
	dir := t.TempDir()
	sk := filepath.Join(dir, "bin")
	if err := os.MkdirAll(filepath.Join(sk, "references"), 0o750); err != nil {
		t.Fatal(err)
	}
	must := func(p string, b []byte) {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join(sk, "SKILL.md"), []byte("---\nname: bin\ndescription: When a binary file is involved.\n---\n# Bin\n"))
	must(filepath.Join(sk, "references", "blob.dat"), []byte{0xff, 0xfe, 0x00, 0x80})
	must(filepath.Join(sk, "references", "big.md"), []byte(strings.Repeat("a", skill.MaxReference+1)))
	must(filepath.Join(sk, "references", "at-cap.md"), []byte(strings.Repeat("b", skill.MaxReference)))
	set, probs := skill.Load(skill.Roots{Agent: dir})
	if len(probs) != 0 {
		t.Fatal(probs)
	}
	g := newSkillRig(t)
	g.src.skills = set.Resolve()
	if res := g.load(t, `{"name":"bin","file":"blob.dat"}`); !res.IsError || !strings.Contains(res.Text, "not text") {
		t.Fatalf("binary: %+v", res)
	}
	if res := g.load(t, `{"name":"bin","file":"big.md"}`); !res.IsError || !strings.Contains(res.Text, "over 16 KiB") {
		t.Fatalf("big: %+v", res)
	}
	// A reference at the cap loads whole: nothing is truncated.
	res := g.load(t, `{"name":"bin","file":"at-cap.md"}`)
	if res.IsError || strings.Count(res.Text, "b") < skill.MaxReference || strings.Contains(res.Text, "[truncated") {
		t.Fatalf("at cap: error=%v len=%d", res.IsError, len(res.Text))
	}
}

// The provenance line names the teacher Teacher resolves for a skill the
// agent saved, from Learned.By; an id it cannot resolve leaves the name out
// (docs/skills.md §4).
func TestSkillLoadProvenanceTeacher(t *testing.T) {
	body := "---\nname: learned-one\ndescription: When taught.\nmetadata:\n  holaclaw:\n    version: 1\n    learned:\n" +
		"      on: 2026-09-12\n      by: 01J9Q4ABCDEFGHJKMNPQRSTVWX\n      from: 01J9Q4ABCDEFGHJKMNPQRSTVWY\n" +
		"      origin: explicit\n---\n# Learned one\n"
	sk, err := skill.Parse(skill.LevelAgent, "learned-one", []byte(body))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("known teacher", func(t *testing.T) {
		g := newSkillRig(t)
		g.src.skills = []skill.Skill{sk}
		g.src.teachers = map[event.ID]string{"01J9Q4ABCDEFGHJKMNPQRSTVWX": "Angel"}
		res := g.load(t, `{"name":"learned-one"}`)
		if res.IsError || !strings.HasPrefix(res.Text, "[skill: learned-one · learned from Angel on 2026-09-12]") {
			t.Fatalf("res = %+v", res)
		}
	})

	t.Run("unknown teacher", func(t *testing.T) {
		g := newSkillRig(t)
		g.src.skills = []skill.Skill{sk}
		res := g.load(t, `{"name":"learned-one"}`)
		if res.IsError || !strings.HasPrefix(res.Text, "[skill: learned-one · learned on 2026-09-12]") {
			t.Fatalf("res = %+v", res)
		}
	})
}

func TestSkillLoadSchema(t *testing.T) {
	s := (&skillLoad{}).Spec()
	if n := tool.SchemaTokens(s); n > tool.MaxSchemaTokens {
		t.Fatalf("schema is %d tokens, cap %d", n, tool.MaxSchemaTokens)
	}
	sl := &skillLoad{}
	if sl.Risk() != tool.RiskRead {
		t.Fatal("skill.load must be class read")
	}
	if err := RegisterSkill(tool.NewRegistry(), nil, nil); err == nil {
		t.Fatal("nil source accepted")
	}
}

// saveTrace is a valid trace id: it becomes learned.from.
const saveTrace event.ID = "01J9Q4ABCDEFGHJKMNPQRSTVWX"

// teacherID is the sender's user id: it becomes learned.by.
const teacherID event.ID = "01J9PXABCDEFGHJKMNPQRSTVWX"

const deployArgs = `{"name":"deploy","title":"Deploy the service","when":"When deploying the service.",` +
	`"steps":["Run the tests."," Tag the release. "],"pitfalls":["Never skip the tests."],"origin":"correction"}`

type writeRig struct {
	src    *fakeSkills
	save   *skillSave
	forget *skillForget
	events []event.Event
}

func newWriteRig(t *testing.T) *writeRig {
	t.Helper()
	g := &writeRig{src: &fakeSkills{root: t.TempDir()}}
	sink := event.SinkFunc(func(_ context.Context, e event.Event) { g.events = append(g.events, e) })
	g.save = &skillSave{src: g.src, sink: sink, now: func() time.Time { return time.Date(2026, 9, 12, 23, 30, 0, 0, time.UTC) }}
	g.forget = &skillForget{src: g.src, sink: sink}
	return g
}

func ownerCall(name, args string) tool.Call {
	return tool.Call{
		Name: name, Args: json.RawMessage(args), Agent: "main", Partition: "dm/U1", UserID: teacherID,
		Role: "owner", RunKind: tool.RunInteractive, TraceID: saveTrace, SessionID: "S1",
	}
}

func runSkill(t *testing.T, tl tool.Tool, c tool.Call) tool.Result {
	t.Helper()
	res, err := tl.Run(context.Background(), c)
	if err != nil {
		t.Fatalf("Go error: %v", err)
	}
	return res
}

// agentSkills reloads the agent root, as the daemon would for the next call.
func (g *writeRig) agentSkills(t *testing.T) []skill.Skill {
	t.Helper()
	set, probs := skill.Load(skill.Roots{Agent: g.src.root})
	if len(probs) > 0 {
		t.Fatalf("problems: %v", probs)
	}
	return set.Resolve()
}

func (g *writeRig) savedAttrs(t *testing.T) map[string]any {
	t.Helper()
	if len(g.events) != 1 || g.events[0].Type != event.SkillSaved {
		t.Fatalf("events = %+v", g.events)
	}
	e := g.events[0]
	if e.TraceID != saveTrace || e.SessionID != "S1" {
		t.Fatalf("event ids: %+v", e)
	}
	var m map[string]any
	if err := json.Unmarshal(e.Attrs, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// handSkill writes <root>/<name>/SKILL.md as a person would, with extra
// frontmatter lines, and returns its content.
func handSkill(t *testing.T, root, name, front string) []byte {
	t.Helper()
	b := []byte("---\nname: " + name + "\ndescription: When the task is " + name + ".\n" + front + "---\n# By hand\n")
	if err := os.MkdirAll(filepath.Join(root, name), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name, "SKILL.md"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return b
}

// tree lists every file under root with its content, to prove a refused
// write changed nothing.
func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		out[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSkillSaveNew(t *testing.T) {
	g := newWriteRig(t)
	res := runSkill(t, g.save, ownerCall("skill.save", deployArgs))
	if res.IsError || res.Text != "saved skill deploy version 1 (new)\nloaded from now on when it matches" {
		t.Fatalf("res = %+v", res)
	}
	sks := g.agentSkills(t)
	if len(sks) != 1 {
		t.Fatalf("skills = %+v", sks)
	}
	sk := sks[0]
	want := skill.Learned{On: time.Date(2026, 9, 12, 23, 30, 0, 0, time.UTC), By: teacherID, From: saveTrace, Origin: skill.OriginCorrection}
	if sk.Name != "deploy" || sk.Level != skill.LevelAgent || sk.Learned == nil || *sk.Learned != want {
		t.Fatalf("skill = %+v learned %+v", sk, sk.Learned)
	}
	// A new skill declares nothing (docs/skills.md §8).
	if len(sk.Requires.Tools)+len(sk.Requires.Bins)+len(sk.Requires.Net) != 0 {
		t.Fatalf("requires = %+v", sk.Requires)
	}
	if !strings.Contains(sk.Body, "2. Tag the release.\n") || !strings.Contains(sk.Body, "- Never skip the tests.") {
		t.Fatalf("body = %q", sk.Body)
	}
	b, err := os.ReadFile(filepath.Join(g.src.root, "deploy", "SKILL.md"))
	if err != nil || !strings.Contains(string(b), "by: "+string(teacherID)) {
		t.Fatalf("frontmatter has no by: %q (%v)", b, err)
	}
	a := g.savedAttrs(t)
	if a["agent"] != "main" || a["name"] != "deploy" || a["by"] != string(teacherID) || a["origin"] != "correction" ||
		a["replaced"] != float64(0) || a["version"] != float64(1) || a["versions"] != float64(0) || a["body_tokens"] != float64(tokens.Estimate(sk.Body)) {
		t.Fatalf("attrs = %v", a)
	}
	if _, ok := a["space"]; ok {
		t.Fatalf("space attr: %v", a)
	}
}

// An admin may teach too, in a room as in a DM.
func TestSkillSaveAdmin(t *testing.T) {
	g := newWriteRig(t)
	c := ownerCall("skill.save", deployArgs)
	c.Role, c.Partition = "admin", "group/telegram-home/-100123"
	if res := runSkill(t, g.save, c); res.IsError {
		t.Fatal(res.Text)
	}
	if len(g.agentSkills(t)) != 1 {
		t.Fatal("not saved")
	}
}

// Saving over an existing agent skill replaces it without a load first: the
// prompt asks for one, and the old version is kept in history/.
func TestSkillSaveReplaces(t *testing.T) {
	g := newWriteRig(t)
	if res := runSkill(t, g.save, ownerCall("skill.save", deployArgs)); res.IsError {
		t.Fatal(res.Text)
	}
	g.events = nil
	before, err := os.ReadFile(filepath.Join(g.src.root, "deploy", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}

	res := runSkill(t, g.save, ownerCall("skill.save", strings.Replace(deployArgs, "correction", "explicit", 1)))
	if res.IsError {
		t.Fatal(res.Text)
	}
	hist, err := os.ReadDir(filepath.Join(g.src.root, "deploy", "history"))
	if err != nil || len(hist) != 1 || hist[0].Name() != "1" {
		t.Fatalf("history = %v, %v", hist, err)
	}
	if !strings.HasPrefix(res.Text, "saved skill deploy version 2 (replaced version 1)\n") {
		t.Fatalf("res = %q", res.Text)
	}
	old, err := os.ReadFile(filepath.Join(g.src.root, "deploy", "history", "1", "SKILL.md"))
	if err != nil || string(old) != string(before) {
		t.Fatalf("history holds %q (%v)", old, err)
	}
	a := g.savedAttrs(t)
	if a["replaced"] != float64(1) || a["version"] != float64(2) || a["versions"] != float64(1) || a["origin"] != "explicit" {
		t.Fatalf("attrs = %v", a)
	}
}

// A hand-written agent skill that declares a binary is replaced; the new
// version keeps its requires unchanged and the hand-written
// file is retired into history/ (docs/skills.md §6, §8).
func TestSkillSaveCarriesRequires(t *testing.T) {
	g := newWriteRig(t)
	hand := handSkill(t, g.src.root, "deploy",
		"metadata:\n  holaclaw:\n    version: 1\n    requires:\n      tools: [exec]\n      bins: [gh, {name: rg, pkg: ripgrep}]\n      net: [gh]\n")
	if res := runSkill(t, g.save, ownerCall("skill.save", deployArgs)); res.IsError {
		t.Fatal(res.Text)
	}
	sks := g.agentSkills(t)
	if len(sks) != 1 || sks[0].Learned == nil {
		t.Fatalf("skills = %+v", sks)
	}
	r := sks[0].Requires
	if strings.Join(r.Tools, ",") != "exec" || strings.Join(r.Net, ",") != "gh" || len(r.Bins) != 2 ||
		r.Bins[0] != (skill.Bin{Name: "gh", Pkg: "gh"}) || r.Bins[1] != (skill.Bin{Name: "rg", Pkg: "ripgrep"}) {
		t.Fatalf("requires = %+v", r)
	}
	b, err := os.ReadFile(filepath.Join(g.src.root, "deploy", "SKILL.md"))
	if err != nil || !strings.Contains(string(b), "    version: 1\n    skill_version: 2\n") {
		t.Fatalf("SKILL.md = %q (%v)", b, err)
	}
	if a := g.savedAttrs(t); a["replaced"] != float64(1) || a["version"] != float64(2) {
		t.Fatalf("attrs = %v", a)
	}
	old, err := os.ReadFile(filepath.Join(g.src.root, "deploy", "history", "1", "SKILL.md"))
	if err != nil || string(old) != string(hand) {
		t.Fatalf("history holds %q (%v)", old, err)
	}
}

// A bundled skill is never written: saving its name creates an agent skill
// that shadows it, with no load needed and nothing carried over.
func TestSkillSaveShadowsBundled(t *testing.T) {
	g := newWriteRig(t)
	g.src.skills = []skill.Skill{{Name: "deploy", Description: "Deploy.", Level: skill.LevelBundled,
		Requires: skill.Requires{Tools: []string{"exec"}}}}
	if res := runSkill(t, g.save, ownerCall("skill.save", deployArgs)); res.IsError || !strings.Contains(res.Text, "(new)") {
		t.Fatalf("res = %+v", res)
	}
	sks := g.agentSkills(t)
	if len(sks) != 1 || sks[0].Level != skill.LevelAgent || len(sks[0].Requires.Tools) != 0 {
		t.Fatalf("skills = %+v", sks)
	}
}

func TestSkillSaveRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*testing.T, *tool.Call, *fakeSkills)
		want string
	}{
		{"guest", func(_ *testing.T, c *tool.Call, _ *fakeSkills) { c.Role = "guest" }, "only an owner or admin can teach a skill"},
		{"user", func(_ *testing.T, c *tool.Call, _ *fakeSkills) { c.Role = "user" }, "only an owner or admin can teach a skill"},
		{"user in a room", func(_ *testing.T, c *tool.Call, _ *fakeSkills) {
			c.Role, c.Partition = "user", "group/telegram-home/-100123"
		}, "only an owner or admin can teach a skill"},
		{"no role", func(_ *testing.T, c *tool.Call, _ *fakeSkills) { c.Role = "" }, "only an owner or admin"},
		{"task run", func(_ *testing.T, c *tool.Call, _ *fakeSkills) { c.RunKind = tool.RunTask }, "not a task"},
		{"bad origin", func(_ *testing.T, c *tool.Call, _ *fakeSkills) {
			c.Args = json.RawMessage(strings.Replace(deployArgs, "correction", "discovery", 1))
		}, "\norigin: must be \"correction\""},
		{"bad json", func(_ *testing.T, c *tool.Call, _ *fakeSkills) { c.Args = json.RawMessage(`{"steps":"one"}`) }, "bad arguments"},
		{"unreadable agent skill", func(t *testing.T, _ *tool.Call, f *fakeSkills) {
			handSkill(t, f.root, "deploy", "bogus: key\n")
		}, `skill "deploy" cannot be read`},
		{"no root", func(_ *testing.T, _ *tool.Call, f *fakeSkills) { f.root = "" }, "skill.save failed"},
		{"agent not configured", func(_ *testing.T, _ *tool.Call, f *fakeSkills) {
			f.writerErr = errors.New(`skills: agent "x" is not configured`)
		}, `skill.save failed: skills: agent "x" is not configured`},
		{"trace not a ulid", func(_ *testing.T, c *tool.Call, _ *fakeSkills) { c.TraceID = "T1" }, "skill.save failed"},
		{"no user", func(_ *testing.T, c *tool.Call, _ *fakeSkills) { c.UserID = "" }, "skill.save failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newWriteRig(t)
			root := g.src.root
			c := ownerCall("skill.save", deployArgs)
			tc.mut(t, &c, g.src)
			before := tree(t, root)
			res := runSkill(t, g.save, c)
			if !res.IsError || !strings.Contains(res.Text, tc.want) {
				t.Fatalf("res = %+v, want %q", res, tc.want)
			}
			if len(g.events) != 0 {
				t.Fatalf("event on a refused save")
			}
			if after := tree(t, root); fmt.Sprint(after) != fmt.Sprint(before) {
				t.Fatalf("something was written: %v", after)
			}
		})
	}
}

// Every violation comes back in the one error, one per line, with its code.
func TestSkillSaveLint(t *testing.T) {
	g := newWriteRig(t)
	args := `{"name":"Deploy!","title":"","when":"` + strings.Repeat("w", skill.MaxDescription+1) + `",` +
		`"steps":["Run the tests.\nThen tag."],"pitfalls":["a","b","c","d","e","f","g"],"origin":"correction"}`
	res := runSkill(t, g.save, ownerCall("skill.save", args))
	if !res.IsError {
		t.Fatalf("res = %+v", res)
	}
	lines := strings.Split(res.Text, "\n")
	for _, code := range []skill.Code{
		skill.CodeBadName, skill.CodeTitle, skill.CodeDescription, skill.CodeStepCount,
		skill.CodePitfallCount, skill.CodeItemLength,
	} {
		found := false
		for _, l := range lines[1:] {
			found = found || strings.HasPrefix(l, string(code)+": ")
		}
		if !found {
			t.Errorf("no %s line in %q", code, res.Text)
		}
	}
	if !strings.Contains(res.Text, "memory.save") {
		t.Errorf("one step does not point at memory.save: %q", res.Text)
	}
	if len(g.events) != 0 {
		t.Fatal("event on a lint failure")
	}
}

func TestSkillForget(t *testing.T) {
	g := newWriteRig(t)
	for range 2 { // the second save replaces the first: one version in history/
		if res := runSkill(t, g.save, ownerCall("skill.save", deployArgs)); res.IsError {
			t.Fatal(res.Text)
		}
	}
	g.events = nil
	res := runSkill(t, g.forget, ownerCall("skill.forget", `{"name":" deploy "}`))
	if res.IsError || res.Text != "forgotten skill deploy" {
		t.Fatalf("res = %+v", res)
	}
	if len(g.agentSkills(t)) != 0 {
		t.Fatal("skill still loads after forget")
	}
	hist, err := os.ReadDir(filepath.Join(g.src.root, skill.TrashDir, "deploy", "history"))
	if err != nil || len(hist) != 1 {
		t.Fatalf("trashed history = %v, %v", hist, err)
	}
	if len(g.events) != 1 || g.events[0].Type != event.SkillForgotten {
		t.Fatalf("events = %+v", g.events)
	}
	e := g.events[0]
	if e.TraceID != saveTrace || e.SessionID != "S1" {
		t.Fatalf("event ids: %+v", e)
	}
	var attrs map[string]any
	if err := json.Unmarshal(e.Attrs, &attrs); err != nil {
		t.Fatal(err)
	}
	if len(attrs) != 3 || attrs["agent"] != "main" || attrs["name"] != "deploy" || attrs["versions"] != float64(1) {
		t.Errorf("attrs = %v", attrs)
	}
	res = runSkill(t, g.forget, ownerCall("skill.forget", `{"name":"deploy"}`))
	if res.IsError || res.Text != `no skill "deploy"; nothing to forget` {
		t.Fatalf("second forget = %+v", res)
	}
	if len(g.events) != 1 {
		t.Fatalf("a failed forget emitted events: %+v", g.events[1:])
	}
}

// Any agent skill can go to the trash, not only one the agent saved.
func TestSkillForgetHandWritten(t *testing.T) {
	g := newWriteRig(t)
	hand := handSkill(t, g.src.root, "release", "")
	c := ownerCall("skill.forget", `{"name":"release"}`)
	c.Role = "admin"
	if res := runSkill(t, g.forget, c); res.IsError {
		t.Fatal(res.Text)
	}
	b, err := os.ReadFile(filepath.Join(g.src.root, skill.TrashDir, "release", "SKILL.md"))
	if err != nil || string(b) != string(hand) {
		t.Fatalf("trashed = %q, %v", b, err)
	}
}

// A symlink in a skill's folder is reported as a folder a person must fix,
// by save and forget alike, and nothing is written through it.
func TestSkillWriteUnsafeFolder(t *testing.T) {
	g := newWriteRig(t)
	handSkill(t, g.src.root, "deploy", "")
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(g.src.root, "deploy", "history")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tl tool.Tool
		c  tool.Call
	}{
		{g.save, ownerCall("skill.save", deployArgs)},
		{g.forget, ownerCall("skill.forget", `{"name":"deploy"}`)},
	} {
		res := runSkill(t, tc.tl, tc.c)
		if !res.IsError || res.Text != `the folder of skill "deploy" is not safe to write (a symlink); a person must fix it` {
			t.Fatalf("%s: res = %+v", tc.c.Name, res)
		}
	}
	if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
		t.Fatalf("wrote through the symlink: %v (%v)", entries, err)
	}
	if len(g.events) != 0 {
		t.Fatalf("events: %+v", g.events)
	}
}

// Forgetting a skill that is not there does what the person asked, the skill
// is not used, so it is not an error; nothing moves and no event is emitted.
func TestSkillForgetMissing(t *testing.T) {
	for _, name := range []string{"deploy", "../deploy"} {
		g := newWriteRig(t)
		handSkill(t, g.src.root, "kept", "")
		before := tree(t, g.src.root)
		res := runSkill(t, g.forget, ownerCall("skill.forget", fmt.Sprintf(`{"name":%q}`, name)))
		if res.IsError || res.Text != fmt.Sprintf("no skill %q; nothing to forget", name) {
			t.Fatalf("%s: res = %+v", name, res)
		}
		if len(g.events) != 0 {
			t.Fatalf("%s: events %+v", name, g.events)
		}
		if after := tree(t, g.src.root); fmt.Sprint(after) != fmt.Sprint(before) {
			t.Fatalf("%s: something moved: %v", name, after)
		}
	}
}

func TestSkillForgetRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
		mut  func(*tool.Call, *fakeSkills)
		want string
	}{
		{"bundled skill", `{"name":"deploy"}`, func(_ *tool.Call, f *fakeSkills) {
			f.skills = []skill.Skill{{Name: "deploy", Level: skill.LevelBundled}}
		}, `skill "deploy" is bundled with holaclaw; it cannot be forgotten`},
		{"missing, skills unavailable", `{"name":"deploy"}`, func(_ *tool.Call, f *fakeSkills) { f.err = errors.New("boom") },
			"skills are unavailable right now; nothing was forgotten"},
		{"no name", `{}`, func(*tool.Call, *fakeSkills) {}, "name is required"},
		{"bad json", `[`, func(*tool.Call, *fakeSkills) {}, "bad arguments"},
		{"guest", `{"name":"deploy"}`, func(c *tool.Call, _ *fakeSkills) { c.Role = "guest" }, "only an owner or admin can forget a skill"},
		{"user", `{"name":"deploy"}`, func(c *tool.Call, _ *fakeSkills) { c.Role = "user" }, "only an owner or admin can forget a skill"},
		{"task run", `{"name":"deploy"}`, func(c *tool.Call, _ *fakeSkills) { c.RunKind = tool.RunTask }, "not a task"},
		{"writer fails", `{"name":"deploy"}`, func(_ *tool.Call, f *fakeSkills) { f.root = "" }, "skill.forget failed"},
		{"agent not configured", `{"name":"deploy"}`, func(_ *tool.Call, f *fakeSkills) {
			f.writerErr = errors.New(`skills: agent "x" is not configured`)
		}, `skill.forget failed: skills: agent "x" is not configured`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newWriteRig(t)
			handSkill(t, g.src.root, "kept", "")
			before := tree(t, g.src.root)
			c := ownerCall("skill.forget", tc.args)
			root := g.src.root
			tc.mut(&c, g.src)
			res := runSkill(t, g.forget, c)
			if !res.IsError || !strings.Contains(res.Text, tc.want) {
				t.Fatalf("res = %+v, want %q", res, tc.want)
			}
			if len(g.events) != 0 {
				t.Fatalf("a refused forget emitted events: %+v", g.events)
			}
			if after := tree(t, root); fmt.Sprint(after) != fmt.Sprint(before) {
				t.Fatalf("something moved: %v", after)
			}
		})
	}
}

func TestSkillWriteSchemas(t *testing.T) {
	r := tool.NewRegistry()
	if err := RegisterSkill(r, &fakeSkills{}, nil); err != nil {
		t.Fatal(err)
	}
	ex, err := r.Select([]string{"skill"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range ex.Specs() {
		got = append(got, s.Name)
		n := tool.SchemaTokens(s)
		if n > tool.MaxSchemaTokens {
			t.Fatalf("%s: %d tokens", s.Name, n)
		}
		t.Logf("%s: %d tokens", s.Name, n)
	}
	if strings.Join(got, ",") != "skill.forget,skill.load,skill.save" {
		t.Fatalf("skill group = %v", got)
	}
	for _, tl := range []tool.Tool{&skillSave{}, &skillForget{}} {
		if tl.Risk() != tool.RiskWrite {
			t.Fatalf("%s: risk %v", tl.Spec().Name, tl.Risk())
		}
	}
}

// With no required list in the schema, an empty call gets every missing
// field back at once.
func TestSkillSaveEmptyCall(t *testing.T) {
	g := newWriteRig(t)
	res := runSkill(t, g.save, ownerCall("skill.save", `{}`))
	if !res.IsError {
		t.Fatalf("res = %+v", res)
	}
	for _, want := range []string{"\norigin: ", "\n" + string(skill.CodeBadName) + ": ", "\n" + string(skill.CodeTitle) + ": ",
		"\n" + string(skill.CodeDescription) + ": ", "\n" + string(skill.CodeStepCount) + ": "} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("no %q in %q", want, res.Text)
		}
	}
}
