package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// MigrationsFS returns the migration set compiled into the binary. A new
// migration is added by dropping a NNNN_name.sql file into the migrations
// directory; nothing else has to change.
func MigrationsFS() fs.FS {
	sub, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		panic(err) // the embed pattern guarantees the directory exists
	}
	return sub
}

// Migration is one numbered forward-only schema step.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

var migrationName = regexp.MustCompile(`^(\d{4})_([A-Za-z0-9_-]+)\.sql$`)

// LoadMigrations reads and sorts the NNNN_name.sql files at the root of fsys.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	out := make([]Migration, 0, len(names))
	seen := map[int]string{}
	for _, n := range names {
		m := migrationName.FindStringSubmatch(n)
		if m == nil {
			return nil, fmt.Errorf("store: migration %q: want NNNN_name.sql", n)
		}
		v, err := strconv.Atoi(m[1])
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("store: migration %q: bad version", n)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("store: migrations %q and %q share version %d", prev, n, v)
		}
		seen[v] = n
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, fmt.Errorf("store: read migration %q: %w", n, err)
		}
		out = append(out, Migration{Version: v, Name: m[2], SQL: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

const createSchemaMigrations = `CREATE TABLE IF NOT EXISTS schema_migrations (
  version     INTEGER PRIMARY KEY,
  name        TEXT NOT NULL,
  applied_at  INTEGER NOT NULL
) STRICT`

// Migrate applies every pending migration from fsys, each in its own
// transaction, recording it in schema_migrations. It is forward-only: a
// database newer than the binary is refused (*ErrSchemaNewer), and so is a
// migration numbered below the current version that was never applied. Before
// touching an existing non-empty database it writes a backup copy next to
// dbPath (VACUUM INTO). It returns the versions applied, oldest first.
func Migrate(ctx context.Context, db *sql.DB, fsys fs.FS, dbPath string) ([]int, error) {
	migrations, err := LoadMigrations(fsys)
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, createSchemaMigrations); err != nil {
		return nil, fmt.Errorf("store: schema_migrations: %w", err)
	}
	applied, current, err := appliedVersions(ctx, db)
	if err != nil {
		return nil, err
	}
	latest := 0
	if n := len(migrations); n > 0 {
		latest = migrations[n-1].Version
	}
	if current > latest {
		return nil, &ErrSchemaNewer{DBVersion: current, KnownLatest: latest}
	}

	var pending []Migration
	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		if m.Version < current {
			return nil, fmt.Errorf("store: migration %04d_%s is below the applied version %d (forward-only)", m.Version, m.Name, current)
		}
		pending = append(pending, m)
	}
	if len(pending) == 0 {
		return nil, nil
	}
	// An existing database (at least one migration applied) is backed up once
	// before the batch; a fresh file has nothing to lose.
	if current > 0 && dbPath != "" {
		if err := backup(ctx, db, dbPath, current); err != nil {
			return nil, err
		}
	}
	done := make([]int, 0, len(pending))
	for _, m := range pending {
		ran, err := applyMigration(ctx, db, m)
		if err != nil {
			return done, err
		}
		if ran {
			done = append(done, m.Version)
		}
	}
	return done, nil
}

func appliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, int, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, 0, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	applied, current := map[int]bool{}, 0
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, 0, err
		}
		applied[v] = true
		if v > current {
			current = v
		}
	}
	return applied, current, rows.Err()
}

// applyMigration applies m and reports whether it ran. Another process opening
// the same database (the daemon and `holaclaw init` on a fresh one) may have
// applied it since appliedVersions: the transaction takes the write lock up
// front (_txlock=immediate in the DSN), so checking inside it is atomic.
func applyMigration(ctx context.Context, db *sql.DB, m Migration) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version = ?`, m.Version).Scan(&n); err != nil {
		return false, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	if n > 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return false, fmt.Errorf("store: migration %04d_%s: %w", m.Version, m.Name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		m.Version, m.Name, time.Now().UnixMilli()); err != nil {
		return false, fmt.Errorf("store: record migration %04d_%s: %w", m.Version, m.Name, err)
	}
	return true, tx.Commit()
}

// backup writes holaclaw.db.bak-<version>-<timestamp> next to the database,
// private like it. Another process opening the database may back it up in
// the same second: the file is created exclusively, with .1, .2… after the
// name when it is taken, and VACUUM INTO fills it, since it takes an empty
// file but never one with content.
func backup(ctx context.Context, db *sql.DB, dbPath string, version int) error {
	base := fmt.Sprintf("%s.bak-%d-%s", dbPath, version, time.Now().UTC().Format("20060102T150405Z"))
	dst := base
	for i := 1; ; i++ {
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("store: backup before migrating: %w", err)
		}
		dst = fmt.Sprintf("%s.%d", base, i)
	}
	// VACUUM INTO takes no parameters, so the path is quoted by hand.
	if _, err := db.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(dst, "'", "''")+`'`); err != nil {
		return fmt.Errorf("store: backup before migrating: %w", err)
	}
	return nil
}
