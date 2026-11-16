package store

import (
	"context"
	"strings"
	"testing"

	"holaclaw/internal/event"
)

// TestBlobSessions covers the authorization lookup behind GET /v1/blobs/{id}:
// every session that carries the blob, once, and nothing for a blob no message
// references.
func TestBlobSessions(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	one := mustSession(t, s, "main/tg-main/one")
	two := mustSession(t, s, "main/tg-main/two")

	shared, err := s.PutBlob(ctx, strings.NewReader("shared bytes"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := s.PutBlob(ctx, strings.NewReader("nobody wants me"), "image/png")
	if err != nil {
		t.Fatal(err)
	}

	att := event.Attachment{
		ID: event.NewID(), BlobID: shared.ID, MIME: shared.MIME, Name: "f.png",
		Size: shared.Size, SHA256: shared.ID, Kind: event.KindFromMIME(shared.MIME),
	}
	for _, sess := range []Session{one, two, one} { // twice in `one`: still one row
		m := msg(sess.ID, event.NewID(), event.RoleUser, "see attached")
		att.ID = event.NewID()
		m.Attachments = []event.Attachment{att}
		if _, err := s.AppendMessage(ctx, m, false); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.BlobSessions(ctx, shared.ID)
	if err != nil {
		t.Fatalf("blob sessions: %v", err)
	}
	seen := map[event.ID]int{}
	for _, id := range got {
		seen[id]++
	}
	if len(got) != 2 || seen[one.ID] != 1 || seen[two.ID] != 1 {
		t.Fatalf("sessions = %v, want one of each of %s and %s", got, one.ID, two.ID)
	}

	empty, err := s.BlobSessions(ctx, orphan.ID)
	if err != nil {
		t.Fatalf("blob sessions (orphan): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("orphan blob sessions = %v, want none", empty)
	}

	unknown, err := s.BlobSessions(ctx, "not-a-blob")
	if err != nil || len(unknown) != 0 {
		t.Fatalf("unknown blob = %v, err %v", unknown, err)
	}
}
