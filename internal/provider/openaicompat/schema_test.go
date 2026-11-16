package openaicompat

import (
	"encoding/json"
	"testing"
)

const toolSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "path":    {"type": "string", "pattern": "^/", "maxLength": 4096, "description": "file"},
    "format":  {"type": "string", "format": "date-time"},
    "tags":    {"type": "array", "maxItems": 10, "items": {"type": "string", "minLength": 1}},
    "nested":  {"type": "object", "properties": {"deep": {"type": "string", "pattern": "x"}}}
  },
  "required": ["path"]
}`

func TestSanitizeSchemaLlamaCPP(t *testing.T) {
	raw := json.RawMessage(toolSchema)
	original := string(raw)

	out := sanitizeSchema(raw, SchemaProfileLlamaCPP)

	if string(raw) != original {
		t.Fatal("the input schema was mutated")
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["$schema"]; ok {
		t.Error("$schema kept")
	}
	if _, ok := got["additionalProperties"]; ok {
		t.Error("additionalProperties kept")
	}
	if got["type"] != "object" {
		t.Errorf("type = %v", got["type"])
	}
	if req, _ := got["required"].([]any); len(req) != 1 || req[0] != "path" {
		t.Errorf("required = %v", got["required"])
	}
	props := got["properties"].(map[string]any)
	// A property literally named "format" must survive as a property name.
	if _, ok := props["format"]; !ok {
		t.Error("the property named \"format\" was dropped")
	}
	path := props["path"].(map[string]any)
	for _, k := range []string{"pattern", "maxLength"} {
		if _, ok := path[k]; ok {
			t.Errorf("path.%s kept", k)
		}
	}
	if path["description"] != "file" || path["type"] != "string" {
		t.Errorf("path lost useful keywords: %v", path)
	}
	if f := props["format"].(map[string]any); f["format"] != nil {
		t.Error("format keyword kept inside a property")
	}
	tags := props["tags"].(map[string]any)
	if _, ok := tags["maxItems"]; ok {
		t.Error("maxItems kept")
	}
	items := tags["items"].(map[string]any)
	if _, ok := items["minLength"]; ok {
		t.Error("minLength kept in items")
	}
	deep := props["nested"].(map[string]any)["properties"].(map[string]any)["deep"].(map[string]any)
	if _, ok := deep["pattern"]; ok {
		t.Error("pattern kept in a nested property")
	}
}

func TestSanitizeSchemaOtherProfilesPassThrough(t *testing.T) {
	raw := json.RawMessage(`{"type":"object","pattern":"x"}`)
	if got := string(sanitizeSchema(raw, "")); got != string(raw) {
		t.Errorf("default profile changed the schema: %s", got)
	}
	if got := string(sanitizeSchema(raw, "openai")); got != string(raw) {
		t.Errorf("unknown profile changed the schema: %s", got)
	}
	bad := json.RawMessage(`not json`)
	if got := string(sanitizeSchema(bad, SchemaProfileLlamaCPP)); got != "not json" {
		t.Errorf("unparsable schema changed: %s", got)
	}
}

func TestBuildRequestAppliesSchemaProfile(t *testing.T) {
	raw := json.RawMessage(toolSchema)
	req := buildRequestFixture(raw, SchemaProfileLlamaCPP)
	if len(req.Tools) != 1 {
		t.Fatalf("tools = %+v", req.Tools)
	}
	var params map[string]any
	if err := json.Unmarshal(req.Tools[0].Function.Parameters, &params); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := params["$schema"]; ok {
		t.Error("schema profile not applied on the wire")
	}
	if string(raw) != toolSchema {
		t.Error("the tool spec was mutated")
	}
}
