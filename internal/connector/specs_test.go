package connector

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"holaclaw/internal/connector/mcp"
	"holaclaw/internal/connector/mcp/mcptest"
	"holaclaw/internal/store"
	"holaclaw/internal/tokens"
)

// longDescription is well over MaxDescriptionTokens, with multi-byte runes
// so a cut can land inside one.
var longDescription = "Search the homes. " + strings.Repeat("ñandú 東京 é. ", 400)

func TestCapDescription(t *testing.T) {
	short := "Search the homes. " + strings.Repeat("word ", 100)
	if got := capDescription(short); got != short {
		t.Fatalf("short description changed: %q", got)
	}
	got := capDescription(longDescription)
	if !strings.HasSuffix(got, " "+descriptionCutMarker) || !strings.HasPrefix(got, "Search the homes. ") {
		t.Fatalf("cut = %q", got)
	}
	if !utf8.ValidString(got) {
		t.Fatal("cut is not valid UTF-8")
	}
	if n := tokens.Estimate(got); n > MaxDescriptionTokens || n < MaxDescriptionTokens-10 {
		t.Fatalf("cut is %d tokens, want just under %d", n, MaxDescriptionTokens)
	}
	if again := capDescription(got); again != got {
		t.Fatal("a cut description is cut again")
	}
	if descriptionsCut([]mcp.Tool{{Description: got}, {Description: short}}) != 1 {
		t.Fatal("descriptionsCut miscounts")
	}
}

// A description over the cap is cut when cached: the row, Tools and the
// specs carry the cut text, the schema is untouched, and the connector says
// so in its detail until a refresh no longer cuts anything.
func TestDescriptionCutAtRegistration(t *testing.T) {
	h := openConnectors(t, "homes")
	schema := `{"type":"object","properties":{"q":{"type":"string","description":"` + strings.Repeat("query words ", 300) + `"}}}`
	h.setTools("homes",
		mcptest.Tool{Name: "search", Description: longDescription, InputSchema: json.RawMessage(schema)},
		mcptest.Tool{Name: "plain", Description: "short"},
	)
	want := capDescription(longDescription)
	listed, _ := h.m.Tools("homes")
	byName := map[string]mcp.Tool{}
	for _, tl := range listed {
		byName[tl.Name] = tl
	}
	if byName["search"].Description != want || byName["plain"].Description != "short" {
		t.Fatalf("tools = %+v", listed)
	}
	var got, wantSchema any
	_ = json.Unmarshal(byName["search"].InputSchema, &got)
	_ = json.Unmarshal([]byte(schema), &wantSchema)
	if !reflect.DeepEqual(got, wantSchema) {
		t.Fatalf("schema changed: %s", byName["search"].InputSchema)
	}
	if spec := lookup(t, h.m.Executor([]string{"homes"}), "homes.search").Spec(); spec.Description != want {
		t.Fatalf("spec description = %q", spec.Description)
	}
	if row := mustRow(t, h, "homes"); !strings.Contains(string(row.Tools), descriptionCutMarker) || strings.Contains(string(row.Tools), longDescription) {
		t.Fatal("the cache row does not hold the cut text")
	}
	if st, d := h.state("homes"); st != store.ConnectorConnected || d != DetailDescriptionsCut {
		t.Fatalf("state = %s/%s", st, d)
	}
	if last := h.events.last("homes"); last != "connected/"+DetailDescriptionsCut {
		t.Fatalf("last status = %q", last)
	}

	// The detail survives a restart: it is the row's.
	h.restart()
	if st, d := h.state("homes"); st != store.ConnectorConnected || d != DetailDescriptionsCut {
		t.Fatalf("after restart: %s/%s", st, d)
	}

	// A failure's detail wins while degraded; recovery brings the cut back.
	h.g.set(func(g *guard) { g.down = true })
	for range 3 {
		h.m.Check(h.ctx)
	}
	if st, d := h.state("homes"); st != store.ConnectorDegraded || d != DetailUnreachable {
		t.Fatalf("degraded: %s/%s", st, d)
	}
	h.g.set(func(g *guard) { g.down = false })
	h.m.Check(h.ctx)
	if st, d := h.state("homes"); st != store.ConnectorConnected || d != DetailDescriptionsCut {
		t.Fatalf("after recovery: %s/%s", st, d)
	}

	// A list with nothing to cut clears it.
	h.setTools("homes", mcptest.Tool{Name: "search", Description: "short now"}, mcptest.Tool{Name: "plain"})
	eventually(t, "the detail to clear", func() bool {
		_, d := h.state("homes")
		return d == ""
	})
	if last := h.events.last("homes"); last != "connected/" {
		t.Fatalf("last status = %q", last)
	}
}

// A marker the server wrote is dropped: the detail and the count are
// holaclaw's own cuts only, and a cut never carries two markers.
func TestForgedCutMarker(t *testing.T) {
	h := openConnectors(t, "homes")
	h.setTools("homes",
		mcptest.Tool{Name: "short", Description: "Looks cut. " + descriptionCutMarker},
		mcptest.Tool{Name: "long", Description: longDescription + " " + descriptionCutMarker + descriptionCutMarker},
	)
	listed, _ := h.m.Tools("homes")
	for _, tl := range listed {
		switch tl.Name {
		case "short":
			if strings.Contains(tl.Description, descriptionCutMarker) {
				t.Errorf("forged marker kept: %q", tl.Description)
			}
		case "long":
			if strings.Count(tl.Description, descriptionCutMarker) != 1 {
				t.Errorf("markers in a cut: %d", strings.Count(tl.Description, descriptionCutMarker))
			}
		}
	}
	if n := descriptionsCut(listed); n != 1 {
		t.Fatalf("cut count = %d, want 1", n)
	}
	h.setTools("homes", mcptest.Tool{Name: "short", Description: "Looks cut. " + descriptionCutMarker})
	listed, _ = h.m.Tools("homes")
	if descriptionsCut(listed) != 0 {
		t.Fatalf("tools = %+v", listed)
	}
	if _, d := h.state("homes"); d != "" {
		t.Fatalf("detail = %q, want none", d)
	}
}

// A connected row cached before the cap says so at start, before any check.
func TestDescriptionCutDetailAtStart(t *testing.T) {
	h := openConnectors(t, "homes")
	row := mustRow(t, h, "homes")
	row.Tools, _ = json.Marshal([]mcp.Tool{{Name: "search", Description: longDescription}})
	row.State, row.Detail = store.ConnectorConnected, ""
	if err := h.st.PutConnector(h.ctx, row); err != nil {
		t.Fatal(err)
	}
	h.restart()
	if st, d := h.state("homes"); st != store.ConnectorConnected || d != DetailDescriptionsCut {
		t.Fatalf("at start: %s/%s", st, d)
	}
	if last := h.events.last("homes"); last != "connected/"+DetailDescriptionsCut {
		t.Fatalf("last status = %q", last)
	}
	if r := mustRow(t, h, "homes"); r.Detail != DetailDescriptionsCut {
		t.Fatalf("row detail = %q", r.Detail)
	}
}

// CachedSpecs reads the rows as the executor reads the manager.
func TestCachedSpecs(t *testing.T) {
	h := openConnectors(t, "homes", "wiki")
	h.setTools("homes", mcptest.Tool{Name: "search", Description: longDescription}, mcptest.Tool{Name: "add"})
	rows, err := h.st.ListConnectors(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"wiki", "homes", "nope", "homes"}
	live := h.m.Executor(ids).Specs()
	cached := CachedSpecs(rows, ids)
	if !reflect.DeepEqual(cached, live) {
		t.Fatalf("cached %v\nlive   %v", names(cached), names(live))
	}
	// Both connectors are the same server.
	if got := names(cached); !slices.Equal(got, []string{"homes.add", "homes.search", "wiki.add", "wiki.search"}) {
		t.Fatalf("names = %v", got)
	}
	if CachedSpecs(nil, ids) != nil || CachedSpecs(rows, nil) != nil {
		t.Fatal("specs without rows or ids")
	}

	// A row cached before the cap is cut on the way out.
	old, _ := json.Marshal([]mcp.Tool{{Name: "search", Description: longDescription}})
	specs := CachedSpecs([]store.Connector{{ID: "homes", Tools: old}}, []string{"homes"})
	if len(specs) != 1 || specs[0].Description != capDescription(longDescription) || string(specs[0].Parameters) != string(emptySchema) {
		t.Fatalf("specs = %+v", specs)
	}
}
