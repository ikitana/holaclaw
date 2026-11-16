// Package model loads the eval model list: OpenAI-compatible endpoints the
// harness may drive (tools/eval/README.md). API keys are read from the
// environment, never from the file, and never printed or recorded.
package model

import (
	"fmt"
	"os"
	"time"

	"github.com/pelletier/go-toml/v2"

	"holaclaw/internal/provider"
	"holaclaw/internal/provider/decisions"
	"holaclaw/internal/provider/openaicompat"
)

// Model is one entry of models.toml.
type Model struct {
	Name          string   `toml:"name"`     // short config name, used in output and recording paths
	BaseURL       string   `toml:"base_url"` // e.g. "https://openrouter.ai/api/v1"
	ID            string   `toml:"id"`       // model id on the wire
	Context       int      `toml:"context"`
	Tools         bool     `toml:"tools"`
	Strict        bool     `toml:"strict"`
	SchemaProfile string   `toml:"schema_profile"`
	ThinkingTags  []string `toml:"thinking_tags"`
	KeyEnv        string   `toml:"key_env"` // env var holding the API key; empty for keyless endpoints
	Paid          bool     `toml:"paid"`    // excluded unless --paid
	Sort          string   `toml:"sort"`    // gateway routing preference (OpenRouter provider.sort), e.g. "throughput"
}

type file struct {
	Models []Model `toml:"models"`
}

// Load reads models.toml.
func Load(path string) ([]Model, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: read models: %w", err)
	}
	var f file
	if err := toml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("eval: parse %s: %w", path, err)
	}
	if len(f.Models) == 0 {
		return nil, fmt.Errorf("eval: no models in %s", path)
	}
	seen := map[string]bool{}
	for _, m := range f.Models {
		switch {
		case m.Name == "":
			return nil, fmt.Errorf("eval: %s: model with empty name", path)
		case seen[m.Name]:
			return nil, fmt.Errorf("eval: %s: model %q listed twice", path, m.Name)
		case m.BaseURL == "":
			return nil, fmt.Errorf("eval: %s: model %s has no base_url", path, m.Name)
		case m.ID == "":
			return nil, fmt.Errorf("eval: %s: model %s has no id", path, m.Name)
		}
		seen[m.Name] = true
	}
	return f.Models, nil
}

// Spec is the provider.Model the loop runs with. window overrides the
// model's own context when the scenario asks for a smaller one.
func (m Model) Spec(window int, timeout time.Duration) provider.Model {
	if window <= 0 {
		window = m.Context
	}
	return provider.Model{
		Name:          m.Name,
		Provider:      "eval",
		ID:            m.ID,
		Context:       window,
		Tools:         m.Tools,
		Strict:        m.Strict,
		ThinkingTags:  m.ThinkingTags,
		SchemaProfile: m.SchemaProfile,
		Timeout:       timeout,
	}
}

// Skip reports why this model cannot be driven live, or "" if it can. The key
// itself is never part of the message.
func (m Model) Skip() string {
	if m.KeyEnv != "" && os.Getenv(m.KeyEnv) == "" {
		return "no API key: " + m.KeyEnv + " is not set"
	}
	return ""
}

// Provider builds the live chat client for this model. The key is read per
// request so a rotated secret is picked up, and it never reaches a log or a
// recording.
func (m Model) Provider() provider.Provider {
	keyEnv := m.KeyEnv
	return openaicompat.New(openaicompat.Options{
		Name:    m.Name,
		BaseURL: m.BaseURL,
		APIKey: func() string {
			if keyEnv == "" {
				return ""
			}
			return os.Getenv(keyEnv)
		},
		MaxConcurrent: 1,
		Sort:          m.Sort,
	})
}

// Decider builds the live decisions client for this model (`eval guard
// --api decisions`, docs/models.md "Decision models"). The endpoint is not
// base_url: internal/provider/decisions is EXPERIMENTAL and reads it from
// provider.Model.URL on every request instead, which is why the caller
// passes --url rather than adding a base_url to models.toml.
func (m Model) Decider() provider.Decider {
	keyEnv := m.KeyEnv
	return decisions.New(decisions.Options{
		Name: m.Name,
		APIKey: func() string {
			if keyEnv == "" {
				return ""
			}
			return os.Getenv(keyEnv)
		},
		MaxConcurrent: 1,
	})
}
