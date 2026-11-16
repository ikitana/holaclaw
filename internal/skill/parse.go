package skill

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Parse parses one SKILL.md of the given level. dir is the skill folder: a
// filesystem path for agent skills, the slash path inside the bundled root
// for bundled ones; its last element must equal the name.
//
// The frontmatter follows the AgentSkills spec: name and description are
// required; license, compatibility, allowed-tools and metadata are optional.
// license, compatibility, allowed-tools and every metadata key but holaclaw
// must be valid YAML but are never interpreted: allowed-tools grants nothing,
// and requirements come only from metadata.holaclaw. Parsing is strict
// otherwise: a YAML syntax error or duplicate key, an unknown top-level key or
// key inside metadata.holaclaw, a value of the wrong type, a description over
// 1024 characters and a body over 8 000 characters are errors, so such a skill
// is never loaded. The error is a Problem carrying its code; line numbers are
// lines of SKILL.md.
func Parse(level Level, dir string, b []byte) (Skill, error) {
	pdir := problemDir(level, dir)
	fail := func(code Code, format string, a ...any) (Skill, error) {
		return Skill{}, Problem{Code: code, Dir: pdir, Msg: fmt.Sprintf(format, a...)}
	}
	if level != LevelBundled && level != LevelAgent {
		return Skill{}, fmt.Errorf("skill: unknown level %q", level)
	}

	front, body, first, ok := splitFrontmatter(string(b))
	if !ok {
		return fail(CodeNoFrontmatter, "SKILL.md must start with a `---` line and close the frontmatter with another")
	}
	version, p := peekVersion(front, first)
	if p != nil {
		p.Dir = pdir
		return Skill{}, *p
	}
	fm, err := decodeYAML(front, first, version)
	if err != nil {
		p := yamlProblem(err)
		p.Dir = pdir
		return Skill{}, *p
	}

	sk := Skill{Level: level, Body: body}
	if level == LevelBundled {
		sk.fsPath = dir
	} else {
		sk.Dir = dir
	}
	if p := decode(fm, level, &sk); p != nil {
		p.Dir = pdir
		return Skill{}, *p
	}

	base := filepath.Base(dir)
	if level == LevelBundled {
		base = path.Base(dir)
	}
	switch {
	case !ValidName(sk.Name):
		return fail(CodeBadName, "field name: %q must match ^[a-z0-9-]{1,64}$", sk.Name)
	case sk.Name != base:
		return fail(CodeBadName, "field name: %q differs from the directory name %q", sk.Name, base)
	}
	if msg := yamlDescriptionIssue(sk.Description); msg != "" {
		return fail(CodeDescription, "field description: %s", msg)
	}
	if n := utf8.RuneCountInString(body); n > MaxBody {
		return fail(CodeTooLong, "body is %d characters, over %d; move detail into references/", n, MaxBody)
	}
	return sk, nil
}

// problemDir is the Dir a Problem reports.
func problemDir(level Level, dir string) string {
	if level == LevelBundled {
		return path.Join("bundled", dir)
	}
	return dir
}

// yamlDescriptionIssue returns why a skill's frontmatter description, or a
// draft's When that Render writes there, is invalid, or "".
func yamlDescriptionIssue(s string) string {
	switch n := utf8.RuneCountInString(s); {
	case strings.TrimSpace(s) == "":
		return "empty; one sentence, the trigger first"
	case strings.ContainsAny(s, "\n\r"):
		return "must be one line"
	case n > MaxDescription:
		return fmt.Sprintf("%d characters, over %d", n, MaxDescription)
	}
	return ""
}

// splitFrontmatter separates the frontmatter from the body. first is the file
// line number of the frontmatter's first line. A delimiter line may carry
// trailing spaces or tabs.
func splitFrontmatter(s string) (front, body string, first int, ok bool) {
	open, rest, found := strings.Cut(s, "\n")
	if !found || !isDelimiter(open) {
		return "", "", 0, false
	}
	for off := 0; off <= len(rest); {
		end := strings.IndexByte(rest[off:], '\n')
		line := rest[off:]
		next := len(rest)
		if end >= 0 {
			line = rest[off : off+end]
			next = off + end + 1
		}
		if isDelimiter(line) {
			return rest[:off], rest[next:], 2, true
		}
		if end < 0 {
			break
		}
		off = next
	}
	return "", "", 0, false
}

// isDelimiter reports whether line is `---`, ignoring trailing whitespace.
func isDelimiter(line string) bool {
	return strings.TrimRight(line, " \t\r") == "---"
}

// asProblem converts a Parse error into a Problem for Load.
func asProblem(err error, dir string) Problem {
	var p Problem
	if errors.As(err, &p) {
		return p
	}
	return Problem{Code: CodeUnreadable, Dir: dir, Msg: err.Error()}
}
