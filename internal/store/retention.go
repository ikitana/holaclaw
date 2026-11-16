package store

import (
	"context"
	"time"
)

// PruneEvents deletes trace events older than the cutoff and reports how many
// went. Messages are never pruned here: they are the conversation, and they
// go with their session (docs/traceability.md: retention is configuration).
func (s *SQLite) PruneEvents(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.exec(ctx, `DELETE FROM events WHERE time < ?`, before.UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
