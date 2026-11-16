// Package chromium finds, launches and supervises the browser processes the
// sidecar owns (docs/browser.md): one Chromium process and one persistent
// profile per agent, visible by default on a virtual display the sidecar
// starts itself, never with `--no-sandbox`.
//
// Nothing here speaks CDP. Start hands out the two raw pipe ends Chromium's
// `--remote-debugging-pipe` uses (fd 3 in, fd 4 out); the protocol client is a
// separate package. Nothing is ever downloaded: the deployer installs the
// browser and the distro patches it.
package chromium

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// MinMajor is the oldest major version we accept. 136 is the first Chromium
// whose `--headless=new` is the only headless mode and whose CDP surface
// matches what the sidecar uses; older majors are refused with a clear message
// rather than failing obscurely later.
const MinMajor = 136

// versionTimeout bounds the `<binary> --version` probe. A browser that cannot
// print its own version in five seconds is not going to render a page.
const versionTimeout = 5 * time.Second

// Kind is which browser a binary turned out to be. It is reported by `check`
// and recorded in the sidecar's health, never used to branch on behaviour.
type Kind string

const (
	KindChromium         Kind = "chromium"
	KindChromeForTesting Kind = "chrome-for-testing"
	KindChrome           Kind = "chrome"
)

// Binary is a resolved browser: an absolute path plus what `--version` said.
type Binary struct {
	Path    string
	Kind    Kind
	Version string // full version, e.g. "153.0.8010.47"
	Major   int
}

// TooOld reports whether the binary is below MinMajor.
func (b Binary) TooOld() bool { return b.Major < MinMajor }

// String is what `check` prints.
func (b Binary) String() string {
	return fmt.Sprintf("%s %s (%s)", b.Kind, b.Version, b.Path)
}

// ErrNotFound is returned when no browser is configured and none is on PATH.
var ErrNotFound = errors.New("chromium: no browser found on PATH (looked for chromium, chromium-browser, chrome-for-testing, google-chrome, google-chrome-stable)")

// candidates are searched in PATH order: Chromium first (what development and
// tests use), then Chrome for Testing, then Google Chrome. Plain `chrome` is
// deliberately not searched: on most systems it is a different program, and
// Chrome for Testing installs ship the `chrome-for-testing` name.
var candidates = []struct {
	name string
	kind Kind
}{
	{"chromium", KindChromium},
	{"chromium-browser", KindChromium},
	{"chrome-for-testing", KindChromeForTesting},
	{"google-chrome", KindChrome},
	{"google-chrome-stable", KindChrome},
}

// Find resolves the browser binary. A configured path is used exactly as
// given: if it does not exist or is not executable that is an error, never a
// silent fall back to something else. Otherwise PATH is searched in candidate
// order. Either way the version is probed, so a too-old browser is visible
// before anything tries to use it.
func Find(configured string) (Binary, error) {
	if configured != "" {
		if err := executable(configured); err != nil {
			return Binary{}, fmt.Errorf("chromium: configured browser %q: %w", configured, err)
		}
		return probe(configured, "")
	}
	for _, c := range candidates {
		path, err := exec.LookPath(c.name)
		if err != nil {
			continue
		}
		b, err := probe(path, c.kind)
		if err != nil {
			continue // a broken candidate must not hide a working one further down
		}
		return b, nil
	}
	return Binary{}, ErrNotFound
}

// executable checks that path is a regular file we may execute.
func executable(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return errors.New("is a directory")
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return errors.New("is not executable")
	}
	return nil
}

// probe runs `<path> --version` and parses the result. fallback is the kind
// implied by the name it was found under, used when the output does not say.
func probe(path string, fallback Kind) (Binary, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Binary{}, fmt.Errorf("chromium: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, abs, "--version")
	// Scrubbed: the probe must not read the service user's home or profile.
	cmd.Env = []string{"HOME=" + os.TempDir(), "PATH=" + filepath.Dir(abs)}
	out, err := cmd.Output()
	if err != nil {
		return Binary{}, fmt.Errorf("chromium: %s --version: %w", abs, err)
	}
	version, major, err := parseVersion(string(out))
	if err != nil {
		return Binary{}, fmt.Errorf("chromium: %s: %w", abs, err)
	}
	return Binary{Path: abs, Kind: kindOf(string(out), fallback), Version: version, Major: major}, nil
}

// kindOf reads the product name out of a `--version` line. "Google Chrome for
// Testing 153.…" must not be mistaken for Google Chrome, so that is checked
// first.
func kindOf(out string, fallback Kind) Kind {
	low := strings.ToLower(out)
	switch {
	case strings.Contains(low, "for testing"):
		return KindChromeForTesting
	case strings.Contains(low, "chromium"):
		return KindChromium
	case strings.Contains(low, "google chrome"):
		return KindChrome
	case fallback != "":
		return fallback
	default:
		return KindChromium
	}
}

// parseVersion pulls the first dotted number out of a `--version` line:
// "Chromium 153.0.8010.47", "Google Chrome 153.0.8010.47",
// "Google Chrome for Testing 153.0.8010.47". Anything without one is an error.
func parseVersion(out string) (string, int, error) {
	for _, f := range strings.Fields(out) {
		f = strings.Trim(f, "()")
		head, _, _ := strings.Cut(f, ".")
		major, err := strconv.Atoi(head)
		if err != nil || major <= 0 || !strings.Contains(f, ".") {
			continue
		}
		return f, major, nil
	}
	return "", 0, fmt.Errorf("cannot parse version from %q", strings.TrimSpace(out))
}
