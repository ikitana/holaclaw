package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
)

// errSchemaOlder is a database the daemon has not migrated to this binary's
// schema yet.
var errSchemaOlder = errors.New("store: database schema is older than this binary's: start the daemon to migrate it")

// OpenReadOnly opens the existing database <dir>/holaclaw.db for reading,
// for offline commands that must leave the data directory as they found it:
// no file or blob directory is created, nothing is migrated or backed up,
// and every connection is read-only. A missing database is an error that
// matches os.ErrNotExist; a schema version other than this binary's is an
// error too, since the queries are written for exactly that schema.
//
// Only the open is retried (see readOnly): a later read through the handle
// is not. ReadConnectors covers its read too.
func OpenReadOnly(ctx context.Context, dir string) (*SQLite, error) {
	return readOnly(ctx, dir, func(s *SQLite) (*SQLite, error) { return s, nil })
}

// ReadConnectors is ListConnectors over a read-only open, the read retried
// with the open.
func ReadConnectors(ctx context.Context, dir string) ([]Connector, error) {
	return readOnly(ctx, dir, func(s *SQLite) ([]Connector, error) {
		defer func() { _ = s.Close() }()
		return s.ListConnectors(ctx)
	})
}

// readOnly opens the database and runs fn on it, closing it when fn fails.
//
// The database is in WAL mode, and a read-only connection to it creates
// -wal and -shm when they are missing, and cannot remove them after. With
// no -wal file every commit is in the main file (the last connection to
// close checkpoints and removes it), so it is opened as immutable: no lock,
// no side file. With one, a daemon may be writing: the files are there
// already, and the reader uses them like any other connection.
//
// An idle daemon removes its -wal when its pool drains and writes again
// later, so an immutable read can overlap a write. A read that fails is
// retried once without immutable, which may leave -wal and -shm behind
// (the daemon uses them anyway). A read that overlaps a write and does not
// fail can still see stale rows: acceptable for the offline estimates this
// is for, not for anything that writes back.
func readOnly[T any](ctx context.Context, dir string, fn func(*SQLite) (T, error)) (T, error) {
	var zero T
	if dir == "" {
		return zero, errors.New("store: empty data directory")
	}
	dbPath := filepath.Join(dir, "holaclaw.db")
	if _, err := os.Stat(dbPath); err != nil {
		return zero, fmt.Errorf("store: %w", err)
	}
	_, walErr := os.Stat(dbPath + "-wal")
	immutable := os.IsNotExist(walErr)
	for {
		v, err := readOnce(ctx, dir, dbPath, immutable, fn)
		if err == nil || !immutable || final(ctx, err) {
			return v, err
		}
		immutable = false
	}
}

func readOnce[T any](ctx context.Context, dir, dbPath string, immutable bool, fn func(*SQLite) (T, error)) (T, error) {
	var zero T
	db, err := sql.Open("sqlite", readOnlyDSN(dbPath, immutable))
	if err != nil {
		return zero, fmt.Errorf("store: open db: %w", err)
	}
	if err := checkVersion(ctx, db); err != nil {
		_ = db.Close()
		return zero, err
	}
	s := &SQLite{db: db, dir: dir, blobDir: filepath.Join(dir, "blobs"), dbPath: dbPath,
		maxBlob: DefaultMaxBlobSize, log: slog.Default()}
	v, err := fn(s)
	if err != nil {
		_ = s.Close()
		return zero, err
	}
	return v, nil
}

// final is an error a second open would only repeat.
func final(ctx context.Context, err error) bool {
	var newer *ErrSchemaNewer
	return ctx.Err() != nil || errors.Is(err, os.ErrNotExist) || errors.Is(err, errSchemaOlder) || errors.As(err, &newer)
}

// readOnlyDSN is dsn's read-only twin: mode=ro at open, query_only on
// every connection, and no pragma that writes (journal_mode).
func readOnlyDSN(path string, immutable bool) string {
	s := "file:" + (&url.URL{Path: path}).EscapedPath() + "?mode=ro"
	if immutable {
		s += "&immutable=1"
	}
	return s + "&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
}

// checkVersion compares the database's newest applied migration with the
// newest one this binary carries.
func checkVersion(ctx context.Context, db *sql.DB) error {
	migrations, err := LoadMigrations(MigrationsFS())
	if err != nil {
		return err
	}
	latest := 0
	if n := len(migrations); n > 0 {
		latest = migrations[n-1].Version
	}
	_, current, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}
	switch {
	case current > latest:
		return &ErrSchemaNewer{DBVersion: current, KnownLatest: latest}
	case current < latest:
		return fmt.Errorf("%w (%d < %d)", errSchemaOlder, current, latest)
	}
	return nil
}
