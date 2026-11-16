package wire

import "testing"

func TestRepairArgumentsStages(t *testing.T) {
	cases := []struct {
		name, in, want, stage string
	}{
		{"strict", `{"a":1}`, `{"a":1}`, ""},
		{"strict whitespace", ` {"a": 1} `, ` {"a": 1} `, ""},
		{"empty", "", "{}", ""},
		{"blank", "   ", "{}", ""},
		{"fenced", "```json\n{\"a\":1}\n```", `{"a":1}`, StageLenient},
		{"fenced bare", "```\n{\"a\":1}\n```", `{"a":1}`, StageLenient},
		{"prose around", `Sure! {"a":1} hope that helps`, `{"a":1}`, StageLenient},
		{"trailing comma", `{"a":1,}`, `{"a":1}`, StageTrailingComma},
		{"nested trailing comma", `{"a":[1,2,],"b":{"c":3,},}`, `{"a":[1,2],"b":{"c":3}}`, StageTrailingComma},
		{"comma in string kept", `{"a":"x,}",}`, `{"a":"x,}"}`, StageTrailingComma},
		{"unclosed brace", `{"a":1`, `{"a":1}`, StageAutoClose},
		{"unclosed string", `{"a":"bc`, `{"a":"bc"}`, StageAutoClose},
		{"unclosed array", `{"a":[1,2`, `{"a":[1,2]}`, StageAutoClose},
		{"dangling comma and brace", `{"a":1,`, `{"a":1}`, StageAutoClose},
		{"dangling key", `{"a":`, `{"a":null}`, StageAutoClose},
		{"array not object", `[1,2]`, "{}", StageEmpty},
		{"garbage", `I cannot do that`, "{}", StageEmpty},
		{"scalar", `42`, "{}", StageEmpty},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, stage := RepairArguments(tc.in)
			if got != tc.want || stage != tc.stage {
				t.Errorf("repair(%q) = (%q, %q) want (%q, %q)", tc.in, got, stage, tc.want, tc.stage)
			}
			if !IsJSONObject(got) {
				t.Errorf("result %q is not a JSON object", got)
			}
		})
	}
}

func TestIsJSONObject(t *testing.T) {
	cases := map[string]bool{
		`{}`:            true,
		`{"a":1}`:       true,
		`[1]`:           false,
		`null`:          false,
		`{"a":1} extra`: false,
		`"str"`:         false,
	}
	for in, want := range cases {
		if got := IsJSONObject(in); got != want {
			t.Errorf("IsJSONObject(%q) = %v", in, got)
		}
	}
}
