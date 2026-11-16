package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// migrated makes a data directory at the binary's schema, with one
// connector row, and removes the blob directory Open made.
func migrated(t *testing.T, opts ...Option) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutConnector(ctx, Connector{ID: "wiki", State: ConnectorConnected}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "blobs")); err != nil {
		t.Fatal(err)
	}
	return dir
}

// snapshot is every entry under dir with its size, mtime and content hash.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		sum := ""
		if !d.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h := sha256.Sum256(b)
			sum = hex.EncodeToString(h[:])
		}
		lines = append(lines, fmt.Sprintf("%s %d %d %s", p, info.Size(), info.ModTime().UnixNano(), sum))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func TestOpenReadOnlyReadsAndNeverWrites(t *testing.T) {
	ctx := context.Background()
	dir := migrated(t)
	before := snapshot(t, dir)
	s, err := OpenReadOnly(ctx, dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	rows, err := s.ListConnectors(ctx)
	if err != nil || len(rows) != 1 || rows[0].ID != "wiki" {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if err := s.PutConnector(ctx, Connector{ID: "other"}); err == nil {
		t.Fatal("a write through a read-only store succeeded")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, dir); after != before {
		t.Fatalf("the data directory changed:\nbefore\n%s\nafter\n%s", before, after)
	}
	if _, err := os.Stat(filepath.Join(dir, "blobs")); !os.IsNotExist(err) {
		t.Fatalf("blobs/ created: %v", err)
	}
}

// With a writer open (a running daemon) the reader sees its commits and
// leaves the main file alone.
func TestOpenReadOnlyBesideAWriter(t *testing.T) {
	ctx := context.Background()
	dir := migrated(t)
	w, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err := w.PutConnector(ctx, Connector{ID: "live", State: ConnectorConnected}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(dir, "holaclaw.db"))
	s, err := OpenReadOnly(ctx, dir)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if _, err := s.GetConnector(ctx, "live"); err != nil {
		t.Fatalf("the writer's row is not visible: %v", err)
	}
	_ = s.Close()
	after, _ := os.Stat(filepath.Join(dir, "holaclaw.db"))
	if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		t.Fatal("the reader wrote the main file")
	}
}

// A -wal and -shm left by a writer that was never closed (a crash, or a
// copy of a live daemon's files) are read like a live writer's, and the
// directory is left as it was.
func TestReadConnectorsWALWithoutWriter(t *testing.T) {
	ctx := context.Background()
	src := migrated(t)
	w, err := Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.PutConnector(ctx, Connector{ID: "live", State: ConnectorConnected}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, name := range []string{"holaclaw.db", "holaclaw.db-wal", "holaclaw.db-shm"} {
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.Close()
	names := func() string {
		entries, _ := os.ReadDir(dir)
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return strings.Join(out, ",")
	}
	stat := func(name string) string {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(info.Size(), info.ModTime().UnixNano())
	}
	entries, db, wal := names(), stat("holaclaw.db"), stat("holaclaw.db-wal")
	rows, err := ReadConnectors(ctx, dir)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if names() != entries || stat("holaclaw.db") != db || stat("holaclaw.db-wal") != wal {
		t.Fatalf("the directory changed: %s", names())
	}
}

// A failed immutable read is retried once without immutable; an error a
// retry would repeat is not.
func TestReadOnlyRetry(t *testing.T) {
	ctx := context.Background()
	dir := migrated(t)
	calls := 0
	n, err := readOnly(ctx, dir, func(s *SQLite) (int, error) {
		calls++
		if calls == 1 {
			return 0, errors.New("torn read")
		}
		rows, err := s.ListConnectors(ctx)
		_ = s.Close()
		return len(rows), err
	})
	if err != nil || n != 1 || calls != 2 {
		t.Fatalf("n = %d, err = %v, calls = %d", n, err, calls)
	}
	calls = 0
	_, err = readOnly(ctx, dir, func(*SQLite) (int, error) {
		calls++
		return 0, fmt.Errorf("x: %w", os.ErrNotExist)
	})
	if err == nil || calls != 1 {
		t.Fatalf("final error retried: calls = %d, err = %v", calls, err)
	}
}

func TestOpenReadOnlyMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := OpenReadOnly(context.Background(), dir)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want not exist", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("created %v", entries)
	}
	if _, err := OpenReadOnly(context.Background(), ""); err == nil {
		t.Fatal("empty dir accepted")
	}
}

func TestOpenReadOnlyVersionMismatch(t *testing.T) {
	all, err := fs.Glob(MigrationsFS(), "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	newer := fstest.MapFS{}
	for _, n := range all {
		b, _ := fs.ReadFile(MigrationsFS(), n)
		newer[n] = &fstest.MapFile{Data: b}
	}
	newer["9999_future.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE future (x INTEGER) STRICT;")}

	dir := migrated(t, WithMigrations(newer))
	var sn *ErrSchemaNewer
	if _, err := OpenReadOnly(context.Background(), dir); !errors.As(err, &sn) {
		t.Fatalf("newer schema: err = %v", err)
	}

	// An older database: the binary's newest migration is not recorded.
	dir = migrated(t)
	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, "holaclaw.db")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = (SELECT max(version) FROM schema_migrations)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	if _, err := OpenReadOnly(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("older schema: err = %v", err)
	}
	if snapshot(t, dir) != before {
		t.Fatal("an older database was touched")
	}
}
