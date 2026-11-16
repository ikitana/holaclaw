package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"holaclaw/internal/event"
)

// withExtra returns the embedded migrations plus one synthetic step, as a
// later package's migration file would look.
func withExtra(t *testing.T, extra map[string]string) fs.FS {
	t.Helper()
	m := fstest.MapFS{}
	names, err := fs.Glob(MigrationsFS(), "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		b, err := fs.ReadFile(MigrationsFS(), n)
		if err != nil {
			t.Fatal(err)
		}
		m[n] = &fstest.MapFile{Data: b}
	}
	for n, sql := range extra {
		m[n] = &fstest.MapFile{Data: []byte(sql)}
	}
	return m
}

const extraSQL = `CREATE TABLE notes (id TEXT PRIMARY KEY, body TEXT NOT NULL) STRICT;`

func TestMigrateFresh(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if got := s.AppliedMigrations(); len(got) != len(realVersions(t)) {
		t.Fatalf("applied = %v, want %v", got, realVersions(t))
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != len(realVersions(t)) {
		t.Fatalf("schema_migrations count = %d, err %v", n, err)
	}
	// Re-opening applies nothing and writes no backup.
	s2, err := Open(ctx, s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	if got := s2.AppliedMigrations(); len(got) != 0 {
		t.Fatalf("second open applied %v, want none", got)
	}
	baks, _ := filepath.Glob(filepath.Join(s.Dir(), "holaclaw.db.bak-*"))
	if len(baks) != 0 {
		t.Fatalf("unexpected backups: %v", baks)
	}
}

// Processes opening a fresh database at once, as the daemon and `holaclaw
// init` do on a container's first start, apply each migration once between
// them, and every open succeeds.
func TestMigrateConcurrentOpens(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	const n = 8
	stores := make([]*SQLite, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stores[i], errs[i] = Open(ctx, dir)
		}()
	}
	wg.Wait()
	applied := map[int]int{}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("open %d: %v", i, errs[i])
		}
		defer func() { _ = stores[i].Close() }()
		for _, v := range stores[i].AppliedMigrations() {
			applied[v]++
		}
	}
	for _, v := range realVersions(t) {
		if applied[v] != 1 {
			t.Errorf("migration %d applied %d times, want once", v, applied[v])
		}
	}
	var rows int
	if err := stores[0].DB().QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&rows); err != nil || rows != len(realVersions(t)) {
		t.Fatalf("schema_migrations count = %d, err %v", rows, err)
	}
}

func TestMigrateUpgradeBacksUp(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateIdentity(ctx, Identity{Name: "ana", Role: RoleOwner}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	up, err := Open(ctx, dir, WithMigrations(withExtra(t, map[string]string{"9000_notes.sql": extraSQL})))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = up.Close() }()
	if got := up.AppliedMigrations(); len(got) != 1 || got[0] != 9000 {
		t.Fatalf("applied = %v, want [9000]", got)
	}
	if _, err := up.DB().Exec(`INSERT INTO notes (id, body) VALUES ('a', 'b')`); err != nil {
		t.Fatalf("new table missing: %v", err)
	}
	baks, err := filepath.Glob(filepath.Join(dir, fmt.Sprintf("holaclaw.db.bak-%d-*", latestReal(t))))
	if err != nil || len(baks) != 1 {
		t.Fatalf("backups = %v, err %v; want exactly one", baks, err)
	}
	// The backup is a complete database holding the pre-migration rows.
	bakDB, err := sql.Open("sqlite", dsn(baks[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bakDB.Close() }()
	var name string
	if err := bakDB.QueryRow(`SELECT name FROM identities`).Scan(&name); err != nil || name != "ana" {
		t.Fatalf("backup identity = %q, err %v", name, err)
	}
	if err := bakDB.QueryRow(`SELECT name FROM notes`).Scan(&name); err == nil {
		t.Fatal("backup should predate the 9000 migration")
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir, WithMigrations(withExtra(t, map[string]string{"9000_notes.sql": extraSQL})))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Open(ctx, dir) // binary knows only 0001
	var newer *ErrSchemaNewer
	if !errors.As(err, &newer) {
		t.Fatalf("err = %v, want *ErrSchemaNewer", err)
	}
	if newer.DBVersion != 9000 || newer.KnownLatest != latestReal(t) {
		t.Fatalf("got %+v", newer)
	}
}

func TestMigrateRefusesBackfilledVersion(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir, WithMigrations(withExtra(t, map[string]string{"9000_notes.sql": extraSQL})))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Pretend 0001 was never applied: a migration numbered below the applied
	// version may not run afterwards.
	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, "holaclaw.db")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Open(ctx, dir, WithMigrations(withExtra(t, map[string]string{"9000_notes.sql": extraSQL})))
	if err == nil || !strings.Contains(err.Error(), "forward-only") {
		t.Fatalf("err = %v, want a forward-only refusal", err)
	}
}

func TestLoadMigrations(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string // "" means success
	}{
		{"ok", map[string]string{"0001_a.sql": "SELECT 1;", "0010_b.sql": "SELECT 1;"}, ""},
		{"bad name", map[string]string{"1_a.sql": "SELECT 1;"}, "want NNNN_name.sql"},
		{"duplicate version", map[string]string{"0001_a.sql": "SELECT 1;", "0001_b.sql": "SELECT 1;"}, "share version"},
		{"zero version", map[string]string{"0000_a.sql": "SELECT 1;"}, "bad version"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := fstest.MapFS{}
			for n, body := range tc.files {
				m[n] = &fstest.MapFile{Data: []byte(body)}
			}
			got, err := LoadMigrations(m)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				if len(got) != len(tc.files) {
					t.Fatalf("got %d migrations, want %d", len(got), len(tc.files))
				}
				for i := 1; i < len(got); i++ {
					if got[i-1].Version >= got[i].Version {
						t.Fatalf("not sorted: %v", got)
					}
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want mention of %q", err, tc.want)
			}
		})
	}
}

func TestMigrateRunsWithoutStore(t *testing.T) {
	// Migrate is usable directly, so a later package can ship its own file.
	ctx := context.Background()
	s := newStore(t)
	applied, err := Migrate(ctx, s.DB(), withExtra(t, map[string]string{"9000_notes.sql": extraSQL}), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 || applied[0] != 9000 {
		t.Fatalf("applied = %v", applied)
	}
	if _, err := s.DB().Exec(`INSERT INTO notes (id, body) VALUES (?, ?)`, string(event.NewID()), "x"); err != nil {
		t.Fatal(err)
	}
}

// realVersions lists the versions of the embedded migrations, so these tests
// keep passing as later packages add files.
func realVersions(t *testing.T) []int {
	t.Helper()
	ms, err := LoadMigrations(MigrationsFS())
	if err != nil {
		t.Fatal(err)
	}
	out := make([]int, len(ms))
	for i, m := range ms {
		out[i] = m.Version
	}
	return out
}

func latestReal(t *testing.T) int {
	v := realVersions(t)
	return v[len(v)-1]
}
