package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// CurrentVersion is the config version this build writes and expects. Old
// configs always migrate: the ladder below is kept forever (ARCHITECTURE.md §4).
const CurrentVersion = 1

// Step migrates a whole config directory from version From to From+1. Fn
// receives every config file keyed by its slash-separated path relative to the
// directory (auth.toml is never included) and returns the new set: a missing
// key deletes the file, a new key creates it.
type Step struct {
	From int
	Fn   func(files map[string][]byte) (map[string][]byte, error)
}

// Ladder is the ordered list of migration steps. v1 is the first version, so
// it is empty; steps are appended and never removed or reordered.
var Ladder []Step

// Migrate computes the migration of a config directory to CurrentVersion and
// returns a unified-style diff of the change. With write set it first copies
// the current files into one timestamped `.backup-<ts>/` directory and then
// writes the migrated files atomically. `auth.toml` is never read, backed up
// or written here. A directory already at CurrentVersion yields an empty diff.
func Migrate(dir string, write bool) (string, error) {
	return migrate(dir, write, Ladder, time.Now())
}

func migrate(dir string, write bool, ladder []Step, now time.Time) (string, error) {
	before, err := readConfigFiles(dir)
	if err != nil {
		return "", err
	}
	from, err := versionOf(before)
	if err != nil {
		return "", err
	}
	if from > CurrentVersion {
		return "", fmt.Errorf("config: directory is version %d, this build knows %d", from, CurrentVersion)
	}

	after := cloneFiles(before)
	steps := append([]Step(nil), ladder...)
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].From < steps[j].From })
	for _, s := range steps {
		if s.From < from {
			continue
		}
		if s.From != from {
			return "", fmt.Errorf("config: migration ladder has no step from version %d", from)
		}
		after, err = s.Fn(cloneFiles(after))
		if err != nil {
			return "", fmt.Errorf("config: migration %d→%d: %w", s.From, s.From+1, err)
		}
		from = s.From + 1
	}

	diff := diffFiles(before, after)
	if !write || diff == "" {
		return diff, nil
	}
	if err := backup(dir, before, now); err != nil {
		return diff, err
	}
	if err := writeFiles(dir, before, after); err != nil {
		return diff, err
	}
	return diff, nil
}

// readConfigFiles collects the files a migration may rewrite: config.toml,
// channels.toml, connectors.toml and every agents/<name>/ file. auth.toml is
// excluded.
func readConfigFiles(dir string) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, name := range []string{FileConfig, FileChannels, FileConnectors} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			if name == FileConfig {
				return nil, fmt.Errorf("config: %s: %w", name, err)
			}
			continue
		}
		out[name] = b
	}
	agents, err := os.ReadDir(filepath.Join(dir, "agents"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, a := range agents {
		if !a.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, "agents", a.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.IsDir() || f.Name() == FileAuth {
				continue
			}
			rel := path.Join("agents", a.Name(), f.Name())
			b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
			if err != nil {
				return nil, err
			}
			out[rel] = b
		}
	}
	return out, nil
}

func versionOf(files map[string][]byte) (int, error) {
	var m struct {
		Version int `toml:"version"`
	}
	if err := toml.Unmarshal(files[FileConfig], &m); err != nil {
		return 0, fmt.Errorf("config: %s: %w", FileConfig, err)
	}
	if m.Version < 1 {
		return 0, fmt.Errorf("config: %s has no usable version", FileConfig)
	}
	return m.Version, nil
}

func cloneFiles(in map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(in))
	for k, v := range in {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// backup copies the current files into one timestamped directory. It is the
// single backup a migration makes, and it never holds secrets.
func backup(dir string, files map[string][]byte, now time.Time) error {
	base := filepath.Join(dir, ".backup-"+now.UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(base, 0o755); err != nil {
		return err
	}
	for _, rel := range sortedKeys(files) {
		dst := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, files[rel], 0o644); err != nil {
			return err
		}
	}
	return nil
}

// writeFiles writes changed files through a temp file + rename and removes
// files the ladder dropped.
func writeFiles(dir string, before, after map[string][]byte) error {
	for _, rel := range sortedKeys(after) {
		if b, ok := before[rel]; ok && string(b) == string(after[rel]) {
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		tmp := dst + ".tmp"
		if err := os.WriteFile(tmp, after[rel], 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, dst); err != nil {
			return err
		}
	}
	for _, rel := range sortedKeys(before) {
		if _, ok := after[rel]; !ok {
			if err := os.Remove(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
				return err
			}
		}
	}
	return nil
}

// diffFiles renders a unified-style diff over the whole file set, in path
// order. The output is for a human reading `holaclaw config migrate`.
func diffFiles(before, after map[string][]byte) string {
	seen := map[string]bool{}
	var paths []string
	for _, m := range []map[string][]byte{before, after} {
		for k := range m {
			if !seen[k] {
				seen[k], paths = true, append(paths, k)
			}
		}
	}
	sort.Strings(paths)

	var b strings.Builder
	for _, p := range paths {
		old, hadOld := before[p]
		neu, hadNew := after[p]
		if hadOld && hadNew && string(old) == string(neu) {
			continue
		}
		from, to := "a/"+p, "b/"+p
		if !hadOld {
			from = "/dev/null"
		}
		if !hadNew {
			to = "/dev/null"
		}
		fmt.Fprintf(&b, "--- %s\n+++ %s\n", from, to)
		b.WriteString(unifiedHunks(lines(string(old)), lines(string(neu))))
	}
	return b.String()
}

func lines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}

// unifiedHunks renders one hunk covering the whole file. Config files are a
// handful of lines; a single hunk keeps the output readable and the code small.
func unifiedHunks(a, b []string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "@@ -1,%d +1,%d @@\n", len(a), len(b))
	for _, op := range diffLines(a, b) {
		out.WriteString(op)
		out.WriteString("\n")
	}
	return out.String()
}

// diffLines is a plain LCS line diff; config files are small enough for the
// quadratic table.
func diffLines(a, b []string) []string {
	n, m := len(a), len(b)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, " "+a[i])
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, "-"+a[i])
			i++
		default:
			out = append(out, "+"+b[j])
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, "-"+a[i])
	}
	for ; j < m; j++ {
		out = append(out, "+"+b[j])
	}
	return out
}
