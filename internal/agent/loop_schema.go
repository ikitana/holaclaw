package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// canonicalArgs re-marshals a JSON argument object so that equal arguments
// have equal text: encoding/json sorts map keys at every depth. Unparsable
// arguments are returned unchanged.
func canonicalArgs(args string) string {
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return args
	}
	b, err := json.Marshal(v)
	if err != nil {
		return args
	}
	return string(b)
}

// validateArgs checks arguments against a tool's parameter JSON Schema. It is
// deliberately small: required, type (string, number, integer, boolean,
// array, object) and enum; every other keyword is ignored. The error names
// the offending field so it can be fed back to the model.
func validateArgs(schema json.RawMessage, args json.RawMessage) error {
	if len(schema) == 0 {
		return nil
	}
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil // an unusable schema is not the model's fault
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return fmt.Errorf("arguments are not JSON: %w", err)
	}
	return validateValue(s, v, "")
}

func validateValue(schema map[string]any, v any, path string) error {
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		found := false
		for _, e := range enum {
			if reflect.DeepEqual(e, v) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("field %s must be one of: %s", fieldName(path), enumText(enum))
		}
	}
	if t, ok := schema["type"].(string); ok {
		if err := checkType(t, v, path); err != nil {
			return err
		}
	}
	if obj, ok := v.(map[string]any); ok {
		for _, req := range stringList(schema["required"]) {
			if _, has := obj[req]; !has {
				return fmt.Errorf("missing required field %s", fieldName(join(path, req)))
			}
		}
		props, _ := schema["properties"].(map[string]any)
		for name, sub := range props {
			ps, ok := sub.(map[string]any)
			if !ok {
				continue
			}
			val, has := obj[name]
			if !has {
				continue
			}
			if err := validateValue(ps, val, join(path, name)); err != nil {
				return err
			}
		}
	}
	if arr, ok := v.([]any); ok {
		if items, ok := schema["items"].(map[string]any); ok {
			for i, el := range arr {
				if err := validateValue(items, el, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func checkType(t string, v any, path string) error {
	var ok bool
	switch t {
	case "string":
		_, ok = v.(string)
	case "number":
		_, ok = v.(float64)
	case "integer":
		f, isNum := v.(float64)
		ok = isNum && f == float64(int64(f))
	case "boolean":
		_, ok = v.(bool)
	case "array":
		_, ok = v.([]any)
	case "object":
		_, ok = v.(map[string]any)
	default:
		return nil // null and unknown types are not enforced
	}
	if !ok {
		return fmt.Errorf("field %s must be %s %s", fieldName(path), article(t), t)
	}
	return nil
}

func article(t string) string {
	switch t {
	case "array", "object", "integer":
		return "an"
	default:
		return "a"
	}
}

func fieldName(path string) string {
	if path == "" {
		return "(arguments)"
	}
	return quote(path)
}

func quote(s string) string { return `"` + s + `"` }

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func stringList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func enumText(enum []any) string {
	parts := make([]string, 0, len(enum))
	for _, e := range enum {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		parts = append(parts, strings.Trim(string(b), `"`))
	}
	return strings.Join(parts, ", ")
}
