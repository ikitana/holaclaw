// Package skill defines skills: procedures in markdown, loaded only when a
// task needs them (docs/skills.md). It parses and lints SKILL.md folders,
// loads the two roots with the shadowing rule, decides eligibility for a
// run and renders the prompt index, the provenance line and the skills the
// agent saves.
// It is a pure library: it reads the roots it is given and writes nothing; the
// tools and the prompt fragment live elsewhere.
//
// Skills the agent saves are written from a Draft (docs/skills.md §5). Lint checks
// only what a program can: the name, the length and shape of each field and
// of the rendered file; wording is guided on the model side and reviewed by a
// person. Render builds the frontmatter with the YAML library and parses its
// own output back before returning it.
//
// Levels and shadowing (docs/skills.md §1, §2, §5): bundled skills ship with
// holaclaw, agent skills live in the agent directory. Skills follow the agent,
// never a person. By name, an agent skill shadows a bundled one. A skill the
// agent saved is an agent skill carrying a metadata.holaclaw.learned block that
// names who taught it and the trace; a bundled skill never carries one. A
// skill grants nothing: eligibility (§3) is decided by the daemon, never the
// model.
//
// Frontmatter follows the AgentSkills spec: name and description, optional
// license, compatibility, allowed-tools and metadata. holaclaw reads only
// metadata.holaclaw (version, skill_version, requires, learned); the other
// spec keys and the other metadata namespaces are accepted and ignored.
// allowed-tools grants nothing. metadata.holaclaw.version names the format of
// the block and is required when the block is present, so a skill without
// the block (one written for another tool) loads as is.
// The frontmatter is YAML, decoded by go.yaml.in/yaml/v3 into strict structs:
// an unknown key at the top level or inside metadata.holaclaw is an error, and
// a panic in the library is reported as a syntax error, never propagated.
//
// Problem and lint codes. Stable strings: an existing code never changes
// meaning and a retired one is never reused. SKL001–SKL007 are the `check`
// rules of docs/skills.md §8; this package owns SKL1xx (a skill's files),
// SKL2xx (Writer) and SKL3xx (install). Every error a caller tells apart is a
// Problem, matched by its Code or with errors.Is against an Err value, never
// by its text.
//
//	SKL101  SKILL.md has no frontmatter, or it is not closed by `---`
//	SKL102  YAML syntax error (as reported by the parser), including duplicate keys
//	SKL103  unknown frontmatter key: top level outside the AgentSkills spec, or inside metadata.holaclaw
//	SKL104  required key missing: name, description, metadata.holaclaw.version, or a key of metadata.holaclaw.learned
//	SKL105  wrong type or value: a list where a scalar belongs, a bad date, ULID (by, from), origin, binary,
//	        metadata.holaclaw.version this holaclaw does not read, or skill_version below 1
//	SKL106  name does not match ^[a-z0-9-]{1,64}$ or differs from the directory name
//	SKL107  description (or a draft's When) empty, multi-line or over 1024 characters
//	SKL108  body (or a rendered draft) over 8 000 characters
//	SKL109  metadata.holaclaw.learned on a bundled skill
//	SKL110  SKILL.md unreadable
//	SKL111  fewer than 2 or more than 24 steps
//	SKL112  a step or pitfall empty, multi-line or over 300 characters
//	SKL113  more than 6 pitfalls
//	SKL114  a draft's Title empty, multi-line or over 128 characters
//	SKL115  a reference name that is not a relative path to a regular file under references/
//	SKL116  a reference over 16 KiB
//	SKL201  no such agent skill, trashed skill or version
//	SKL202  a path under the skills root is a symlink, leaves the root, or a version file is not a
//	        regular file; a person must fix the folder
//	SKL203  a live skill already has the name (restore never overwrites)
//	SKL204  a write aimed at a bundled skill
//	SKL301  an install source that does not parse, fetch or stage
//	SKL302  the install scan found something and force was not given
//	SKL303  the install source no longer hashes to the hash of its preview
//	SKL304  an install aimed at a collection without `all`, or `all` with a name
package skill

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"holaclaw/internal/event"
)

// Code identifies a kind of Problem. A caller compares it with the constants
// below, never with a literal or with the error text.
type Code string

// Codes, see the package comment.
const (
	CodeNoFrontmatter   Code = "SKL101"
	CodeSyntax          Code = "SKL102"
	CodeUnknownKey      Code = "SKL103"
	CodeMissing         Code = "SKL104"
	CodeBadValue        Code = "SKL105"
	CodeBadName         Code = "SKL106"
	CodeDescription     Code = "SKL107"
	CodeTooLong         Code = "SKL108"
	CodeLearnedNotAllow Code = "SKL109"
	CodeUnreadable      Code = "SKL110"
	CodeStepCount       Code = "SKL111"
	CodeItemLength      Code = "SKL112"
	CodePitfallCount    Code = "SKL113"
	CodeTitle           Code = "SKL114"
	CodeBadReference    Code = "SKL115"
	CodeReferenceSize   Code = "SKL116"
	CodeNotFound        Code = "SKL201"
	CodeUnsafePath      Code = "SKL202"
	CodeExists          Code = "SKL203"
	CodeBundled         Code = "SKL204"
	CodeBadSource       Code = "SKL301"
	CodeUnsafe          Code = "SKL302"
	CodeChanged         Code = "SKL303"
	CodeCollection      Code = "SKL304"
)

// Limits from docs/skills.md §2, §4 and §5, in characters (runes).
const (
	MaxDescription = 1024
	MaxIndexed     = 160 // of a description shown in the prompt index; the rest is cut
	MaxBody        = 8000
	MaxReference   = 16 << 10 // bytes of one reference; a larger one is refused
	MaxListed      = 200      // entries in Skill.References and Skill.Scripts
	maxFile        = 64 << 10 // bytes read of one SKILL.md
)

// Level is where a skill comes from; it decides shadowing and provenance.
type Level string

const (
	LevelBundled Level = "bundled"
	LevelAgent   Level = "agent"
)

// precedence orders levels for shadowing: higher shadows lower.
func (l Level) precedence() int {
	if l == LevelAgent {
		return 1
	}
	return 0
}

// Origin is why the agent saved a skill (docs/skills.md §5).
type Origin string

const (
	OriginCorrection Origin = "correction" // the person said how it should have been done
	OriginExplicit   Origin = "explicit"   // the person asked for it to be saved
)

// Bin is a binary a skill runs through exec; Pkg is its nixpkgs attribute and
// defaults to Name (docs/skills.md §8).
type Bin struct {
	Name, Pkg string
}

// Requires is the metadata.holaclaw.requires block: what the skill needs, never what
// it grants.
type Requires struct {
	Tools []string
	Bins  []Bin
	Net   []string // the bins that may open the network
}

// Learned is the metadata.holaclaw.learned block, written by the daemon only,
// on an agent skill the agent saved.
type Learned struct {
	On     time.Time // when it was saved, UTC, to the second; midnight for a bare date
	By     event.ID  // the identity that taught it (docs/identity.md)
	From   event.ID  // the trace that saved it
	Origin Origin
}

// Skill is one parsed SKILL.md.
type Skill struct {
	Name, Description string
	Requires          Requires
	Learned           *Learned // set when the daemon wrote this version; never on a bundled skill
	Version           int      // metadata.holaclaw.skill_version; 0 when the file carries none
	Level             Level
	Dir               string // absolute folder for agent skills; "" for bundled
	Body              string // markdown after the frontmatter, never preprocessed
	// References and Scripts are the regular files under references/ and
	// scripts/, as slash paths relative to that folder, sorted; hidden entries
	// and symlinks are skipped and each list stops at MaxListed. Filled by
	// Load, nil from Parse. Scripts are listed only, never run here.
	References, Scripts []string

	fsys   fs.FS  // bundled only: the root it was read from
	fsPath string // bundled only: the folder inside fsys
}

// Problem is an error of this package: a stable code (see the package
// comment), the skill folder and a message naming the field, line or path at
// fault. Load collects them all, so one broken folder never hides the others;
// Writer and Reference return one. Named after config.Problem, which reports
// config faults the same way.
type Problem struct {
	Code Code
	Dir  string // the skill folder; "bundled/<name>" for bundled skills
	Msg  string // names the file line, field or path
}

func (p Problem) Error() string {
	s := string(p.Code)
	for _, part := range []string{p.Dir, p.Msg} {
		if part != "" {
			s += ": " + part
		}
	}
	return s
}

// Is matches any Problem with the same code, so errors.Is(err, ErrNotFound)
// holds for every not-found Problem whatever its folder or message.
func (p Problem) Is(target error) bool {
	t, ok := target.(Problem)
	return ok && t.Code == p.Code
}

var nameRE = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// ValidName reports whether s is a valid skill name.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Set is the result of Load: every loaded skill, in the order of All.
type Set struct {
	skills []Skill
}

// All returns every loaded skill ordered by level (bundled, agent), then
// name.
func (s Set) All() []Skill { return slices.Clone(s.skills) }

// Resolve returns the skills with shadowing applied: an agent skill over a
// bundled one of the same name. Sorted by name.
func (s Set) Resolve() []Skill {
	byName := map[string]Skill{}
	for _, sk := range s.skills { // All order: lower precedence first
		byName[sk.Name] = sk
	}
	out := make([]Skill, 0, len(byName))
	for _, sk := range byName {
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get resolves one name as Resolve does.
func (s Set) Get(name string) (Skill, bool) {
	var best Skill
	found := false
	for _, sk := range s.skills {
		if sk.Name == name {
			best, found = sk, true
		}
	}
	return best, found
}

func newSet(skills []Skill) Set {
	sort.SliceStable(skills, func(i, j int) bool {
		a, b := skills[i], skills[j]
		if a.Level != b.Level {
			return a.Level.precedence() < b.Level.precedence()
		}
		return a.Name < b.Name
	})
	return Set{skills: skills}
}

// Env is what eligibility needs, all decided by the daemon, never the model.
type Env struct {
	Tools    []string // the agent's tool set (agent.toml `tools`)
	Denied   []string // tools the policy denies for the sender's role
	Disabled []string // agent.toml `skills.disabled`
}

// ReasonKind is why a skill is not eligible, in the order Eligible checks.
type ReasonKind string

const (
	ReasonMissingTool ReasonKind = "missing_tool" // a required tool the agent does not list
	ReasonDeniedTool  ReasonKind = "denied_tool"  // a required tool the policy denies for the role
	ReasonNeedsBinary ReasonKind = "needs_binary" // a required binary; none is supported yet
	ReasonDisabled    ReasonKind = "disabled"     // agent.toml `skills.disabled`
)

// Reason is why a skill is not eligible: the kind and, for a tool or binary,
// its name. The zero Reason is an eligible skill.
type Reason struct {
	Kind    ReasonKind
	Subject string
}

// String is the reason in words.
func (r Reason) String() string {
	switch r.Kind {
	case ReasonMissingTool:
		return "requires tool " + r.Subject + ", not in the agent's tools"
	case ReasonDeniedTool:
		return "tool " + r.Subject + " is denied for this role"
	case ReasonNeedsBinary:
		return "requires binary " + r.Subject + " (binaries are not supported yet)"
	default:
		return string(r.Kind)
	}
}

// Eligible reports whether sk is in the index for a run and, if not, why,
// in the order of docs/skills.md §3. A skill that requires any binary is
// never eligible until exec and the sandbox land.
func Eligible(sk Skill, e Env) (ok bool, why Reason) {
	for _, t := range sk.Requires.Tools {
		if !slices.Contains(e.Tools, t) {
			return false, Reason{ReasonMissingTool, t}
		}
	}
	for _, t := range sk.Requires.Tools {
		if slices.Contains(e.Denied, t) {
			return false, Reason{ReasonDeniedTool, t}
		}
	}
	if len(sk.Requires.Bins) > 0 {
		return false, Reason{ReasonNeedsBinary, sk.Requires.Bins[0].Name}
	}
	if slices.Contains(e.Disabled, sk.Name) {
		return false, Reason{Kind: ReasonDisabled}
	}
	return true, Reason{}
}

// Index renders the prompt index: one "- name: description" line per skill,
// in the order given, with no level marker (docs/skills.md §4). A description
// over MaxIndexed characters is cut there and marked with "…", so a long one
// does not grow every turn's prompt.
func Index(skills []Skill) string {
	var b strings.Builder
	for _, sk := range skills {
		b.WriteString("- ")
		b.WriteString(sk.Name)
		b.WriteString(": ")
		b.WriteString(indexed(sk.Description))
		b.WriteString("\n")
	}
	return b.String()
}

// indexed returns d cut to MaxIndexed characters, marking a cut.
func indexed(d string) string {
	if utf8.RuneCountInString(d) <= MaxIndexed {
		return d
	}
	return string([]rune(d)[:MaxIndexed]) + "…"
}

// Provenance is the first line of a loaded body, so the model knows doctrine
// from recent inference (docs/skills.md §4). teacher is the name of the
// identity in Learned.By, looked up by the caller; it is read only for a
// skill with a learned block, and an empty one leaves the name out.
func Provenance(sk Skill, teacher string) string {
	switch {
	case sk.Learned != nil && teacher != "":
		return "[skill: " + sk.Name + " · learned from " + teacher + " on " + sk.Learned.On.Format(time.DateOnly) + "]"
	case sk.Learned != nil:
		return "[skill: " + sk.Name + " · learned on " + sk.Learned.On.Format(time.DateOnly) + "]"
	case sk.Level == LevelBundled:
		return "[skill: " + sk.Name + " · bundled skill]"
	default:
		return "[skill: " + sk.Name + " · agent skill]"
	}
}

// Errors from Reference, matched with errors.Is; a missing file wraps
// fs.ErrNotExist.
var (
	ErrBadReference = Problem{Code: CodeBadReference}
	ErrTooLarge     = Problem{Code: CodeReferenceSize}
)

// Reference reads a file under the skill's references/ folder. name is a
// slash-separated relative path; "..", absolute paths and symlinks leaving
// references/ are refused (os.Root), and a file over MaxReference is refused
// whole, never truncated.
func (sk Skill) Reference(name string) ([]byte, error) {
	if !fs.ValidPath(name) || name == "." || strings.Contains(name, `\`) {
		return nil, Problem{Code: CodeBadReference, Dir: problemDir(sk.Level, sk.Dir), Msg: fmt.Sprintf("reference %q: not a relative path under references/", name)}
	}
	var f fs.File
	switch sk.Level {
	case LevelBundled:
		if sk.fsys == nil {
			return nil, fmt.Errorf("skill %s: no bundled root: %w", sk.Name, fs.ErrNotExist)
		}
		var err error
		if f, err = sk.fsys.Open(path.Join(sk.fsPath, "references", name)); err != nil {
			return nil, err
		}
	default:
		root, err := os.OpenRoot(filepath.Join(sk.Dir, "references"))
		if err != nil {
			return nil, err
		}
		defer func() { _ = root.Close() }()
		if f, err = root.Open(filepath.FromSlash(name)); err != nil {
			return nil, err
		}
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, Problem{Code: CodeBadReference, Dir: problemDir(sk.Level, sk.Dir), Msg: fmt.Sprintf("reference %q: not a regular file", name)}
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxReference+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxReference {
		return nil, Problem{Code: CodeReferenceSize, Dir: problemDir(sk.Level, sk.Dir), Msg: fmt.Sprintf("reference %q: over %d bytes", name, MaxReference)}
	}
	return b, nil
}
