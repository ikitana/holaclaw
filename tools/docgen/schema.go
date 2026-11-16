package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"holaclaw/internal/config"
)

// row is one line of the config reference table, walked out of the JSON
// Schema config.Schema(file) returns. leaf reports whether this row is a
// terminal key (counted by config.KeyCount) as opposed to a container
// (a table or map-table, documented but not counted).
type row struct {
	path     string
	typ      string
	allowed  string
	def      string
	required bool
	desc     string
	leaf     bool
}

// walkSchema parses one file's JSON Schema and returns its rows in a stable,
// deterministic order (by path).
func walkSchema(file string) ([]row, error) {
	raw, err := config.Schema(file)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("docgen: parsing schema for %s: %w", file, err)
	}
	props, _ := doc["properties"].(map[string]any)
	required := stringSet(doc["required"])

	var out []row
	walkProps("", props, required, &out)
	sort.SliceStable(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// walkProps recurses into one object's "properties", appending one row per
// property and, for tables and map-tables, the rows of its members.
func walkProps(prefix string, props map[string]any, required map[string]bool, out *[]row) {
	for _, name := range sortedKeys(props) {
		node, _ := props[name].(map[string]any)
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		r := rowFor(path, node, required[name])
		*out = append(*out, r)

		if sub, ok := node["properties"].(map[string]any); ok {
			walkProps(path, sub, stringSet(node["required"]), out)
			continue
		}
		if ap, ok := node["additionalProperties"].(map[string]any); ok {
			if subProps, ok := ap["properties"].(map[string]any); ok {
				// map-table: user-named instances, all of one shape.
				walkProps(path+".<name>", subProps, stringSet(ap["required"]), out)
			}
			// map-enum: additionalProperties is itself {type:string,enum:[...]}
			// with no nested properties — nothing further to recurse into.
		}
	}
}

// rowFor renders one schema node (already at `path`) into a table row.
func rowFor(path string, node map[string]any, required bool) row {
	r := row{path: path, required: required}
	if d, ok := node["description"].(string); ok {
		r.desc = d
	}
	if d, ok := node["default"].(string); ok {
		r.def = d
	}

	typ, _ := node["type"].(string)
	switch typ {
	case "object":
		switch ap := node["additionalProperties"].(type) {
		case bool:
			r.typ = "table"
		case map[string]any:
			if _, hasProps := ap["properties"]; hasProps {
				r.typ = "map-table"
			} else if enum, hasEnum := ap["enum"].([]any); hasEnum {
				r.typ = "map<string,enum>"
				r.allowed = joinAny(enum)
			} else {
				r.typ = "map-table"
			}
		default:
			r.typ = "table"
		}
	case "array":
		r.typ = "list<string>"
		r.leaf = true
	case "string":
		if pattern, ok := node["pattern"].(string); ok && strings.Contains(pattern, "ns|us|ms|s|m|h") {
			r.typ = "duration"
			r.allowed = `Go duration string, e.g. "90s"`
		} else {
			r.typ = "string"
		}
		if enum, ok := node["enum"].([]any); ok {
			r.allowed = joinAny(enum)
		}
		r.leaf = true
	case "integer":
		r.typ = "integer"
		r.leaf = true
	case "boolean":
		r.typ = "boolean"
		r.leaf = true
	default:
		r.typ = typ
		r.leaf = true
	}
	// map-enum keys are leaves too (config.KeyCount counts every field that
	// is not a kTable/kMapTable container).
	if r.typ == "map<string,enum>" {
		r.leaf = true
	}
	return r
}

func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	arr, _ := v.([]any)
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out[s] = true
		}
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func joinAny(vs []any) string {
	ss := make([]string, len(vs))
	for i, v := range vs {
		switch t := v.(type) {
		case string:
			if t == "" {
				t = `""`
			}
			ss[i] = t
		default:
			ss[i] = fmt.Sprint(t)
		}
	}
	return strings.Join(ss, ", ")
}

// countLeaves counts the rows config.KeyCount also counts, for the
// determinism/consistency check: the doc's total must equal the parser's.
func countLeaves(rows []row) int {
	n := 0
	for _, r := range rows {
		if r.leaf {
			n++
		}
	}
	return n
}

// renderConfigMD renders docs/reference/config.md: one table per config
// file, the total key count, then the CFGxxx problem-code table.
func renderConfigMD() (string, error) {
	var b strings.Builder
	b.WriteString("# Config reference\n\n")
	b.WriteString("Generated from `internal/config.Schema` — the same description the parser\n")
	b.WriteString("validates against, so this table can never disagree with `holaclaw check`.\n\n")

	totalLeaves := 0
	for _, file := range config.SchemaFiles() {
		rows, err := walkSchema(file)
		if err != nil {
			return "", err
		}
		totalLeaves += countLeaves(rows)

		fmt.Fprintf(&b, "## `%s`\n\n", file)
		b.WriteString("| Key | Type | Allowed values | Default | Required | Description |\n")
		b.WriteString("|---|---|---|---|---|---|\n")
		for _, r := range rows {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s |\n",
				r.path, r.typ, mdCell(r.allowed), mdCell(r.def), yesNo(r.required), mdCell(r.desc))
		}
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "Total keys: **%d** (from `config.KeyCount()`).\n\n", config.KeyCount())
	if totalLeaves != config.KeyCount() {
		return "", fmt.Errorf("docgen: walked %d leaf keys but config.KeyCount() = %d; the schema walker is out of sync with the parser",
			totalLeaves, config.KeyCount())
	}

	b.WriteString("## Problem codes\n\n")
	b.WriteString("Stable ids `holaclaw check` and `config.Load` report; never reused or\n")
	b.WriteString("repurposed (see `internal/check/RULES.md`).\n\n")
	b.WriteString("| Code | Meaning |\n|---|---|\n")
	for _, cm := range config.Codes() {
		fmt.Fprintf(&b, "| %s | %s |\n", cm.Code, mdCell(cm.Meaning))
	}
	return b.String(), nil
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return ""
}

// mdCell escapes a value for a markdown table cell: pipes and newlines
// would otherwise break the row.
func mdCell(s string) string {
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
