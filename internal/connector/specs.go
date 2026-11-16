package connector

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"holaclaw/internal/connector/mcp"
	"holaclaw/internal/provider"
	"holaclaw/internal/store"
	"holaclaw/internal/tokens"
)

// MaxDescriptionTokens caps one connector tool's description, cut when the
// list is cached so the cache row, Tools, the executor and the prompt all
// carry the same text (docs/connectors.md §5). The input schema is never
// cut: the model needs all of it to call the tool.
const MaxDescriptionTokens = 400

// descriptionCutMarker ends a cut description. Changing MaxDescriptionTokens
// changes it; rows cached before are cut again by parseTools.
var descriptionCutMarker = fmt.Sprintf("[description cut at %d tokens]", MaxDescriptionTokens)

// serverDescription is a server's description as holaclaw takes it: capped,
// after dropping any trailing marker the server wrote itself, so the marker
// at the end of a cached description is only ever holaclaw's own.
func serverDescription(d string) string {
	for {
		trimmed := strings.TrimRight(d, " \t\n")
		if !strings.HasSuffix(trimmed, descriptionCutMarker) {
			break
		}
		d = strings.TrimSuffix(trimmed, descriptionCutMarker)
	}
	return capDescription(d)
}

// capDescription cuts d on a UTF-8 boundary so that it and the marker fit
// MaxDescriptionTokens. A description that fits is returned unchanged, so a
// cut one is never cut twice: parseTools re-caps cached text with it, which
// must keep holaclaw's marker.
func capDescription(d string) string {
	if tokens.Estimate(d) <= MaxDescriptionTokens {
		return d
	}
	budget := MaxDescriptionTokens - tokens.Estimate(descriptionCutMarker)
	r := []rune(d)
	lo, hi := 0, len(r)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if tokens.Estimate(string(r[:mid])) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return strings.TrimRight(string(r[:lo]), " \t\n") + " " + descriptionCutMarker
}

// DescriptionCut reports whether a cached description was cut by
// capDescription: it ends with holaclaw's marker.
func DescriptionCut(d string) bool { return strings.HasSuffix(d, descriptionCutMarker) }

// descriptionsCut counts the tools whose description capDescription cut.
func descriptionsCut(tools []mcp.Tool) int {
	n := 0
	for _, t := range tools {
		if DescriptionCut(t.Description) {
			n++
		}
	}
	return n
}

// CachedSpecs is what Executor(ids).Specs would offer, read from cache rows
// instead of a running manager: `check` and `prompt-size` count connector
// tools offline with it. An id without a row contributes nothing.
func CachedSpecs(rows []store.Connector, ids []string) []provider.ToolSpec {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	var out []provider.ToolSpec
	for _, id := range slices.Compact(ids) {
		i := slices.IndexFunc(rows, func(r store.Connector) bool { return r.ID == id })
		if i >= 0 {
			out = append(out, specsOf(id, parseTools(rows[i].Tools))...)
		}
	}
	return out
}

// specsOf converts one connector's tools, sorted by name, so the prompt
// prefix stays byte-stable while nothing changes.
func specsOf(id string, tools []mcp.Tool) []provider.ToolSpec {
	tools = slices.Clone(tools)
	slices.SortFunc(tools, func(a, b mcp.Tool) int { return strings.Compare(a.Name, b.Name) })
	out := make([]provider.ToolSpec, len(tools))
	for i, t := range tools {
		out[i] = toolSpec(id, t)
	}
	return out
}

// toolSpec is t as the model sees it: "<id>.<name>", the server's
// description (cut) and schema, or an empty object schema when the server's
// is missing or not an object.
func toolSpec(id string, t mcp.Tool) provider.ToolSpec {
	schema := t.InputSchema
	var obj map[string]any
	if json.Unmarshal(schema, &obj) != nil || obj == nil {
		schema = emptySchema
	}
	return provider.ToolSpec{Name: id + "." + t.Name, Description: t.Description, Parameters: schema}
}
