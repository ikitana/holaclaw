package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"time"
)

var _ InboundStore = (*SQLite)(nil)

// ClaimInbound records the keys in one write transaction. INSERT OR IGNORE
// leaves a recorded key and its first time alone; any ignored row makes the
// message a duplicate.
func (s *SQLite) ClaimInbound(ctx context.Context, channel string, keys []string, at time.Time) (bool, error) {
	if len(keys) == 0 {
		return true, nil
	}
	fresh := true
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT OR IGNORE INTO inbound_seen (channel, key, seen_at) VALUES (?, ?, ?)`)
		if err != nil {
			return err
		}
		defer func() { _ = stmt.Close() }()
		for _, k := range keys {
			res, err := stmt.ExecContext(ctx, channel, inboundHash(k), ms(at))
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n == 0 {
				fresh = false
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("store: claim inbound: %w", err)
	}
	return fresh, nil
}

func (s *SQLite) ReleaseInbound(ctx context.Context, channel string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		for _, k := range keys {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM inbound_seen WHERE channel = ? AND key = ?`, channel, inboundHash(k)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: release inbound: %w", err)
	}
	return nil
}

func (s *SQLite) PruneInbound(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.exec(ctx, `DELETE FROM inbound_seen WHERE seen_at < ?`, before.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: prune inbound: %w", err)
	}
	return res.RowsAffected()
}

// inboundHash is the stored form of a dedupe key (table inbound_seen).
func inboundHash(key string) []byte {
	h := sha256.Sum256([]byte(key))
	return h[:]
}
