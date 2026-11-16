package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"holaclaw/internal/store"
)

// A restore replaces the state of an installation — the database and the
// blobs — with what an archive holds. It never touches the config files or
// auth.toml: those are the operator's, and the daemon does not write them
// (ARCHITECTURE.md §4).
//
// It happens in two steps, because the daemon cannot swap the database it
// has open. StageRestore, in the running daemon, checks the archive and
// unpacks its data into <data_dir>/restore. ApplyPendingRestore, at the next
// start and before the store opens, moves that into place. A staged restore
// is a directory that appears by one rename and disappears once applied, so
// a crash at any point leaves either nothing pending or a restore the next
// start finishes.

const (
	// restoreDirName is the staged restore inside the data directory.
	restoreDirName = "restore"
	// restoreTmpName is where one is unpacked before it counts.
	restoreTmpName = "restore.tmp"
	// restoreInfo names what was staged.
	restoreInfo = "restore.json"
	// previousDBSuffix is the database a restore replaced, kept beside the
	// new one until the next restore.
	previousDBSuffix = ".pre-restore"
)

// Restore is one staged or applied restore.
type Restore struct {
	// BackupID is the managed backup the data came from; for an offline
	// restore, the name of the archive file.
	BackupID  string    `json:"backup_id"`
	Requested time.Time `json:"requested"`
	// By is who asked, for the trace.
	By string `json:"by"`
}

// StageRestore checks a ready backup and stages its data for the next start,
// which the caller must then bring about: nothing changes until the daemon
// restarts. A damaged archive is ErrBadArchive, one written by a newer
// holaclaw ErrIncompatible; a backup running, or a restore already staged,
// ErrBusy.
func (m *Manager) StageRestore(id, by string) error {
	m.mu.Lock()
	rec, ok := m.recs[id]
	switch {
	case !ok:
		m.mu.Unlock()
		return ErrNotFound
	case rec.Status != StatusReady:
		m.mu.Unlock()
		return ErrNotReady
	case m.job != nil || m.restoring:
		m.mu.Unlock()
		return ErrBusy
	}
	f, err := os.Open(m.archive(id))
	if err != nil {
		m.mu.Unlock()
		return fmt.Errorf("backup: %w", err)
	}
	// From here no backup starts: it would snapshot data about to go.
	m.restoring = true
	m.mu.Unlock()
	defer func() { _ = f.Close() }()

	err = stageRestore(f, m.opts.DataDir, Restore{BackupID: id, Requested: time.Now().UTC(), By: by})
	if err != nil {
		m.mu.Lock()
		m.restoring = false
		m.mu.Unlock()
	}
	return err
}

// stageRestore unpacks the data of an archive into dataDir's restore
// directory, checking it against the manifest on the way.
func stageRestore(archive io.Reader, dataDir string, info Restore) error {
	tmp, err := unpackRestore(archive, dataDir, keepData)
	if err != nil {
		return err
	}
	return commitRestore(tmp, dataDir, info)
}

// unpackRestore writes what keep picks from an archive under dataDir's
// restore.tmp and returns that directory: checked against the manifest, with
// a database this binary can open. Nothing is staged yet; on an error the
// directory is gone.
func unpackRestore(archive io.Reader, dataDir string, keep func(dir string) func(name string) (io.WriteCloser, error)) (string, error) {
	if _, err := os.Lstat(filepath.Join(dataDir, restoreDirName)); err == nil {
		return "", ErrBusy
	}
	tmp := filepath.Join(dataDir, restoreTmpName)
	if err := os.RemoveAll(tmp); err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	unpacked := false
	defer func() {
		if !unpacked {
			_ = os.RemoveAll(tmp)
		}
	}()

	if _, err := readArchive(archive, "the archive", keep(tmp)); err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			return "", err // the disk, not the archive
		}
		return "", fmt.Errorf("%w: %w", ErrBadArchive, err)
	}
	db := filepath.Join(tmp, dbFileName)
	if _, err := os.Stat(db); err != nil {
		return "", fmt.Errorf("%w: it holds no database", ErrBadArchive)
	}
	if err := checkSchema(db); err != nil {
		return "", err
	}
	unpacked = true
	return tmp, nil
}

// commitRestore turns an unpacked directory into the staged restore, by one
// rename. On an error the directory is gone.
func commitRestore(tmp, dataDir string, info Restore) error {
	body, err := json.MarshalIndent(info, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(tmp, restoreInfo), body, 0o600)
	}
	if err == nil {
		err = os.Rename(tmp, filepath.Join(dataDir, restoreDirName))
	}
	if err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}

// keepData is the readArchive filter of a restore: the database and the
// blobs are written under dir, everything else (the config, auth.toml) is
// only checked. A blob name that would leave dir fails the archive.
func keepData(dir string) func(name string) (io.WriteCloser, error) {
	return func(name string) (io.WriteCloser, error) {
		var dst string
		switch {
		case name == entryDB:
			dst = filepath.Join(dir, dbFileName)
		case strings.HasPrefix(name, dirBlobs):
			rel := filepath.FromSlash(strings.TrimPrefix(name, dirBlobs))
			if !filepath.IsLocal(rel) {
				return nil, fmt.Errorf("backup: %s is not a path inside the blob store", name)
			}
			dst = filepath.Join(dir, "blobs", rel)
		default:
			return nil, nil
		}
		return createSynced(dst)
	}
}

// createSynced creates a file that must not exist yet, and its directories.
func createSynced(dst string) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return syncFile{f}, nil
}

// syncFile reaches the disk before it is closed: a staged restore is only
// committed by its rename, and must be whole by then.
type syncFile struct{ *os.File }

func (f syncFile) Close() error {
	err := f.Sync()
	if cerr := f.File.Close(); err == nil {
		err = cerr
	}
	return err
}

// checkSchema refuses a database this binary could not open: one that is not
// a holaclaw database, or that a newer holaclaw migrated further than this one
// knows (migrations are forward-only).
func checkSchema(path string) error {
	migrations, err := store.LoadMigrations(store.MigrationsFS())
	if err != nil {
		return err
	}
	latest := 0
	if n := len(migrations); n > 0 {
		latest = migrations[n-1].Version
	}
	// Read-only and immutable: looking at the staged file must not leave a
	// journal beside it.
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?mode=ro&immutable=1"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer func() { _ = db.Close() }()
	var version sql.NullInt64
	if err := db.QueryRowContext(context.Background(), `SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("%w: it does not hold a holaclaw database", ErrBadArchive)
	}
	if int(version.Int64) > latest {
		return fmt.Errorf("%w: its database is at schema version %d, this holaclaw knows up to %d",
			ErrIncompatible, version.Int64, latest)
	}
	return nil
}

// ApplyPendingRestore finishes a staged restore, if there is one: the blobs
// are added to the blob store, the database is replaced, and the one it
// replaced is kept as holaclaw.db.pre-restore. It must run while nothing has
// the store open. Every step can be repeated, so a start that died halfway
// is finished by the next; an error means the data directory needs a person,
// and the daemon must not open the store over it.
func ApplyPendingRestore(dataDir string) (Restore, bool, error) {
	_ = os.RemoveAll(filepath.Join(dataDir, restoreTmpName)) // never committed
	dir := filepath.Join(dataDir, restoreDirName)
	body, err := os.ReadFile(filepath.Join(dir, restoreInfo))
	if errors.Is(err, os.ErrNotExist) {
		if _, serr := os.Lstat(dir); serr == nil {
			return Restore{}, false, fmt.Errorf("backup: %s is not a staged restore: remove it", dir)
		}
		return Restore{}, false, nil
	}
	if err != nil {
		return Restore{}, false, fmt.Errorf("backup: %w", err)
	}
	var info Restore
	if err := json.Unmarshal(body, &info); err != nil {
		return Restore{}, false, fmt.Errorf("backup: %s: %w", filepath.Join(dir, restoreInfo), err)
	}
	fail := func(err error) (Restore, bool, error) {
		return info, false, fmt.Errorf("backup: restore of %s: %w", info.BackupID, err)
	}

	// Blobs are content-addressed: one already there is the same bytes.
	if err := mergeBlobs(filepath.Join(dir, "blobs"), filepath.Join(dataDir, "blobs")); err != nil {
		return fail(err)
	}

	db := filepath.Join(dataDir, dbFileName)
	staged := filepath.Join(dir, dbFileName)
	if _, err := os.Stat(staged); err == nil {
		if _, err := os.Stat(db); err == nil {
			// The WAL belongs to the database it sits beside: it moves with it.
			for _, ext := range []string{"", "-wal"} {
				err := os.Rename(db+ext, db+previousDBSuffix+ext)
				if ext != "" && errors.Is(err, os.ErrNotExist) {
					_ = os.Remove(db + previousDBSuffix + ext)
					continue
				}
				if err != nil {
					return fail(err)
				}
			}
		}
		// Nothing of the old database may be replayed into the new one.
		for _, ext := range []string{"-wal", "-shm"} {
			if err := os.Remove(db + ext); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fail(err)
			}
		}
		if err := os.Rename(staged, db); err != nil {
			return fail(err)
		}
	} else if _, err := os.Stat(db); err != nil {
		// Neither the staged database nor a live one: nothing sane to open.
		return fail(errors.New("the staged database is gone and there is none in place"))
	}
	if err := os.RemoveAll(dir); err != nil {
		return fail(err)
	}
	return info, true, nil
}

// mergeBlobs moves every staged blob that the store does not have yet.
func mergeBlobs(src, dst string) error {
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if _, err := os.Lstat(target); err == nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		return os.Rename(p, target)
	})
	if errors.Is(err, os.ErrNotExist) {
		return nil // an archive without blobs
	}
	return err
}
