package chromium

import (
	"context"
	"errors"
	"holaclaw/internal/config"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// recorder collects OnEvent callbacks.
type recorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *recorder) on(agent string, kind EventKind) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, agent+":"+string(kind))
}

func (r *recorder) count(want string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.seen {
		if s == want {
			n++
		}
	}
	return n
}

// waitFor polls until cond holds or the test gives up.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fakeClock is the Manager's clock in tests: time moves only when told.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newManager builds a headless Manager over a fake binary. Headless keeps the
// unit tests off Xvfb; the real display is covered by the integration tests.
func newManager(t *testing.T, body string, opts ManagerOptions) (*Manager, *recorder, *fakeClock, chan time.Time) {
	t.Helper()
	shortSettle(t)
	state, _ := newState(t)
	rec := &recorder{}
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	tick := make(chan time.Time)
	opts.Binary = testBinary(t, body)
	opts.StateDir = state
	opts.Headless = true
	opts.OnEvent = rec.on
	opts.Now = clock.Now
	opts.Tick = tick
	m := NewManager(opts)
	t.Cleanup(func() { m.Close(context.Background()) })
	return m, rec, clock, tick
}

func TestManagerAcquireAndProfile(t *testing.T) {
	m, rec, _, _ := newManager(t, sleeper, ManagerOptions{})
	p, err := m.Acquire(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}
	// Profiles follow agents: <state>/profiles/<agent>, 0700.
	profile := filepath.Join(m.opts.StateDir, "profiles", "main")
	fi, err := os.Stat(profile)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("profile %s: %v, %v", profile, fi, err)
	}
	// A second Acquire reuses the process.
	q, err := m.Acquire(t.Context(), "main")
	if err != nil || q.PID != p.PID {
		t.Fatalf("got %v pid %v, want the same process", err, q)
	}
	if n := rec.count("main:start"); n != 1 {
		t.Errorf("%d start events, want 1", n)
	}
	m.Release("main")
	m.Release("main")

	// Two agents, two processes, two profiles.
	other, err := m.Acquire(t.Context(), "other")
	if err != nil {
		t.Fatal(err)
	}
	if other.PID == p.PID {
		t.Fatal("two agents share a process")
	}
	if _, err := os.Stat(filepath.Join(m.opts.StateDir, "profiles", "other")); err != nil {
		t.Fatal(err)
	}
}

func TestManagerAgentNames(t *testing.T) {
	m, _, _, _ := newManager(t, sleeper, ManagerOptions{})
	bad := []string{"", "..", ".", "../x", "a/b", "/abs", "Main", "main ", "-lead", "_x", "a\x00b", "a.b"}
	for _, name := range bad {
		if _, err := m.Acquire(t.Context(), name); !errors.Is(err, ErrBadAgent) {
			t.Errorf("Acquire(%q) = %v, want ErrBadAgent", name, err)
		}
	}
	for _, name := range []string{"main", "a", "0", "research-bot", "a_b-2"} {
		if !config.ValidAgentName(name) {
			t.Errorf("%q should be a valid agent name", name)
		}
	}
	// 63 bytes is the limit.
	long := make([]byte, 63)
	for i := range long {
		long[i] = 'a'
	}
	if !config.ValidAgentName(string(long)) {
		t.Error("63 bytes should be accepted")
	}
	if config.ValidAgentName(string(long) + "a") {
		t.Error("64 bytes should be refused")
	}
}

// Concurrent Acquire for one agent must start exactly one browser.
func TestManagerConcurrentAcquire(t *testing.T) {
	m, rec, _, _ := newManager(t, sleeper, ManagerOptions{})
	const n = 12
	pids := make([]int, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := m.Acquire(t.Context(), "main")
			errs[i] = err
			if err == nil {
				pids[i] = p.PID
			}
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if pids[i] != pids[0] {
			t.Fatalf("goroutine %d got pid %d, want %d", i, pids[i], pids[0])
		}
	}
	if got := rec.count("main:start"); got != 1 {
		t.Fatalf("%d start events, want exactly 1", got)
	}
}

func TestManagerMaxProcesses(t *testing.T) {
	m, _, _, _ := newManager(t, sleeper, ManagerOptions{MaxProcesses: 2})
	for _, agent := range []string{"a", "b"} {
		if _, err := m.Acquire(t.Context(), agent); err != nil {
			t.Fatalf("%s: %v", agent, err)
		}
	}
	if _, err := m.Acquire(t.Context(), "c"); !errors.Is(err, ErrTooManyProcesses) {
		t.Fatalf("got %v, want ErrTooManyProcesses", err)
	}
	// Stopping one makes room again.
	m.Stop(t.Context(), "a")
	if _, err := m.Acquire(t.Context(), "c"); err != nil {
		t.Fatalf("after Stop: %v", err)
	}
}

func TestManagerIdleStop(t *testing.T) {
	m, rec, clock, tick := newManager(t, sleeper, ManagerOptions{IdleTimeout: 10 * time.Minute})
	p, err := m.Acquire(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}

	// Still in use: a sweep long after the timeout must not touch it.
	clock.advance(time.Hour)
	tick <- clock.Now()
	tick <- clock.Now() // the second send proves the first sweep finished
	if !alive(p.PID) {
		t.Fatal("a browser in use was stopped")
	}

	m.Release("main")
	// Released, but not yet idle long enough.
	clock.advance(9 * time.Minute)
	tick <- clock.Now()
	tick <- clock.Now()
	if !alive(p.PID) {
		t.Fatal("stopped before the idle timeout")
	}

	clock.advance(2 * time.Minute)
	tick <- clock.Now()
	waitFor(t, "idle stop", func() bool { return rec.count("main:idle_stop") == 1 })
	waitFor(t, "the process to go", func() bool { return !alive(p.PID) })

	// The next Acquire starts a fresh one.
	q, err := m.Acquire(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if q.PID == p.PID {
		t.Fatal("got the stopped process back")
	}
}

// A browser that dies on its own is reported and replaced.
func TestManagerCrashRestart(t *testing.T) {
	m, rec, _, _ := newManager(t, "sleep 0.4\nexit 3", ManagerOptions{})
	p, err := m.Acquire(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the crash event", func() bool { return rec.count("main:crash") == 1 })

	q, err := m.Acquire(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}
	if q.PID == p.PID {
		t.Fatal("the crashed process was handed out again")
	}
	if got := rec.count("main:start"); got != 2 {
		t.Errorf("%d start events, want 2", got)
	}
}

// A deliberate stop is not a crash.
func TestManagerStopIsNotACrash(t *testing.T) {
	m, rec, _, _ := newManager(t, sleeper, ManagerOptions{})
	p, err := m.Acquire(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}
	m.Stop(t.Context(), "main")
	waitFor(t, "the stop event", func() bool { return rec.count("main:stop") == 1 })
	time.Sleep(100 * time.Millisecond)
	if n := rec.count("main:crash"); n != 0 {
		t.Errorf("%d crash events after a deliberate stop", n)
	}
	if alive(p.PID) {
		t.Error("process survived Stop")
	}
	m.Stop(t.Context(), "main") // unknown agent: a no-op
}

func TestManagerClose(t *testing.T) {
	m, rec, _, _ := newManager(t, sleeper, ManagerOptions{})
	var pids []int
	for _, agent := range []string{"a", "b"} {
		p, err := m.Acquire(t.Context(), agent)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, p.PID)
	}
	m.Close(t.Context())
	for _, pid := range pids {
		if alive(pid) {
			t.Errorf("pid %d survived Close", pid)
		}
	}
	if rec.count("a:stop") != 1 || rec.count("b:stop") != 1 {
		t.Error("Close should emit one stop event per browser")
	}
	if _, err := m.Acquire(t.Context(), "a"); err == nil {
		t.Error("Acquire on a closed manager succeeded")
	}
	m.Close(t.Context()) // idempotent
}

// A browser that cannot start is not left behind as a half-made entry.
func TestManagerStartFailure(t *testing.T) {
	m, _, _, _ := newManager(t, "exit 1", ManagerOptions{})
	if _, err := m.Acquire(t.Context(), "main"); err == nil {
		t.Fatal("Acquire succeeded with a broken browser")
	}
	m.mu.Lock()
	n := len(m.procs)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d entries left after a failed start", n)
	}
	// The slot is free again.
	if _, err := m.Acquire(t.Context(), "main"); err == nil {
		t.Fatal("expected the same failure twice")
	}
}
