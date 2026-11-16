package fsutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func mkdirs(t *testing.T, root string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSubdirs(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "b", "a", ".trash", "c/deep")
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		o    Options
		want []string
	}{
		{"all", Options{}, []string{".trash", "a", "b", "c"}},
		{"skip hidden", Options{SkipHidden: true}, []string{"a", "b", "c"}},
		{"max", Options{Max: 2}, []string{".trash", "a"}},
		{"skip hidden and max", Options{SkipHidden: true, Max: 1}, []string{"a"}},
	}
	for _, c := range cases {
		got, err := Subdirs(root, c.o)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: Subdirs = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

func TestSubdirsErrors(t *testing.T) {
	if _, err := Subdirs(filepath.Join(t.TempDir(), "missing"), Options{}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing dir: err = %v, want ErrNotExist", err)
	}
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Subdirs(f, Options{}); err == nil {
		t.Error("a file: no error")
	}
}

func TestFiles(t *testing.T) {
	fsys := fstest.MapFS{
		"refs/b.md":         {Data: []byte("b")},
		"refs/a.md":         {Data: []byte("a")},
		"refs/sub/c.md":     {Data: []byte("c")},
		"refs/.draft.md":    {Data: []byte("hidden file")},
		"refs/.cache/x.md":  {Data: []byte("in a hidden dir")},
		"refs/link":         {Data: []byte("x"), Mode: fs.ModeSymlink},
		"refs/empty/.keep":  {},
		"other/not-listed":  {},
		"notadir":           {Data: []byte("x")},
		"top.md":            {Data: []byte("top")},
		".hidden-top/y.md":  {Data: []byte("y")},
		"refs/sub/deeper/d": {Data: []byte("d")},
	}
	cases := []struct {
		name, root string
		o          Options
		want       []string
	}{
		{"all", "refs", Options{}, []string{".cache/x.md", ".draft.md", "a.md", "b.md", "empty/.keep", "sub/c.md", "sub/deeper/d"}},
		{"skip hidden", "refs", Options{SkipHidden: true}, []string{"a.md", "b.md", "sub/c.md", "sub/deeper/d"}},
		{"max", "refs", Options{SkipHidden: true, Max: 2}, []string{"a.md", "b.md"}},
		{"dot root", ".", Options{SkipHidden: true}, []string{"notadir", "other/not-listed", "refs/a.md", "refs/b.md", "refs/sub/c.md", "refs/sub/deeper/d", "top.md"}},
		{"missing root", "nope", Options{}, nil},
		{"file root", "notadir", Options{}, nil},
	}
	for _, c := range cases {
		got, err := Files(fsys, c.root, c.o)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s: Files = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

func TestFilesOnDisk(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "refs/sub", "outside")
	for _, p := range []string{"refs/a.md", "refs/sub/b.md", "outside/secret.md"} {
		if err := os.WriteFile(filepath.Join(root, p), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Neither a symlinked file nor a symlinked directory is followed.
	if err := os.Symlink(filepath.Join(root, "outside/secret.md"), filepath.Join(root, "refs/link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(root, "refs/out")); err != nil {
		t.Fatal(err)
	}
	got, err := Files(os.DirFS(root), "refs", Options{})
	if want := []string{"a.md", "sub/b.md"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("Files = %q, %v; want %q", got, err, want)
	}
	// A symlinked root is not a directory.
	if err := os.Symlink(filepath.Join(root, "refs"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if got, err := Files(os.DirFS(root), "alias", Options{}); err != nil || got != nil {
		t.Errorf("symlinked root: Files = %q, %v", got, err)
	}
}

func TestFilesRootError(t *testing.T) {
	fsys := errFS{}
	if _, err := Files(fsys, "refs", Options{}); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("err = %v, want the stat error", err)
	}
}

// errFS fails every open with an error that is not fs.ErrNotExist.
type errFS struct{}

func (errFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: errors.New("denied")}
}

func TestAbs(t *testing.T) {
	got, err := Abs("a/../b")
	if err != nil || !filepath.IsAbs(got) || filepath.Base(got) != "b" {
		t.Errorf("Abs = %q, %v", got, err)
	}
	if got, err := Abs("/x/./y"); err != nil || got != "/x/y" {
		t.Errorf("Abs(abs) = %q, %v", got, err)
	}
}

// TestAbsNoWorkingDir removes the working directory, the one case where
// filepath.Abs fails: Abs reports it instead of returning the relative path.
func TestAbsNoWorkingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "gone")
	mkdirs(t, dir, ".")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	t.Setenv("PWD", "")
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	got, err := Abs("rel")
	if err == nil || got != "" || !strings.Contains(err.Error(), `absolute path of "rel"`) {
		t.Fatalf("Abs = %q, %v; want an error", got, err)
	}
}

func TestMkdirAll(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a")
	dir := filepath.Join(root, "a", "b", "c")
	created, err := MkdirAll(dir, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Join(root, "a", "b"), dir}; !slices.Equal(created, want) {
		t.Fatalf("created = %v, want %v", created, want)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("stat %s: %v, %v", dir, fi, err)
	}
	if created, err := MkdirAll(dir, 0o700); err != nil || len(created) != 0 {
		t.Fatalf("again: created = %v, err %v; want nothing", created, err)
	}
}

// A file in the way stops it before it creates anything.
func TestMkdirAllUnderAFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	created, err := MkdirAll(filepath.Join(root, "f", "x"), 0o700)
	if err == nil || len(created) != 0 {
		t.Fatalf("created = %v, err %v; want an error and nothing created", created, err)
	}
}
