package skill

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"

	"holaclaw/internal/fsutil"
)

// bundledFS holds the skills that ship with holaclaw. Empty today; a skill
// dropped into bundled/<name>/ is picked up without touching the loader
// (docs/skills.md §1).
//
//go:embed all:bundled
var bundledFS embed.FS

// Bundled is the embedded bundled root, for Roots.Bundled.
var Bundled fs.FS = mustSub(bundledFS, "bundled")

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// Roots is where one agent's skills live. Bundled may be nil; a missing
// Agent directory holds no skills and is not a problem.
type Roots struct {
	Bundled fs.FS
	Agent   string // agents/<name>/skills
}

// Load walks the two roots once (no watcher) and returns every skill that
// parses plus a Problem for each that does not. A skill folder is a direct
// child of its root holding SKILL.md. The walk does not descend into a skill
// folder (so history/ and references/ are never read as skills), skips hidden
// directories (so .trash/ is never indexed) and does not follow symlinked
// directories. Each loaded skill gets its References and Scripts listed. An
// agent root that cannot be resolved or read is one Problem. Problems are
// sorted by directory.
func Load(r Roots) (Set, []Problem) {
	var skills []Skill
	var probs []Problem
	add := func(sk Skill, err error, dir string) {
		if err != nil {
			probs = append(probs, asProblem(err, dir))
			return
		}
		skills = append(skills, sk)
	}

	if r.Bundled != nil {
		entries, _ := fs.ReadDir(r.Bundled, ".")
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			b, err := fs.ReadFile(r.Bundled, path.Join(e.Name(), "SKILL.md"))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				add(Skill{}, err, path.Join("bundled", e.Name()))
				continue
			}
			sk, err := Parse(LevelBundled, e.Name(), b)
			sk.fsys = r.Bundled
			if err == nil {
				sk.References, sk.Scripts = listFiles(r.Bundled, e.Name())
			}
			add(sk, err, path.Join("bundled", e.Name()))
		}
	}
	if r.Agent != "" {
		root, names, err := agentDirs(r.Agent)
		if err != nil {
			probs = append(probs, Problem{Code: CodeUnreadable, Dir: root, Msg: err.Error()})
		}
		for _, name := range names {
			dir := filepath.Join(root, name)
			if b, ok, err := readSkillFile(dir); ok {
				sk, perr := parseRead(LevelAgent, dir, b, err)
				add(sk, perr, dir)
			}
		}
	}

	sort.SliceStable(probs, func(i, j int) bool {
		if probs[i].Dir != probs[j].Dir {
			return probs[i].Dir < probs[j].Dir
		}
		return probs[i].Code < probs[j].Code
	})
	return newSet(skills), probs
}

func parseRead(level Level, dir string, b []byte, readErr error) (Skill, error) {
	if readErr != nil {
		return Skill{}, readErr
	}
	sk, err := Parse(level, dir, b)
	if err == nil {
		sk.References, sk.Scripts = listFiles(os.DirFS(dir), ".")
	}
	return sk, err
}

// agentDirs resolves the agent root and lists its skill folders: the real
// subdirectories, hidden ones skipped. A missing root holds none. root is
// what a Problem names: the absolute root, or agent as given when it cannot
// be resolved.
func agentDirs(agent string) (root string, names []string, err error) {
	root, err = fsutil.Abs(agent)
	if err != nil {
		return agent, nil, err
	}
	names, err = fsutil.Subdirs(root, fsutil.Options{SkipHidden: true})
	if errors.Is(err, fs.ErrNotExist) {
		return root, nil, nil
	}
	return root, names, err
}

// readSkillFile reads dir/SKILL.md, capped at maxFile bytes so one oversized
// file cannot fill the context of every session that indexes it; detail
// belongs in references/, which is read only on demand. ok is false when
// there is none.
func readSkillFile(dir string) (b []byte, ok bool, err error) {
	f, err := os.Open(filepath.Join(dir, "SKILL.md"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, Problem{Code: CodeUnreadable, Dir: dir, Msg: err.Error()}
	}
	defer func() { _ = f.Close() }()
	b, err = io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, true, Problem{Code: CodeUnreadable, Dir: dir, Msg: err.Error()}
	}
	if len(b) > maxFile {
		return nil, true, Problem{Code: CodeTooLong, Dir: dir, Msg: fmt.Sprintf("SKILL.md is over %d bytes; move detail into references/", maxFile)}
	}
	return b, true, nil
}

// listFiles lists the regular files under references/ and scripts/ of the
// skill folder dir inside fsys (see Skill.References): hidden entries
// skipped, at most MaxListed each; a listing that fails is empty.
func listFiles(fsys fs.FS, dir string) (refs, scripts []string) {
	o := fsutil.Options{SkipHidden: true, Max: MaxListed}
	refs, _ = fsutil.Files(fsys, path.Join(dir, "references"), o)
	scripts, _ = fsutil.Files(fsys, path.Join(dir, "scripts"), o)
	return refs, scripts
}

// Trashed walks <root>/.trash/ the way Load walks the agent root, one level,
// and returns the forgotten skills in it, each an agent skill with its Dir
// under the trash, References and Scripts listed, plus a Problem for each
// folder that does not parse. Load never enters the trash: it is a hidden
// directory. A missing .trash/ holds nothing; one that cannot be read is a
// Problem.
func Trashed(root string) ([]Skill, []Problem) {
	var skills []Skill
	var probs []Problem
	root, names, err := agentDirs(filepath.Join(root, TrashDir))
	if err != nil {
		probs = append(probs, Problem{Code: CodeUnreadable, Dir: root, Msg: err.Error()})
	}
	trash := root
	for _, name := range names {
		dir := filepath.Join(trash, name)
		b, ok, err := readSkillFile(dir)
		if !ok {
			continue
		}
		sk, err := parseRead(LevelAgent, dir, b, err)
		if err != nil {
			probs = append(probs, asProblem(err, dir))
			continue
		}
		skills = append(skills, sk)
	}
	return skills, probs
}
