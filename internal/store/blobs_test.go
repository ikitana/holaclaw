package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holaclaw/internal/event"
)

func TestBlobRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	data := []byte("hello blobs")
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])

	b, err := s.PutBlob(ctx, bytes.NewReader(data), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != want || b.Size != int64(len(data)) || b.MIME != "text/plain" {
		t.Fatalf("got %+v, want id %s", b, want)
	}
	// Sharded path: blobs/ab/cd/<hex>.
	path := filepath.Join(s.dir, "blobs", want[0:2], want[2:4], want)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("blob file: %v", err)
	}
	rc, meta, err := s.OpenBlob(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	got, err := io.ReadAll(rc)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read %q, err %v", got, err)
	}
	if meta.Size != b.Size || meta.MIME != b.MIME || meta.CreatedAt.IsZero() {
		t.Fatalf("meta = %+v", meta)
	}

	// Dedupe: same content, same id, one row, first metadata wins.
	b2, err := s.PutBlob(ctx, bytes.NewReader(data), "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}
	if b2.ID != b.ID || b2.MIME != "text/plain" {
		t.Fatalf("dedupe = %+v", b2)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM blobs`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("blob rows = %d, err %v", n, err)
	}
	// No temp files are left behind.
	tmps, _ := filepath.Glob(filepath.Join(s.dir, "blobs", tmpSubdir, "*"))
	if len(tmps) != 0 {
		t.Fatalf("temp files left: %v", tmps)
	}

	if _, _, err := s.OpenBlob(ctx, "deadbeef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open missing blob: %v", err)
	}
	// Metadata without bytes is also "not found".
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.OpenBlob(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open blob with no file: %v", err)
	}
}

func TestBlobSizeCap(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, WithMaxBlobSize(16))

	if _, err := s.PutBlob(ctx, strings.NewReader(strings.Repeat("x", 16)), "text/plain"); err != nil {
		t.Fatalf("blob exactly at the cap: %v", err)
	}
	_, err := s.PutBlob(ctx, strings.NewReader(strings.Repeat("x", 17)), "text/plain")
	if !errors.Is(err, ErrBlobTooLarge) {
		t.Fatalf("err = %v, want ErrBlobTooLarge", err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM blobs`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("blob rows = %d, err %v", n, err)
	}
	if tmps, _ := filepath.Glob(filepath.Join(s.dir, "blobs", tmpSubdir, "*")); len(tmps) != 0 {
		t.Fatalf("temp files left: %v", tmps)
	}
}

// TestMessageBlobsAndCascade covers attachment linking and the blob cleanup
// that follows a session delete.
func TestMessageBlobsAndCascade(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sess := mustSession(t, s, "main/tg-main/blobs")
	other := mustSession(t, s, "main/tg-main/keeps")

	shared, err := s.PutBlob(ctx, strings.NewReader("shared"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	only, err := s.PutBlob(ctx, strings.NewReader("only here"), "image/png")
	if err != nil {
		t.Fatal(err)
	}

	att := func(b Blob) event.Attachment {
		return event.Attachment{ID: event.NewID(), BlobID: b.ID, MIME: b.MIME, Name: "f.png",
			Size: b.Size, SHA256: b.ID, Kind: event.KindFromMIME(b.MIME)}
	}
	m := msg(sess.ID, event.NewID(), event.RoleUser, "see attached")
	m.Attachments = []event.Attachment{att(shared), att(only), {ID: event.NewID(), BlobID: "0000"}}
	if _, err := s.AppendMessage(ctx, m, false); err != nil {
		t.Fatal(err)
	}
	keep := msg(other.ID, event.NewID(), event.RoleUser, "also attached")
	keep.Attachments = []event.Attachment{att(shared)}
	if _, err := s.AppendMessage(ctx, keep, false); err != nil {
		t.Fatal(err)
	}
	// Only attachments with a stored blob are linked.
	var links int
	if err := s.DB().QueryRow(`SELECT count(*) FROM message_blobs WHERE message_id = ?`, string(m.ID)).Scan(&links); err != nil {
		t.Fatal(err)
	}
	if links != 2 {
		t.Fatalf("message_blobs = %d, want 2", links)
	}
	// The round-tripped message still carries all three attachments.
	got, err := s.GetMessage(ctx, m.ID)
	if err != nil || len(got.Attachments) != 3 || got.Attachments[0].Kind != event.KindImage {
		t.Fatalf("got %+v, err %v", got.Attachments, err)
	}

	s.Emit(ctx, event.New(event.ChannelReceived, m.TraceID, sess.ID, "", nil))

	if err := s.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := s.GetMessage(ctx, m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("message survived: %v", err)
	}
	evs, err := s.ListEventsAfter(ctx, "", "", 0)
	if err != nil || len(evs) != 0 {
		t.Fatalf("events survived: %+v, err %v", evs, err)
	}
	// The blob nobody else references is gone, rows and bytes.
	if _, _, err := s.OpenBlob(ctx, only.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan blob survived: %v", err)
	}
	if _, err := os.Stat(s.blobPath(only.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan blob file survived: %v", err)
	}
	// The shared one is still referenced by the other session.
	rc, _, err := s.OpenBlob(ctx, shared.ID)
	if err != nil {
		t.Fatalf("shared blob dropped: %v", err)
	}
	_ = rc.Close()
}
