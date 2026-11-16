// The strict decoding of the AgentSkills frontmatter. Parse calls
// splitFrontmatter, then decodeYAML (syntax: go.yaml.in/yaml/v3 into the
// structs below, unknown keys refused), then decode (values: required keys,
// names, requirements, the versions and the learned block). peekVersion runs
// first and picks the format decodeYAML reads.

package skill

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"holaclaw/internal/event"
)

// binRE bounds binary and package names: a name, never a path (docs/skills.md §8).
var binRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// frontmatter is the decoded frontmatter. The spec keys holaclaw never
// interprets are kept as nodes, so any value is accepted and none is expanded.
type frontmatter struct {
	Name          string        `yaml:"name"`
	Description   string        `yaml:"description"`
	License       yaml.Node     `yaml:"license"`
	Compatibility yaml.Node     `yaml:"compatibility"`
	AllowedTools  yaml.Node     `yaml:"allowed-tools"`
	Metadata      *metadataYAML `yaml:"metadata"`
}

// metadataYAML is the metadata mapping: holaclaw is read, every other key is
// another tool's namespace, kept as a node and ignored.
type metadataYAML struct {
	HolaClaw *holaclawYAML        `yaml:"holaclaw"`
	Other    map[string]yaml.Node `yaml:",inline"`
}

// holaclawYAML is metadata.holaclaw in format 1 (MetadataVersion).
type holaclawYAML struct {
	Version      int           `yaml:"version"`
	SkillVersion yaml.Node     `yaml:"skill_version"` // an integer, checked by decode
	Requires     *requiresYAML `yaml:"requires"`
	Learned      *learnedYAML  `yaml:"learned"`
}

// requiresYAML holds pointers because the library drops null list entries
// from a slice of values; a nil entry is reported instead.
type requiresYAML struct {
	Tools []*string `yaml:"tools"`
	Bins  []*Bin    `yaml:"bins"`
	Net   []*string `yaml:"net"`
}

// learnedYAML holds strings, so on is never read as a YAML timestamp.
type learnedYAML struct {
	On     string `yaml:"on"`
	By     string `yaml:"by"`
	From   string `yaml:"from"`
	Origin string `yaml:"origin"`
}

// structs maps the Go type the decoder names in an unknown-field error to the
// field path and the allowed keys.
var structs = map[string]struct{ prefix, allowed string }{
	"skill.frontmatter":  {"", "name, description, license, compatibility, allowed-tools, metadata"},
	"skill.holaclawYAML": {"metadata.holaclaw.", "version, skill_version, requires, learned"},
	"skill.requiresYAML": {"metadata.holaclaw.requires.", "tools, bins, net"},
	"skill.learnedYAML":  {"metadata.holaclaw.learned.", "on, by, from, origin"},
}

// MetadataVersion is the metadata.holaclaw format this holaclaw writes, in
// metadata.holaclaw.version. metadataFormats lists every format it reads; a
// new format adds its number there and its structs to decodeYAML. There is
// no migration between formats yet.
const MetadataVersion = 1

var metadataFormats = []int{1}

// peekVersion reads metadata.holaclaw.version before the strict decode, so a
// file in a newer format is refused for its version, not for a key that
// format added. It returns 0 when there is no metadata.holaclaw: an imported
// skill may omit the block, but once it is present the version is required.
// Every other fault is left to the strict decode.
func peekVersion(front string, first int) (v int, p *Problem) {
	const field = "metadata.holaclaw.version"
	defer func() {
		if r := recover(); r != nil {
			v, p = 0, nil
		}
	}()
	var loose struct {
		Metadata struct {
			HolaClaw *struct {
				Version yaml.Node `yaml:"version"`
			} `yaml:"holaclaw"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(strings.Repeat("\n", first-1)+front), &loose); err != nil || loose.Metadata.HolaClaw == nil {
		return 0, nil
	}
	n := loose.Metadata.HolaClaw.Version
	switch {
	case n.Kind == 0:
		return 0, missing(field, fmt.Sprintf("required when metadata.holaclaw is present; this holaclaw writes %d", MetadataVersion))
	case !isInt(&n) || n.Decode(&v) != nil:
		return 0, bad(field, fmt.Sprintf("line %d: expected an integer", n.Line))
	case !slices.Contains(metadataFormats, v):
		return 0, bad(field, fmt.Sprintf("line %d: format %d is not one this holaclaw reads (%s)", n.Line, v, formatList()))
	}
	return v, nil
}

// isInt reports whether n is a plain integer scalar. The library would
// truncate a float such as 1.5 into an int, so the tag is checked first.
func isInt(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!int"
}

// decodeSkillVersion reads metadata.holaclaw.skill_version into v: absent is
// 0, otherwise an integer of at least 1.
func decodeSkillVersion(n *yaml.Node, v *int) *Problem {
	const field = "metadata.holaclaw.skill_version"
	if n.Kind == 0 {
		return nil
	}
	if !isInt(n) || n.Decode(v) != nil {
		return bad(field, fmt.Sprintf("line %d: expected an integer", n.Line))
	}
	if *v < 1 {
		return bad(field, fmt.Sprintf("%d: a skill version starts at 1", *v))
	}
	return nil
}

func formatList() string {
	s := make([]string, len(metadataFormats))
	for i, v := range metadataFormats {
		s[i] = strconv.Itoa(v)
	}
	return strings.Join(s, ", ")
}

// decodeYAML decodes the frontmatter strictly, in the metadata.holaclaw format
// peekVersion read (0: no metadata.holaclaw). first is the file line of the
// frontmatter's first line, so the library reports file lines. A panic inside
// the library is returned as an error.
func decodeYAML(front string, first, version int) (fm frontmatter, err error) {
	switch version {
	case 0, 1: // frontmatter and the structs below
	default:
		return frontmatter{}, fmt.Errorf("yaml: no decoder for metadata.holaclaw format %d", version)
	}
	defer func() {
		if r := recover(); r != nil {
			fm, err = frontmatter{}, fmt.Errorf("yaml: parser failed: %v", r)
		}
	}()
	dec := yaml.NewDecoder(strings.NewReader(strings.Repeat("\n", first-1) + front))
	dec.KnownFields(true)
	if err := dec.Decode(&fm); err != nil && !errors.Is(err, io.EOF) {
		return frontmatter{}, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("yaml: more than one YAML document")
		}
		return frontmatter{}, err
	}
	return fm, nil
}

var (
	unknownRE = regexp.MustCompile(`^line (\d+): field (.*) not found in type (\S+)$`)
	typeRE    = regexp.MustCompile(`^line (\d+): cannot unmarshal (\S+).* into (\S+)$`)
)

// yamlProblem converts a decodeYAML error into a Problem: a Problem from
// Bin.UnmarshalYAML as is, an unknown field as SKL103, a type mismatch as
// SKL105, anything else (syntax, duplicate keys) as SKL102.
func yamlProblem(err error) *Problem {
	var p Problem
	if errors.As(err, &p) {
		return &p
	}
	var te *yaml.TypeError
	if !errors.As(err, &te) || len(te.Errors) == 0 {
		return &Problem{Code: CodeSyntax, Msg: "SKILL.md " + err.Error()}
	}
	e := te.Errors[0]
	if m := unknownRE.FindStringSubmatch(e); m != nil {
		s := structs[m[3]]
		if s.prefix == "" && m[2] == "holaclaw" {
			return &Problem{Code: CodeUnknownKey, Msg: "field holaclaw: unknown top-level key; use metadata.holaclaw"}
		}
		return &Problem{Code: CodeUnknownKey, Msg: fmt.Sprintf("field %s%s: line %s: unknown key; allowed: %s", s.prefix, m[2], m[1], s.allowed)}
	}
	if m := typeRE.FindStringSubmatch(e); m != nil {
		return &Problem{Code: CodeBadValue, Msg: fmt.Sprintf("line %s: expected %s, got %s", m[1], kindOfType(m[3]), kindOfTag(m[2]))}
	}
	return &Problem{Code: CodeSyntax, Msg: "SKILL.md yaml: " + e}
}

// kindOfType names the YAML kind a Go type of this package decodes from.
func kindOfType(t string) string {
	switch {
	case strings.HasPrefix(t, "[]"):
		return "a list"
	case t == "string":
		return "a scalar"
	case t == "int":
		return "an integer"
	default:
		return "a mapping"
	}
}

// kindOfTag names the YAML kind of a node tag.
func kindOfTag(tag string) string {
	switch tag {
	case "!!seq":
		return "a list"
	case "!!map":
		return "a mapping"
	default:
		return "a scalar"
	}
}

// decode fills sk from the decoded frontmatter, checking values.
func decode(fm frontmatter, level Level, sk *Skill) *Problem {
	sk.Name, sk.Description = fm.Name, fm.Description
	if strings.TrimSpace(sk.Name) == "" {
		return missing("name", "required")
	}
	if strings.TrimSpace(sk.Description) == "" {
		return missing("description", "required")
	}
	var l *learnedYAML
	if fm.Metadata != nil && fm.Metadata.HolaClaw != nil {
		hb := fm.Metadata.HolaClaw
		if hb.Requires != nil {
			if p := decodeRequires(hb.Requires, &sk.Requires); p != nil {
				return p
			}
		}
		if p := decodeSkillVersion(&hb.SkillVersion, &sk.Version); p != nil {
			return p
		}
		l = hb.Learned
	}
	switch {
	case l != nil && level == LevelBundled:
		return &Problem{Code: CodeLearnedNotAllow, Msg: "field metadata.holaclaw.learned: only an agent skill the agent saved carries it, this is a bundled skill"}
	case l != nil:
		sk.Learned = &Learned{}
		return decodeLearned(l, sk.Learned)
	}
	return nil
}

func decodeRequires(in *requiresYAML, r *Requires) *Problem {
	var p *Problem
	if r.Tools, p = names("metadata.holaclaw.requires.tools", in.Tools); p != nil {
		return p
	}
	if r.Net, p = names("metadata.holaclaw.requires.net", in.Net); p != nil {
		return p
	}
	for _, b := range in.Bins {
		if b == nil {
			return bad("metadata.holaclaw.requires.bins", "empty entry")
		}
		r.Bins = append(r.Bins, *b)
	}
	for _, t := range r.Tools {
		if strings.TrimSpace(t) == "" {
			return bad("metadata.holaclaw.requires.tools", "empty tool name")
		}
	}
	for _, b := range r.Bins {
		if !binRE.MatchString(b.Name) || !binRE.MatchString(b.Pkg) {
			return bad("metadata.holaclaw.requires.bins", fmt.Sprintf("%q / %q is not a binary or package name", b.Name, b.Pkg))
		}
	}
	for _, name := range r.Net {
		if !hasBin(r.Bins, name) {
			return bad("metadata.holaclaw.requires.net", fmt.Sprintf("%q is not one of the skill's bins", name))
		}
	}
	return nil
}

func hasBin(bins []Bin, name string) bool {
	for _, b := range bins {
		if b.Name == name {
			return true
		}
	}
	return false
}

// UnmarshalYAML reads a bare name or a {name, pkg} mapping; Pkg defaults to
// Name. A bad entry is returned as a Problem, which stops decoding; the names
// themselves are checked by Parse.
func (b *Bin) UnmarshalYAML(n *yaml.Node) error {
	const field = "metadata.holaclaw.requires.bins"
	switch n.Kind {
	case yaml.ScalarNode:
		*b = Bin{Name: n.Value, Pkg: n.Value}
		return nil
	case yaml.MappingNode:
	default:
		return *bad(field, fmt.Sprintf("line %d: each entry is a name or {name: x, pkg: y}", n.Line))
	}
	*b = Bin{}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		if k.Value != "name" && k.Value != "pkg" {
			return Problem{Code: CodeUnknownKey, Msg: fmt.Sprintf("field %s.%s: line %d: unknown key; allowed: name, pkg", field, k.Value, k.Line)}
		}
		if seen[k.Value] {
			return Problem{Code: CodeSyntax, Msg: fmt.Sprintf("SKILL.md yaml: line %d: mapping key %q already defined", k.Line, k.Value)}
		}
		seen[k.Value] = true
		if v.Kind == yaml.AliasNode && v.Alias != nil {
			v = v.Alias
		}
		if v.Kind != yaml.ScalarNode {
			return *bad(field+"."+k.Value, fmt.Sprintf("line %d: expected a scalar", v.Line))
		}
		if k.Value == "name" {
			b.Name = v.Value
		} else {
			b.Pkg = v.Value
		}
	}
	if strings.TrimSpace(b.Name) == "" {
		return *missing(field+".name", "required")
	}
	if b.Pkg == "" {
		b.Pkg = b.Name
	}
	return nil
}

// parseLearnedOn reads metadata.holaclaw.learned.on: the RFC 3339 time the
// daemon writes, or a bare date (a hand-written file, or one saved before the
// time was recorded), read as midnight UTC. The result is in UTC.
func parseLearnedOn(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Parse(time.DateOnly, s)
}

func decodeLearned(in *learnedYAML, l *Learned) *Problem {
	const field = "metadata.holaclaw.learned"
	for _, f := range []struct{ key, v string }{{"on", in.On}, {"by", in.By}, {"from", in.From}, {"origin", in.Origin}} {
		if strings.TrimSpace(f.v) == "" {
			return missing(field+"."+f.key, "required")
		}
	}
	t, err := parseLearnedOn(in.On)
	if err != nil {
		return bad(field+".on", fmt.Sprintf("%q is not a time (RFC 3339, e.g. 2026-09-12T14:03:05Z) or a date YYYY-MM-DD", in.On))
	}
	if !event.ID(in.By).Valid() {
		return bad(field+".by", fmt.Sprintf("%q is not an identity id (ULID)", in.By))
	}
	if !event.ID(in.From).Valid() {
		return bad(field+".from", fmt.Sprintf("%q is not a trace id (ULID)", in.From))
	}
	if o := Origin(in.Origin); o != OriginCorrection && o != OriginExplicit {
		return bad(field+".origin", fmt.Sprintf("%q must be correction or explicit", in.Origin))
	}
	*l = Learned{On: t, By: event.ID(in.By), From: event.ID(in.From), Origin: Origin(in.Origin)}
	return nil
}

func missing(field, msg string) *Problem {
	return &Problem{Code: CodeMissing, Msg: "field " + field + ": " + msg}
}

func bad(field, msg string) *Problem {
	return &Problem{Code: CodeBadValue, Msg: "field " + field + ": " + msg}
}

// names dereferences a decoded list of names; a null entry is an error.
func names(field string, in []*string) ([]string, *Problem) {
	var out []string
	for _, s := range in {
		if s == nil {
			return nil, bad(field, "empty entry")
		}
		out = append(out, *s)
	}
	return out, nil
}
