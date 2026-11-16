package skill

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/pelletier/go-toml/v2"

	"holaclaw/internal/fsutil"
)

// packed is the folder dir as the CLI sends it.
func packed(t *testing.T, dir string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	if err := fsutil.WriteTarGz(&buf, dir, fsutil.CopyOptions{}); err != nil {
		t.Fatal(err)
	}
	return &buf
}

// rawTar is a gzipped tar of headers, each regular file with body "x" and
// a size of its own.
func rawTar(t *testing.T, hdrs ...tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, h := range hdrs {
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg && h.Size == 1 {
			if _, err := tw.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
		}
		if h.Size > 1 {
			// Cut short: the size alone is past a limit.
			_ = zw.Close()
			return &buf
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func dirHdr(name string) tar.Header {
	return tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: 0o755}
}

func fileHdr(name string) tar.Header {
	return tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: 1}
}

func TestSaveUpload(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "tmp")
	src := srcSkill(t, "deploy")
	put(t, src, ".git/HEAD", "x")
	before := time.Now()
	u, err := SaveUpload(tmp, packed(t, src), "/home/me/deploy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ulid.ParseStrict(u.ID); err != nil || filepath.Base(u.Dir) != "deploy" || u.Path != "/home/me/deploy" ||
		u.Expires.Before(before.Add(UploadTTL)) || u.Expires.After(time.Now().Add(UploadTTL)) {
		t.Fatalf("upload = %+v, %v", u, err)
	}
	if !strings.HasPrefix(u.Dir, filepath.Join(tmp, uploadPrefix+u.ID)) {
		t.Errorf("dir %s is not under %s", u.Dir, tmp)
	}
	for _, rel := range []string{"SKILL.md", "references/notes.md"} {
		if _, err := os.Stat(filepath.Join(u.Dir, rel)); err != nil {
			t.Error(err)
		}
	}
	if _, err := os.Stat(filepath.Join(u.Dir, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".git kept: %v", err)
	}
	got, err := OpenUpload(tmp, u.ID)
	if err != nil || got.Dir != u.Dir || got.Path != u.Path || !got.Expires.Equal(u.Expires) {
		t.Fatalf("open = %+v, %v; want %+v", got, err, u)
	}
	if err := RemoveUpload(tmp, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUpload(tmp, u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("removed upload opens: %v", err)
	}
	if err := RemoveUpload(tmp, u.ID); err != nil {
		t.Errorf("remove twice: %v", err)
	}
}

func TestSaveUploadRefuses(t *testing.T) {
	tmp := t.TempDir()
	many := []tar.Header{dirHdr("s/")}
	for i := range MaxInstallFiles + 1 {
		many = append(many, fileHdr("s/f"+strings.Repeat("x", i%50)+string(rune('a'+i/50))))
	}
	for _, c := range []struct {
		name string
		body *bytes.Buffer
		msg  string
	}{
		{"not gzip", bytes.NewBufferString("plain text"), "gzip"},
		{"empty", rawTar(t), "one folder"},
		{"two folders", rawTar(t, dirHdr("a/"), dirHdr("b/")), "one folder"},
		{"a file", rawTar(t, fileHdr("SKILL.md")), "one folder"},
		{"hidden folder", rawTar(t, dirHdr(".x/")), "one folder"},
		{"symlink", rawTar(t, dirHdr("s/"), tar.Header{Typeflag: tar.TypeSymlink, Name: "s/l", Linkname: "/etc"}), "is a symlink"},
		{"hardlink", rawTar(t, dirHdr("s/"), tar.Header{Typeflag: tar.TypeLink, Name: "s/l", Linkname: "s/x"}), "is a symlink"},
		{"device", rawTar(t, dirHdr("s/"), tar.Header{Typeflag: tar.TypeBlock, Name: "s/d"}), "not a regular file"},
		{"escape", rawTar(t, dirHdr("s/"), fileHdr("s/../../x")), "not a local path"},
		{"absolute", rawTar(t, fileHdr("/tmp/x")), "not a local path"},
		{"too many", rawTar(t, many...), "too many files"},
		{"too large", rawTar(t, dirHdr("s/"), tar.Header{Typeflag: tar.TypeReg, Name: "s/big", Mode: 0o644, Size: MaxInstallTotal + 1}), "too large"},
	} {
		if u, err := SaveUpload(tmp, c.body, ""); !errors.Is(err, ErrBadSource) || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: %+v, %v; want ErrBadSource with %q", c.name, u, err, c.msg)
		}
		if left := tmpEntries(t, tmp); len(left) != 0 {
			t.Fatalf("%s: left %v", c.name, left)
		}
	}
}

// age sets the creation time an upload records.
func age(t *testing.T, tmp, id string, created time.Time) {
	t.Helper()
	b, err := toml.Marshal(meta{Path: "/p", Created: created})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, uploadPrefix+id, uploadMeta), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestUploadExpires(t *testing.T) {
	tmp := t.TempDir()
	old, err := SaveUpload(tmp, packed(t, srcSkill(t, "old")), "")
	if err != nil {
		t.Fatal(err)
	}
	age(t, tmp, old.ID, time.Now().Add(-UploadTTL-time.Minute))
	if _, err := OpenUpload(tmp, old.ID); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired = %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, uploadPrefix+old.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expired upload kept: %v", err)
	}

	// Every save sweeps the expired uploads and keeps the fresh ones; one
	// without a record goes by its folder's time.
	stale, err := SaveUpload(tmp, packed(t, srcSkill(t, "stale")), "")
	if err != nil {
		t.Fatal(err)
	}
	age(t, tmp, stale.ID, time.Now().Add(-2*UploadTTL))
	fresh, err := SaveUpload(tmp, packed(t, srcSkill(t, "fresh")), "")
	if err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(tmp, uploadPrefix+ulid.Make().String())
	put(t, bare, "x/SKILL.md", "x")
	long := time.Now().Add(-2 * UploadTTL)
	if err := os.Chtimes(bare, long, long); err != nil {
		t.Fatal(err)
	}
	if _, err := SaveUpload(tmp, packed(t, srcSkill(t, "next")), ""); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string]bool{
		filepath.Join(tmp, uploadPrefix+stale.ID): false,
		bare: false,
		filepath.Join(tmp, uploadPrefix+fresh.ID): true,
	} {
		if _, err := os.Stat(dir); (err == nil) != want {
			t.Errorf("%s kept = %v, want %v", filepath.Base(dir), err == nil, want)
		}
	}
}

func TestOpenUploadUnknown(t *testing.T) {
	tmp := t.TempDir()
	put(t, tmp, "elsewhere/x/SKILL.md", "x")
	for _, id := range []string{ulid.Make().String(), "", "../elsewhere", "01ARZ3NDEKTSV4RRFFQ69G5FA/"} {
		if _, err := OpenUpload(tmp, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("OpenUpload(%q) = %v", id, err)
		}
	}
	if err := RemoveUpload(tmp, "../elsewhere"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RemoveUpload outside = %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "elsewhere")); err != nil {
		t.Error(err)
	}
}

func TestCleanTmp(t *testing.T) {
	tmp := t.TempDir()
	for _, rel := range []string{"skill-git-1/r/x", "skill-stage-2/deploy/SKILL.md", "skill-upload-3/.meta.toml", "other/x"} {
		put(t, tmp, rel, "x")
	}
	if err := CleanTmp(tmp); err != nil {
		t.Fatal(err)
	}
	if left := tmpEntries(t, tmp); len(left) != 0 {
		t.Errorf("left %v", left)
	}
	if _, err := os.Stat(filepath.Join(tmp, "other", "x")); err != nil {
		t.Errorf("CleanTmp took more: %v", err)
	}
	if err := CleanTmp(filepath.Join(tmp, "missing")); err != nil {
		t.Errorf("missing root: %v", err)
	}
}
