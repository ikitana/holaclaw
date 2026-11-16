package config

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// withConnectors is the minimal fixture plus a connectors.toml and an agent
// granted the given connectors.
func withConnectors(t *testing.T, connectors, grant string) string {
	t.Helper()
	d := t.TempDir()
	copyDir(t, dir("minimal"), d)
	if connectors != "" {
		write(t, filepath.Join(d, FileConnectors), connectors)
	}
	write(t, filepath.Join(d, "agents", "main", FileAgent),
		"version = 1\ntools = [\"memory\"]\nconnectors = "+grant+"\n[models]\nchat = \"gemma\"\n")
	return d
}

const twoConnectors = `version = 1

[connectors.github-admin]
url       = "https://mcp.github.com/"
auth      = "oauth"
scopes    = ["repo", "issues:write"]
client_id = "Iv1.abc123"

[connectors.homes]
url     = "http://127.0.0.1:8080/mcp"
auth    = "none"
trusted = true
`

// twoConnectorsWant is twoConnectors as loaded.
var twoConnectorsWant = map[string]Connector{
	"github-admin": {ID: "github-admin", URL: "https://mcp.github.com/", Auth: "oauth",
		Scopes: []string{"repo", "issues:write"}, ClientID: "Iv1.abc123"},
	"homes": {ID: "homes", URL: "http://127.0.0.1:8080/mcp", Auth: "none", Trusted: true},
}

func TestLoadConnectors(t *testing.T) {
	cfg, err := Load(withConnectors(t, twoConnectors, `["github-admin", "homes"]`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !reflect.DeepEqual(cfg.Connectors, twoConnectorsWant) {
		t.Fatalf("connectors = %+v\nwant %+v", cfg.Connectors, twoConnectorsWant)
	}
	if got := cfg.Agents["main"].Connectors; !reflect.DeepEqual(got, []string{"github-admin", "homes"}) {
		t.Fatalf("agent connectors = %v", got)
	}
	if got := cfg.Definition("main").Connectors; !reflect.DeepEqual(got, []string{"github-admin", "homes"}) {
		t.Fatalf("definition connectors = %v", got)
	}
}

func TestConnectorsFileIsOptional(t *testing.T) {
	cfg, err := Load(dir("minimal"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Connectors) != 0 || cfg.Agents["main"].Connectors != nil {
		t.Fatalf("connectors without a file: %v %v", cfg.Connectors, cfg.Agents["main"].Connectors)
	}
}

func problemsOf(t *testing.T, err error) Problems {
	t.Helper()
	var ps Problems
	if !errors.As(err, &ps) {
		t.Fatalf("error = %v, want Problems", err)
	}
	return ps
}

func TestConnectorDanglingReference(t *testing.T) {
	_, err := Load(withConnectors(t, twoConnectors, `["github-admn", "nope"]`))
	ps := problemsOf(t, err)
	var keys []string
	for _, p := range ps {
		if p.Code != CodeDanglingRef {
			t.Errorf("unexpected %s: %s", p.Code, p.Message)
			continue
		}
		keys = append(keys, p.File+":"+p.Key)
	}
	want := []string{"agents/main/agent.toml:connectors[0]", "agents/main/agent.toml:connectors[1]"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("dangling refs = %v, want %v", keys, want)
	}
	if !strings.Contains(ps[0].Hint, `"github-admin"`) {
		t.Errorf("hint = %q, want a did-you-mean", ps[0].Hint)
	}

	// No connectors.toml at all: every grant dangles, and the hint says why.
	_, err = Load(withConnectors(t, "", `["homes"]`))
	ps = problemsOf(t, err)
	if len(ps) != 1 || ps[0].Code != CodeDanglingRef || ps[0].Hint != "no connector is defined" {
		t.Fatalf("problems = %v", ps)
	}
}

func TestConnectorSchemaProblems(t *testing.T) {
	cases := []struct {
		name, file, code, key string
	}{
		{"missing url", "version = 1\n[connectors.a]\nauth = \"none\"\n", CodeMissingRequired, "connectors.a.url"},
		{"missing auth", "version = 1\n[connectors.a]\nurl = \"https://x.example/mcp\"\n", CodeMissingRequired, "connectors.a.auth"},
		{"bad auth", "version = 1\n[connectors.a]\nurl = \"https://x.example/mcp\"\nauth = \"bearer\"\n", CodeBadEnum, "connectors.a.auth"},
		{"relative url", "version = 1\n[connectors.a]\nurl = \"mcp.example\"\nauth = \"none\"\n", CodeBadValue, "connectors.a.url"},
		{"scopes not a list", "version = 1\n[connectors.a]\nurl = \"https://x.example/mcp\"\nauth = \"oauth\"\nscopes = \"repo\"\n", CodeWrongType, "connectors.a.scopes"},
		{"trusted not a bool", "version = 1\n[connectors.a]\nurl = \"https://x.example/mcp\"\nauth = \"none\"\ntrusted = \"yes\"\n", CodeWrongType, "connectors.a.trusted"},
		{"unknown key", "version = 1\n[connectors.a]\nurl = \"https://x.example/mcp\"\nauth = \"none\"\ntoken = \"x\"\n", CodeUnknownKey, "connectors.a.token"},
		{"missing version", "[connectors.a]\nurl = \"https://x.example/mcp\"\nauth = \"none\"\n", CodeBadVersion, "version"},
		{"uppercase id", "version = 1\n[connectors.GitHub]\nurl = \"https://x.example/mcp\"\nauth = \"none\"\n", CodeBadValue, `connectors."GitHub"`},
		{"dotted id", "version = 1\n[connectors.\"a.b\"]\nurl = \"https://x.example/mcp\"\nauth = \"none\"\n", CodeBadValue, `connectors."a.b"`},
		{"id too long", "version = 1\n[connectors." + strings.Repeat("a", 33) + "]\nurl = \"https://x.example/mcp\"\nauth = \"none\"\n", CodeBadValue, `connectors."` + strings.Repeat("a", 33) + `"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(withConnectors(t, tc.file, `[]`))
			ps := problemsOf(t, err)
			for _, p := range ps {
				if p.Code == tc.code && p.Key == tc.key && p.File == FileConnectors {
					return
				}
			}
			t.Fatalf("want %s at %s:%s, got %v", tc.code, FileConnectors, tc.key, ps)
		})
	}
}

// ParseConnectors reads the file as Load does, and ConnectorEntry writes what
// it reads back.
func TestParseConnectors(t *testing.T) {
	got, err := ParseConnectors([]byte(twoConnectors))
	if err != nil || !reflect.DeepEqual(got, twoConnectorsWant) {
		t.Fatalf("parse = %+v, %v", got, err)
	}

	f := map[string]any{"version": CurrentVersion, "connectors": map[string]any{}}
	for id, c := range twoConnectorsWant {
		f["connectors"].(map[string]any)[id] = ConnectorEntry(c)
	}
	b, err := toml.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseConnectors(b); err != nil || !reflect.DeepEqual(got, twoConnectorsWant) {
		t.Fatalf("round trip = %+v, %v\n%s", got, err, b)
	}

	for name, file := range map[string]string{
		"not toml":    "this is = = not toml",
		"unknown key": "version = 1\n[connectors.a]\nurl = \"https://x.example/mcp\"\nauth = \"none\"\ntoken = \"x\"\n",
		"bad id":      "version = 1\n[connectors.GitHub]\nurl = \"https://x.example/mcp\"\nauth = \"none\"\n",
	} {
		if _, err := ParseConnectors([]byte(file)); len(problemsOf(t, err)) == 0 {
			t.Errorf("%s: no problems", name)
		}
	}
}

func TestValidConnectorID(t *testing.T) {
	for id, want := range map[string]bool{
		"github-admin": true, "a": true, "0x": true, strings.Repeat("a", 32): true,
		"": false, "GitHub": false, "a_b": false, "a.b": false, "a b": false, "ü": false,
		strings.Repeat("a", 33): false,
	} {
		if got := ValidConnectorID(id); got != want {
			t.Errorf("ValidConnectorID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestValidateConnectorCollidesWithBuiltin(t *testing.T) {
	const file = "version = 1\n" +
		"[connectors.memory]\nurl = \"https://x.example/mcp\"\nauth = \"none\"\n" +
		"[connectors.fetch]\nurl = \"https://y.example/mcp\"\nauth = \"none\"\n" +
		"[connectors.homes]\nurl = \"https://z.example/mcp\"\nauth = \"none\"\n"
	cfg, err := Load(withConnectors(t, file, `["homes"]`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ps := problemsOf(t, Validate(cfg, []string{"memory.save", "memory.search", "fetch"}))
	var keys []string
	for _, p := range ps {
		if p.Code != CodeBadValue || p.File != FileConnectors {
			t.Errorf("unexpected %s", p)
			continue
		}
		keys = append(keys, p.Key)
	}
	if !reflect.DeepEqual(keys, []string{"connectors.fetch", "connectors.memory"}) {
		t.Fatalf("collisions = %v", keys)
	}
}

// policy.tools may name a defined connector, or one of its tools as
// <id>.<tool> (docs/connectors.md §6); an undefined prefix stays CFG010.
func TestValidatePolicyToolsNamingConnectors(t *testing.T) {
	d := withConnectors(t, twoConnectors, `["github-admin", "homes"]`)
	write(t, filepath.Join(d, "agents", "main", FileAgent), "version = 1\ntools = [\"memory\"]\n"+
		"connectors = [\"github-admin\", \"homes\"]\n[models]\nchat = \"gemma\"\n[policy.tools]\n"+
		"\"homes\" = \"allow\"\n\"github-admin.create_issue\" = \"ask\"\n\"gitlab.create_issue\" = \"ask\"\n\"homes.\" = \"deny\"\n")
	cfg, err := Load(d)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ps := problemsOf(t, Validate(cfg, []string{"memory.save"}))
	var keys []string
	for _, p := range ps {
		if p.Code != CodeUnknownTool {
			t.Errorf("unexpected %s", p)
			continue
		}
		keys = append(keys, p.Key)
	}
	if want := []string{"policy.tools.gitlab.create_issue", "policy.tools.homes."}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("CFG010 keys = %v, want %v", keys, want)
	}
}

func TestConnectorGrantedTwice(t *testing.T) {
	_, err := Load(withConnectors(t, twoConnectors, `["homes", "github-admin", "homes"]`))
	ps := problemsOf(t, err)
	if len(ps) != 1 || ps[0].Code != CodeBadValue || ps[0].Key != "connectors[2]" {
		t.Fatalf("problems = %v", ps)
	}
}

// A connector id in `tools` is unknown there, and the hint says where it goes.
func TestValidateConnectorInTools(t *testing.T) {
	d := withConnectors(t, twoConnectors, `[]`)
	write(t, filepath.Join(d, "agents", "main", FileAgent),
		"version = 1\ntools = [\"memory\", \"homes\"]\n[models]\nchat = \"gemma\"\n")
	cfg, err := Load(d)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	ps := problemsOf(t, Validate(cfg, []string{"memory.save"}))
	if len(ps) != 1 || ps[0].Code != CodeUnknownTool || !strings.Contains(ps[0].Hint, "`connectors`") {
		t.Fatalf("problems = %v", ps)
	}
}
