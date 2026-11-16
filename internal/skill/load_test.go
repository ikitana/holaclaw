package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

const fixtures = "../../testdata/skills"

func skillFile(name, desc string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\nbody of " + name + "\n"
}

func bundledFixture() fstest.MapFS {
	return fstest.MapFS{
		"summarize/SKILL.md":                  {Data: []byte(skillFile("summarize", "Bundled summarize."))},
		"writing-skills/SKILL.md":             {Data: []byte(skillFile("writing-skills", "When saving a skill."))},
		"writing-skills/references/guide.md":  {Data: []byte("guide")},
		"writing-skills/references/b/deep.md": {Data: []byte("deep")},
		"writing-skills/references/.draft.md": {Data: []byte("hidden")},
		"writing-skills/scripts/run.sh":       {Data: []byte("echo")},
		"bad/references/x.md":                 {Data: []byte("not listed")},
		"bad/SKILL.md":                        {Data: []byte("no frontmatter")},
		"empty/README.md":                     {Data: []byte("not a skill")},
		".keep":                               {Data: nil},
	}
}

func loadFixtures(t *testing.T) (Set, []Problem) {
	t.Helper()
	return Load(Roots{
		Bundled: bundledFixture(),
		Agent:   filepath.Join(fixtures, "agent"),
	})
}

func keys(sks []Skill) string {
	var out []string
	for _, sk := range sks {
		out = append(out, string(sk.Level)+":"+sk.Name)
	}
	return strings.Join(out, " ")
}

func TestLoadFixtures(t *testing.T) {
	set, probs := loadFixtures(t)
	want := "bundled:summarize bundled:writing-skills " +
		"agent:pr-style agent:release agent:summarize"
	if got := keys(set.All()); got != want {
		t.Fatalf("All =\n%s\nwant\n%s", got, want)
	}
	if len(probs) != 2 {
		t.Fatalf("problems = %+v", probs)
	}
	agentRoot, _ := filepath.Abs(filepath.Join(fixtures, "agent"))
	if p := probs[0]; p.Code != CodeUnknownKey || p.Dir != filepath.Join(agentRoot, "broken") || !strings.Contains(p.Msg, "field version") {
		t.Fatalf("problem 0 = %+v", p)
	}
	if p := probs[1]; p.Code != CodeNoFrontmatter || p.Dir != "bundled/bad" {
		t.Fatalf("problem 1 = %+v", p)
	}
	for _, sk := range set.All() {
		if sk.Level != LevelBundled && !filepath.IsAbs(sk.Dir) {
			t.Fatalf("%s: dir %q is not absolute", sk.Name, sk.Dir)
		}
	}
}

func TestLoadDeterministic(t *testing.T) {
	a, pa := loadFixtures(t)
	for range 5 {
		b, pb := loadFixtures(t)
		if keys(a.All()) != keys(b.All()) || len(pa) != len(pb) {
			t.Fatal("Load output changed between runs")
		}
	}
}

func TestResolveShadowing(t *testing.T) {
	set, _ := loadFixtures(t)
	// An agent skill shadows a bundled one by name.
	want := "agent:pr-style agent:release agent:summarize bundled:writing-skills"
	if got := keys(set.Resolve()); got != want {
		t.Fatalf("Resolve =\n%s\nwant\n%s", got, want)
	}
	if sk, ok := set.Get("summarize"); !ok || sk.Level != LevelAgent {
		t.Fatalf("Get(summarize) = %+v, %v", sk, ok)
	}
	if sk, ok := set.Get("writing-skills"); !ok || sk.Level != LevelBundled {
		t.Fatalf("Get(writing-skills) = %+v, %v", sk, ok)
	}
	if _, ok := set.Get("nope"); ok {
		t.Fatal("Get found a missing skill")
	}
	if len(Set{}.Resolve()) != 0 {
		t.Fatal("empty set resolved to skills")
	}
}

// TestResolveSavedShadowsBundled: a skill the agent saved under a bundled
// skill's name is an agent skill and shadows it like any other.
func TestResolveSavedShadowsBundled(t *testing.T) {
	agent := t.TempDir()
	write(t, filepath.Join(agent, "writing-skills", "SKILL.md"), learnedFile("writing-skills"))
	set, probs := Load(Roots{Bundled: bundledFixture(), Agent: agent})
	if len(probs) != 1 || probs[0].Dir != "bundled/bad" {
		t.Fatalf("problems = %+v", probs)
	}
	if got := keys(set.All()); got != "bundled:summarize bundled:writing-skills agent:writing-skills" {
		t.Fatalf("All = %s", got)
	}
	sk, ok := set.Get("writing-skills")
	if !ok || sk.Level != LevelAgent || sk.Learned == nil || sk.Learned.By != goodBy {
		t.Fatalf("Get(writing-skills) = %+v, %v", sk, ok)
	}
	if got := keys(set.Resolve()); got != "bundled:summarize agent:writing-skills" {
		t.Fatalf("Resolve = %s", got)
	}
}

func TestLoadMissingRoots(t *testing.T) {
	dir := t.TempDir()
	set, probs := Load(Roots{Agent: filepath.Join(dir, "none")})
	if len(set.All()) != 0 || len(probs) != 0 {
		t.Fatalf("got %v, %v", set.All(), probs)
	}
	set, probs = Load(Roots{})
	if len(set.All()) != 0 || len(probs) != 0 {
		t.Fatalf("empty roots: %v, %v", set.All(), probs)
	}
}

func TestBundledEmbedIsEmpty(t *testing.T) {
	set, probs := Load(Roots{Bundled: Bundled})
	if len(set.All()) != 0 || len(probs) != 0 {
		t.Fatalf("bundled root not empty: %v, %v", set.All(), probs)
	}
}

// TestLoadBundledLearned: a bundled skill carrying a learned block is refused.
func TestLoadBundledLearned(t *testing.T) {
	set, probs := Load(Roots{Bundled: fstest.MapFS{"x/SKILL.md": {Data: []byte(learnedFile("x"))}}})
	if len(set.All()) != 0 || len(probs) != 1 || probs[0].Code != CodeLearnedNotAllow || probs[0].Dir != "bundled/x" {
		t.Fatalf("got %v, %+v", set.All(), probs)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadProblemsOnDisk(t *testing.T) {
	root := t.TempDir()
	agent := filepath.Join(root, "agent")
	// SKL110: SKILL.md is a directory.
	if err := os.MkdirAll(filepath.Join(agent, "unreadable", "SKILL.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	// SKL108: a file over the read cap.
	write(t, filepath.Join(agent, "huge", "SKILL.md"), strings.Repeat("x", maxFile+1))
	// SKL106: name differs from the directory.
	write(t, filepath.Join(agent, "wrong", "SKILL.md"), skillFile("right", "When x."))
	// SKL105: a saved skill whose teacher is not an identity id.
	write(t, filepath.Join(agent, "bad-by", "SKILL.md"), strings.Replace(learnedFile("bad-by"), goodBy, "angel", 1))
	// Hidden directories are skipped, the trash included.
	write(t, filepath.Join(agent, ".git", "SKILL.md"), "junk")
	write(t, filepath.Join(agent, ".trash", "old", "SKILL.md"), skillFile("old", "When x."))
	// A symlinked skill directory is not followed.
	write(t, filepath.Join(root, "outside", "linked", "SKILL.md"), skillFile("linked", "When x."))
	if err := os.Symlink(filepath.Join(root, "outside", "linked"), filepath.Join(agent, "linked")); err != nil {
		t.Fatal(err)
	}
	// A skill one level down is not a skill: folders are direct children.
	write(t, filepath.Join(agent, "dm", "u1", "deep", "SKILL.md"), learnedFile("deep"))
	// A saved skill with its history, which is never read as a skill.
	write(t, filepath.Join(agent, "ok", "SKILL.md"), learnedFile("ok"))
	write(t, filepath.Join(agent, "ok", "history", "nested", "SKILL.md"), "junk")

	set, probs := Load(Roots{Agent: agent})
	if got := keys(set.All()); got != "agent:ok" {
		t.Fatalf("All = %s", got)
	}
	want := map[string]Code{
		filepath.Join(agent, "bad-by"):     CodeBadValue,
		filepath.Join(agent, "huge"):       CodeTooLong,
		filepath.Join(agent, "unreadable"): CodeUnreadable,
		filepath.Join(agent, "wrong"):      CodeBadName,
	}
	if len(probs) != len(want) {
		t.Fatalf("problems = %+v", probs)
	}
	for i, p := range probs {
		if want[p.Dir] != p.Code {
			t.Errorf("problem %+v, want code %s", p, want[p.Dir])
		}
		if i > 0 && probs[i-1].Dir > p.Dir {
			t.Error("problems not sorted by dir")
		}
	}
}

func learnedFile(name string) string {
	return "---\nname: " + name + "\ndescription: When x.\nmetadata:\n  holaclaw:\n    version: 1\n    learned:\n" +
		"      on: 2026-09-12\n      by: " + goodBy + "\n      from: " + goodULID + "\n      origin: correction\n---\nbody\n"
}

func TestLoadListsFiles(t *testing.T) {
	set, _ := loadFixtures(t)
	cases := []struct {
		name, refs, scripts string
	}{
		{"pr-style", "checklist.md", "check.sh"},
		{"writing-skills", "b/deep.md guide.md", "run.sh"},
		{"release", "", ""},
	}
	for _, c := range cases {
		sk, ok := set.Get(c.name)
		if !ok {
			t.Fatalf("%s missing", c.name)
		}
		if got := strings.Join(sk.References, " "); got != c.refs {
			t.Errorf("%s references = %q, want %q", c.name, got, c.refs)
		}
		if got := strings.Join(sk.Scripts, " "); got != c.scripts {
			t.Errorf("%s scripts = %q, want %q", c.name, got, c.scripts)
		}
	}
	// Each listed reference is readable through Reference.
	sk, _ := set.Get("writing-skills")
	for _, name := range sk.References {
		if _, err := sk.Reference(name); err != nil {
			t.Errorf("Reference(%q) = %v", name, err)
		}
	}
	// Parse alone lists nothing.
	b, err := os.ReadFile(filepath.Join(fixtures, "agent", "pr-style", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if sk, err := Parse(LevelAgent, "pr-style", b); err != nil || sk.References != nil || sk.Scripts != nil {
		t.Fatalf("Parse = %+v, %v", sk, err)
	}
}

func TestLoadListsFilesOnDisk(t *testing.T) {
	root := t.TempDir()
	agent := filepath.Join(root, "agent")
	outside := filepath.Join(root, "outside")
	write(t, filepath.Join(outside, "secret.md"), "x")
	write(t, filepath.Join(outside, "dir", "inner.md"), "x")
	symlink := func(target, link string) {
		t.Helper()
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}

	sk := filepath.Join(agent, "tree")
	write(t, filepath.Join(sk, "SKILL.md"), skillFile("tree", "When x."))
	for _, f := range []string{"z.md", "a-b.md", "a/b.md", "a/c/d.md", ".hidden.md", ".git/config", "a/.cache/x.md"} {
		write(t, filepath.Join(sk, "references", filepath.FromSlash(f)), "x")
	}
	symlink(filepath.Join(outside, "secret.md"), filepath.Join(sk, "references", "link.md"))
	symlink(filepath.Join(outside, "dir"), filepath.Join(sk, "references", "linkdir"))
	// A symlinked scripts/ is not followed at all.
	symlink(filepath.Join(outside, "dir"), filepath.Join(sk, "scripts"))

	many := filepath.Join(agent, "many")
	write(t, filepath.Join(many, "SKILL.md"), learnedFile("many"))
	write(t, filepath.Join(many, "history", "01JA0000000000000000000000.md"), "old")
	for i := range MaxListed + 5 {
		write(t, filepath.Join(many, "scripts", fmt.Sprintf("s%03d.sh", i)), "x")
	}
	// scripts/ is a file, not a folder.
	write(t, filepath.Join(agent, "flat", "SKILL.md"), skillFile("flat", "When x."))
	write(t, filepath.Join(agent, "flat", "scripts"), "x")

	set, probs := Load(Roots{Agent: agent})
	if len(probs) != 0 {
		t.Fatalf("problems = %+v", probs)
	}
	got, _ := set.Get("tree")
	if refs := strings.Join(got.References, " "); refs != "a-b.md a/b.md a/c/d.md z.md" || got.Scripts != nil {
		t.Fatalf("tree: references = %q, scripts = %v", refs, got.Scripts)
	}
	got, _ = set.Get("many")
	if len(got.Scripts) != MaxListed || got.Scripts[0] != "s000.sh" || got.Scripts[MaxListed-1] != fmt.Sprintf("s%03d.sh", MaxListed-1) {
		t.Fatalf("many: %d scripts, %v", len(got.Scripts), got.Scripts[:3])
	}
	if got.References != nil {
		t.Fatalf("many: history listed as references: %v", got.References)
	}
	if got, _ = set.Get("flat"); got.Scripts != nil || got.References != nil {
		t.Fatalf("flat: %+v", got)
	}
}

// TestLoadAgentRootUnreadable: a root that exists but cannot be listed is a
// problem, not an empty root.
func TestLoadAgentRootUnreadable(t *testing.T) {
	f := filepath.Join(t.TempDir(), "skills")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	set, probs := Load(Roots{Agent: f})
	if len(set.All()) != 0 || len(probs) != 1 || probs[0].Code != CodeUnreadable || probs[0].Dir != f {
		t.Fatalf("got %v, %+v", set.All(), probs)
	}
}

func TestTrashed(t *testing.T) {
	root := t.TempDir()
	if sks, probs := Trashed(root); len(sks) != 0 || len(probs) != 0 {
		t.Fatalf("no trash: %v %v", sks, probs)
	}
	trash := filepath.Join(root, TrashDir)
	write(t, filepath.Join(trash, "ok", "SKILL.md"), learnedFile("ok"))
	write(t, filepath.Join(trash, "ok", "references", "notes.md"), "x")
	write(t, filepath.Join(trash, "two", "SKILL.md"), learnedFile("two"))
	write(t, filepath.Join(trash, "broken", "SKILL.md"), "no frontmatter")
	write(t, filepath.Join(trash, "deep", "nested", "SKILL.md"), learnedFile("nested"))
	write(t, filepath.Join(root, "live", "SKILL.md"), learnedFile("live"))

	sks, probs := Trashed(root)
	if got := keys(sks); got != "agent:ok agent:two" {
		t.Fatalf("Trashed = %s", got)
	}
	if sks[0].Dir != filepath.Join(trash, "ok") || len(sks[0].References) != 1 || sks[0].Learned == nil {
		t.Errorf("ok = %+v", sks[0])
	}
	if len(probs) != 1 || probs[0].Dir != filepath.Join(trash, "broken") || probs[0].Code != CodeNoFrontmatter {
		t.Errorf("problems = %+v", probs)
	}
	if set, _ := Load(Roots{Agent: root}); keys(set.All()) != "agent:live" {
		t.Errorf("Load entered the trash: %s", keys(set.All()))
	}
}

// TestTrashedUnreadable: a .trash that is not a directory is a problem, not
// an empty trash.
func TestTrashedUnreadable(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, TrashDir), "not a folder")
	sks, probs := Trashed(root)
	if len(sks) != 0 || len(probs) != 1 || probs[0].Code != CodeUnreadable || probs[0].Dir != filepath.Join(root, TrashDir) {
		t.Fatalf("got %v, %+v", sks, probs)
	}
}
