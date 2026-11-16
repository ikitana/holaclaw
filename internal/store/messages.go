package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"holaclaw/internal/event"
)

// AppendMessage stores a message and links the blobs of its attachments.
// Re-storing the same id is a no-op (client ids are idempotency keys).
func (s *SQLite) AppendMessage(ctx context.Context, m event.Message, pending bool) (bool, error) {
	if m.SessionID == "" {
		return false, errors.New("store: append message: empty session id")
	}
	if m.ID == "" {
		m.ID = event.NewID()
	}
	if m.Schema == 0 {
		m.Schema = event.SchemaVersion
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = m.ID.Time()
	}
	body, err := json.Marshal(m)
	if err != nil {
		return false, fmt.Errorf("store: append message: %w", err)
	}
	pendingFlag := 0
	if pending {
		pendingFlag = 1
	}
	var inserted bool
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		pos := m.Pos
		if pos == "" && !pending {
			// Placed after the transcript's end even when the clock is behind
			// it: a taken mailbox entry can sit ahead of the clock, and the
			// answer to it must not sort before it.
			var last string
			if err := tx.QueryRowContext(ctx,
				`SELECT COALESCE(MAX(pos), '') FROM messages WHERE session_id = ? AND pending = 0`,
				string(m.SessionID)).Scan(&last); err != nil {
				return err
			}
			pos = m.ID
			if pos <= event.ID(last) {
				pos = posAfter(event.ID(last))
			}
		}
		if pos == "" {
			pos = m.ID // a mailbox entry; TakePending places it
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO messages
			(id, session_id, trace_id, role, text, body, pending, created_at, pos)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
			string(m.ID), string(m.SessionID), string(m.TraceID), string(m.Role), m.Text,
			string(body), pendingFlag, ms(m.CreatedAt), string(pos))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		inserted = n == 1
		if !inserted {
			return nil
		}
		// Attachments whose blob is already stored become message_blobs rows;
		// an attachment with no stored blob is simply not linked.
		for _, a := range m.Attachments {
			if a.BlobID == "" {
				continue
			}
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM blobs WHERE id = ?`, a.BlobID).Scan(new(int)); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO message_blobs (message_id, blob_id) VALUES (?, ?)
				 ON CONFLICT (message_id, blob_id) DO NOTHING`, string(m.ID), a.BlobID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("store: append message: %w", err)
	}
	return inserted, nil
}

func decodeMessage(body string) (event.Message, error) {
	var m event.Message
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		return event.Message{}, fmt.Errorf("decode message: %w", err)
	}
	return m, nil
}

func (s *SQLite) GetMessage(ctx context.Context, id event.ID) (event.Message, error) {
	var body, pos string
	err := s.db.QueryRowContext(ctx, `SELECT body, pos FROM messages WHERE id = ?`, string(id)).Scan(&body, &pos)
	if errors.Is(err, sql.ErrNoRows) {
		return event.Message{}, ErrNotFound
	}
	if err != nil {
		return event.Message{}, fmt.Errorf("store: get message: %w", err)
	}
	m, err := decodeMessage(body)
	m.Pos = event.ID(pos)
	return m, err
}

// ListMessages returns the transcript of a session in transcript order (pos),
// excluding pending mailbox entries. after is a message id: the transcript
// resumes past that message's place.
func (s *SQLite) ListMessages(ctx context.Context, sessionID, after event.ID, limit int) ([]event.Message, error) {
	q := `SELECT body, pos FROM messages WHERE session_id = ? AND pending = 0`
	args := []any{string(sessionID)}
	if after != "" {
		// A cursor that is no stored message falls back to comparing ids.
		q += ` AND pos > COALESCE((SELECT pos FROM messages WHERE id = ?), ?)`
		args = append(args, string(after), string(after))
	}
	q += ` ORDER BY pos, id`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	return s.queryMessages(ctx, q, args...)
}

// TakePending atomically un-marks and returns the session's mailbox entries,
// in id order. Each one takes a place after everything already in the
// transcript: a message that arrived while a turn ran sorts after that turn's
// answer, not before it. With perMention it stops at the first message that
// mentions the bot, so a room answers each request on its own; what follows
// stays pending for the next turn.
func (s *SQLite) TakePending(ctx context.Context, sessionID event.ID, perMention bool) ([]event.Message, error) {
	var out []event.Message
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		var last string
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(pos), '') FROM messages WHERE session_id = ? AND pending = 0`,
			string(sessionID)).Scan(&last); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT body FROM messages WHERE session_id = ? AND pending = 1 ORDER BY id`, string(sessionID))
		if err != nil {
			return err
		}
		for rows.Next() {
			var body string
			if err := rows.Scan(&body); err != nil {
				_ = rows.Close()
				return err
			}
			m, err := decodeMessage(body)
			if err != nil {
				_ = rows.Close()
				return err
			}
			out = append(out, m)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if perMention {
			for i, m := range out {
				if m.Mentioned {
					out = out[:i+1]
					break
				}
			}
		}
		pos := event.ID(last)
		for i := range out {
			pos = posAfter(pos)
			out[i].Pos = pos
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET pending = 0, pos = ? WHERE id = ?`,
				string(pos), string(out[i].ID)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: take pending: %w", err)
	}
	return out, nil
}

// posAfter returns a new id that sorts after last: a fresh one, or one a
// millisecond past last when the clock is behind it.
func posAfter(last event.ID) event.ID {
	p := event.NewID()
	if p <= last {
		p = event.NewIDAt(last.Time().Add(time.Millisecond))
	}
	return p
}

// SessionsWithPending lists sessions holding unconsumed mailbox entries.
func (s *SQLite) SessionsWithPending(ctx context.Context) ([]event.ID, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT session_id FROM messages WHERE pending = 1 ORDER BY session_id`)
	if err != nil {
		return nil, fmt.Errorf("store: sessions with pending: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []event.ID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: sessions with pending: %w", err)
		}
		out = append(out, event.ID(id))
	}
	return out, rows.Err()
}

// ListTraceMessages returns every message of one trace, in transcript order.
func (s *SQLite) ListTraceMessages(ctx context.Context, traceID event.ID) ([]event.Message, error) {
	return s.queryMessages(ctx, `SELECT body, pos FROM messages WHERE trace_id = ? ORDER BY pos, id`, string(traceID))
}

func (s *SQLite) queryMessages(ctx context.Context, q string, args ...any) ([]event.Message, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list messages: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []event.Message
	for rows.Next() {
		var body, pos string
		if err := rows.Scan(&body, &pos); err != nil {
			return nil, fmt.Errorf("store: list messages: %w", err)
		}
		m, err := decodeMessage(body)
		if err != nil {
			return nil, fmt.Errorf("store: list messages: %w", err)
		}
		m.Pos = event.ID(pos)
		out = append(out, m)
	}
	return out, rows.Err()
}

// RequeueLastUser marks the newest message of a session as pending again when
// it is a user message with no answer after it. The daemon calls it for
// sessions healed after a crash inside a turn, so the input is replayed
// instead of staying unanswered. It reports whether a message was requeued.
func (s *SQLite) RequeueLastUser(ctx context.Context, sessionID event.ID) (bool, error) {
	res, err := s.exec(ctx, `UPDATE messages SET pending = 1
		WHERE id = (SELECT id FROM messages WHERE session_id = ? ORDER BY pos DESC, id DESC LIMIT 1)
		  AND role = 'user' AND pending = 0`, string(sessionID))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetDerived records text derived from one attachment (transcript, image
// description) on its stored message. Messages are otherwise immutable.
func (s *SQLite) SetDerived(ctx context.Context, messageID, attachmentID event.ID, d event.Derived) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var body string
		err := tx.QueryRowContext(ctx, `SELECT body FROM messages WHERE id = ?`, string(messageID)).Scan(&body)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var m event.Message
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			return err
		}
		found := false
		for i := range m.Attachments {
			if m.Attachments[i].ID == attachmentID {
				m.Attachments[i].Derived = &d
				found = true
			}
		}
		if !found {
			return ErrNotFound
		}
		out, err := json.Marshal(m)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE messages SET body = ? WHERE id = ?`, string(out), string(messageID))
		return err
	})
}

// Repend puts messages back in the mailbox. The session manager calls it when
// a turn was cut short by a shutdown, so the restart replays the input
// instead of losing it (docs/sessions.md).
func (s *SQLite) Repend(ctx context.Context, ids []event.ID) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `UPDATE messages SET pending = 1 WHERE id = ? AND role = 'user'`, string(id)); err != nil {
				return err
			}
		}
		return nil
	})
}
