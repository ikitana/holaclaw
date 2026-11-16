package tools

import (
	"encoding/json"
	"fmt"
)

// object is the JSON Schema of a tool's arguments. Specs build it from Go
// values and marshal it, rather than writing JSON by hand, so a spec cannot
// be malformed. Properties marshal in name order.
type object struct {
	Properties map[string]prop
	Required   []string
}

// prop is one argument of a tool. The zero prop accepts any value: specs
// short on schema tokens leave the type out and check in Run.
type prop struct {
	Type        propType `json:"type,omitempty"`
	Description string   `json:"description,omitempty"`
	Enum        []string `json:"enum,omitempty"`
	Items       *prop    `json:"items,omitempty"` // for tArray
}

type propType string

const (
	tString  propType = "string"
	tInteger propType = "integer"
	tBoolean propType = "boolean"
	tArray   propType = "array"
)

// raw marshals the schema for provider.ToolSpec.Parameters. It panics when a
// required name has no property: that is a bug in the spec, caught by the
// first test that builds it.
func (o object) raw() json.RawMessage {
	for _, r := range o.Required {
		if _, ok := o.Properties[r]; !ok {
			panic(fmt.Sprintf("tools: required %q is not a property", r))
		}
	}
	props := o.Properties
	if props == nil {
		props = map[string]prop{}
	}
	b, err := json.Marshal(struct {
		Type       string          `json:"type"`
		Properties map[string]prop `json:"properties"`
		Required   []string        `json:"required,omitempty"`
	}{"object", props, o.Required})
	if err != nil {
		panic(fmt.Sprintf("tools: marshal schema: %v", err)) // strings, slices and maps only
	}
	return b
}
