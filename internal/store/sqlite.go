package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"modernc.org/sqlite" // pure Go driver, registered as "sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// DefaultMaxBlobSize caps a single blob unless changed with WithMaxBlobSize.
const DefaultMaxBlobSize = 50 << 20

// SQLite is the store backed by one SQLite file plus a content-addressed blob
// directory under the same data directory.
type SQLite struct {
	db       *sql.DB
	dir      string // data directory
	blobDir  string // <dir>/blobs
	dbPath   string // <dir>/holaclaw.db
	maxBlob  int64
	log      *slog.Logger
	writeMu  sync.Mutex // see writeTx
	closeMu  sync.Mutex
	closed   bool
	migrated []int
}

var _ Store = (*SQLite)(nil)

type options struct {
	maxBlob    int64
	migrations fs.FS
	log        *slog.Logger
}

// Option configures Open.
type Option func(*options)

// WithMaxBlobSize sets the per-blob size cap (default DefaultMaxBlobSize).
func WithMaxBlobSize(n int64) Option {
	return func(o *options) { o.maxBlob = n }
}

// WithMigrations replaces the embedded migration set; the fs.FS holds
// NNNN_name.sql files at its root. Tests use it to inject extra migrations.
func WithMigrations(fsys fs.FS) Option {
	return func(o *options) { o.migrations = fsys }
}

// WithLogger sets the logger used for failures that are never returned
// (Emit, blob cleanup). Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.log = l }
}

// Open opens (creating it if needed) the store in dir: the SQLite database
// <dir>/holaclaw.db and the blob directory <dir>/blobs. Pending migrations are
// applied before returning (ARCHITECTURE.md §7: run at start, backup first).
func Open(ctx context.Context, dir string, opts ...Option) (*SQLite, error) {
	o := options{maxBlob: DefaultMaxBlobSize, migrations: MigrationsFS(), log: slog.Default()}
	for _, fn := range opts {
		fn(&o)
	}
	if dir == "" {
		return nil, errors.New("store: empty data directory")
	}
	blobDir := filepath.Join(dir, "blobs")
	if err := os.MkdirAll(filepath.Join(blobDir, tmpSubdir), 0o700); err != nil {
		return nil, fmt.Errorf("store: create blob dir: %w", err)
	}
	dbPath := filepath.Join(dir, "holaclaw.db")
	// The database holds conversations: create it private to the user, like
	// the data directory, instead of inheriting the umask.
	if f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0o600); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	} else {
		_ = f.Close()
	}
	db, err := sql.Open("sqlite", dsn(dbPath))
	if err != nil {
		return nil, fmt.Errorf("store: open db: %w", err)
	}
	// SQLite allows exactly one writer. Readers are cheap under WAL, so the
	// pool stays open for reads and every write goes through writeTx, which
	// holds writeMu; busy_timeout covers other processes (CLI, backups).
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := pingBusy(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open db: %w", err)
	}
	s := &SQLite{db: db, dir: dir, blobDir: blobDir, dbPath: dbPath, maxBlob: o.maxBlob, log: o.log}
	applied, err := Migrate(ctx, db, o.migrations, dbPath)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	s.migrated = applied
	return s, nil
}

// dsn builds the driver DSN. The pragmas are per connection, so they belong in
// the DSN rather than in a one-off statement.
// The path is escaped: in a URI DSN a "#" starts a fragment and "?" the query,
// so an unescaped data directory could silently open a different file.
func dsn(path string) string {
	return "file:" + (&url.URL{Path: path}).EscapedPath() +
		"?_txlock=immediate" + // write transactions take the lock up front, so busy_timeout applies instead of failing on a read→write upgrade
		// busy_timeout first: the pragmas run in order, and switching a new
		// database to WAL waits for another process opening it at once.
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_pragma=synchronous(NORMAL)"
}

// pingBusy opens the first connection. Switching a new database to WAL takes
// a lock SQLite's busy handler does not wait for, so a process opening it at
// the same moment as another (the daemon and `holaclaw init` on a fresh one)
// retries for as long as busy_timeout would wait.
func pingBusy(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := db.PingContext(ctx)
		var se *sqlite.Error
		if err == nil || !errors.As(err, &se) || se.Code()&0xff != sqlite3.SQLITE_BUSY || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// DB returns the shared database handle. Later packages (memory, approvals,
// tasks) use it instead of opening the file a second time; writes should go
// through their own transactions and tolerate SQLITE_BUSY.
func (s *SQLite) DB() *sql.DB { return s.db }

// Dir returns the data directory.
func (s *SQLite) Dir() string { return s.dir }

// AppliedMigrations returns the versions applied by the last Open, in order.
func (s *SQLite) AppliedMigrations() []int { return append([]int(nil), s.migrated...) }

// Close closes the database. It is safe to call twice.
func (s *SQLite) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// writeTx runs fn inside a write transaction. writeMu serialises writers in
// this process so concurrent callers queue instead of failing on SQLITE_BUSY.
func (s *SQLite) writeTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// exec runs one write statement under the writer lock.
func (s *SQLite) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.ExecContext(ctx, query, args...)
}

// ms converts a time to the unix milliseconds used by every timestamp column;
// the zero time means "now".
func ms(t time.Time) int64 {
	if t.IsZero() {
		t = time.Now()
	}
	return t.UnixMilli()
}

// fromMS converts a stored timestamp back to UTC time.
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }
