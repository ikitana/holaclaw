package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

// Connector tools in the builder (docs/connectors.md §5): outside the schema
// cap, counted on their own, named in one `connectors` fragment.

// connectorInput grants github-admin and wiki.
func connectorInput(window int, specs ...provider.ToolSpec) BuildInput {
	in := budgetInput(window, userMsg("u1", "hi"))
	in.Def.Connectors = []string{"wiki", "github-admin"}
	in.Tools = specs
	return in
}

func TestConnectorSchemaOutsideTheCap(t *testing.T) {
	fat := builderSpec("github-admin.create_issue", words(tool.MaxSchemaTokens+100))
	in := connectorInput(32768, builderSpec("memory.save", "save"), fat)
	p := build(t, in)
	b := p.Breakdown
	n := tool.SchemaTokens(fat)
	if n <= tool.MaxSchemaTokens {
		t.Fatalf("fixture is %d tokens, want over the cap", n)
	}
	var connFrag, toolFrag *Fragment
	for i, f := range b.Fragments {
		switch f.Name {
		case "connector:github-admin.create_issue":
			connFrag = &b.Fragments[i]
		case "tool:memory.save":
			toolFrag = &b.Fragments[i]
		case "tool:github-admin.create_issue":
			t.Fatalf("connector schema named as a built-in: %+v", f)
		}
	}
	if connFrag == nil || connFrag.Tokens != n || toolFrag == nil {
		t.Fatalf("fragments = %+v", b.Fragments)
	}
	frag := fragment("connectors", connectorsFragment(map[string][]string{"github-admin": {"create_issue"}}))
	if b.Connectors != n+frag.Tokens {
		t.Fatalf("Connectors = %d, want schema %d + fragment %d", b.Connectors, n, frag.Tokens)
	}
	if b.ToolSchemas != toolFrag.Tokens {
		t.Fatalf("ToolSchemas = %d, want only the built-in %d", b.ToolSchemas, toolFrag.Tokens)
	}
	sys := 0
	for _, f := range b.Fragments {
		if !strings.HasPrefix(f.Name, "tool:") && !strings.HasPrefix(f.Name, "connector") {
			sys += f.Tokens
		}
	}
	if b.System != sys {
		t.Fatalf("System = %d, want %d (connectors fragment excluded)", b.System, sys)
	}
	if b.Total != b.System+b.ToolSchemas+b.Connectors+b.History+b.Volatile {
		t.Fatalf("Total %d is not the sum of %+v", b.Total, b)
	}
	if got := BuiltAttrs(p)["connectors"]; got != b.Connectors {
		t.Fatalf("context.built connectors = %v, want %d", got, b.Connectors)
	}
}

func TestBuiltInSchemaStillCapped(t *testing.T) {
	for name, spec := range map[string]provider.ToolSpec{
		"built-in":              builderSpec("memory.save", words(tool.MaxSchemaTokens+5)),
		"ungranted connector":   builderSpec("foo.bar", words(tool.MaxSchemaTokens+5)),
		"granted id, no tool":   builderSpec("wiki.", words(tool.MaxSchemaTokens+5)),
		"granted id, bad tool":  builderSpec("wiki.a.b", words(tool.MaxSchemaTokens+5)),
		"granted id, long tool": builderSpec("wiki."+strings.Repeat("x", 65), words(tool.MaxSchemaTokens+5)),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewBuilder().Build(context.Background(), connectorInput(32768, spec))
			var be *BudgetError
			if !errors.As(err, &be) || be.What != "tool schema "+spec.Name || be.Cap != tool.MaxSchemaTokens {
				t.Fatalf("err = %v, want the schema cap", err)
			}
		})
	}
}

func TestConnectorsFragment(t *testing.T) {
	specs := []provider.ToolSpec{
		builderSpec("wiki.search", "s"),
		builderSpec("github-admin.list_issues", "l"),
		builderSpec("memory.save", "m"),
		builderSpec("github-admin.create_issue", "c"),
	}
	p := build(t, connectorInput(8192, specs...))
	want := connectorsHeading + "\n- github-admin: create_issue, list_issues\n- wiki: search"
	if !strings.HasSuffix(p.System, "\n\n"+want) {
		t.Fatalf("system =\n%s\nwant it to end with\n%s", p.System, want)
	}
	names := []string{}
	for _, f := range p.Breakdown.Fragments {
		names = append(names, f.Name)
	}
	if got := strings.Join(names[:2], ","); got != "core,connectors" {
		t.Fatalf("fragment order = %v", names)
	}

	// Byte-stable: the same tools in another order give the same text.
	reversed := []provider.ToolSpec{specs[3], specs[2], specs[1], specs[0]}
	if q := build(t, connectorInput(8192, reversed...)); q.System != p.System {
		t.Fatalf("system depends on tool order:\n%s\n%s", q.System, p.System)
	}
	// Names only what the model was given.
	q := build(t, connectorInput(8192, specs[0]))
	if strings.Contains(q.System, "github-admin") || !strings.HasSuffix(q.System, "\n- wiki: search") {
		t.Fatalf("system names a tool it was not given:\n%s", q.System)
	}
	// The prefix hash covers connector schemas.
	changed := append([]provider.ToolSpec(nil), specs...)
	changed[0] = builderSpec("wiki.search", "other")
	if r := build(t, connectorInput(8192, changed...)); r.PrefixHash == p.PrefixHash {
		t.Fatal("prefix hash ignores a connector description")
	}
}

func TestNoConnectorsFragmentWithoutConnectorTools(t *testing.T) {
	p := build(t, connectorInput(8192, builderSpec("memory.save", "m")))
	for _, f := range p.Breakdown.Fragments {
		if f.Name == "connectors" {
			t.Fatalf("connectors fragment without connector tools: %+v", p.Breakdown.Fragments)
		}
	}
	if strings.Contains(p.System, connectorsHeading) || p.Breakdown.Connectors != 0 {
		t.Fatalf("system = %q, connectors = %d", p.System, p.Breakdown.Connectors)
	}
}

func TestWindowTooSmallCountsConnectors(t *testing.T) {
	var specs []provider.ToolSpec
	// 3000 connector tokens + 1024 reserve + 512 tail do not fit 4096.
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		specs = append(specs, builderSpec("wiki."+name, words(500)))
	}
	in := connectorInput(4096, specs...)
	_, err := NewBuilder().Build(context.Background(), in)
	var be *BudgetError
	if !errors.As(err, &be) || be.What != "window too small" {
		t.Fatalf("err = %v, want window too small", err)
	}
	in.Model.Context = 16384
	if b := build(t, in).Breakdown; b.Connectors < 3000 {
		t.Fatalf("connectors = %d, want the six schemas", b.Connectors)
	}
}
