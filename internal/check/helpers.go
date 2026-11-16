package check

import (
	"path/filepath"
	"sort"
)

// sortedKeys returns a map's keys sorted, so rule output never depends on
// map iteration order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// relFile renders an absolute or config-dir-relative path the way findings
// report File: relative to the config directory when possible, unchanged
// otherwise (a secrets path or data dir outside it).
func relFile(dir, path string) string {
	if dir == "" || path == "" {
		return path
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil || len(rel) >= 2 && rel[:2] == ".." {
		return path
	}
	return rel
}

// contains reports whether s is in ss.
func contains(ss []string, s string) bool {
	for _, e := range ss {
		if e == s {
			return true
		}
	}
	return false
}
