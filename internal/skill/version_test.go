package skill

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestReadVersion(t *testing.T) {
	root := t.TempDir()
	w := Writer{Root: root}
	first := rendered(t, nil)
	if _, err := w.Save("pr-style", first); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "pr-style")
	write(t, filepath.Join(dir, "references", "old.md"), "old")
	write(t, filepath.Join(dir, "scripts", "run.sh"), "run")
	s, err := w.Save("pr-style", rendered(t, func(d *Draft) { d.When = "When opening any pull request" }))
	if err != nil {
		t.Fatal(err)
	}
	// Changed after version 1 retired: the live folder, not the version.
	write(t, filepath.Join(dir, "references", "new.md"), "new")

	sk, b, err := ReadVersion(dir, s.Replaced)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, first) || sk.Name != "pr-style" || sk.Level != LevelAgent || sk.Learned == nil {
		t.Fatalf("ReadVersion = %+v, %q", sk, b)
	}
	if sk.Dir != dir {
		t.Errorf("Dir = %q, want %q", sk.Dir, dir)
	}
	if fmt.Sprint(sk.References, sk.Scripts) != "[old.md] [run.sh]" {
		t.Errorf("version files = %v %v, want its own", sk.References, sk.Scripts)
	}

	// A number below 1 never touches the disk; the current version (2) is not
	// in history/.
	for _, bad := range []int{0, -1, 2, 99} {
		if _, _, err := ReadVersion(dir, bad); !errors.Is(err, ErrNotFound) {
			t.Errorf("ReadVersion(%d) = %v, want ErrNotFound", bad, err)
		}
	}

	// A version that does not parse any more is still handed back as bytes,
	// with the parse error.
	seedVersion(t, filepath.Join(dir, "history"), 7, "no frontmatter\n")
	// A folder without a SKILL.md is not a version to read.
	if err := os.MkdirAll(filepath.Join(dir, "history", "8"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadVersion(dir, 8); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadVersion(8) = %v, want ErrNotFound", err)
	}
	var p Problem
	if _, b, err := ReadVersion(dir, 7); !errors.As(err, &p) || p.Code != CodeNoFrontmatter || string(b) != "no frontmatter\n" {
		t.Errorf("a broken version: %v, %q", err, b)
	}
}

func TestFile(t *testing.T) {
	root := t.TempDir()
	w := Writer{Root: root}
	content := rendered(t, nil)
	if _, err := w.Save("pr-style", content); err != nil {
		t.Fatal(err)
	}
	set, _ := Load(Roots{Agent: root})
	sk, ok := set.Get("pr-style")
	if !ok {
		t.Fatal("the saved skill did not load")
	}
	b, err := sk.File()
	if err != nil || !bytes.Equal(b, content) {
		t.Fatalf("File = %q, %v", b, err)
	}

	// Bundled skills read from their root.
	raw := "---\nname: hello\ndescription: Say hello when greeted\n---\n# Hello\n"
	set, _ = Load(Roots{Bundled: fstest.MapFS{"hello/SKILL.md": {Data: []byte(raw)}}})
	bsk, ok := set.Get("hello")
	if !ok {
		t.Fatal("the bundled skill did not load")
	}
	if b, err := bsk.File(); err != nil || string(b) != raw {
		t.Fatalf("bundled File = %q, %v", b, err)
	}
	if _, err := (Skill{Level: LevelBundled, Name: "x"}).File(); err == nil {
		t.Error("a bundled skill without a root read something")
	}

	// A folder whose file is gone is not found rather than empty.
	if err := os.Remove(filepath.Join(sk.Dir, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := sk.File(); err == nil {
		t.Error("a removed file read something")
	}
}

// The live number is the one Save gave the file, through a revert, history/
// trimming and the trash.
func TestLiveVersion(t *testing.T) {
	w := Writer{Root: t.TempDir()}
	dir := filepath.Join(w.Root, "pr-style")
	// No folder, no history: version 1, what the first Save makes.
	if n, err := LiveVersion(dir); err != nil || n != 1 {
		t.Fatalf("no history: %d, %v", n, err)
	}
	var s Saved
	var err error
	for _, c := range []string{"a", "b", "c"} {
		if s, err = w.Save("pr-style", []byte(c)); err != nil {
			t.Fatal(err)
		}
		if n, err := LiveVersion(dir); err != nil || n != s.Version {
			t.Fatalf("after save: %d, %v, want %d", n, err, s.Version)
		}
	}
	r, err := w.Revert("pr-style", 1)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := LiveVersion(dir); err != nil || n != r.Version || n != 4 {
		t.Fatalf("after revert: %d, %v, want %d", n, err, r.Version)
	}
	// Trimming drops the oldest, never the newest: the number holds.
	hist := filepath.Join(dir, "history")
	for i := range MaxVersions + 1 {
		seedVersion(t, hist, i+1, "old")
	}
	if s, err = w.Save("pr-style", []byte("d")); err != nil {
		t.Fatal(err)
	}
	if n, err := LiveVersion(dir); err != nil || n != s.Version || s.Versions != MaxVersions {
		t.Fatalf("after trim: %d, %v, want %d (%+v)", n, err, s.Version, s)
	}
	if _, err := w.Forget("pr-style"); err != nil {
		t.Fatal(err)
	}
	if n, err := LiveVersion(filepath.Join(w.Root, TrashDir, "pr-style")); err != nil || n != s.Version {
		t.Fatalf("trashed: %d, %v, want %d", n, err, s.Version)
	}

	// A history/ that cannot be read is an error, not version 1.
	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "history"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LiveVersion(bad); err == nil {
		t.Error("history a file: no error")
	}
}
