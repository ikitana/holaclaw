package page

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"holaclaw/internal/browser/cdp"
	"holaclaw/internal/browser/proto"
)

func newTestQuarantine(t *testing.T) *quarantine {
	t.Helper()
	dir := t.TempDir()
	return newQuarantine(dir, slog.New(slog.DiscardHandler))
}

// write puts a file where Chromium would have ("allowAndName": named by GUID).
func writeBrowserFile(t *testing.T, q *quarantine, guid string, size int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(q.dir, guid), bytes.Repeat([]byte("x"), size), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestQuarantineFilesACompletedDownload(t *testing.T) {
	f := newFakeCDP(t)
	q := newTestQuarantine(t)
	q.willBegin(cdp.DownloadWillBeginEvent{GUID: "g1", SuggestedFilename: "../../etc/passwd"})
	writeBrowserFile(t, q, "g1", 64)

	mark := q.mark()
	q.progress(t.Context(), f.conn, cdp.DownloadProgressEvent{GUID: "g1", State: "completed", ReceivedBytes: 64})

	got := q.since(mark)
	if len(got) != 1 {
		t.Fatalf("since = %+v, want one file", got)
	}
	if got[0].Name != "passwd" {
		t.Fatalf("name = %q, want the suggested name sanitised to one element", got[0].Name)
	}
	if got[0].Size != 64 || got[0].ID == "" {
		t.Fatalf("download = %+v", got[0])
	}
	if _, err := os.Stat(filepath.Join(q.dir, got[0].ID)); err != nil {
		t.Fatalf("the file is not stored under its opaque id: %v", err)
	}
	if _, err := os.Stat(filepath.Join(q.dir, "g1")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the browser's own file name survived")
	}
	// The mark is what one action downloaded: nothing new since.
	if got := q.since(q.mark()); len(got) != 0 {
		t.Fatalf("since the new mark = %+v", got)
	}
}

func TestQuarantineCancelsOversizeDownload(t *testing.T) {
	f := newFakeCDP(t)
	q := newTestQuarantine(t)
	q.willBegin(cdp.DownloadWillBeginEvent{GUID: "g1", SuggestedFilename: "big.bin"})
	writeBrowserFile(t, q, "g1", 128)

	q.progress(t.Context(), f.conn, cdp.DownloadProgressEvent{
		GUID: "g1", State: "inProgress", ReceivedBytes: proto.MaxDownloadBytes + 1,
	})
	if _, ok := f.sent("Browser.cancelDownload"); !ok {
		t.Fatalf("the download was not cancelled: %v", f.methods())
	}
	if _, err := os.Stat(filepath.Join(q.dir, "g1")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the partial file was kept")
	}
	if got := q.since(0); len(got) != 0 {
		t.Fatalf("a cancelled download was filed: %+v", got)
	}
}

func TestQuarantineRefusesOverTotal(t *testing.T) {
	f := newFakeCDP(t)
	q := newTestQuarantine(t)
	q.mu.Lock()
	q.total = proto.MaxDownloadTotal - 10
	q.mu.Unlock()

	q.willBegin(cdp.DownloadWillBeginEvent{GUID: "g1", SuggestedFilename: "one.bin"})
	writeBrowserFile(t, q, "g1", 64)
	q.progress(t.Context(), f.conn, cdp.DownloadProgressEvent{GUID: "g1", State: "completed", ReceivedBytes: 64})

	if got := q.since(0); len(got) != 0 {
		t.Fatalf("a download over the agent's total was filed: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(q.dir, "g1")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the file was kept")
	}
}

func TestQuarantineSweepsOldFiles(t *testing.T) {
	f := newFakeCDP(t)
	q := newTestQuarantine(t)
	q.willBegin(cdp.DownloadWillBeginEvent{GUID: "g1", SuggestedFilename: "old.bin"})
	writeBrowserFile(t, q, "g1", 8)
	q.progress(t.Context(), f.conn, cdp.DownloadProgressEvent{GUID: "g1", State: "completed", ReceivedBytes: 8})
	got := q.since(0)
	if len(got) != 1 {
		t.Fatalf("setup: %+v", got)
	}
	id := got[0].ID
	q.mu.Lock()
	q.files[0].at = time.Now().Add(-proto.MaxDownloadAge - time.Minute)
	q.mu.Unlock()

	q.sweep()
	if left := q.since(0); len(left) != 0 {
		t.Fatalf("a file older than MaxDownloadAge survived: %+v", left)
	}
	if _, err := os.Stat(filepath.Join(q.dir, id)); err == nil {
		t.Error("the swept file is still on disk")
	}
}

// Download hands the bytes over once and takes the file away with it.
func TestEngineDownloadReadsOnceThenDeletes(t *testing.T) {
	f := newFakeCDP(t)
	e := testEngineNoBrowser(t)
	q := newTestQuarantine(t)
	e.sessions["alpha"] = &session{e: e, agent: "alpha", dl: q}

	q.willBegin(cdp.DownloadWillBeginEvent{GUID: "g1", SuggestedFilename: "notes.txt"})
	writeBrowserFile(t, q, "g1", 5)
	q.progress(t.Context(), f.conn, cdp.DownloadProgressEvent{GUID: "g1", State: "completed", ReceivedBytes: 5})
	id := q.since(0)[0].ID

	rc, meta, err := e.Download(t.Context(), "alpha", id)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	if string(body) != "xxxxx" || meta.Name != "notes.txt" {
		t.Fatalf("body = %q, meta = %+v", body, meta)
	}
	if _, err := os.Stat(filepath.Join(q.dir, id)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the file survived being fetched")
	}
	// A second fetch of the same id is a bad action, not a panic.
	if _, _, err := e.Download(t.Context(), "alpha", id); !isCode(err, proto.ErrBadAction) {
		t.Fatalf("second Download = %v, want bad_action", err)
	}
	for _, id := range []string{"", "..", "a/b", "x.y"} {
		if _, _, err := e.Download(t.Context(), "alpha", id); !isCode(err, proto.ErrBadAction) {
			t.Errorf("Download(%q) = %v, want bad_action", id, err)
		}
	}
}

func TestSanitiseName(t *testing.T) {
	cases := map[string]string{
		"report.pdf":            "report.pdf",
		"../../etc/shadow":      "shadow",
		"/absolute/path.txt":    "path.txt",
		"":                      "download",
		"..":                    "download",
		"with\nnewline.txt":     "withnewline.txt",
		"trailing dots...":      "trailing dots",
		"a\\windows\\path.docx": "a_windows_path.docx",
	}
	for in, want := range cases {
		if got := sanitiseName(in); got != want {
			t.Errorf("sanitiseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func isCode(err error, code proto.ErrorCode) bool {
	var pe *proto.Error
	return errors.As(err, &pe) && pe.Code == code
}
