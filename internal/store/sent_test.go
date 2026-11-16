package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"holaclaw/internal/event"
)

func TestSentMessages(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sess := mustSession(t, s, "main/tg/42")
	msgID, apprID := event.NewID(), event.NewID()
	old := time.Now().Add(-48 * time.Hour)

	if err := s.PutSent(ctx, nil); err != nil {
		t.Fatalf("empty put: %v", err)
	}
	if err := s.PutSent(ctx, []Sent{
		{Channel: "tg", Conversation: "42", PlatformMsgID: "42:100", SessionID: sess.ID, MessageID: msgID},
		{Channel: "tg", Conversation: "42", PlatformMsgID: "42:101", SessionID: sess.ID, ApprovalID: apprID, CreatedAt: old},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := s.GetSent(ctx, "tg", "42:100")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.SessionID != sess.ID || got.MessageID != msgID || got.ApprovalID != "" || got.Conversation != "42" || got.CreatedAt.IsZero() {
		t.Fatalf("row = %+v", got)
	}
	if got, err := s.GetSent(ctx, "tg", "42:101"); err != nil || got.ApprovalID != apprID {
		t.Fatalf("prompt row = %+v, %v", got, err)
	}
	// The whole id matches, never a part of it, and only on its own instance.
	for _, k := range [][2]string{{"tg", "100"}, {"tg", "2:100"}, {"tg", "42:10"}, {"tg2", "42:100"}} {
		if _, err := s.GetSent(ctx, k[0], k[1]); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%v: err = %v, want ErrNotFound", k, err)
		}
	}
	if err := s.PutSent(ctx, []Sent{{Channel: "tg", PlatformMsgID: "x"}}); err == nil {
		t.Fatal("a row without a session was stored")
	}

	n, err := s.PruneSent(ctx, time.Now().Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("prune = %d, %v; want 1", n, err)
	}
	if _, err := s.GetSent(ctx, "tg", "42:101"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pruned row: err = %v", err)
	}

	// Rows go with their session.
	if err := s.DeleteSession(ctx, sess.ID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := s.GetSent(ctx, "tg", "42:100"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after session delete: err = %v", err)
	}
}
