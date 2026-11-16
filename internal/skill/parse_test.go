package skill

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const goodULID = "01J9Q4ABCDEFGHJKMNPQRSTVWX"

// goodBy is the identity that taught a learned skill.
const goodBy = "01J9PXABCDEFGHJKMNPQRSTVWX"

// codeOf returns the Problem code of err, or "" when err is not a Problem.
func codeOf(err error) Code {
	var p Problem
	if errors.As(err, &p) {
		return p.Code
	}
	return ""
}

func TestParseFull(t *testing.T) {
	src := `---
# a comment line
name: release
description: "Cutting a release: \"tests\", tag"   # trailing comment
metadata:
  holaclaw:
    version: 1
    requires:
      tools:
        - exec
        - "files"
      bins: [gh, {name: rg, pkg: ripgrep}, {name: jq}, {name: bpython, pkg: python3Packages.bpython}]
      net: [gh]
---
# Release

Body # not a comment, never preprocessed.
`
	sk, err := Parse(LevelAgent, "/x/skills/release", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "release" || sk.Description != `Cutting a release: "tests", tag` {
		t.Fatalf("name/description = %q / %q", sk.Name, sk.Description)
	}
	if got := strings.Join(sk.Requires.Tools, ","); got != "exec,files" {
		t.Fatalf("tools = %q", got)
	}
	want := []Bin{{"gh", "gh"}, {"rg", "ripgrep"}, {"jq", "jq"}, {"bpython", "python3Packages.bpython"}}
	if len(sk.Requires.Bins) != len(want) {
		t.Fatalf("bins = %+v", sk.Requires.Bins)
	}
	for i := range want {
		if sk.Requires.Bins[i] != want[i] {
			t.Fatalf("bins[%d] = %+v, want %+v", i, sk.Requires.Bins[i], want[i])
		}
	}
	if len(sk.Requires.Net) != 1 || sk.Requires.Net[0] != "gh" {
		t.Fatalf("net = %v", sk.Requires.Net)
	}
	if sk.Body != "# Release\n\nBody # not a comment, never preprocessed.\n" {
		t.Fatalf("body = %q", sk.Body)
	}
	if sk.Dir != "/x/skills/release" || sk.Level != LevelAgent || sk.Learned != nil {
		t.Fatalf("skill = %+v", sk)
	}
}

// FuzzParse checks that no input crashes Parse and that a skill it returns
// has a name.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"---\nname: x\ndescription: When x.\n---\nbody\n",
		"---\nname: x\ndescription: When x.\nmetadata: {\"a\": [1, {b: 'c'}]}\n---\n",
		"---\nname: x\ndescription: When x.\nmetadata:\n  holaclaw:\n    version: 1\n    requires:\n      tools: [exec]\n      bins: [gh, {name: rg, pkg: ripgrep}]\n      net: [gh]\n---\n",
		"---\nname: &a x\ndescription: *a\nmetadata: {k: [*a, *a]}\n---\n",
		"---\nname: x\ndescription: |\n  y\n\tz\n---\n",
		"---\n[[[{{{\n---\n",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		sk, err := Parse(LevelAgent, "x", []byte(s))
		if err == nil && sk.Name == "" {
			t.Fatalf("skill without a name from %q", s)
		}
	})
}

// A delimiter with trailing whitespace still opens or closes the frontmatter,
// even when the body has no later `---` line.
func TestParseDelimiterTrailingSpace(t *testing.T) {
	// · marks trailing whitespace on a `---` line; it is written as a
	// placeholder so that editors stripping trailing whitespace keep it.
	for _, src := range []string{
		"---\nname: x\ndescription: When x.\n---·\nbody\n",
		"---·\t\nname: x\ndescription: When x.\n---\nbody\n",
		"---·\r\nname: x\r\ndescription: When x.\r\n---\t·\r\nbody\n",
	} {
		sk, err := Parse(LevelAgent, "x", []byte(strings.ReplaceAll(src, "·", " ")))
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		if sk.Name != "x" || sk.Body != "body\n" {
			t.Fatalf("%q: skill = %+v", src, sk)
		}
	}
	// Anything else on the line is not a delimiter.
	_, err := Parse(LevelAgent, "x", []byte("---\nname: x\ndescription: When x.\n--- x\nbody\n"))
	if codeOf(err) != CodeNoFrontmatter {
		t.Fatalf("err = %v, want %s", err, CodeNoFrontmatter)
	}
}

func TestParseCRLF(t *testing.T) {
	src := "---\r\nname: x\r\ndescription: When x.\r\n---\r\nbody\r\n"
	sk, err := Parse(LevelAgent, "x", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "x" || sk.Body != "body\r\n" {
		t.Fatalf("skill = %+v", sk)
	}
}

func TestParseLearned(t *testing.T) {
	src := "---\nname: pr-style\ndescription: When opening a PR.\nmetadata:\n  holaclaw:\n    version: 1\n    learned:\n" +
		"      on: 2026-09-12\n      by: " + goodBy + "\n      from: " + goodULID + "\n      origin: explicit\n---\nbody\n"
	sk, err := Parse(LevelAgent, "/x/skills/pr-style", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	l := sk.Learned
	if l == nil || !l.On.Equal(time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)) || l.By != goodBy || l.From != goodULID || l.Origin != OriginExplicit {
		t.Fatalf("learned = %+v", l)
	}
	if sk.Level != LevelAgent || sk.Dir != "/x/skills/pr-style" {
		t.Fatalf("skill = %+v", sk)
	}
}

// TestParseProblems covers every code Parse can return.
func TestParseProblems(t *testing.T) {
	hb := "metadata:\n  holaclaw:\n    version: 1\n"
	req := hb + "    requires:\n"
	learned := hb + "    learned:\n      on: 2026-09-12\n      by: " + goodBy + "\n      from: " + goodULID + "\n      origin: correction\n"
	fm := func(s string) string { return "---\n" + s + "---\nbody\n" }
	base := "name: x\ndescription: When x.\n"
	cases := []struct {
		name  string
		level Level
		src   string
		code  Code
		msg   string // substring of the message
	}{
		{"no frontmatter", LevelAgent, "name: x\n", CodeNoFrontmatter, "---"},
		{"unterminated", LevelAgent, "---\nname: x\ndescription: y\n", CodeNoFrontmatter, "---"},
		{"tab indent", LevelAgent, fm(base + "metadata:\n\tholaclaw:\n"), CodeSyntax, "line 5: found character that cannot start any token"},
		{"over-indented sibling", LevelAgent, fm("name: x\n  description: y\n"), CodeSyntax, "line 3: mapping values are not allowed"},
		{"plain scalar with colon", LevelAgent, fm("name: x\ndescription: Cut a release: tests, tag.\n"), CodeSyntax, "mapping values are not allowed"},
		{"no space after colon", LevelAgent, fm("name:x\ndescription: y\n"), CodeSyntax, "mapping values are not allowed"},
		{"unterminated quote", LevelAgent, fm("name: \"x\ndescription: y\n"), CodeSyntax, "unexpected end of stream"},
		{"text after quote", LevelAgent, fm("name: \"x\" y\ndescription: y\n"), CodeSyntax, "did not find expected key"},
		{"bad escape", LevelAgent, fm("name: \"x\\q\"\ndescription: y\n"), CodeSyntax, "escape"},
		{"unterminated flow", LevelAgent, fm(base + req + "      tools: [a, b\n"), CodeSyntax, "did not find expected ',' or ']'"},
		{"text after flow", LevelAgent, fm(base + req + "      tools: [a] b\n"), CodeSyntax, "did not find expected key"},
		{"empty flow item", LevelAgent, fm(base + req + "      tools: [a,,b]\n"), CodeSyntax, "did not find expected node content"},
		{"unterminated nested flow", LevelAgent, fm(base + "metadata: {a: {b: [1, 2}}\n"), CodeSyntax, "did not find expected ',' or ']'"},
		{"key in list", LevelAgent, fm(base + req + "      bins:\n        - a\n        b: c\n"), CodeSyntax, "did not find expected '-' indicator"},
		{"duplicate key", LevelAgent, fm("name: x\nname: y\ndescription: z\n"), CodeSyntax, `line 3: mapping key "name" already defined at line 2`},
		{"duplicate bin key", LevelAgent, fm(base + req + "      bins: [{name: a, name: b}]\n"), CodeSyntax, `mapping key "name" already defined`},
		{"duplicate foreign key", LevelAgent, fm(base + "metadata: {\"a\": 1, a: 2}\n"), CodeSyntax, `mapping key "a" already defined`},
		{"unknown anchor", LevelAgent, fm("name: *a\ndescription: y\n"), CodeSyntax, "unknown anchor"},
		{"second document", LevelAgent, fm(base + "--- y\n"), CodeSyntax, "more than one YAML document"},

		{"no colon", LevelAgent, fm("name x\n"), CodeBadValue, "expected a mapping, got a scalar"},
		{"top level list", LevelAgent, fm("- a\n"), CodeBadValue, "expected a mapping, got a list"},

		{"unknown top key", LevelAgent, fm(base + "version: 2\n"), CodeUnknownKey, "field version"},
		{"holaclaw at top level", LevelAgent, fm(base + "holaclaw:\n  version: 1\n  requires:\n    tools: [a]\n"), CodeUnknownKey, "use metadata.holaclaw"},
		{"unknown holaclaw key", LevelAgent, fm(base + hb + "    enabled: true\n"), CodeUnknownKey, "field metadata.holaclaw.enabled"},
		{"unknown requires key", LevelAgent, fm(base + req + "      env: [A]\n"), CodeUnknownKey, "field metadata.holaclaw.requires.env"},
		{"unknown bin key", LevelAgent, fm(base + req + "      bins: [{name: a, path: /x}]\n"), CodeUnknownKey, "metadata.holaclaw.requires.bins.path"},
		{"bin without colon", LevelAgent, fm(base + req + "      bins: [{rg}]\n"), CodeUnknownKey, "metadata.holaclaw.requires.bins.rg"},
		{"key with space", LevelAgent, fm("na me: x\n"), CodeUnknownKey, "field na me"},
		{"unknown learned key", LevelAgent, fm(base + learned + "      space: dm/a\n"), CodeUnknownKey, "metadata.holaclaw.learned.space"},
		{"duplicate by", LevelAgent, fm(base + learned + "      by: " + goodBy + "\n"), CodeSyntax, `mapping key "by" already defined`},

		{"missing name", LevelAgent, fm("description: When x.\n"), CodeMissing, "field name"},
		{"missing description", LevelAgent, fm("name: x\n"), CodeMissing, "field description"},
		{"empty description", LevelAgent, fm("name: x\ndescription: \"  \"\n"), CodeMissing, "field description"},
		{"learned without from", LevelAgent, fm(base + strings.Replace(learned, "      from: "+goodULID+"\n", "", 1)), CodeMissing, "metadata.holaclaw.learned.from"},
		{"learned without by", LevelAgent, fm(base + strings.Replace(learned, "      by: "+goodBy+"\n", "", 1)), CodeMissing, "metadata.holaclaw.learned.by"},
		{"learned empty by", LevelAgent, fm(base + strings.Replace(learned, "by: "+goodBy, `by: ""`, 1)), CodeMissing, "metadata.holaclaw.learned.by"},
		{"learned empty block", LevelAgent, fm(base + hb + "    learned: {}\n"), CodeMissing, "metadata.holaclaw.learned.on"},

		{"tools scalar", LevelAgent, fm(base + req + "      tools: exec\n"), CodeBadValue, "expected a list"},
		{"metadata scalar", LevelAgent, fm(base + "metadata: yes\n"), CodeBadValue, "expected a mapping"},
		{"metadata list", LevelAgent, fm(base + "metadata: [a]\n"), CodeBadValue, "expected a mapping"},
		{"holaclaw scalar", LevelAgent, fm(base + "metadata:\n  holaclaw: yes\n"), CodeBadValue, "expected a mapping"},
		{"requires list", LevelAgent, fm(base + hb + "    requires: [a]\n"), CodeBadValue, "expected a mapping"},
		{"name list", LevelAgent, fm("name: [x]\ndescription: y\n"), CodeBadValue, "expected a scalar"},
		{"tools with map", LevelAgent, fm(base + req + "      tools: [{a: b}]\n"), CodeBadValue, "expected a scalar, got a mapping"},
		{"tools with nested list", LevelAgent, fm(base + req + "      tools: [[a]]\n"), CodeBadValue, "expected a scalar, got a list"},
		{"bins scalar", LevelAgent, fm(base + req + "      bins: gh\n"), CodeBadValue, "expected a list"},
		{"empty bin entry", LevelAgent, fm(base + req + "      bins:\n        -\n"), CodeBadValue, "bins: empty entry"},
		{"null tool entry", LevelAgent, fm(base + req + "      tools: [a, ~]\n"), CodeBadValue, "tools: empty entry"},
		{"empty tool name", LevelAgent, fm(base + req + "      tools: [\"\"]\n"), CodeBadValue, "empty tool name"},
		{"bin sequence", LevelAgent, fm(base + req + "      bins: [[rg]]\n"), CodeBadValue, "each entry is a name"},
		{"bin path", LevelAgent, fm(base + req + "      bins: [/usr/bin/gh]\n"), CodeBadValue, "not a binary"},
		{"bin with nested value", LevelAgent, fm(base + req + "      bins: [{name: [rg]}]\n"), CodeBadValue, "expected a scalar"},
		{"bin without name", LevelAgent, fm(base + req + "      bins: [{pkg: ripgrep}]\n"), CodeMissing, "bins.name"},
		{"net not a bin", LevelAgent, fm(base + req + "      bins: [gh]\n      net: [curl]\n"), CodeBadValue, "net"},
		{"bad date", LevelAgent, fm(base + strings.Replace(learned, "2026-09-12", "12/09/2026", 1)), CodeBadValue, "metadata.holaclaw.learned.on"},
		{"bad ulid", LevelAgent, fm(base + strings.Replace(learned, goodULID, "not-a-ulid", 1)), CodeBadValue, "metadata.holaclaw.learned.from"},
		{"bad by", LevelAgent, fm(base + strings.Replace(learned, goodBy, "angel", 1)), CodeBadValue, "metadata.holaclaw.learned.by"},
		{"by a list", LevelAgent, fm(base + strings.Replace(learned, goodBy, "["+goodBy+"]", 1)), CodeBadValue, "expected a scalar, got a list"},
		{"bad origin", LevelAgent, fm(base + strings.Replace(learned, "correction", "discovery", 1)), CodeBadValue, "metadata.holaclaw.learned.origin"},
		{"learned scalar", LevelAgent, fm(base + hb + "    learned: yes\n"), CodeBadValue, "expected a mapping"},

		{"version missing", LevelAgent, fm(base + "metadata:\n  holaclaw:\n    requires:\n      tools: [a]\n"), CodeMissing, "metadata.holaclaw.version: required"},
		{"version missing on bundled", LevelBundled, fm(base + "metadata:\n  holaclaw:\n    skill_version: 2\n"), CodeMissing, "metadata.holaclaw.version"},
		{"version unknown", LevelAgent, fm(base + "metadata:\n  holaclaw:\n    version: 2\n"), CodeBadValue, "line 6: format 2 is not one this holaclaw reads (1)"},
		{"version unknown with its own keys", LevelAgent, fm(base + "metadata:\n  holaclaw:\n    version: 2\n    labels: [a]\n"), CodeBadValue, "format 2"},
		{"version zero", LevelAgent, fm(base + "metadata:\n  holaclaw:\n    version: 0\n"), CodeBadValue, "format 0"},
		{"version a string", LevelAgent, fm(base + "metadata:\n  holaclaw:\n    version: one\n"), CodeBadValue, "metadata.holaclaw.version: line 6: expected an integer"},
		{"version a list", LevelAgent, fm(base + "metadata:\n  holaclaw:\n    version: [1]\n"), CodeBadValue, "expected an integer"},
		{"version a float", LevelAgent, fm(base + "metadata:\n  holaclaw:\n    version: 1.5\n"), CodeBadValue, "expected an integer"},
		{"skill_version zero", LevelAgent, fm(base + hb + "    skill_version: 0\n"), CodeBadValue, "metadata.holaclaw.skill_version: 0: a skill version starts at 1"},
		{"skill_version a string", LevelAgent, fm(base + hb + "    skill_version: three\n"), CodeBadValue, "metadata.holaclaw.skill_version: line 7: expected an integer"},
		{"skill_version a float", LevelAgent, fm(base + hb + "    skill_version: 2.5\n"), CodeBadValue, "skill_version: line 7: expected an integer"},
		{"skill_version a list", LevelAgent, fm(base + hb + "    skill_version: [2]\n"), CodeBadValue, "skill_version: line 7: expected an integer"},

		{"bad name", LevelAgent, fm("name: X_y\ndescription: y\n"), CodeBadName, "must match"},
		{"name mismatch", LevelAgent, fm("name: other\ndescription: y\n"), CodeBadName, "differs from the directory"},

		{"description too long", LevelAgent, fm("name: x\ndescription: " + strings.Repeat("a", 1025) + "\n"), CodeDescription, "1025 characters, over 1024"},
		{"description multi-line", LevelAgent, fm("name: x\ndescription: \"a\\nb\"\n"), CodeDescription, "one line"},
		{"description block scalar", LevelAgent, fm("name: x\ndescription: |\n  y\n"), CodeDescription, "one line"},

		{"body too long", LevelAgent, "---\n" + base + "---\n" + strings.Repeat("é", MaxBody+1), CodeTooLong, "body"},

		{"learned on bundled", LevelBundled, fm(base + learned), CodeLearnedNotAllow, "bundled skill"},
		{"learned on bundled beside requires", LevelBundled, fm(base + req + "      tools: [a]\n" + strings.TrimPrefix(learned, hb)), CodeLearnedNotAllow, "bundled skill"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(c.level, "x", []byte(c.src))
			if got := codeOf(err); got != c.code {
				t.Fatalf("code = %q (%v), want %s", got, err, c.code)
			}
			if !strings.Contains(err.Error(), c.msg) {
				t.Fatalf("message %q lacks %q", err, c.msg)
			}
			if p := err.(Problem); p.Dir == "" {
				t.Fatalf("problem without dir: %+v", p)
			}
		})
	}
}

func TestParseBodyAtCap(t *testing.T) {
	src := "---\nname: x\ndescription: When x.\n---\n" + strings.Repeat("é", MaxBody)
	if _, err := Parse(LevelAgent, "x", []byte(src)); err != nil {
		t.Fatalf("body at the cap: %v", err)
	}
}

func TestParseDescriptionAtCap(t *testing.T) {
	// The AgentSkills spec allows 1024; skills written for other runtimes
	// carry descriptions that long.
	if MaxDescription != 1024 {
		t.Fatalf("MaxDescription = %d, want 1024", MaxDescription)
	}
	src := "---\nname: x\ndescription: " + strings.Repeat("a", MaxDescription) + "\n---\n"
	if _, err := Parse(LevelAgent, "x", []byte(src)); err != nil {
		t.Fatalf("description at the cap: %v", err)
	}
	src = "---\nname: x\ndescription: " + strings.Repeat("a", MaxDescription+1) + "\n---\n"
	if _, err := Parse(LevelAgent, "x", []byte(src)); codeOf(err) != CodeDescription {
		t.Fatalf("description over the cap: %v", err)
	}
}

func TestParseProgrammerErrors(t *testing.T) {
	src := []byte("---\nname: x\ndescription: y\n---\n")
	for _, l := range []Level{"nope", "learned", ""} {
		if _, err := Parse(l, "x", src); err == nil || codeOf(err) != "" {
			t.Fatalf("level %q: %v", l, err)
		}
	}
}

func TestParseBundledDir(t *testing.T) {
	src := []byte("---\nname: x\ndescription: When x.\n---\nbody\n")
	sk, err := Parse(LevelBundled, "x", src)
	if err != nil {
		t.Fatal(err)
	}
	if sk.Dir != "" || sk.fsPath != "x" {
		t.Fatalf("dir = %q, fsPath = %q", sk.Dir, sk.fsPath)
	}
	_, err = Parse(LevelBundled, "x", []byte("nope"))
	if p := err.(Problem); p.Dir != "bundled/x" {
		t.Fatalf("problem dir = %q", p.Dir)
	}
}

// TestParseVersions: the skill version is read when present; a skill without
// metadata.holaclaw (written for another tool) needs no format version.
func TestParseVersions(t *testing.T) {
	src := "---\nname: x\ndescription: When x.\nmetadata:\n  holaclaw:\n    version: 1\n    skill_version: 7\n---\nbody\n"
	sk, err := Parse(LevelAgent, "x", []byte(src))
	if err != nil || sk.Version != 7 {
		t.Fatalf("Parse = %+v, %v", sk, err)
	}
	for name, src := range map[string]string{
		"no metadata":          "---\nname: x\ndescription: When x.\n---\nbody\n",
		"another namespace":    "---\nname: x\ndescription: When x.\nmetadata:\n  openclaw:\n    emoji: x\n---\nbody\n",
		"format only":          "---\nname: x\ndescription: When x.\nmetadata:\n  holaclaw:\n    version: 1\n---\nbody\n",
		"holaclaw without one": "---\nname: x\ndescription: When x.\nmetadata:\n  holaclaw:\n---\nbody\n",
	} {
		sk, err := Parse(LevelBundled, "x", []byte(src))
		if err != nil || sk.Version != 0 {
			t.Errorf("%s: Parse = %+v, %v", name, sk, err)
		}
	}
}

func TestDecodeYAMLUnknownFormat(t *testing.T) {
	if _, err := decodeYAML("name: x\n", 2, 2); err == nil || !strings.Contains(err.Error(), "format 2") {
		t.Fatalf("err = %v", err)
	}
}
