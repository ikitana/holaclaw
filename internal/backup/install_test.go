package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/store"
)

// archiveOf backs the fixture installation up, through its live handle, and
// closes it: what an operator carries to another host.
func archiveOf(t *testing.T, withSecrets bool) (archive, configDir string) {
	t.Helper()
	configDir, dataDir, s := fixture(t)
	archive, err := Create(context.Background(), Options{
		DataDir: dataDir, ConfigDir: configDir, SecretsPath: filepath.Join(configDir, "secrets", "auth.toml"),
		Out: filepath.Join(t.TempDir(), "backup.tar.gz"), IncludeSecrets: withSecrets, DB: s.DB(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return archive, configDir
}

// plan inspects an archive and plans its install into t.
func plan(t *testing.T, archive string, target Target) *Plan {
	t.Helper()
	m, _, err := Inspect(archive)
	if err != nil {
		t.Fatal(err)
	}
	p, err := PlanInstall(m, target, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func actions(p *Plan) map[string]Action {
	out := map[string]Action{}
	for _, c := range p.Changes {
		out[c.Path] = c.Action
	}
	return out
}

func read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// A host that has nothing: every directory is created, and what comes out is
// an installation the store opens.
func TestInstallOnAFreshHost(t *testing.T) {
	archive, source := archiveOf(t, true)
	root := t.TempDir()
	target := Target{
		ConfigDir: filepath.Join(root, "etc"), DataDir: filepath.Join(root, "var", "data"),
		SecretsPath: filepath.Join(root, "secrets", "auth.toml"),
	}

	m, cfg, err := Inspect(archive)
	if err != nil || string(cfg) != "version = 1\n" {
		t.Fatalf("Inspect: config.toml = %q, %v", cfg, err)
	}
	p, err := PlanInstall(m, target, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !p.Secrets || len(p.Dirs) != 3 || p.Blobs != 1 || p.NewBlobs != 1 || len(p.Changes) != 6 {
		t.Fatalf("plan = %+v", p)
	}
	for _, c := range p.Changes {
		if c.Action != ActionCreate || c.Kept != "" {
			t.Errorf("%s: %s, kept %q", c.Path, c.Action, c.Kept)
		}
	}
	if left, _ := os.ReadDir(root); len(left) != 0 {
		t.Fatalf("planning wrote %d entries", len(left))
	}

	if err := Install(archive, p); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"config.toml", "channels.toml", "agents/main/agent.toml", "agents/main/prompt.md"} {
		if got, want := read(t, filepath.Join(target.ConfigDir, rel)), read(t, filepath.Join(source, rel)); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	if !strings.Contains(read(t, target.SecretsPath), secretMarker) {
		t.Error("auth.toml was not restored")
	}
	if _, err := os.Stat(filepath.Join(target.ConfigDir, "auth.toml")); !os.IsNotExist(err) {
		t.Errorf("auth.toml landed in the config directory (%v)", err)
	}
	for _, path := range []string{target.SecretsPath, filepath.Join(target.ConfigDir, "config.toml"), filepath.Join(target.DataDir, dbFileName)} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, %v", path, info, err)
		}
	}
	for _, name := range []string{restoreDirName, restoreTmpName, dbFileName + previousDBSuffix} {
		if _, err := os.Lstat(filepath.Join(target.DataDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s is left in the data directory (%v)", name, err)
		}
	}
	s, err := store.Open(context.Background(), target.DataDir)
	if err != nil {
		t.Fatalf("the restored database does not open: %v", err)
	}
	_ = s.Close()
	blobs := 0
	_ = filepath.WalkDir(filepath.Join(target.DataDir, "blobs"), func(_ string, d os.DirEntry, _ error) error {
		if d != nil && d.Type().IsRegular() {
			blobs++
		}
		return nil
	})
	if blobs != 1 {
		t.Errorf("%d blobs restored, want 1", blobs)
	}

	// Again, over what it just wrote: only the database is replaced.
	p = plan(t, archive, target)
	for path, a := range actions(p) {
		if want := map[bool]Action{true: ActionReplace, false: ActionKeep}[path == filepath.Join(target.DataDir, dbFileName)]; a != want {
			t.Errorf("second plan: %s is %s, want %s", path, a, want)
		}
	}
	if len(p.Dirs) != 0 || p.NewBlobs != 0 {
		t.Errorf("second plan = %+v", p)
	}
}

// An installation already there: what differs is replaced and kept, what the
// archive does not hold is left alone.
func TestInstallOverAnInstallation(t *testing.T) {
	archive, _ := archiveOf(t, true)
	configDir, dataDir, s := fixture(t)
	if _, err := s.DB().Exec(`CREATE TABLE after_backup (n INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(configDir, "secrets", "auth.toml")
	write(t, configDir, "config.toml", "version = 1\n# edited since\n")
	write(t, configDir, "agents/main/prompt.md", "You are somebody else.")
	write(t, configDir, "agents/other/prompt.md", "Not in the backup.")
	write(t, configDir, "secrets/auth.toml", "version = 1\n# other credentials\n")
	target := Target{ConfigDir: configDir, DataDir: dataDir, SecretsPath: secrets}

	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	m, _, err := Inspect(archive)
	if err != nil {
		t.Fatal(err)
	}
	p, err := PlanInstall(m, target, now)
	if err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(configDir, ".backup-20261008T090000Z")
	want := map[string]Action{
		filepath.Join(configDir, "config.toml"):                  ActionReplace,
		filepath.Join(configDir, "channels.toml"):                ActionKeep,
		filepath.Join(configDir, "agents", "main", "agent.toml"): ActionKeep,
		filepath.Join(configDir, "agents", "main", "prompt.md"):  ActionReplace,
		secrets:                            ActionReplace,
		filepath.Join(dataDir, dbFileName): ActionReplace,
	}
	got := actions(p)
	if len(got) != len(want) {
		t.Fatalf("plan = %+v", p.Changes)
	}
	for path, a := range want {
		if got[path] != a {
			t.Errorf("%s is %q, want %q", path, got[path], a)
		}
	}
	if len(p.Dirs) != 0 {
		t.Errorf("directories to create: %v", p.Dirs)
	}

	if err := Install(archive, p); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(configDir, "config.toml")); got != "version = 1\n" {
		t.Errorf("config.toml = %q", got)
	}
	if got := read(t, filepath.Join(kept, "config.toml")); !strings.Contains(got, "edited since") {
		t.Errorf("the replaced config.toml was kept as %q", got)
	}
	if got := read(t, filepath.Join(kept, "agents", "main", "prompt.md")); got != "You are somebody else." {
		t.Errorf("the replaced prompt was kept as %q", got)
	}
	if _, err := os.Stat(filepath.Join(kept, "channels.toml")); !os.IsNotExist(err) {
		t.Errorf("an unchanged file was kept too (%v)", err)
	}
	if got := read(t, filepath.Join(configDir, "agents", "other", "prompt.md")); got != "Not in the backup." {
		t.Errorf("a file the archive does not hold is now %q", got)
	}
	if !strings.Contains(read(t, secrets), secretMarker) || !strings.Contains(read(t, secrets+previousDBSuffix), "other credentials") {
		t.Error("auth.toml was not replaced and kept")
	}
	if _, err := os.Stat(filepath.Join(dataDir, dbFileName+previousDBSuffix)); err != nil {
		t.Errorf("the replaced database was not kept: %v", err)
	}
	st, err := store.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if probe(t, st) {
		t.Error("the database in place is still the one from after the backup")
	}
}

// Without secrets in the archive, the auth.toml of the host is not touched.
func TestInstallWithoutSecrets(t *testing.T) {
	archive, _ := archiveOf(t, false)
	root := t.TempDir()
	target := Target{ConfigDir: filepath.Join(root, "etc"), DataDir: filepath.Join(root, "data"), SecretsPath: filepath.Join(root, "secrets", "auth.toml")}
	write(t, root, "secrets/auth.toml", "mine")

	p := plan(t, archive, target)
	if p.Secrets || len(p.Dirs) != 2 {
		t.Fatalf("plan = %+v", p)
	}
	if err := Install(archive, p); err != nil {
		t.Fatal(err)
	}
	if got := read(t, target.SecretsPath); got != "mine" {
		t.Errorf("auth.toml = %q", got)
	}
}

func TestInstallRefusals(t *testing.T) {
	root := t.TempDir()
	target := Target{ConfigDir: filepath.Join(root, "etc"), DataDir: filepath.Join(root, "data"), SecretsPath: filepath.Join(root, "secrets", "auth.toml")}
	save := func(body []byte) string {
		path := filepath.Join(t.TempDir(), "a.tar.gz")
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// Not an archive, and one whose bytes do not match its manifest.
	if _, _, err := Inspect(save([]byte("junk"))); err == nil {
		t.Error("Inspect accepted junk")
	}
	if _, _, err := Inspect(filepath.Join(root, "missing.tar.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Inspect of a missing file: %v", err)
	}

	for name, files := range map[string][][2]string{
		"no database":            {{"config/config.toml", "version = 1\n"}},
		"a stray config file":    {{entryDB, "x"}, {"config/run.sh", "#!/bin/sh"}},
		"a path out of agents":   {{entryDB, "x"}, {"config/agents/../../escape", "x"}},
		"a path out of blobs":    {{entryDB, "x"}, {"data/blobs/../../escape", "x"}},
		"a file of another kind": {{entryDB, "x"}, {"other/file", "x"}},
	} {
		m, _, err := Inspect(save(craft(t, files...)))
		if err != nil {
			t.Fatalf("%s: Inspect: %v", name, err)
		}
		if _, err := PlanInstall(m, target, time.Now()); !errors.Is(err, ErrBadArchive) {
			t.Errorf("%s: PlanInstall: %v", name, err)
		}
	}

	// A database that is not one is found before any config file is written.
	bad := save(craft(t, [2]string{entryDB, "not sqlite"}, [2]string{"config/config.toml", "version = 1\n"}))
	p := plan(t, bad, target)
	if err := Install(bad, p); !errors.Is(err, ErrBadArchive) {
		t.Errorf("Install of a bad database: %v", err)
	}
	if _, err := os.Stat(target.ConfigDir); !os.IsNotExist(err) {
		t.Errorf("a refused install wrote the config directory (%v)", err)
	}
	if left, _ := os.ReadDir(target.DataDir); len(left) != 0 {
		t.Errorf("a refused install left %d entries in the data directory", len(left))
	}

	// A restore a daemon staged is its to finish.
	archive, _ := archiveOf(t, false)
	if err := os.MkdirAll(filepath.Join(target.DataDir, restoreDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	m, _, err := Inspect(archive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PlanInstall(m, target, time.Now()); err == nil || !strings.Contains(err.Error(), "already staged") {
		t.Errorf("PlanInstall over a staged restore: %v", err)
	}
}

// A config file that cannot be written stops the install before the data is
// touched.
func TestInstallStopsBeforeTheDataWhenTheConfigFails(t *testing.T) {
	archive, _ := archiveOf(t, false)
	configDir, dataDir, s := fixture(t)
	if _, err := s.DB().Exec(`CREATE TABLE after_backup (n INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	target := Target{ConfigDir: configDir, DataDir: dataDir, SecretsPath: filepath.Join(configDir, "secrets", "auth.toml")}
	write(t, configDir, "config.toml", "version = 1\n# edited since\n")
	p := plan(t, archive, target)
	// The place replaced files are kept in is taken by a file.
	for _, c := range p.Changes {
		if c.Action == ActionReplace && c.Kept != "" && strings.HasPrefix(c.Kept, configDir) {
			if err := os.WriteFile(filepath.Dir(c.Kept), []byte("in the way"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := Install(archive, p); err == nil {
		t.Fatal("Install went through")
	}
	if got := read(t, filepath.Join(configDir, "config.toml")); !strings.Contains(got, "edited since") {
		t.Errorf("config.toml = %q", got)
	}
	for _, name := range []string{restoreDirName, restoreTmpName, dbFileName + previousDBSuffix} {
		if _, err := os.Lstat(filepath.Join(dataDir, name)); !os.IsNotExist(err) {
			t.Errorf("%s is left in the data directory (%v)", name, err)
		}
	}
	st, err := store.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if !probe(t, st) {
		t.Error("the database was replaced")
	}
}

// connectors.toml is a config file like channels.toml: a backup carries it
// and an install puts it back (docs/connectors.md §4).
func TestBackupCarriesConnectors(t *testing.T) {
	const connectors = "version = 1\n\n[connectors.homes]\nurl = \"https://homes.example.com/mcp\"\nauth = \"none\"\n"
	configDir, dataDir, s := fixture(t)
	write(t, configDir, "connectors.toml", connectors)
	archive, err := Create(context.Background(), Options{
		DataDir: dataDir, ConfigDir: configDir, SecretsPath: filepath.Join(configDir, "secrets", "auth.toml"),
		Out: filepath.Join(t.TempDir(), "backup.tar.gz"), DB: s.DB(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	target := Target{ConfigDir: filepath.Join(root, "etc"), DataDir: filepath.Join(root, "data"), SecretsPath: filepath.Join(root, "secrets", "auth.toml")}
	p := plan(t, archive, target)
	if a := actions(p)[filepath.Join(target.ConfigDir, "connectors.toml")]; a != ActionCreate {
		t.Fatalf("connectors.toml: %q in plan %+v", a, p.Changes)
	}
	if err := Install(archive, p); err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(target.ConfigDir, "connectors.toml")); got != connectors {
		t.Errorf("connectors.toml = %q", got)
	}
}
