package check

import (
	"context"
	"testing"

	"holaclaw/internal/config"
)

func TestACC001(t *testing.T) {
	t.Run("open dm with permissive guest exec", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Channels["tg-main"] = config.Channel{
			Name: "tg-main", Type: "telegram", Agent: "main",
			Access: config.Access{DM: "open", Rooms: "off", Reply: "mention"},
		}
		a := cfg.Agents["main"]
		a.Policy.Guest.Exec = "ask"
		cfg.Agents["main"] = a
		fs := acc001Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 1 || fs[0].ID != "ACC001" || fs[0].Severity != SeverityError {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("open dm with guest external allowed", func(t *testing.T) {
		// Strangers could spend the owner's money on images and fetches.
		cfg := minimalConfig()
		cfg.Channels["tg-main"] = config.Channel{
			Name: "tg-main", Type: "telegram", Agent: "main",
			Access: config.Access{DM: "open", Rooms: "off", Reply: "mention"},
		}
		a := cfg.Agents["main"]
		a.Policy.Guest.External = "allow"
		cfg.Agents["main"] = a
		if fs := acc001Rule().Run(context.Background(), &Input{Config: cfg}); len(fs) != 1 {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("open dm with deny-deny guest", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Channels["tg-main"] = config.Channel{
			Name: "tg-main", Type: "telegram", Agent: "main",
			Access: config.Access{DM: "open", Rooms: "off", Reply: "mention"},
		}
		fs := acc001Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("invite dm never flagged", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Channels["tg-main"] = config.Channel{
			Name: "tg-main", Type: "telegram", Agent: "main",
			Access: config.Access{DM: "invite", Rooms: "off", Reply: "mention"},
		}
		a := cfg.Agents["main"]
		a.Policy.Guest.Exec = "allow"
		cfg.Agents["main"] = a
		fs := acc001Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
}

func TestACC002(t *testing.T) {
	t.Run("always plus open", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Channels["tg-main"] = config.Channel{
			Name: "tg-main", Type: "telegram", Agent: "main",
			Access: config.Access{DM: "invite", Rooms: "open", Reply: "always"},
		}
		fs := acc002Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 1 || fs[0].ID != "ACC002" || fs[0].Severity != SeverityWarning {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("mention plus open ok", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Channels["tg-main"] = config.Channel{
			Name: "tg-main", Type: "telegram", Agent: "main",
			Access: config.Access{DM: "invite", Rooms: "open", Reply: "mention"},
		}
		fs := acc002Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
}

func TestACC003(t *testing.T) {
	t.Run("googlechat with reply always", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Channels["gc-main"] = config.Channel{
			Name: "gc-main", Type: "googlechat", Agent: "main",
			Access: config.Access{DM: "invite", Rooms: "approved", Reply: "always"},
		}
		fs := acc003Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 1 || fs[0].ID != "ACC003" || fs[0].Severity != SeverityWarning {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("googlechat with reply mention ok", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Channels["gc-main"] = config.Channel{
			Name: "gc-main", Type: "googlechat", Agent: "main",
			Access: config.Access{DM: "invite", Rooms: "approved", Reply: "mention"},
		}
		fs := acc003Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("telegram with reply always not flagged", func(t *testing.T) {
		cfg := minimalConfig()
		cfg.Channels["tg-main"] = config.Channel{
			Name: "tg-main", Type: "telegram", Agent: "main",
			Access: config.Access{DM: "invite", Rooms: "off", Reply: "always"},
		}
		fs := acc003Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
}
