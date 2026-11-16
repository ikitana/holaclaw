package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"holaclaw/internal/config"
	"holaclaw/internal/connector/mcp"
	"holaclaw/internal/store"
)

// The offline numbers of `check` and `prompt-size` count the granted
// connectors' cached tool lists (docs/connectors.md §5).

// connectorConfig is the minimal fixture with one connector, wiki, granted
// to main, and its secrets in the temp dir.
func connectorConfig(t *testing.T) string {
	t.Helper()
	dir := fixture(t, "minimal")
	f := struct {
		Version    int64                     `toml:"version"`
		Connectors map[string]map[string]any `toml:"connectors"`
	}{config.CurrentVersion, map[string]map[string]any{
		"wiki": config.ConnectorEntry(config.Connector{ID: "wiki", URL: "http://127.0.0.1:8080/mcp", Auth: "none"}),
	}}
	b, err := toml.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, config.FileConnectors), string(b))
	writeTestFile(t, filepath.Join(dir, "agents", "main", config.FileAgent),
		"version = 1\ntools = [\"memory\"]\nconnectors = [\"wiki\"]\n\n[models]\nchat = \"gemma\"\n")
	body, err := os.ReadFile(filepath.Join(dir, config.FileConfig))
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, config.FileConfig),
		string(body)+fmt.Sprintf("\n[server]\nsecrets_dir = %q\n", filepath.Join(dir, "secrets")))
	return dir
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// cacheWiki stores wiki's cache row with n tools of about 500 tokens each.
func cacheWiki(t *testing.T, dir string, n int) {
	t.Helper()
	var tools []mcp.Tool
	for i := range n {
		tools = append(tools, mcp.Tool{
			Name: fmt.Sprintf("op%d", i), Description: strings.Repeat("word ", 300),
			InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string","description":"` + strings.Repeat("word ", 190) + `"}}}`),
		})
	}
	raw, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.PutConnector(ctx, store.Connector{ID: "wiki", State: store.ConnectorConnected, Tools: raw}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Gone, to show the commands do not make it.
	if err := os.RemoveAll(filepath.Join(dir, "data", "blobs")); err != nil {
		t.Fatal(err)
	}
}

// dataFiles lists the data directory with sizes and mtimes.
func dataFiles(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range entries {
		info, _ := e.Info()
		fmt.Fprintf(&b, "%s %d %d\n", e.Name(), info.Size(), info.ModTime().UnixNano())
	}
	return b.String()
}

func TestPromptSizeCountsCachedConnectors(t *testing.T) {
	dir := connectorConfig(t)
	cacheWiki(t, dir, 5)
	before := dataFiles(t, dir)
	defer func() {
		if after := dataFiles(t, dir); after != before {
			t.Errorf("the data directory changed:\n%s\n%s", before, after)
		}
	}()
	code, out, _ := runCLI(t, "", "prompt-size", "--config", dir)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"connector:wiki.op0", "connector:wiki.op4", "\nconnectors   ", "BUD002 warning: agents/main/agent.toml: connectors: connector tools are"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if strings.Contains(out, "tool:wiki.") {
		t.Error("a connector schema counted as a built-in")
	}

	code, out, _ = runCLI(t, "", "check", "--config", dir, "--offline")
	if code != 1 || !strings.Contains(out, "BUD002 warning") || strings.Contains(out, "BUD001") {
		t.Fatalf("check: exit %d\n%s", code, out)
	}
}

func TestPromptSizeSmallConnectorNoWarning(t *testing.T) {
	dir := connectorConfig(t)
	cacheWiki(t, dir, 1)
	code, out, _ := runCLI(t, "", "prompt-size", "--config", dir)
	if code != 0 || !strings.Contains(out, "connector:wiki.op0") || strings.Contains(out, "BUD002") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// specsFor keeps the connectors when a tool is unknown, and leaves out a
// connector id that is a built-in group name.
func TestSpecsForConnectors(t *testing.T) {
	reg, err := checkRegistry()
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := json.Marshal([]mcp.Tool{{Name: "big", Description: strings.Repeat("word ", 300)}})
	rows := []store.Connector{{ID: "wiki", Tools: tools}, {ID: "memory", Tools: tools}}
	cfg := &config.Config{Agents: map[string]config.Agent{
		"main": {Name: "main", Tools: []string{"no-such-tool"}, Connectors: []string{"wiki", "memory"}},
	}}
	specs := specsFor(cfg, reg, rows)("main")
	if len(specs) != 1 || specs[0].Name != "wiki.big" {
		t.Fatalf("specs = %+v", specs)
	}
}

// A database that cannot be read is no connector numbers and one note.
func TestPromptSizeUnreadableStore(t *testing.T) {
	dir := connectorConfig(t)
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(dir, "data", "holaclaw.db"), "not a database")
	before := dataFiles(t, dir)
	for _, args := range [][]string{{"prompt-size", "--config", dir}, {"check", "--config", dir, "--offline", "--only", "BUD"}} {
		code, out, errOut := runCLI(t, "", args...)
		if code != 0 || strings.Contains(out, "connector:") || strings.Count(errOut, "connector cache not read: ") != 1 {
			t.Fatalf("%s: exit %d\nstdout:\n%s\nstderr:\n%s", args[0], code, out, errOut)
		}
	}
	if after := dataFiles(t, dir); after != before {
		t.Fatalf("the data directory changed:\n%s\n%s", before, after)
	}
}

// Without a database there are no connector numbers, and neither command
// creates one.
func TestPromptSizeWithoutStore(t *testing.T) {
	dir := connectorConfig(t)
	code, out, _ := runCLI(t, "", "prompt-size", "--config", dir)
	if code != 0 || strings.Contains(out, "connector:") || strings.Contains(out, "BUD002") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if code, out, _ := runCLI(t, "", "check", "--config", dir, "--offline"); code != 0 || strings.Contains(out, "BUD") {
		t.Fatalf("check: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "holaclaw.db")); !os.IsNotExist(err) {
		t.Fatalf("a database was created: %v", err)
	}
}

// check reads the cache rows for CON002 even when no agent is granted the
// connector: a connector in connectors.toml is enough.
func TestCheckReportsExpiredUngrantedConnector(t *testing.T) {
	dir := connectorConfig(t)
	writeTestFile(t, filepath.Join(dir, "agents", "main", config.FileAgent),
		"version = 1\ntools = [\"memory\"]\n\n[models]\nchat = \"gemma\"\n")
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutConnector(ctx, store.Connector{ID: "wiki", State: store.ConnectorExpired}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCLI(t, "", "check", "--config", dir, "--offline", "--only", "CON")
	want := "CON002 warning: connectors.toml: connectors.wiki: connector \"wiki\" is expired: its sign-in can no longer be refreshed (fix: run `holaclaw connectors reconnect wiki`)\n"
	if code != 1 || out != want || errOut != "" {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out, errOut)
	}
}
