package tools

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/tool"
)

func TestAttachRoundTrip(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "report.md"), []byte("# hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blobs := &fakeBlobs{}
	r := registry(t, Deps{Blobs: blobs})

	res := run(t, r, "attach", ws, `{"path":"report.md"}`)
	if res.IsError {
		t.Fatalf("attach: %s", res.Text)
	}
	if len(res.Attachments) != 1 {
		t.Fatalf("got %d attachments, want 1", len(res.Attachments))
	}
	a := res.Attachments[0]
	if string(blobs.body) != "# hi\n" {
		t.Errorf("stored body = %q", blobs.body)
	}
	if !a.ID.Valid() {
		t.Errorf("attachment id %q is not a ULID", a.ID)
	}
	if a.BlobID == "" || a.SHA256 != a.BlobID {
		t.Errorf("blob id %q / sha256 %q", a.BlobID, a.SHA256)
	}
	if a.Name != "report.md" || a.Size != 5 {
		t.Errorf("name %q size %d", a.Name, a.Size)
	}
	if !strings.HasPrefix(a.MIME, "text/markdown") || a.Kind != event.KindDocument {
		t.Errorf("mime %q kind %q", a.MIME, a.Kind)
	}
	if blobs.mime != a.MIME {
		t.Errorf("blob stored as %q, attachment says %q", blobs.mime, a.MIME)
	}
	if !strings.Contains(res.Text, "report.md") {
		t.Errorf("text = %q", res.Text)
	}
}

func TestAttachMIMEAndKind(t *testing.T) {
	ws := t.TempDir()
	png := append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, make([]byte, 32)...)
	files := map[string][]byte{
		"pic.png":  png,
		"clip.ogg": []byte("OggS\x00\x02"),
		"noext":    []byte("plain text\n"),
		"sniff":    png, // no extension: content sniffing decides
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(ws, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := registry(t, Deps{Blobs: &fakeBlobs{}})
	want := map[string]event.Kind{
		"pic.png":  event.KindImage,
		"clip.ogg": event.KindAudio,
		"noext":    event.KindDocument,
		"sniff":    event.KindImage,
	}
	for name, kind := range want {
		args, err := json.Marshal(map[string]string{"path": name})
		if err != nil {
			t.Fatal(err)
		}
		res := run(t, r, "attach", ws, string(args))
		if res.IsError {
			t.Fatalf("attach %s: %s", name, res.Text)
		}
		if got := res.Attachments[0].Kind; got != kind {
			t.Errorf("%s: kind %q (mime %q), want %q", name, got, res.Attachments[0].MIME, kind)
		}
	}
}

func TestAttachFailures(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "ok.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(ws, "dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(ws, "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxAttachBytes + 1); err != nil { // sparse, costs nothing
		t.Fatal(err)
	}
	f.Close() //nolint:errcheck // test fixture

	r := registry(t, Deps{Blobs: &fakeBlobs{}})
	for _, tc := range []struct{ args, want string }{
		{`{"path":"missing.txt"}`, "no such file"},
		{`{"path":"dir"}`, "directory"},
		{`{"path":"big.bin"}`, "cap is"},
		{`{"path":""}`, "path is required"},
		{`nope`, "bad arguments"},
	} {
		res := run(t, r, "attach", ws, tc.args)
		if !res.IsError || !strings.Contains(res.Text, tc.want) {
			t.Errorf("attach %s = %q (err=%v), want %q", tc.args, res.Text, res.IsError, tc.want)
		}
		if len(res.Attachments) != 0 {
			t.Errorf("attach %s returned attachments on failure", tc.args)
		}
	}

	// No blob store, and a failing blob store.
	noStore := registry(t, Deps{})
	if res := run(t, noStore, "attach", ws, `{"path":"ok.txt"}`); !res.IsError {
		t.Error("attach without a blob store succeeded")
	}
	broken := tool.NewRegistry()
	if err := Register(broken, Deps{Blobs: &fakeBlobs{err: errors.New("disk full")}}); err != nil {
		t.Fatal(err)
	}
	res := run(t, broken, "attach", ws, `{"path":"ok.txt"}`)
	if !res.IsError || !strings.Contains(res.Text, "disk full") {
		t.Errorf("failing blob store = %q (err=%v)", res.Text, res.IsError)
	}
}

func TestSafeName(t *testing.T) {
	cases := map[string]string{
		"a/b/c.txt":                       "c.txt",
		"weird\x00na\nme":                 "weirdname",
		".hidden":                         "hidden",
		"...":                             "file",
		"":                                "file",
		strings.Repeat("a", 200) + ".txt": strings.Repeat("a", 120),
	}
	for in, want := range cases {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}
