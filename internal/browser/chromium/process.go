package chromium

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Typed start failures. The sidecar turns these into the fixed, actionable
// sentences `check` and Health.Problem report.
var (
	// ErrProfileInUse means another live Chromium holds this profile. Two
	// processes on one --user-data-dir corrupt it, so we refuse rather than
	// let Chromium decide.
	ErrProfileInUse = errors.New("chromium: profile is already in use by a running browser")
	// ErrProfileOutside means the profile path escapes the state directory.
	ErrProfileOutside = errors.New("chromium: profile directory must be inside the state directory")
	// ErrNoDisplay means a visible browser was asked for without a display.
	ErrNoDisplay = errors.New("chromium: a display is required unless headless")
)

// ErrSandbox is returned when Chromium exits at start because its own sandbox
// could not be set up. --no-sandbox is never used (docs/browser.md): a renderer that parses
// hostile pages stays sandboxed or the browser does not run.
type ErrSandbox struct {
	// Stderr is what Chromium printed, for diagnostics only. It may contain
	// URLs, so it must never reach a trace event.
	Stderr string
}

func (e *ErrSandbox) Error() string {
	return "chromium: the browser sandbox could not start, and --no-sandbox is never used. " +
		"Unprivileged user namespaces are most likely disabled (kernel or AppArmor). Check: " +
		"`sysctl kernel.unprivileged_userns_clone` and `sysctl kernel.apparmor_restrict_unprivileged_userns` " +
		"(both must allow user namespaces for this user), or install the setuid sandbox helper that ships with the browser."
}

// sandboxMarkers are what Chromium prints when its sandbox cannot start.
var sandboxMarkers = []string{
	"No usable sandbox",
	"setuid sandbox",
	"SUID sandbox",
	"namespace sandbox",
	"Failed to move to new namespace",
	"clone(): Operation not permitted",
}

// startSettle is how long Start watches a fresh process before calling it
// started. A sandbox or profile failure is immediate; anything later is
// reported through Wait. A variable so tests need not wait a whole second.
var startSettle = time.Second

// The window every browser opens with, and so the display it runs on, the
// screencast a person watches and the viewport a move is aimed at when the
// page reports none. Taller than a laptop screen on purpose: a form that
// spreads down the page — a travel search with its banner, its trip type, its
// stations and its dates — is on screen at once, and what is on screen is what
// the shortlist ranks first and what a screenshot shows. The width stays the
// one the live view was measured at.
const (
	DefaultWindowWidth  = 1280
	DefaultWindowHeight = 1200
)

const (
	// stopGrace is how long a SIGTERMed process group has before SIGKILL.
	stopGrace = 5 * time.Second
	// stderrTail is the diagnostic ring buffer size.
	stderrTail = 8 << 10
)

// Options configures one browser process.
type Options struct {
	Binary Binary
	// StateDir is the sidecar's StateDirectory. ProfileDir must be inside it.
	StateDir string
	// ProfileDir is the agent's persistent --user-data-dir. Created 0700.
	ProfileDir string
	// Headless opts into --headless=new for hosts without X (docs/browser.md).
	Headless bool
	// Display is required when !Headless.
	Display *Display
	// WindowWidth/Height default to DefaultWindowWidth × DefaultWindowHeight.
	WindowWidth, WindowHeight int
	// DownloadDir, when set, is where downloads land (a quarantine directory
	// owned by the caller).
	DownloadDir string
	// EnvAllow names extra environment variables to pass through from this
	// process, on top of the built-in allow-list. Everything else is scrubbed.
	EnvAllow []string
	// ExtraArgs are appended last. Tests only: every flag is a fingerprint.
	ExtraArgs []string
	Logger    *slog.Logger
}

// Pipe is Chromium's --remote-debugging-pipe, raw. The CDP client owns the
// NUL-framed JSON on it; this package only hands over the ends.
type Pipe struct {
	Read  io.ReadCloser  // Chromium's fd 4: replies and events
	Write io.WriteCloser // Chromium's fd 3: commands
}

// Process is one running browser.
type Process struct {
	Pipe Pipe
	PID  int

	cmd     *exec.Cmd
	stderr  *ring
	done    chan struct{}
	exitErr error

	stopOnce sync.Once
}

// Start launches Chromium. The caller owns Options.Display: Stop does not stop
// it, because the Manager pairs the two and integration tests do not.
func Start(ctx context.Context, opts Options) (*Process, error) {
	if opts.Binary.Path == "" {
		return nil, errors.New("chromium: no binary")
	}
	if !opts.Headless && opts.Display == nil {
		return nil, ErrNoDisplay
	}
	profile, err := profilePath(opts.StateDir, opts.ProfileDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return nil, fmt.Errorf("chromium: profile: %w", err)
	}
	if err := checkSingleton(profile); err != nil {
		return nil, err
	}
	home := profile + ".home"
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("chromium: home: %w", err)
	}

	// fd 3 is what Chromium reads, fd 4 what it writes.
	toRead, toWrite, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("chromium: %w", err)
	}
	fromRead, fromWrite, err := os.Pipe()
	if err != nil {
		_, _ = toRead.Close(), toWrite.Close()
		return nil, fmt.Errorf("chromium: %w", err)
	}

	tail := newRing(stderrTail)
	cmd := exec.CommandContext(ctx, opts.Binary.Path, args(opts, profile)...)
	cmd.Env = environ(opts, home)
	cmd.Dir = home
	cmd.Stdout = io.Discard
	cmd.Stderr = tail
	cmd.ExtraFiles = []*os.File{toRead, fromWrite}
	// Setpgid so Stop can signal the whole browser (zygote, renderers, GPU),
	// Pdeathsig so a dead sidecar never leaves a browser behind.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}

	if err := cmd.Start(); err != nil {
		closeAll(toRead, toWrite, fromRead, fromWrite)
		return nil, fmt.Errorf("chromium: start: %w", err)
	}
	// The child holds its own copies now.
	_, _ = toRead.Close(), fromWrite.Close()

	p := &Process{
		Pipe:   Pipe{Read: fromRead, Write: toWrite},
		PID:    cmd.Process.Pid,
		cmd:    cmd,
		stderr: tail,
		done:   make(chan struct{}),
	}
	go func() {
		err := cmd.Wait()
		p.exitErr = classify(err, tail.String())
		close(p.done)
	}()

	// Watch briefly: a sandbox or profile failure kills Chromium at once, and
	// the caller deserves the typed error rather than a dead pipe.
	timer := time.NewTimer(startSettle)
	defer timer.Stop()
	select {
	case <-p.done:
		closeAll(toWrite, fromRead)
		return nil, p.exitErr
	case <-timer.C:
	case <-ctx.Done():
		p.Stop(context.Background())
		return nil, ctx.Err()
	}
	if opts.Logger != nil {
		opts.Logger.Info("browser started", "pid", p.PID, "profile", profile, "headless", opts.Headless)
	}
	return p, nil
}

// args is the whole flag list. Every flag is a fingerprint, so the list is
// short and each entry earns its place:
//
//	--remote-debugging-pipe                 CDP over fd 3/4; no TCP port to find
//	--user-data-dir                         the agent's persistent profile (docs/browser.md)
//	--no-first-run                          no welcome tab on a fresh profile
//	--no-default-browser-check              no modal on a server
//	--disable-blink-features=Automation…    measured: without it a remote-debugging
//	                                        launch makes navigator.webdriver true
//	--window-size=1280,1200                 DefaultWindowWidth × DefaultWindowHeight
//	--password-store=basic                  no gnome-keyring/kwallet prompt headlessly
//	--disable-features=Translate,MediaRouter  two background services that pop UI and
//	                                        talk to the network on their own
//	--headless=new                          only when Headless is set
//	about:blank                             a first tab to attach to
//
// Deliberately absent: --enable-automation (a fingerprint and it changes UI),
// --no-sandbox (never), --disable-gpu (not needed; the GPU process falls back
// to software on its own), and any user-agent override (docs/browser.md: browse honestly).
func args(opts Options, profile string) []string {
	w, h := opts.WindowWidth, opts.WindowHeight
	if w <= 0 || h <= 0 {
		w, h = DefaultWindowWidth, DefaultWindowHeight
	}
	a := []string{
		"--remote-debugging-pipe",
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-blink-features=AutomationControlled",
		fmt.Sprintf("--window-size=%d,%d", w, h),
		"--password-store=basic",
		"--disable-features=Translate,MediaRouter",
		// A virtual display has no GPU. Without this the GPU process gives up
		// and pages see no WebGL at all — a stronger bot signal than the
		// software renderer Chromium ships anyway (headless picks it by itself).
		// Measured on sannysoft, Chromium 153: "no webgl context" → SwiftShader.
		"--use-angle=swiftshader",
	}
	if opts.Headless {
		a = append(a, "--headless=new")
	}
	a = append(a, opts.ExtraArgs...)
	return append(a, "about:blank")
}

// passThrough are the only variables inherited from the sidecar: locale and
// time zone so pages render as the owner's machine would, and the fontconfig
// paths a packaged browser may need to find fonts. Everything else — provider
// keys, the service user's HOME, session buses, NIXOS_OZONE_WL — is dropped.
var passThrough = []string{"LANG", "LC_ALL", "LC_TIME", "TZ", "FONTCONFIG_FILE", "FONTCONFIG_PATH"}

// environ builds Chromium's environment from nothing. HOME points at a private
// directory beside the profile so no part of the browser reads or writes the
// service user's home, and the XDG_* triple keeps caches and runtime files
// there too.
func environ(opts Options, home string) []string {
	env := []string{
		"HOME=" + home,
		"PATH=" + filepath.Dir(opts.Binary.Path) + ":/usr/bin:/bin",
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"),
		"XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_DATA_HOME=" + filepath.Join(home, "share"),
		"XDG_RUNTIME_DIR=" + filepath.Join(home, "run"),
	}
	for _, dir := range env[2:] {
		_, path, _ := strings.Cut(dir, "=")
		_ = os.MkdirAll(path, 0o700)
	}
	if opts.Display != nil {
		env = append(env, "DISPLAY="+opts.Display.Name, "XAUTHORITY="+opts.Display.Auth)
	}
	for _, name := range append(passThrough, opts.EnvAllow...) {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// profilePath resolves the profile and refuses anything that escapes the state
// directory — `../x`, an absolute path elsewhere, or a symlink pointing out.
func profilePath(stateDir, profileDir string) (string, error) {
	if stateDir == "" || profileDir == "" {
		return "", errors.New("chromium: state and profile directories are required")
	}
	state, err := resolve(stateDir)
	if err != nil {
		return "", fmt.Errorf("chromium: state directory: %w", err)
	}
	profile, err := resolve(profileDir)
	if err != nil {
		return "", fmt.Errorf("chromium: profile directory: %w", err)
	}
	rel, err := filepath.Rel(state, profile)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrProfileOutside, profileDir)
	}
	return profile, nil
}

// resolve makes a path absolute and follows symlinks as far as the path
// exists, so a link planted anywhere along it cannot hide an escape.
func resolve(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for {
		real, err := filepath.EvalSymlinks(abs)
		if err == nil {
			return filepath.Join(real, rest), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return filepath.Join(abs, rest), nil
		}
		rest = filepath.Join(filepath.Base(abs), rest)
		abs = parent
	}
}

// checkSingleton reads Chromium's own SingletonLock, a symlink to
// "<host>-<pid>". A live pid on this host means a second browser would corrupt
// the profile; a stale lock (crash, reboot) is removed.
func checkSingleton(profile string) error {
	lock := filepath.Join(profile, "SingletonLock")
	target, err := os.Readlink(lock)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		// Not a symlink: not a lock we understand, leave it alone.
		return nil
	}
	// The host name may itself contain dashes, so split on the last one.
	if i := strings.LastIndex(target, "-"); i >= 0 {
		host, pidText := target[:i], target[i+1:]
		pid, err := strconv.Atoi(pidText)
		if err == nil && pid > 0 && sameHost(host) && alive(pid) {
			return fmt.Errorf("%w (pid %d, %s)", ErrProfileInUse, pid, profile)
		}
	}
	if err := os.Remove(lock); err != nil {
		return fmt.Errorf("chromium: stale %s: %w", lock, err)
	}
	return nil
}

// sameHost reports whether the lock was written on this machine. A lock from
// another host (a shared state directory) can only be stale here.
func sameHost(host string) bool {
	h, err := os.Hostname()
	return err == nil && h == host
}

// alive reports whether a pid exists. Signal 0 checks without delivering.
func alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// Wait returns a channel that yields the exit error once, then closes. It may
// be called any number of times.
func (p *Process) Wait() <-chan error {
	ch := make(chan error, 1)
	go func() {
		<-p.done
		ch <- p.exitErr
		close(ch)
	}()
	return ch
}

// Stderr is the tail of what Chromium printed, capped. Diagnostics only: it
// can contain URLs, so it must never reach a trace event or a message.
func (p *Process) Stderr() string { return p.stderr.String() }

// Stop closes the pipes, asks the whole process group to quit, and kills it if
// it will not. Idempotent.
func (p *Process) Stop(ctx context.Context) {
	p.stopOnce.Do(func() {
		closeAll(p.Pipe.Write, p.Pipe.Read)
		if p.cmd.Process == nil {
			return
		}
		killGroup(p.PID, syscall.SIGTERM)
		timer := time.NewTimer(stopGrace)
		defer timer.Stop()
		select {
		case <-p.done:
		case <-timer.C:
		case <-ctx.Done():
		}
		// Unconditional: renderers and the zygote can outlive the browser.
		killGroup(p.PID, syscall.SIGKILL)
	})
	<-p.done
}

// killGroup signals the process group led by pid, falling back to the process
// itself if the group is gone.
func killGroup(pid int, sig syscall.Signal) {
	if err := syscall.Kill(-pid, sig); err != nil {
		_ = syscall.Kill(pid, sig)
	}
}

// classify turns an exit error into a typed one when the stderr tail explains
// it. Only the sandbox gets its own type: it is the one failure with a fixed
// operator fix.
func classify(err error, stderr string) error {
	if err == nil {
		return nil
	}
	for _, m := range sandboxMarkers {
		if strings.Contains(stderr, m) {
			return &ErrSandbox{Stderr: stderr}
		}
	}
	return fmt.Errorf("chromium: %w", err)
}

// closeAll closes every non-nil closer, ignoring errors.
func closeAll(cs ...io.Closer) {
	for _, c := range cs {
		if c != nil {
			_ = c.Close()
		}
	}
}

// ring is a fixed-size tail buffer: the last n bytes written, nothing more.
type ring struct {
	mu   sync.Mutex
	buf  []byte
	n    int
	full bool
}

func newRing(n int) *ring { return &ring{buf: make([]byte, n)} }

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := len(p)
	if len(p) > len(r.buf) {
		p = p[len(p)-len(r.buf):]
		r.full = true
		r.n = 0
	}
	for _, b := range p {
		r.buf[r.n] = b
		r.n++
		if r.n == len(r.buf) {
			r.n, r.full = 0, true
		}
	}
	return total, nil
}

func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		return string(r.buf[:r.n])
	}
	return string(r.buf[r.n:]) + string(r.buf[:r.n])
}
