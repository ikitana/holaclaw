package check

import (
	"io/fs"
	"os"
	"path/filepath"
)

// worldWritable walks dir and reports every entry (the directory itself
// included) that is group- or other-writable. Symlinks are inspected, never
// followed: a link that itself is world-writable is reported, but its target
// is not descended into.
func worldWritable(dir string) []Finding {
	var out []Finding
	check := func(path string) {
		info, err := os.Lstat(path)
		if err != nil {
			return
		}
		if info.Mode().Perm()&0o002 != 0 {
			out = append(out, Finding{
				ID:       "SEC005",
				Severity: SeverityError,
				File:     relFile(dir, path),
				Message:  "world-writable",
				Fix:      "chmod o-w " + path,
			})
		}
	}
	check(dir)
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return nil
		}
		check(path)
		if d.Type()&os.ModeSymlink != 0 {
			return nil // never follow: do not descend into a symlinked directory
		}
		return nil
	})
	return out
}
