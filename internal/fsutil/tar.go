package fsutil

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// WriteTarGz writes the directories and regular files under src to w as a
// gzipped tar whose one top-level folder is src's base name, directories
// before what they hold. What o skips, refuses and limits is as in
// CopyTree. A file is stored with its permission bits.
func WriteTarGz(w io.Writer, src string, o CopyOptions) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	base := filepath.Base(src)
	if !st.IsDir() || !filepath.IsLocal(base) {
		return fmt.Errorf("fsutil: %s is not a named directory", src)
	}
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: base + "/", Mode: 0o755, ModTime: st.ModTime()}); err != nil {
		return err
	}
	b := &budget{o: o}
	err = walk(src, o, func(p, rel string, d fs.DirEntry) error {
		name := base + "/" + rel
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			if err := b.dir(rel); err != nil {
				return err
			}
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755, ModTime: info.ModTime()})
		}
		if err := b.file(rel); err != nil {
			return err
		}
		if err := b.add(rel, info.Size()); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		h := &tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime()}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		// CopyN: a file that grew since its header is cut at the header's size,
		// one that shrank fails.
		_, err = io.CopyN(tw, f, info.Size())
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// ExtractTarGz reads a gzipped tar from r into dst, which must not exist
// yet. Only directories and regular files are taken; a link is ErrSymlink,
// any other entry ErrNotRegular. Every name must be a clean local slash path
// whose parent directory came earlier in the archive, as WriteTarGz writes
// them. A file is 0o755 when any execute bit is set, else 0o644. What o
// skips and limits is as in CopyTree. On an error what was extracted stays
// for the caller to remove.
func ExtractTarGz(r io.Reader, dst string, o CopyOptions) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()
	if err := os.Mkdir(dst, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	b := &budget{o: o}
	var skipped []string // skipped directories, each with a trailing slash
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(h.Name, "/")
		if name == "." || path.Clean(name) != name || !filepath.IsLocal(filepath.FromSlash(name)) {
			return fmt.Errorf("fsutil: %q is not a local path", h.Name)
		}
		if under(name, skipped) {
			continue
		}
		if o.Skip != nil && o.Skip(name, fs.FileInfoToDirEntry(h.FileInfo())) {
			if h.Typeflag == tar.TypeDir {
				skipped = append(skipped, name+"/")
			}
			continue
		}
		to := filepath.Join(dst, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := b.dir(name); err != nil {
				return err
			}
			if err := os.Mkdir(to, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := b.file(name); err != nil {
				return err
			}
			if err := b.add(name, h.Size); err != nil {
				return err
			}
			if err := extractFile(tr, to, h); err != nil {
				return err
			}
		case tar.TypeSymlink, tar.TypeLink:
			return fmt.Errorf("%s: %w", name, ErrSymlink)
		default:
			return fmt.Errorf("%s: %w", name, ErrNotRegular)
		}
	}
}

// under reports whether name is inside one of dirs (each ending in a slash).
func under(name string, dirs []string) bool {
	for _, d := range dirs {
		if strings.HasPrefix(name, d) {
			return true
		}
	}
	return false
}

// extractFile writes the body of the entry h, read from r, to the new file to.
func extractFile(r io.Reader, to string, h *tar.Header) (err error) {
	perm := fs.FileMode(0o644)
	if h.Mode&0o111 != 0 {
		perm = 0o755
	}
	f, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	if _, err := io.Copy(f, r); err != nil {
		return err
	}
	return f.Chmod(perm) // the umask may have narrowed it
}
