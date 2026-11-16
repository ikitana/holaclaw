package fsutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTarGzRoundTrip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "deploy")
	mkdirs(t, src, "references/deep", "empty", ".git")
	for p, mode := range map[string]os.FileMode{
		"SKILL.md": 0o600, "references/deep/b.md": 0o644, "run.sh": 0o700, ".git/HEAD": 0o644,
	} {
		if err := os.WriteFile(filepath.Join(src, p), []byte("content of "+p), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(src, "SKILL.md"), filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	skip := func(rel string, _ fs.DirEntry) bool { return rel == ".git" }
	var buf bytes.Buffer
	if err := WriteTarGz(&buf, src, CopyOptions{Skip: skip}); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := ExtractTarGz(bytes.NewReader(buf.Bytes()), dst, CopyOptions{MaxFiles: 4, MaxBytes: 1 << 10}); err != nil {
		t.Fatal(err)
	}
	var got []string
	err := filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dst {
			return err
		}
		rel, _ := filepath.Rel(dst, p)
		st, _ := d.Info()
		got = append(got, filepath.ToSlash(rel)+" "+st.Mode().String())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"deploy drwxr-xr-x",
		"deploy/SKILL.md -rw-r--r--",
		"deploy/empty drwxr-xr-x",
		"deploy/references drwxr-xr-x",
		"deploy/references/deep drwxr-xr-x",
		"deploy/references/deep/b.md -rw-r--r--",
		"deploy/run.sh -rwxr-xr-x",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extracted\n%v\nwant\n%v", got, want)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "deploy", "run.sh")); err != nil || string(b) != "content of run.sh" {
		t.Errorf("run.sh = %q, %v", b, err)
	}

	// The same limits and refusals apply on the way in.
	for _, c := range []struct {
		o    CopyOptions
		want error
	}{
		{CopyOptions{Skip: skip, RefuseSymlinks: true}, ErrSymlink},
		{CopyOptions{Skip: skip, MaxFiles: 2}, ErrTooMany},
		{CopyOptions{Skip: skip, MaxBytes: 10}, ErrTooLarge},
	} {
		if err := WriteTarGz(&bytes.Buffer{}, src, c.o); !errors.Is(err, c.want) {
			t.Errorf("WriteTarGz(%+v) = %v, want %v", c.o, err, c.want)
		}
	}
	for _, p := range []string{filepath.Join(src, "SKILL.md"), filepath.Join(src, "missing"), "/"} {
		if err := WriteTarGz(&bytes.Buffer{}, p, CopyOptions{}); err == nil {
			t.Errorf("WriteTarGz(%s) wrote", p)
		}
	}
}

// entry is one tar header, with body for a regular file.
type entry struct {
	h    tar.Header
	body string
}

func dirEntry(name string) entry {
	return entry{h: tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: 0o755}}
}

func fileEntry(name, body string) entry {
	return entry{h: tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(body))}, body: body}
}

// tarGz writes entries as a gzipped tar. A file whose Size is larger than
// its body is cut short: the archive ends after its header.
func tarGz(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		if err := tw.WriteHeader(&e.h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
		if e.h.Size > int64(len(e.body)) {
			if err := tw.Flush(); err == nil {
				t.Fatal("flush of a short file")
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			return buf.Bytes()
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractTarGzRefuses(t *testing.T) {
	many := []entry{dirEntry("s/")}
	for i := range 4 {
		many = append(many, fileEntry("s/f"+string(rune('a'+i)), "x"))
	}
	limits := CopyOptions{MaxFiles: 3, MaxBytes: 1 << 20}
	for _, c := range []struct {
		name    string
		archive []byte
		want    error
		msg     string
	}{
		{"dotdot", tarGz(t, dirEntry("s/"), fileEntry("s/../../x", "x")), nil, "not a local path"},
		{"absolute", tarGz(t, fileEntry("/etc/x", "x")), nil, "not a local path"},
		{"unclean", tarGz(t, dirEntry("s/"), fileEntry("s//x", "x")), nil, "not a local path"},
		{"dot", tarGz(t, dirEntry("./")), nil, "not a local path"},
		{"symlink", tarGz(t, dirEntry("s/"), entry{h: tar.Header{Typeflag: tar.TypeSymlink, Name: "s/l", Linkname: "/etc"}}), ErrSymlink, "s/l: "},
		{"hardlink", tarGz(t, dirEntry("s/"), entry{h: tar.Header{Typeflag: tar.TypeLink, Name: "s/l", Linkname: "s/x"}}), ErrSymlink, "s/l: "},
		{"device", tarGz(t, dirEntry("s/"), entry{h: tar.Header{Typeflag: tar.TypeChar, Name: "s/null", Devmajor: 1, Devminor: 3}}), ErrNotRegular, "s/null: "},
		{"fifo", tarGz(t, entry{h: tar.Header{Typeflag: tar.TypeFifo, Name: "p"}}), ErrNotRegular, "p: "},
		{"too many", tarGz(t, many...), ErrTooMany, "s/fd: "},
		{"too large", tarGz(t, dirEntry("s/"), fileEntry("s/a", "abc"), entry{h: tar.Header{Typeflag: tar.TypeReg, Name: "s/big", Mode: 0o644, Size: 1 << 20}}), ErrTooLarge, "s/big: "},
		{"no parent", tarGz(t, fileEntry("s/x", "x")), fs.ErrNotExist, ""},
		{"twice", tarGz(t, fileEntry("x", "a"), fileEntry("x", "b")), fs.ErrExist, ""},
		{"not gzip", []byte("plain text, not an archive"), gzip.ErrHeader, ""},
		{"truncated", tarGz(t, entry{h: tar.Header{Typeflag: tar.TypeReg, Name: "x", Mode: 0o644, Size: 10}, body: "abc"}), nil, "EOF"},
	} {
		dst := filepath.Join(t.TempDir(), "out")
		err := ExtractTarGz(bytes.NewReader(c.archive), dst, limits)
		if err == nil || (c.want != nil && !errors.Is(err, c.want)) || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %v, want %v with %q", c.name, err, c.want, c.msg)
		}
	}
	dst := t.TempDir()
	if err := ExtractTarGz(bytes.NewReader(tarGz(t, fileEntry("x", "a"))), dst, CopyOptions{}); !errors.Is(err, fs.ErrExist) {
		t.Errorf("existing dst: %v", err)
	}
}

// A skipped directory leaves out everything under it.
func TestExtractTarGzSkip(t *testing.T) {
	archive := tarGz(t, dirEntry("s/"), dirEntry("s/.git/"), fileEntry("s/.git/HEAD", "x"), fileEntry("s/a", "a"))
	dst := filepath.Join(t.TempDir(), "out")
	skip := func(rel string, d fs.DirEntry) bool { return d.IsDir() && filepath.Base(rel) == ".git" }
	if err := ExtractTarGz(bytes.NewReader(archive), dst, CopyOptions{Skip: skip}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "s", ".git")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf(".git extracted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "s", "a")); err != nil {
		t.Error(err)
	}
}
