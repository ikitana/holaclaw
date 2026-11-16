package tools

import "testing"

func TestObjectRaw(t *testing.T) {
	got := string(object{
		Properties: map[string]prop{
			"b": {Type: tInteger},
			"a": {Type: tString, Description: "say \"hi\"", Enum: []string{"x", "y"}},
			"c": {Type: tArray, Items: &prop{}},
			"d": {},
		},
		Required: []string{"a"},
	}.raw())
	want := `{"type":"object","properties":{"a":{"type":"string","description":"say \"hi\"","enum":["x","y"]},"b":{"type":"integer"},` +
		`"c":{"type":"array","items":{}},"d":{}},"required":["a"]}`
	if got != want {
		t.Fatalf("raw =\n%s\nwant\n%s", got, want)
	}
	if got := string(object{}.raw()); got != `{"type":"object","properties":{}}` {
		t.Fatalf("empty raw = %s", got)
	}
}

func TestObjectRawUnknownRequired(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a required name with no property did not panic")
		}
	}()
	object{Properties: map[string]prop{"a": {Type: tString}}, Required: []string{"b"}}.raw()
}
