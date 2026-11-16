package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holaclaw/internal/store"
)

const secretMarker = "SUPER-SECRET-SHOULD-NEVER-LEAVE-9f3a"

// fixture builds a config dir + data dir pair: config.toml, channels.toml,
// one agent, an auth.toml holding secretMarker, a live store (so DataDir has
// a real holaclaw.db and at least one blob).
func fixture(t *testing.T) (configDir, dataDir string, db *store.SQLite) {
	t.Helper()
	configDir = t.TempDir()
	dataDir = t.TempDir()

	write(t, configDir, "config.toml", "version = 1\n")
	write(t, configDir, "channels.toml", "version = 1\n")
	write(t, configDir, "agents/main/agent.toml", "version = 1\ntools = [\"memory\"]\n")
	write(t, configDir, "agents/main/prompt.md", "You are Hola.")
	write(t, configDir, "secrets/auth.toml", "version = 1\n\n[providers.ollama]\napi_key = \""+secretMarker+"\"\n")

	s, err := store.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := s.PutBlob(context.Background(), strings.NewReader("blob content"), "text/plain"); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	return configDir, dataDir, s
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// rawGunzip returns the decompressed tar bytes, for assertions that must see
// past gzip (the "no secret leaked" checks).
func rawGunzip(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	b, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func entries(t *testing.T, path string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(rawGunzip(t, path)))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[hdr.Name] = b
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	configDir, dataDir, db := fixture(t)
	defer func() { _ = db.Close() }()
	out := filepath.Join(t.TempDir(), "backup.tar.gz")

	path, err := Create(context.Background(), Options{
		DataDir: dataDir, ConfigDir: configDir, Out: out, DB: db.DB(),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if path != out {
		t.Fatalf("path = %s, want %s", path, out)
	}
	if err := Verify(path); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	ents := entries(t, path)
	want := []string{"MANIFEST.json", "data/holaclaw.db", "config/config.toml", "config/channels.toml", "config/agents/main/agent.toml"}
	for _, name := range want {
		if _, ok := ents[name]; !ok {
			t.Errorf("archive missing %s; has %v", name, keysOf(ents))
		}
	}
	blobFound := false
	for name := range ents {
		if strings.HasPrefix(name, "data/blobs/") {
			blobFound = true
		}
	}
	if !blobFound {
		t.Errorf("archive has no blob entries: %v", keysOf(ents))
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSecretsExcludedByDefault(t *testing.T) {
	configDir, dataDir, db := fixture(t)
	defer func() { _ = db.Close() }()
	out := filepath.Join(t.TempDir(), "backup.tar.gz")

	path, err := Create(context.Background(), Options{DataDir: dataDir, ConfigDir: configDir, Out: out, DB: db.DB()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ents := entries(t, path)
	if _, ok := ents["config/auth.toml"]; ok {
		t.Error("auth.toml was archived without IncludeSecrets")
	}
	raw := rawGunzip(t, path)
	if bytes.Contains(raw, []byte(secretMarker)) {
		t.Error("secret marker leaked into the archive")
	}
}

func TestSecretsIncludedWhenAsked(t *testing.T) {
	configDir, dataDir, db := fixture(t)
	defer func() { _ = db.Close() }()
	out := filepath.Join(t.TempDir(), "backup.tar.gz")

	path, err := Create(context.Background(), Options{
		DataDir: dataDir, ConfigDir: configDir, Out: out, DB: db.DB(), IncludeSecrets: true,
		SecretsPath: filepath.Join(configDir, "secrets", "auth.toml"),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ents := entries(t, path)
	body, ok := ents["config/auth.toml"]
	if !ok {
		t.Fatal("auth.toml missing though IncludeSecrets was set")
	}
	if !bytes.Contains(body, []byte(secretMarker)) {
		t.Error("auth.toml archived without its secret content")
	}
}

func TestArchiveMode0600(t *testing.T) {
	configDir, dataDir, db := fixture(t)
	defer func() { _ = db.Close() }()
	out := filepath.Join(t.TempDir(), "backup.tar.gz")

	path, err := Create(context.Background(), Options{DataDir: dataDir, ConfigDir: configDir, Out: out, DB: db.DB()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %s, want 0600", info.Mode().Perm())
	}
}

func TestSymlinkNotFollowed(t *testing.T) {
	configDir, dataDir, db := fixture(t)
	defer func() { _ = db.Close() }()

	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("outside content, must never appear"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(configDir, "agents", "main", "escape.md")); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	path, err := Create(context.Background(), Options{DataDir: dataDir, ConfigDir: configDir, Out: out, DB: db.DB()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	ents := entries(t, path)
	if _, ok := ents["config/agents/main/escape.md"]; ok {
		t.Error("archive followed a symlink into a tracked entry")
	}
	raw := rawGunzip(t, path)
	if bytes.Contains(raw, []byte("outside content, must never appear")) {
		t.Error("symlink target content leaked into the archive")
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	configDir, dataDir, db := fixture(t)
	defer func() { _ = db.Close() }()
	out := filepath.Join(t.TempDir(), "backup.tar.gz")

	path, err := Create(context.Background(), Options{DataDir: dataDir, ConfigDir: configDir, Out: out, DB: db.DB()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := Verify(path); err != nil {
		t.Fatalf("Verify on an untouched archive: %v", err)
	}

	raw := rawGunzip(t, path)
	tampered := bytes.Replace(raw, []byte("You are Hola."), []byte("You are Evil."), 1)
	if bytes.Equal(raw, tampered) {
		t.Fatal("fixture content not found to tamper with")
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(tampered); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	tamperedPath := filepath.Join(t.TempDir(), "tampered.tar.gz")
	if err := os.WriteFile(tamperedPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Verify(tamperedPath); err == nil {
		t.Error("Verify did not detect tampering")
	}
}

func TestWALRefusal(t *testing.T) {
	configDir, dataDir, db := fixture(t)
	if err := db.Close(); err != nil { // release the handle: this path exercises the no-live-DB copy path
		t.Fatal(err)
	}

	walPath := filepath.Join(dataDir, "holaclaw.db-wal")
	if err := os.WriteFile(walPath, []byte("pending wal frame"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	_, err := Create(context.Background(), Options{DataDir: dataDir, ConfigDir: configDir, Out: out})
	if err == nil {
		t.Fatal("expected a WAL refusal error")
	}
	if !strings.Contains(err.Error(), "WAL") {
		t.Errorf("error = %v, want it to mention the WAL", err)
	}
}

func TestWALRefusalIgnoresEmptyWAL(t *testing.T) {
	configDir, dataDir, db := fixture(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// An empty -wal file (the common steady state after a clean checkpoint)
	// must not block a backup.
	if err := os.WriteFile(filepath.Join(dataDir, "holaclaw.db-wal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "backup.tar.gz")
	path, err := Create(context.Background(), Options{DataDir: dataDir, ConfigDir: configDir, Out: out})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := Verify(path); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestOutDefaultsToTimestampedName(t *testing.T) {
	configDir, dataDir, db := fixture(t)
	defer func() { _ = db.Close() }()
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldwd) }()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	path, err := Create(context.Background(), Options{DataDir: dataDir, ConfigDir: configDir, DB: db.DB()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(path), "holaclaw-backup-") || !strings.HasSuffix(path, ".tar.gz") {
		t.Errorf("path = %s, want the default holaclaw-backup-<ts>.tar.gz name", path)
	}
}

func TestOutDirectory(t *testing.T) {
	configDir, dataDir, db := fixture(t)
	defer func() { _ = db.Close() }()
	dir := t.TempDir()

	path, err := Create(context.Background(), Options{DataDir: dataDir, ConfigDir: configDir, Out: dir, DB: db.DB()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if filepath.Dir(path) != dir {
		t.Errorf("path = %s, want it under %s", path, dir)
	}
}
