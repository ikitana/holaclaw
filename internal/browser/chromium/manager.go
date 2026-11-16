package chromium

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"holaclaw/internal/config"
)

// Profiles follow agents (docs/browser.md): one Chromium process and one persistent
// profile per agent, started on first use, stopped when idle, restarted after
// a crash. Agents never share cookies or logins.
const (
	// DefaultIdleTimeout is how long an unused browser stays up.
	DefaultIdleTimeout = 10 * time.Minute
	// DefaultMaxProcesses caps concurrent browsers. Each is a few hundred MB;
	// the sidecar's unit has a MemoryMax and refusing is better than swapping.
	DefaultMaxProcesses = 4
)

// ErrTooManyProcesses is returned when MaxProcesses browsers already run.
var ErrTooManyProcesses = errors.New("chromium: too many browser processes")

// ErrBadAgent is returned for an agent name that is not a safe path segment.
var ErrBadAgent = errors.New("chromium: invalid agent name")

// EventKind is what happened to a browser process, as OnEvent reports it.
type EventKind string

const (
	EventStart    EventKind = "start"
	EventIdleStop EventKind = "idle_stop"
	EventCrash    EventKind = "crash"
	EventStop     EventKind = "stop"
)

// ManagerOptions configures the supervisor.
type ManagerOptions struct {
	Binary   Binary
	StateDir string
	// Headless opts every browser into --headless=new and starts no display.
	Headless     bool
	IdleTimeout  time.Duration // default DefaultIdleTimeout
	MaxProcesses int           // default DefaultMaxProcesses
	// OnEvent is called for every process transition. It must not block.
	OnEvent func(agent string, kind EventKind)
	Logger  *slog.Logger

	// Now and Tick are the clock, faked in tests. Tick drives the idle sweep;
	// when nil a ticker at a quarter of IdleTimeout is used.
	Now  func() time.Time
	Tick <-chan time.Time
}

// Manager keeps one browser per agent.
type Manager struct {
	opts   ManagerOptions
	mu     sync.Mutex
	procs  map[string]*entry
	closed bool

	stop   chan struct{}
	ticker *time.Ticker
	wg     sync.WaitGroup
}

// entry is one agent's browser, possibly still starting.
type entry struct {
	ready   chan struct{} // closed when proc or err is set
	proc    *Process
	display *Display
	err     error

	refs int
	idle time.Time // when refs last reached zero
	dead bool      // the process exited; the next Acquire starts a new one
}

// NewManager starts the supervisor. Close stops every browser it started.
func NewManager(opts ManagerOptions) *Manager {
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.MaxProcesses <= 0 {
		opts.MaxProcesses = DefaultMaxProcesses
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	m := &Manager{opts: opts, procs: map[string]*entry{}, stop: make(chan struct{})}
	tick := opts.Tick
	if tick == nil {
		m.ticker = time.NewTicker(opts.IdleTimeout / 4)
		tick = m.ticker.C
	}
	m.wg.Add(1)
	go m.sweep(tick)
	return m
}

// Acquire returns the agent's browser, starting it if needed, and counts the
// caller as a user of it until Release. Concurrent callers for one agent get
// the same process: exactly one is started.
func (m *Manager) Acquire(ctx context.Context, agent string) (*Process, error) {
	if !config.ValidAgentName(agent) {
		return nil, fmt.Errorf("%w: %q", ErrBadAgent, agent)
	}
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, errors.New("chromium: manager is closed")
		}
		e, ok := m.procs[agent]
		switch {
		case ok && e.dead:
			// A crashed browser is replaced, not reused.
			delete(m.procs, agent)
			m.mu.Unlock()
			continue
		case ok:
			e.refs++
			m.mu.Unlock()
			select {
			case <-e.ready:
			case <-ctx.Done():
				m.release(e)
				return nil, ctx.Err()
			}
			if e.err != nil {
				m.release(e)
				return nil, e.err
			}
			return e.proc, nil
		}
		if len(m.procs) >= m.opts.MaxProcesses {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w (max %d)", ErrTooManyProcesses, m.opts.MaxProcesses)
		}
		// Publish the entry before starting, so a concurrent Acquire waits on
		// it instead of starting a second browser.
		e = &entry{ready: make(chan struct{}), refs: 1}
		m.procs[agent] = e
		m.mu.Unlock()

		m.start(ctx, agent, e)
		if e.err != nil {
			m.mu.Lock()
			delete(m.procs, agent)
			m.mu.Unlock()
			return nil, e.err
		}
		m.event(agent, EventStart)
		return e.proc, nil
	}
}

// start brings up the display and the process for e and closes e.ready.
func (m *Manager) start(ctx context.Context, agent string, e *entry) {
	defer close(e.ready)
	opts := Options{
		Binary:     m.opts.Binary,
		StateDir:   m.opts.StateDir,
		ProfileDir: filepath.Join(m.opts.StateDir, "profiles", agent),
		Headless:   m.opts.Headless,
		Logger:     m.opts.Logger,
	}
	if !m.opts.Headless {
		d, err := StartDisplay(ctx, DisplayOptions{})
		if err != nil {
			e.err = err
			return
		}
		e.display, opts.Display = d, d
	}
	proc, err := Start(ctx, opts)
	if err != nil {
		if e.display != nil {
			e.display.Stop()
			e.display = nil
		}
		e.err = err
		return
	}
	e.proc = proc
	m.wg.Add(1)
	go m.watch(agent, e)
}

// watch marks the entry dead when the browser exits on its own, so the next
// Acquire restarts it, and reports the crash.
func (m *Manager) watch(agent string, e *entry) {
	defer m.wg.Done()
	err := <-e.proc.Wait()
	m.mu.Lock()
	stopping := e.dead
	e.dead = true
	same := m.procs[agent] == e
	m.mu.Unlock()
	if stopping || !same {
		return // a deliberate stop already accounted for it
	}
	if m.opts.Logger != nil {
		m.opts.Logger.Warn("browser exited", "agent", agent, "pid", e.proc.PID, "err", err)
	}
	if e.display != nil {
		e.display.Stop()
	}
	m.event(agent, EventCrash)
}

// Release says the caller is done with the agent's browser. The process stays
// up until IdleTimeout passes with no user.
func (m *Manager) Release(agent string) {
	m.mu.Lock()
	e := m.procs[agent]
	m.mu.Unlock()
	if e != nil {
		m.release(e)
	}
}

func (m *Manager) release(e *entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.refs > 0 {
		e.refs--
	}
	if e.refs == 0 {
		e.idle = m.opts.Now()
	}
}

// Stop shuts one agent's browser down now, whether or not it is idle.
func (m *Manager) Stop(ctx context.Context, agent string) {
	m.mu.Lock()
	e, ok := m.procs[agent]
	if ok {
		delete(m.procs, agent)
	}
	m.mu.Unlock()
	if !ok {
		return
	}
	m.shutdown(ctx, e)
	m.event(agent, EventStop)
}

// sweep stops browsers that have had no user for IdleTimeout.
func (m *Manager) sweep(tick <-chan time.Time) {
	defer m.wg.Done()
	for {
		select {
		case <-m.stop:
			return
		case <-tick:
			m.sweepOnce()
		}
	}
}

func (m *Manager) sweepOnce() {
	now := m.opts.Now()
	var idle []string
	m.mu.Lock()
	for agent, e := range m.procs {
		if e.refs == 0 && !e.idle.IsZero() && now.Sub(e.idle) >= m.opts.IdleTimeout {
			idle = append(idle, agent)
		}
	}
	var stopped []*entry
	for _, agent := range idle {
		stopped = append(stopped, m.procs[agent])
		delete(m.procs, agent)
	}
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), stopGrace+time.Second)
	defer cancel()
	for i, agent := range idle {
		m.shutdown(ctx, stopped[i])
		m.event(agent, EventIdleStop)
	}
}

// shutdown stops a process and its display. The entry is already unpublished.
func (m *Manager) shutdown(ctx context.Context, e *entry) {
	<-e.ready
	m.mu.Lock()
	e.dead = true // tell watch this exit was deliberate
	m.mu.Unlock()
	if e.proc != nil {
		e.proc.Stop(ctx)
	}
	if e.display != nil {
		e.display.Stop()
	}
}

// Close stops every browser and the idle sweep.
func (m *Manager) Close(ctx context.Context) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	agents := make([]string, 0, len(m.procs))
	entries := make([]*entry, 0, len(m.procs))
	for agent, e := range m.procs {
		agents = append(agents, agent)
		entries = append(entries, e)
	}
	m.procs = map[string]*entry{}
	m.mu.Unlock()

	for i, e := range entries {
		m.shutdown(ctx, e)
		m.event(agents[i], EventStop)
	}
	close(m.stop)
	if m.ticker != nil {
		m.ticker.Stop()
	}
	m.wg.Wait()
}

func (m *Manager) event(agent string, kind EventKind) {
	if m.opts.OnEvent != nil {
		m.opts.OnEvent(agent, kind)
	}
}
