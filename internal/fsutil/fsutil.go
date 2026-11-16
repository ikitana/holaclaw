// Package fsutil holds the filesystem helpers holaclaw shares: listing
// directories the way its loaders need (sorted, never following symlinks,
// with hidden entries kept unless the caller asks otherwise), the small write
// helpers the writers share (a synced temp file to rename into place, a new
// file that never overwrites, a file replaced with one rename, and a symlink
// check on a path), creating files and directories a caller may have to take
// back, and copying a tree within limits: to a folder, or to a gzipped tar
// and back.
package fsutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Options selects what Subdirs and Files return. The zero value lists every
// entry, hidden ones included.
type Options struct {
	SkipHidden bool // skip entries whose name starts with "."; Files does not descend into them
	Max        int  // stop after this many entries; 0 means no limit
}

func (o Options) skip(name string) bool {
	return o.SkipHidden && strings.HasPrefix(name, ".")
}

func (o Options) full(n int) bool { return o.Max > 0 && n >= o.Max }

// Abs returns p as an absolute, clean path. Unlike a caller that falls back
// to p, it fails when the working directory cannot be read, so a relative
// path is never returned as if it were absolute.
func Abs(p string) (string, error) {
	a, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("fsutil: absolute path of %q: %w", p, err)
	}
	return a, nil
}

// MkdirAll is os.MkdirAll that returns the directories it created, outermost
// first, so a caller that fails later can take them back. On an error the
// ones created so far are returned with it.
func MkdirAll(dir string, perm fs.FileMode) ([]string, error) {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	var created []string
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(missing[i], perm); err != nil {
			return created, err
		}
		created = append(created, missing[i])
	}
	return created, nil
}

// Subdirs lists the names of the real subdirectories of dir, sorted. A
// symlink to a directory is not one. A missing dir is an error wrapping
// fs.ErrNotExist.
func Subdirs(dir string, o Options) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || o.skip(e.Name()) {
			continue
		}
		if o.full(len(out)) {
			break
		}
		out = append(out, e.Name())
	}
	return out, nil
}

// Files walks root inside fsys without following symlinks and returns its
// regular files as slash paths relative to root, sorted. The walk stops at
// o.Max files, so with a limit the result is the first ones in walk order,
// which is lexical. Entries that cannot be read are skipped. A root that is
// missing or not a directory holds no files and is not an error; any other
// failure to stat it is.
func Files(fsys fs.FS, root string, o Options) ([]string, error) {
	st, err := fs.Lstat(fsys, root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	case !st.IsDir():
		return nil, nil
	}
	var out []string
	err = fs.WalkDir(fsys, root, func(p string, e fs.DirEntry, err error) error {
		switch {
		case err != nil || p == root:
			return nil
		case o.skip(e.Name()):
			if e.IsDir() {
				return fs.SkipDir
			}
			return nil
		case !e.Type().IsRegular():
			return nil // a directory is walked, a symlink or device is not
		}
		out = append(out, strings.TrimPrefix(p, root+"/"))
		if o.full(len(out)) {
			return fs.SkipAll
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}
