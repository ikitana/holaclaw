package main

import (
	"strings"
	"testing"

	"holaclaw/internal/config"
)

func TestWalkSchemaConfigFile(t *testing.T) {
	rows, err := walkSchema(config.FileConfig)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("walkSchema returned no rows for config.toml")
	}

	byPath := map[string]row{}
	for _, r := range rows {
		byPath[r.path] = r
	}

	version, ok := byPath["version"]
	if !ok {
		t.Fatal("missing row for \"version\"")
	}
	if version.typ != "integer" || !version.required || !version.leaf {
		t.Errorf("version row = %+v, want integer/required/leaf", version)
	}

	// server is a fixed table: documented, not a leaf, not counted.
	server, ok := byPath["server"]
	if !ok || server.typ != "table" || server.leaf {
		t.Errorf("server row = %+v, want table/not-leaf", server)
	}
	if _, ok := byPath["server.data_dir"]; !ok {
		t.Error("missing nested row \"server.data_dir\"")
	}

	// providers is a map-table: walked through its "<name>" placeholder.
	providers, ok := byPath["providers"]
	if !ok || providers.typ != "map-table" {
		t.Errorf("providers row = %+v, want map-table", providers)
	}
	pType, ok := byPath["providers.<name>.type"]
	if !ok {
		t.Fatal("missing nested row \"providers.<name>.type\"")
	}
	if pType.typ != "string" || !pType.required || !pType.leaf {
		t.Errorf("providers.<name>.type = %+v, want required leaf string", pType)
	}
	if !strings.Contains(pType.allowed, "openai-compatible") {
		t.Errorf("providers.<name>.type allowed = %q, want it to list openai-compatible", pType.allowed)
	}
}

func TestWalkSchemaAgentFile(t *testing.T) {
	rows, err := walkSchema(config.FileAgent)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]row{}
	for _, r := range rows {
		byPath[r.path] = r
	}

	// policy.tools is a map-enum: user tool names with an enum value.
	toolsField, ok := byPath["policy.tools"]
	if !ok {
		t.Fatal("missing row \"policy.tools\"")
	}
	if toolsField.typ != "map<string,enum>" || !toolsField.leaf {
		t.Errorf("policy.tools row = %+v, want leaf map<string,enum>", toolsField)
	}
	if !strings.Contains(toolsField.allowed, "allow") || !strings.Contains(toolsField.allowed, "deny") {
		t.Errorf("policy.tools allowed = %q, want allow/ask/deny", toolsField.allowed)
	}

	// duration field.
	limits, ok := byPath["limits"]
	if !ok || limits.typ != "table" {
		t.Errorf("limits row = %+v, want table", limits)
	}
}

func TestWalkSchemaChannelsFile(t *testing.T) {
	rows, err := walkSchema(config.FileChannels)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.path == "channels.<name>.access.dm" {
			found = true
			if !strings.Contains(r.allowed, "invite") {
				t.Errorf("channels.<name>.access.dm allowed = %q, want invite/request/open", r.allowed)
			}
		}
	}
	if !found {
		t.Fatal("missing row \"channels.<name>.access.dm\"")
	}
}

// TestKeyCountMatchesWalker is the invariant the doc depends on: the
// generator's leaf count must equal config.KeyCount(), the same number
// ARCHITECTURE.md §1 tracks in CI, or the doc could silently drift from the
// parser.
func TestKeyCountMatchesWalker(t *testing.T) {
	total := 0
	for _, f := range config.SchemaFiles() {
		rows, err := walkSchema(f)
		if err != nil {
			t.Fatal(err)
		}
		total += countLeaves(rows)
	}
	if total != config.KeyCount() {
		t.Errorf("walked %d leaf keys, config.KeyCount() = %d", total, config.KeyCount())
	}
}

func TestWalkSchemaUnknownFile(t *testing.T) {
	if _, err := walkSchema("nonexistent.toml"); err == nil {
		t.Fatal("want an error for an unknown schema file")
	}
}

func TestRenderConfigMDDeterministic(t *testing.T) {
	a, err := renderConfigMD()
	if err != nil {
		t.Fatal(err)
	}
	b, err := renderConfigMD()
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Error("renderConfigMD is not deterministic across two runs")
	}
	if !strings.Contains(a, "Problem codes") {
		t.Error("renderConfigMD output is missing the problem-code table")
	}
	if !strings.Contains(a, "CFG001") {
		t.Error("renderConfigMD output is missing CFG001")
	}
}
