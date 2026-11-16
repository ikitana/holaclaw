package skill

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// GitSource is a git repository, pinned to a ref or at its default branch,
// and the folder in it that is a skill or a collection.
type GitSource struct {
	URL  string // https, ssh, git or file; ParseGitURL reads it
	Ref  string // a tag, branch or commit, "" for the default branch
	Path string // the folder inside the checkout, slash separated, "" for the root
}

var (
	// scpRE matches the start of an scp-like git url (`git@host:`).
	scpRE = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:`)
	// refRE is what a ref may hold: enough for tags, branches and commits,
	// nothing git reads as a refspec.
	refRE      = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)
	gitSchemes = map[string]bool{"https": true, "ssh": true, "git": true, "file": true}
)

// ParseGitURL reads a git url with net/url and returns it in canonical form.
// An scp-like `user@host:path` is read as `ssh://user@host/path` but
// returned as given: git sends its path relative to the ssh user's home,
// which no ssh:// form says for every host. The scheme is https, ssh, git
// or file, a host is required except for file, and a query, a fragment, a
// password, a space or a control character is refused. Anything else is
// ErrBadSource.
func ParseGitURL(s string) (string, error) {
	bad := func(why string) (string, error) { return "", fmt.Errorf("%w: %s: %q", ErrBadSource, why, s) }
	if s == "" || strings.HasPrefix(s, "-") || strings.ContainsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return bad("not a git url")
	}
	raw, scp := s, !strings.Contains(s, "://") && scpRE.MatchString(s)
	if scp {
		host, p, _ := strings.Cut(s, ":")
		raw = "ssh://" + host + "/" + strings.TrimPrefix(p, "/")
	}
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return bad("not a git url")
	case !gitSchemes[u.Scheme]:
		return bad("the scheme is not https, ssh, git or file")
	case u.Opaque != "" || (u.Host == "" && u.Scheme != "file"):
		return bad("no host")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#"):
		return bad("a query or fragment")
	case strings.Trim(u.Path, "/") == "":
		return bad("no path")
	}
	if _, ok := u.User.Password(); ok {
		return bad("a password")
	}
	if scp {
		return s, nil
	}
	return u.String(), nil
}

// IsGitURL reports whether s is meant as a git url, not a local path: it
// has a scheme or the scp form, and ParseGitURL takes it.
func IsGitURL(s string) bool {
	if !strings.Contains(s, "://") && !scpRE.MatchString(s) {
		return false
	}
	_, err := ParseGitURL(s)
	return err == nil
}

// Validate returns g in canonical form: the url through ParseGitURL, no ref
// or one refRE takes that does not start with `-`, and a path that stays
// inside the checkout (filepath.IsLocal after path.Clean; "." is the root).
// Anything else is ErrBadSource.
func (g GitSource) Validate() (GitSource, error) {
	u, err := ParseGitURL(g.URL)
	if err != nil {
		return GitSource{}, err
	}
	if g.Ref != "" && (!refRE.MatchString(g.Ref) || strings.HasPrefix(g.Ref, "-")) {
		return GitSource{}, fmt.Errorf("%w: bad ref %q", ErrBadSource, g.Ref)
	}
	p := g.Path
	if p != "" {
		if p = path.Clean(p); !filepath.IsLocal(p) || strings.ContainsRune(p, '\\') {
			return GitSource{}, fmt.Errorf("%w: bad path %q", ErrBadSource, g.Path)
		}
		if p == "." {
			p = ""
		}
	}
	return GitSource{URL: u, Ref: g.Ref, Path: p}, nil
}

// fetchTimeout bounds a whole fetch, checkout included.
var fetchTimeout = 2 * time.Minute

// Fetched is a git source on disk: Dir is the folder to stage from, Commit
// the commit the ref resolved to, and Close removes the checkout.
type Fetched struct {
	Dir    string
	Commit string
	Close  func()
}

// Fetch validates g and checks it out under a fresh <tmpRoot>/skill-git-*
// within two minutes: a shallow fetch of the one ref, or of the remote's
// HEAD (its default branch) without one (`git init`, `git fetch --depth 1
// --end-of-options <url> <ref|HEAD>`, `git checkout FETCH_HEAD`)
// with terminal prompts off and hooks, fsmonitor and LFS filters disabled.
// Dir is g.Path inside the checkout, checked to stay inside it with no
// symlink on the way and to be a directory. Whether Dir is a skill (it
// holds SKILL.md) or a collection (Candidates) is the caller's to decide.
// The `git` binary is taken from the PATH; without it, or when the fetch
// fails or times out, ErrBadSource with git's last stderr line.
func Fetch(ctx context.Context, tmpRoot string, g GitSource) (Fetched, error) {
	g, err := g.Validate()
	if err != nil {
		return Fetched{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	if err := os.MkdirAll(tmpRoot, 0o700); err != nil {
		return Fetched{}, err
	}
	tmp, err := os.MkdirTemp(tmpRoot, tmpPrefix+"git-*")
	if err != nil {
		return Fetched{}, err
	}
	f, err := fetchGit(ctx, g, tmp)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return Fetched{}, err
	}
	return f, nil
}

// fetchGit checks g out into <tmp>/<repository name>, so a skill at the
// root of its repository is named after the repository.
func fetchGit(ctx context.Context, g GitSource, tmp string) (Fetched, error) {
	co := filepath.Join(tmp, repoName(g.URL))
	if err := os.Mkdir(co, 0o755); err != nil {
		return Fetched{}, err
	}
	steps := [][]string{
		{"init", "-q"},
		{"-c", "protocol.file.allow=always", "fetch", "-q", "--depth", "1", "--no-tags", "--end-of-options", g.URL, cmp.Or(g.Ref, "HEAD")},
		{"checkout", "-q", "--detach", "FETCH_HEAD"},
	}
	for _, args := range steps {
		if _, err := runGit(ctx, co, args...); err != nil {
			return Fetched{}, err
		}
	}
	commit, err := runGit(ctx, co, "rev-parse", "HEAD")
	if err != nil {
		return Fetched{}, err
	}
	dir := filepath.Join(co, filepath.FromSlash(g.Path))
	realCo, err := filepath.EvalSymlinks(co)
	if err != nil {
		return Fetched{}, err
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return Fetched{}, fmt.Errorf("%w: no %s in the repository", ErrBadSource, g.Path)
	}
	if real != filepath.Join(realCo, filepath.FromSlash(g.Path)) {
		return Fetched{}, fmt.Errorf("%w: %s goes through a symlink", ErrBadSource, g.Path)
	}
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return Fetched{}, fmt.Errorf("%w: %s is not a directory", ErrBadSource, g.Path)
	}
	return Fetched{Dir: dir, Commit: strings.TrimSpace(commit), Close: func() { _ = os.RemoveAll(tmp) }}, nil
}

// repoName is the last element of a git url's path without ".git", or
// "skill" when there is none.
func repoName(u string) string {
	u = strings.TrimSuffix(strings.TrimRight(u, "/"), ".git")
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	if u == "" || u == "." || u == ".." || strings.ContainsRune(u, '\\') {
		return "skill"
	}
	return u
}

// gitConfig turns off what a repository's content or the person's config
// could make git run or fetch during a checkout: hooks, an fsmonitor
// daemon, and the LFS filters.
var gitConfig = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "filter.lfs.smudge=",
	"-c", "filter.lfs.process=",
	"-c", "filter.lfs.required=false",
}

// runGit runs git with gitConfig in dir with a minimal environment: no
// terminal prompt, no askpass, no system config, the four transports only.
// The url is one argv element, never a shell word. A failure is
// ErrBadSource with git's last stderr line, or with the context's error
// when it ended.
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append(append([]string{}, gitConfig...), args...)...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_ALLOW_PROTOCOL=https:ssh:git:file",
		// ssh may use the daemon's agent, and never waits on a prompt.
		"SSH_AUTH_SOCK=" + os.Getenv("SSH_AUTH_SOCK"),
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes",
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		verb := args[0]
		for i := 0; i+2 < len(args) && args[i] == "-c"; i += 2 {
			verb = args[i+2]
		}
		switch {
		case errors.Is(err, exec.ErrNotFound):
			return "", fmt.Errorf("%w: git is not installed", ErrBadSource)
		case ctx.Err() != nil:
			return "", fmt.Errorf("%w: git %s: %w", ErrBadSource, verb, ctx.Err())
		}
		line := err.Error()
		for l := range strings.SplitSeq(stderr.String(), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				line = l
			}
		}
		return "", fmt.Errorf("%w: git %s: %s", ErrBadSource, verb, line)
	}
	return stdout.String(), nil
}
