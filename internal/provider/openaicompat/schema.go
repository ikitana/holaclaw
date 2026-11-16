package openaicompat

import "encoding/json"

// SchemaProfileLlamaCPP strips JSON-schema keywords that grammar-constrained
// samplers (llama.cpp and friends) either reject or turn into unusable
// grammars.
const SchemaProfileLlamaCPP = "llamacpp"

// grammarHostile are the keywords removed under the llamacpp profile.
var grammarHostile = map[string]bool{
	"$schema":              true,
	"$id":                  true,
	"$comment":             true,
	"additionalProperties": true,
	"pattern":              true,
	"patternProperties":    true,
	"format":               true,
	"minLength":            true,
	"maxLength":            true,
	"minItems":             true,
	"maxItems":             true,
	"minProperties":        true,
	"maxProperties":        true,
	"uniqueItems":          true,
	"minimum":              true,
	"maximum":              true,
	"exclusiveMinimum":     true,
	"exclusiveMaximum":     true,
	"multipleOf":           true,
	"contentEncoding":      true,
	"contentMediaType":     true,
}

// sanitizeSchema returns the schema to put on the wire for the given profile.
// The input is never mutated: unknown profiles pass through untouched, the
// llamacpp profile gets a stripped deep copy.
func sanitizeSchema(raw json.RawMessage, profile string) json.RawMessage {
	if profile != SchemaProfileLlamaCPP || len(raw) == 0 {
		return raw
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw // not our problem: send it as the tool declared it
	}
	out, err := json.Marshal(stripSchema(v))
	if err != nil {
		return raw
	}
	return out
}

// stripSchema deep-copies v, dropping grammar-hostile keywords recursively.
func stripSchema(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if grammarHostile[k] {
				continue
			}
			// "properties" keys are field names, not keywords: keep them all.
			if k == "properties" || k == "$defs" || k == "definitions" {
				if m, ok := val.(map[string]any); ok {
					sub := make(map[string]any, len(m))
					for name, s := range m {
						sub[name] = stripSchema(s)
					}
					out[k] = sub
					continue
				}
			}
			out[k] = stripSchema(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = stripSchema(e)
		}
		return out
	default:
		return v
	}
}
