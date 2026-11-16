package check

import (
	"context"
	"testing"
)

func TestPOL001(t *testing.T) {
	t.Run("allow", func(t *testing.T) {
		cfg := minimalConfig()
		a := cfg.Agents["main"]
		a.Policy.Exec = "allow"
		cfg.Agents["main"] = a
		fs := pol001Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 1 || fs[0].ID != "POL001" || fs[0].Severity != SeverityWarning {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("deny", func(t *testing.T) {
		fs := pol001Rule().Run(context.Background(), &Input{Config: minimalConfig()})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
}

func TestPOL002(t *testing.T) {
	t.Run("lists exec while denied", func(t *testing.T) {
		cfg := minimalConfig()
		a := cfg.Agents["main"]
		a.Tools = append(a.Tools, "exec")
		cfg.Agents["main"] = a
		fs := pol002Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 1 || fs[0].ID != "POL002" || fs[0].Severity != SeverityInfo {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("no exec tool", func(t *testing.T) {
		fs := pol002Rule().Run(context.Background(), &Input{Config: minimalConfig()})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("exec allowed, tool listed: not this rule's concern", func(t *testing.T) {
		cfg := minimalConfig()
		a := cfg.Agents["main"]
		a.Tools = append(a.Tools, "exec")
		a.Policy.Exec = "ask"
		cfg.Agents["main"] = a
		fs := pol002Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
}
