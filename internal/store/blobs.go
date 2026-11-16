package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// tmpSubdir holds partially written blobs; it lives inside the blob directory
// so the final rename never crosses a filesystem.
const tmpSubdir = "tmp"

// PutBlob stores the bytes of r content-addressed by sha256 at
// blobs/ab/cd/<hex>. The reader is streamed through the hash into a temp file
// that is fsynced and renamed into place; an already known id is deduped and
// the temp file dropped. More than the size cap yields ErrBlobTooLarge.
func (s *SQLite) PutBlob(ctx context.Context, r io.Reader, mime string) (Blob, error) {
	if err := os.MkdirAll(filepath.Join(s.blobDir, tmpSubdir), 0o700); err != nil {
		return Blob{}, fmt.Errorf("store: put blob: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Join(s.blobDir, tmpSubdir), "blob-*")
	if err != nil {
		return Blob{}, fmt.Errorf("store: put blob: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName) // no-op once renamed
	}()

	h := sha256.New()
	// One extra byte tells an exactly-at-cap blob from an oversized one.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(r, s.maxBlob+1))
	if err != nil {
		return Blob{}, fmt.Errorf("store: put blob: %w", err)
	}
	if n > s.maxBlob {
		return Blob{}, fmt.Errorf("%w: cap %d bytes", ErrBlobTooLarge, s.maxBlob)
	}
	if err := tmp.Sync(); err != nil {
		return Blob{}, fmt.Errorf("store: put blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Blob{}, fmt.Errorf("store: put blob: %w", err)
	}

	id := hex.EncodeToString(h.Sum(nil))
	b := Blob{ID: id, Size: n, MIME: mime, CreatedAt: time.Now().UTC()}

	if existing, err := s.statBlob(ctx, id); err == nil {
		return existing, nil // dedupe: same content, keep the first metadata
	} else if !errors.Is(err, ErrNotFound) {
		return Blob{}, err
	}

	path := s.blobPath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Blob{}, fmt.Errorf("store: put blob: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return Blob{}, fmt.Errorf("store: put blob: %w", err)
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return Blob{}, fmt.Errorf("store: put blob: %w", err)
	}
	if _, err := s.exec(ctx, `INSERT INTO blobs (id, size, mime, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`, b.ID, b.Size, b.MIME, b.CreatedAt.UnixMilli()); err != nil {
		return Blob{}, fmt.Errorf("store: put blob: %w", err)
	}
	return b, nil
}

// OpenBlob returns the bytes and metadata of a blob.
func (s *SQLite) OpenBlob(ctx context.Context, id string) (io.ReadCloser, Blob, error) {
	b, err := s.statBlob(ctx, id)
	if err != nil {
		return nil, Blob{}, err
	}
	f, err := os.Open(s.blobPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Blob{}, ErrNotFound
		}
		return nil, Blob{}, fmt.Errorf("store: open blob: %w", err)
	}
	return f, b, nil
}

func (s *SQLite) statBlob(ctx context.Context, id string) (Blob, error) {
	var b Blob
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id, size, mime, created_at FROM blobs WHERE id = ?`, id).
		Scan(&b.ID, &b.Size, &b.MIME, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Blob{}, ErrNotFound
	}
	if err != nil {
		return Blob{}, fmt.Errorf("store: blob metadata: %w", err)
	}
	b.CreatedAt = fromMS(created)
	return b, nil
}

// blobPath is the sharded path of a blob: blobs/ab/cd/<hex>.
func (s *SQLite) blobPath(id string) string {
	if len(id) < 4 {
		return filepath.Join(s.blobDir, id)
	}
	return filepath.Join(s.blobDir, id[0:2], id[2:4], id)
}

// removeBlobFiles deletes blob files whose rows are gone; a failure only
// leaves garbage behind, so it is logged rather than returned.
func (s *SQLite) removeBlobFiles(ids []string) {
	for _, id := range ids {
		if err := os.Remove(s.blobPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.log.Error("store: remove blob file", "blob_id", id, "err", err)
		}
	}
}

// syncDir fsyncs a directory so a rename survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
