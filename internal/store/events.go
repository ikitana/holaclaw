package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"holaclaw/internal/event"
)

const eventColumns = `id, trace_id, span_id, parent_id, session_id, type, time, duration_ns, message_id, attrs, schema`

// Emit persists one trace event. Ephemeral types (model.token) are streamed
// but never stored, and a write failure is logged rather than returned: a
// trace must never break the run (docs/traceability.md).
func (s *SQLite) Emit(ctx context.Context, e event.Event) {
	if e.Type.Ephemeral() {
		return
	}
	if e.ID == "" {
		e.ID = event.NewIDAt(e.Time)
	}
	if e.Schema == 0 {
		e.Schema = event.SchemaVersion
	}
	attrs := string(e.Attrs)
	if attrs == "" {
		attrs = "{}"
	}
	_, err := s.exec(ctx, `INSERT INTO events
		(`+eventColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		string(e.ID), string(e.TraceID), string(e.SpanID), string(e.ParentID), string(e.SessionID),
		string(e.Type), ms(e.Time), int64(e.Duration), string(e.MessageID), attrs, e.Schema)
	if err != nil {
		s.log.Error("store: emit event", "type", e.Type, "trace_id", e.TraceID, "err", err)
	}
}

func scanEvent(sc interface{ Scan(...any) error }) (event.Event, error) {
	var e event.Event
	var id, traceID, spanID, parentID, sessionID, typ, msgID, attrs string
	var t, dur int64
	if err := sc.Scan(&id, &traceID, &spanID, &parentID, &sessionID, &typ, &t, &dur, &msgID, &attrs, &e.Schema); err != nil {
		return event.Event{}, err
	}
	e.ID, e.TraceID, e.SpanID = event.ID(id), event.ID(traceID), event.ID(spanID)
	e.ParentID, e.SessionID, e.MessageID = event.ID(parentID), event.ID(sessionID), event.ID(msgID)
	e.Type, e.Time, e.Duration = event.Type(typ), fromMS(t), time.Duration(dur)
	e.Attrs = []byte(attrs)
	return e, nil
}

// ListEvents returns every event of a trace, in id order.
func (s *SQLite) ListEvents(ctx context.Context, traceID event.ID) ([]event.Event, error) {
	return s.queryEvents(ctx, `SELECT `+eventColumns+` FROM events WHERE trace_id = ? ORDER BY id`, string(traceID))
}

// ReceivedTrace walks events newest first, so a message received recently is
// found without reading the whole log.
func (s *SQLite) ReceivedTrace(ctx context.Context, channel, platformMsgID string) (event.ID, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT trace_id FROM events
		WHERE type = ? AND json_extract(attrs, '$.channel') = ? AND json_extract(attrs, '$.platform_msg_id') = ?
		ORDER BY id DESC LIMIT 1`, string(event.ChannelReceived), channel, platformMsgID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: received trace: %w", err)
	}
	return event.ID(id), nil
}

// ListEventsAfter pages events in id order, optionally restricted to one
// session (SSE resume via Last-Event-ID).
func (s *SQLite) ListEventsAfter(ctx context.Context, after event.ID, sessionID event.ID, limit int) ([]event.Event, error) {
	q := `SELECT ` + eventColumns + ` FROM events WHERE 1 = 1`
	var args []any
	if after != "" {
		q += ` AND id > ?`
		args = append(args, string(after))
	}
	if sessionID != "" {
		q += ` AND session_id = ?`
		args = append(args, string(sessionID))
	}
	q += ` ORDER BY id`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	return s.queryEvents(ctx, q, args...)
}

func (s *SQLite) queryEvents(ctx context.Context, q string, args ...any) ([]event.Event, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []event.Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list events: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
