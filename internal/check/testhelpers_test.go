package check

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
)

// minimalConfig returns a valid, in-memory config with one provider, one
// model and one agent — enough for every rule to find nothing to complain
// about. Tests mutate a copy of it to trigger the one thing they check.
func minimalConfig() *config.Config {
	return &config.Config{
		Version: config.CurrentVersion,
		Server:  config.Server{},
		Providers: map[string]config.Provider{
			"ollama": {Name: "ollama", Type: "openai-compatible", BaseURL: "http://localhost:11434/v1", Auth: "none"},
		},
		Models: map[string]config.Model{
			"gemma": {Name: "gemma", Provider: "ollama", ID: "gemma4:e2b", Context: 8192},
		},
		Channels: map[string]config.Channel{},
		Agents: map[string]config.Agent{
			"main": {
				Name:   "main",
				Tools:  []string{"memory"},
				Models: config.Roles{Chat: "gemma"},
				Policy: config.Policy{
					Classes: config.Classes{Read: "allow", Write: "allow", External: "allow", Exec: "deny"},
					Guest:   config.Classes{Read: "allow", Write: "deny", External: "deny", Exec: "deny"},
					Task:    config.Classes{Read: "allow", Write: "allow", External: "allow", Exec: "deny"},
				},
				Prompt: "You are Hola.",
			},
		},
	}
}

// writeFileErr writes rel under dir, creating parent directories as needed.
func writeFileErr(dir, rel, content string) error {
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

// openSecrets opens (creating) an auth.toml under dir and returns both the
// handle and its path.
func openSecrets(t *testing.T, dir string) (*secrets.File, string) {
	t.Helper()
	path := filepath.Join(dir, "auth.toml")
	f, err := secrets.Open(path)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	return f, path
}

// hashTree hashes every regular file's relative path, mode and content under
// dir, order-independent — used to prove a rule run touched nothing.
func hashTree(t *testing.T, dir string) string {
	t.Helper()
	type entry struct{ path, sum string }
	var entries []entry
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		info, err := d.Info()
		if err != nil {
			return err
		}
		h := sha256.New()
		if _, err := fmt.Fprintf(h, "%s|%s|", rel, info.Mode()); err != nil {
			return err
		}
		if !d.IsDir() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			if _, err := io.Copy(h, f); err != nil {
				return err
			}
		}
		entries = append(entries, entry{rel, hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	if err != nil {
		t.Fatalf("hashTree: %v", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	all := sha256.New()
	for _, e := range entries {
		if _, err := fmt.Fprintf(all, "%s=%s\n", e.path, e.sum); err != nil {
			t.Fatalf("hashTree: %v", err)
		}
	}
	return hex.EncodeToString(all.Sum(nil))
}
