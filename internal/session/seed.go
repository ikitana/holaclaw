package session

import (
	"context"
	"errors"
	"fmt"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// SeedThread starts a new thread session with the exchange its root message
// started: the user and assistant messages of the trace that received
// o.ThreadRoot, copied with new ids and their taint. Tool calls and results
// stay behind; a copy needs none of them. A root never received, or received
// in this same session, seeds nothing. Call it once, right after
// EnsureSession created the session and before its first message is stored.
func SeedThread(ctx context.Context, st store.Store, sess store.Session, o event.Origin, traceID event.ID) error {
	if o.Thread == "" || o.ThreadRoot == "" {
		return nil
	}
	root, err := st.ReceivedTrace(ctx, o.Channel, o.ThreadRoot)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("session: seed thread: %w", err)
	}
	msgs, err := st.ListTraceMessages(ctx, root)
	if err != nil {
		return fmt.Errorf("session: seed thread: %w", err)
	}
	for _, m := range msgs {
		if m.SessionID == sess.ID || m.Role == event.RoleTool || len(m.ToolCalls) > 0 ||
			(m.Role != event.RoleUser && m.Role != event.RoleAssistant) {
			continue
		}
		m.ID, m.SessionID, m.TraceID, m.Pos, m.Mentioned = event.NewID(), sess.ID, traceID, "", false
		if _, err := st.AppendMessage(ctx, m, false); err != nil {
			return fmt.Errorf("session: seed thread: %w", err)
		}
	}
	return nil
}
