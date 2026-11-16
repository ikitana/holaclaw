package harness

import (
	"strings"
	"testing"

	"holaclaw/tools/eval/internal/scenario"
)

func callOf(name string, args map[string]any) scenario.Call {
	return scenario.Call{Name: name, Args: args, Raw: "{}"}
}

func TestCheck(t *testing.T) {
	obs := Observed{
		Calls: []scenario.Call{
			callOf("weather", map[string]any{"city": "Mirefield"}),
			callOf("brew", map[string]any{"size": 3.0, "kind": "rooibos"}),
		},
		Reply:        "Brewing 3 cups in Mirefield.",
		Steps:        3,
		PromptTokens: 900,
		Stop:         "final",
	}
	cases := []struct {
		name   string
		expect scenario.Expect
		obs    Observed
		want   string // "" means the turn must pass
	}{
		{"ordered calls match", scenario.Expect{ToolCalls: []scenario.CallExpect{
			{Name: "weather", Args: map[string]any{"city": "Mirefield"}},
			{Name: "brew", Args: map[string]any{"size": int64(3), "kind": "rooibos"}},
		}}, obs, ""},
		{"ordered calls in the wrong order", scenario.Expect{ToolCalls: []scenario.CallExpect{
			{Name: "brew"}, {Name: "weather"},
		}}, obs, "expected weather, got brew"},
		{"wrong number of calls", scenario.Expect{ToolCalls: []scenario.CallExpect{{Name: "weather"}}}, obs,
			"expected 1 tool calls"},
		{"argument mismatch", scenario.Expect{ToolCalls: []scenario.CallExpect{
			{Name: "weather", Args: map[string]any{"city": "Elsewhere"}},
			{Name: "brew"},
		}}, obs, `argument "city"`},
		{"missing argument", scenario.Expect{ToolCalls: []scenario.CallExpect{
			{Name: "weather", Args: map[string]any{"units": "c"}},
			{Name: "brew"},
		}}, obs, "missing"},
		{"any order", scenario.Expect{ToolCallsAnyOrder: []scenario.CallExpect{
			{Name: "brew", Args: map[string]any{"size": 3}}, {Name: "weather"},
		}}, obs, ""},
		{"any order, absent call", scenario.Expect{ToolCallsAnyOrder: []scenario.CallExpect{
			{Name: "ghost"},
		}}, obs, "expected a call to ghost"},
		{"any order does not reuse one call twice", scenario.Expect{ToolCallsAnyOrder: []scenario.CallExpect{
			{Name: "brew"}, {Name: "brew"},
		}}, obs, "expected a call to brew"},
		{"no tool calls", scenario.Expect{NoToolCalls: true}, obs, "expected no tool calls"},
		{"no tool calls, none made", scenario.Expect{NoToolCalls: true}, Observed{Reply: "hi"}, ""},
		{"forbidden tool", scenario.Expect{ForbidTools: []string{"brew", "ghost"}}, obs, "forbidden tool brew"},
		{"reply contains, case-insensitive", scenario.Expect{ReplyContains: []string{"MIREFIELD"}}, obs, ""},
		{"reply does not contain", scenario.Expect{ReplyContains: []string{"tea"}}, obs, `does not contain "tea"`},
		{"reply not contains", scenario.Expect{ReplyNotContains: []string{"brewing"}}, obs, `contains "brewing"`},
		{"regex", scenario.Expect{ReplyRegex: `\d+ cups`}, obs, ""},
		{"regex mismatch", scenario.Expect{ReplyRegex: `^never`}, obs, "does not match"},
		{"step limit", scenario.Expect{MaxSteps: 2}, obs, "took 3 steps"},
		{"step limit met", scenario.Expect{MaxSteps: 3}, obs, ""},
		{"prompt budget", scenario.Expect{MaxPromptTokens: 800}, obs, "prompt was 900 tokens"},
		{"stop reason", scenario.Expect{Stop: "step_limit"}, obs, `stopped with "final"`},
		{"nothing asserted", scenario.Expect{}, obs, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Check(c.expect, c.obs)
			if c.want == "" {
				if len(got) != 0 {
					t.Fatalf("want a pass, got %v", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("want a failure mentioning %q, got a pass", c.want)
			}
			if !strings.Contains(strings.Join(got, "\n"), c.want) {
				t.Fatalf("failures %v, want one mentioning %q", got, c.want)
			}
		})
	}
}

func TestEqualValueNumberTypes(t *testing.T) {
	if !equalValue(int64(3), 3.0) || !equalValue(3, 3.0) || !equalValue(3.0, 3.0) {
		t.Fatal("TOML integers must compare equal to JSON numbers")
	}
	if equalValue(int64(3), "3") || equalValue("3", 3.0) {
		t.Fatal("a number must not compare equal to a string")
	}
	if !equalValue(true, true) || equalValue(true, false) {
		t.Fatal("bools compare by value")
	}
}
