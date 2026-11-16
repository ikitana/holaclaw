package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func shipped(t *testing.T) []Model {
	t.Helper()
	ms, err := Load(filepath.Join("..", "..", "..", "..", "testdata", "eval", "models.toml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return ms
}

func TestLoadShippedModels(t *testing.T) {
	ms := shipped(t)
	if len(ms) != 6 {
		t.Fatalf("got %d models, want 6", len(ms))
	}
	paid := 0
	for _, m := range ms {
		if m.KeyEnv != "OPENROUTER_KEY" {
			t.Errorf("%s: key_env = %q", m.Name, m.KeyEnv)
		}
		if !strings.HasPrefix(m.BaseURL, "https://openrouter.ai/") {
			t.Errorf("%s: base_url = %q", m.Name, m.BaseURL)
		}
		if m.Context <= 0 {
			t.Errorf("%s: context %d", m.Name, m.Context)
		}
		// The guard classifier answers one word and is never given tools; every
		// other model drives the agent loop and needs native tool calling.
		if !m.Tools && m.Name != "jev-guard" {
			t.Errorf("%s: tools = false", m.Name)
		}
		if m.Paid {
			paid++
		}
	}
	if paid != 2 {
		t.Fatalf("got %d paid models, want exactly 2", paid)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"empty", "", "no models"},
		{"no name", "[[models]]\nbase_url=\"u\"\nid=\"i\"\n", "empty name"},
		{"no base_url", "[[models]]\nname=\"m\"\nid=\"i\"\n", "no base_url"},
		{"no id", "[[models]]\nname=\"m\"\nbase_url=\"u\"\n", "no id"},
		{"duplicate", "[[models]]\nname=\"m\"\nbase_url=\"u\"\nid=\"i\"\n[[models]]\nname=\"m\"\nbase_url=\"u\"\nid=\"i\"\n", "twice"},
		{"bad toml", "[[models]", "parse"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "models.toml")
			if err := os.WriteFile(p, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(p)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, c.want)
			}
		})
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.toml")); err == nil {
		t.Fatal("want an error for a missing file")
	}
}

func TestSkipWithoutKey(t *testing.T) {
	m := Model{Name: "m", KeyEnv: "EVAL_TEST_KEY"}
	t.Setenv("EVAL_TEST_KEY", "")
	if s := m.Skip(); !strings.Contains(s, "EVAL_TEST_KEY") {
		t.Fatalf("Skip = %q, want it to name the variable", s)
	}
	t.Setenv("EVAL_TEST_KEY", "secret-value")
	if s := m.Skip(); s != "" {
		t.Fatalf("Skip = %q, want none", s)
	}
	if strings.Contains(m.Skip(), "secret-value") {
		t.Fatal("the key must never appear in a message")
	}
	if s := (Model{Name: "local"}).Skip(); s != "" {
		t.Fatalf("a keyless endpoint must not be skipped, got %q", s)
	}
}

func TestSpec(t *testing.T) {
	m := Model{Name: "m", ID: "vendor/model", Context: 32768, Tools: true, Strict: true,
		ThinkingTags: []string{"think"}, SchemaProfile: "llamacpp"}
	s := m.Spec(8192, 30*time.Second)
	if s.Context != 8192 || s.ID != "vendor/model" || s.Provider != "eval" ||
		!s.Tools || !s.Strict || s.SchemaProfile != "llamacpp" || s.Timeout != 30*time.Second {
		t.Fatalf("spec = %+v", s)
	}
	if got := m.Spec(0, 0).Context; got != 32768 {
		t.Fatalf("window 0 must fall back to the model context, got %d", got)
	}
}

func TestProviderUsesTheEnvironment(t *testing.T) {
	m := Model{Name: "m", BaseURL: "http://localhost:1/v1", KeyEnv: "EVAL_TEST_KEY"}
	t.Setenv("EVAL_TEST_KEY", "k")
	if p := m.Provider(); p == nil || p.Name() != "m" {
		t.Fatalf("provider = %v", p)
	}
}
