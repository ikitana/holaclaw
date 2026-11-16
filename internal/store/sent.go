package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"holaclaw/internal/event"
)

var _ SentStore = (*SQLite)(nil)

func (s *SQLite) PutSent(ctx context.Context, rows []Sent) error {
	if len(rows) == 0 {
		return nil
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		for _, r := range rows {
			if r.Channel == "" || r.PlatformMsgID == "" || r.SessionID == "" {
				return errors.New("store: put sent: channel, platform id and session are required")
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO sent_messages
				(channel, conversation, platform_msg_id, session_id, message_id, approval_id, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)`,
				r.Channel, r.Conversation, r.PlatformMsgID, string(r.SessionID), string(r.MessageID),
				string(r.ApprovalID), ms(r.CreatedAt)); err != nil {
				return fmt.Errorf("store: put sent: %w", err)
			}
		}
		return nil
	})
}

func (s *SQLite) GetSent(ctx context.Context, channel, platformMsgID string) (Sent, error) {
	r := Sent{Channel: channel, PlatformMsgID: platformMsgID}
	var sess, msg, appr string
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT conversation, session_id, message_id, approval_id, created_at
		FROM sent_messages WHERE channel = ? AND platform_msg_id = ?`,
		channel, platformMsgID).Scan(&r.Conversation, &sess, &msg, &appr, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Sent{}, ErrNotFound
	}
	if err != nil {
		return Sent{}, fmt.Errorf("store: get sent: %w", err)
	}
	r.SessionID, r.MessageID, r.ApprovalID, r.CreatedAt = event.ID(sess), event.ID(msg), event.ID(appr), fromMS(at)
	return r, nil
}

func (s *SQLite) PruneSent(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.exec(ctx, `DELETE FROM sent_messages WHERE created_at < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
