package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"holaclaw/internal/fsutil"
)

// MaxVersions is how many earlier versions history/ keeps. Each version is a
// full copy of the skill folder (SKILL.md, references/, scripts/), so the
// cap is what bounds the disk one skill can take: most skills are a few KiB,
// but a folder a person brings can be megabytes, and an agent stuck saving in
// a loop would otherwise grow history without end. 256 keeps far more than
// anyone reverts through while keeping that worst case finite.
const MaxVersions = 256

// TrashDir is the hidden folder under an agent's skills root that holds
// forgotten skills, whole (SKILL.md and history/), until a person restores or
// removes them. The loader skips hidden directories, so nothing there is
// indexed.
const TrashDir = ".trash"

// Errors from Writer, matched with errors.Is. An empty Root is a wiring bug,
// not something a caller handles, so it stays a plain error.
var (
	ErrNotFound   = Problem{Code: CodeNotFound}   // no such live skill, trashed skill or version
	ErrExists     = Problem{Code: CodeExists}     // Restore over a live skill
	ErrBundled    = Problem{Code: CodeBundled}    // a write aimed at a bundled skill (callers)
	ErrBadName    = Problem{Code: CodeBadName}    // a name that fails ValidName
	ErrUnsafePath = Problem{Code: CodeUnsafePath} // a symlink under Root, a path that left it, or a version file that is not a regular file
	errNoRoot     = errors.New("skill: writer has no root")
)

// historyRE matches a version folder in history/: a version number.
var historyRE = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)

// Writer versions and writes the skills of one agent under its skills root
// (agents/<name>/skills), the only place under agents/ the daemon writes
// (docs/skills.md §1, §6). A bundled skill is never written: an agent skill
// saved under its name shadows it.
//
// Versions are numbered from 1: history/<N>/ is version N, a copy of the
// whole skill folder as it was (SKILL.md, references/, scripts/ and any other
// file; hidden entries such as .holaclaw/ are not part of a version), and the
// current folder is the version after the newest in history/, 1 when there
// is none. The number comes from history/, never from the file, so a hand
// edit keeps its number and a file without metadata.holaclaw.skill_version
// still has one.
type Writer struct {
	Root string
}

// Saved reports one write.
type Saved struct {
	Dir      string // absolute folder of the skill
	Replaced int    // version the replaced folder retired as, history/<Replaced>/; 0 for a new skill
	Version  int    // version of the file this write made current
	Versions int    // versions in history/ after the write
}

// Current reads and parses <Root>/<name>/SKILL.md as an agent skill. exists
// is false, with no error, when there is none. A file that is there but is a
// symlink, unreadable or does not parse gives exists true and the error: the
// caller must not replace what it cannot read, since its requires carry over
// (docs/skills.md §8).
func (w Writer) Current(name string) (sk Skill, exists bool, err error) {
	dir, err := w.skillDir(name)
	if err != nil {
		return Skill{}, false, fmt.Errorf("skill: read %s: %w", name, err)
	}
	cur := filepath.Join(dir, "SKILL.md")
	st, err := os.Lstat(cur)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Skill{}, false, nil
	case err != nil:
		return Skill{}, true, fmt.Errorf("skill: read %s: %w", cur, err)
	case st.Mode()&fs.ModeSymlink != 0:
		return Skill{}, true, Problem{Code: CodeUnsafePath, Dir: dir, Msg: "SKILL.md is a symlink"}
	}
	b, _, rerr := readSkillFile(dir)
	if sk, err = parseRead(LevelAgent, dir, b, rerr); err != nil {
		return Skill{}, true, err
	}
	return sk, true, nil
}

// Next returns the version the next Save of name makes current: one after
// the live version (LiveVersion) when there is a current file, the live
// version itself when there is none. The caller renders it into
// metadata.holaclaw.skill_version.
func (w Writer) Next(name string) (int, error) {
	dir, err := w.skillDir(name)
	if err != nil {
		return 0, fmt.Errorf("skill: next %s: %w", name, err)
	}
	n, err := LiveVersion(dir)
	if err != nil {
		return 0, fmt.Errorf("skill: next %s: %w", dir, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "SKILL.md")); err == nil {
		n++
	} else if !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("skill: next %s: %w", dir, err)
	}
	return n, nil
}

// Save writes content as <Root>/<name>/SKILL.md: the current version, if
// any, retires into history/<N>/ first under its version number whoever
// wrote it (see retire), so a hand edit or an installed version is never
// lost; the new file lands by temp file + rename in the same directory as
// version N+1, the rest of the folder carried over as it is, then history is
// trimmed to MaxVersions (lowest numbers first). Save
// does not parse content: the caller renders it with Render, with the
// version from Next. name must satisfy ValidName. The temp file is written
// before the current file moves, so a failed write leaves the current
// version in place.
func (w Writer) Save(name string, content []byte) (Saved, error) {
	dir, err := w.skillDir(name)
	if err != nil {
		return Saved{}, fmt.Errorf("skill: save %s: %w", name, err)
	}
	fail := func(err error) (Saved, error) { return Saved{}, fmt.Errorf("skill: save %s: %w", dir, err) }
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(err)
	}
	tmp, err := fsutil.WriteTemp(dir, ".SKILL-*.md", content, 0o644)
	if err != nil {
		return fail(err)
	}
	cur := filepath.Join(dir, "SKILL.md")
	replaced := 0
	if _, err := os.Lstat(cur); err == nil {
		if replaced, err = retire(dir); err != nil {
			_ = os.Remove(tmp)
			return fail(err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		_ = os.Remove(tmp)
		return fail(err)
	}
	version := replaced + 1
	if replaced == 0 {
		n, err := newest(filepath.Join(dir, "history"))
		if err != nil {
			_ = os.Remove(tmp)
			return fail(err)
		}
		version = n + 1
	}
	if err := os.Rename(tmp, cur); err != nil {
		_ = os.Remove(tmp)
		return fail(err)
	}
	n, err := trim(filepath.Join(dir, "history"))
	if err != nil {
		return fail(err)
	}
	return Saved{Dir: dir, Replaced: replaced, Version: version, Versions: n}, nil
}

// Forget moves the skill folder <Root>/<name> to <Root>/.trash/<name>,
// replacing an earlier trashed copy of the same name (that copy is removed
// first). Returns how many versions the folder held in history/ (the current
// SKILL.md not counted). ErrNotFound when there is no current SKILL.md.
func (w Writer) Forget(name string) (versions int, err error) {
	dir, err := w.skillDir(name)
	if err != nil {
		return 0, fmt.Errorf("skill: forget %s: %w", name, err)
	}
	fail := func(err error) (int, error) { return 0, fmt.Errorf("skill: forget %s: %w", dir, err) }
	if _, err := os.Lstat(filepath.Join(dir, "SKILL.md")); errors.Is(err, fs.ErrNotExist) {
		return 0, Problem{Code: CodeNotFound, Dir: dir, Msg: "no SKILL.md to forget"}
	} else if err != nil {
		return fail(err)
	}
	root := filepath.Dir(dir)
	if err := fsutil.NoSymlinks(root, TrashDir, name); err != nil {
		return 0, unsafePath(dir, err)
	}
	dst := filepath.Join(root, TrashDir, name)
	n, err := trim(filepath.Join(dir, "history"))
	if err != nil {
		return fail(err)
	}
	if err := os.RemoveAll(dst); err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fail(err)
	}
	if err := os.Rename(dir, dst); err != nil {
		return fail(err)
	}
	return n, nil
}

// skillDir validates name and returns the absolute skill folder. No element
// under Root that exists may be a symlink.
func (w Writer) skillDir(name string) (string, error) {
	if !ValidName(name) {
		return "", Problem{Code: CodeBadName, Msg: fmt.Sprintf("%q is not a skill name", name)}
	}
	if w.Root == "" {
		return "", errNoRoot
	}
	root, err := fsutil.Abs(w.Root)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, name)
	if rel, err := filepath.Rel(root, dir); err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", Problem{Code: CodeUnsafePath, Dir: dir, Msg: "outside the skills root"}
	}
	if err := fsutil.NoSymlinks(root, name, "history"); err != nil {
		return "", unsafePath(dir, err)
	}
	return dir, nil
}

// unsafePath returns a symlink fsutil.NoSymlinks found as an ErrUnsafePath
// Problem for the skill folder dir, and any other error as is.
func unsafePath(dir string, err error) error {
	if errors.Is(err, fsutil.ErrSymlink) {
		return Problem{Code: CodeUnsafePath, Dir: dir, Msg: err.Error()}
	}
	return err
}

// retire makes the current version of dir history/<N>/, N one after the
// newest version there, and returns N. The version is the whole folder: every
// entry but history/ and hidden ones (.holaclaw/, a write's temp file) is
// copied into a hidden folder under history/, SKILL.md is moved in last, and
// the folder is renamed to <N>, so history/ never holds half a version. The
// rest of the folder stays live. If the last rename fails, SKILL.md is moved
// back.
func retire(dir string) (int, error) {
	hist := filepath.Join(dir, "history")
	if err := os.MkdirAll(hist, 0o755); err != nil {
		return 0, err
	}
	n, err := newest(hist)
	if err != nil {
		return 0, err
	}
	n++
	stage, err := os.MkdirTemp(hist, ".retire-")
	if err != nil {
		return 0, err
	}
	// MkdirTemp made the folder CopyTree wants to create.
	if err := os.Remove(stage); err != nil {
		return 0, err
	}
	skip := func(rel string, _ fs.DirEntry) bool {
		return rel == "history" || rel == "SKILL.md" || strings.HasPrefix(path.Base(rel), ".")
	}
	if err := fsutil.CopyTree(dir, stage, fsutil.CopyOptions{Skip: skip}); err != nil {
		_ = os.RemoveAll(stage)
		return 0, err
	}
	cur, staged := filepath.Join(dir, "SKILL.md"), filepath.Join(stage, "SKILL.md")
	if err := os.Rename(cur, staged); err != nil {
		_ = os.RemoveAll(stage)
		return 0, err
	}
	if err := os.Rename(stage, filepath.Join(hist, versionDir(n))); err != nil {
		if rerr := os.Rename(staged, cur); rerr != nil {
			return 0, errors.Join(err, rerr) // SKILL.md stays in the hidden stage folder
		}
		_ = os.RemoveAll(stage)
		return 0, err
	}
	return n, nil
}

// versionDir is the history/ folder name of version n.
func versionDir(n int) string { return strconv.Itoa(n) }

// versions lists the version numbers in hist, lowest first. Only folders
// named <N> count; a missing hist holds none.
func versions(hist string) ([]int, error) {
	entries, err := os.ReadDir(hist)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []int
	for _, e := range entries {
		if e.IsDir() && historyRE.MatchString(e.Name()) {
			n, _ := strconv.Atoi(e.Name())
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

// newest returns the highest version number in hist, 0 when there is none.
func newest(hist string) (int, error) {
	vs, err := versions(hist)
	if err != nil || len(vs) == 0 {
		return 0, err
	}
	return vs[len(vs)-1], nil
}

// trim removes the lowest-numbered version folders in hist beyond
// MaxVersions and returns how many remain. Entries that are not <N>/ are left
// alone and not counted; a missing hist holds none. The newest is never
// removed, so the current version keeps its number.
func trim(hist string) (int, error) {
	vs, err := versions(hist)
	if err != nil {
		return 0, err
	}
	for len(vs) > MaxVersions {
		if err := os.RemoveAll(filepath.Join(hist, versionDir(vs[0]))); err != nil {
			return len(vs), err
		}
		vs = vs[1:]
	}
	return len(vs), nil
}

// Restore moves <Root>/.trash/<name> back to <Root>/<name>, whole, history
// included. ErrNotFound when nothing is in the trash under that name;
// ErrExists when the live folder is already there (it is never overwritten).
func (w Writer) Restore(name string) error {
	dir, err := w.skillDir(name)
	if err != nil {
		return fmt.Errorf("skill: restore %s: %w", name, err)
	}
	src, err := w.trashed(name)
	if err != nil {
		return fmt.Errorf("skill: restore %s: %w", name, err)
	}
	fail := func(err error) error { return fmt.Errorf("skill: restore %s: %w", src, err) }
	if _, err := os.Lstat(dir); err == nil {
		return Problem{Code: CodeExists, Dir: dir, Msg: "a live skill has this name; restore never overwrites it"}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fail(err)
	}
	if err := os.Rename(src, dir); err != nil {
		return fail(err)
	}
	return nil
}

// Remove deletes <Root>/.trash/<name> for good. ErrNotFound when it is not in
// the trash. It never touches a live skill: that is Forget.
func (w Writer) Remove(name string) error {
	src, err := w.trashed(name)
	if err != nil {
		return fmt.Errorf("skill: remove %s: %w", name, err)
	}
	if err := os.RemoveAll(src); err != nil {
		return fmt.Errorf("skill: remove %s: %w", src, err)
	}
	return nil
}

// Reverted reports one revert.
type Reverted struct {
	From     int // version the current folder retired as, history/<From>/
	To       int // version whose folder was copied; it stays in history/
	Version  int // version of the copy, now current
	Versions int // versions in history/ after the revert
}

// Revert copies version to, the folder history/<to>/, into a new version:
// the current folder retires into history/ (From) through Save, to's
// SKILL.md becomes the next version, and the rest of the folder (references/,
// scripts/, any other file) is replaced by to's, while to itself stays in
// history/. Hidden entries such as .holaclaw/ are left as they are. The copy
// keeps its skill_version, which names the version it restores
// (docs/skills.md §6). ErrNotFound when the skill is not live or has no
// version to in history/, a number below 1 included.
// to is copied aside first, since Save trims history/ to MaxVersions and
// reverting to the oldest of a full history drops that folder. If replacing
// the rest of the folder fails partway, the previous version is already whole
// in history/ (From).
func (w Writer) Revert(name string, to int) (Reverted, error) {
	dir, err := w.skillDir(name)
	if err != nil {
		return Reverted{}, fmt.Errorf("skill: revert %s: %w", name, err)
	}
	fail := func(err error) (Reverted, error) { return Reverted{}, fmt.Errorf("skill: revert %s: %w", dir, err) }
	if err := live(dir); err != nil {
		return fail(err)
	}
	noVersion := Problem{Code: CodeNotFound, Dir: dir, Msg: fmt.Sprintf("no version %d", to)}
	if to < 1 {
		return Reverted{}, noVersion
	}
	vdir := filepath.Join(dir, "history", versionDir(to))
	if st, err := os.Lstat(vdir); errors.Is(err, fs.ErrNotExist) {
		return Reverted{}, noVersion
	} else if err != nil {
		return fail(err)
	} else if !st.IsDir() {
		return Reverted{}, Problem{Code: CodeUnsafePath, Dir: dir, Msg: "history/" + versionDir(to) + " is not a folder"}
	}
	if st, err := os.Lstat(filepath.Join(vdir, "SKILL.md")); errors.Is(err, fs.ErrNotExist) {
		return Reverted{}, noVersion
	} else if err != nil {
		return fail(err)
	} else if !st.Mode().IsRegular() {
		return Reverted{}, Problem{Code: CodeUnsafePath, Dir: dir, Msg: "history/" + versionDir(to) + "/SKILL.md is not a regular file"}
	}
	stage, err := os.MkdirTemp(dir, ".revert-")
	if err != nil {
		return fail(err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	copied := filepath.Join(stage, "v")
	if err := fsutil.CopyTree(vdir, copied, fsutil.CopyOptions{}); err != nil {
		return fail(err)
	}
	b, err := os.ReadFile(filepath.Join(copied, "SKILL.md"))
	if err != nil {
		return fail(err)
	}
	saved, err := w.Save(name, b)
	if err != nil {
		return Reverted{}, err
	}
	if err := replaceRest(dir, copied); err != nil {
		return fail(err)
	}
	return Reverted{From: saved.Replaced, To: to, Version: saved.Version, Versions: saved.Versions}, nil
}

// replaceRest makes the entries of dir other than SKILL.md, history/ and the
// hidden ones those of src: the live ones are removed, then src's are moved
// in. SKILL.md is the caller's.
func replaceRest(dir, src string) error {
	keep := func(name string) bool {
		return name == "SKILL.md" || name == "history" || strings.HasPrefix(name, ".")
	}
	live, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range live {
		if !keep(e.Name()) {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	old, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range old {
		if !keep(e.Name()) {
			if err := os.Rename(filepath.Join(src, e.Name()), filepath.Join(dir, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// Versions lists the version numbers in history/ of the live skill
// <Root>/<name>, newest first. ErrNotFound when the skill is not live.
func (w Writer) Versions(name string) ([]int, error) {
	dir, err := w.skillDir(name)
	if err != nil {
		return nil, fmt.Errorf("skill: versions %s: %w", name, err)
	}
	if err := live(dir); err != nil {
		return nil, fmt.Errorf("skill: versions %s: %w", dir, err)
	}
	return ListVersions(dir)
}

// ListVersions lists the version numbers in dir/history/ (folders <N>),
// newest first, never nil; a missing history/ holds none. Other entries and
// symlinks are not listed. It is Versions without the checks, for a folder
// the caller already has, such as a trashed skill's Dir.
func ListVersions(dir string) ([]int, error) {
	vs, err := versions(filepath.Join(dir, "history"))
	if err != nil {
		return nil, err
	}
	slices.Reverse(vs)
	if vs == nil {
		vs = []int{}
	}
	return vs, nil
}

// live fails with ErrNotFound unless dir/SKILL.md exists.
func live(dir string) error {
	if _, err := os.Lstat(filepath.Join(dir, "SKILL.md")); errors.Is(err, fs.ErrNotExist) {
		return Problem{Code: CodeNotFound, Dir: dir, Msg: "no live SKILL.md"}
	} else if err != nil {
		return err
	}
	return nil
}

// trashed validates name and returns the trashed folder <Root>/.trash/<name>,
// ErrNotFound when it does not exist. No element under Root may be a symlink.
func (w Writer) trashed(name string) (string, error) {
	dir, err := w.skillDir(name)
	if err != nil {
		return "", err
	}
	root := filepath.Dir(dir)
	if err := fsutil.NoSymlinks(root, TrashDir, name); err != nil {
		return "", unsafePath(dir, err)
	}
	p := filepath.Join(root, TrashDir, name)
	if st, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return "", Problem{Code: CodeNotFound, Dir: p, Msg: "not in the trash"}
	} else if err != nil {
		return "", err
	} else if !st.IsDir() {
		return "", Problem{Code: CodeNotFound, Dir: p, Msg: "not a folder"}
	}
	return p, nil
}
