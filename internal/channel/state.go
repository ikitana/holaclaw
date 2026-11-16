package channel

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SQLState is the StateStore of one channel instance over the shared DB.
type SQLState struct {
	DB      *sql.DB
	Channel string
}

var _ StateStore = SQLState{}

func (s SQLState) Get(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM channel_state WHERE channel = ? AND key = ?`, s.Channel, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s SQLState) Set(ctx context.Context, key, value string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO channel_state (channel, key, value, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (channel, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		s.Channel, key, value, time.Now().UnixMilli())
	return err
}
