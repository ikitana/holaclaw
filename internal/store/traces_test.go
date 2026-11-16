package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/event"
)

// emitAt writes one event of a trace at a fixed instant, which is what the
// summaries are built out of.
func emitAt(t *testing.T, s *SQLite, sessionID, traceID event.ID, typ event.Type, at time.Time) {
	t.Helper()
	s.Emit(context.Background(), event.Event{
		ID:        event.NewIDAt(at),
		TraceID:   traceID,
		SpanID:    event.NewIDAt(at),
		SessionID: sessionID,
		Type:      typ,
		Time:      at,
	})
}

// mustMessage stores one transcript entry of a trace.
func mustMessage(t *testing.T, s *SQLite, sessionID, traceID event.ID, role event.Role, text string) {
	t.Helper()
	if _, err := s.AppendMessage(context.Background(), event.Message{
		ID: event.NewID(), SessionID: sessionID, TraceID: traceID, Role: role, Text: text,
	}, false); err != nil {
		t.Fatalf("append message: %v", err)
	}
}

func TestListTracesSummarisesEveryTraceOfTheSession(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	one := mustSession(t, s, "one")
	two := mustSession(t, s, "two")

	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	older, newer, elsewhere := event.NewIDAt(t0), event.NewIDAt(t0.Add(time.Minute)), event.NewIDAt(t0)

	emitAt(t, s, one.ID, older, event.RunStarted, t0)
	emitAt(t, s, one.ID, older, event.ToolCallEvent, t0.Add(time.Second))
	emitAt(t, s, one.ID, older, event.ToolCallEvent, t0.Add(2*time.Second))
	emitAt(t, s, one.ID, older, event.RunFinished, t0.Add(3*time.Second))

	emitAt(t, s, one.ID, newer, event.RunStarted, t0.Add(time.Minute))
	emitAt(t, s, one.ID, newer, event.ModelRequest, t0.Add(time.Minute+time.Second))

	emitAt(t, s, two.ID, elsewhere, event.RunStarted, t0)

	// The message that opened the older trace, and one the agent wrote back:
	// only the user's own text may become a preview.
	mustMessage(t, s, one.ID, older, event.RoleUser, "  what is\nthe deploy status?  ")
	mustMessage(t, s, one.ID, older, event.RoleUser, "and the second thing I asked")
	mustMessage(t, s, one.ID, older, event.RoleAssistant, "it is green")
	mustMessage(t, s, one.ID, older, event.RoleTool, "a page the agent fetched")

	got, err := s.ListTraces(ctx, one.ID)
	if err != nil {
		t.Fatalf("list traces: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d traces, want the session's two: %+v", len(got), got)
	}
	// Newest first, so the trace somebody is most likely looking for is the
	// one at the top of the list.
	if got[0].TraceID != newer || got[1].TraceID != older {
		t.Fatalf("order = %s, %s; want the newer trace first", got[0].TraceID, got[1].TraceID)
	}
	first := got[1]
	if first.Events != 4 || first.ToolCalls != 2 {
		t.Errorf("counts = %d events / %d tool calls, want 4 / 2", first.Events, first.ToolCalls)
	}
	if first.LastType != event.RunFinished {
		t.Errorf("last type = %q, want the last event of the trace", first.LastType)
	}
	if first.SessionID != one.ID {
		t.Errorf("session = %q, want %q", first.SessionID, one.ID)
	}
	if !first.StartedAt.Equal(t0) || !first.EndedAt.Equal(t0.Add(3*time.Second)) {
		t.Errorf("span = %s … %s, want %s … %s", first.StartedAt, first.EndedAt, t0, t0.Add(3*time.Second))
	}
	if first.Preview != "what is the deploy status?" {
		t.Errorf("preview = %q, want the first user message on one line", first.Preview)
	}
	// The other session's trace is not in this list, and a trace nobody typed
	// into has no preview to show.
	if got[0].Preview != "" {
		t.Errorf("preview = %q, want none for a trace with no user message", got[0].Preview)
	}
}

func TestListTracesClipsALongPreview(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sess := mustSession(t, s, "long")
	trace := event.NewID()
	emitAt(t, s, sess.ID, trace, event.RunStarted, time.Now().UTC())
	mustMessage(t, s, sess.ID, trace, event.RoleUser, strings.Repeat("é", 400))

	got, err := s.ListTraces(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list traces: %v", err)
	}
	if n := len([]rune(got[0].Preview)); n != tracePreviewRunes {
		t.Fatalf("preview is %d runes, want it clipped to %d", n, tracePreviewRunes)
	}
}

func TestListTracesIsEmptyForASessionWithNoEvents(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sess := mustSession(t, s, "quiet")

	got, err := s.ListTraces(ctx, sess.ID)
	if err != nil {
		t.Fatalf("list traces: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d traces, want none", len(got))
	}
	// And so is a session id that never existed: there is nothing to say
	// about it here, the API is what decides it is a not_found.
	got, err = s.ListTraces(ctx, event.NewID())
	if err != nil || len(got) != 0 {
		t.Fatalf("unknown session: %d traces, err %v", len(got), err)
	}
	if got, err := s.ListTraces(ctx, ""); err != nil || got != nil {
		t.Fatalf("empty session id: %v, %v", got, err)
	}
}

// The browser asks through this interface, never through *SQLite.
func TestSQLiteIsATraceLister(t *testing.T) {
	var _ TraceLister = (*SQLite)(nil)
}
