package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"holaclaw/internal/connector/mcp"
	"holaclaw/internal/connector/mcp/mcptest"
	"holaclaw/internal/provider"
	"holaclaw/internal/store"
	"holaclaw/internal/tool"
)

// openConnectors is a harness whose server is open, with one open
// connector per id over it.
func openConnectors(t *testing.T, ids ...string) *harness {
	t.Helper()
	h := newHarness(t)
	h.g.set(func(g *guard) { g.open = true })
	for _, id := range ids {
		if _, err := h.m.Create(h.ctx, CreateRequest{ID: id, URL: h.srv.URL}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	return h
}

// setTools replaces the server's tools and waits for the cache to follow.
func (h *harness) setTools(id string, tools ...mcptest.Tool) {
	h.t.Helper()
	h.srv.SetTools(tools...)
	h.m.Check(h.ctx)
	eventually(h.t, "the tool list", func() bool {
		got, _ := h.m.Tools(id)
		return len(got) == len(tools)
	})
}

func lookup(t *testing.T, e tool.Executor, name string) tool.Tool {
	t.Helper()
	tl, ok := e.(tool.Lookuper).Lookup(name)
	if !ok {
		t.Fatalf("lookup %s failed", name)
	}
	return tl
}

func names(specs []provider.ToolSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func eq(a, b tool.Result) bool { return reflect.DeepEqual(a, b) }

// The adapter maps name, description, schema, risk and the gate hooks.
func TestToolMapping(t *testing.T) {
	h := openConnectors(t, "homes")
	schema := `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`
	h.setTools("homes",
		mcptest.Tool{Name: "search", Description: "Search the homes. " + strings.Repeat("Long. ", 200), InputSchema: json.RawMessage(schema),
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}},
		mcptest.Tool{Name: "delete", Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true)}},
		mcptest.Tool{Name: "safe", Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(false)}},
		mcptest.Tool{Name: "plain"},
	)
	e := h.m.Executor([]string{"homes"})

	s := lookup(t, e, "homes.search")
	spec := s.Spec()
	if spec.Name != "homes.search" || !strings.HasPrefix(spec.Description, "Search the homes.") || tool.SchemaTokens(spec) <= tool.MaxSchemaTokens {
		t.Errorf("spec = %s %q (%d tokens)", spec.Name, spec.Description[:20], tool.SchemaTokens(spec))
	}
	var got, want any
	_ = json.Unmarshal(spec.Parameters, &got)
	_ = json.Unmarshal([]byte(schema), &want)
	if b1, _ := json.Marshal(got); string(b1) != mustJSON(want) {
		t.Errorf("schema = %s", spec.Parameters)
	}
	if s.Risk() != tool.RiskRead {
		t.Errorf("search risk = %s", s.Risk())
	}
	for name, ask := range map[string]bool{"homes.search": false, "homes.delete": true, "homes.safe": false, "homes.plain": false} {
		tl := lookup(t, e, name)
		if got := tl.(tool.AlwaysAsker).AlwaysAsk(); got != ask {
			t.Errorf("%s AlwaysAsk = %v", name, got)
		}
		if name != "homes.search" && tl.Risk() != tool.RiskExternal {
			t.Errorf("%s risk = %s", name, tl.Risk())
		}
		if tl.(tool.ConnectorTool).Connector() != "homes" || tl.(tool.TrustedTool).Trusted() {
			t.Errorf("%s connector/trusted wrong", name)
		}
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A missing or unusable schema is still an object schema.
func TestToolEmptySchema(t *testing.T) {
	for _, raw := range []string{"", "null", "[]", "not json"} {
		ct := &connTool{id: "homes", tool: mcp.Tool{Name: "x", InputSchema: json.RawMessage(raw)}}
		var obj map[string]any
		if err := json.Unmarshal(ct.Spec().Parameters, &obj); err != nil || obj["type"] != "object" {
			t.Errorf("schema for %q = %s", raw, ct.Spec().Parameters)
		}
	}
	ct := &connTool{id: "homes", tool: mcp.Tool{Name: "x", InputSchema: json.RawMessage(`{}`)}}
	if string(ct.Spec().Parameters) != `{}` {
		t.Errorf("declared {} changed: %s", ct.Spec().Parameters)
	}
}

// A result over the cap is cut on a UTF-8 boundary, saying so. mcp refuses a
// bigger response first, so this is the flattened text alone.
func TestCapResult(t *testing.T) {
	if s := strings.Repeat("a", maxResultBytes); capResult(s) != s {
		t.Error("text at the cap was cut")
	}
	// Every é starts at an odd offset, so the even cap splits one.
	got := capResult("a" + strings.Repeat("é", maxResultBytes/2))
	kept, note, ok := strings.Cut(got, "\n[result cut")
	if !ok || note != fmt.Sprintf(" at %d bytes]", maxResultBytes) || len(kept) != maxResultBytes-1 || !utf8.ValidString(kept) {
		t.Errorf("%d bytes kept, note %q", len(kept), note)
	}
}

// trusted comes from the connector's config, read live.
func TestToolTrusted(t *testing.T) {
	h := openConnectors(t, "homes", "wiki")
	conns := h.fileConnectors()
	w := conns["wiki"]
	w.Trusted = true
	conns["wiki"] = w
	_ = h.m.Close()
	h.m = h.start(conns)
	e := h.m.Executor([]string{"homes", "wiki"})
	if lookup(t, e, "homes.echo").(tool.TrustedTool).Trusted() {
		t.Error("homes trusted")
	}
	wt := lookup(t, e, "wiki.echo").(tool.TrustedTool)
	if !wt.Trusted() {
		t.Error("wiki not trusted")
	}
	if err := h.m.Delete(h.ctx, "wiki"); err != nil {
		t.Fatal(err)
	}
	if wt.Trusted() {
		t.Error("a deleted connector is still trusted")
	}
}

func TestToolRun(t *testing.T) {
	h := openConnectors(t, "homes")
	h.setTools("homes",
		mcptest.TextTool("echo", "hello"),
		mcptest.Tool{Name: "fail", Handler: func(context.Context, json.RawMessage) (*sdk.CallToolResult, error) {
			return mcptest.Error("no such home"), nil
		}},
		mcptest.Tool{Name: "args", Handler: func(_ context.Context, args json.RawMessage) (*sdk.CallToolResult, error) {
			return mcptest.Text(string(args)), nil
		}},
	)
	e := h.m.Executor([]string{"homes"})
	run := func(name, args string) tool.Result {
		t.Helper()
		res, err := e.Execute(h.ctx, tool.Call{Name: name, Args: json.RawMessage(args)})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return res
	}
	if r := run("homes.echo", ""); !eq(r, tool.Result{Text: "hello", Tainted: true}) {
		t.Errorf("echo = %+v", r)
	}
	if r := run("homes.fail", "{}"); !eq(r, tool.Result{Text: "no such home", IsError: true, Tainted: true}) {
		t.Errorf("isError = %+v", r)
	}
	if r := run("homes.args", `{"q":"x"}`); r.Text != `{"q":"x"}` {
		t.Errorf("args = %+v", r)
	}
	// A cancelled ctx is the only Go error.
	ctx, cancel := context.WithCancel(h.ctx)
	cancel()
	if _, err := e.Execute(ctx, tool.Call{Name: "homes.echo"}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled = %v", err)
	}

	// Unknown names answer like the registry.
	for _, name := range []string{"homes.nope", "other.echo", "echo", ""} {
		if r := run(name, ""); !r.IsError || r.Text != `unknown tool "`+name+`"` {
			t.Errorf("%q = %+v", name, r)
		}
	}
}

// A degraded connector still calls; an expired one keeps its tools and
// answers a readable error result.
func TestToolRunStates(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	e := h.m.Executor([]string{"gh"})
	echo := lookup(t, e, "gh.echo")

	h.g.set(func(g *guard) { g.down = true })
	for range maxFailures {
		h.m.Check(h.ctx)
	}
	if st, _ := h.state("gh"); st != store.ConnectorDegraded {
		t.Fatalf("state = %s", st)
	}
	res, err := echo.Run(h.ctx, tool.Call{Name: "gh.echo"})
	if err != nil || !res.IsError || !res.Tainted || res.Text != "connector gh: request failed" {
		t.Errorf("degraded and down = %+v %v", res, err)
	}
	h.g.set(func(g *guard) { g.down = false })
	if res, err := echo.Run(h.ctx, tool.Call{Name: "gh.echo"}); err != nil || !eq(res, tool.Result{Text: "hello", Tainted: true}) {
		t.Errorf("degraded = %+v %v", res, err)
	}

	h.is.setFail("invalid_grant")
	h.clock.advance(55 * time.Minute)
	h.m.Check(h.ctx)
	if st, _ := h.state("gh"); st != store.ConnectorExpired {
		t.Fatalf("state = %s", st)
	}
	if got := names(e.Specs()); !slices.Equal(got, []string{"gh.echo"}) {
		t.Errorf("expired specs = %v", got)
	}
	res, err = e.Execute(h.ctx, tool.Call{Name: "gh.echo"})
	if err != nil || !eq(res, tool.Result{Text: "connector gh needs to be reconnected by an admin", IsError: true, Tainted: true}) {
		t.Errorf("expired = %+v %v", res, err)
	}
}

// A connector still being created or unknown answers a readable error.
func TestToolRunNotConnected(t *testing.T) {
	h := newHarness(t)
	h.create("new", pinned)
	for id, want := range map[string]string{
		"new":  "connector new is not connected yet",
		"nope": "connector nope does not exist",
	} {
		ct := &connTool{manager: h.m, id: id, tool: mcp.Tool{Name: "echo"}}
		res, err := ct.Run(h.ctx, tool.Call{Name: id + ".echo"})
		if err != nil || !eq(res, tool.Result{Text: want, IsError: true, Tainted: true}) {
			t.Errorf("%s = %+v %v", id, res, err)
		}
	}
	if e := h.m.Executor([]string{"new", "nope"}); len(e.Specs()) != 0 {
		t.Errorf("specs = %v", names(e.Specs()))
	}
}

// Only granted ids, in a stable order, read live.
func TestExecutor(t *testing.T) {
	h := openConnectors(t, "wiki", "homes", "other")
	h.setTools("homes", mcptest.TextTool("zeta", "z"), mcptest.TextTool("alpha", "a"))
	h.m.Check(h.ctx) // every connector follows
	e := h.m.Executor([]string{"wiki", "homes", "ghost", "homes"})
	want := []string{"homes.alpha", "homes.zeta", "wiki.alpha", "wiki.zeta"}
	if got := names(e.Specs()); !slices.Equal(got, want) {
		t.Fatalf("specs = %v", got)
	}
	if got := names(e.Specs()); !slices.Equal(got, want) {
		t.Errorf("second read = %v", got)
	}
	for _, name := range []string{"other.alpha", "ghost.alpha", "homes.nope", "homes", "homes.alpha.x"} {
		if _, ok := e.(tool.Lookuper).Lookup(name); ok {
			t.Errorf("lookup %s succeeded", name)
		}
	}
	if res, err := e.Execute(h.ctx, tool.Call{Name: "other.alpha"}); err != nil || res.Text != `unknown tool "other.alpha"` || res.Tainted {
		t.Errorf("ungranted = %+v %v", res, err)
	}

	// A refreshed list shows on the next read.
	h.setTools("homes", mcptest.TextTool("beta", "b"))
	h.m.Check(h.ctx)
	if got := names(e.Specs()); !slices.Equal(got, []string{"homes.beta", "wiki.beta"}) {
		t.Errorf("after refresh = %v", got)
	}
	if res, err := e.Execute(h.ctx, tool.Call{Name: "homes.beta"}); err != nil || res.Text != "b" {
		t.Errorf("new tool = %+v %v", res, err)
	}
	if _, ok := e.(tool.Lookuper).Lookup("homes.alpha"); ok {
		t.Error("a removed tool is still found")
	}
	if e := h.m.Executor(nil); e.Specs() != nil {
		t.Error("no grants, some specs")
	}
}

// A deleted connector leaves the live view at once: no specs, no lookup,
// and a tool looked up before the delete answers an error the model reads.
func TestExecutorAfterDelete(t *testing.T) {
	h := openConnectors(t, "homes", "wiki")
	e := h.m.Executor([]string{"homes", "wiki"})
	held := lookup(t, e, "wiki.echo")
	if err := h.m.Delete(h.ctx, "wiki"); err != nil {
		t.Fatal(err)
	}
	if got := names(e.Specs()); !slices.Equal(got, []string{"homes.echo"}) {
		t.Errorf("specs = %v", got)
	}
	if _, ok := e.(tool.Lookuper).Lookup("wiki.echo"); ok {
		t.Error("a deleted connector's tool is found")
	}
	if res, err := e.Execute(h.ctx, tool.Call{Name: "wiki.echo"}); err != nil || res.Text != `unknown tool "wiki.echo"` {
		t.Errorf("execute = %+v %v", res, err)
	}
	if res, err := held.Run(h.ctx, tool.Call{Name: "wiki.echo"}); err != nil || !res.IsError || res.Text != "connector wiki does not exist" {
		t.Errorf("held tool = %+v %v", res, err)
	}
}

// The executor can be merged with the registry's selection.
func TestExecutorMerges(t *testing.T) {
	h := openConnectors(t, "homes")
	if _, err := tool.Merge(h.m.Executor([]string{"homes"})); err != nil {
		t.Fatal(err)
	}
}
