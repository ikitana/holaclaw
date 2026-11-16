package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCodes checks the doc accessor added for tools/docgen: every code it
// lists is one of the CFGxxx constants, in order, with no duplicates or
// blanks, and it matches problem.go's constant count.
func TestCodes(t *testing.T) {
	want := []string{
		CodeUnknownKey, CodeWrongFile, CodeWrongType, CodeBadEnum, CodeMissingRequired,
		CodeDanglingRef, CodeBadVersion, CodeVersionMismatch, CodeLiteralSecret,
		CodeUnknownTool, CodeBadValue, CodeMissingFile,
	}
	got := Codes()
	if len(got) != len(want) {
		t.Fatalf("Codes() returned %d entries, want %d", len(got), len(want))
	}
	seen := map[string]bool{}
	for i, cm := range got {
		if cm.Code != want[i] {
			t.Errorf("Codes()[%d].Code = %q, want %q", i, cm.Code, want[i])
		}
		if cm.Meaning == "" {
			t.Errorf("Codes()[%d] (%s) has an empty Meaning", i, cm.Code)
		}
		if seen[cm.Code] {
			t.Errorf("Codes() lists %s more than once", cm.Code)
		}
		seen[cm.Code] = true
	}
}

// TestKeyCountPositive is a smoke test: KeyCount must track the schema, not
// drift to zero or negative from a refactor.
func TestKeyCountPositive(t *testing.T) {
	if n := KeyCount(); n <= 0 {
		t.Fatalf("KeyCount() = %d, want > 0", n)
	}
}

// TestSchemaExportsNewKinds pins what the float, URL and conditional-required
// keys of M6 / M7 look like in the exported JSON Schema.
func TestSchemaExportsNewKinds(t *testing.T) {
	agent := decodeSchema(t, FileAgent)
	policy := object(t, object(t, object(t, agent, "properties"), "policy"), "properties")
	thr := object(t, policy, "guard_threshold")
	if thr["type"] != "number" || thr["exclusiveMinimum"] != 0.0 || thr["maximum"] != 1.0 {
		t.Errorf("guard_threshold = %v", thr)
	}
	if mode := object(t, policy, "guard_mode"); mode["default"] != "recommend" {
		t.Errorf("guard_mode = %v", mode)
	}
	limits := object(t, object(t, object(t, agent, "properties"), "limits"), "properties")
	if img := object(t, limits, "images_per_day"); img["type"] != "integer" || img["minimum"] != 0.0 {
		t.Errorf("images_per_day = %v", img)
	}

	cfgDoc := decodeSchema(t, FileConfig)
	models := object(t, object(t, object(t, cfgDoc, "properties"), "models"), "additionalProperties")
	if u := object(t, object(t, models, "properties"), "url"); !strings.Contains(u["pattern"].(string), "https?") {
		t.Errorf("url = %v", u)
	}
	// `context` and `url` are conditionally required, so they are not in the
	// unconditional list but are covered by allOf if/then pairs.
	for _, k := range []string{"context", "url"} {
		for _, r := range models["required"].([]any) {
			if r == k {
				t.Errorf("%q is listed as unconditionally required", k)
			}
		}
	}
	all, _ := models["allOf"].([]any)
	if len(all) != 2 {
		t.Fatalf("allOf = %v, want one if/then per conditional key", all)
	}
}

// TestSchemaExportsBrowserKinds pins the M8 additions: kPath and kHostList.
func TestSchemaExportsBrowserKinds(t *testing.T) {
	cfgDoc := decodeSchema(t, FileConfig)
	browser := object(t, object(t, cfgDoc, "properties"), "browser")
	bprops := object(t, browser, "properties")
	socket := object(t, bprops, "socket")
	if socket["pattern"] != "^/" {
		t.Errorf("socket = %v", socket)
	}
	if req, _ := browser["required"].([]any); len(req) != 0 {
		t.Errorf("browser required = %v, want none: socket has a default", browser["required"])
	}
	binary := object(t, bprops, "binary")
	if binary["pattern"] != "^/" {
		t.Errorf("binary = %v", binary)
	}
	headless := object(t, bprops, "headless")
	if headless["type"] != "boolean" || headless["default"] != "false" {
		t.Errorf("headless = %v", headless)
	}

	agent := decodeSchema(t, FileAgent)
	abrowser := object(t, object(t, agent, "properties"), "browser")
	allow := object(t, object(t, abrowser, "properties"), "allow")
	if allow["type"] != "array" {
		t.Errorf("allow = %v", allow)
	}
	items := object(t, allow, "items")
	if _, ok := items["pattern"].(string); !ok {
		t.Errorf("allow items = %v, want a host-name pattern", items)
	}

	models := object(t, object(t, agent, "properties"), "models")
	decide := object(t, object(t, models, "properties"), "decide")
	if decide["type"] != "string" {
		t.Errorf("decide = %v", decide)
	}
}

func decodeSchema(t *testing.T, file string) map[string]any {
	t.Helper()
	b, err := Schema(file)
	if err != nil {
		t.Fatalf("schema %s: %v", file, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("schema %s: %v", file, err)
	}
	return doc
}

func object(t *testing.T, m map[string]any, k string) map[string]any {
	t.Helper()
	sub, ok := m[k].(map[string]any)
	if !ok {
		t.Fatalf("%q is %T, want an object", k, m[k])
	}
	return sub
}

// TestSchemaFilesKnown checks every name SchemaFiles lists is a name Schema
// actually accepts.
func TestSchemaFilesKnown(t *testing.T) {
	for _, f := range SchemaFiles() {
		if _, err := Schema(f); err != nil {
			t.Errorf("Schema(%q) failed: %v", f, err)
		}
	}
}
