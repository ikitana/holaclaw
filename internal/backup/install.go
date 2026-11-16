package backup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// An offline restore recreates a whole installation from an archive, with no
// daemon: a new host, or one whose daemon no longer starts. Unlike a restore
// through the daemon it writes the config files too, and auth.toml when the
// archive holds it — here the operator is the one doing it.
//
// It is three steps, so that everything can be shown before anything is
// written: Inspect checks the archive and returns its config.toml, which says
// where the data and the secrets go; PlanInstall lists what would be created
// and replaced; Install does it. Nothing is deleted: a replaced config file
// is moved into a `.backup-<ts>/` directory, as `config migrate` does, and a
// replaced auth.toml or database is kept beside the new one as
// `.pre-restore`. Files the archive does not hold are left alone.

// entryConfigTOML and entryAuth are the two config entries a restore treats
// apart: one says where the rest goes, the other goes to the secrets directory.
const (
	entryConfigTOML = dirConfig + "config.toml"
	entryAuth       = dirConfig + "auth.toml"
)

// Target is where an installation lives on disk.
type Target struct {
	ConfigDir string
	DataDir   string
	// SecretsPath is auth.toml; it sits outside ConfigDir by default.
	SecretsPath string
}

// Action is what an install does to one file.
type Action string

const (
	ActionCreate  Action = "create"
	ActionReplace Action = "replace"
	// ActionKeep is a file already there with the bytes the archive has.
	ActionKeep Action = "same"
)

// Change is one file of the plan: a config file, auth.toml or the database.
type Change struct {
	Path   string
	Action Action
	// Kept is where the file it replaces goes; empty unless Action is
	// ActionReplace.
	Kept string

	entry string // its name in the archive
}

// Plan is what Install would do, computed without writing anything.
type Plan struct {
	Target Target
	// Created is when the backup was taken.
	Created time.Time
	// Secrets says whether the archive holds auth.toml.
	Secrets bool
	// Dirs are the directories that do not exist yet.
	Dirs    []string
	Changes []Change
	// Blobs counts the blobs of the archive, NewBlobs the ones the blob store
	// does not have yet.
	Blobs, NewBlobs int
}

// Inspect checks an archive against its manifest and returns the manifest
// and the archive's config.toml, nil when it holds none.
func Inspect(archivePath string) (*Manifest, []byte, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, nil, fmt.Errorf("backup: %w", err)
	}
	defer func() { _ = f.Close() }()
	var cfg *bytes.Buffer
	m, err := readArchive(f, archivePath, func(name string) (io.WriteCloser, error) {
		if name != entryConfigTOML {
			return nil, nil
		}
		cfg = &bytes.Buffer{}
		return nopCloser{cfg}, nil
	})
	if err != nil {
		return nil, nil, err
	}
	if cfg == nil {
		return m, nil, nil
	}
	return m, cfg.Bytes(), nil
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

// configRel returns where a config entry goes inside the config directory.
// Only what Create archives is accepted — config.toml, channels.toml,
// connectors.toml, auth.toml and agents/** — so an archive cannot plant any
// other file.
func configRel(entry string) (string, error) {
	rel := strings.TrimPrefix(entry, dirConfig)
	ok := rel == "config.toml" || rel == "channels.toml" || rel == "connectors.toml" || rel == "auth.toml" ||
		strings.HasPrefix(rel, "agents/")
	if !ok || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return "", fmt.Errorf("%w: %s is not a file a backup holds", ErrBadArchive, entry)
	}
	return filepath.FromSlash(rel), nil
}

// PlanInstall lists what installing the archive of m into t would do. now
// names the directory replaced config files are kept in.
func PlanInstall(m *Manifest, t Target, now time.Time) (*Plan, error) {
	if _, err := os.Lstat(filepath.Join(t.DataDir, restoreDirName)); err == nil {
		return nil, fmt.Errorf("backup: a restore is already staged in %s: start the daemon to apply it, or remove it",
			filepath.Join(t.DataDir, restoreDirName))
	}
	p := &Plan{Target: t, Created: m.Created}
	kept := filepath.Join(t.ConfigDir, ".backup-"+now.UTC().Format("20060102T150405Z"))
	hasDB := false
	for _, f := range m.Files {
		switch {
		case f.Path == entryDB:
			hasDB = true
			c := Change{Path: filepath.Join(t.DataDir, dbFileName), Action: ActionCreate, entry: f.Path}
			if _, err := os.Lstat(c.Path); err == nil {
				c.Action, c.Kept = ActionReplace, c.Path+previousDBSuffix
			}
			p.Changes = append(p.Changes, c)
		case strings.HasPrefix(f.Path, dirBlobs):
			rel := filepath.FromSlash(strings.TrimPrefix(f.Path, dirBlobs))
			if !filepath.IsLocal(rel) {
				return nil, fmt.Errorf("%w: %s is not a path inside the blob store", ErrBadArchive, f.Path)
			}
			p.Blobs++
			if _, err := os.Lstat(filepath.Join(t.DataDir, "blobs", rel)); err != nil {
				p.NewBlobs++
			}
		case strings.HasPrefix(f.Path, dirConfig):
			rel, err := configRel(f.Path)
			if err != nil {
				return nil, err
			}
			c := Change{Path: filepath.Join(t.ConfigDir, rel), Kept: filepath.Join(kept, rel), entry: f.Path}
			if f.Path == entryAuth {
				p.Secrets = true
				c.Path, c.Kept = t.SecretsPath, t.SecretsPath+previousDBSuffix
			}
			var err2 error
			if c.Action, err2 = actionFor(c.Path, f); err2 != nil {
				return nil, err2
			}
			if c.Action != ActionReplace {
				c.Kept = ""
			}
			p.Changes = append(p.Changes, c)
		default:
			return nil, fmt.Errorf("%w: %s is not a file a backup holds", ErrBadArchive, f.Path)
		}
	}
	if !hasDB {
		return nil, fmt.Errorf("%w: it holds no database", ErrBadArchive)
	}
	dirs := []string{t.ConfigDir, t.DataDir}
	if p.Secrets {
		dirs = append(dirs, filepath.Dir(t.SecretsPath))
	}
	for _, d := range dirs {
		if _, err := os.Stat(d); errors.Is(err, fs.ErrNotExist) {
			p.Dirs = append(p.Dirs, d)
		}
	}
	return p, nil
}

// actionFor compares the file at path with the one the archive has.
func actionFor(path string, want ManifestFile) (Action, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return ActionCreate, nil
	}
	if err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", fmt.Errorf("backup: %w", err)
	}
	if n == want.Size && hex.EncodeToString(h.Sum(nil)) == want.SHA256 {
		return ActionKeep, nil
	}
	return ActionReplace, nil
}

// Install carries out a plan made for this archive. It must run while no
// daemon has the store open. The archive is unpacked whole and checked before
// anything changes; then the config files go in, then the data, the way a
// daemon applies a staged restore. A failure halfway leaves what was replaced
// where the plan said it is kept, and running the restore again finishes it.
func Install(archivePath string, p *Plan) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer func() { _ = f.Close() }()
	dataDir := p.Target.DataDir
	tmp, err := unpackRestore(f, dataDir, keepAll)
	if err != nil {
		return err
	}
	staged := filepath.Join(tmp, "config")
	if err := installConfig(staged, p); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	// The staged restore is the data alone, as the daemon stages it.
	if err := os.RemoveAll(staged); err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("backup: %w", err)
	}
	info := Restore{BackupID: filepath.Base(archivePath), Requested: time.Now().UTC(), By: "cli"}
	if err := commitRestore(tmp, dataDir, info); err != nil {
		return err
	}
	_, _, err = ApplyPendingRestore(dataDir)
	return err
}

// keepAll is keepData plus the config files, which are written under
// dir/config by the name they have in the archive.
func keepAll(dir string) func(name string) (io.WriteCloser, error) {
	data := keepData(dir)
	return func(name string) (io.WriteCloser, error) {
		if !strings.HasPrefix(name, dirConfig) {
			return data(name)
		}
		rel, err := configRel(name)
		if err != nil {
			return nil, err
		}
		return createSynced(filepath.Join(dir, "config", rel))
	}
}

// installConfig puts the staged config files in place, moving aside the
// ones they replace.
func installConfig(staged string, p *Plan) error {
	for _, c := range p.Changes {
		if !strings.HasPrefix(c.entry, dirConfig) || c.Action == ActionKeep {
			continue
		}
		if c.Action == ActionReplace {
			if err := os.MkdirAll(filepath.Dir(c.Kept), 0o700); err != nil {
				return fmt.Errorf("backup: %w", err)
			}
			if err := os.Rename(c.Path, c.Kept); err != nil {
				return fmt.Errorf("backup: %w", err)
			}
		}
		src := filepath.Join(staged, filepath.FromSlash(strings.TrimPrefix(c.entry, dirConfig)))
		if err := copyInto(src, c.Path); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	return nil
}

// copyInto writes src at dst, mode 0600, through a temp file beside dst: the
// staged file may be on another filesystem, and dst must never be half a file.
func copyInto(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dst), tmpPrefix+"*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // a no-op once renamed
	_, err = io.Copy(tmp, in)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
