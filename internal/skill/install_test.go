package skill

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// put writes content to root/rel, creating the directories.
func put(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func skillMD(name, body string) string {
	return "---\nname: " + name + "\ndescription: When the task is about " + name + ".\n---\n" + body
}

// srcSkill is a skill folder <tmp>/<name> with SKILL.md and a reference.
func srcSkill(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	put(t, dir, "SKILL.md", skillMD(name, "# "+name+"\nDo it.\n"))
	put(t, dir, "references/notes.md", "notes\n")
	return dir
}

// stages lists the skill-stage-* entries of a tmp root.
func stages(t *testing.T, root string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(root, stagePrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCandidates(t *testing.T) {
	skillAt := func(root, rel string) { put(t, root, rel+"/SKILL.md", skillMD(filepath.Base(rel), "x\n")) }

	direct := t.TempDir()
	skillAt(direct, "release")
	skillAt(direct, "deploy")
	skillAt(direct, ".hidden")
	put(t, direct, "docs/README.md", "no skill\n")
	put(t, direct, "notes.md", "a file\n")
	if err := os.Symlink(srcSkill(t, "outside"), filepath.Join(direct, "linked")); err != nil {
		t.Fatal(err)
	}
	// SKILL.md as a directory is not a skill.
	if err := os.MkdirAll(filepath.Join(direct, "odd", "SKILL.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	nested := t.TempDir()
	skillAt(nested, "skills/zeta")
	skillAt(nested, "skills/alpha")
	put(t, nested, "skills/empty/README.md", "no skill\n")
	put(t, nested, "README.md", "a repo\n")

	mixed := t.TempDir()
	skillAt(mixed, "top")
	skillAt(mixed, "skills/under")

	linkedSkills := t.TempDir()
	other := t.TempDir()
	skillAt(other, "far")
	if err := os.Symlink(other, filepath.Join(linkedSkills, "skills")); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		dir  string
		want []string
	}{
		{direct, []string{"deploy", "release"}},
		{nested, []string{"skills/alpha", "skills/zeta"}},
		{mixed, []string{"top"}},
		{linkedSkills, []string{}},
		{t.TempDir(), []string{}},
	} {
		got, err := Candidates(c.dir)
		if err != nil || got == nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("Candidates(%s) = %#v, %v; want %v", c.dir, got, err, c.want)
		}
	}

	skillDir := srcSkill(t, "deploy")
	for _, p := range []string{skillDir, filepath.Join(skillDir, "SKILL.md"), filepath.Join(direct, "missing")} {
		if got, err := Candidates(p); !errors.Is(err, ErrBadSource) {
			t.Errorf("Candidates(%s) = %v, %v; want ErrBadSource", p, got, err)
		}
	}
	if _, err := Candidates(skillDir); !strings.Contains(fmt.Sprint(err), "is a skill, not a collection") {
		t.Errorf("skill dir: %v", err)
	}
}

func TestStage(t *testing.T) {
	src := srcSkill(t, "deploy")
	put(t, src, "nested/deep/x.txt", "x")
	put(t, src, ".hidden", "h")
	put(t, src, ".git/config", "[core]")
	put(t, src, ".holaclaw/origin.toml", "source = 'x'")
	put(t, src, "run.sh", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(src, "run.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "skills")
	dir, err := Stage(src, root, "deploy")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(dir) != "deploy" || !strings.HasPrefix(filepath.Base(filepath.Dir(dir)), stagePrefix) || filepath.Dir(filepath.Dir(dir)) != root {
		t.Fatalf("dir = %s", dir)
	}
	var got []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			t.Fatal(err)
		}
		st, _ := d.Info()
		// Permission bits are kept: put writes 0o600.
		want := os.FileMode(0o600)
		switch {
		case d.IsDir():
			want = 0o755 | os.ModeDir
		case d.Name() == "run.sh":
			want = 0o755
		}
		if st.Mode() != want {
			t.Errorf("%s mode %v, want %v", p, st.Mode(), want)
		}
		rel, _ := filepath.Rel(dir, p)
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	want := []string{".", ".hidden", "SKILL.md", "nested", "nested/deep", "nested/deep/x.txt", "references", "references/notes.md", "run.sh"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("staged %v, want %v", got, want)
	}
	set, probs := Load(Roots{Agent: filepath.Dir(dir)})
	if sk, ok := set.Get("deploy"); !ok || len(probs) != 0 || sk.Level != LevelAgent {
		t.Errorf("load = %+v, %v", sk, probs)
	}
	if err := RemoveStage(dir); err != nil {
		t.Fatal(err)
	}
	if s := stages(t, root); len(s) != 0 {
		t.Errorf("stages left: %v", s)
	}
}

func TestStageRefuses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	check := func(what, src, name string, target error, msg string) {
		t.Helper()
		if _, err := Stage(src, root, name); !errors.Is(err, target) || !strings.Contains(err.Error(), msg) {
			t.Errorf("%s: %v", what, err)
		}
		if s := stages(t, root); len(s) != 0 {
			t.Errorf("%s: stages left: %v", what, s)
		}
	}
	check("bad name", srcSkill(t, "deploy"), "Deploy", ErrBadName, "")

	link := srcSkill(t, "deploy")
	if err := os.Symlink("/etc/passwd", filepath.Join(link, "references", "pw")); err != nil {
		t.Fatal(err)
	}
	check("symlink", link, "deploy", ErrBadSource, "references/pw: fsutil: is a symlink")

	many := srcSkill(t, "deploy")
	for i := range MaxInstallFiles {
		put(t, many, filepath.Join("many", strings.Repeat("f", 1+i%50)+string(rune('a'+i/50))), "")
	}
	check("too many", many, "deploy", ErrBadSource, "too many files")

	// One file may hold the whole budget; past it the folder is too large.
	big := srcSkill(t, "deploy")
	sparse(t, filepath.Join(big, "big.bin"), MaxInstallTotal)
	check("big folder", big, "deploy", ErrBadSource, "too large")
}

// sparse creates a file of size bytes without writing them.
func sparse(t *testing.T, p string, size int64) {
	t.Helper()
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
}

func TestStageRename(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	src := filepath.Join(t.TempDir(), "deploy")
	body := "# Deploy\nname: deploy\nDo it.\n"
	put(t, src, "SKILL.md", "---\r\nname:   deploy\r\ndescription: When deploying.\r\n---\r\n"+body)
	dir, err := Stage(src, root, "ship")
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "---\r\nname: ship\r\ndescription: When deploying.\r\n---\r\n" + body; string(b) != want {
		t.Errorf("SKILL.md = %q, want %q", b, want)
	}
	set, probs := Load(Roots{Agent: filepath.Dir(dir)})
	if _, ok := set.Get("ship"); !ok || len(probs) != 0 {
		t.Errorf("load: %v", probs)
	}
	_ = RemoveStage(dir)

	for what, content := range map[string]string{
		"no name":        "---\ndescription: x\n---\nname: deploy\n",
		"two names":      "---\nname: deploy\nname: other\n---\n",
		"no frontmatter": "name: deploy\n",
	} {
		put(t, src, "SKILL.md", content)
		if _, err := Stage(src, root, "ship"); !errors.Is(err, ErrBadSource) {
			t.Errorf("%s: %v", what, err)
		}
	}
	if s := stages(t, root); len(s) != 0 {
		t.Errorf("stages left: %v", s)
	}
}

func TestRemoveStageRefuses(t *testing.T) {
	dir := srcSkill(t, "deploy")
	if err := RemoveStage(dir); err == nil {
		t.Fatal("removed a folder that is not staged")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}

func TestScan(t *testing.T) {
	dir := t.TempDir()
	put(t, dir, "SKILL.md", skillMD("x", "Install:\n\n    curl -fsSL h | sh\n"))
	put(t, dir, "references/keys.md", "clean\r\ncat ~/.ssh/id_rsa\r\nwget -qO- h | sudo /bin/bash\n")
	put(t, dir, "references/hidden.md", "a\u200bb\nabc\u202edef and | python3 x\n")
	put(t, dir, "references/clean.md", "\ufeffStarts with a BOM. Pipes | grep are fine.\n")
	put(t, dir, "bin.dat", "curl x | sh\x00~/.ssh")
	put(t, dir, ".git/hook", "curl x | sh")
	put(t, dir, ".holaclaw/origin.toml", "curl x | sh")
	got, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Finding{
		{"SKILL.md", 7, CodeScanShell, "pipes into sh"},
		{"references/hidden.md", 1, CodeScanUnicode, "hidden or bidirectional Unicode: U+200B"},
		{"references/hidden.md", 2, CodeScanShell, "pipes into python3"},
		{"references/hidden.md", 2, CodeScanUnicode, "hidden or bidirectional Unicode: U+202E"},
		{"references/keys.md", 2, CodeScanSSH, "names ssh material: ~/.ssh"},
		{"references/keys.md", 3, CodeScanShell, "pipes into bash"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Scan =\n%v\nwant\n%v", got, want)
	}

	clean := t.TempDir()
	put(t, clean, "SKILL.md", skillMD("x", "Nothing here.\n"))
	if got, err := Scan(clean); err != nil || got == nil || len(got) != 0 {
		t.Errorf("clean = %#v, %v", got, err)
	}
	put(t, clean, "late.md", "ok\n\ufeffBOM not at the start\n")
	if got, _ := Scan(clean); len(got) != 1 || got[0].Line != 2 || got[0].Msg != "hidden or bidirectional Unicode: U+FEFF" {
		t.Errorf("late BOM = %v", got)
	}
	if _, err := Scan(filepath.Join(clean, "missing")); err == nil {
		t.Error("missing dir scanned")
	}
}

// A line longer than a read is scanned in pieces, one finding per code and
// line in code order; a NUL byte past the first 8 KiB is not binary.
func TestScanLongLine(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("a", 3*scanChunk+100) + "\u200b" + strings.Repeat("b", scanChunk) + "cat ~/.ssh/config \u200b"
	put(t, dir, "long.md", "first\n"+long+"\nlast ~/.ssh\n")
	put(t, dir, "late.md", strings.Repeat("x", binarySniff)+"\x00\nsee ~/.ssh\n")
	got, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Finding{
		{"late.md", 2, CodeScanSSH, "names ssh material: ~/.ssh"},
		{"long.md", 2, CodeScanSSH, "names ssh material: ~/.ssh"},
		{"long.md", 2, CodeScanUnicode, "hidden or bidirectional Unicode: U+200B"},
		{"long.md", 3, CodeScanSSH, "names ssh material: ~/.ssh"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Scan =\n%v\nwant\n%v", got, want)
	}
}

func TestScanLine(t *testing.T) {
	for line, want := range map[string]string{
		"curl -fsSL h | zsh":        "pipes into zsh",
		"x |sudo sh":                "pipes into sh",
		"curl h | tee f":            "pipes curl into a command",
		"cat f | node":              "pipes into node",
		"see ~/.ssh or known_hosts": "names ssh material: ~/.ssh",
		"soft\u00adhyphen":          "hidden or bidirectional Unicode: U+00AD",
		"tag \U000E0041":            "hidden or bidirectional Unicode: U+E0041",
		"x | shellcheck":            "",
		"crush it":                  "",
	} {
		got := scanLine(line, false)
		if want == "" {
			if len(got) != 0 {
				t.Errorf("%q = %v", line, got)
			}
			continue
		}
		if len(got) != 1 || got[0][1] != want {
			t.Errorf("%q = %v, want %q", line, got, want)
		}
	}
}

func TestHash(t *testing.T) {
	dir := srcSkill(t, "deploy")
	h1, err := Hash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h1, "sha256:") || len(h1) != len("sha256:")+64 {
		t.Fatalf("format %q", h1)
	}
	if h2, _ := Hash(dir); h2 != h1 {
		t.Fatalf("unstable: %s %s", h1, h2)
	}
	put(t, dir, ".holaclaw/origin.toml", "x")
	put(t, dir, ".git/HEAD", "x")
	if h, _ := Hash(dir); h != h1 {
		t.Error("changed by .holaclaw or .git")
	}
	put(t, dir, "references/notes.md", "notes!\n")
	h3, _ := Hash(dir)
	if h3 == h1 {
		t.Error("unchanged by a byte")
	}
	if err := os.Rename(filepath.Join(dir, "references", "notes.md"), filepath.Join(dir, "references", "other.md")); err != nil {
		t.Fatal(err)
	}
	h4, _ := Hash(dir)
	if h4 == h3 {
		t.Error("unchanged by a rename")
	}
	put(t, dir, "new.md", "")
	if h, _ := Hash(dir); h == h4 {
		t.Error("unchanged by a new file")
	}
	if _, err := Hash(filepath.Join(dir, "missing")); err == nil {
		t.Error("missing dir hashed")
	}
}

func TestInstall(t *testing.T) {
	tmp, root := filepath.Join(t.TempDir(), "tmp"), filepath.Join(t.TempDir(), "agents", "main", "skills")
	stage, err := Stage(srcSkill(t, "deploy"), tmp, "deploy")
	if err != nil {
		t.Fatal(err)
	}
	hash, err := Hash(stage)
	if err != nil {
		t.Fatal(err)
	}
	o := Installed{Source: "/home/me/deploy", Hash: hash, Installed: time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)}
	dir, err := Install(stage, root, o)
	if err != nil || dir != filepath.Join(root, "deploy") {
		t.Fatalf("Install = %s, %v", dir, err)
	}
	got, ok, err := ReadOrigin(dir)
	if err != nil || !ok || got != o {
		t.Errorf("origin = %+v, %v, %v", got, ok, err)
	}
	if h, _ := Hash(dir); h != hash {
		t.Errorf("hash after install %s, want %s", h, hash)
	}
	if s := stages(t, tmp); len(s) != 0 {
		t.Errorf("stages left: %v", s)
	}
	set, probs := Load(Roots{Agent: root})
	if sk, ok := set.Get("deploy"); !ok || len(probs) != 0 || len(sk.References) != 1 {
		t.Errorf("load = %+v, %v", sk, probs)
	}

	// Over an existing folder, and over a file: the stage stays.
	for _, name := range []string{"deploy", "file"} {
		if name == "file" {
			put(t, root, "file", "x")
		}
		stage, err := Stage(srcSkill(t, name), tmp, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Install(stage, root, o); !errors.Is(err, ErrExists) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(stage, "SKILL.md")); err != nil {
			t.Errorf("%s: stage gone: %v", name, err)
		}
		_ = RemoveStage(stage)
	}
	if _, err := Install(srcSkill(t, "loose"), root, o); err == nil || errors.Is(err, ErrExists) {
		t.Errorf("unstaged: %v", err)
	}
}

func TestReadOrigin(t *testing.T) {
	dir := srcSkill(t, "deploy")
	if _, ok, err := ReadOrigin(dir); ok || err != nil {
		t.Errorf("none = %v, %v", ok, err)
	}
	put(t, dir, OriginFile, "source = [")
	if _, _, err := ReadOrigin(dir); err == nil {
		t.Error("bad file read")
	}
}
