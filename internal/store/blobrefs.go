package store

import (
	"context"
	"fmt"

	"holaclaw/internal/event"
)

// BlobSessions returns the sessions whose transcript references a blob, in id
// order and without duplicates. It is the authorization side of GET
// /v1/blobs/{id}: a blob is only readable by a caller who may read a session
// that carries it (docs/security.md). A blob nothing references yet — a fresh
// upload — yields an empty list, never an error.
func (s *SQLite) BlobSessions(ctx context.Context, blobID string) ([]event.ID, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT m.session_id FROM message_blobs mb
		 JOIN messages m ON m.id = mb.message_id
		 WHERE mb.blob_id = ? ORDER BY m.session_id`, blobID)
	if err != nil {
		return nil, fmt.Errorf("store: blob sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []event.ID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: blob sessions: %w", err)
		}
		out = append(out, event.ID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: blob sessions: %w", err)
	}
	return out, nil
}
