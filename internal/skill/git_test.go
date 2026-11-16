package skill

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseGitURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://h/acme/skills.git":     "https://h/acme/skills.git",
		"https://user@h:8443/x":         "https://user@h:8443/x",
		"ssh://git@h/x":                 "ssh://git@h/x",
		"git://h/x":                     "git://h/x",
		"file:///srv/r.git":             "file:///srv/r.git",
		"git@h:acme/skills.git":         "git@h:acme/skills.git",
		"git@h:/srv/skills":             "git@h:/srv/skills",
		"deploy-bot@h.example:team/r":   "deploy-bot@h.example:team/r",
		"HTTPS://h/x":                   "https://h/x",
		"https://h/x/with%20escape.git": "https://h/x/with%20escape.git",
	} {
		if got, err := ParseGitURL(in); err != nil || got != want {
			t.Errorf("ParseGitURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{
		"", "skills/deploy", "./deploy", "/srv/skills", "h:x",
		"http://h/x", "ext::sh -c x", "ftp://h/x", "https:x",
		"https:///x", "ssh:///x", "https://h", "https://h/", "git@h:",
		"https://h/x?ref=v1", "https://h/x?", "https://h/x#v1", "git@h:x#y",
		"https://user:secret@h/x",
		"https://h/x y", "https://h/x\ty", "https://h/x" + string(rune(1)), "https://h/x" + string(rune(0x7f)),
		"-oProxyCommand=x@h:p", "--upload-pack=x",
	} {
		if got, err := ParseGitURL(in); !errors.Is(err, ErrBadSource) {
			t.Errorf("ParseGitURL(%q) = %q, %v; want ErrBadSource", in, got, err)
		}
	}
}

func TestIsGitURL(t *testing.T) {
	for s, want := range map[string]bool{
		"https://h/acme/skills": true,
		"ssh://git@h/x":         true,
		"git@h:acme/skills":     true,
		"file:///srv/r.git":     true,
		"https://h":             false, // looks like one, but ParseGitURL refuses it
		"/srv/skills/deploy":    false,
		"deploy":                false,
		"./a@b:c":               false,
		"dir/user@host:x":       false,
		"SKILL.md":              false,
	} {
		if got := IsGitURL(s); got != want {
			t.Errorf("IsGitURL(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestGitSourceValidate(t *testing.T) {
	for _, c := range []struct{ in, want GitSource }{
		{GitSource{URL: "git@h:acme/skills", Ref: "v1"}, GitSource{URL: "git@h:acme/skills", Ref: "v1"}},
		{GitSource{URL: "https://h/x", Ref: "release/v1.2", Path: "skills/release/"}, GitSource{URL: "https://h/x", Ref: "release/v1.2", Path: "skills/release"}},
		{GitSource{URL: "https://h/x", Ref: "abc123", Path: "a/../b"}, GitSource{URL: "https://h/x", Ref: "abc123", Path: "b"}},
		{GitSource{URL: "https://h/x", Ref: "main", Path: "."}, GitSource{URL: "https://h/x", Ref: "main"}},
		{GitSource{URL: "https://h/x"}, GitSource{URL: "https://h/x"}},
		{GitSource{URL: "https://h/x/", Path: "skills/"}, GitSource{URL: "https://h/x/", Path: "skills"}},
	} {
		if got, err := c.in.Validate(); err != nil || got != c.want {
			t.Errorf("Validate(%+v) = %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
	for _, g := range []GitSource{
		{URL: "https://h/x", Ref: "-upload-pack=x"},
		{URL: "https://h/x", Ref: "a:b"},
		{URL: "https://h/x", Ref: "a b"},
		{URL: "https://h/x", Ref: "v1", Path: ".."},
		{URL: "https://h/x", Ref: "v1", Path: "a/../../b"},
		{URL: "https://h/x", Ref: "v1", Path: "/etc"},
		{URL: `https://h/x`, Ref: "v1", Path: `a\b`},
		{URL: "http://h/x", Ref: "v1"},
		{Ref: "v1"},
		{},
	} {
		if got, err := g.Validate(); !errors.Is(err, ErrBadSource) {
			t.Errorf("Validate(%+v) = %+v, %v; want ErrBadSource", g, got, err)
		}
	}
}

// git runs git in dir for a test fixture, away from the person's config.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitRepo is a repository <tmp>/skills with a root SKILL.md, release/SKILL.md,
// escape (a symlink to a skill folder outside) and a tag v1; it returns the
// directory and the commit.
func gitRepo(t *testing.T) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := filepath.Join(t.TempDir(), "skills")
	put(t, dir, "SKILL.md", skillMD("skills", "root\n"))
	put(t, dir, "release/SKILL.md", skillMD("release", "release\n"))
	outside := srcSkill(t, "outside")
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	git(t, dir, "config", "uploadpack.allowAnySHA1InWant", "true")
	git(t, dir, "add", ".")
	git(t, dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "one")
	git(t, dir, "tag", "v1")
	return dir, git(t, dir, "rev-parse", "HEAD")
}

// TestFetchDefaultBranch fetches the remote's HEAD without a ref: its
// default branch, here trunk, not main and not another branch.
func TestFetchDefaultBranch(t *testing.T) {
	repo, _ := gitRepo(t)
	git(t, repo, "branch", "-m", "trunk")
	git(t, repo, "checkout", "-q", "-b", "other")
	put(t, repo, "SKILL.md", skillMD("skills", "other\n"))
	git(t, repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-am", "other")
	git(t, repo, "checkout", "-q", "trunk")
	put(t, repo, "SKILL.md", skillMD("skills", "trunk\n"))
	git(t, repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-am", "trunk")
	trunk := git(t, repo, "rev-parse", "HEAD")

	tmpRoot := t.TempDir()
	f, err := Fetch(context.Background(), tmpRoot, GitSource{URL: "file://" + repo, Path: "release"})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if f.Commit != trunk || filepath.Base(f.Dir) != "release" {
		t.Errorf("fetched %+v, want trunk's commit %s", f, trunk)
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(f.Dir), "SKILL.md"))
	if err != nil || !strings.Contains(string(b), "trunk") {
		t.Errorf("root SKILL.md = %q, %v; want trunk's", b, err)
	}
}

// tmpEntries lists the skill-* entries of a tmp root.
func tmpEntries(t *testing.T, tmpRoot string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(tmpRoot, tmpPrefix+"*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFetch(t *testing.T) {
	ctx := context.Background()
	repo, commit := gitRepo(t)
	tmpRoot := filepath.Join(t.TempDir(), "tmp")
	url := "file://" + repo

	f, err := Fetch(ctx, tmpRoot, GitSource{URL: url, Ref: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(f.Dir) != "skills" || f.Commit != commit {
		t.Errorf("root = %+v, want dir skills and commit %s", f, commit)
	}
	if !strings.HasPrefix(f.Dir, filepath.Join(tmpRoot, tmpPrefix+"git-")) {
		t.Errorf("checkout %s is not under %s", f.Dir, tmpRoot)
	}
	if _, err := os.Stat(filepath.Join(f.Dir, "SKILL.md")); err != nil {
		t.Error(err)
	}
	f.Close()
	if left := tmpEntries(t, tmpRoot); len(left) != 0 {
		t.Errorf("checkout left after Close: %v", left)
	}

	f, err = Fetch(ctx, tmpRoot, GitSource{URL: url, Ref: "v1", Path: "release"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(f.Dir, "SKILL.md"))
	if err != nil || !strings.Contains(string(b), "name: release") || filepath.Base(f.Dir) != "release" {
		t.Errorf("path = %s, %q, %v", f.Dir, b, err)
	}
	f.Close()

	f, err = Fetch(ctx, tmpRoot, GitSource{URL: url, Ref: commit})
	if err != nil || f.Commit != commit {
		t.Fatalf("by commit = %+v, %v", f, err)
	}
	f.Close()

	for _, c := range []struct {
		g    GitSource
		want string
	}{
		{GitSource{URL: url, Ref: "nope"}, "git fetch: "},
		{GitSource{URL: url, Ref: "v1", Path: "missing"}, "no missing"},
		{GitSource{URL: url, Ref: "v1", Path: "escape"}, "symlink"},
		{GitSource{URL: url, Ref: "v1", Path: "release/SKILL.md"}, "not a directory"},
		{GitSource{URL: "file:///nonexistent/repo.git", Ref: "v1"}, "git fetch: "},
		{GitSource{URL: url, Ref: "-v1"}, "bad ref"},
		{GitSource{URL: url, Ref: "v1", Path: "../x"}, "bad path"},
	} {
		if f, err := Fetch(ctx, tmpRoot, c.g); !errors.Is(err, ErrBadSource) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Fetch(%+v) = %+v, %v; want ErrBadSource with %q", c.g, f, err, c.want)
		}
	}
	if left := tmpEntries(t, tmpRoot); len(left) != 0 {
		t.Errorf("checkouts left: %v", left)
	}
}

// Hooks from the person's config never run during a fetch.
func TestFetchNoHooks(t *testing.T) {
	repo, _ := gitRepo(t)
	home := t.TempDir()
	hooks := filepath.Join(home, "hooks")
	marker := filepath.Join(home, "ran")
	put(t, hooks, "post-checkout", "#!/bin/sh\ntouch "+marker+"\n")
	if err := os.Chmod(filepath.Join(hooks, "post-checkout"), 0o755); err != nil {
		t.Fatal(err)
	}
	put(t, home, ".gitconfig", "[core]\n\thooksPath = "+hooks+"\n")
	t.Setenv("HOME", home)
	f, err := Fetch(context.Background(), t.TempDir(), GitSource{URL: "file://" + repo, Ref: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a hook ran: %v", err)
	}
}

func TestFetchTimeout(t *testing.T) {
	repo, _ := gitRepo(t)
	defer func(d time.Duration) { fetchTimeout = d }(fetchTimeout)
	fetchTimeout = time.Nanosecond
	tmpRoot := t.TempDir()
	_, err := Fetch(context.Background(), tmpRoot, GitSource{URL: "file://" + repo, Ref: "v1"})
	if !errors.Is(err, ErrBadSource) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if left := tmpEntries(t, tmpRoot); len(left) != 0 {
		t.Errorf("checkout left: %v", left)
	}
}

func TestFetchNoGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Fetch(context.Background(), t.TempDir(), GitSource{URL: "https://h/x", Ref: "v1"})
	if !errors.Is(err, ErrBadSource) || !strings.Contains(err.Error(), "git is not installed") {
		t.Fatalf("err = %v", err)
	}
}
