package connector

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"holaclaw/internal/config"
)

// fileManager is a Manager that only knows its config directory.
func fileManager(t *testing.T) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	return &Manager{o: Options{Dir: dir}}, filepath.Join(dir, config.FileConnectors)
}

// connectorsTOML is a connectors.toml holding cs, built from config's own
// keys so fixtures follow the schema. A comment heads it, as a user's would.
func connectorsTOML(t *testing.T, cs ...config.Connector) string {
	t.Helper()
	f := connectorsFile{Version: config.CurrentVersion, Connectors: map[string]map[string]any{}}
	for _, c := range cs {
		f.Connectors[c.ID] = config.ConnectorEntry(c)
	}
	b, err := toml.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return "# by hand\n" + string(b)
}

// parseConnectors reads connectors.toml the way config loads it; a file
// config would refuse fails the test. A missing file is empty.
func parseConnectors(t *testing.T, path string) map[string]config.Connector {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]config.Connector{}
	}
	if err != nil {
		t.Fatal(err)
	}
	cs, err := config.ParseConnectors(b)
	if err != nil {
		t.Fatalf("connectors.toml: %v\n%s", err, b)
	}
	return cs
}

// writeEntry sets its own keys and keeps every other key and entry; the
// rewrite is 0600, drops comments and leaves no temp file.
func TestFileWriteEntry(t *testing.T) {
	m, path := fileManager(t)
	if ok, err := m.hasEntry("gh"); err != nil || ok {
		t.Fatalf("missing file: %v %v", ok, err)
	}
	homes := config.Connector{ID: "homes", URL: "https://homes.example/", Auth: "none", Trusted: true}
	old := config.Connector{ID: "gh", URL: "https://old.example/", Auth: "oauth", Scopes: []string{"old"}, ClientID: "Iv1.old", Trusted: true}
	if err := os.WriteFile(path, []byte(connectorsTOML(t, homes, old)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.writeEntry(config.Connector{ID: "gh", URL: "https://gh.example/", Auth: "oauth", Scopes: []string{"repo"}}); err != nil {
		t.Fatal(err)
	}
	// trusted is not the writer's key: it stays.
	want := map[string]config.Connector{
		"homes": homes,
		"gh":    {ID: "gh", URL: "https://gh.example/", Auth: "oauth", Scopes: []string{"repo"}, Trusted: true},
	}
	if got := parseConnectors(t, path); !reflect.DeepEqual(got, want) {
		t.Errorf("connectors = %+v\nwant %+v", got, want)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v", info.Mode(), err)
	}
	if b, _ := os.ReadFile(path); !strings.HasPrefix(string(b), fileHeader) || strings.Contains(string(b), "# by hand\n") {
		t.Errorf("header or comments: %s", b)
	}
	if tmp, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".connectors-*")); len(tmp) != 0 {
		t.Errorf("temp files left: %v", tmp)
	}

	// No scopes: the key goes; a client id comes.
	if err := m.writeEntry(config.Connector{ID: "gh", URL: "https://gh.example/", Auth: "oauth", ClientID: "Iv1.app"}); err != nil {
		t.Fatal(err)
	}
	if gh := parseConnectors(t, path)["gh"]; gh.ClientID != "Iv1.app" || gh.Scopes != nil {
		t.Errorf("gh = %+v", gh)
	}

	if err := m.removeEntry("gh"); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.hasEntry("gh"); err != nil || ok {
		t.Errorf("after remove: %v %v", ok, err)
	}
	if ids := slices.Sorted(maps.Keys(parseConnectors(t, path))); !slices.Equal(ids, []string{"homes"}) {
		t.Errorf("entries = %v", ids)
	}
	if err := m.removeEntry("nope"); err != nil {
		t.Errorf("remove unknown: %v", err)
	}
}

// Removing an entry the file does not name leaves it byte for byte,
// comments included.
func TestFileRemoveNoop(t *testing.T) {
	m, path := fileManager(t)
	seed := connectorsTOML(t, config.Connector{ID: "homes", URL: "https://homes.example/", Auth: "none"})
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.removeEntry("gh"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != seed {
		t.Errorf("file rewritten: %s", b)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, %v", info.Mode(), err)
	}
}

// A version the file already has is kept, even one this build would not
// load; a missing one becomes the current version.
func TestFileVersion(t *testing.T) {
	m, path := fileManager(t)
	readVersion := func() any {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var raw map[string]any
		if err := toml.Unmarshal(b, &raw); err != nil {
			t.Fatal(err)
		}
		return raw["version"]
	}
	if err := os.WriteFile(path, []byte("version = 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.writeEntry(config.Connector{ID: "gh", URL: "https://gh.example/", Auth: "none"}); err != nil {
		t.Fatal(err)
	}
	if v := readVersion(); v != int64(7) {
		t.Errorf("version = %v, want 7", v)
	}
	if err := os.WriteFile(path, []byte("[connectors.gh]\nurl = \"https://gh.example/\"\nauth = \"none\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.writeEntry(config.Connector{ID: "gh", URL: "https://gh.example/", Auth: "none"}); err != nil {
		t.Fatal(err)
	}
	if v := readVersion(); v != int64(config.CurrentVersion) {
		t.Errorf("version = %v, want %d", v, config.CurrentVersion)
	}
}

// Writers of different entries in parallel lose none of them: fileMu
// orders them in the process, the flock across processes.
func TestFileConcurrentWrites(t *testing.T) {
	m, path := fileManager(t)
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			if err := m.writeEntry(config.Connector{ID: id, URL: "https://" + id + ".example/", Auth: "none"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if got := slices.Sorted(maps.Keys(parseConnectors(t, path))); !slices.Equal(got, ids) {
		t.Errorf("entries = %v, want %v", got, ids)
	}
}

// A missing file is empty: removing from it creates nothing.
func TestFileMissing(t *testing.T) {
	m, path := fileManager(t)
	if err := m.removeEntry("gh"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file created: %v", err)
	}
}

// A file that does not parse, or has a top-level key config refuses, is
// refused and never overwritten.
func TestFileBroken(t *testing.T) {
	for name, body := range map[string]string{
		"garbage":     "this is = = not toml",
		"unknown key": "version = 1\nextra = true\n",
	} {
		t.Run(name, func(t *testing.T) {
			m, path := fileManager(t)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := m.hasEntry("gh"); !errors.Is(err, ErrBadFile) {
				t.Errorf("hasEntry on a broken file: %v", err)
			}
			if err := m.writeEntry(config.Connector{ID: "gh", URL: "https://gh.example/", Auth: "none"}); !errors.Is(err, ErrBadFile) || strings.Contains(err.Error(), "= =") {
				t.Errorf("writeEntry: %v", err)
			}
			if err := m.removeEntry("gh"); !errors.Is(err, ErrBadFile) {
				t.Errorf("removeEntry on a broken file: %v", err)
			}
			if b, _ := os.ReadFile(path); string(b) != body {
				t.Errorf("file touched: %q", b)
			}
		})
	}
}

// A symlink in place of the file is read but not written through.
func TestFileSymlink(t *testing.T) {
	m, path := fileManager(t)
	target := filepath.Join(t.TempDir(), "elsewhere.toml")
	seed := connectorsTOML(t)
	if err := os.WriteFile(target, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := m.hasEntry("gh"); err != nil {
		t.Errorf("hasEntry through a symlink: %v", err)
	}
	if err := m.writeEntry(config.Connector{ID: "gh", URL: "https://gh.example/", Auth: "none"}); !errors.Is(err, ErrHandManaged) {
		t.Errorf("writeEntry through a symlink: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != seed {
		t.Errorf("target touched: %q", b)
	}
}

// Anything else that is not a regular file is refused too.
func TestFileNotRegular(t *testing.T) {
	m, path := fileManager(t)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := m.writeEntry(config.Connector{ID: "gh", URL: "https://gh.example/", Auth: "none"}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("writeEntry over a directory: %v", err)
	}
}
