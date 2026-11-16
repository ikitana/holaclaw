package chromium

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests drive a real browser on a real virtual display: the flag list,
// the sandbox, the pipe and the environment are only proven that way. The
// sandbox stays on — if Chromium cannot start sandboxed here, the test fails
// with what it printed rather than working around it.
func realBinary(t *testing.T) Binary {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode")
	}
	b, err := Find("")
	if err != nil {
		t.Skipf("no browser: %v", err)
	}
	if b.TooOld() {
		t.Skipf("browser %s is older than %d", b.Version, MinMajor)
	}
	return b
}

func needXvfb(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("Xvfb"); err != nil {
		t.Skip("no Xvfb")
	}
}

// getVersion proves the browser is live and the pipe is wired to fd 3/4:
// NUL-terminated JSON in, NUL-terminated JSON out.
func getVersion(t *testing.T, p *Process) string {
	t.Helper()
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		if _, err := p.Pipe.Write.Write([]byte("{\"id\":1,\"method\":\"Browser.getVersion\"}\x00")); err != nil {
			ch <- result{err: err}
			return
		}
		s, err := bufio.NewReader(p.Pipe.Read).ReadString(0)
		ch <- result{s: s, err: err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("CDP handshake: %v\nstderr: %s", res.err, p.Stderr())
		}
		return res.s
	case <-time.After(30 * time.Second):
		t.Fatalf("no CDP reply in 30s\nstderr: %s", p.Stderr())
		return ""
	}
}

func TestIntegrationHeadless(t *testing.T) {
	bin := realBinary(t)
	shortSettle(t)
	state, profile := newState(t)
	p, err := Start(t.Context(), Options{
		Binary: bin, StateDir: state, ProfileDir: profile, Headless: true,
	})
	if err != nil {
		t.Fatalf("headless start: %v", err)
	}
	defer p.Stop(context.Background())
	if reply := getVersion(t, p); !strings.Contains(reply, "product") {
		t.Fatalf("reply without a product field: %q", reply)
	}
}

func TestIntegrationWindowed(t *testing.T) {
	bin := realBinary(t)
	needXvfb(t)
	shortSettle(t)

	d, err := StartDisplay(t.Context(), DisplayOptions{})
	if err != nil {
		t.Fatalf("Xvfb: %v", err)
	}
	if d.Number < 0 || d.Name != ":"+strings.TrimPrefix(d.Name, ":") {
		t.Fatalf("display %+v", d)
	}
	if fi, err := os.Stat(d.Auth); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("authority file: %v, %v", fi, err)
	}

	state, profile := newState(t)
	p, err := Start(t.Context(), Options{
		Binary: bin, StateDir: state, ProfileDir: profile, Display: d,
	})
	if err != nil {
		d.Stop()
		t.Fatalf("windowed start on %s: %v", d.Name, err)
	}
	if reply := getVersion(t, p); !strings.Contains(reply, "product") {
		t.Fatalf("reply without a product field: %q", reply)
	}

	xvfbPID := d.cmd.Process.Pid
	browserPID := p.PID
	p.Stop(context.Background())
	d.Stop()
	waitFor(t, "the browser to go", func() bool { return !alive(browserPID) })
	waitFor(t, "Xvfb to go", func() bool { return !alive(xvfbPID) })
	if _, err := os.Stat(d.Auth); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("authority file left behind: %v", err)
	}
	// A killed X server leaves its socket behind; a stopped one must not.
	if _, err := os.Stat("/tmp/.X11-unix/X" + itoa(d.Number)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("X socket for display %d left behind: %v", d.Number, err)
	}
	if n := countDescendants(t, browserPID); n != 0 {
		t.Errorf("%d browser processes left in the group", n)
	}
}

// countDescendants counts live processes in the stopped browser's group.
func countDescendants(t *testing.T, pgid int) int {
	t.Helper()
	out, err := exec.Command("ps", "-eo", "pgid=").Output()
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	n := 0
	for _, line := range strings.Fields(string(out)) {
		if line == itoa(pgid) {
			n++
		}
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// The X socket in /tmp/.X11-unix is reachable by every local user, so the
// cookie is what keeps them from watching or driving the agent's browser.
// Proven by speaking X11 connection setup directly: refused without the
// cookie, accepted with it.
func TestIntegrationDisplayRequiresCookie(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	needXvfb(t)
	d, err := StartDisplay(t.Context(), DisplayOptions{})
	if err != nil {
		t.Fatalf("Xvfb: %v", err)
	}
	defer d.Stop()

	if got := xSetup(t, d, nil, nil); got != 0 {
		t.Errorf("an anonymous client got reply %d, want 0 (Failed): the display has no access control", got)
	}
	f, err := os.Open(d.Auth)
	if err != nil {
		t.Fatal(err)
	}
	entries := readXauth(t, f)
	_ = f.Close()
	if len(entries) != 1 || entries[0].number != itoa(d.Number) {
		t.Fatalf("authority file holds %+v, want one entry for display %d", entries, d.Number)
	}
	if got := xSetup(t, d, []byte(entries[0].name), entries[0].data); got != 1 {
		t.Errorf("the cookie from XAUTHORITY got reply %d, want 1 (Success)", got)
	}
}

// xSetup performs an X11 connection setup on the display's unix socket and
// returns the first reply byte: 0 Failed, 1 Success, 2 Authenticate.
func xSetup(t *testing.T, d *Display, name, data []byte) byte {
	t.Helper()
	conn, err := net.Dial("unix", "/tmp/.X11-unix/X"+itoa(d.Number))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	pad := func(n int) int { return (4 - n%4) % 4 }
	req := []byte{'l', 0}
	req = binary.LittleEndian.AppendUint16(req, 11) // protocol major
	req = binary.LittleEndian.AppendUint16(req, 0)  // protocol minor
	req = binary.LittleEndian.AppendUint16(req, uint16(len(name)))
	req = binary.LittleEndian.AppendUint16(req, uint16(len(data)))
	req = append(req, 0, 0)
	req = append(req, name...)
	req = append(req, make([]byte, pad(len(name)))...)
	req = append(req, data...)
	req = append(req, make([]byte, pad(len(data)))...)
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("write: %v", err)
	}
	reply := make([]byte, 1)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read: %v", err)
	}
	return reply[0]
}

// Two agents get two profiles and two displays: no agent sees another's
// window, and no agent shares another's cookies (docs/browser.md).
func TestIntegrationTwoAgents(t *testing.T) {
	bin := realBinary(t)
	needXvfb(t)
	shortSettle(t)
	state, _ := newState(t)

	m := NewManager(ManagerOptions{Binary: bin, StateDir: state, Tick: make(chan time.Time)})
	defer m.Close(context.Background())

	a, err := m.Acquire(t.Context(), "alpha")
	if err != nil {
		t.Fatalf("alpha: %v", err)
	}
	b, err := m.Acquire(t.Context(), "beta")
	if err != nil {
		t.Fatalf("beta: %v", err)
	}
	if a.PID == b.PID {
		t.Fatal("two agents share a process")
	}
	for _, agent := range []string{"alpha", "beta"} {
		if _, err := os.Stat(filepath.Join(state, "profiles", agent)); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Lock()
	da, db := m.procs["alpha"].display, m.procs["beta"].display
	m.mu.Unlock()
	if da == nil || db == nil || da.Number == db.Number {
		t.Fatalf("displays %v / %v, want two different ones", da, db)
	}
	for _, p := range []*Process{a, b} {
		if reply := getVersion(t, p); !strings.Contains(reply, "product") {
			t.Fatalf("reply without a product field: %q", reply)
		}
	}

	pids := []int{a.PID, b.PID, da.cmd.Process.Pid, db.cmd.Process.Pid}
	m.Close(context.Background())
	for _, pid := range pids {
		waitFor(t, "pid "+itoa(pid)+" to go", func() bool { return !alive(pid) })
	}
}

// One profile, one browser: the second Start is refused rather than left to
// corrupt the profile.
func TestIntegrationProfileInUse(t *testing.T) {
	bin := realBinary(t)
	shortSettle(t)
	state, profile := newState(t)
	opts := Options{Binary: bin, StateDir: state, ProfileDir: profile, Headless: true}

	p, err := Start(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Stop(context.Background())
	getVersion(t, p) // the lock exists once the browser is up

	lock := filepath.Join(profile, "SingletonLock")
	waitFor(t, "the singleton lock", func() bool {
		_, err := os.Lstat(lock)
		return err == nil
	})
	if _, err := Start(t.Context(), opts); !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("second start: %v, want ErrProfileInUse", err)
	}
}
