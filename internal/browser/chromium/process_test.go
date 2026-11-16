package chromium

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// shortSettle keeps launch tests quick; the real second only matters for a
// browser that takes a moment to fail.
func shortSettle(t *testing.T) {
	t.Helper()
	old := startSettle
	startSettle = 200 * time.Millisecond
	t.Cleanup(func() { startSettle = old })
}

// testBinary writes a fake browser and describes it as a probed Binary.
func testBinary(t *testing.T, body string) Binary {
	t.Helper()
	path := fakeBin(t, t.TempDir(), "chromium", body)
	return Binary{Path: path, Kind: KindChromium, Version: "153.0.8010.47", Major: 153}
}

// sleeper stays up until it is signalled, like a browser would.
const sleeper = "while true; do sleep 0.05; done"

// newState makes a state directory and returns it with the profile path for
// one agent inside it.
func newState(t *testing.T) (string, string) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	return state, filepath.Join(state, "profiles", "main")
}

func TestStartAndStop(t *testing.T) {
	shortSettle(t)
	state, profile := newState(t)
	p, err := Start(t.Context(), Options{
		Binary: testBinary(t, sleeper), StateDir: state, ProfileDir: profile, Headless: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.PID <= 0 {
		t.Fatalf("pid %d", p.PID)
	}
	if fi, err := os.Stat(profile); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("profile: %v, %v", fi, err)
	}
	select {
	case err := <-p.Wait():
		t.Fatalf("exited early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	p.Stop(t.Context())
	if err := <-p.Wait(); err == nil {
		t.Error("want a non-nil exit error after a signal")
	}
	p.Stop(t.Context()) // idempotent
	if alive(p.PID) {
		t.Error("process still alive after Stop")
	}
}

// Stop must take the whole process group: a browser is a tree of renderers.
func TestStopKillsProcessGroup(t *testing.T) {
	shortSettle(t)
	state, profile := newState(t)
	childFile := filepath.Join(t.TempDir(), "child.pid")
	bin := testBinary(t, "sh -c 'while true; do sleep 0.05; done' &\n"+
		"echo $! > "+childFile+"\n"+sleeper)

	p, err := Start(t.Context(), Options{Binary: bin, StateDir: state, ProfileDir: profile, Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	var child int
	for range 100 {
		b, err := os.ReadFile(childFile)
		if err == nil && len(strings.TrimSpace(string(b))) > 0 {
			child, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("the fake browser never reported a child pid")
	}
	p.Stop(t.Context())
	for range 100 {
		if !alive(child) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child %d survived Stop", child)
}

// --no-sandbox is never used: a sandbox failure is a typed error with a fix.
func TestStartSandboxError(t *testing.T) {
	shortSettle(t)
	state, profile := newState(t)
	bin := testBinary(t, "echo 'Failed to move to new namespace: PID namespaces supported, "+
		"Network namespace supported, but failed: errno = Operation not permitted' >&2\nexit 1")
	_, err := Start(t.Context(), Options{Binary: bin, StateDir: state, ProfileDir: profile, Headless: true})
	var sandbox *ErrSandbox
	if !errors.As(err, &sandbox) {
		t.Fatalf("got %v, want *ErrSandbox", err)
	}
	for _, want := range []string{"unprivileged_userns_clone", "apparmor_restrict_unprivileged_userns", "--no-sandbox is never used"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q: %s", want, err)
		}
	}
	if !strings.Contains(sandbox.Stderr, "new namespace") {
		t.Errorf("stderr not captured: %q", sandbox.Stderr)
	}
}

// Any other early exit is a plain error, not a sandbox diagnosis.
func TestStartOtherEarlyExit(t *testing.T) {
	shortSettle(t)
	state, profile := newState(t)
	bin := testBinary(t, "echo 'something else went wrong' >&2\nexit 7")
	_, err := Start(t.Context(), Options{Binary: bin, StateDir: state, ProfileDir: profile, Headless: true})
	var sandbox *ErrSandbox
	if err == nil || errors.As(err, &sandbox) {
		t.Fatalf("got %v, want a plain error", err)
	}
}

func TestStartRefusesProfileOutsideState(t *testing.T) {
	shortSettle(t)
	root := t.TempDir()
	state := filepath.Join(root, "state")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{state, outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink inside the state directory pointing out of it.
	link := filepath.Join(state, "linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		filepath.Join(state, "..", "outside", "p"), // ../ escape
		outside,                  // absolute elsewhere
		filepath.Join(link, "p"), // through the symlink
		state,                    // the state directory itself
		"",                       // nothing at all
	}
	for _, profile := range bad {
		_, err := Start(t.Context(), Options{
			Binary: testBinary(t, sleeper), StateDir: state, ProfileDir: profile, Headless: true,
		})
		if err == nil {
			t.Fatalf("Start with profile %q succeeded", profile)
		}
		if profile != "" && !errors.Is(err, ErrProfileOutside) {
			t.Errorf("profile %q: got %v, want ErrProfileOutside", profile, err)
		}
		if _, err := os.Stat(filepath.Join(outside, "p")); err == nil {
			t.Fatalf("profile %q was created outside the state directory", profile)
		}
	}
}

func TestStartRefusesLiveSingletonLock(t *testing.T) {
	shortSettle(t)
	state, profile := newState(t)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(profile, "SingletonLock")
	// This test process is certainly alive.
	if err := os.Symlink(host+"-"+strconv.Itoa(os.Getpid()), lock); err != nil {
		t.Fatal(err)
	}
	opts := Options{Binary: testBinary(t, sleeper), StateDir: state, ProfileDir: profile, Headless: true}
	if _, err := Start(t.Context(), opts); !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("got %v, want ErrProfileInUse", err)
	}

	// A lock naming a dead pid is stale: removed, and the browser starts.
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(host+"-999999", lock); err != nil {
		t.Fatal(err)
	}
	p, err := Start(t.Context(), opts)
	if err != nil {
		t.Fatalf("stale lock should have been removed: %v", err)
	}
	defer p.Stop(context.Background())
	if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Error("stale lock still there")
	}
}

// A lock written on another machine cannot be live here, whatever the pid.
func TestStartRemovesForeignSingletonLock(t *testing.T) {
	shortSettle(t)
	state, profile := newState(t)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(profile, "SingletonLock")
	if err := os.Symlink("some-other-host-"+strconv.Itoa(os.Getpid()), lock); err != nil {
		t.Fatal(err)
	}
	p, err := Start(t.Context(), Options{
		Binary: testBinary(t, sleeper), StateDir: state, ProfileDir: profile, Headless: true,
	})
	if err != nil {
		t.Fatalf("foreign lock should have been removed: %v", err)
	}
	p.Stop(context.Background())
}

func TestStartNeedsDisplayWhenVisible(t *testing.T) {
	shortSettle(t)
	state, profile := newState(t)
	_, err := Start(t.Context(), Options{
		Binary: testBinary(t, sleeper), StateDir: state, ProfileDir: profile, Headless: false,
	})
	if !errors.Is(err, ErrNoDisplay) {
		t.Fatalf("got %v, want ErrNoDisplay", err)
	}
	if _, err := Start(t.Context(), Options{StateDir: state, ProfileDir: profile, Headless: true}); err == nil {
		t.Fatal("Start without a binary succeeded")
	}
}

// Nothing from the sidecar's environment reaches the browser unless it is on
// the allow-list: no provider keys, not even the service user's HOME.
func TestStartScrubsEnvironment(t *testing.T) {
	shortSettle(t)
	t.Setenv("OPENROUTER_KEY", "sk-must-not-leak")
	t.Setenv("HOLACLAW_TEST_MARKER", "must-not-leak")
	t.Setenv("HOLACLAW_TEST_ALLOWED", "on-the-list")
	t.Setenv("LANG", "en_GB.UTF-8")
	t.Setenv("NIXOS_OZONE_WL", "1")
	realHome := "/home/definitely-not-the-browsers-home"
	t.Setenv("HOME", realHome)

	state, profile := newState(t)
	dump := filepath.Join(t.TempDir(), "env.txt")
	bin := testBinary(t, "env > "+dump+"\n"+sleeper)
	p, err := Start(t.Context(), Options{
		Binary: bin, StateDir: state, ProfileDir: profile, Headless: true,
		EnvAllow: []string{"HOLACLAW_TEST_ALLOWED"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop(context.Background())

	var env map[string]string
	for range 100 {
		b, err := os.ReadFile(dump)
		if err == nil && len(b) > 0 {
			env = parseEnv(string(b))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if env == nil {
		t.Fatal("the fake browser never wrote its environment")
	}
	for _, name := range []string{"OPENROUTER_KEY", "HOLACLAW_TEST_MARKER", "NIXOS_OZONE_WL", "DISPLAY", "XAUTHORITY"} {
		if v, ok := env[name]; ok {
			t.Errorf("%s leaked as %q", name, v)
		}
	}
	if env["HOME"] == realHome || !strings.HasSuffix(env["HOME"], filepath.Join("profiles", "main.home")) {
		t.Errorf("HOME = %q, want a private directory beside the profile", env["HOME"])
	}
	if env["LANG"] != "en_GB.UTF-8" {
		t.Errorf("LANG = %q, want it passed through", env["LANG"])
	}
	if env["HOLACLAW_TEST_ALLOWED"] != "on-the-list" {
		t.Errorf("EnvAllow was ignored: %q", env["HOLACLAW_TEST_ALLOWED"])
	}
	for _, name := range []string{"XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR"} {
		if !strings.HasPrefix(env[name], env["HOME"]) {
			t.Errorf("%s = %q, want it under HOME", name, env[name])
		}
	}
}

func parseEnv(dump string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(dump, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out
}

func TestArgs(t *testing.T) {
	opts := Options{Binary: Binary{Path: "/x/chromium"}, Headless: true, ExtraArgs: []string{"--flag"}}
	got := strings.Join(args(opts, "/state/profiles/main"), " ")
	for _, want := range []string{
		"--remote-debugging-pipe", "--user-data-dir=/state/profiles/main", "--no-first-run",
		"--no-default-browser-check", "--disable-blink-features=AutomationControlled",
		"--window-size=1280,1200", "--password-store=basic",
		"--disable-features=Translate,MediaRouter", "--headless=new", "--flag", "about:blank",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("args lack %q: %s", want, got)
		}
	}
	// Flags we deliberately never pass (docs/browser.md).
	for _, never := range []string{"--no-sandbox", "--enable-automation", "--disable-gpu", "--user-agent"} {
		if strings.Contains(got, never) {
			t.Errorf("args contain %q: %s", never, got)
		}
	}
	// Windowed: no headless flag, and a custom size is honoured.
	got = strings.Join(args(Options{WindowWidth: 800, WindowHeight: 600}, "/p"), " ")
	if strings.Contains(got, "--headless") || !strings.Contains(got, "--window-size=800,600") {
		t.Errorf("windowed args wrong: %s", got)
	}
}

func TestRing(t *testing.T) {
	r := newRing(8)
	if r.String() != "" {
		t.Fatal("a fresh ring should be empty")
	}
	mustWrite(t, r, "abc")
	if r.String() != "abc" {
		t.Fatalf("got %q", r.String())
	}
	mustWrite(t, r, "defghij") // wraps: keeps the last 8
	if got := r.String(); got != "cdefghij" {
		t.Fatalf("got %q, want %q", got, "cdefghij")
	}
	mustWrite(t, r, "0123456789ABC") // one write larger than the whole ring
	if got := r.String(); got != "56789ABC" {
		t.Fatalf("got %q, want %q", got, "56789ABC")
	}
}

func mustWrite(t *testing.T, r *ring, s string) {
	t.Helper()
	n, err := r.Write([]byte(s))
	if err != nil || n != len(s) {
		t.Fatalf("Write(%q) = %d, %v", s, n, err)
	}
}
