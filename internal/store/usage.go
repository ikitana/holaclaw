package store

import (
	"context"
	"time"
)

const usageDay = "2006-01-02"

// TakeDaily counts one use of key for agent on the UTC day of at, unless that
// would exceed limit. It reports whether the use was counted. The check and
// the increment are one statement, so concurrent callers never overshoot.
func (s *SQLite) TakeDaily(ctx context.Context, agent, key string, at time.Time, limit int) (bool, error) {
	if limit < 1 {
		return false, nil
	}
	res, err := s.exec(ctx, `INSERT INTO usage_daily (agent, key, day, n) VALUES (?, ?, ?, 1)
		ON CONFLICT (agent, key, day) DO UPDATE SET n = n + 1 WHERE n < ?`,
		agent, key, at.UTC().Format(usageDay), limit)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReturnDaily gives back one use taken with TakeDaily, for an operation that
// failed before it cost anything.
func (s *SQLite) ReturnDaily(ctx context.Context, agent, key string, at time.Time) error {
	_, err := s.exec(ctx, `UPDATE usage_daily SET n = n - 1 WHERE agent = ? AND key = ? AND day = ? AND n > 0`,
		agent, key, at.UTC().Format(usageDay))
	return err
}
