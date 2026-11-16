package skill

import (
	"errors"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/event"
)

func goodDraft() Draft {
	return Draft{
		Name:     "pr-style",
		Title:    "Pull request style",
		When:     "When opening a pull request",
		Steps:    []string{"Keep the title under 50 characters.", "Put the test plan last."},
		Pitfalls: []string{"Long titles get cut in the list view."},
	}
}

func goodLearned() Learned {
	return Learned{On: time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC), By: goodBy, From: goodULID, Origin: OriginCorrection}
}

func lintCodes(errs []LintError) string {
	var out []string
	for _, e := range errs {
		out = append(out, string(e.Code))
	}
	return strings.Join(out, ",")
}

func TestLintClean(t *testing.T) {
	if errs := Lint(goodDraft()); len(errs) != 0 {
		t.Fatalf("Lint = %v", errs)
	}
	// Wording is not judged: dates, tickets, URLs, narrative and claims pass.
	d := goodDraft()
	d.When = "This skill is for release day"
	d.Steps = []string{
		"I checked https://x.io on 2026-01-01 for #12.",
		"We tag it as in ABC-123; the browser does not work.",
		"Answer in the same thread as the question.",
	}
	if errs := Lint(d); len(errs) != 0 {
		t.Fatalf("Lint = %v", errs)
	}
}

func TestLintWhenAtCap(t *testing.T) {
	d := goodDraft()
	d.When = "When " + strings.Repeat("x", MaxDescription-5)
	if errs := Lint(d); len(errs) != 0 {
		t.Fatalf("When at the cap: %v", errs)
	}
}

func TestLintEachRule(t *testing.T) {
	steps := func(s ...string) []string { return append(s, "Put the test plan last.") }
	cases := []struct {
		name string
		edit func(*Draft)
		code Code
	}{
		{"bad name", func(d *Draft) { d.Name = "PR Style" }, CodeBadName},
		{"empty name", func(d *Draft) { d.Name = "" }, CodeBadName},
		{"title missing", func(d *Draft) { d.Title = "" }, CodeTitle},
		{"title blank", func(d *Draft) { d.Title = " \t" }, CodeTitle},
		{"title multi-line", func(d *Draft) { d.Title = "Pull\nrequests" }, CodeTitle},
		{"title long", func(d *Draft) { d.Title = strings.Repeat("é", MaxTitle+1) }, CodeTitle},
		{"when empty", func(d *Draft) { d.When = " " }, CodeDescription},
		{"when long", func(d *Draft) { d.When = "When " + strings.Repeat("x", MaxDescription-4) }, CodeDescription},
		{"when multi-line", func(d *Draft) { d.When = "When a\nor b" }, CodeDescription},
		{"one step", func(d *Draft) { d.Steps = d.Steps[:1] }, CodeStepCount},
		{"no steps", func(d *Draft) { d.Steps = nil }, CodeStepCount},
		{"too many steps", func(d *Draft) {
			d.Steps = nil
			for range MaxSteps + 1 {
				d.Steps = append(d.Steps, "Do the thing.")
			}
		}, CodeStepCount},
		{"seven pitfalls", func(d *Draft) {
			d.Pitfalls = nil
			for range 7 {
				d.Pitfalls = append(d.Pitfalls, "Watch out.")
			}
		}, CodePitfallCount},
		{"empty step", func(d *Draft) { d.Steps = steps("") }, CodeItemLength},
		{"long step", func(d *Draft) { d.Steps = steps(strings.Repeat("a", MaxItem+1)) }, CodeItemLength},
		{"multi-line step", func(d *Draft) { d.Steps = steps("a\nb") }, CodeItemLength},
		{"long pitfall", func(d *Draft) { d.Pitfalls = []string{strings.Repeat("a", MaxItem+1)} }, CodeItemLength},
		{"multi-line pitfall", func(d *Draft) { d.Pitfalls = []string{"a\rb"} }, CodeItemLength},
	}
	for _, c := range cases {
		d := goodDraft()
		c.edit(&d)
		errs := Lint(d)
		if lintCodes(errs) != string(c.code) {
			t.Errorf("%s: Lint = %v, want only %s", c.name, errs, c.code)
			continue
		}
		if errs[0].Error() != string(errs[0].Code)+": "+errs[0].Msg || errs[0].Msg == "" {
			t.Errorf("%s: Error() = %q", c.name, errs[0].Error())
		}
	}
	d := goodDraft()
	d.Title = strings.Repeat("é", MaxTitle)
	if errs := Lint(d); len(errs) != 0 {
		t.Fatalf("title at the limit: %v", errs)
	}
}

// TestLintRenderedTooLong: within the item limits a draft stays under the
// file cap, so the cap only fires together with over-long items.
func TestLintRenderedTooLong(t *testing.T) {
	d := goodDraft()
	d.Steps = nil
	for range MaxSteps {
		d.Steps = append(d.Steps, strings.Repeat("a", 700))
	}
	codes := lintCodes(Lint(d))
	if !strings.HasSuffix(codes, ","+string(CodeTooLong)) || !strings.Contains(codes, string(CodeItemLength)) {
		t.Fatalf("codes = %s", codes)
	}
}

func TestLintReportsEveryViolation(t *testing.T) {
	d := Draft{
		Name:     "Bad Name",
		When:     "",
		Steps:    []string{"a\nb"},
		Pitfalls: []string{""},
	}
	want := "SKL106,SKL114,SKL107,SKL111,SKL112,SKL112"
	if got := lintCodes(Lint(d)); got != want {
		t.Fatalf("codes = %s, want %s", got, want)
	}
	if msg := Lint(Draft{Name: "x", Title: "X", When: "When x", Steps: []string{"a"}})[0].Msg; !strings.Contains(msg, "memory.save") {
		t.Fatalf("one-step message does not point at memory: %q", msg)
	}
}

func TestRenderExact(t *testing.T) {
	d := goodDraft()
	d.Title = "  Keep pull requests short: \"PR\" # style  "
	d.When = `When opening a "pull request"`
	b, err := Render(d, goodLearned(), Requires{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := `---
name: pr-style
description: When opening a "pull request"
metadata:
  holaclaw:
    version: 1
    skill_version: 1
    learned:
      "on": "2026-09-12T00:00:00Z"
      by: ` + goodBy + `
      from: ` + goodULID + `
      origin: correction
---
# Keep pull requests short: "PR" # style

How this is done here.

## When

When opening a "pull request"

## Steps

1. Keep the title under 50 characters.
2. Put the test plan last.

## Pitfalls

- Long titles get cut in the list view.

## Verify

Confirm the outcome with the person before ending the turn.
`
	if string(b) != want {
		t.Fatalf("Render =\n%s\nwant\n%s", b, want)
	}
	for range 3 {
		if again, _ := Render(d, goodLearned(), Requires{}, 1); string(again) != want {
			t.Fatal("Render is not deterministic")
		}
	}
	d.Pitfalls = nil
	b, _ = Render(d, goodLearned(), Requires{}, 1)
	if strings.Contains(string(b), "## Pitfalls") {
		t.Fatal("empty pitfalls rendered a section")
	}
}

// TestRenderQuoting checks that the library quotes a description a plain
// scalar would misread, and leaves a harmless one plain.
func TestRenderQuoting(t *testing.T) {
	cases := []struct{ desc, line string }{
		{"When x: y", `description: 'When x: y'`},
		{"When # x", `description: 'When # x'`},
		{"When x #y", `description: 'When x #y'`},
		{" When x", `description: ' When x'`},
		{"When x ", `description: 'When x '`},
		{"yes", `description: "yes"`},
		{"- When x", `description: '- When x'`},
		{"When x# y", `description: When x# y`},
	}
	for _, c := range cases {
		var f renderedFront
		f.Name, f.Description = "x", c.desc
		b, err := encodeFront(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "\n"+c.line+"\n") {
			t.Errorf("%q encodes as\n%s\nwant line %s", c.desc, b, c.line)
		}
	}
}

func TestRenderRoundTrip(t *testing.T) {
	drafts := []Draft{goodDraft(), {
		Name:  "deploy",
		Title: "Deploy",
		When:  `When deploying: a \ back-slash, "quotes" # hash`,
		Steps: []string{"Run the tests.", "Tag the build: v-next.", "Deploy with the exec tool."},
	}, {
		// Characters a YAML emitter must escape.
		Name:  "odd",
		Title: "Odd",
		When:  "When a\x07b\x7f\u0085c\u2028d\u2029e\ufefff\tg: 'h' é 🎮",
		Steps: []string{"Run the tests.", "Deploy."},
	}, {
		// Not UTF-8: the library writes it as !!binary, which reads back.
		Name:  "bytes",
		Title: "Bytes",
		When:  "When a\xffb",
		Steps: []string{"Run the tests.", "Deploy."},
	}, {
		Name:  "spaces",
		Title: "Spaces",
		When:  "  When padded: yes  ",
		Steps: []string{"Run the tests.", "Deploy."},
	}}
	for _, d := range drafts {
		l := goodLearned()
		l.Origin = OriginExplicit
		b, err := Render(d, l, Requires{}, 1)
		if err != nil {
			t.Fatal(err)
		}
		sk, err := Parse(LevelAgent, "/x/"+d.Name, b)
		if err != nil {
			t.Fatalf("Parse(Render) = %v\n%s", err, b)
		}
		if sk.Name != d.Name || sk.Description != strings.TrimSpace(d.When) || *sk.Learned != l {
			t.Fatalf("round trip = %+v %+v", sk, sk.Learned)
		}
		if !strings.HasPrefix(sk.Body, "# "+d.Title+"\n") || !strings.Contains(sk.Body, "## Steps\n\n1. "+d.Steps[0]) {
			t.Fatalf("body = %q", sk.Body)
		}
	}
	// A time of day and a zone keep the instant, written in UTC to the second.
	l := goodLearned()
	l.On = time.Date(2026, 9, 12, 23, 30, 5, 999, time.FixedZone("x", -5*3600))
	b, err := Render(goodDraft(), l, Requires{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"on": "2026-09-13T04:30:05Z"`) {
		t.Fatalf("learned.on not in UTC:\n%s", b)
	}
	sk, err := Parse(LevelAgent, "pr-style", b)
	if err != nil || !sk.Learned.On.Equal(time.Date(2026, 9, 13, 4, 30, 5, 0, time.UTC)) {
		t.Fatalf("read back %v, %v", sk.Learned, err)
	}
}

func TestRenderRefuses(t *testing.T) {
	bad := goodDraft()
	bad.Steps = bad.Steps[:1]
	if _, err := Render(bad, goodLearned(), Requires{}, 1); err == nil || !strings.Contains(err.Error(), string(CodeStepCount)) {
		t.Fatalf("Render of a bad draft: %v", err)
	}
	for _, v := range []int{0, -1} {
		if _, err := Render(goodDraft(), goodLearned(), Requires{}, v); err == nil || !strings.Contains(err.Error(), "at least 1") {
			t.Errorf("version %d: Render = %v", v, err)
		}
	}
	for name, l := range map[string]Learned{
		"no date":    {By: goodBy, From: goodULID, Origin: OriginCorrection},
		"bad id":     {On: time.Now(), By: goodBy, From: event.ID("x"), Origin: OriginCorrection},
		"no by":      {On: time.Now(), From: goodULID, Origin: OriginCorrection},
		"bad by":     {On: time.Now(), By: event.ID("angel"), From: goodULID, Origin: OriginCorrection},
		"bad origin": {On: time.Now(), By: goodBy, From: goodULID, Origin: "discovery"},
	} {
		if _, err := Render(goodDraft(), l, Requires{}, 1); err == nil {
			t.Errorf("%s: Render accepted %+v", name, l)
		}
	}
}

// TestRenderSelfCheck breaks the encoder to reach the checks the library
// never fails: an encode error, output that does not parse, and output that
// parses to something else.
func TestRenderSelfCheck(t *testing.T) {
	orig := encodeFront
	t.Cleanup(func() { encodeFront = orig })
	cases := []struct {
		name string
		enc  func(any) ([]byte, error)
		msg  string
	}{
		{"encode error", func(any) ([]byte, error) { return nil, errors.New("boom") }, "encode frontmatter: boom"},
		{"does not parse", func(any) ([]byte, error) { return []byte("name: [x\n"), nil }, "does not parse: " + string(CodeSyntax)},
		{"reads back different", func(v any) ([]byte, error) {
			f := v.(renderedFront)
			f.Description = "When something else"
			return orig(f)
		}, "reads back as"},
		{"learned block altered", func(v any) ([]byte, error) {
			f := v.(renderedFront)
			f.Metadata.HolaClaw.Learned.Origin = string(OriginExplicit)
			return orig(f)
		}, "reads back as"},
		{"teacher altered", func(v any) ([]byte, error) {
			f := v.(renderedFront)
			f.Metadata.HolaClaw.Learned.By = goodULID
			return orig(f)
		}, "reads back as"},
		{"requires widened", func(v any) ([]byte, error) {
			f := v.(renderedFront)
			if r := f.Metadata.HolaClaw.Requires; r != nil { // nil in Lint's size check
				f.Metadata.HolaClaw.Requires = &renderedRequires{Tools: append(r.Tools, "exec")}
			}
			return orig(f)
		}, "reads back as"},
		{"requires dropped", func(v any) ([]byte, error) {
			f := v.(renderedFront)
			f.Metadata.HolaClaw.Requires = nil
			return orig(f)
		}, "reads back as"},
	}
	for _, c := range cases {
		encodeFront = c.enc
		if _, err := Render(goodDraft(), goodLearned(), Requires{Tools: []string{"files"}}, 1); err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%s: Render = %v, want %q", c.name, err, c.msg)
		}
	}
}

// TestRenderRequires checks that the requires block of the replaced version
// is carried over as given, and that none is written when there is none.
func TestRenderRequires(t *testing.T) {
	r := Requires{
		Tools: []string{"exec", "files"},
		Bins:  []Bin{{"gh", "gh"}, {"rg", "ripgrep"}, {Name: "jq"}, {"bpython", "python3Packages.bpython"}},
		Net:   []string{"gh"},
	}
	b, err := Render(goodDraft(), goodLearned(), r, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := `metadata:
  holaclaw:
    version: 1
    skill_version: 1
    requires:
      tools:
        - exec
        - files
      bins:
        - gh
        - name: rg
          pkg: ripgrep
        - jq
        - name: bpython
          pkg: python3Packages.bpython
      net:
        - gh
    learned:
`
	if !strings.Contains(string(b), want) {
		t.Fatalf("Render =\n%s\nwant a block\n%s", b, want)
	}
	sk, err := Parse(LevelAgent, "pr-style", b)
	if err != nil {
		t.Fatal(err)
	}
	r.Bins[2].Pkg = "jq"
	if !sameRequires(sk.Requires, r) || sk.Learned == nil || *sk.Learned != goodLearned() {
		t.Fatalf("round trip = %+v, %+v", sk.Requires, sk.Learned)
	}

	// Only tools: the other keys are left out.
	b, err = Render(goodDraft(), goodLearned(), Requires{Tools: []string{"files"}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "    requires:\n      tools:\n        - files\n    learned:\n") {
		t.Fatalf("tools only:\n%s", b)
	}

	// The zero block and empty lists render no requires key.
	for _, r := range []Requires{{}, {Tools: []string{}, Bins: []Bin{}, Net: []string{}}} {
		b, err := Render(goodDraft(), goodLearned(), r, 1)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "requires") {
			t.Fatalf("empty requires rendered:\n%s", b)
		}
	}

	// A block Parse would refuse is refused, never written.
	for name, r := range map[string]Requires{
		"net not a bin": {Bins: []Bin{{"gh", "gh"}}, Net: []string{"curl"}},
		"bin path":      {Bins: []Bin{{"/usr/bin/gh", "gh"}}},
		"empty tool":    {Tools: []string{""}},
	} {
		if _, err := Render(goodDraft(), goodLearned(), r, 1); err == nil || !strings.Contains(err.Error(), "does not parse") {
			t.Errorf("%s: Render = %v", name, err)
		}
	}
}

func TestSameRequires(t *testing.T) {
	a := Requires{Tools: []string{"exec"}, Bins: []Bin{{"rg", "rg"}}, Net: []string{"rg"}}
	cases := []struct {
		b    Requires
		same bool
	}{
		{Requires{Tools: []string{"exec"}, Bins: []Bin{{Name: "rg"}}, Net: []string{"rg"}}, true},
		{Requires{Tools: []string{"exec"}, Bins: []Bin{{"rg", "ripgrep"}}, Net: []string{"rg"}}, false},
		{Requires{Tools: []string{"exec"}, Bins: []Bin{{"rg", "rg"}}}, false},
		{Requires{Bins: []Bin{{"rg", "rg"}}, Net: []string{"rg"}}, false},
		{Requires{Tools: []string{"exec"}, Net: []string{"rg"}}, false},
	}
	for i, c := range cases {
		if got := sameRequires(a, c.b); got != c.same {
			t.Errorf("case %d: sameRequires = %v", i, got)
		}
	}
	if !sameRequires(Requires{}, Requires{Tools: []string{}}) {
		t.Error("nil and empty lists differ")
	}
}

// TestRenderVersion: the skill version given is written and read back, next
// to the format version this holaclaw writes.
func TestRenderVersion(t *testing.T) {
	b, err := Render(goodDraft(), goodLearned(), Requires{}, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "  holaclaw:\n    version: 1\n    skill_version: 42\n") {
		t.Fatalf("Render =\n%s", b)
	}
	sk, err := Parse(LevelAgent, goodDraft().Name, b)
	if err != nil || sk.Version != 42 {
		t.Fatalf("Parse = %+v, %v", sk, err)
	}
}
