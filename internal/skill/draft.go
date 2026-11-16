package skill

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// Draft is the structured form the model produces through skill.save; the
// model never writes markdown (docs/skills.md §5). Title is written verbatim
// as the "# " heading; When becomes the description and the ## When section.
// Every field is written as given, only surrounding whitespace trimmed.
type Draft struct {
	Name, Title, When string
	Steps, Pitfalls   []string
}

// Draft limits (docs/skills.md §5).
const (
	MinSteps    = 2
	MaxSteps    = 24
	MaxPitfalls = 6
	MaxTitle    = 128 // characters in the title
	MaxItem     = 300 // characters in one step or pitfall
)

// verifyLine is the fixed ## Verify section of a rendered skill.
const verifyLine = "Confirm the outcome with the person before ending the turn."

// intentLine is the fixed line under the title of a rendered skill.
const intentLine = "How this is done here."

// LintError is one violation in a draft, phrased so the model can fix it in
// its next call.
type LintError struct {
	Code Code
	Msg  string
}

func (e LintError) Error() string { return string(e.Code) + ": " + e.Msg }

// Lint checks a draft and returns every violation, not just the first. It
// checks only shape and length; the wording is not judged here.
func Lint(d Draft) []LintError {
	var errs []LintError
	add := func(code Code, format string, a ...any) {
		errs = append(errs, LintError{Code: code, Msg: fmt.Sprintf(format, a...)})
	}
	if !ValidName(d.Name) {
		add(CodeBadName, "name %q must be 1-64 lowercase letters, digits or dashes", d.Name)
	}
	if msg := lineProblem(d.Title, MaxTitle); msg != "" {
		add(CodeTitle, "title: %s", msg)
	}
	if msg := yamlDescriptionIssue(d.When); msg != "" {
		add(CodeDescription, "when: %s", msg)
	}
	switch n := len(d.Steps); {
	case n < MinSteps:
		add(CodeStepCount, "%d step(s): a skill needs at least %d; a single preference or fact belongs in memory.save", n, MinSteps)
	case n > MaxSteps:
		add(CodeStepCount, "%d steps, over %d; merge steps or split the skill", n, MaxSteps)
	}
	if n := len(d.Pitfalls); n > MaxPitfalls {
		add(CodePitfallCount, "%d pitfalls, over %d; keep the ones that matter", n, MaxPitfalls)
	}
	for i, s := range d.Steps {
		if msg := lineProblem(s, MaxItem); msg != "" {
			add(CodeItemLength, "step %d: %s", i+1, msg)
		}
	}
	for i, s := range d.Pitfalls {
		if msg := lineProblem(s, MaxItem); msg != "" {
			add(CodeItemLength, "pitfall %d: %s", i+1, msg)
		}
	}
	// placeholderID is not an id of anything: it only stands in for the by and
	// from ULIDs, which have a fixed length, so the rendered length below is
	// the saved file's up to the digits of the skill version. The rendered
	// bytes are discarded.
	const placeholderID = "01J9Q4ZZZZZZZZZZZZZZZZZZZZ"
	l := Learned{On: time.Unix(0, 0).UTC(), By: placeholderID, From: placeholderID, Origin: OriginCorrection}
	b, err := render(d, l, Requires{}, 1)
	if n := utf8.RuneCount(b); err == nil && n > MaxBody {
		add(CodeTooLong, "the skill renders to %d characters, over %d; shorten the steps", n, MaxBody)
	}
	return errs
}

// lineProblem returns why s is not one non-empty line of at most limit
// characters, or "".
func lineProblem(s string, limit int) string {
	switch n := utf8.RuneCountInString(s); {
	case strings.TrimSpace(s) == "":
		return "empty"
	case strings.ContainsAny(s, "\n\r"):
		return "must be one line"
	case n > limit:
		return fmt.Sprintf("%d characters, over %d", n, limit)
	}
	return ""
}

// Render produces the SKILL.md of an agent skill the agent saved, from a
// draft. r is the requires block of the version it replaces, rendered under
// metadata.holaclaw.requires as given; a new skill passes the zero Requires,
// which renders no requires key. Saving never widens the list: the caller
// passes what it read, and nothing is added here (docs/skills.md §8). Render
// refuses a draft that Lint rejects or a Learned block that Parse would
// reject, then parses its own output and refuses it unless the name,
// description, requires, learned block and version read back as given.
//
// version is metadata.holaclaw.skill_version, at least 1: the number the
// file has as the current version (docs/skills.md §6).
// metadata.holaclaw.version is always MetadataVersion.
func Render(d Draft, l Learned, r Requires, version int) ([]byte, error) {
	if errs := Lint(d); len(errs) > 0 {
		all := make([]error, len(errs))
		for i, e := range errs {
			all[i] = e
		}
		return nil, errors.Join(all...)
	}
	switch {
	case l.On.IsZero():
		return nil, errors.New("skill: learned date is required")
	case !l.By.Valid():
		return nil, fmt.Errorf("skill: learned identity id %q is not a ULID", l.By)
	case !l.From.Valid():
		return nil, fmt.Errorf("skill: learned trace id %q is not a ULID", l.From)
	case l.Origin != OriginCorrection && l.Origin != OriginExplicit:
		return nil, fmt.Errorf("skill: learned origin %q must be correction or explicit", l.Origin)
	case version < 1:
		return nil, fmt.Errorf("skill: skill version %d must be at least 1", version)
	}
	b, err := render(d, l, r, version)
	if err != nil {
		return nil, err
	}
	sk, err := Parse(LevelAgent, d.Name, b)
	if err != nil {
		return nil, fmt.Errorf("skill: rendered file does not parse: %w", err)
	}
	// The file keeps the time to the second, in UTC.
	want := Learned{By: l.By, From: l.From, Origin: l.Origin}
	var got Learned
	if sk.Learned != nil {
		got = *sk.Learned
	}
	onOK := got.On.Equal(l.On.UTC().Truncate(time.Second))
	got.On = time.Time{}
	if sk.Name != d.Name || sk.Description != strings.TrimSpace(d.When) || sk.Learned == nil || !onOK || got != want ||
		!sameRequires(sk.Requires, r) || sk.Version != version {
		return nil, fmt.Errorf("skill: rendered file reads back as %q / %q / %+v / %+v / version %d", sk.Name, sk.Description, sk.Learned, sk.Requires, sk.Version)
	}
	return b, nil
}

// sameRequires reports whether a parsed block equals the one given, an empty
// Pkg in b standing for the binary's name as Parse reads it.
func sameRequires(a, b Requires) bool {
	if !slices.Equal(a.Tools, b.Tools) || !slices.Equal(a.Net, b.Net) || len(a.Bins) != len(b.Bins) {
		return false
	}
	for i, bin := range b.Bins {
		if bin.Pkg == "" {
			bin.Pkg = bin.Name
		}
		if a.Bins[i] != bin {
			return false
		}
	}
	return true
}

// renderedFront is the frontmatter of a skill the agent saved. Field order is
// key order, so the output is byte-stable; the library decides the quoting.
type renderedFront struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Metadata    struct {
		HolaClaw struct {
			Version      int               `yaml:"version"`
			SkillVersion int               `yaml:"skill_version"`
			Requires     *renderedRequires `yaml:"requires,omitempty"`
			Learned      learnedYAML       `yaml:"learned"`
		} `yaml:"holaclaw"`
	} `yaml:"metadata"`
}

// renderedRequires is metadata.holaclaw.requires as Parse reads it back. A
// bin is a bare name, or {name, pkg} when the package is named differently.
type renderedRequires struct {
	Tools []string `yaml:"tools,omitempty"`
	Bins  []any    `yaml:"bins,omitempty"`
	Net   []string `yaml:"net,omitempty"`
}

type renderedBin struct {
	Name string `yaml:"name"`
	Pkg  string `yaml:"pkg"`
}

// requiresFront returns the block to render, or nil for an empty one.
func requiresFront(r Requires) *renderedRequires {
	if len(r.Tools) == 0 && len(r.Bins) == 0 && len(r.Net) == 0 {
		return nil
	}
	out := &renderedRequires{Tools: r.Tools, Net: r.Net}
	for _, b := range r.Bins {
		if b.Pkg == "" || b.Pkg == b.Name {
			out.Bins = append(out.Bins, b.Name)
		} else {
			out.Bins = append(out.Bins, renderedBin(b))
		}
	}
	return out
}

// encodeFront marshals the frontmatter; a variable so a test can break it.
var encodeFront = func(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// render writes the file without checks.
func render(d Draft, l Learned, r Requires, version int) ([]byte, error) {
	var f renderedFront
	f.Metadata.HolaClaw.Version, f.Metadata.HolaClaw.SkillVersion = MetadataVersion, version
	f.Name, f.Description = d.Name, strings.TrimSpace(d.When)
	f.Metadata.HolaClaw.Requires = requiresFront(r)
	f.Metadata.HolaClaw.Learned = learnedYAML{On: l.On.UTC().Format(time.RFC3339), By: string(l.By), From: string(l.From), Origin: string(l.Origin)}
	front, err := encodeFront(f)
	if err != nil {
		return nil, fmt.Errorf("skill: encode frontmatter: %w", err)
	}
	var b bytes.Buffer
	b.WriteString("---\n")
	b.Write(front)
	b.WriteString("---\n")
	b.WriteString("# " + strings.TrimSpace(d.Title) + "\n\n")
	b.WriteString(intentLine + "\n\n")
	b.WriteString("## When\n\n" + strings.TrimSpace(d.When) + "\n\n")
	b.WriteString("## Steps\n\n")
	for i, s := range d.Steps {
		b.WriteString(strconv.Itoa(i+1) + ". " + strings.TrimSpace(s) + "\n")
	}
	if len(d.Pitfalls) > 0 {
		b.WriteString("\n## Pitfalls\n\n")
		for _, s := range d.Pitfalls {
			b.WriteString("- " + strings.TrimSpace(s) + "\n")
		}
	}
	b.WriteString("\n## Verify\n\n" + verifyLine + "\n")
	return b.Bytes(), nil
}
