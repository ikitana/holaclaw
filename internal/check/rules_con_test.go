package check

import (
	"context"
	"errors"
	"strings"
	"testing"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
	"holaclaw/internal/store"
)

// withConnectors returns minimalConfig plus the given connectors.
func withConnectors(cs ...config.Connector) *config.Config {
	cfg := minimalConfig()
	cfg.Connectors = map[string]config.Connector{}
	for _, c := range cs {
		cfg.Connectors[c.ID] = c
	}
	return cfg
}

func TestCON001(t *testing.T) {
	cfg := withConnectors(
		config.Connector{ID: "open", URL: "https://open.example/mcp", Auth: "none"},
		config.Connector{ID: "gh", URL: "https://gh.example/mcp", Auth: "oauth"},
	)
	t.Run("no grant", func(t *testing.T) {
		fs := con001Rule().Run(context.Background(), &Input{Config: cfg})
		if len(fs) != 1 || fs[0].ID != "CON001" || fs[0].Severity != SeverityError ||
			fs[0].File != config.FileConnectors || fs[0].Key != "connectors.gh.auth" ||
			!strings.Contains(fs[0].Message, `"gh"`) || fs[0].Fix != "run `holaclaw connectors reconnect gh`" {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("client secret alone is not a grant", func(t *testing.T) {
		f, _ := openSecrets(t, t.TempDir())
		if err := f.Set(secrets.Connectors, "gh", map[string]string{secrets.ConnectorClientSecret: "x"}); err != nil {
			t.Fatal(err)
		}
		fs := con001Rule().Run(context.Background(), &Input{Config: cfg, Secrets: f})
		if len(fs) != 1 || fs[0].Key != "connectors.gh.auth" {
			t.Fatalf("findings = %+v", fs)
		}
	})
	t.Run("granted", func(t *testing.T) {
		f, _ := openSecrets(t, t.TempDir())
		if err := f.Set(secrets.Connectors, "gh", map[string]string{secrets.ConnectorAccessToken: "x"}); err != nil {
			t.Fatal(err)
		}
		fs := con001Rule().Run(context.Background(), &Input{Config: cfg, Secrets: f})
		if len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
	t.Run("nil config", func(t *testing.T) {
		if fs := con001Rule().Run(context.Background(), &Input{}); len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
}

func TestCON002(t *testing.T) {
	cfg := withConnectors(
		config.Connector{ID: "a", URL: "https://a.example/mcp", Auth: "oauth"},
		config.Connector{ID: "b", URL: "https://b.example/mcp", Auth: "none"},
		config.Connector{ID: "c", URL: "https://c.example/mcp", Auth: "none"},
		config.Connector{ID: "d", URL: "https://d.example/mcp", Auth: "none"},
		config.Connector{ID: "e", URL: "https://e.example/mcp", Auth: "none"},
	)
	rows := []store.Connector{
		{ID: "e", State: store.ConnectorPending},
		{ID: "a", State: store.ConnectorExpired, Detail: "refresh_rejected", ServerName: "Evil <server>"},
		{ID: "b", State: store.ConnectorDegraded, Detail: "ping_failed", ServerName: "Evil <server>"},
		{ID: "c", State: store.ConnectorDegraded},
		{ID: "d", State: store.ConnectorConnected},
		{ID: "ghost", State: store.ConnectorExpired}, // no config: the daemon's business
	}
	fs := con002Rule().Run(context.Background(), &Input{Config: cfg, Connectors: rows})
	if len(fs) != 3 {
		t.Fatalf("findings = %+v, want 3", fs)
	}
	want := []struct{ key, msg, fix string }{
		{"connectors.a", `connector "a" is expired: its sign-in can no longer be refreshed`, "run `holaclaw connectors reconnect a`"},
		{"connectors.b", `connector "b" is degraded: ping_failed`, "run `holaclaw connectors show b`; reconnect if it persists"},
		{"connectors.c", `connector "c" is degraded`, "run `holaclaw connectors show c`; reconnect if it persists"},
	}
	for i, w := range want {
		f := fs[i]
		if f.ID != "CON002" || f.Severity != SeverityWarning || f.File != config.FileConnectors ||
			f.Key != w.key || f.Message != w.msg || f.Fix != w.fix {
			t.Errorf("finding %d = %+v, want %+v", i, f, w)
		}
		if strings.Contains(f.Message, "Evil") {
			t.Errorf("server text leaked into %q", f.Message)
		}
	}

	t.Run("no rows or no config", func(t *testing.T) {
		if fs := con002Rule().Run(context.Background(), &Input{Config: cfg}); len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
		if fs := con002Rule().Run(context.Background(), &Input{Connectors: rows}); len(fs) != 0 {
			t.Fatalf("findings = %v, want none", fs)
		}
	})
}

func TestCON003(t *testing.T) {
	cases := []struct {
		url, auth string
		fires     bool
	}{
		{"http://mcp.example.com/mcp", "none", true},
		{"http://10.0.0.5:8080/mcp", "oauth", true},
		{"https://mcp.example.com/mcp", "oauth", false},
		{"http://127.0.0.1:8080/mcp", "none", false},
		{"http://localhost/mcp", "oauth", false},
		{"http://[::1]:9000/mcp", "none", false},
		{"http://[::1]/mcp", "oauth", false},
		{"http://LOCALHOST/mcp", "oauth", false},
		{"http://localhost.evil.example/mcp", "oauth", true},
		{"http://127.0.0.1.nip.io/mcp", "oauth", true},
		{"http://127.0.0.1@evil.example/mcp", "oauth", true},
		{"http:///mcp", "none", false},         // no host: create refuses it
		{"http://bad host/%zz", "none", false}, // unparsable: create refuses it
	}
	for _, c := range cases {
		t.Run(c.url, func(t *testing.T) {
			cfg := withConnectors(config.Connector{ID: "x", URL: c.url, Auth: c.auth})
			fs := con003Rule().Run(context.Background(), &Input{Config: cfg})
			if !c.fires {
				if len(fs) != 0 {
					t.Fatalf("findings = %v, want none", fs)
				}
				return
			}
			if len(fs) != 1 || fs[0].ID != "CON003" || fs[0].Severity != SeverityError ||
				fs[0].File != config.FileConnectors || fs[0].Key != "connectors.x.url" {
				t.Fatalf("findings = %+v", fs)
			}
		})
	}
	insecure := withConnectors(config.Connector{ID: "lab", URL: "http://10.0.4.20:8765/mcp", Auth: "none", AllowInsecureTransport: true})
	if fs := con003Rule().Run(context.Background(), &Input{Config: insecure}); len(fs) != 0 {
		t.Fatalf("allow_insecure_transport: findings = %v, want none", fs)
	}
	bad := withConnectors(config.Connector{ID: "Bad\x1b[31m", URL: "http://mcp.example.com/mcp", Auth: "oauth", Trusted: true})
	for _, r := range []Rule{con001Rule(), con003Rule(), con004Rule()} {
		if fs := r.Run(context.Background(), &Input{Config: bad}); len(fs) != 0 {
			t.Fatalf("%s on an invalid id: findings = %v, want none (CFG011)", r.ID, fs)
		}
	}
	if fs := con003Rule().Run(context.Background(), &Input{}); len(fs) != 0 {
		t.Fatalf("nil config: findings = %v", fs)
	}
}

func TestCON004(t *testing.T) {
	cfg := withConnectors(
		config.Connector{ID: "wiki", URL: "https://wiki.example/mcp", Auth: "none", Trusted: true},
		config.Connector{ID: "web", URL: "https://web.example/mcp", Auth: "none"},
	)
	fs := con004Rule().Run(context.Background(), &Input{Config: cfg})
	if len(fs) != 1 || fs[0].ID != "CON004" || fs[0].Severity != SeverityInfo ||
		fs[0].File != config.FileConnectors || fs[0].Key != "connectors.wiki.trusted" {
		t.Fatalf("findings = %+v", fs)
	}
	if ExitCode(fs) != 0 {
		t.Errorf("an info finding changed the exit code")
	}
	if fs := con004Rule().Run(context.Background(), &Input{}); len(fs) != 0 {
		t.Fatalf("nil config: findings = %v", fs)
	}
}

func TestSEC004Connector(t *testing.T) {
	dir := t.TempDir()
	f, path := openSecrets(t, dir)
	for _, id := range []string{"gone", "gh"} {
		if err := f.Set(secrets.Connectors, id, map[string]string{secrets.ConnectorAccessToken: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := withConnectors(config.Connector{ID: "gh", URL: "https://gh.example/mcp", Auth: "oauth"})
	fs := sec004Rule().Run(context.Background(), &Input{Config: cfg, Secrets: f, SecretsPath: path, ConfigDir: dir})
	if len(fs) != 1 || fs[0].ID != "SEC004" || fs[0].Severity != SeverityWarning || fs[0].Key != "connectors.gone" ||
		fs[0].Message != `auth.toml has a secret for connector "gone", which is not configured` ||
		fs[0].Fix != "run `holaclaw auth rm connectors.gone` if it is no longer needed" {
		t.Fatalf("findings = %+v", fs)
	}
}

// CFG006 is config's: an agent granting an undefined connector reaches
// `check` through the CFG pass-through.
func TestCFG006UndefinedConnectorGrant(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "config.toml", "version = 1\n\n[providers.ollama]\ntype = \"openai-compatible\"\nbase_url = \"http://localhost:11434/v1\"\n\n"+
		"[models.gemma]\nprovider = \"ollama\"\nid = \"gemma4:e2b\"\ncontext = 8192\n")
	writeFile(t, dir, "connectors.toml", "version = 1\n\n[connectors.weather]\nurl = \"https://weather.example/mcp\"\nauth = \"none\"\n")
	writeFile(t, dir, "agents/main/agent.toml", "version = 1\ntools = [\"memory\"]\nconnectors = [\"tracker\"]\n\n[models]\nchat = \"gemma\"\n")
	writeFile(t, dir, "agents/main/prompt.md", "You are Hola.\n")

	cfg, err := config.Load(dir)
	var ps config.Problems
	if !errors.As(err, &ps) {
		t.Fatalf("Load = %v, want problems", err)
	}
	fs := Run(context.Background(), &Input{ConfigDir: dir, Config: cfg, LoadProblems: ps}, []string{"CFG006"}, nil)
	if len(fs) != 1 || fs[0].Severity != SeverityError || fs[0].File != "agents/main/agent.toml" ||
		fs[0].Key != "connectors[0]" || !strings.Contains(fs[0].Message, `"tracker"`) {
		t.Fatalf("findings = %+v", fs)
	}
}

// The CON rules sit after the BRW rules and before the SKL rules, in Rules()
// and in RULES.md alike.
func TestCONRuleOrder(t *testing.T) {
	var ids []string
	for _, r := range Rules() {
		ids = append(ids, r.ID)
	}
	got := strings.Join(ids, " ")
	if !strings.Contains(got, "BRW006 CON001 CON002 CON003 CON004 SKL") {
		t.Fatalf("rule order = %s", got)
	}
}
