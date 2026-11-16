package check

import (
	"context"
	"errors"
	"testing"
	"time"

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
