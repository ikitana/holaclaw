package fsutil

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// CopyOptions selects what CopyTree, WalkFiles, WriteTarGz and ExtractTarGz
// take. The zero value takes every directory and regular file, skips
// symlinks and has no limit.
type CopyOptions struct {
	// Skip, when not nil, leaves an entry out by its slash path relative to
	// the root, and for a directory everything under it.
	Skip           func(rel string, d fs.DirEntry) bool
	RefuseSymlinks bool  // a symlink is ErrSymlink instead of skipped
	MaxFiles       int   // regular files, and directories counted apart; 0 means no limit
	MaxBytes       int64 // the files' bytes together; 0 means no limit
}

// Errors of the limits and of an entry CopyOptions or a tar does not allow,
// matched with errors.Is; the detail is wrapped around them.
var (
	ErrTooMany    = errors.New("fsutil: too many files")
	ErrTooLarge   = errors.New("fsutil: too large")
	ErrNotRegular = errors.New("fsutil: not a regular file or directory")
)

// budget counts what a copy has taken against the limits of o.
type budget struct {
	o           CopyOptions
	files, dirs int
	bytes       int64
}

// dir takes one directory.
func (b *budget) dir(rel string) error {
	if b.dirs++; b.o.MaxFiles > 0 && b.dirs > b.o.MaxFiles {
		return fmt.Errorf("%s: %w (over %d directories)", rel, ErrTooMany, b.o.MaxFiles)
	}
	return nil
}

// file takes one file.
func (b *budget) file(rel string) error {
	if b.files++; b.o.MaxFiles > 0 && b.files > b.o.MaxFiles {
		return fmt.Errorf("%s: %w (over %d)", rel, ErrTooMany, b.o.MaxFiles)
	}
	return nil
}

// add takes n bytes of the file rel.
func (b *budget) add(rel string, n int64) error {
	if b.bytes += n; b.o.MaxBytes > 0 && b.bytes > b.o.MaxBytes {
		return fmt.Errorf("%s: %w (over %d bytes)", rel, ErrTooLarge, b.o.MaxBytes)
	}
	return nil
}

// left is how many more bytes the files may hold, -1 for no limit.
func (b *budget) left() int64 {
	if b.o.MaxBytes <= 0 {
		return -1
	}
	return b.o.MaxBytes - b.bytes
}

// walk calls fn for every entry under src that o takes, root excluded, with
// its slash path: directories before what they hold, in lexical order.
func walk(src string, o CopyOptions, fn func(p, rel string, d fs.DirEntry) error) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == src {
			return nil
		}
		r, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(r)
		if o.Skip != nil && o.Skip(rel, d) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0 && o.RefuseSymlinks:
			return fmt.Errorf("%s: %w", rel, ErrSymlink)
		case !d.IsDir() && !d.Type().IsRegular():
			return nil // a symlink or device is not taken
		}
		return fn(p, rel, d)
	})
}

// CopyTree copies the directories and regular files under src into dst,
// which must not exist yet, keeping each file's permission bits and syncing
// it. Symlinks and other non-regular entries are skipped, never followed,
// unless o refuses symlinks. Past a limit of o the copy stops with
// ErrTooMany or ErrTooLarge, and what was copied stays for the caller to
// remove.
func CopyTree(src, dst string, o CopyOptions) error {
	if _, err := os.Stat(src); err != nil {
		return err
	}
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	b := &budget{o: o}
	return walk(src, o, func(p, rel string, d fs.DirEntry) error {
		to := filepath.Join(dst, filepath.FromSlash(rel))
		if d.IsDir() {
			if err := b.dir(rel); err != nil {
				return err
			}
			return os.Mkdir(to, 0o755)
		}
		// The size up front refuses a big file without copying it; the
		// limited copy catches one that grows meanwhile.
		st, err := d.Info()
		if err != nil {
			return err
		}
		if err := b.file(rel); err != nil {
			return err
		}
		limit := b.left()
		if err := b.add(rel, st.Size()); err != nil {
			return err
		}
		n, err := copyFile(p, to, limit)
		if err != nil {
			return err
		}
		return b.add(rel, n-st.Size())
	})
}

// copyFile copies the regular file src to the new file dst with src's
// permission bits, synced, and returns how many bytes it copied. limit, when
// not negative, stops the copy after limit+1 bytes.
func copyFile(src, dst string, limit int64) (n int64, err error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		return 0, err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, st.Mode().Perm())
	if err != nil {
		return 0, err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	var r io.Reader = in
	if limit >= 0 {
		r = io.LimitReader(in, limit+1)
	}
	if n, err = io.Copy(out, r); err != nil {
		return n, err
	}
	if err = out.Chmod(st.Mode().Perm()); err != nil { // the umask may have narrowed it
		return n, err
	}
	return n, out.Sync()
}

// WalkFiles calls fn with the slash path of every regular file under dir
// that skip does not leave out, in sorted slash-path order, and a way to
// open it; symlinks are skipped. An error from fn stops the walk.
func WalkFiles(dir string, skip func(rel string, d fs.DirEntry) bool, fn func(rel string, open func() (fs.File, error)) error) error {
	var rels []string
	err := walk(dir, CopyOptions{Skip: skip}, func(_, rel string, d fs.DirEntry) error {
		if !d.IsDir() {
			rels = append(rels, rel)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(rels)
	for _, rel := range rels {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := fn(rel, func() (fs.File, error) { return os.Open(p) }); err != nil {
			return err
		}
	}
	return nil
}

// ErrSymlink is returned by NoSymlinks for a path element that is a symlink,
// and by the copies for a symlink they refuse.
var ErrSymlink = errors.New("fsutil: is a symlink")

// NoSymlinks walks root/parts[0]/parts[1]/... and fails with ErrSymlink on
// the first element that is a symlink. The walk ends at the first element
// that does not exist, so it checks a path before it is created. root itself
// is not checked.
func NoSymlinks(root string, parts ...string) error {
	p := root
	for _, part := range parts {
		p = filepath.Join(p, part)
		st, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s: %w", p, ErrSymlink)
		}
	}
	return nil
}

// WriteTemp writes data to a new temp file in dir, named by pattern as
// os.CreateTemp does, synced and with mode perm, and returns its path. The
// caller renames it into place, in the same directory so the rename is
// atomic. On failure nothing is left behind.
func WriteTemp(dir, pattern string, data []byte, perm fs.FileMode) (path string, err error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	if _, err = f.Write(data); err != nil {
		return "", err
	}
	if err = f.Sync(); err != nil {
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	if err = os.Chmod(f.Name(), perm); err != nil {
		return "", err
	}
	return f.Name(), nil
}

// CreateFile writes data to a new file at path with mode perm. A file
// already there is an error wrapping fs.ErrExist and is left as it is. On
// any other failure nothing is left behind.
func CreateFile(path string, data []byte, perm fs.FileMode) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ReplaceFile swaps data in at path with one rename, keeping the mode of the
// file there, or giving perm to a new one.
func ReplaceFile(path string, data []byte, perm fs.FileMode) error {
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
	}
	tmp, err := WriteTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*", data, perm)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Missing returns, as paths, the names in dir that do not exist yet.
func Missing(dir string, names ...string) []string {
	var out []string
	for _, n := range names {
		p := filepath.Join(dir, n)
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			out = append(out, p)
		}
	}
	return out
}
