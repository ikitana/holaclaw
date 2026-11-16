package fsutil

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCopyTree(t *testing.T) {
	src := t.TempDir()
	mkdirs(t, src, "references/deep", "scripts", ".hidden", "empty")
	for p, mode := range map[string]os.FileMode{
		"SKILL.md": 0o644, "references/a.md": 0o644, "references/deep/b.md": 0o600,
		"scripts/run.sh": 0o755, ".hidden/x": 0o644,
	} {
		if err := os.WriteFile(filepath.Join(src, p), []byte("content of "+p), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(src, p), mode); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(src, "references", "link.md")); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "copy")
	var skipped []string
	err := CopyTree(src, dst, CopyOptions{Skip: func(rel string, _ fs.DirEntry) bool {
		if strings.HasPrefix(rel, ".") {
			skipped = append(skipped, rel)
			return true
		}
		return false
	}})
	if err != nil {
		t.Fatal(err)
	}
	for p, mode := range map[string]os.FileMode{
		"SKILL.md": 0o644, "references/a.md": 0o644, "references/deep/b.md": 0o600, "scripts/run.sh": 0o755,
	} {
		b, err := os.ReadFile(filepath.Join(dst, p))
		if err != nil || string(b) != "content of "+p {
			t.Errorf("%s = %q, %v", p, b, err)
			continue
		}
		if st, _ := os.Stat(filepath.Join(dst, p)); st.Mode().Perm() != mode {
			t.Errorf("%s mode %v, want %v", p, st.Mode().Perm(), mode)
		}
	}
	if st, err := os.Stat(filepath.Join(dst, "empty")); err != nil || !st.IsDir() {
		t.Errorf("empty dir not copied: %v", err)
	}
	for _, gone := range []string{".hidden", "references/link.md"} {
		if _, err := os.Lstat(filepath.Join(dst, gone)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s copied: %v", gone, err)
		}
	}
	if strings.Join(skipped, ",") != ".hidden" {
		t.Errorf("skip saw %v; a skipped dir's contents are not walked", skipped)
	}
}

func TestCopyTreeFailures(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	if err := CopyTree(src, dst, CopyOptions{}); !errors.Is(err, fs.ErrExist) {
		t.Errorf("existing dst: %v", err)
	}
	if err := CopyTree(filepath.Join(src, "missing"), filepath.Join(dst, "x"), CopyOptions{}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing src: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "x")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("dst made for a missing src: %v", err)
	}
}

// tree is a folder with two files of 4 bytes in sub/ and a symlink.
func tree(t *testing.T) string {
	t.Helper()
	src := t.TempDir()
	mkdirs(t, src, "sub")
	for _, p := range []string{"a.md", "sub/b.md"} {
		if err := os.WriteFile(filepath.Join(src, p), []byte("four"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(src, "a.md"), filepath.Join(src, "sub", "link")); err != nil {
		t.Fatal(err)
	}
	return src
}

func TestCopyTreeLimits(t *testing.T) {
	src := tree(t)
	for _, c := range []struct {
		name string
		o    CopyOptions
		want error
		msg  string
	}{
		{"within", CopyOptions{MaxFiles: 2, MaxBytes: 8}, nil, ""},
		{"files", CopyOptions{MaxFiles: 1}, ErrTooMany, "sub/b.md: "},
		{"bytes", CopyOptions{MaxBytes: 7}, ErrTooLarge, "sub/b.md: "},
		{"symlink", CopyOptions{RefuseSymlinks: true}, ErrSymlink, "sub/link: "},
	} {
		dst := filepath.Join(t.TempDir(), "copy")
		err := CopyTree(src, dst, c.o)
		if !errors.Is(err, c.want) || (c.want == nil) != (err == nil) || !strings.Contains(fmt.Sprint(err), c.msg) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// Directories count apart from files.
	deep := t.TempDir()
	mkdirs(t, deep, "a", "b", "c")
	if err := CopyTree(deep, filepath.Join(t.TempDir(), "copy"), CopyOptions{MaxFiles: 2}); !errors.Is(err, ErrTooMany) {
		t.Errorf("dirs: %v", err)
	}
}

// A file bigger than the limit is refused by its size, before it is copied.
func TestCopyTreeBigFile(t *testing.T) {
	src := t.TempDir()
	f, err := os.Create(filepath.Join(src, "big"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	dst := filepath.Join(t.TempDir(), "copy")
	if err := CopyTree(src, dst, CopyOptions{MaxBytes: 1 << 20}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "big")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("big copied: %v", err)
	}
}

func TestWalkFiles(t *testing.T) {
	src := tree(t)
	// "a-c" sorts before "a/..." as a slash path, after it in a walk.
	mkdirs(t, src, "a", ".git")
	for _, p := range []string{"a-c", "a/x", ".git/HEAD"} {
		if err := os.WriteFile(filepath.Join(src, p), []byte(p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	skip := func(rel string, _ fs.DirEntry) bool { return rel == ".git" }
	err := WalkFiles(src, skip, func(rel string, open func() (fs.File, error)) error {
		f, err := open()
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		b, err := io.ReadAll(f)
		got = append(got, rel+"="+string(b))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "a-c=a-c a.md=four a/x=a/x sub/b.md=four"
	if strings.Join(got, " ") != want {
		t.Errorf("walk = %v, want %s", got, want)
	}
	stop := errors.New("stop")
	n := 0
	if err := WalkFiles(src, nil, func(string, func() (fs.File, error)) error { n++; return stop }); !errors.Is(err, stop) || n != 1 {
		t.Errorf("stop = %v after %d", err, n)
	}
	if err := WalkFiles(filepath.Join(src, "missing"), nil, func(string, func() (fs.File, error)) error { return nil }); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing = %v", err)
	}
}

func TestNoSymlinks(t *testing.T) {
	root := t.TempDir()
	mkdirs(t, root, "a/b", "real")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "a", "link")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		parts []string
		want  error
	}{
		{"real path", []string{"a", "b"}, nil},
		{"no parts", nil, nil},
		{"missing element ends the walk", []string{"a", "missing", "link"}, nil},
		{"symlink", []string{"a", "link"}, ErrSymlink},
		{"under a symlink", []string{"a", "link", "x"}, ErrSymlink},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := NoSymlinks(root, c.parts...)
			if !errors.Is(err, c.want) || (c.want == nil) != (err == nil) {
				t.Fatalf("err = %v, want %v", err, c.want)
			}
			if err != nil && !strings.Contains(err.Error(), filepath.Join(root, "a", "link")) {
				t.Fatalf("error %q does not name the symlink", err)
			}
		})
	}
}

// A root that is itself a symlink is not checked: the caller resolved it.
func TestNoSymlinksRootNotChecked(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "real/a")
	link := filepath.Join(dir, "root")
	if err := os.Symlink(filepath.Join(dir, "real"), link); err != nil {
		t.Fatal(err)
	}
	if err := NoSymlinks(link, "a"); err != nil {
		t.Fatal(err)
	}
}

func TestWriteTemp(t *testing.T) {
	dir := t.TempDir()
	p, err := WriteTemp(dir, ".x-*.md", []byte("hello"), 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != dir || !strings.HasPrefix(filepath.Base(p), ".x-") || !strings.HasSuffix(p, ".md") {
		t.Fatalf("path %q", p)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "hello" {
		t.Fatalf("content %q (%v)", b, err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v (%v)", st.Mode(), err)
	}
}

func TestWriteTempFailures(t *testing.T) {
	dir := t.TempDir()
	if _, err := WriteTemp(filepath.Join(dir, "missing"), "x-*", []byte("x"), 0o644); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing dir: %v", err)
	}
	if _, err := WriteTemp(dir, "bad/pattern-*", []byte("x"), 0o644); err == nil {
		t.Fatal("pattern with a separator: no error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("left behind: %v (%v)", entries, err)
	}
}

func TestCreateFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := CreateFile(p, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CreateFile(p, []byte("two"), 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second create: %v, want fs.ErrExist", err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "one" {
		t.Fatalf("content %q (%v), want the first kept", b, err)
	}
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v (%v)", st.Mode(), err)
	}
	if err := CreateFile(filepath.Join(t.TempDir(), "missing", "f"), nil, 0o600); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing dir: %v", err)
	}
}

func TestReplaceFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := ReplaceFile(p, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode %v (%v)", st.Mode(), err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(p, []byte("swapped"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "swapped" {
		t.Fatalf("content %q (%v)", b, err)
	}
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v (%v), want the old 0644 kept", st.Mode(), err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("left behind: %v (%v)", entries, err)
	}
}

// A rename onto a directory fails and takes its temp file with it.
func TestReplaceFileFailure(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "d/x")
	if err := ReplaceFile(filepath.Join(dir, "d"), []byte("x"), 0o600); err == nil {
		t.Fatal("replacing a directory: no error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("left behind: %v (%v)", entries, err)
	}
	if err := ReplaceFile(filepath.Join(dir, "missing", "f"), nil, 0o600); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing dir: %v", err)
	}
}

func TestMissing(t *testing.T) {
	dir := t.TempDir()
	mkdirs(t, dir, "there")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "dangling")); err != nil {
		t.Fatal(err)
	}
	got := Missing(dir, "there", "gone", "dangling", "also-gone")
	if want := []string{filepath.Join(dir, "gone"), filepath.Join(dir, "also-gone")}; !slices.Equal(got, want) {
		t.Fatalf("Missing = %v, want %v", got, want)
	}
}
