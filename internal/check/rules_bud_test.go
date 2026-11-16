package check

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"holaclaw/internal/config"
	"holaclaw/internal/provider"
)

func TestBUD001(t *testing.T) {
	t.Run("fits comfortably", func(t *testing.T) {
		fs := bud001Rule().Run(context.Background(), &Input{Config: minimalConfig()})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("large persona: warning", func(t *testing.T) {
		cfg := minimalConfig()
		a := cfg.Agents["main"]
		a.Prompt = strings.Repeat("word ", 1500) // over the 1500-token target, under 1/4 of 8192
		cfg.Agents["main"] = a
		fs := bud001Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 1 || fs[0].ID != "BUD001" || fs[0].Severity != SeverityWarning {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("tiny window: error", func(t *testing.T) {
		cfg := minimalConfig()
		m := cfg.Models["gemma"]
		m.Context = 4096
		cfg.Models["gemma"] = m
		a := cfg.Agents["main"]
		a.Prompt = strings.Repeat("word ", 2000)
		cfg.Agents["main"] = a
		fs := bud001Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 1 || fs[0].ID != "BUD001" || fs[0].Severity != SeverityError {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("uses Specs for tool schema cost", func(t *testing.T) {
		spec := provider.ToolSpec{Name: "memory.save", Description: "save a note", Parameters: json.RawMessage(`{"type":"object"}`)}
		cfg := minimalConfig()
		fs := bud001Rule().Run(context.Background(), &Input{
			Config: cfg,
			Specs:  func(agent string) []provider.ToolSpec { return []provider.ToolSpec{spec} },
		})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("connector schemas left out", func(t *testing.T) {
		// Over the 150-token schema cap and over the 1500-token target, but
		// a granted connector's: BUD002's business, not BUD001's.
		cfg := grantWiki(minimalConfig())
		fs := bud001Rule().Run(context.Background(), &Input{Config: cfg, Specs: connectorSpecs(4)})
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
		// The same schema not granted is a built-in over the cap.
		fs = bud001Rule().Run(context.Background(), &Input{Config: minimalConfig(), Specs: connectorSpecs(4)})
		if len(fs) != 1 || fs[0].Severity != SeverityError || !strings.Contains(fs[0].Message, "tool schema wiki.op0") {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("missing chat model reference skipped", func(t *testing.T) {
		cfg := minimalConfig()
		a := cfg.Agents["main"]
		a.Models.Chat = "no-such-model"
		cfg.Agents["main"] = a
		fs := bud001Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
}

// grantWiki grants the connector wiki to main.
func grantWiki(cfg *config.Config) *config.Config {
	a := cfg.Agents["main"]
	a.Connectors = []string{"wiki"}
	cfg.Agents["main"] = a
	return cfg
}

// connectorSpecs is n wiki tools of about 500 tokens each.
func connectorSpecs(n int) func(string) []provider.ToolSpec {
	return func(string) []provider.ToolSpec {
		var out []provider.ToolSpec
		for i := range n {
			out = append(out, provider.ToolSpec{
				Name:        fmt.Sprintf("wiki.op%d", i),
				Description: strings.Repeat("word ", 490),
				Parameters:  json.RawMessage(`{"type":"object"}`),
			})
		}
		return out
	}
}

func TestBUD002(t *testing.T) {
	// The gemma window is 8192: a quarter is 2048.
	t.Run("under a quarter", func(t *testing.T) {
		fs := bud002Rule().Run(context.Background(), &Input{Config: grantWiki(minimalConfig()), Specs: connectorSpecs(3)})
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("over a quarter: warning", func(t *testing.T) {
		fs := bud002Rule().Run(context.Background(), &Input{Config: grantWiki(minimalConfig()), Specs: connectorSpecs(5)})
		if len(fs) != 1 || fs[0].ID != "BUD002" || fs[0].Severity != SeverityWarning || fs[0].Key != "connectors" {
			t.Fatalf("findings = %+v", fs)
		}
		for _, want := range []string{"connector tools are 2", "1/4", "gemma", "8192"} {
			if !strings.Contains(fs[0].Message, want) {
				t.Errorf("message %q lacks %q", fs[0].Message, want)
			}
		}
		if !strings.Contains(fs[0].Fix, "grant fewer connectors") {
			t.Errorf("fix = %q", fs[0].Fix)
		}
		// BUD001 stays quiet: connector tokens are not fixed overhead.
		if fs := bud001Rule().Run(context.Background(), &Input{Config: grantWiki(minimalConfig()), Specs: connectorSpecs(5)}); len(fs) != 0 {
			t.Fatalf("BUD001 findings = %+v", fs)
		}
	})
	t.Run("id that is a built-in group is not a connector", func(t *testing.T) {
		cfg := minimalConfig()
		a := cfg.Agents["main"]
		a.Connectors = []string{"memory"}
		cfg.Agents["main"] = a
		big := func(string) []provider.ToolSpec {
			return []provider.ToolSpec{{Name: "memory.big", Description: strings.Repeat("word ", 300), Parameters: json.RawMessage(`{"type":"object"}`)}}
		}
		in := &Input{Config: cfg, Specs: big, KnownTools: []string{"memory.save", "fetch"}}
		fs := bud001Rule().Run(context.Background(), in)
		if len(fs) != 1 || !strings.Contains(fs[0].Message, "tool schema memory.big") {
			t.Fatalf("BUD001 findings = %+v", fs)
		}
		if got := GrantedConnectors(in.KnownTools, []string{"memory", "fetch", "wiki", "memory.save"}); len(got) != 1 || got[0] != "wiki" {
			t.Fatalf("GrantedConnectors = %v", got)
		}
	})
	t.Run("not granted: no connector tokens", func(t *testing.T) {
		fs := bud002Rule().Run(context.Background(), &Input{Config: minimalConfig()})
		if len(fs) != 0 {
			t.Fatalf("findings = %+v, want none", fs)
		}
	})
	t.Run("failed build left to BUD001", func(t *testing.T) {
		cfg := grantWiki(minimalConfig())
		m := cfg.Models["gemma"]
		m.Context = 4096
		cfg.Models["gemma"] = m
		in := &Input{Config: cfg, Specs: connectorSpecs(8)}
		if fs := bud002Rule().Run(context.Background(), in); len(fs) != 0 {
			t.Fatalf("BUD002 findings = %+v, want none", fs)
		}
		fs := bud001Rule().Run(context.Background(), in)
		if len(fs) != 1 || !strings.Contains(fs[0].Message, "window too small") {
			t.Fatalf("BUD001 findings = %+v", fs)
		}
	})
}
