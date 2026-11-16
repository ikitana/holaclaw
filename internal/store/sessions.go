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

const sessionColumns = `id, key, agent, kind, user_id, origin, title, prefix_hash, summary_msg_id,
	compact_fails, turn_token, run_generation, created_at, updated_at, ended_at`

func scanSession(sc interface{ Scan(...any) error }) (Session, error) {
	var s Session
	var id, key, kind, origin string
	var userID sql.NullString
	var created, updated int64
	var ended sql.NullInt64
	var summaryMsgID string
	if err := sc.Scan(&id, &key, &s.Agent, &kind, &userID, &origin, &s.Title, &s.PrefixHash,
		&summaryMsgID, &s.CompactFails, &s.TurnToken, &s.RunGeneration, &created, &updated, &ended); err != nil {
		return Session{}, err
	}
	s.ID, s.Key, s.Kind = event.ID(id), key, SessionKind(kind)
	s.UserID = event.ID(userID.String)
	s.SummaryMsgID = event.ID(summaryMsgID)
	s.CreatedAt, s.UpdatedAt = fromMS(created), fromMS(updated)
	if ended.Valid {
		t := fromMS(ended.Int64)
		s.EndedAt = &t
	}
	if origin != "" {
		if err := json.Unmarshal([]byte(origin), &s.Origin); err != nil {
			return Session{}, fmt.Errorf("decode origin: %w", err)
		}
	}
	return s, nil
}

func nullID(id event.ID) any {
	if id == "" {
		return nil
	}
	return string(id)
}

// EnsureSession creates the session for s.Key if it does not exist and returns
// the stored row either way, in one transaction (docs/sessions.md: registry
// lookup and mailbox creation are one atomic operation).
func (s *SQLite) EnsureSession(ctx context.Context, in Session) (Session, bool, error) {
	if in.Key == "" {
		return Session{}, false, errors.New("store: ensure session: empty key")
	}
	if in.ID == "" {
		in.ID = event.NewID()
	}
	if in.Kind == "" {
		in.Kind = SessionChat
	}
	origin, err := json.Marshal(in.Origin)
	if err != nil {
		return Session{}, false, fmt.Errorf("store: ensure session: %w", err)
	}
	now := ms(in.CreatedAt)
	var out Session
	var created bool
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO sessions
			(id, key, agent, kind, user_id, origin, title, prefix_hash, summary_msg_id, compact_fails,
			 turn_token, run_generation, created_at, updated_at, ended_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
			ON CONFLICT (key) DO NOTHING`,
			string(in.ID), in.Key, in.Agent, string(in.Kind), nullID(in.UserID), string(origin), in.Title,
			in.PrefixHash, string(in.SummaryMsgID), in.CompactFails, in.TurnToken, in.RunGeneration, now, now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		created = n == 1
		row := tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE key = ?`, in.Key)
		out, err = scanSession(row)
		return err
	})
	if err != nil {
		return Session{}, false, fmt.Errorf("store: ensure session: %w", err)
	}
	return out, created, nil
}

// AdoptSession gives an ownerless session to owner: deleting a room identity
// leaves its sessions with no user and their keys unchanged, so the room's next
// identity finds them.
func (s *SQLite) AdoptSession(ctx context.Context, id, owner event.ID) (Session, error) {
	if _, err := s.exec(ctx, `UPDATE sessions SET user_id = ? WHERE id = ? AND user_id IS NULL`,
		string(owner), string(id)); err != nil {
		return Session{}, fmt.Errorf("store: adopt session: %w", err)
	}
	return s.GetSession(ctx, id)
}

func (s *SQLite) GetSession(ctx context.Context, id event.ID) (Session, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, string(id))
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: get session: %w", err)
	}
	return sess, nil
}

func (s *SQLite) ListSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+sessionColumns+` FROM sessions ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list sessions: %w", err)
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// UpdateSessionContext records the result of a context build or compaction.
func (s *SQLite) UpdateSessionContext(ctx context.Context, id event.ID, prefixHash string, summaryMsgID event.ID, compactFails int) error {
	res, err := s.exec(ctx, `UPDATE sessions
		SET prefix_hash = ?, summary_msg_id = ?, compact_fails = ?, updated_at = ?
		WHERE id = ?`,
		prefixHash, string(summaryMsgID), compactFails, time.Now().UnixMilli(), string(id))
	if err != nil {
		return fmt.Errorf("store: update session context: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// BeginTurn claims the session for one turn. A session that already holds a
// token is left alone (ErrTurnActive); HealTurns clears tokens left by a crash.
func (s *SQLite) BeginTurn(ctx context.Context, id event.ID) (string, int64, error) {
	token := string(event.NewID())
	var generation int64
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		var current string
		err := tx.QueryRowContext(ctx, `SELECT turn_token, run_generation FROM sessions WHERE id = ?`,
			string(id)).Scan(&current, &generation)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if current != "" {
			return ErrTurnActive
		}
		generation++
		_, err = tx.ExecContext(ctx,
			`UPDATE sessions SET turn_token = ?, run_generation = ?, updated_at = ? WHERE id = ?`,
			token, generation, time.Now().UnixMilli(), string(id))
		return err
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrTurnActive) {
			return "", 0, err
		}
		return "", 0, fmt.Errorf("store: begin turn: %w", err)
	}
	return token, generation, nil
}

// EndTurn clears the turn token only if it still matches.
func (s *SQLite) EndTurn(ctx context.Context, id event.ID, token string) error {
	var err error
	var n int64
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE sessions SET turn_token = '', updated_at = ? WHERE id = ? AND turn_token = ?`,
			time.Now().UnixMilli(), string(id), token)
		if err != nil {
			return err
		}
		if n, err = res.RowsAffected(); err != nil || n == 1 {
			return err
		}
		// Nothing updated: either the session is gone or another turn owns it.
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, string(id)).Scan(new(int)); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		return ErrTurnMismatch
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrTurnMismatch) {
			return err
		}
		return fmt.Errorf("store: end turn: %w", err)
	}
	return nil
}

// HealTurns clears turn tokens left behind by a crash and reports the sessions
// that held one (docs/sessions.md: self-heal on start).
func (s *SQLite) HealTurns(ctx context.Context) ([]event.ID, error) {
	var ids []event.ID
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM sessions WHERE turn_token <> '' ORDER BY id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, event.ID(id))
		}
		if err := rows.Err(); err != nil {
			return err
		}
		_ = rows.Close()
		if len(ids) == 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET turn_token = '', updated_at = ? WHERE turn_token <> ''`,
			time.Now().UnixMilli())
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: heal turns: %w", err)
	}
	return ids, nil
}

// DeleteSession removes the session with its messages and events, and drops
// blobs that no message references any more (docs/traceability.md).
func (s *SQLite) DeleteSession(ctx context.Context, id event.ID) error {
	var orphans []string
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, string(id)).Scan(new(int)); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		// Candidates: blobs attached to this session's messages.
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT mb.blob_id FROM message_blobs mb
			JOIN messages m ON m.id = mb.message_id WHERE m.session_id = ?`, string(id))
		if err != nil {
			return err
		}
		var candidates []string
		for rows.Next() {
			var b string
			if err := rows.Scan(&b); err != nil {
				_ = rows.Close()
				return err
			}
			candidates = append(candidates, b)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE session_id = ?`, string(id)); err != nil {
			return err
		}
		// messages and message_blobs cascade from the session row.
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, string(id)); err != nil {
			return err
		}
		for _, b := range candidates {
			var n int
			if err := tx.QueryRowContext(ctx,
				`SELECT count(*) FROM message_blobs WHERE blob_id = ?`, b).Scan(&n); err != nil {
				return err
			}
			if n > 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM blobs WHERE id = ?`, b); err != nil {
				return err
			}
			orphans = append(orphans, b)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return err
		}
		return fmt.Errorf("store: delete session: %w", err)
	}
	s.removeBlobFiles(orphans)
	return nil
}
