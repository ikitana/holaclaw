// Package secrets reads and writes auth.toml, the one file the daemon writes
// (ARCHITECTURE.md §4). Every secret lives here and nowhere else: not in SQLite,
// so backups, snapshots and trace exports never carry tokens. Writes are
// re-read, locked, atomic and mode 0600, because OAuth refresh tokens are
// single-use and the daemon is their single owner (docs/models.md).
package secrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/pelletier/go-toml/v2"
)

// Version is the schema version every config file of a directory carries (ARCHITECTURE.md §4).
const Version = 1

// Section is one of the tables auth.toml is keyed by, matching the config:
// [providers.<name>], [channels.<name>] and [connectors.<id>].
type Section string

const (
	Providers  Section = "providers"
	Channels   Section = "channels"
	Connectors Section = "connectors"
)

// allSections is every section, in the order the file is written.
var allSections = [...]Section{Providers, Channels, Connectors}

// Sections lists every section, in the order the file is written. The slice
// is a copy: callers cannot change what the package accepts.
func Sections() []Section { return append([]Section(nil), allSections[:]...) }

// Keys of a [connectors.<id>] entry (docs/connectors.md §4). The token's
// expiry is not a secret and lives in the connector's cache row.
const (
	ConnectorAccessToken  = "access_token"
	ConnectorRefreshToken = "refresh_token"
	// ConnectorClientSecret is set only when the server issued one, either
	// for a pre-registered client or through dynamic client registration.
	ConnectorClientSecret = "client_secret"
)

// known reports whether s is one of the sections.
func (s Section) known() bool {
	for _, k := range allSections {
		if s == k {
			return true
		}
	}
	return false
}

// emptySections is the parsed form of a file with no entries.
func emptySections() sections {
	data := make(sections, len(allSections))
	for _, s := range allSections {
		data[s] = map[string]map[string]string{}
	}
	return data
}

// sections is the whole file: section → entry name → key → value.
type sections map[Section]map[string]map[string]string

// minRedact is the shortest value the redactor will look for. Shorter values
// (an empty key, a two-letter placeholder) would match ordinary prose.
const minRedact = 8

// renameFile is the final step of the write path, indirected so tests can make
// it fail and assert that no temp file is left behind.
var renameFile = os.Rename

// File is an open auth.toml. Reads are served from the in-memory copy; every
// write re-reads the file from disk under an exclusive lock first, so writers
// in other goroutines or other processes never lose each other's entries.
type File struct {
	path string
	lock string

	mu   sync.RWMutex
	data sections
}

// Open reads auth.toml. A missing file is not an error: it is an empty secret
// set the first write creates. Bad permissions are not an error either, so
// `holaclaw check` can report them — use CheckPerms for that.
func Open(path string) (*File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	f := &File{path: abs, lock: abs + ".lock"}
	if err := f.Reload(); err != nil {
		return nil, err
	}
	return f, nil
}

// Path is the file this handle reads and writes.
func (f *File) Path() string { return f.path }

// Reload re-reads the file from disk, picking up writes made by another
// process.
func (f *File) Reload() error {
	_, data, err := read(f.path)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = data
	return nil
}

// Get returns one value from the in-memory copy.
func (f *File) Get(section Section, name, key string) (string, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	v, ok := f.data[section][name][key]
	return v, ok
}

// Lookup returns a closure that reads the current value on each call, so a
// provider holding it keeps working across a token rotation without rewiring.
func (f *File) Lookup(section Section, name, key string) func() string {
	return func() string {
		v, _ := f.Get(section, name, key)
		return v
	}
}

// Names lists the entries of a section, sorted.
func (f *File) Names(section Section) []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return sortedKeys(f.data[section])
}

// Keys lists the key names of one entry, sorted. Names only: nothing in this
// package returns every value at once.
func (f *File) Keys(section Section, name string) []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return sortedKeys(f.data[section][name])
}

// Redactor returns a function that replaces every secret currently in the file
// with "[redacted]", for log and event sinks. It reads live state, so values
// set after it was built are redacted too.
func (f *File) Redactor() func(string) string {
	return func(s string) string {
		f.mu.RLock()
		values := make([]string, 0, 8)
		for _, entries := range f.data {
			for _, kv := range entries {
				for _, v := range kv {
					if len(v) >= minRedact {
						values = append(values, v)
					}
				}
			}
		}
		f.mu.RUnlock()

		// Longest first: a token that contains a shorter one still redacts
		// whole rather than leaving a tail behind.
		sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
		for _, v := range values {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
		return s
	}
}

// Set merges kv into one entry, creating it if needed, and rewrites the file.
func (f *File) Set(section Section, name string, kv map[string]string) error {
	if err := checkName(name); err != nil {
		return err
	}
	for k := range kv {
		if err := checkName(k); err != nil {
			return err
		}
	}
	return f.mutate(section, func(data sections) {
		entries := data[section]
		if entries == nil {
			entries = map[string]map[string]string{}
			data[section] = entries
		}
		if entries[name] == nil {
			entries[name] = map[string]string{}
		}
		for k, v := range kv {
			entries[name][k] = v
		}
	})
}

// Delete removes the named keys from an entry, or the whole entry when no key
// is given. Removing what is not there is not an error.
func (f *File) Delete(section Section, name string, keys ...string) error {
	if err := checkName(name); err != nil {
		return err
	}
	return f.mutate(section, func(data sections) {
		if len(keys) == 0 {
			delete(data[section], name)
			return
		}
		entry := data[section][name]
		for _, k := range keys {
			delete(entry, k)
		}
		if len(entry) == 0 {
			delete(data[section], name)
		}
	})
}

// mutate is the write path: exclusive lock, re-read from disk, apply the
// change to that fresh copy, write a temp file (0600) + fsync + rename + fsync
// dir, release the lock, refresh the in-memory copy.
func (f *File) mutate(section Section, apply func(sections)) error {
	if !section.known() {
		return &LoadError{Path: f.path, Key: string(section), Err: ErrUnknownKey}
	}
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("secrets: %w", err)
	}

	unlock, err := flockExclusive(f.lock)
	if err != nil {
		return err
	}
	defer unlock()

	if err := refuseIrregular(f.path); err != nil {
		return err
	}
	version, data, err := read(f.path)
	if err != nil {
		return err
	}
	apply(data)

	if err := writeFile(f.path, version, data); err != nil {
		return err
	}

	f.mu.Lock()
	f.data = data
	f.mu.Unlock()
	return nil
}

// writeFile renders the whole file and swaps it in atomically. The temp file
// lives in the target directory (same filesystem) and is removed on any error,
// so a failed write leaves nothing behind.
func writeFile(path string, version int, data sections) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".auth-*.toml")
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	tmpName := tmp.Name()
	done := false
	defer func() {
		_ = tmp.Close()
		if !done {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil { // explicit: the swap carries this mode over
		return fmt.Errorf("secrets: %w", err)
	}
	if _, err := tmp.Write(encode(version, data)); err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	if err := renameFile(tmpName, path); err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	done = true
	return syncDir(dir)
}

// read parses the file from disk. A missing file is an empty set.
func read(path string) (int, sections, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Version, emptySections(), nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("secrets: %w", err)
	}

	var raw map[string]any
	if err := toml.Unmarshal(b, &raw); err != nil {
		// Never wrap the decoder's error: it quotes the offending line, which
		// is a secret. Only the position is reported.
		detail := ""
		var de *toml.DecodeError
		if errors.As(err, &de) {
			row, col := de.Position()
			detail = fmt.Sprintf("line %d, column %d", row, col)
		}
		return 0, nil, &LoadError{Path: path, Detail: detail, Err: ErrMalformed}
	}

	version, err := readVersion(path, raw)
	if err != nil {
		return 0, nil, err
	}
	data := emptySections()
	for key, val := range raw {
		switch {
		case key == "version":
			continue
		case Section(key).known():
			entries, err := readSection(path, key, val)
			if err != nil {
				return 0, nil, err
			}
			data[Section(key)] = entries
		default:
			return 0, nil, &LoadError{Path: path, Key: key, Err: ErrUnknownKey}
		}
	}
	return version, data, nil
}

func readVersion(path string, raw map[string]any) (int, error) {
	v, ok := raw["version"]
	if !ok {
		return 0, &LoadError{Path: path, Key: "version", Detail: "missing", Err: ErrBadVersion}
	}
	n, ok := v.(int64)
	if !ok {
		return 0, &LoadError{Path: path, Key: "version", Detail: typeName(v), Err: ErrBadVersion}
	}
	if int(n) != Version {
		return 0, &LoadError{
			Path: path, Key: "version",
			Detail: fmt.Sprintf("have %d, want %d", n, Version), Err: ErrBadVersion,
		}
	}
	return int(n), nil
}

// readSection walks one section table: a table of tables of
// strings, nothing else.
func readSection(path, section string, val any) (map[string]map[string]string, error) {
	table, ok := val.(map[string]any)
	if !ok {
		return nil, &LoadError{Path: path, Key: section, Detail: typeName(val), Err: ErrNotString}
	}
	entries := make(map[string]map[string]string, len(table))
	for name, entry := range table {
		kvAny, ok := entry.(map[string]any)
		if !ok {
			return nil, &LoadError{
				Path: path, Key: section + "." + name, Detail: typeName(entry), Err: ErrNotString,
			}
		}
		kv := make(map[string]string, len(kvAny))
		for k, v := range kvAny {
			s, ok := v.(string)
			if !ok {
				return nil, &LoadError{
					Path: path, Key: section + "." + name + "." + k,
					Detail: typeName(v), Err: ErrNotString,
				}
			}
			kv[k] = s
		}
		entries[name] = kv
	}
	return entries, nil
}

// encode renders the file. Deterministic order keeps an unchanged entry
// byte-identical across rewrites.
func encode(version int, data sections) []byte {
	var b strings.Builder
	b.WriteString("# holaclaw auth.toml — every secret, written by the daemon.\n")
	b.WriteString("# Machine file: comments and formatting are not preserved on rewrite.\n")
	b.WriteString("# Keep it mode 0600.\n\n")
	fmt.Fprintf(&b, "version = %d\n", version)
	for _, section := range allSections {
		for _, name := range sortedKeys(data[section]) {
			fmt.Fprintf(&b, "\n[%s.%s]\n", section, tomlKey(name))
			kv := data[section][name]
			for _, k := range sortedKeys(kv) {
				fmt.Fprintf(&b, "%s = %s\n", tomlKey(k), tomlString(kv[k]))
			}
		}
	}
	return []byte(b.String())
}

// flockExclusive takes an exclusive advisory lock on the sibling lock file.
// flock is per open file description, so it serializes goroutines of this
// process as well as other processes. Linux only, like the daemon.
func flockExclusive(path string) (func(), error) {
	lf, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("secrets: lock: %w", err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		_ = lf.Close()
		return nil, fmt.Errorf("secrets: lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		_ = lf.Close()
	}, nil
}

// syncDir fsyncs a directory so a rename survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	return nil
}

// CheckPerms reports whether the file is safe to hold secrets: a regular file
// (a symlink is refused) with nothing set for group or other. A missing file
// is fine — it is an empty secret set. This is what `holaclaw check` calls;
// Open deliberately does not, so check can report instead of fail.
func CheckPerms(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	if !info.Mode().IsRegular() {
		return &PermError{Path: path, Mode: info.Mode(), Err: ErrNotRegular}
	}
	if info.Mode().Perm()&0o077 != 0 {
		return &PermError{Path: path, Mode: info.Mode(), Err: ErrBadMode}
	}
	return nil
}

// refuseIrregular blocks a write through a symlink or anything that is not a
// regular file; the rename would otherwise replace the link itself, or worse,
// follow it out of the config directory.
func refuseIrregular(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	if !info.Mode().IsRegular() {
		return &PermError{Path: path, Mode: info.Mode(), Err: ErrNotRegular}
	}
	return nil
}

// checkName rejects entry and key names that are empty or cannot be written
// back as a TOML key.
func checkName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrBadName)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: control character", ErrBadName)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// tomlKey writes a key bare when it can be, quoted otherwise.
func tomlKey(s string) string {
	bare := s != ""
	for _, r := range s {
		if !bareKeyRune(r) {
			bare = false
			break
		}
	}
	if bare {
		return s
	}
	return tomlString(s)
}

// bareKeyRune reports whether r may appear in an unquoted TOML key.
func bareKeyRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	default:
		return r == '_' || r == '-'
	}
}

func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// typeName names a value's kind for an error message — never the value.
func typeName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case int64, float64:
		return "number"
	case string:
		return "string"
	case map[string]any:
		return "table"
	case []any:
		return "array"
	default:
		return "value"
	}
}
