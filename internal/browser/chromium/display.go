package chromium

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrNoXvfb says the virtual display server is missing. Visible is the default
// (docs/browser.md), so this is a deployment problem `check` reports rather than a reason
// to fall back to headless behind the operator's back.
var ErrNoXvfb = errors.New("chromium: Xvfb not found on PATH; install it or set browser.headless = true")

// displayTimeout bounds how long we wait for Xvfb to report its display number.
const displayTimeout = 10 * time.Second

// DisplayOptions configures one virtual display. One display per browser
// process: agents never see each other's windows.
type DisplayOptions struct {
	// Dir holds the authority file. It must already exist and be private to
	// the sidecar; when empty a private temporary directory is made and
	// removed by Stop.
	Dir           string
	Width, Height int // default DefaultWindowWidth × DefaultWindowHeight
}

// Display is a running Xvfb. Name and Auth go into Chromium's environment as
// DISPLAY and XAUTHORITY; nothing else may read the cookie.
type Display struct {
	Number int
	Name   string // ":7"
	Auth   string // path of the Xauthority file

	cmd     *exec.Cmd
	done    chan struct{} // closed when Xvfb has exited
	tmpDir  string        // removed by Stop when we made it
	stopped sync.Once
}

// displayGrace is how long a SIGTERMed Xvfb has to remove its socket and lock
// file before it is killed.
const displayGrace = 2 * time.Second

// StartDisplay starts Xvfb on a free display. The number is chosen by Xvfb
// itself and reported through -displayfd, which avoids the race two sidecars
// (or a leftover lock) would have on a fixed :99. The server listens on no TCP
// port and accepts only clients holding the generated MIT-MAGIC-COOKIE-1.
func StartDisplay(ctx context.Context, opts DisplayOptions) (*Display, error) {
	bin, err := exec.LookPath("Xvfb")
	if err != nil {
		return nil, ErrNoXvfb
	}
	if opts.Width <= 0 {
		opts.Width = DefaultWindowWidth
	}
	if opts.Height <= 0 {
		opts.Height = DefaultWindowHeight
	}

	d := &Display{}
	dir := opts.Dir
	if dir == "" {
		dir, err = os.MkdirTemp("", "holaclaw-xauth-")
		if err != nil {
			return nil, fmt.Errorf("chromium: %w", err)
		}
		d.tmpDir = dir
	}
	d.Auth = filepath.Join(dir, "Xauthority")

	cookie, err := newCookie()
	if err != nil {
		d.cleanup()
		return nil, err
	}
	host, err := os.Hostname()
	if err != nil {
		host = "localhost"
	}
	// The server reads the cookie data out of this file at start; the entry's
	// display number is rewritten below once Xvfb has picked one.
	if err := writeXauthFile(d.Auth, host, "0", cookie); err != nil {
		d.cleanup()
		return nil, fmt.Errorf("chromium: xauthority: %w", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		d.cleanup()
		return nil, fmt.Errorf("chromium: %w", err)
	}
	defer func() { _ = r.Close() }()

	// -displayfd 3: the pipe is the child's fd 3. -nolisten tcp plus the
	// authority file leave the unix socket as the only way in.
	cmd := exec.CommandContext(ctx, bin,
		"-displayfd", "3",
		"-screen", "0", fmt.Sprintf("%dx%dx24", opts.Width, opts.Height),
		"-nolisten", "tcp",
		"-auth", d.Auth,
	)
	cmd.ExtraFiles = []*os.File{w}
	cmd.Env = []string{"PATH=" + filepath.Dir(bin)}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		_ = w.Close()
		d.cleanup()
		return nil, fmt.Errorf("chromium: Xvfb: %w", err)
	}
	_ = w.Close() // the parent's copy, or the read below never sees EOF
	d.cmd, d.done = cmd, make(chan struct{})
	// Reap Xvfb when it exits so a stopped display leaves no zombie.
	go func() {
		_ = cmd.Wait()
		close(d.done)
	}()

	num, err := readDisplayNumber(ctx, r)
	if err != nil {
		d.Stop()
		return nil, err
	}
	d.Number, d.Name = num, ":"+strconv.Itoa(num)
	if err := writeXauthFile(d.Auth, host, strconv.Itoa(num), cookie); err != nil {
		d.Stop()
		return nil, fmt.Errorf("chromium: xauthority: %w", err)
	}
	return d, nil
}

// readDisplayNumber reads the decimal number Xvfb writes to -displayfd. Xvfb
// writes it only once the server is ready to accept clients.
func readDisplayNumber(ctx context.Context, r *os.File) (int, error) {
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		if err != nil && strings.TrimSpace(line) == "" {
			ch <- result{err: fmt.Errorf("chromium: Xvfb did not report a display: %w", err)}
			return
		}
		n, cerr := strconv.Atoi(strings.TrimSpace(line))
		if cerr != nil {
			ch <- result{err: fmt.Errorf("chromium: Xvfb reported %q, not a display number", strings.TrimSpace(line))}
			return
		}
		ch <- result{n: n}
	}()
	timer := time.NewTimer(displayTimeout)
	defer timer.Stop()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-timer.C:
		return 0, errors.New("chromium: timed out waiting for Xvfb to start")
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Stop shuts Xvfb down and removes the authority file. SIGTERM first: a
// killed X server leaves its socket in /tmp/.X11-unix and its lock file
// behind. Safe to call twice.
func (d *Display) Stop() {
	d.stopped.Do(func() {
		if d.cmd == nil || d.cmd.Process == nil {
			d.cleanup()
			return
		}
		pid := d.cmd.Process.Pid
		killGroup(pid, syscall.SIGTERM)
		timer := time.NewTimer(displayGrace)
		defer timer.Stop()
		select {
		case <-d.done:
		case <-timer.C:
			killGroup(pid, syscall.SIGKILL)
			<-d.done
		}
		d.cleanup()
	})
}

// cleanup removes what StartDisplay created.
func (d *Display) cleanup() {
	if d.tmpDir != "" {
		_ = os.RemoveAll(d.tmpDir)
		return
	}
	if d.Auth != "" {
		_ = os.Remove(d.Auth)
	}
}
