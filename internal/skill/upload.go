package skill

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/pelletier/go-toml/v2"

	"holaclaw/internal/fsutil"
)

// An upload is a local skill folder the CLI read and sent as a gzipped tar
// (docs/skills.md §7): the daemon never reads a path on its own host from
// the API. It lives under <tmpRoot>/skill-upload-<ulid>/, the one folder of
// the archive next to uploadMeta, until an install from it succeeds or it
// is an hour old.

// UploadTTL is how long an upload waits for its install.
const UploadTTL = time.Hour

const (
	uploadPrefix = tmpPrefix + "upload-"
	uploadMeta   = ".meta.toml"
)

// meta is what uploadMeta records.
type meta struct {
	Path    string    `toml:"path"` // the folder the CLI read, for the origin record
	Created time.Time `toml:"created"`
}

// Upload is an upload on disk: Dir is its folder, a skill or a collection,
// and Path the folder the CLI read.
type Upload struct {
	ID      string
	Dir     string
	Path    string
	Expires time.Time
}

// SaveUpload extracts the gzipped tar r into a new upload under tmpRoot and
// returns it. The archive holds exactly one folder, whose name is the
// skill's unless the install renames it; directories and regular files
// only, `.git` and `.holaclaw` left out, within the install limits. path is
// kept for the origin record. Uploads past UploadTTL are swept first. A bad
// archive is ErrBadSource, and nothing of it is kept.
func SaveUpload(tmpRoot string, r io.Reader, path string) (u Upload, err error) {
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return Upload{}, err
	}
	now := time.Now().UTC()
	sweepUploads(tmpRoot, now)
	id := ulid.Make().String()
	dir := filepath.Join(tmpRoot, uploadPrefix+id)
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	if err := fsutil.ExtractTarGz(r, dir, installCopy); err != nil {
		return Upload{}, fmt.Errorf("%w: upload: %w", ErrBadSource, err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return Upload{}, err
	}
	if len(ents) != 1 || !ents[0].IsDir() || strings.HasPrefix(ents[0].Name(), ".") {
		return Upload{}, fmt.Errorf("%w: upload: the archive must hold one folder", ErrBadSource)
	}
	b, err := toml.Marshal(meta{Path: path, Created: now})
	if err != nil {
		return Upload{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, uploadMeta), b, 0o600); err != nil {
		return Upload{}, err
	}
	return Upload{ID: id, Dir: filepath.Join(dir, ents[0].Name()), Path: path, Expires: now.Add(UploadTTL)}, nil
}

// OpenUpload finds the upload id under tmpRoot. An id that is not a ULID,
// that has no upload, or whose upload is past UploadTTL (then removed) is
// ErrNotFound.
func OpenUpload(tmpRoot, id string) (Upload, error) {
	missing := fmt.Errorf("upload %s: %w", id, ErrNotFound)
	if _, err := ulid.ParseStrict(id); err != nil {
		return Upload{}, missing
	}
	dir := filepath.Join(tmpRoot, uploadPrefix+id)
	m, err := readMeta(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Upload{}, missing
	}
	if err != nil {
		return Upload{}, err
	}
	expires := m.Created.Add(UploadTTL)
	if !time.Now().Before(expires) {
		_ = os.RemoveAll(dir)
		return Upload{}, fmt.Errorf("upload %s expired: %w", id, ErrNotFound)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return Upload{}, err
	}
	for _, e := range ents {
		if e.IsDir() {
			return Upload{ID: id, Dir: filepath.Join(dir, e.Name()), Path: m.Path, Expires: expires}, nil
		}
	}
	return Upload{}, missing
}

// RemoveUpload deletes the upload id; one already gone is not an error.
func RemoveUpload(tmpRoot, id string) error {
	if _, err := ulid.ParseStrict(id); err != nil {
		return fmt.Errorf("upload %s: %w", id, ErrNotFound)
	}
	return os.RemoveAll(filepath.Join(tmpRoot, uploadPrefix+id))
}

func readMeta(dir string) (meta, error) {
	var m meta
	b, err := os.ReadFile(filepath.Join(dir, uploadMeta))
	if err != nil {
		return meta{}, err
	}
	if err := toml.Unmarshal(b, &m); err != nil {
		return meta{}, fmt.Errorf("skill: %s: %w", uploadMeta, err)
	}
	return m, nil
}

// sweepUploads removes the uploads under tmpRoot that are past UploadTTL at
// now; one without a readable record goes by the folder's time.
func sweepUploads(tmpRoot string, now time.Time) {
	dirs, _ := filepath.Glob(filepath.Join(tmpRoot, uploadPrefix+"*"))
	for _, dir := range dirs {
		created := time.Time{}
		if m, err := readMeta(dir); err == nil {
			created = m.Created
		} else if st, err := os.Stat(dir); err == nil {
			created = st.ModTime()
		}
		if !now.Before(created.Add(UploadTTL)) {
			_ = os.RemoveAll(dir)
		}
	}
}

// CleanTmp removes every entry an install left under tmpRoot: checkouts,
// uploads and stages. The daemon runs it when it starts, before any install
// can be under way. A missing tmpRoot is clean.
func CleanTmp(tmpRoot string) error {
	dirs, err := filepath.Glob(filepath.Join(tmpRoot, tmpPrefix+"*"))
	if err != nil {
		return err
	}
	var errs []error
	for _, dir := range dirs {
		errs = append(errs, os.RemoveAll(dir))
	}
	return errors.Join(errs...)
}
