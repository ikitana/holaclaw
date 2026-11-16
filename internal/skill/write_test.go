package skill

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func rendered(t *testing.T, edit func(*Draft)) []byte {
	t.Helper()
	d := goodDraft()
	if edit != nil {
		edit(&d)
	}
	b, err := Render(d, goodLearned(), Requires{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// versionSkill is the SKILL.md of version n in dir's history/.
func versionSkill(dir string, n int) string {
	return filepath.Join(dir, "history", versionDir(n), "SKILL.md")
}

// seedVersion writes version n into hist as a folder holding a SKILL.md.
func seedVersion(t *testing.T, hist string, n int, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(hist, versionDir(n)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hist, versionDir(n), "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// treeFiles lists the regular files under dir as sorted slash paths, joined
// by spaces.
func treeFiles(t *testing.T, dir string) string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// historyFiles lists history/ of dir.
func historyFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "history"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestSaveNewAndReplace(t *testing.T) {
	root := t.TempDir()
	w := Writer{Root: root}
	first := rendered(t, nil)
	s, err := w.Save("pr-style", first)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "pr-style")
	if s.Dir != want || s.Replaced != 0 || s.Version != 1 || s.Versions != 0 {
		t.Fatalf("Saved = %+v", s)
	}
	b, err := os.ReadFile(filepath.Join(want, "SKILL.md"))
	if err != nil || !bytes.Equal(b, first) {
		t.Fatalf("SKILL.md = %q, %v", b, err)
	}
	if st, _ := os.Stat(filepath.Join(want, "SKILL.md")); st.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", st.Mode())
	}

	// The rest of the folder belongs to the version too, except hidden entries.
	write(t, filepath.Join(want, "references", "checklist.md"), "check v1")
	write(t, filepath.Join(want, "scripts", "run.sh"), "echo v1")
	write(t, filepath.Join(want, ".holaclaw", "origin.toml"), "source = 'x'")

	second := rendered(t, func(d *Draft) { d.When = "When opening any pull request" })
	s, err = w.Save("pr-style", second)
	if err != nil {
		t.Fatal(err)
	}
	if s.Replaced != 1 || s.Version != 2 || s.Versions != 1 {
		t.Fatalf("Saved = %+v", s)
	}
	if h := historyFiles(t, want); len(h) != 1 || h[0] != "1" {
		t.Fatalf("history = %v", h)
	}
	b, _ = os.ReadFile(versionSkill(want, 1))
	if !bytes.Equal(b, first) {
		t.Errorf("history version = %q", b)
	}
	if got := treeFiles(t, filepath.Join(want, "history", "1")); got != "SKILL.md references/checklist.md scripts/run.sh" {
		t.Errorf("version 1 holds %s", got)
	}
	b, _ = os.ReadFile(filepath.Join(want, "SKILL.md"))
	if !bytes.Equal(b, second) {
		t.Errorf("SKILL.md = %q", b)
	}
	// The live folder keeps its references, scripts and .holaclaw/.
	for p, c := range map[string]string{
		"references/checklist.md": "check v1", "scripts/run.sh": "echo v1", ".holaclaw/origin.toml": "source = 'x'",
	} {
		if b, err := os.ReadFile(filepath.Join(want, p)); err != nil || string(b) != c {
			t.Errorf("live %s = %q, %v", p, b, err)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(want, ".SKILL-*")); len(m) != 0 {
		t.Errorf("temp files left: %v", m)
	}
	if m, _ := filepath.Glob(filepath.Join(want, "history", ".retire-*")); len(m) != 0 {
		t.Errorf("retire folders left: %v", m)
	}
}

func TestSaveNumbers(t *testing.T) {
	w := Writer{Root: t.TempDir()}
	for i := range 3 {
		next, err := w.Next("pr-style")
		if err != nil || next != i+1 {
			t.Fatalf("save %d: Next = %d, %v", i, next, err)
		}
		s, err := w.Save("pr-style", []byte{byte('a' + i)})
		if err != nil {
			t.Fatal(err)
		}
		if s.Replaced != i || s.Version != i+1 {
			t.Fatalf("save %d: Saved = %+v", i, s)
		}
	}
	if h := historyFiles(t, filepath.Join(w.Root, "pr-style")); strings.Join(h, " ") != "1 2" {
		t.Fatalf("history = %v", h)
	}
}

// TestSaveNumbersFromHistory: the numbers come from history/, not from the
// files: with no current file the next version follows the newest, and a
// hand-edited current file keeps its number.
func TestSaveNumbersFromHistory(t *testing.T) {
	w := Writer{Root: t.TempDir()}
	hist := filepath.Join(w.Root, "pr-style", "history")
	if err := os.MkdirAll(hist, 0o755); err != nil {
		t.Fatal(err)
	}
	seedVersion(t, hist, 3, "old")
	seedVersion(t, hist, 10, "old")
	// Not versions: files, even one named like a version.
	for _, n := range []string{"notes.txt", "12", "3.md"} {
		if err := os.WriteFile(filepath.Join(hist, n), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if next, err := w.Next("pr-style"); err != nil || next != 11 {
		t.Fatalf("Next = %d, %v", next, err)
	}
	s, err := w.Save("pr-style", []byte("a"))
	if err != nil || s.Replaced != 0 || s.Version != 11 || s.Versions != 2 {
		t.Fatalf("Saved = %+v, %v", s, err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "SKILL.md"), []byte("hand edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if next, err := w.Next("pr-style"); err != nil || next != 12 {
		t.Fatalf("Next after a hand edit = %d, %v", next, err)
	}
	if s, err = w.Save("pr-style", []byte("b")); err != nil || s.Replaced != 11 || s.Version != 12 {
		t.Fatalf("Saved = %+v, %v", s, err)
	}
	if b, err := os.ReadFile(versionSkill(s.Dir, 11)); err != nil || string(b) != "hand edit" {
		t.Fatalf("11/SKILL.md = %q, %v", b, err)
	}
}

func TestNextRefuses(t *testing.T) {
	if _, err := (Writer{Root: t.TempDir()}).Next("Bad Name"); !errors.Is(err, ErrBadName) {
		t.Errorf("bad name: %v", err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pr-style"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pr-style", "history"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (Writer{Root: root}).Next("pr-style"); err == nil {
		t.Error("history a file: no error")
	}
}

func TestSaveTrimsHistory(t *testing.T) {
	root := t.TempDir()
	w := Writer{Root: root}
	var replaced []int
	var s Saved
	var err error
	saves := MaxVersions + 2
	for i := range saves {
		if i == 1 {
			if err := os.MkdirAll(filepath.Join(s.Dir, "history"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(s.Dir, "history", "notes.txt"), []byte("mine"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if s, err = w.Save("pr-style", []byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
		if s.Replaced != 0 {
			replaced = append(replaced, s.Replaced)
		}
	}
	// MaxVersions+1 replaced versions, the oldest one trimmed.
	if len(replaced) != saves-1 || s.Versions != MaxVersions {
		t.Fatalf("replaced %d, Versions %d", len(replaced), s.Versions)
	}
	h := historyFiles(t, s.Dir)
	if len(h) != MaxVersions+1 || !strings.Contains(strings.Join(h, " "), "notes.txt") {
		t.Fatalf("history has %d entries", len(h))
	}
	// The oldest is trimmed, folder and all, and the newest is current.
	if _, err := os.Stat(filepath.Join(s.Dir, "history", versionDir(replaced[0]))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("oldest version kept: %v", err)
	}
	b, _ := os.ReadFile(versionSkill(s.Dir, replaced[1]))
	if !bytes.Equal(b, []byte{1}) {
		t.Errorf("oldest kept version = %v, want [1]", b)
	}
}

func TestSaveTrimsTwoOldest(t *testing.T) {
	// MaxVersions+1 seeded versions plus the one this save replaces: the 2
	// oldest go.
	root := t.TempDir()
	w := Writer{Root: root}
	s, err := w.Save("pr-style", []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	hist := filepath.Join(s.Dir, "history")
	if err := os.MkdirAll(hist, 0o755); err != nil {
		t.Fatal(err)
	}
	var seeded []string
	for i := range MaxVersions + 1 {
		seeded = append(seeded, versionDir(i+1))
		seedVersion(t, hist, i+1, "old")
	}
	if err := os.WriteFile(filepath.Join(hist, "notes.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s, err = w.Save("pr-style", []byte("y")); err != nil {
		t.Fatal(err)
	}
	if s.Versions != MaxVersions || s.Replaced != MaxVersions+2 || s.Version != MaxVersions+3 {
		t.Fatalf("Saved = %+v", s)
	}
	for _, gone := range seeded[:2] {
		if _, err := os.Stat(filepath.Join(hist, gone)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s kept", gone)
		}
	}
	if _, err := os.Stat(filepath.Join(hist, versionDir(s.Replaced))); err != nil {
		t.Errorf("newest version trimmed: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(hist, "notes.txt")); err != nil || string(b) != "mine" {
		t.Errorf("foreign file: %q, %v", b, err)
	}
	if n := len(historyFiles(t, s.Dir)); n != MaxVersions+1 {
		t.Errorf("history has %d entries", n)
	}
}

func TestSaveLoadForget(t *testing.T) {
	root := t.TempDir()
	w := Writer{Root: root}
	if _, err := w.Save("pr-style", rendered(t, nil)); err != nil {
		t.Fatal(err)
	}
	set, probs := Load(Roots{Agent: root})
	if len(probs) != 0 {
		t.Fatalf("problems: %v", probs)
	}
	sk, ok := set.Get("pr-style")
	if !ok || sk.Level != LevelAgent || sk.Learned == nil {
		t.Fatalf("Get = %+v, %v", sk, ok)
	}

	if _, err := w.Save("pr-style", []byte("second")); err != nil {
		t.Fatal(err)
	}

	n, err := w.Forget("pr-style")
	if err != nil || n != 1 {
		t.Fatalf("Forget = %d, %v", n, err)
	}
	if _, err := os.Stat(sk.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("skill folder still there: %v", err)
	}
	trashed := filepath.Join(root, TrashDir, "pr-style")
	if b, err := os.ReadFile(filepath.Join(trashed, "SKILL.md")); err != nil || string(b) != "second" {
		t.Errorf("trashed SKILL.md = %q, %v", b, err)
	}
	if h := historyFiles(t, trashed); len(h) != 1 {
		t.Errorf("trashed history = %v", h)
	}
	set, probs = Load(Roots{Agent: root})
	if len(set.All()) != 0 || len(probs) != 0 {
		t.Fatalf("after forget: %v, %v", set.All(), probs)
	}
	if _, err := w.Forget("pr-style"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second Forget = %v", err)
	}

	// A save after forget starts a fresh folder and leaves the trash alone.
	s, err := w.Save("pr-style", []byte("third"))
	if err != nil || s.Replaced != 0 || s.Version != 1 || s.Versions != 0 {
		t.Fatalf("Save after forget = %+v, %v", s, err)
	}
	if h := historyFiles(t, trashed); len(h) != 1 {
		t.Errorf("trashed history after save = %v", h)
	}
	// Forgetting it again replaces the trashed copy.
	if n, err := w.Forget("pr-style"); err != nil || n != 0 {
		t.Fatalf("Forget again = %d, %v", n, err)
	}
	if b, err := os.ReadFile(filepath.Join(trashed, "SKILL.md")); err != nil || string(b) != "third" {
		t.Errorf("trashed SKILL.md = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(trashed, "history")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old trashed history survived: %v", err)
	}
	if _, probs := Load(Roots{Agent: root}); len(probs) != 0 {
		t.Fatalf("problems with a trash: %v", probs)
	}
}

// TestLoadSkipsTrash: a well-formed skill under .trash is neither indexed nor
// reported.
func TestLoadSkipsTrash(t *testing.T) {
	root := t.TempDir()
	w := Writer{Root: root}
	if _, err := w.Save("pr-style", rendered(t, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Forget("pr-style"); err != nil {
		t.Fatal(err)
	}
	// Also a broken skill folder straight under the trash.
	if err := os.MkdirAll(filepath.Join(root, TrashDir, "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, TrashDir, "other", "SKILL.md"), []byte("not a skill"), 0o644); err != nil {
		t.Fatal(err)
	}
	set, probs := Load(Roots{Agent: root})
	if len(set.All()) != 0 || len(probs) != 0 {
		t.Fatalf("Load = %v, %v", set.All(), probs)
	}
}

func TestForgetMissing(t *testing.T) {
	root := t.TempDir()
	w := Writer{Root: root}
	if _, err := w.Forget("nothing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Forget = %v", err)
	}
	assertEmpty(t, root)
}

func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("%s: %v, %v", dir, entries, err)
	}
}

func TestWriterRefusesBadInput(t *testing.T) {
	for _, name := range []string{"../x", "..", ".trash", "PR", "", "a/b"} {
		root := t.TempDir()
		w := Writer{Root: root}
		if _, err := w.Save(name, []byte("x")); !errors.Is(err, ErrBadName) {
			t.Errorf("Save(%q) = %v", name, err)
		}
		if _, err := w.Forget(name); !errors.Is(err, ErrBadName) {
			t.Errorf("Forget(%q) = %v", name, err)
		}
		if _, ok, err := w.Current(name); ok || !errors.Is(err, ErrBadName) {
			t.Errorf("Current(%q) = %v, %v", name, ok, err)
		}
		assertEmpty(t, root)
	}
	if _, err := (Writer{}).Save("pr-style", []byte("x")); err == nil {
		t.Error("Save with no root succeeded")
	}
}

func TestWriterRefusesSymlinks(t *testing.T) {
	cases := map[string]func(root, target string) string{
		"skill folder": func(root, target string) string {
			return filepath.Join(root, "pr-style")
		},
		"history": func(root, target string) string {
			return filepath.Join(root, "pr-style", "history")
		},
	}
	for name, link := range cases {
		t.Run(name, func(t *testing.T) {
			root, target := t.TempDir(), t.TempDir()
			w := Writer{Root: root}
			if name == "history" {
				if _, err := w.Save("pr-style", []byte("a")); err != nil {
					t.Fatal(err)
				}
			}
			l := link(root, target)
			if err := os.MkdirAll(filepath.Dir(l), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, l); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Save("pr-style", []byte("b")); !errors.Is(err, ErrUnsafePath) {
				t.Errorf("Save = %v", err)
			}
			if _, err := w.Forget("pr-style"); !errors.Is(err, ErrUnsafePath) {
				t.Errorf("Forget = %v", err)
			}
			assertEmpty(t, target)
		})
	}
}

func TestForgetRefusesTrashSymlinks(t *testing.T) {
	for _, rel := range []string{TrashDir, TrashDir + "/pr-style"} {
		t.Run(rel, func(t *testing.T) {
			root, target := t.TempDir(), t.TempDir()
			w := Writer{Root: root}
			s, err := w.Save("pr-style", []byte("a"))
			if err != nil {
				t.Fatal(err)
			}
			l := filepath.Join(root, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(l), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, l); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Forget("pr-style"); !errors.Is(err, ErrUnsafePath) {
				t.Errorf("Forget = %v", err)
			}
			assertEmpty(t, target)
			assertOnly(t, s.Dir, "a")
		})
	}
}

func TestSaveUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	t.Run("skill folder", func(t *testing.T) {
		w := Writer{Root: t.TempDir()}
		s, err := w.Save("pr-style", []byte("a"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(s.Dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(s.Dir, 0o755) })
		if _, err := w.Save("pr-style", []byte("b")); err == nil || !strings.Contains(err.Error(), "skill: save ") {
			t.Fatalf("Save = %v", err)
		}
		assertOnly(t, s.Dir, "a")
	})
	t.Run("history", func(t *testing.T) {
		// The temp file is created, then moving the current version fails.
		w := Writer{Root: t.TempDir()}
		s, err := w.Save("pr-style", []byte("a"))
		if err != nil {
			t.Fatal(err)
		}
		hist := filepath.Join(s.Dir, "history")
		if err := os.Mkdir(hist, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(hist, 0o755) })
		if _, err := w.Save("pr-style", []byte("b")); err == nil {
			t.Fatal("Save succeeded")
		}
		assertOnly(t, s.Dir, "a")
	})
}

// assertOnly checks that dir holds SKILL.md with content want and no temp file.
func assertOnly(t *testing.T, dir, want string) {
	t.Helper()
	if b, err := os.ReadFile(filepath.Join(dir, "SKILL.md")); err != nil || string(b) != want {
		t.Errorf("SKILL.md = %q, %v", b, err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".SKILL-*")); len(m) != 0 {
		t.Errorf("temp files left: %v", m)
	}
}

// A hand-written or installed SKILL.md is retired into history/ like any
// other version, so a save never loses a hand edit (docs/skills.md §1, §6).
func TestSaveRetiresHandEdit(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pr-style")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	hand := []byte("---\nname: pr-style\ndescription: When opening a pull request\n---\n# Mine\n")
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), hand, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Writer{Root: root}.Save("pr-style", rendered(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if s.Replaced != 1 || s.Version != 2 || s.Versions != 1 {
		t.Fatalf("Saved = %+v", s)
	}
	if b, err := os.ReadFile(versionSkill(dir, 1)); err != nil || !bytes.Equal(b, hand) {
		t.Fatalf("history holds %q, %v", b, err)
	}
}

// When the version folder cannot take its name, the current SKILL.md goes
// back where it was and no half version is left in history/.
func TestSaveRetireFailsCleanly(t *testing.T) {
	w := Writer{Root: t.TempDir()}
	s, err := w.Save("pr-style", []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(s.Dir, "references", "r.md"), "ref")
	// A file where version 1's folder goes: not a version, and in the way.
	write(t, filepath.Join(s.Dir, "history", "1"), "in the way")
	if _, err := w.Save("pr-style", []byte("b")); err == nil {
		t.Fatal("Save over a blocked version name succeeded")
	}
	assertOnly(t, s.Dir, "a")
	if got := treeFiles(t, s.Dir); got != "SKILL.md history/1 references/r.md" {
		t.Fatalf("folder holds %s", got)
	}
}

func TestCurrent(t *testing.T) {
	root := t.TempDir()
	w := Writer{Root: root}
	if sk, ok, err := w.Current("pr-style"); ok || err != nil || sk.Name != "" {
		t.Fatalf("missing: %+v, %v, %v", sk, ok, err)
	}
	assertEmpty(t, root)

	req := Requires{Tools: []string{"exec"}, Bins: []Bin{{Name: "gh", Pkg: "gh"}}, Net: []string{"gh"}}
	b, err := Render(goodDraft(), goodLearned(), req, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Save("pr-style", b); err != nil {
		t.Fatal(err)
	}
	sk, ok, err := w.Current("pr-style")
	if !ok || err != nil || sk.Level != LevelAgent || !sameRequires(sk.Requires, req) || sk.Learned == nil {
		t.Fatalf("current: %+v, %v, %v", sk, ok, err)
	}

	if _, err := w.Save("pr-style", []byte("not a skill")); err != nil {
		t.Fatal(err)
	}
	var p Problem
	if _, ok, err := w.Current("pr-style"); !ok || !errors.As(err, &p) {
		t.Fatalf("broken: %v, %v", ok, err)
	}

	link := filepath.Join(root, "linked")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "pr-style", "SKILL.md"), filepath.Join(link, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := w.Current("linked"); !ok || !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlinked file: %v, %v", ok, err)
	}
	if _, _, err := (Writer{}).Current("pr-style"); err == nil {
		t.Fatal("Current with no root succeeded")
	}
}

// saveN saves contents in order and returns the writer and the skill folder.
func saveN(t *testing.T, contents ...string) (Writer, string) {
	t.Helper()
	w := Writer{Root: t.TempDir()}
	var dir string
	for _, c := range contents {
		s, err := w.Save("pr-style", []byte(c))
		if err != nil {
			t.Fatal(err)
		}
		dir = s.Dir
	}
	return w, dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestVersions(t *testing.T) {
	w, _ := saveN(t, "a")
	if v, err := w.Versions("pr-style"); err != nil || v == nil || len(v) != 0 {
		t.Fatalf("no history: %v, %v", v, err)
	}
	w, dir := saveN(t, "a", "b", "c")
	v, err := w.Versions("pr-style")
	if err != nil || len(v) != 2 || v[0] != 2 || v[1] != 1 {
		t.Fatalf("Versions = %v, %v", v, err)
	}
	if readFile(t, versionSkill(dir, v[0])) != "b" {
		t.Error("newest is not first")
	}
	// Other files in history/ are not versions.
	write(t, filepath.Join(dir, "history", "notes.txt"), "x")
	if got, _ := ListVersions(dir); len(got) != 2 {
		t.Errorf("ListVersions = %v", got)
	}
	if _, err := w.Versions("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := w.Versions("BAD"); !errors.Is(err, ErrBadName) {
		t.Errorf("bad name: %v", err)
	}
	if _, err := w.Forget("pr-style"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Versions("pr-style"); !errors.Is(err, ErrNotFound) {
		t.Errorf("trashed: %v", err)
	}
	trashed := filepath.Join(w.Root, TrashDir, "pr-style")
	if got, err := ListVersions(trashed); err != nil || len(got) != 2 {
		t.Errorf("ListVersions(trash) = %v, %v", got, err)
	}
}

func TestRestore(t *testing.T) {
	w, dir := saveN(t, "a", "b")
	if err := w.Restore("pr-style"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty trash: %v", err)
	}
	if _, err := w.Forget("pr-style"); err != nil {
		t.Fatal(err)
	}
	if err := w.Restore("pr-style"); err != nil {
		t.Fatal(err)
	}
	assertOnly(t, dir, "b")
	if h := historyFiles(t, dir); len(h) != 1 || readFile(t, filepath.Join(dir, "history", h[0], "SKILL.md")) != "a" {
		t.Fatalf("history = %v", h)
	}
	assertEmpty(t, filepath.Join(w.Root, TrashDir))

	// Over a live skill: conflict, both untouched.
	if _, err := w.Forget("pr-style"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Save("pr-style", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := w.Restore("pr-style"); !errors.Is(err, ErrExists) {
		t.Fatalf("over live: %v", err)
	}
	assertOnly(t, dir, "new")
	assertOnly(t, filepath.Join(w.Root, TrashDir, "pr-style"), "b")
	if err := w.Restore("BAD"); !errors.Is(err, ErrBadName) {
		t.Errorf("bad name: %v", err)
	}
}

func TestRemove(t *testing.T) {
	w, dir := saveN(t, "a")
	if err := w.Remove("pr-style"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("live, not in trash: %v", err)
	}
	assertOnly(t, dir, "a")
	if _, err := w.Forget("pr-style"); err != nil {
		t.Fatal(err)
	}
	if err := w.Remove("pr-style"); err != nil {
		t.Fatal(err)
	}
	assertEmpty(t, filepath.Join(w.Root, TrashDir))
	if err := w.Remove("pr-style"); !errors.Is(err, ErrNotFound) {
		t.Errorf("twice: %v", err)
	}
	if err := w.Remove("../x"); !errors.Is(err, ErrBadName) {
		t.Errorf("bad name: %v", err)
	}
}

func TestRevert(t *testing.T) {
	w, dir := saveN(t, "a", "b", "c")
	// history/1/ is a, 2/ is b; c is current, version 3.
	// 0 and below never name a version, and the live number is not in history/.
	for _, to := range []int{0, -1, 3} {
		if _, err := w.Revert("pr-style", to); !errors.Is(err, ErrNotFound) {
			t.Errorf("to %d: %v", to, err)
		}
	}
	assertOnly(t, dir, "c")
	r, err := w.Revert("pr-style", 2)
	if err != nil {
		t.Fatal(err)
	}
	if r != (Reverted{From: 3, To: 2, Version: 4, Versions: 3}) {
		t.Fatalf("Reverted = %+v", r)
	}
	assertOnly(t, dir, "b")
	if readFile(t, versionSkill(dir, r.From)) != "c" {
		t.Error("current did not retire into history")
	}
	if readFile(t, versionSkill(dir, r.To)) != "b" {
		t.Error("chosen version left history")
	}

	r, err = w.Revert("pr-style", 1)
	if err != nil || r != (Reverted{From: 4, To: 1, Version: 5, Versions: 4}) {
		t.Fatalf("named: %+v, %v", r, err)
	}
	assertOnly(t, dir, "a")

	for _, to := range []int{5, 99, -1} {
		if _, err := w.Revert("pr-style", to); !errors.Is(err, ErrNotFound) {
			t.Errorf("to %d: %v", to, err)
		}
	}
	assertOnly(t, dir, "a")

	w, _ = saveN(t, "only")
	for _, to := range []int{0, 1} {
		if _, err := w.Revert("pr-style", to); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "no version") {
			t.Errorf("no history, to %d: %v", to, err)
		}
	}
	if _, err := w.Revert("missing", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if _, err := w.Forget("pr-style"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Revert("pr-style", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("trashed: %v", err)
	}
}

// A revert brings back the whole version: its SKILL.md, and references/ and
// scripts/ as they were, files added since removed. Hidden entries stay.
func TestRevertRestoresFolder(t *testing.T) {
	w := Writer{Root: t.TempDir()}
	s, err := w.Save("pr-style", []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(s.Dir, "references", "r.md"), "ref a")
	write(t, filepath.Join(s.Dir, "scripts", "run.sh"), "run a")
	if _, err := w.Save("pr-style", []byte("b")); err != nil { // version 1 is a with its files
		t.Fatal(err)
	}
	write(t, filepath.Join(s.Dir, "references", "r.md"), "ref b")
	write(t, filepath.Join(s.Dir, "references", "new.md"), "added in b")
	if err := os.RemoveAll(filepath.Join(s.Dir, "scripts")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(s.Dir, ".holaclaw", "origin.toml"), "kept")

	r, err := w.Revert("pr-style", 1)
	if err != nil {
		t.Fatal(err)
	}
	assertOnly(t, s.Dir, "a")
	if got := treeFiles(t, s.Dir); !strings.HasPrefix(got, ".holaclaw/origin.toml SKILL.md history/") ||
		!strings.HasSuffix(got, " references/r.md scripts/run.sh") {
		t.Fatalf("folder holds %s", got)
	}
	if readFile(t, filepath.Join(s.Dir, "references", "r.md")) != "ref a" || readFile(t, filepath.Join(s.Dir, "scripts", "run.sh")) != "run a" {
		t.Error("references or scripts not reverted")
	}
	// What was current is kept whole as From.
	if got := treeFiles(t, filepath.Join(s.Dir, "history", versionDir(r.From))); got != "SKILL.md references/new.md references/r.md" {
		t.Errorf("version %d holds %s", r.From, got)
	}
	if m, _ := filepath.Glob(filepath.Join(s.Dir, ".revert-*")); len(m) != 0 {
		t.Errorf("revert folders left: %v", m)
	}
}

func TestRevertRefusesSymlinkedVersion(t *testing.T) {
	w, dir := saveN(t, "a")
	outside := t.TempDir()
	write(t, filepath.Join(outside, "SKILL.md"), "secret")
	if err := os.MkdirAll(filepath.Join(dir, "history", "8"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Version 7 is a symlinked folder, version 8 a folder with a symlinked SKILL.md.
	if err := os.Symlink(outside, filepath.Join(dir, "history", "7")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "SKILL.md"), filepath.Join(dir, "history", "8", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	for _, to := range []int{7, 8} {
		if _, err := w.Revert("pr-style", to); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("Revert to %d = %v", to, err)
		}
	}
	assertOnly(t, dir, "a")
}

func TestRestoreRemoveRefuseTrashSymlinks(t *testing.T) {
	for _, rel := range []string{TrashDir, TrashDir + "/pr-style"} {
		t.Run(rel, func(t *testing.T) {
			root, target := t.TempDir(), t.TempDir()
			write(t, filepath.Join(target, "SKILL.md"), "outside")
			w := Writer{Root: root}
			l := filepath.Join(root, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(l), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, l); err != nil {
				t.Fatal(err)
			}
			if err := w.Restore("pr-style"); !errors.Is(err, ErrUnsafePath) {
				t.Errorf("Restore = %v", err)
			}
			if err := w.Remove("pr-style"); !errors.Is(err, ErrUnsafePath) {
				t.Errorf("Remove = %v", err)
			}
			assertOnly(t, target, "outside")
		})
	}
}
