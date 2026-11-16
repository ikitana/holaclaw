package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
	"holaclaw/internal/connector"
	"holaclaw/internal/connector/mcp/mcptest"
	"holaclaw/internal/provider"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
)

// stubTool is a registry tool that answers its own name.
type stubTool struct{ name string }

func (s stubTool) Spec() provider.ToolSpec {
	return provider.ToolSpec{Name: s.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (stubTool) Risk() tool.Risk { return tool.RiskRead }
func (s stubTool) Run(context.Context, tool.Call) (tool.Result, error) {
	return tool.Result{Text: s.name}, nil
}

// connectorApp is testApp with a registry of fetch, skill.load and react
// (which joins every tool set), and a manager over one open MCP server
// serving the connector "homes".
func connectorApp(t *testing.T) (*App, *mcptest.Server) {
	t.Helper()
	ctx := context.Background()
	a := testApp(t, "")
	a.Registry = tool.NewRegistry()
	for _, n := range []string{"fetch", "skill.load", tool.ReactTool} {
		if err := a.Registry.Register(stubTool{n}); err != nil {
			t.Fatal(err)
		}
	}
	srv := mcptest.New(t, mcptest.Options{Tools: []mcptest.Tool{mcptest.TextTool("echo", "hello")}})
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sec, err := secrets.Open(filepath.Join(dir, "auth.toml"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := connector.New(ctx, connector.Options{
		Dir: dir, Store: st, Secrets: sec,
		Connectors: map[string]config.Connector{"homes": {ID: "homes", URL: srv.URL, Auth: "none"}},
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	m.Check(ctx)
	if c, _ := m.Get(ctx, "homes"); c.State != store.ConnectorConnected {
		t.Fatalf("homes = %s", c.State)
	}
	a.Connectors = m
	return a, srv
}

func specNames(e tool.Executor) []string {
	var out []string
	for _, s := range e.Specs() {
		out = append(out, s.Name)
	}
	return out
}

func TestExecutorConnectors(t *testing.T) {
	ctx := context.Background()
	a, srv := connectorApp(t)
	a.Config.Agents["main"] = config.Agent{Name: "main", Models: config.Roles{Chat: "chat"}, Tools: []string{"fetch"}, Connectors: []string{"homes"}}
	a.Config.Agents["plain"] = config.Agent{Name: "plain", Models: config.Roles{Chat: "chat"}, Tools: []string{"fetch"}}

	e, err := a.Executor("main")
	if err != nil {
		t.Fatal(err)
	}
	if got := specNames(e); !slices.Equal(got, []string{"fetch", "react", "skill.load", "homes.echo"}) {
		t.Fatalf("specs = %v", got)
	}
	for name, want := range map[string]string{"fetch": "fetch", "homes.echo": "hello"} {
		res, err := e.Execute(ctx, tool.Call{Name: name})
		if err != nil || res.Text != want {
			t.Errorf("%s = %+v %v", name, res, err)
		}
	}
	lk := e.(tool.Lookuper)
	if tl, ok := lk.Lookup("homes.echo"); !ok || tl.(tool.ConnectorTool).Connector() != "homes" {
		t.Error("homes.echo not found")
	}
	if _, ok := lk.Lookup("fetch"); !ok {
		t.Error("fetch not found")
	}
	if res, _ := e.Execute(ctx, tool.Call{Name: "homes.nope"}); !res.IsError || res.Text != `unknown tool "homes.nope"` {
		t.Errorf("unknown = %+v", res)
	}

	// The same executor sees a refreshed list on its next Specs call.
	srv.SetTools(mcptest.TextTool("echo", "hello"), mcptest.TextTool("add", "added"))
	a.Connectors.Check(ctx)
	if got := specNames(e); !slices.Equal(got, []string{"fetch", "react", "skill.load", "homes.add", "homes.echo"}) {
		t.Errorf("after refresh = %v", got)
	}
	if res, err := e.Execute(ctx, tool.Call{Name: "homes.add"}); err != nil || res.Text != "added" {
		t.Errorf("new tool = %+v %v", res, err)
	}

	// An agent without connectors gets the registry only.
	e, err = a.Executor("plain")
	if err != nil {
		t.Fatal(err)
	}
	if got := specNames(e); !slices.Equal(got, []string{"fetch", "react", "skill.load"}) {
		t.Errorf("plain = %v", got)
	}

	// Without a manager, connectors contribute nothing.
	a.Connectors = nil
	if e, err = a.Executor("main"); err != nil {
		t.Fatal(err)
	}
	if got := specNames(e); !slices.Equal(got, []string{"fetch", "react", "skill.load"}) {
		t.Errorf("no manager = %v", got)
	}

	// A bad registry entry still fails.
	a.Config.Agents["bad"] = config.Agent{Name: "bad", Models: config.Roles{Chat: "chat"}, Tools: []string{"nope"}, Connectors: []string{"homes"}}
	if _, err := a.Executor("bad"); err == nil {
		t.Error("unknown tool accepted")
	}
}

// liveMerge: nil without tools, either side alone, and a refusal of a name
// offered twice.
func TestLiveMerge(t *testing.T) {
	a, _ := connectorApp(t)
	sel, err := a.Registry.Select([]string{"fetch"})
	if err != nil {
		t.Fatal(err)
	}
	conn := a.Connectors.Executor([]string{"homes"})
	if e, err := liveMerge(nil, nil, slog.Default()); e != nil || err != nil {
		t.Errorf("no tools = %v %v", e, err)
	}
	if e, _ := liveMerge(nil, conn, slog.Default()); !slices.Equal(specNames(e), []string{"homes.echo"}) {
		t.Errorf("connectors only = %v", specNames(e))
	}
	if e, _ := liveMerge(sel, nil, slog.Default()); !slices.Equal(specNames(e), []string{"fetch"}) {
		t.Errorf("registry only = %v", specNames(e))
	}
	if _, err := liveMerge(sel, sel, slog.Default()); err == nil {
		t.Error("a name offered twice was merged")
	}
}

// A name offered twice after the merge was built offers the registry's
// specs alone, logged once.
func TestMergedSpecsFallback(t *testing.T) {
	a, _ := connectorApp(t)
	sel, err := a.Registry.Select([]string{"fetch"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	m := &merged{sel: sel, conn: sel, log: slog.New(slog.NewTextHandler(&buf, nil))}
	for range 2 {
		if got := specNames(m); !slices.Equal(got, []string{"fetch"}) {
			t.Errorf("specs = %v", got)
		}
	}
	if n := strings.Count(buf.String(), "connector tools left out"); n != 1 {
		t.Errorf("logged %d times: %s", n, buf.String())
	}
}
