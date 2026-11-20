package check

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"holaclaw/internal/config"
	"holaclaw/internal/provider"
	"holaclaw/internal/provider/openaicompat"
)

func TestMOD001(t *testing.T) {
	t.Run("small window", func(t *testing.T) {
		cfg := minimalConfig()
		m := cfg.Models["gemma"]
		m.Context = 2048
		cfg.Models["gemma"] = m
		fs := mod001Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 1 || fs[0].ID != "MOD001" || fs[0].Severity != SeverityWarning {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("big enough", func(t *testing.T) {
		fs := mod001Rule().Run(context.Background(), &Input{Config: minimalConfig()})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
}

func TestMOD002(t *testing.T) {
	t.Run("nil probe skips the rule", func(t *testing.T) {
		fs := mod002Rule().Run(context.Background(), &Input{Config: minimalConfig(), Probe: nil})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("unsupported: no finding", func(t *testing.T) {
		probe := func(ctx context.Context, name string, m provider.Model) (int, string, error) {
			return 0, "", openaicompat.ErrProbeUnsupported
		}
		fs := mod002Rule().Run(context.Background(), &Input{Config: minimalConfig(), Probe: probe})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("network error: info", func(t *testing.T) {
		probe := func(ctx context.Context, name string, m provider.Model) (int, string, error) {
			return 0, "", errors.New("connection refused")
		}
		fs := mod002Rule().Run(context.Background(), &Input{Config: minimalConfig(), Probe: probe})
		if len(fs) != 1 || fs[0].ID != "MOD002" || fs[0].Severity != SeverityInfo {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("timeout: info", func(t *testing.T) {
		probe := func(ctx context.Context, name string, m provider.Model) (int, string, error) {
			<-ctx.Done()
			return 0, "", ctx.Err()
		}
		start := time.Now()
		fs := mod002Rule().Run(context.Background(), &Input{Config: minimalConfig(), Probe: probe})
		if time.Since(start) > 2*ProbeTimeout {
			t.Fatalf("probe took too long: %v", time.Since(start))
		}
		if len(fs) != 1 || fs[0].ID != "MOD002" || fs[0].Severity != SeverityInfo {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("runtime allocated less: error", func(t *testing.T) {
		probe := func(ctx context.Context, name string, m provider.Model) (int, string, error) {
			return m.Context - 1, "ollama:num_ctx", nil
		}
		fs := mod002Rule().Run(context.Background(), &Input{Config: minimalConfig(), Probe: probe})
		if len(fs) != 1 || fs[0].ID != "MOD002" || fs[0].Severity != SeverityError {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("runtime allocated more or equal: no finding", func(t *testing.T) {
		probe := func(ctx context.Context, name string, m provider.Model) (int, string, error) {
			return m.Context + 1000, "ollama:num_ctx", nil
		}
		fs := mod002Rule().Run(context.Background(), &Input{Config: minimalConfig(), Probe: probe})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("non-openai-compatible provider skipped", func(t *testing.T) {
		cfg := minimalConfig()
		p := cfg.Providers["ollama"]
		p.Type = "codex"
		cfg.Providers["ollama"] = p
		called := false
		probe := func(ctx context.Context, name string, m provider.Model) (int, string, error) {
			called = true
			return 0, "", nil
		}
		fs := mod002Rule().Run(context.Background(), &Input{Config: cfg, Probe: probe})
		if len(fs) != 0 || called {
			t.Fatalf("findings = %+v, called = %v, want none/false", fs, called)
		}
	})
}

func TestMOD003(t *testing.T) {
	// models answers every provider with the same list.
	models := func(live bool, ids ...string) func(context.Context, string) ([]string, bool, error) {
		return func(context.Context, string) ([]string, bool, error) { return ids, live, nil }
	}
	run := func(in *Input) []Finding { return mod003Rule().Run(context.Background(), in) }

	t.Run("nil models skips the rule", func(t *testing.T) {
		if fs := run(&Input{Config: minimalConfig()}); len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("id served: no finding", func(t *testing.T) {
		fs := run(&Input{Config: minimalConfig(), Models: models(true, "qwen3:8b", "gemma4:e2b")})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("id served as latest: no finding", func(t *testing.T) {
		cfg := minimalConfig()
		m := cfg.Models["gemma"]
		m.ID = "gemma4"
		cfg.Models["gemma"] = m
		if fs := run(&Input{Config: cfg, Models: models(true, "gemma4:latest")}); len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("id not served: error", func(t *testing.T) {
		fs := run(&Input{Config: minimalConfig(), Models: models(true, "qwen3:8b", "llama4")})
		if len(fs) != 1 || fs[0].ID != "MOD003" || fs[0].Severity != SeverityError {
			t.Fatalf("findings = %+v", fs)
		}
		if fs[0].Key != "models.gemma.id" || fs[0].Fix == "" {
			t.Errorf("finding = %+v", fs[0])
		}
		for _, want := range []string{`"gemma4:e2b"`, "ollama", "qwen3:8b, llama4"} {
			if !strings.Contains(fs[0].Message, want) {
				t.Errorf("message %q does not name %s", fs[0].Message, want)
			}
		}
	})
	t.Run("id not in a guessed list: warning", func(t *testing.T) {
		fs := run(&Input{Config: minimalConfig(), Models: models(false, "gpt-6.1-sol")})
		if len(fs) != 1 || fs[0].ID != "MOD003" || fs[0].Severity != SeverityWarning {
			t.Fatalf("findings = %+v", fs)
		}
		if fs[0].Key != "models.gemma.id" || !strings.Contains(fs[0].Message, "gpt-6.1-sol") {
			t.Errorf("finding = %+v", fs[0])
		}
	})
	t.Run("no model list: no finding", func(t *testing.T) {
		fs := run(&Input{Config: minimalConfig(), Models: func(context.Context, string) ([]string, bool, error) {
			return nil, false, fmt.Errorf("codex: %w", ErrNoModelList)
		}})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("network error: info", func(t *testing.T) {
		fs := run(&Input{Config: minimalConfig(), Models: func(context.Context, string) ([]string, bool, error) {
			return nil, false, errors.New("connection refused")
		}})
		if len(fs) != 1 || fs[0].ID != "MOD003" || fs[0].Severity != SeverityInfo || fs[0].Key != "providers.ollama" {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("timeout: info", func(t *testing.T) {
		start := time.Now()
		fs := run(&Input{Config: minimalConfig(), Models: func(ctx context.Context, _ string) ([]string, bool, error) {
			<-ctx.Done()
			return nil, false, ctx.Err()
		}})
		if time.Since(start) > 2*ProbeTimeout {
			t.Fatalf("listing took too long: %v", time.Since(start))
		}
		if len(fs) != 1 || fs[0].Severity != SeverityInfo {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("a provider is asked once, a model with its own url is left out", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Models["other"] = config.Model{Name: "other", Provider: "ollama", ID: "missing-a"}
		cfg.Models["decide"] = config.Model{Name: "decide", Provider: "ollama", ID: "missing-b", URL: "http://localhost:9000/decide"}
		cfg.Models["orphan"] = config.Model{Name: "orphan", Provider: "gone", ID: "missing-c"}
		var calls atomic.Int32
		fs := run(&Input{Config: cfg, Models: func(_ context.Context, name string) ([]string, bool, error) {
			calls.Add(1)
			if name != "ollama" {
				t.Errorf("asked for provider %q", name)
			}
			return []string{"gemma4:e2b"}, true, nil
		}})
		if calls.Load() != 1 {
			t.Errorf("calls = %d, want 1", calls.Load())
		}
		if len(fs) != 1 || fs[0].Key != "models.other.id" {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("a long list is cut", func(t *testing.T) {
		var ids []string
		for i := range maxListedModels + 5 {
			ids = append(ids, fmt.Sprintf("model-%02d", i))
		}
		fs := run(&Input{Config: minimalConfig(), Models: models(true, ids...)})
		if len(fs) != 1 {
			t.Fatalf("findings = %+v", fs)
		}
		msg := fs[0].Message
		if !strings.Contains(msg, "model-19 and 5 more") || strings.Contains(msg, "model-20") {
			t.Errorf("message = %q", msg)
		}
	})
}
