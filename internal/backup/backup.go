// Package backup writes and verifies `holaclaw backup` archives: a
// consistent snapshot of the database, the blob store and the config files,
// as one gzipped tar (ARCHITECTURE.md §4, docs/traceability.md). Secrets are excluded by
// default — a backup is meant to travel, and auth.toml must not.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ManifestVersion is the archive format version, bumped when the layout
// below changes in a way old backups do not have.
const ManifestVersion = 1

// Archive layout, relative to the tar root.
const (
	entryManifest = "MANIFEST.json"
	entryDB       = "data/holaclaw.db"
	dirBlobs      = "data/blobs/"
	dirConfig     = "config/"
)

// dbFileName mirrors internal/store's SQLite file name.
const dbFileName = "holaclaw.db"

// tmpPrefix starts the name of every file a backup stages, so a Manager can
// tell its leftovers from its archives.
const tmpPrefix = ".holaclaw-backup-"

// Manifest is the archive's MANIFEST.json: what a restore, or Verify, checks
// the extracted bytes against.
type Manifest struct {
	Version         int            `json:"version"`
	Created         time.Time      `json:"created"`
	SecretsIncluded bool           `json:"secrets_included"`
	Files           []ManifestFile `json:"files"`
}

// ManifestFile is one archived file's identity.
type ManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Options configures Create.
type Options struct {
	DataDir   string
	ConfigDir string
	// Out is a file path or a directory; a directory (or a path ending in a
	// separator) gets the default name
	// holaclaw-backup-<UTC timestamp>.tar.gz appended. Empty means the
	// default name in the current directory.
	Out string
	// IncludeSecrets copies auth.toml into the archive. Default: excluded.
	IncludeSecrets bool
	// SecretsPath is where auth.toml lives; it sits outside ConfigDir by
	// default. It is archived beside the config all the same.
	SecretsPath string
	// DB is an optional live handle. When set, the database is snapshotted
	// with VACUUM INTO — safe under concurrent writes. When nil, the file is
	// copied directly, which requires the daemon to be stopped (or at least
	// to have nothing in its WAL).
	DB *sql.DB
	// TmpDir is where the VACUUM INTO snapshot is staged; empty means the
	// system temp directory.
	TmpDir string
}

// ErrWALPresent is returned when the database file has a non-empty `-wal`
// sibling and no live DB handle was given: copying the main file alone would
// silently drop committed data still sitting in the WAL.
var ErrWALPresent = fmt.Errorf("backup: database has pending WAL data")

// Create writes one backup archive and returns its path.
func Create(ctx context.Context, opts Options) (string, error) {
	out, err := resolveOut(opts.Out)
	if err != nil {
		return "", err
	}

	tmpDB, cleanup, err := snapshotDB(ctx, opts)
	if err != nil {
		return "", err
	}
	defer cleanup()

	if err := writeArchive(ctx, out, opts, tmpDB); err != nil {
		return "", err
	}
	return out, nil
}

// resolveOut turns Options.Out into a concrete file path.
func resolveOut(out string) (string, error) {
	name := "holaclaw-backup-" + time.Now().UTC().Format("20060102T150405Z") + ".tar.gz"
	if out == "" {
		return name, nil
	}
	if strings.HasSuffix(out, string(filepath.Separator)) {
		return filepath.Join(out, name), nil
	}
	if info, err := os.Stat(out); err == nil && info.IsDir() {
		return filepath.Join(out, name), nil
	}
	return out, nil
}

// snapshotDB produces a consistent copy of the database at a temp path
// (empty if there is no database yet) and a cleanup func to remove it.
func snapshotDB(ctx context.Context, opts Options) (string, func(), error) {
	noop := func() {}
	if opts.DataDir == "" {
		return "", noop, nil
	}
	dbPath := filepath.Join(opts.DataDir, dbFileName)

	if opts.DB != nil {
		tmp, err := os.CreateTemp(opts.TmpDir, tmpPrefix+"db-*")
		if err != nil {
			return "", noop, fmt.Errorf("backup: %w", err)
		}
		tmpPath := tmp.Name()
		_ = tmp.Close()
		_ = os.Remove(tmpPath) // VACUUM INTO refuses to write over an existing file
		cleanup := func() { _ = os.Remove(tmpPath) }
		if _, err := opts.DB.ExecContext(ctx, "VACUUM INTO ?", tmpPath); err != nil {
			cleanup()
			return "", noop, fmt.Errorf("backup: VACUUM INTO: %w", err)
		}
		return tmpPath, cleanup, nil
	}

	if _, err := os.Stat(dbPath); err != nil {
		return "", noop, nil // no database yet: nothing to snapshot
	}
	if info, err := os.Stat(dbPath + "-wal"); err == nil && info.Size() > 0 {
		return "", noop, fmt.Errorf("%w at %s-wal: stop the daemon first, or back up through the control API "+
			"so a live DB handle can VACUUM INTO a consistent snapshot", ErrWALPresent, dbPath)
	}
	return dbPath, noop, nil
}

// writeArchive builds the tar.gz at out: a temp file in the same directory,
// fsynced, chmod 0600 and renamed into place, so a failed or interrupted
// backup never leaves a partial file where Out expects one. A cancelled ctx
// stops the write.
func writeArchive(ctx context.Context, out string, opts Options, dbPath string) error {
	dir := filepath.Dir(out)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	tmp, err := os.CreateTemp(dir, tmpPrefix+"*")
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	tmpName := tmp.Name()
	done := false
	defer func() {
		_ = tmp.Close()
		if !done {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("backup: %w", err)
	}

	if err := build(ctxWriter{ctx, tmp}, opts, dbPath); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if err := os.Rename(tmpName, out); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	done = true
	return os.Chmod(out, 0o600) // belt and suspenders: rename preserves the source's mode, but make it explicit
}

// ctxWriter fails a write once its context is done, which is what stops an
// archive halfway.
type ctxWriter struct {
	ctx context.Context
	w   io.Writer
}

func (c ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.w.Write(p)
}

// build writes the gzip+tar stream: every source file, then MANIFEST.json.
func build(w io.Writer, opts Options, dbPath string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	var files []ManifestFile
	add := func(name string, size int64, sha string) {
		files = append(files, ManifestFile{Path: name, Size: size, SHA256: sha})
	}

	if dbPath != "" {
		size, sum, err := addFile(tw, entryDB, dbPath)
		if err != nil {
			return err
		}
		add(entryDB, size, sum)
	}
	if opts.DataDir != "" {
		more, err := addTree(tw, filepath.Join(opts.DataDir, "blobs"), dirBlobs, skipTmpBlobDir)
		if err != nil {
			return err
		}
		files = append(files, more...)
	}
	if opts.ConfigDir != "" {
		more, err := addConfig(tw, opts.ConfigDir, opts.SecretsPath, opts.IncludeSecrets)
		if err != nil {
			return err
		}
		files = append(files, more...)
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	m := Manifest{Version: ManifestVersion, Created: time.Now().UTC(), SecretsIncluded: opts.IncludeSecrets, Files: files}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if err := writeEntry(tw, entryManifest, body); err != nil {
		return err
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return gz.Close()
}

// skipTmpBlobDir excludes the blob store's own scratch directory: partially
// written blobs that were never promoted into place.
func skipTmpBlobDir(rel string) bool { return rel == "tmp" || strings.HasPrefix(rel, "tmp/") }

// addConfig archives config.toml, channels.toml, connectors.toml, agents/**
// and, when asked, auth.toml — the fixed set of files ARCHITECTURE.md §4
// defines, never the backup timestamp directories `config migrate` leaves
// behind.
func addConfig(tw *tar.Writer, configDir, secretsPath string, includeSecrets bool) ([]ManifestFile, error) {
	var out []ManifestFile
	top := []string{"config.toml", "channels.toml", "connectors.toml"}
	if includeSecrets && secretsPath != "" {
		top = append(top, "auth.toml")
	}
	for _, name := range top {
		full := filepath.Join(configDir, name)
		if name == "auth.toml" {
			full = secretsPath
		}
		info, err := os.Lstat(full)
		if err != nil {
			continue // optional files: channels.toml, connectors.toml, auth.toml
		}
		if !info.Mode().IsRegular() {
			continue
		}
		size, sum, err := addFile(tw, dirConfig+name, full)
		if err != nil {
			return nil, err
		}
		out = append(out, ManifestFile{Path: dirConfig + name, Size: size, SHA256: sum})
	}
	more, err := addTree(tw, filepath.Join(configDir, "agents"), dirConfig+"agents/", nil)
	if err != nil {
		return nil, err
	}
	return append(out, more...), nil
}

// addTree archives every regular file under src, at dst+<relative path>,
// skipping symlinks entirely (never followed, in or out of the tree) and any
// relative path for which skip returns true.
func addTree(tw *tar.Writer, src, dst string, skip func(rel string) bool) ([]ManifestFile, error) {
	var out []ManifestFile
	info, err := os.Lstat(src)
	if err != nil {
		return nil, nil // optional tree: blobs/, agents/ may not exist yet
	}
	if !info.IsDir() {
		return nil, nil
	}
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == src {
			return nil
		}
		rel := filepath.ToSlash(mustRel(src, p))
		if skip != nil && skip(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil // never follow a symlink, in or out of the tree
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		name := dst + rel
		size, sum, err := addFile(tw, name, p)
		if err != nil {
			return err
		}
		out = append(out, ManifestFile{Path: name, Size: size, SHA256: sum})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func mustRel(base, target string) string {
	r, err := filepath.Rel(base, target)
	if err != nil {
		return target
	}
	return r
}

// addFile streams one file into the tar as name, hashing as it goes.
func addFile(tw *tar.Writer, name, path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("backup: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return 0, "", fmt.Errorf("backup: %w", err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Size: info.Size(), Mode: 0o600, ModTime: info.ModTime(),
		Typeflag: tar.TypeReg,
	}); err != nil {
		return 0, "", fmt.Errorf("backup: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tw, h), f)
	if err != nil {
		return 0, "", fmt.Errorf("backup: %w", err)
	}
	if n != info.Size() {
		return 0, "", fmt.Errorf("backup: %s changed size while being archived", path)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// writeEntry writes one small in-memory entry (MANIFEST.json).
func writeEntry(tw *tar.Writer, name string, body []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name: name, Size: int64(len(body)), Mode: 0o600, ModTime: time.Now(), Typeflag: tar.TypeReg,
	}); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return nil
}

// Verify re-reads an archive in one pass and checks every manifest hash
// against the actual bytes; it is the round-trip half of Create's guarantee.
func Verify(archivePath string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer func() { _ = f.Close() }()
	return verify(f, archivePath)
}

// verify is Verify on an open archive; archivePath only names it in errors.
func verify(r io.Reader, archivePath string) error {
	_, err := readArchive(r, archivePath, nil)
	return err
}

// maxManifest bounds MANIFEST.json: an archive is read from clients too.
const maxManifest = 64 << 20

// readArchive is the one pass every reader of an archive makes: it checks
// each file against the manifest and returns the manifest. keep, when set, is
// offered every file by its archive name and may return a writer to receive
// the bytes (nil skips the file); what it kept is only good if readArchive
// returns no error.
func readArchive(r io.Reader, archivePath string, keep func(name string) (io.WriteCloser, error)) (*Manifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)

	got := map[string]ManifestFile{}
	var manifest *Manifest
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Name == entryManifest {
			body, err := io.ReadAll(io.LimitReader(tr, maxManifest))
			if err != nil {
				return nil, fmt.Errorf("backup: %w", err)
			}
			var m Manifest
			if err := json.Unmarshal(body, &m); err != nil {
				return nil, fmt.Errorf("backup: %s: %w", entryManifest, err)
			}
			manifest = &m
			continue
		}
		h := sha256.New()
		var w io.Writer = h
		var kept io.WriteCloser
		if keep != nil {
			if kept, err = keep(hdr.Name); err != nil {
				return nil, err
			}
			if kept != nil {
				w = io.MultiWriter(h, kept)
			}
		}
		n, err := io.Copy(w, tr)
		if kept != nil {
			if cerr := kept.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			return nil, fmt.Errorf("backup: %w", err)
		}
		got[hdr.Name] = ManifestFile{Path: hdr.Name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}
	}
	if manifest == nil {
		return nil, fmt.Errorf("backup: %s has no %s", archivePath, entryManifest)
	}
	if manifest.Version > ManifestVersion {
		return nil, fmt.Errorf("backup: %s has manifest version %d, this holaclaw reads up to %d", archivePath, manifest.Version, ManifestVersion)
	}

	for _, want := range manifest.Files {
		g, ok := got[want.Path]
		if !ok {
			return nil, fmt.Errorf("backup: %s: %s is missing from the archive", archivePath, want.Path)
		}
		if g.Size != want.Size || g.SHA256 != want.SHA256 {
			return nil, fmt.Errorf("backup: %s: %s does not match its manifest hash", archivePath, want.Path)
		}
	}
	if len(manifest.Files) != len(got) {
		return nil, fmt.Errorf("backup: %s: archive has %d files, manifest lists %d", archivePath, len(got), len(manifest.Files))
	}
	return manifest, nil
}
