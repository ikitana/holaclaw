package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestParseFullYAML loads frontmatter that only a full YAML parser accepts:
// quoting styles, block scalars, any indentation, anchors, block mappings in
// lists and flow collections across lines.
func TestParseFullYAML(t *testing.T) {
	cases := []struct {
		name, front, desc string
		bins              []Bin
	}{
		{"single-quoted description", "name: 'x'\ndescription: 'It''s when x.'\n", "It's when x.", nil},
		{"folded description", "name: x\ndescription: >-\n  When\n  x.\n", "When x.", nil},
		{"indented first key", "  name: x\n  description: When x.\n", "When x.", nil},
		{"four-space indent", "name: x\ndescription: When x.\nmetadata:\n    holaclaw:\n        version: 1\n        requires:\n            bins: [rg]\n", "When x.", []Bin{{"rg", "rg"}}},
		{"list at key indent", "name: x\ndescription: When x.\nmetadata:\n  holaclaw:\n    version: 1\n    requires:\n      bins:\n      - rg\n", "When x.", []Bin{{"rg", "rg"}}},
		{"block mapping in bins", "name: x\ndescription: When x.\nmetadata:\n  holaclaw:\n    version: 1\n    requires:\n      bins:\n        - name: rg\n          pkg: ripgrep\n", "When x.", []Bin{{"rg", "ripgrep"}}},
		{"anchor and alias", "name: &n x\ndescription: When x.\nmetadata:\n  holaclaw:\n    version: 1\n    requires:\n      bins: [*n]\n", "When x.", []Bin{{"x", "x"}}},
		{"metadata without value", "name: x\ndescription: When x.\nmetadata:\n", "When x.", nil},
		{"holaclaw without value", "name: x\ndescription: When x.\nmetadata:\n  holaclaw:\n", "When x.", nil},
		{"multi-line JSON metadata", "name: x\ndescription: When x.\nmetadata: {\n  \"openclaw\": {\"emoji\": 'x', \"requires\": {\"config\": [\n    \"a\", \"b\"]}},\n  \"\": 1, a b: 2\n}\n", "When x.", nil},
		{"block list of maps in foreign metadata", "name: x\ndescription: When x.\nmetadata:\n  openclaw:\n    install:\n      - kind: brew\n        formula: fly\n", "When x.", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sk, err := Parse(LevelAgent, "x", []byte("---\n"+c.front+"---\nbody\n"))
			if err != nil {
				t.Fatal(err)
			}
			if sk.Name != "x" || sk.Description != c.desc || !slices.Equal(sk.Requires.Bins, c.bins) {
				t.Fatalf("skill = %+v", sk)
			}
		})
	}
}

// TestParseNasty checks that hostile frontmatter returns a syntax error
// quickly instead of crashing or hanging the daemon at prompt build.
func TestParseNasty(t *testing.T) {
	// A billion-laughs chain: nine levels of nine aliases each.
	var bomb strings.Builder
	bomb.WriteString("a0: &a0 [lol]\n")
	for i := 1; i <= 9; i++ {
		fmt.Fprintf(&bomb, "a%d: &a%d [", i, i)
		for j := range 9 {
			if j > 0 {
				bomb.WriteString(", ")
			}
			fmt.Fprintf(&bomb, "*a%d", i-1)
		}
		bomb.WriteString("]\n")
	}
	laughs := strings.ReplaceAll(bomb.String(), "\n", "\n  ")
	cases := []struct {
		name, front string
		code        Code
		msg         string
	}{
		{"deep flow nesting", "name: x\ndescription: " + strings.Repeat("[", 20000) + "\n", CodeSyntax, "exceeded max depth"},
		// Compact nested lists, two bytes a level: deep and still under maxFile.
		{"deep block nesting", "name: x\ndescription: y\nmetadata:\n  k:\n    " + strings.Repeat("- ", 12000) + "x\n", CodeSyntax, "exceeded max depth"},
		// Where holaclaw decodes it, the first entry is already the wrong type.
		{"alias bomb in requires", "name: x\ndescription: When x.\nmetadata:\n  " + laughs + "holaclaw:\n    version: 1\n    requires:\n      tools: *a9\n", CodeBadValue, "expected a scalar, got a list"},
		// Kept as an unexpanded node in a foreign namespace: loads.
		{"alias bomb in foreign metadata", "name: x\ndescription: When x.\nmetadata:\n  " + laughs + "\nallowed-tools: *a9\n", "", ""},
		{"self-referencing alias", "name: x\ndescription: When x.\nmetadata:\n  holaclaw:\n    version: 1\n    requires: &r\n      tools: *r\n", CodeBadValue, "expected a list, got a mapping"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if len(c.front) > maxFile {
				t.Fatalf("front is %d bytes; Load refuses a SKILL.md over %d", len(c.front), maxFile)
			}
			start := time.Now()
			_, err := Parse(LevelAgent, "x", []byte("---\n"+c.front+"---\nbody\n"))
			if d := time.Since(start); d > 5*time.Second {
				t.Fatalf("took %v", d)
			}
			if got := codeOf(err); got != c.code {
				t.Fatalf("code = %q (%v), want %q", got, err, c.code)
			}
			if err != nil && !strings.Contains(err.Error(), c.msg) {
				t.Fatalf("message %q lacks %q", err, c.msg)
			}
		})
	}
}

// learned.on is the RFC 3339 time the daemon writes, or a bare date a person
// wrote, read as midnight; both come back in UTC. Anything else is refused.
func TestParseLearnedOn(t *testing.T) {
	for in, want := range map[string]time.Time{
		"2026-09-12T14:03:05Z":      time.Date(2026, 9, 12, 14, 3, 5, 0, time.UTC),
		"2026-09-12T16:03:05+02:00": time.Date(2026, 9, 12, 14, 3, 5, 0, time.UTC),
		"2026-09-12":                time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC),
	} {
		got, err := parseLearnedOn(in)
		if err != nil || !got.Equal(want) || got.Location() != time.UTC {
			t.Errorf("parseLearnedOn(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"yesterday", "2026-09-12 14:03", "12/09/2026"} {
		if _, err := parseLearnedOn(in); err == nil {
			t.Errorf("parseLearnedOn(%q) accepted", in)
		}
	}
}

// TestParseSpecKeys loads skills that use the other AgentSkills keys and a
// foreign metadata namespace: they parse, and only metadata.holaclaw is read.
func TestParseSpecKeys(t *testing.T) {
	cases := []struct {
		dir   string
		tools string
		bins  []Bin
	}{
		// Block metadata.openclaw beside metadata.holaclaw; allowed-tools names
		// tools the skill does not require.
		{"deploy", "exec", []Bin{{"git", "git"}}},
		// JSON-style inline metadata, allowed-tools as a spec string.
		{"discord-game", "", nil},
	}
	for _, c := range cases {
		t.Run(c.dir, func(t *testing.T) {
			dir := filepath.Join(fixtures, "spec", c.dir)
			b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
			if err != nil {
				t.Fatal(err)
			}
			sk, err := Parse(LevelAgent, dir, b)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(sk.Requires.Tools, ","); got != c.tools {
				t.Fatalf("tools = %q, want %q: allowed-tools must be ignored", got, c.tools)
			}
			if !slices.Equal(sk.Requires.Bins, c.bins) || len(sk.Requires.Net) != 0 || sk.Learned != nil {
				t.Fatalf("skill = %+v", sk)
			}
		})
	}
	set, probs := Load(Roots{Agent: filepath.Join(fixtures, "spec")})
	if len(probs) != 0 || keys(set.All()) != "agent:deploy agent:discord-game" {
		t.Fatalf("Load = %s, %v", keys(set.All()), probs)
	}
}

// TestParseForeignMetadata checks that any shape another tool may put under
// metadata parses, and that allowed-tools in any form is ignored.
func TestParseForeignMetadata(t *testing.T) {
	for _, extra := range []string{
		"metadata: {}\n",
		"metadata: {a: {b: [1, 2]}}\n",
		"metadata: {\"x.y\": [[1, [2]], {\"k\": \"v, w\"}], plain: a:b}\n",
		"metadata:\n  other:\n    list:\n      - [1, {a: b}]\n      - \"s\"\n    deep:\n      deeper:\n        k: v # comment\n",
		"allowed-tools: Read Write\n",
		"allowed-tools: []\n",
		"allowed-tools:\n  - files\n  - browser\n",
		"license: MIT\ncompatibility: \"Needs Python 3.12, uv\"\n",
	} {
		src := "---\nname: x\ndescription: When x.\n" + extra + "---\nbody\n"
		sk, err := Parse(LevelAgent, "x", []byte(src))
		if err != nil {
			t.Fatalf("%q: %v", extra, err)
		}
		if len(sk.Requires.Tools) != 0 {
			t.Fatalf("%q: tools = %v", extra, sk.Requires.Tools)
		}
	}
}
