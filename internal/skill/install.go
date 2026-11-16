package skill

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"holaclaw/internal/fsutil"
)

// Install (docs/skills.md §7): a person brings a skill folder from a source
// they chose into agents/<name>/skills/. No hub, no update: a local directory,
// or a git repository pinned to a ref. The daemon never writes under agents/
// on its own; an install is a person-initiated write there, through the
// admin API, and it never overwrites a folder.
//
// The steps, in the order the API runs them: Fetch (a GitSource) or
// OpenUpload (a folder the CLI sent, SaveUpload), Stage (a copy under the
// tmp root, regular files only, the frontmatter name rewritten for `--as`),
// Scan, Hash, Load over the staging directory to parse it, then Install
// (the rename) or a cleanup. The tmp root is on the skills root's
// filesystem, so the rename is atomic.

// Errors from the install path, besides ErrExists and ErrBadName, matched
// with errors.Is; the detail is wrapped around them.
var (
	ErrBadSource = Problem{Code: CodeBadSource} // a source that does not parse, fetch or stage (a 400)
	ErrUnsafe    = Problem{Code: CodeUnsafe}    // the scan found something and force was not given
	ErrChanged   = Problem{Code: CodeChanged}   // the source no longer hashes to the hash of the preview
)

// Scan codes. Like the SKL codes: a code keeps its meaning, a retired one is
// never reused.
const (
	CodeScanShell   = "SCN001" // a pipe into a shell (`curl … | sh`, `| bash`)
	CodeScanSSH     = "SCN002" // ssh material (`~/.ssh`, `id_rsa`, `authorized_keys`)
	CodeScanUnicode = "SCN003" // hidden or bidirectional Unicode (zero-width, U+202A–U+202E, U+2066–U+2069, tags)
)

// OriginFile is where an installed skill records where it came from, inside
// its folder: a hidden directory, so the loader never lists it as a reference.
const OriginFile = ".holaclaw/origin.toml"

// Limits on an install, a staged folder or an upload: past either of them
// the source is ErrBadSource. There is none on one file.
const (
	MaxInstallFiles = 256       // regular files, and directories counted apart
	MaxInstallTotal = 128 << 20 // bytes, the files together
)

// hasSkillFile is ErrBadSource unless dir/SKILL.md is a regular file.
func hasSkillFile(dir string) error {
	st, err := os.Lstat(filepath.Join(dir, "SKILL.md"))
	if err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("%w: no SKILL.md in %s", ErrBadSource, dir)
	}
	return nil
}

// Stage copies the skill folder at from into
// <tmpRoot>/skill-stage-<random>/<name>, and returns that folder. Only
// regular files and directories are copied, with their permission bits,
// `.git` and `.holaclaw` are skipped, a symlink anywhere is ErrBadSource, and
// the limits above apply. When name differs from the source's directory
// name, the frontmatter `name:` line of SKILL.md is rewritten to name so the
// copy parses (the loader requires the two to match); name must be
// ValidName. The caller removes the parent `skill-stage-*` directory when
// done (RemoveStage), whether it installed or not. tmpRoot is created if
// missing; it is outside every skills root, so no loader sees a stage, and
// CleanTmp removes one a crash left.
func Stage(from, tmpRoot, name string) (dir string, err error) {
	if !ValidName(name) {
		return "", fmt.Errorf("%w: %q", ErrBadName, name)
	}
	src, err := filepath.EvalSymlinks(from)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadSource, err)
	}
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(tmpRoot, stagePrefix+"*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(stage)
		}
	}()
	dir = filepath.Join(stage, name)
	if err := fsutil.CopyTree(src, dir, installCopy); err != nil {
		return "", copyErr(err)
	}
	if name != filepath.Base(from) {
		if err := renameSkill(filepath.Join(dir, "SKILL.md"), name); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// stagePrefix starts the name of a staging directory.
const stagePrefix = tmpPrefix + "stage-"

// tmpPrefix starts the name of every entry an install makes under its tmp
// root.
const tmpPrefix = "skill-"

// skipped reports whether the walks leave an entry out: git metadata and
// the origin record.
func skipped(name string) bool { return name == ".git" || name == ".holaclaw" }

// skipEntry is skipped as an fsutil skip.
func skipEntry(_ string, d fs.DirEntry) bool { return skipped(d.Name()) }

// installCopy is how a source is copied: regular files and directories
// only, a symlink refused, within the install limits.
var installCopy = fsutil.CopyOptions{Skip: skipEntry, RefuseSymlinks: true, MaxFiles: MaxInstallFiles, MaxBytes: MaxInstallTotal}

// copyErr is ErrBadSource around an error of a copy that the source caused:
// a symlink, another entry that is not a file, or a limit.
func copyErr(err error) error {
	if errors.Is(err, fsutil.ErrSymlink) || errors.Is(err, fsutil.ErrNotRegular) ||
		errors.Is(err, fsutil.ErrTooMany) || errors.Is(err, fsutil.ErrTooLarge) {
		return fmt.Errorf("%w: %w", ErrBadSource, err)
	}
	return err
}

// nameLineRE matches the frontmatter line that holds the name.
var nameLineRE = regexp.MustCompile(`^name:\s*`)

// renameSkill sets the one `name:` line in the frontmatter of the SKILL.md
// at p to name; the rest of the file is untouched.
func renameSkill(p, name string) error {
	b, err := os.ReadFile(p)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadSource, err)
	}
	lines := strings.SplitAfter(string(b), "\n")
	if strings.TrimRight(lines[0], "\r\n") != "---" {
		return fmt.Errorf("%w: SKILL.md has no frontmatter", ErrBadSource)
	}
	at := -1
	for i := 1; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r\n")
		if line == "---" {
			break
		}
		if nameLineRE.MatchString(line) {
			if at >= 0 {
				return fmt.Errorf("%w: SKILL.md has two name lines", ErrBadSource)
			}
			at = i
		}
	}
	if at < 0 {
		return fmt.Errorf("%w: SKILL.md has no name line", ErrBadSource)
	}
	old := lines[at]
	lines[at] = "name: " + name + old[len(strings.TrimRight(old, "\r\n")):]
	return os.WriteFile(p, []byte(strings.Join(lines, "")), 0o644)
}

// RemoveStage deletes the `skill-stage-*` parent of a staged folder.
// Refuses a dir that is not a staged folder under such a parent.
func RemoveStage(dir string) error {
	parent := filepath.Dir(dir)
	if !strings.HasPrefix(filepath.Base(parent), stagePrefix) {
		return fmt.Errorf("skill: %s is not a staged folder", dir)
	}
	return os.RemoveAll(parent)
}

// Finding is one hit of the install scan: a hint for the person reading the
// body, not a boundary.
type Finding struct {
	File string // slash path inside the folder
	Line int    // 1-based
	Code string // a SCN code
	Msg  string
}

// Scan reads every regular file under dir (`.git` and `.holaclaw` skipped,
// one with a NUL byte in its first 8 KiB skipped as binary) and reports each
// line that pipes into a shell, names ssh material, or carries a hidden or
// bidirectional Unicode character, at most one finding per line and code,
// sorted by file then line. An empty result is a clean folder.
func Scan(dir string) ([]Finding, error) {
	out := []Finding{}
	err := fsutil.WalkFiles(dir, skipEntry, func(rel string, open func() (fs.File, error)) error {
		f, err := open()
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		found, err := scanFile(rel, f)
		out = append(out, found...)
		return err
	})
	if err != nil {
		return nil, err
	}
	// WalkFiles goes in file order and scanFile in line and code order.
	return out, nil
}

// scanFile reads a file in pieces of at most scanChunk bytes, and looks
// for a NUL byte in its first binarySniff bytes.
const (
	scanChunk   = 64 << 10
	binarySniff = 8 << 10
)

// scanFile scans the file rel read from r line by line, never holding more
// than scanChunk bytes of it: a longer line is scanned in pieces, so a
// pattern cut across two pieces is missed. A NUL byte early on makes it
// binary, with no findings.
func scanFile(rel string, r io.Reader) ([]Finding, error) {
	br := bufio.NewReaderSize(r, scanChunk)
	head, err := br.Peek(binarySniff)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return nil, nil
	}
	var out []Finding
	seen := map[string]bool{} // the codes of the line so far
	line, start := 1, true
	for {
		chunk, err := br.ReadSlice('\n')
		end := err == nil || errors.Is(err, io.EOF)
		s := string(chunk)
		if end {
			s = strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r")
		}
		for _, c := range scanLine(s, line == 1 && start) {
			if !seen[c[0]] {
				seen[c[0]] = true
				out = append(out, Finding{File: rel, Line: line, Code: c[0], Msg: c[1]})
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			start = false
			continue
		case errors.Is(err, io.EOF):
			// A line in pieces may have found its codes out of order.
			slices.SortStableFunc(out, func(a, b Finding) int {
				return cmp.Or(cmp.Compare(a.Line, b.Line), strings.Compare(a.Code, b.Code))
			})
			return out, nil
		case err != nil:
			return nil, err
		}
		line, start = line+1, true
		clear(seen)
	}
}

var (
	pipeShellRE = regexp.MustCompile(`\|\s*(?:sudo\s+)?(?:/usr/bin/|/bin/)?((?:ba|z|da|k|fi)?sh)\b`)
	pipeInterRE = regexp.MustCompile(`\|\s*(?:sudo\s+)?(python[23]?|perl|node)\b`)
	pipeFetchRE = regexp.MustCompile(`(curl|wget)\b[^|\n]*\|`)
	sshWords    = []string{"~/.ssh", ".ssh/", "id_rsa", "id_ed25519", "authorized_keys", "known_hosts"}
)

// scanLine returns the (code, message) pairs of one line, at most one per
// code, in code order. first is the file's first line, where a BOM at byte
// 0 is allowed.
func scanLine(line string, first bool) [][2]string {
	var out [][2]string
	if m := pipeShellRE.FindStringSubmatch(line); m != nil {
		out = append(out, [2]string{CodeScanShell, "pipes into " + m[1]})
	} else if m := pipeInterRE.FindStringSubmatch(line); m != nil {
		out = append(out, [2]string{CodeScanShell, "pipes into " + m[1]})
	} else if m := pipeFetchRE.FindStringSubmatch(line); m != nil {
		out = append(out, [2]string{CodeScanShell, "pipes " + m[1] + " into a command"})
	}
	for _, w := range sshWords {
		if strings.Contains(line, w) {
			out = append(out, [2]string{CodeScanSSH, "names ssh material: " + w})
			break
		}
	}
	for i, r := range line {
		if hiddenRune(r) && (r != 0xFEFF || !first || i != 0) {
			out = append(out, [2]string{CodeScanUnicode, fmt.Sprintf("hidden or bidirectional Unicode: U+%04X", r)})
			break
		}
	}
	return out
}

// hiddenRune reports a zero-width, bidirectional, invisible or tag rune.
func hiddenRune(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F, r >= 0x2028 && r <= 0x202E,
		r >= 0x2060 && r <= 0x2064, r >= 0x2066 && r <= 0x2069,
		r >= 0xE0000 && r <= 0xE007F, r == 0xFEFF, r == 0x00AD:
		return true
	}
	return false
}

// Hash is `sha256:<hex>` over the folder: for every regular file in sorted
// slash-path order, its path, a NUL, its size in decimal, a NUL and its
// bytes. `.git` and `.holaclaw` are excluded, so the hash of an installed
// folder equals the hash of its stage, and `check` can tell an edited skill
// (SKL006).
func Hash(dir string) (string, error) {
	h := sha256.New()
	err := fsutil.WalkFiles(dir, skipEntry, func(rel string, open func() (fs.File, error)) error {
		f, err := open()
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		h.Write([]byte(rel + "\x00" + strconv.FormatInt(st.Size(), 10) + "\x00"))
		n, err := io.Copy(h, f)
		if err == nil && n != st.Size() {
			err = fmt.Errorf("skill: %s changed while hashed", rel)
		}
		return err
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// Installed is what OriginFile records: where an installed skill came from.
type Installed struct {
	Source    string    `toml:"source"`           // the git url, or the local path
	Ref       string    `toml:"ref,omitempty"`    // git only
	Path      string    `toml:"path,omitempty"`   // git only, the folder in the repository
	Commit    string    `toml:"commit,omitempty"` // git only
	Hash      string    `toml:"hash"`
	Installed time.Time `toml:"installed"`
}

// ReadOrigin reads OriginFile of a skill folder; ok is false when there is
// none (a hand-written agent skill).
func ReadOrigin(dir string) (o Installed, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(OriginFile)))
	if errors.Is(err, fs.ErrNotExist) {
		return Installed{}, false, nil
	}
	if err != nil {
		return Installed{}, false, err
	}
	if err := toml.Unmarshal(b, &o); err != nil {
		return Installed{}, false, fmt.Errorf("skill: %s: %w", OriginFile, err)
	}
	return o, true, nil
}

// Install writes OriginFile into the staged folder and renames it to
// <root>/<name>, creating root if missing. ErrExists when that folder
// exists, in any form. The rename's error is returned as is: a root on
// another filesystem than the stage fails with EXDEV, never a copy. The
// stage's parent is removed afterwards. Returns the installed folder.
func Install(stage, root string, o Installed) (dir string, err error) {
	parent := filepath.Dir(stage)
	if !strings.HasPrefix(filepath.Base(parent), stagePrefix) {
		return "", fmt.Errorf("skill: install %s: not a staged folder", stage)
	}
	fail := func(err error) (string, error) { return "", fmt.Errorf("skill: install %s: %w", stage, err) }
	if err := hasSkillFile(stage); err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fail(err)
	}
	target := filepath.Join(root, filepath.Base(stage))
	if _, err := os.Lstat(target); err == nil {
		return "", fmt.Errorf("skill: install %s: %w", target, ErrExists)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fail(err)
	}
	b, err := toml.Marshal(o)
	if err != nil {
		return fail(err)
	}
	origin := filepath.Join(stage, filepath.FromSlash(OriginFile))
	if err := os.MkdirAll(filepath.Dir(origin), 0o755); err != nil {
		return fail(err)
	}
	tmp, err := fsutil.WriteTemp(filepath.Dir(origin), ".origin-*.toml", b, 0o644)
	if err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp, origin); err != nil {
		_ = os.Remove(tmp)
		return fail(err)
	}
	// rename(2) replaces an empty directory: the Lstat above is the check,
	// this catches a folder that appeared since.
	if err := os.Rename(stage, target); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("skill: install %s: %w", target, ErrExists)
		}
		return fail(err)
	}
	_ = RemoveStage(stage)
	return target, nil
}

// ErrCollection is an install aimed at a collection without `all`, or `all`
// together with a name (a 400).
var ErrCollection = Problem{Code: CodeCollection}

// Candidates lists the skill folders of a collection: a folder that has no
// SKILL.md itself, whose direct children with a SKILL.md are skills, or,
// when none of them has one, the children of its `skills/` folder (the
// layout most skill repositories use). Returned as sorted relative slash
// paths (`release`, `skills/release`); symlinked and hidden children are
// skipped. Empty when dir holds nothing installable; an error when dir
// itself is a skill (has SKILL.md) or cannot be read.
func Candidates(dir string) ([]string, error) {
	st, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSource, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a directory", ErrBadSource, dir)
	}
	if hasSkillFile(dir) == nil {
		return nil, fmt.Errorf("%w: %s is a skill, not a collection", ErrBadSource, dir)
	}
	out, err := skillChildren(dir, "")
	if err != nil || len(out) > 0 {
		return out, err
	}
	sub := filepath.Join(dir, "skills")
	if st, err := os.Lstat(sub); err != nil || !st.IsDir() {
		return out, nil
	}
	return skillChildren(sub, "skills/")
}

// skillChildren lists, prefixed, the names of the direct children of dir
// that are visible directories (not symlinks) holding a regular SKILL.md,
// in name order (ReadDir's).
func skillChildren(dir, prefix string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSource, err)
	}
	out := []string{}
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || hasSkillFile(filepath.Join(dir, e.Name())) != nil {
			continue
		}
		out = append(out, prefix+e.Name())
	}
	return out, nil
}
