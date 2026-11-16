package skill

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// LiveVersion returns the number of the agent skill in dir as it is now: one
// after the newest version in history/, 1 when there is none. It is the
// number Save gave the file and the one it retires as, whoever edited it
// since. dir may be a trashed skill's folder.
func LiveVersion(dir string) (int, error) {
	n, err := newest(filepath.Join(dir, "history"))
	if err != nil {
		return 0, err
	}
	return n + 1, nil
}

// ReadVersion parses version N of dir, the folder history/<N>/ (a number
// ListVersions returned), as the agent skill it was when it was current, and
// returns its SKILL.md too. References and Scripts are the version's own
// files, listed from its folder, not the live skill's. A number below 1 or
// one with no SKILL.md is ErrNotFound: the number comes from a request. A
// version that no longer parses is returned as the parse error, so a caller
// can still list it by name. Both are Problems: check ErrNotFound before
// taking a Problem for a parse error.
func ReadVersion(dir string, version int) (Skill, []byte, error) {
	noVersion := Problem{Code: CodeNotFound, Dir: dir, Msg: fmt.Sprintf("no version %d", version)}
	if version < 1 {
		return Skill{}, nil, noVersion
	}
	vdir := filepath.Join(dir, "history", versionDir(version))
	f, err := os.Open(filepath.Join(vdir, "SKILL.md"))
	if errors.Is(err, fs.ErrNotExist) {
		return Skill{}, nil, noVersion
	}
	if err != nil {
		return Skill{}, nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return Skill{}, nil, err
	}
	if len(b) > maxFile {
		return Skill{}, b[:maxFile], Problem{Code: CodeTooLong, Dir: dir, Msg: "version " + versionDir(version) + ": SKILL.md is over the size limit"}
	}
	sk, err := Parse(LevelAgent, dir, b)
	if err != nil {
		return Skill{}, b, err
	}
	sk.References, sk.Scripts = listFiles(os.DirFS(vdir), ".")
	return sk, b, nil
}

// File is the skill's SKILL.md as it is on disk (or in the bundled root),
// frontmatter included: what the explorer's raw view shows.
func (sk Skill) File() ([]byte, error) {
	if sk.Level == LevelBundled {
		if sk.fsys == nil {
			return nil, fmt.Errorf("skill %s: no bundled root: %w", sk.Name, fs.ErrNotExist)
		}
		return fs.ReadFile(sk.fsys, path.Join(sk.fsPath, "SKILL.md"))
	}
	b, _, err := readSkillFile(sk.Dir)
	if err == nil && b == nil {
		return nil, fmt.Errorf("skill %s: %w", sk.Name, fs.ErrNotExist)
	}
	return b, err
}
