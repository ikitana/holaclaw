package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A Problem matches an Err value by code alone, through wrapping, and never
// a Problem of another code or a plain error.
func TestProblemIs(t *testing.T) {
	p := Problem{Code: CodeNotFound, Dir: "/skills/x", Msg: "no SKILL.md to forget"}
	for _, err := range []error{p, fmt.Errorf("skill: forget: %w", p)} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("errors.Is(%v, ErrNotFound) = false", err)
		}
		if errors.Is(err, ErrUnsafePath) || errors.Is(err, errors.New(string(CodeNotFound))) {
			t.Errorf("%v matches another error", err)
		}
	}
	var got Problem
	if !errors.As(fmt.Errorf("wrap: %w", p), &got) || got.Code != CodeNotFound {
		t.Fatalf("errors.As = %+v", got)
	}
	for _, c := range []struct {
		p    Problem
		want string
	}{
		{p, "SKL201: /skills/x: no SKILL.md to forget"},
		{Problem{Code: CodeBadName, Msg: `"X" is not a skill name`}, `SKL106: "X" is not a skill name`},
		{ErrNotFound, "SKL201"},
	} {
		if s := c.p.Error(); s != c.want {
			t.Errorf("Error() = %q, want %q", s, c.want)
		}
	}
}

func TestEligible(t *testing.T) {
	base := Env{Tools: []string{"exec", "files", "memory"}}
	cases := []struct {
		name  string
		sk    Skill
		env   Env
		ok    bool
		why   Reason
		words string
	}{
		{"plain", Skill{Name: "s", Level: LevelAgent}, base, true, Reason{}, ""},
		{"tools present", Skill{Name: "s", Level: LevelAgent, Requires: Requires{Tools: []string{"files"}}}, base, true, Reason{}, ""},
		{"missing tool", Skill{Name: "s", Level: LevelAgent, Requires: Requires{Tools: []string{"files", "browser"}}}, base,
			false, Reason{ReasonMissingTool, "browser"}, "requires tool browser, not in the agent's tools"},
		{"denied tool", Skill{Name: "s", Level: LevelAgent, Requires: Requires{Tools: []string{"exec"}}},
			Env{Tools: base.Tools, Denied: []string{"exec"}}, false, Reason{ReasonDeniedTool, "exec"}, "tool exec is denied for this role"},
		{"missing before denied", Skill{Name: "s", Level: LevelAgent, Requires: Requires{Tools: []string{"exec", "browser"}}},
			Env{Tools: base.Tools, Denied: []string{"exec"}}, false, Reason{ReasonMissingTool, "browser"}, "requires tool browser, not in the agent's tools"},
		{"bins", Skill{Name: "s", Level: LevelBundled, Requires: Requires{Tools: []string{"exec"}, Bins: []Bin{{"gh", "gh"}}}}, base,
			false, Reason{ReasonNeedsBinary, "gh"}, "requires binary gh (binaries are not supported yet)"},
		{"disabled", Skill{Name: "s", Level: LevelAgent}, Env{Disabled: []string{"s"}}, false, Reason{Kind: ReasonDisabled}, "disabled"},
		{"saved by the agent", Skill{Name: "s", Level: LevelAgent, Learned: &Learned{By: goodBy}}, base, true, Reason{}, ""},
		{"saved, disabled", Skill{Name: "s", Level: LevelAgent, Learned: &Learned{By: goodBy}}, Env{Disabled: []string{"s"}}, false, Reason{Kind: ReasonDisabled}, "disabled"},
	}
	for _, c := range cases {
		ok, why := Eligible(c.sk, c.env)
		if ok != c.ok || why != c.why || why.String() != c.words {
			t.Errorf("%s: Eligible = %v, %+v (%q); want %v, %+v (%q)", c.name, ok, why, why, c.ok, c.why, c.words)
		}
	}
}

func TestIndex(t *testing.T) {
	got := Index([]Skill{
		{Name: "release", Description: "Cutting a release: tests, tag.", Level: LevelAgent},
		{Name: "pr-style", Description: "When opening a PR.", Level: LevelAgent, Learned: &Learned{By: goodBy}},
	})
	want := "- release: Cutting a release: tests, tag.\n- pr-style: When opening a PR.\n"
	if got != want {
		t.Fatalf("Index = %q, want %q", got, want)
	}
	if Index(nil) != "" {
		t.Fatal("Index(nil) not empty")
	}
}

// A description over MaxIndexed is cut on a rune boundary; one at the cap is
// rendered whole.
func TestIndexCapsDescription(t *testing.T) {
	at := strings.Repeat("é", MaxIndexed)
	if got, want := Index([]Skill{{Name: "x", Description: at}}), "- x: "+at+"\n"; got != want {
		t.Fatalf("Index at the cap = %q, want %q", got, want)
	}
	long := at + strings.Repeat("a", MaxDescription-MaxIndexed)
	if got, want := Index([]Skill{{Name: "x", Description: long}}), "- x: "+at+"…\n"; got != want {
		t.Fatalf("Index over the cap = %q, want %q", got, want)
	}
}

func TestProvenance(t *testing.T) {
	on := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	learned := &Learned{On: on, By: goodBy}
	cases := []struct {
		sk      Skill
		teacher string
		want    string
	}{
		{Skill{Name: "release", Level: LevelAgent}, "", "[skill: release · agent skill]"},
		{Skill{Name: "release", Level: LevelAgent}, "Angel", "[skill: release · agent skill]"},
		{Skill{Name: "x", Level: LevelBundled}, "Angel", "[skill: x · bundled skill]"},
		{Skill{Name: "pr-style", Level: LevelAgent, Learned: learned}, "Angel", "[skill: pr-style · learned from Angel on 2026-09-12]"},
		{Skill{Name: "pr-style", Level: LevelAgent, Learned: learned}, "", "[skill: pr-style · learned on 2026-09-12]"},
	}
	for _, c := range cases {
		if got := Provenance(c.sk, c.teacher); got != c.want {
			t.Errorf("Provenance = %q, want %q", got, c.want)
		}
	}
}

func TestReferenceFixture(t *testing.T) {
	set, _ := loadFixtures(t)
	sk, _ := set.Get("pr-style")
	b, err := sk.Reference("checklist.md")
	if err != nil || !strings.HasPrefix(string(b), "# Checklist") {
		t.Fatalf("Reference = %q, %v", b, err)
	}
	bundled, _ := set.Get("writing-skills")
	if b, err = bundled.Reference("guide.md"); err != nil || string(b) != "guide" {
		t.Fatalf("bundled Reference = %q, %v", b, err)
	}
	if _, err = bundled.Reference("../summarize/SKILL.md"); !errors.Is(err, ErrBadReference) {
		t.Fatalf("bundled escape: %v", err)
	}
	if _, err = bundled.Reference("missing.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("bundled missing: %v", err)
	}
	if _, err = (Skill{Name: "x", Level: LevelBundled}).Reference("a.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("bundled without root: %v", err)
	}
}

func TestReferenceEscapes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "skill")
	write(t, filepath.Join(dir, "references", "ok.md"), "ok")
	write(t, filepath.Join(dir, "references", "sub", "deep.md"), "deep")
	write(t, filepath.Join(dir, "SKILL.md"), "not a reference")
	write(t, filepath.Join(root, "secret.txt"), "secret")
	write(t, filepath.Join(dir, "references", "big.md"), strings.Repeat("x", MaxReference+1))
	write(t, filepath.Join(dir, "references", "at-cap.md"), strings.Repeat("x", MaxReference))
	if err := os.Symlink(filepath.Join(root, "secret.txt"), filepath.Join(dir, "references", "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("ok.md", filepath.Join(dir, "references", "inside.md")); err != nil {
		t.Fatal(err)
	}
	sk := Skill{Name: "skill", Level: LevelAgent, Dir: dir}

	for name, want := range map[string]string{"ok.md": "ok", "sub/deep.md": "deep", "inside.md": "ok"} {
		if b, err := sk.Reference(name); err != nil || string(b) != want {
			t.Errorf("Reference(%q) = %q, %v", name, b, err)
		}
	}
	if b, err := sk.Reference("at-cap.md"); err != nil || len(b) != MaxReference {
		t.Errorf("at cap: %d, %v", len(b), err)
	}
	for _, name := range []string{"../SKILL.md", "../../secret.txt", "/etc/passwd", "", ".", "sub/../ok.md", `sub\deep.md`, "sub/"} {
		if _, err := sk.Reference(name); !errors.Is(err, ErrBadReference) {
			t.Errorf("Reference(%q) = %v, want ErrBadReference", name, err)
		}
	}
	if _, err := sk.Reference("link.md"); err == nil {
		t.Error("symlink escaping references/ was followed")
	}
	if _, err := sk.Reference("missing.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	if _, err := sk.Reference("big.md"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("over cap: %v", err)
	}
	if _, err := sk.Reference("sub"); !errors.Is(err, ErrBadReference) {
		t.Errorf("directory: %v", err)
	}
	noRefs := Skill{Name: "n", Level: LevelAgent, Dir: filepath.Join(root, "none")}
	if _, err := noRefs.Reference("a.md"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("no references dir: %v", err)
	}
}

func TestProblemError(t *testing.T) {
	p := Problem{Code: CodeBadName, Dir: "/a/b", Msg: "field name: bad"}
	if p.Error() != "SKL106: /a/b: field name: bad" {
		t.Fatalf("Error = %q", p.Error())
	}
}
