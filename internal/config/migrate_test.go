package config

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// testLadders holds the ladder each golden case under testdata/config/migrate
// runs with. Real steps go in Ladder and need no entry here: a case directory
// whose name is not listed exercises the shipped ladder. Adding a future step
// means adding one <case>/input + <case>/expected pair (and, while the step is
// the one under test, one entry here).
var testLadders = map[string][]Step{
	// "bump" stands in for the shape every real step has: rewrite every file
	// of the directory and raise the version.
	"bump": {{From: 1, Fn: func(files map[string][]byte) (map[string][]byte, error) {
		out := map[string][]byte{}
		for name, b := range files {
			if strings.HasSuffix(name, ".toml") {
				b = bytes.Replace(b, []byte("version = 1"), []byte("version = 2"), 1)
			}
			out[name] = b
		}
		return out, nil
	}}},
}

func TestLadderIsEmptyAtV1(t *testing.T) {
	if CurrentVersion != 1 {
		t.Skip("ladder is no longer empty")
	}
	if len(Ladder) != 0 {
		t.Fatalf("Ladder = %v, want empty at version 1", Ladder)
	}
	diff, err := Migrate(dir("minimal"), false)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if diff != "" {
		t.Fatalf("a current directory produced a diff:\n%s", diff)
	}
}

// TestMigrateGolden runs every case directory under testdata/config/migrate.
func TestMigrateGolden(t *testing.T) {
	cases, err := os.ReadDir(filepath.Join("..", "..", "testdata", "config", "migrate"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no migration golden cases")
	}
	for _, c := range cases {
		if !c.IsDir() {
			continue
		}
		t.Run(c.Name(), func(t *testing.T) {
			ladder, ok := testLadders[c.Name()]
			if !ok {
				ladder = Ladder
			}
			base := filepath.Join("..", "..", "testdata", "config", "migrate", c.Name())
			work := t.TempDir()
			copyDir(t, filepath.Join(base, "input"), work)

			diff, err := migrate(work, true, ladder, time.Unix(0, 0).UTC())
			if err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if diff == "" {
				t.Fatal("no diff produced")
			}
			assertSameTree(t, filepath.Join(base, "expected"), work)

			// One backup directory holding the pre-migration files.
			backups := backupDirs(t, work)
			if len(backups) != 1 {
				t.Fatalf("backup dirs = %v, want exactly one", backups)
			}
			assertSameTree(t, filepath.Join(base, "input"), filepath.Join(work, backups[0]))
		})
	}
}

func TestMigrateDryRunWritesNothing(t *testing.T) {
	base := filepath.Join("..", "..", "testdata", "config", "migrate", "bump")
	work := t.TempDir()
	copyDir(t, filepath.Join(base, "input"), work)

	diff, err := migrate(work, false, testLadders["bump"], time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, want := range []string{"--- a/" + FileConfig, "-version = 1", "+version = 2", "@@"} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff missing %q:\n%s", want, diff)
		}
	}
	assertSameTree(t, filepath.Join(base, "input"), work)
	if b := backupDirs(t, work); len(b) != 0 {
		t.Fatalf("dry run wrote backups: %v", b)
	}
}

// auth.toml holds every secret and is owned by another package: a migration
// neither backs it up nor rewrites it (ARCHITECTURE.md §4).
func TestMigrateIgnoresAuthFile(t *testing.T) {
	base := filepath.Join("..", "..", "testdata", "config", "migrate", "bump")
	work := t.TempDir()
	copyDir(t, filepath.Join(base, "input"), work)
	auth := filepath.Join(work, FileAuth)
	write(t, auth, "[providers.openrouter]\napi_key = \"sk-secret\"\n")

	if _, err := migrate(work, true, testLadders["bump"], time.Unix(0, 0).UTC()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	b, err := os.ReadFile(auth)
	if err != nil || !strings.Contains(string(b), "sk-secret") {
		t.Fatalf("auth.toml = %q, %v", b, err)
	}
	backups := backupDirs(t, work)
	if _, err := os.Stat(filepath.Join(work, backups[0], FileAuth)); err == nil {
		t.Fatal("auth.toml was copied into the backup")
	}
}

func TestMigrateRejectsNewerDirectory(t *testing.T) {
	work := t.TempDir()
	copyDir(t, dir("minimal"), work)
	write(t, filepath.Join(work, FileConfig), "version = 99\n")
	if _, err := migrate(work, false, nil, time.Now()); err == nil {
		t.Fatal("a newer directory migrated without an error")
	}
}

func TestMigrateMissingStep(t *testing.T) {
	work := t.TempDir()
	copyDir(t, dir("minimal"), work)
	// A ladder that starts at 2 cannot migrate a version-1 directory.
	_, err := migrate(work, false, []Step{{From: 2, Fn: func(f map[string][]byte) (map[string][]byte, error) { return f, nil }}}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "no step") {
		t.Fatalf("err = %v, want a missing-step error", err)
	}
}

func TestDiffAddedAndRemovedFiles(t *testing.T) {
	before := map[string][]byte{"a.toml": []byte("x = 1\n")}
	after := map[string][]byte{"b.toml": []byte("y = 2\n")}
	d := diffFiles(before, after)
	for _, want := range []string{"--- a/a.toml", "+++ /dev/null", "--- /dev/null", "+++ b/b.toml", "-x = 1", "+y = 2"} {
		if !strings.Contains(d, want) {
			t.Errorf("diff missing %q:\n%s", want, d)
		}
	}
}

// --- helpers ----------------------------------------------------------------

var backupRE = regexp.MustCompile(`^\.backup-\d{8}T\d{6}Z$`)

func backupDirs(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() && backupRE.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out
}

// assertSameTree checks that every file of want exists in got with the same
// bytes, and that got holds no extra config file.
func assertSameTree(t *testing.T, want, got string) {
	t.Helper()
	wantFiles := readTree(t, want)
	gotFiles := readTree(t, got)
	for name, b := range wantFiles {
		g, ok := gotFiles[name]
		if !ok {
			t.Errorf("%s: missing", name)
			continue
		}
		if !bytes.Equal(b, g) {
			t.Errorf("%s:\n got %q\nwant %q", name, g, b)
		}
	}
	for name := range gotFiles {
		if _, ok := wantFiles[name]; !ok {
			t.Errorf("%s: unexpected file", name)
		}
	}
}

func readTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if info.IsDir() {
			if p != root && strings.HasPrefix(filepath.Base(p), ".backup-") {
				return filepath.SkipDir
			}
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
