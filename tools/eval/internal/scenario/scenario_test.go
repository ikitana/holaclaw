package scenario

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"holaclaw/internal/tool"
)

func TestLoadShippedScenarios(t *testing.T) {
	ss, err := Load(filepath.Join("..", "..", "..", "..", "testdata", "eval", "scenarios"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(ss) != 10 {
		t.Fatalf("got %d scenarios, want 10", len(ss))
	}
	for _, s := range ss {
		if s.Window() <= 0 {
			t.Errorf("%s: window %d", s.Name, s.Window())
		}
		if len(s.Turns) == 0 {
			t.Errorf("%s: no turns", s.Name)
		}
	}
}

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "s.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFileRejects(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"no turns", `name = "x"`, "no turns"},
		{"bad toml", "name = [", "parse"},
		{
			"bad schema",
			"name=\"x\"\n[[tools]]\nname=\"t\"\nparameters=\"nope\"\n[[turns]]\nuser=\"hi\"\n",
			"not a JSON object",
		},
		{
			"unknown tool in expectation",
			"name=\"x\"\n[[turns]]\nuser=\"hi\"\n[[turns.expect.tool_calls]]\nname=\"ghost\"\n",
			"unmocked tool",
		},
		{
			"bad regex",
			"name=\"x\"\n[[turns]]\nuser=\"hi\"\n[turns.expect]\nreply_regex=\"(\"\n",
			"reply_regex",
		},
		{
			"empty user",
			"name=\"x\"\n[[turns]]\nuser=\"  \"\n",
			"no user text",
		},
		{
			"bad risk",
			"name=\"x\"\n[[tools]]\nname=\"t\"\nparameters=\"{}\"\nrisk=\"nuclear\"\n[[turns]]\nuser=\"hi\"\n",
			"unknown risk",
		},
		{
			"skill without description",
			"name=\"x\"\n[[skills]]\nname=\"s\"\n[[turns]]\nuser=\"hi\"\n",
			"a skill needs",
		},
		{
			"contradictory expectations",
			"name=\"x\"\n[[tools]]\nname=\"t\"\nparameters=\"{}\"\n[[turns]]\nuser=\"hi\"\n[turns.expect]\nno_tool_calls=true\n[[turns.expect.tool_calls]]\nname=\"t\"\n",
			"no_tool_calls",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadFile(write(t, c.body))
			if err == nil {
				t.Fatal("want error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestLoadFileRejectsOversizedSchema(t *testing.T) {
	params, err := json.Marshal(map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "string", "description": strings.Repeat("very long field docs ", 60)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := "name=\"x\"\n[[tools]]\nname=\"t\"\nparameters='''" + string(params) + "'''\n[[turns]]\nuser=\"hi\"\n"
	_, err = LoadFile(write(t, body))
	if err == nil || !strings.Contains(err.Error(), "cap is") {
		t.Fatalf("got %v, want a schema cap error (cap %d)", err, tool.MaxSchemaTokens)
	}
}

func TestLoadRejectsDuplicateNames(t *testing.T) {
	dir := t.TempDir()
	body := "name=\"dup\"\n[[turns]]\nuser=\"hi\"\n"
	for _, n := range []string{"a.toml", "b.toml"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("got %v, want a duplicate-name error", err)
	}
}

func TestLoadEmptyDir(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("want an error for a directory with no scenarios")
	}
}

func TestMatches(t *testing.T) {
	args := map[string]any{"city": "Mirefield", "size": 3.0, "hot": true, "note": nil}
	cases := []struct {
		name string
		when map[string]string
		want bool
	}{
		{"exact string", map[string]string{"city": "Mirefield"}, true},
		{"exact string mismatch", map[string]string{"city": "mirefield"}, false},
		{"substring", map[string]string{"city": "~fiel"}, true},
		{"substring case-insensitive", map[string]string{"city": "~MIRE"}, true},
		{"substring mismatch", map[string]string{"city": "~lake"}, false},
		{"integer rendered without decimals", map[string]string{"size": "3"}, true},
		{"bool", map[string]string{"hot": "true"}, true},
		{"null", map[string]string{"note": ""}, true},
		{"missing arg", map[string]string{"ghost": "x"}, false},
		{"all conditions must hold", map[string]string{"city": "Mirefield", "size": "4"}, false},
		{"empty when", nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Matches(c.when, args); got != c.want {
				t.Fatalf("Matches(%v) = %v, want %v", c.when, got, c.want)
			}
		})
	}
}

func TestPick(t *testing.T) {
	rs := []Response{
		{Text: "default"},
		{When: map[string]string{"city": "~mire"}, Text: "hit"},
	}
	if r, _ := Pick(rs, map[string]any{"city": "Mirefield"}); r.Text != "hit" {
		t.Fatalf("got %q, want the conditional response even when the default comes first", r.Text)
	}
	if r, _ := Pick(rs, map[string]any{"city": "Elsewhere"}); r.Text != "default" {
		t.Fatalf("got %q, want the default", r.Text)
	}
	if _, ok := Pick([]Response{{When: map[string]string{"a": "b"}}}, nil); ok {
		t.Fatal("want no response when nothing matches and there is no default")
	}
}

func TestExecutor(t *testing.T) {
	s, err := LoadFile(write(t, `
name = "x"
[[tools]]
name = "weather"
description = "Weather."
parameters = '''{"type":"object","properties":{"city":{"type":"string"}}}'''
risk = "external"
  [[tools.responses]]
  when = { city = "~mire" }
  text = "rain"
  tainted = true
  [[tools.responses]]
  is_error = true
  text = "unknown city"
[[turns]]
user = "hi"
`))
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewExecutor(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Specs()) != 1 || e.Specs()[0].Name != "weather" {
		t.Fatalf("specs = %v", e.Specs())
	}
	mock, ok := e.Lookup("weather")
	if !ok || mock.Risk() != tool.RiskExternal {
		t.Fatalf("lookup = %v %v", mock, ok)
	}

	res, err := e.Execute(context.Background(), tool.Call{Name: "weather", Args: json.RawMessage(`{"city":"Mirefield"}`)})
	if err != nil || res.Text != "rain" || !res.Tainted || res.IsError {
		t.Fatalf("result = %+v, err %v", res, err)
	}
	res, err = e.Execute(context.Background(), tool.Call{Name: "weather", Args: json.RawMessage(`{"city":"Elsewhere"}`)})
	if err != nil || !res.IsError {
		t.Fatalf("result = %+v, err %v", res, err)
	}
	res, _ = e.Execute(context.Background(), tool.Call{Name: "ghost", Args: json.RawMessage(`{}`)})
	if !res.IsError {
		t.Fatalf("unknown tool result = %+v", res)
	}
	if got := e.Calls(); len(got) != 3 || got[0].Args["city"] != "Mirefield" {
		t.Fatalf("calls = %+v", got)
	}
	e.Reset()
	if len(e.Calls()) != 0 {
		t.Fatal("Reset did not clear the calls")
	}
}

func TestLoadSkills(t *testing.T) {
	s, err := LoadFile(write(t, "name=\"x\"\n[[skills]]\nname=\"s\"\ndescription=\"When s.\"\n[[turns]]\nuser=\"hi\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Skills) != 1 || s.Skills[0] != (Skill{Name: "s", Description: "When s."}) {
		t.Fatalf("skills = %+v", s.Skills)
	}
}
