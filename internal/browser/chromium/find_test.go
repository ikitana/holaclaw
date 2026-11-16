package chromium

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBin writes an executable shell script at dir/name. The script restores
// the test runner's PATH for itself: a launched browser gets a deliberately
// minimal one (see environ), and these stand-ins need ordinary tools.
func fakeBin(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\nexport PATH=" + os.Getenv("PATH") + "\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// versionBin writes a fake browser that only prints a version line.
func versionBin(t *testing.T, dir, name, line string) string {
	t.Helper()
	return fakeBin(t, dir, name, "echo '"+line+"'")
}

func TestParseVersion(t *testing.T) {
	cases := []struct {
		out   string
		want  string
		major int
		bad   bool
	}{
		{out: "Chromium 153.0.8010.47 \n", want: "153.0.8010.47", major: 153},
		{out: "Google Chrome 153.0.8010.47\n", want: "153.0.8010.47", major: 153},
		{out: "Google Chrome for Testing 140.0.7339.16\n", want: "140.0.7339.16", major: 140},
		{out: "Chromium 99.0.1.2 snap\n", want: "99.0.1.2", major: 99},
		{out: "Brave Browser 1.60.118 (Chromium 120.0.6099.71)\n", want: "1.60.118", major: 1},
		{out: "", bad: true},
		{out: "not a browser\n", bad: true},
		{out: "Chromium version unknown\n", bad: true},
		{out: "Chromium 0.0.0\n", bad: true}, // a zero major is garbage
		{out: "Chromium 153\n", bad: true},   // no dots: not a version
	}
	for _, c := range cases {
		got, major, err := parseVersion(c.out)
		if c.bad {
			if err == nil {
				t.Errorf("parseVersion(%q) = %q, want error", c.out, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseVersion(%q): %v", c.out, err)
			continue
		}
		if got != c.want || major != c.major {
			t.Errorf("parseVersion(%q) = %q/%d, want %q/%d", c.out, got, major, c.want, c.major)
		}
	}
}

func TestKindOf(t *testing.T) {
	cases := []struct {
		out  string
		want Kind
	}{
		{"Chromium 153.0.8010.47", KindChromium},
		{"Google Chrome 153.0.8010.47", KindChrome},
		{"Google Chrome for Testing 153.0.8010.47", KindChromeForTesting},
		{"Chromium for Testing 1.2.3.4", KindChromeForTesting},
		{"Something 1.2.3.4", KindChromium},
	}
	for _, c := range cases {
		if got := kindOf(c.out, ""); got != c.want {
			t.Errorf("kindOf(%q) = %q, want %q", c.out, got, c.want)
		}
	}
}

func TestFindOrder(t *testing.T) {
	dir := t.TempDir()
	versionBin(t, dir, "google-chrome", "Google Chrome 150.0.1.2")
	versionBin(t, dir, "chrome-for-testing", "Google Chrome for Testing 151.0.1.2")
	versionBin(t, dir, "chromium-browser", "Chromium 152.0.1.2")
	t.Setenv("PATH", dir)

	// Chrome for Testing beats Google Chrome, chromium-browser beats both.
	b, err := Find("")
	if err != nil {
		t.Fatal(err)
	}
	if b.Kind != KindChromium || b.Version != "152.0.1.2" {
		t.Fatalf("got %+v, want chromium 152.0.1.2", b)
	}

	// With Chromium gone, Chrome for Testing is next.
	if err := os.Remove(filepath.Join(dir, "chromium-browser")); err != nil {
		t.Fatal(err)
	}
	if b, err = Find(""); err != nil || b.Kind != KindChromeForTesting {
		t.Fatalf("got %+v, %v; want chrome-for-testing", b, err)
	}

	// Then Google Chrome.
	if err := os.Remove(filepath.Join(dir, "chrome-for-testing")); err != nil {
		t.Fatal(err)
	}
	if b, err = Find(""); err != nil || b.Kind != KindChrome {
		t.Fatalf("got %+v, %v; want chrome", b, err)
	}

	// `chromium` wins over everything when present.
	versionBin(t, dir, "chromium", "Chromium 153.0.8010.47")
	if b, err = Find(""); err != nil || b.Kind != KindChromium || b.Major != 153 {
		t.Fatalf("got %+v, %v; want chromium 153", b, err)
	}
}

func TestFindNone(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := Find(""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

// A candidate that cannot report a version must not hide a working one below.
func TestFindSkipsBroken(t *testing.T) {
	dir := t.TempDir()
	fakeBin(t, dir, "chromium", "echo broken; exit 1")
	versionBin(t, dir, "google-chrome", "Google Chrome 153.0.1.2")
	t.Setenv("PATH", dir)
	b, err := Find("")
	if err != nil || b.Kind != KindChrome {
		t.Fatalf("got %+v, %v; want chrome", b, err)
	}
}

func TestFindConfigured(t *testing.T) {
	dir := t.TempDir()
	good := versionBin(t, dir, "my-browser", "Chromium 153.0.8010.47")
	// A configured path is used exactly, even with better candidates on PATH.
	versionBin(t, dir, "chromium", "Chromium 99.0.0.1")
	t.Setenv("PATH", dir)

	b, err := Find(good)
	if err != nil {
		t.Fatal(err)
	}
	if b.Path != good || b.Version != "153.0.8010.47" {
		t.Fatalf("got %+v, want %s", b, good)
	}

	// Missing, not executable, a directory: each a clear error, never a
	// silent fall back to the one on PATH.
	notThere := filepath.Join(dir, "nope")
	noExec := filepath.Join(dir, "plain")
	if err := os.WriteFile(noExec, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{notThere, noExec, dir} {
		if _, err := Find(path); err == nil {
			t.Fatalf("Find(%q) succeeded, want error", path)
		} else if !strings.Contains(err.Error(), "configured browser") {
			t.Fatalf("Find(%q): %v, want a configured-browser error", path, err)
		}
	}
}

func TestTooOld(t *testing.T) {
	if !(Binary{Major: MinMajor - 1}).TooOld() {
		t.Error("MinMajor-1 should be too old")
	}
	if (Binary{Major: MinMajor}).TooOld() {
		t.Error("MinMajor should be accepted")
	}
	if !(Binary{}).TooOld() {
		t.Error("an unprobed binary should count as too old")
	}
	b := Binary{Path: "/x/chromium", Kind: KindChromium, Version: "153.0.1.2", Major: 153}
	if got := b.String(); !strings.Contains(got, "chromium") || !strings.Contains(got, "153.0.1.2") {
		t.Errorf("String() = %q", got)
	}
}
