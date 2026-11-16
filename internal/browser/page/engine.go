// Package page is the page layer of the browser sidecar (docs/browser.md):
// one Chromium per agent, one CDP connection per process, one active tab per
// agent, and the four things the executor asks for — observe, act, screenshot,
// live view — on top of them. It implements sidecar.Engine; the sidecar server
// above it never touches CDP, and nothing here knows about HTTP, policy or the
// database.
//
// THE RULE of this layer: everything a page says is untrusted content. Names,
// values, titles, URLs and text go into the returned proto structs and nowhere
// else — never into a log line, never into an error Detail, which carries
// fixed words and codes only. The same holds, harder, for live-view input:
// what a person types is not logged by either half.
package page

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"holaclaw/internal/browser/cdp"
	"holaclaw/internal/browser/chromium"
	"holaclaw/internal/browser/proto"
	"holaclaw/internal/browser/sidecar"
)

// The page layer is the sidecar's engine, and only that: the server above it
// holds the socket, this holds Chromium.
var _ sidecar.Engine = (*Engine)(nil)

// Defaults of the layer. They are deliberately not configuration: the sidecar
// has three config keys (docs/browser.md) and none of them is a timing.
const (
	// DefaultActTimeout bounds one action including its settle wait.
	DefaultActTimeout = 15 * time.Second
	// attachTimeout bounds the CDP calls that configure one freshly attached
	// target. A target that cannot be configured is left alone, not retried.
	attachTimeout = 10 * time.Second
	// openTabWait is how long Open waits for the browser's first tab to
	// attach before answering with an empty tab list.
	openTabWait = 10 * time.Second
	// actInterval is the rate limit: at most one action per agent per
	// interval, wall clock (docs/browser.md "pacing is human-like, and rate-limited").
	actInterval = 100 * time.Millisecond
	// settleWindow is how long an action waits for the page to go quiet when
	// nothing navigated. A navigation waits for networkIdle within the
	// action's own timeout instead.
	settleWindow = 250 * time.Millisecond
)

// DefaultPacing is the scale of the human-like delays: 1 is a person, 0 is no
// delay at all, which is what tests set.
const DefaultPacing = 1.0

// Options configures an Engine. Binary is resolved by the caller (chromium.Find)
// so Health can report a missing or too-old browser without starting anything.
type Options struct {
	Binary chromium.Binary
	// StateDir is the sidecar's StateDirectory: profiles and the download
	// quarantine live under it.
	StateDir string
	// Headless opts every browser into --headless=new and starts no display;
	// otherwise the Manager starts one Xvfb per agent and owns it.
	Headless bool
	Logger   *slog.Logger
	// IdleTimeout is how long an unused browser stays up; 0 means the
	// Manager's default.
	IdleTimeout time.Duration
	// Pacing scales the human-like delays of Act. nil means DefaultPacing; a
	// zero value removes them, which is what tests set.
	Pacing *float64
}

// Engine is the page layer for every agent: one chromium.Manager, one CDP
// connection per agent process, one active tab per agent.
type Engine struct {
	opts Options
	log  *slog.Logger
	mgr  *chromium.Manager
	pace float64
	// quarantine is <state>/downloads; each agent gets a subdirectory.
	quarantine string
	// botWallCount is how many bot walls every agent's pages have shown; see
	// botwall.go.
	botWallCount atomic.Int64

	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
}

// New builds the engine and its process manager. It starts no browser: the
// first Open does.
func New(opts Options) (*Engine, error) {
	if opts.StateDir == "" {
		return nil, errors.New("page: a state directory is required")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	pace := DefaultPacing
	if opts.Pacing != nil {
		pace = *opts.Pacing
		if pace < 0 {
			pace = 0
		}
	}
	quarantine := filepath.Join(opts.StateDir, "downloads")
	if err := os.MkdirAll(quarantine, 0o700); err != nil {
		return nil, fmt.Errorf("page: quarantine: %w", err)
	}
	e := &Engine{
		opts:       opts,
		log:        log,
		pace:       pace,
		quarantine: quarantine,
		sessions:   map[string]*session{},
	}
	e.mgr = chromium.NewManager(chromium.ManagerOptions{
		Binary:      opts.Binary,
		StateDir:    opts.StateDir,
		Headless:    opts.Headless,
		IdleTimeout: opts.IdleTimeout,
		Logger:      log,
	})
	return e, nil
}

// Shutdown is the graceful stop the sidecar server calls: every browser and
// every display it started go, every attachment is dropped, and the
// quarantined downloads are deleted — nothing a page gave us outlives the
// process. It is idempotent.
//
// It is not spelled Close(): sidecar.Engine already has Close(ctx, agent) for
// one agent's browser, and a type cannot have both.
func (e *Engine) Shutdown(ctx context.Context) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	sessions := make([]*session, 0, len(e.sessions))
	for _, s := range e.sessions {
		sessions = append(sessions, s)
	}
	e.sessions = map[string]*session{}
	e.mu.Unlock()

	for _, s := range sessions {
		s.detach()
		s.dl.remove()
	}
	e.mgr.Close(ctx)
	return nil
}

// Health reports the resolved binary and the fixed sentence that says why no
// browser can run here. It starts nothing.
func (e *Engine) Health(context.Context) proto.Health {
	h := proto.Health{
		Binary:   e.opts.Binary.Path,
		Browser:  e.opts.Binary.Version,
		Headless: e.opts.Headless,
	}
	switch {
	case e.opts.Binary.Path == "":
		h.Problem = chromium.ErrNotFound.Error() + "; install one or set browser.binary."
	case e.opts.Binary.TooOld():
		h.Problem = fmt.Sprintf("the browser at %s is version %s; holaclaw needs major %d or newer.",
			e.opts.Binary.Path, e.opts.Binary.Version, chromium.MinMajor)
	case !e.opts.Headless:
		if _, err := exec.LookPath("Xvfb"); err != nil {
			h.Problem = chromium.ErrNoXvfb.Error()
		}
	}
	return h
}

// Sessions lists the agents with an open browser and the tabs each one has.
func (e *Engine) Sessions(context.Context) []proto.Session {
	e.mu.Lock()
	sessions := make([]*session, 0, len(e.sessions))
	for _, s := range e.sessions {
		sessions = append(sessions, s)
	}
	e.mu.Unlock()

	out := make([]proto.Session, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.snapshot())
	}
	return out
}

// Open starts the agent's browser or returns the running one, and replaces its
// allow-list with req.Allow from now on. req.Fresh puts it on a page of its own
// (freshTab).
func (e *Engine) Open(ctx context.Context, agent string, req proto.OpenRequest) (proto.Session, error) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return proto.Session{}, errf(proto.ErrNoSession, "closed")
	}
	s := e.sessions[agent]
	if s == nil {
		s = &session{
			e:       e,
			agent:   agent,
			started: time.Now(),
		}
		e.sessions[agent] = s
	}
	e.mu.Unlock()

	s.act.Lock()
	defer s.act.Unlock()

	// A re-Open replaces the list; it applies to the next request of every
	// attached target, since the check is made when a request pauses.
	s.mu.Lock()
	s.allow = normaliseAllow(req.Allow)
	s.mu.Unlock()

	if s.dl == nil {
		dir := filepath.Join(e.quarantine, agent)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return proto.Session{}, errf(proto.ErrBrowser, "quarantine")
		}
		s.dl = newQuarantine(dir, e.log)
	}
	s.dl.sweep()

	at, release, err := s.ensure(ctx)
	if err != nil {
		return proto.Session{}, err
	}
	defer release()
	at.waitForTab(ctx, openTabWait)
	if req.Fresh {
		if err := at.freshTab(ctx); err != nil {
			return proto.Session{}, err
		}
	}
	return s.snapshot(), nil
}

// Close stops the agent's browser and deletes its quarantined downloads.
func (e *Engine) Close(ctx context.Context, agent string) error {
	e.mu.Lock()
	s := e.sessions[agent]
	delete(e.sessions, agent)
	e.mu.Unlock()
	if s == nil {
		return errf(proto.ErrNoSession, "unknown agent")
	}
	s.act.Lock()
	defer s.act.Unlock()
	s.detach()
	e.mgr.Stop(ctx, agent)
	s.dl.remove()
	return nil
}

// Screenshot is one JPEG of the active tab.
func (e *Engine) Screenshot(ctx context.Context, agent string) ([]byte, error) {
	s, err := e.session(agent)
	if err != nil {
		return nil, err
	}
	s.act.Lock()
	defer s.act.Unlock()
	at, release, err := s.ensure(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	t, err := at.activeTab()
	if err != nil {
		return nil, err
	}
	b, err := at.conn.Session(t.sessionID).CaptureScreenshot(ctx, "jpeg", jpegQuality)
	if err != nil {
		return nil, browserErr(ctx, err, "screenshot")
	}
	return b, nil
}

// session returns the agent's open session, or ErrNoSession.
func (e *Engine) session(agent string) (*session, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := e.sessions[agent]; s != nil && !e.closed {
		return s, nil
	}
	return nil, errf(proto.ErrNoSession, "not open")
}

// ── one agent ────────────────────────────────────────────────────────────────

// session is one agent's browser as this layer sees it: the allow-list and the
// quarantine live as long as the session, the attachment only as long as the
// process behind it.
type session struct {
	e       *Engine
	agent   string
	started time.Time
	dl      *quarantine

	// act serialises the engine calls of one agent (sidecar.Engine: "calls for
	// one agent are serialised by the engine"). build serialises rebuilding
	// the attachment, which Stream does outside act.
	act   sync.Mutex
	build sync.Mutex
	last  time.Time // last action, for the rate limit; guarded by act

	streaming atomic.Bool

	// amu guards the activity ring and its subscribers (activity.go). It is
	// its own lock: an entry is recorded after the call it describes has
	// returned, so recording never waits on act.
	amu  sync.Mutex
	acts []proto.Activity
	subs map[chan proto.Activity]struct{}

	// mu guards the fields below. Event handlers take it too, so the critical
	// sections stay short and no CDP call is made while holding it.
	mu    sync.Mutex
	allow []string
	at    *attachment
	// The bot-wall record of the URL the agent is on: the fixed word of the
	// last check (botwall.go).
	botURL  string
	botLast string
}

// ensure returns a live attachment for the agent, building one when the
// browser has just started or was restarted after an idle stop or a crash.
// The returned function releases the process back to the Manager.
func (s *session) ensure(ctx context.Context) (*attachment, func(), error) {
	proc, err := s.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	release := func() { s.e.mgr.Release(s.agent) }

	s.build.Lock()
	defer s.build.Unlock()

	s.mu.Lock()
	at := s.at
	s.mu.Unlock()
	if at != nil && at.proc == proc && at.conn.Err() == nil {
		return at, release, nil
	}
	// A restarted browser: every ref, tab and session id of the old one is
	// gone, so the attachment is rebuilt from scratch.
	s.detach()
	at, err = s.attach(ctx, proc)
	if err != nil {
		release()
		return nil, nil, err
	}
	return at, release, nil
}

// acquire gets the agent's process from the Manager without handing it this
// call's context: chromium.Start ties the browser's lifetime to the context
// Acquire was given, so a caller whose request ends would take the browser
// down with it. The wait is bounded here instead, and a process that arrives
// after the caller gave up is released rather than leaked.
func (s *session) acquire(ctx context.Context) (*chromium.Process, error) {
	type result struct {
		proc *chromium.Process
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		proc, err := s.e.mgr.Acquire(context.WithoutCancel(ctx), s.agent)
		ch <- result{proc, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, startErr(r.err)
		}
		return r.proc, nil
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.err == nil {
				s.e.mgr.Release(s.agent)
			}
		}()
		return nil, errf(proto.ErrTimeout, "start")
	}
}

// attach builds the CDP connection and subscriptions for one browser process.
func (s *session) attach(ctx context.Context, proc *chromium.Process) (*attachment, error) {
	conn := cdp.New(proc.Pipe.Read, proc.Pipe.Write, cdp.Options{Logger: s.e.log})
	at := &attachment{
		s:       s,
		proc:    proc,
		conn:    conn,
		bs:      conn.Session(""),
		tabs:    map[string]*tab{},
		refs:    map[string]elemRef{},
		refDesc: map[string]string{},
	}
	// Published before the first subscription, so a handler that fires while
	// this function is still running recognises its own attachment.
	s.mu.Lock()
	s.at = at
	s.mu.Unlock()

	at.addCancels(
		cdp.On(at.bs, cdp.EventAttachedToTarget, func(ev cdp.AttachedToTargetEvent) {
			at.onAttached("", ev)
		}),
		cdp.On(at.bs, cdp.EventDetachedFromTarget, func(ev cdp.DetachedFromTargetEvent) {
			at.onDetached(ev.SessionID)
		}),
		cdp.On(at.bs, cdp.EventTargetInfoChanged, func(ev cdp.TargetInfoChangedEvent) {
			at.onTargetInfo(ev.TargetInfo)
		}),
		cdp.On(at.bs, cdp.EventTargetDestroyed, func(ev cdp.TargetDestroyedEvent) {
			at.onDestroyed(ev.TargetID)
		}),
		cdp.On(at.bs, cdp.EventDownloadWillBegin, func(ev cdp.DownloadWillBeginEvent) {
			s.dl.willBegin(ev)
		}),
		cdp.On(at.bs, cdp.EventDownloadProgress, func(ev cdp.DownloadProgressEvent) {
			s.dl.progress(context.Background(), at.conn, ev)
		}),
	)

	// Downloads land in the agent's quarantine, named by the browser's own
	// GUID: a suggested file name is page content and never becomes a path.
	if err := at.bs.SetDownloadBehavior(ctx, "allowAndName", "", s.dl.dir, true); err != nil {
		s.detach()
		return nil, browserErr(ctx, err, "downloads")
	}
	if err := at.bs.SetDiscoverTargets(ctx, true); err != nil {
		s.detach()
		return nil, browserErr(ctx, err, "discover")
	}
	// Pause-and-release: a popup is attached, and configured, before it has
	// loaded anything (cdp: measured on Chromium 153).
	if err := at.bs.SetAutoAttach(ctx, true, true); err != nil {
		s.detach()
		return nil, browserErr(ctx, err, "autoattach")
	}
	// Auto-attach only catches targets that appear after it; the tab a fresh
	// browser already has is attached by hand.
	infos, err := at.bs.GetTargets(ctx)
	if err != nil {
		s.detach()
		return nil, browserErr(ctx, err, "targets")
	}
	for _, info := range infos {
		if info.Type != "page" || at.hasTab(info.TargetID) {
			continue
		}
		if sid, err := at.bs.AttachToTarget(ctx, info.TargetID); err == nil {
			at.onAttached("", cdp.AttachedToTargetEvent{SessionID: sid, TargetInfo: info})
		}
	}
	return at, nil
}

// detach cancels every subscription and closes the connection. The browser
// itself belongs to the Manager and is left alone.
func (s *session) detach() {
	s.mu.Lock()
	at := s.at
	s.at = nil
	s.mu.Unlock()
	if at == nil {
		return
	}
	for _, cancel := range at.takeCancels() {
		cancel()
	}
	_ = at.conn.Close()
}

// snapshot is the wire view of the session.
func (s *session) snapshot() proto.Session {
	out := proto.Session{Agent: s.agent, Started: s.started}
	s.mu.Lock()
	at := s.at
	s.mu.Unlock()
	if at != nil {
		out.Tabs = at.tabList()
	}
	return out
}

// allowed reports whether a host may be navigated to.
func (s *session) allowed(host string) bool {
	s.mu.Lock()
	allow := s.allow
	s.mu.Unlock()
	return hostAllowed(allow, host)
}

// ── the attachment ───────────────────────────────────────────────────────────

// attachment is one CDP connection and everything built on it. It is replaced
// wholesale when the browser restarts.
type attachment struct {
	s    *session
	proc *chromium.Process
	conn *cdp.Conn
	bs   *cdp.Session

	mu     sync.Mutex
	tabs   map[string]*tab
	order  []string // target ids, in the order they were attached
	active string
	refs   map[string]elemRef
	// refDesc is how the last observation named each ref — `button "Search"` —
	// for the live view's activity panel alone. Page content, replaced with
	// refs on every observation.
	refDesc map[string]string
	// gen numbers the observations: it is minted into every ref, so a handle
	// from a superseded observation cannot be mistaken for a current one.
	gen uint64
	// prev is what the observation before this one found, so this one can say
	// what the last move put on the screen and what the page has started
	// saying since (observe.go).
	prev    seen
	dialog  string
	tabWake chan struct{}
	// px, py is where the pointer was left: a path starts where the last one
	// ended, as a hand would.
	px, py float64

	// cmu guards the subscription cancels alone: they are appended from the
	// cdp handler goroutines (a new tab, a new iframe) while detach ranges
	// over them, and nothing here may be held while a cancel runs.
	cmu      sync.Mutex
	cancels  []func()
	detached bool
}

// addCancels registers subscription cancels. After detach there is nothing
// left to cancel later, so a subscription that arrives then is cancelled at
// once rather than kept in a list nobody reads again.
func (a *attachment) addCancels(cancels ...func()) {
	a.cmu.Lock()
	if a.detached {
		a.cmu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		return
	}
	a.cancels = append(a.cancels, cancels...)
	a.cmu.Unlock()
}

// takeCancels empties the list and hands it over, so the cancels themselves
// run outside the lock.
func (a *attachment) takeCancels() []func() {
	a.cmu.Lock()
	defer a.cmu.Unlock()
	a.detached = true
	out := a.cancels
	a.cancels = nil
	return out
}

// tab is one page target: the unit the executor observes and acts on.
type tab struct {
	targetID  string
	sessionID string
	opener    string
	navigated bool // a document of this tab has committed

	url     string
	title   string
	loading bool

	// frames are the out-of-process iframes attached below this tab, by
	// session id: they carry candidates of their own and enforce the
	// allow-list on their own documents.
	frames map[string]string

	waiters []chan string // lifecycle names, for settle waits
}

// elemRef is what one opaque ref of the last observation points at. It is
// replaced on every Observe, which is what makes an older ref stale.
type elemRef struct {
	tab     string
	session string
	frame   string
	backend int64
}

func (a *attachment) hasTab(targetID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.tabs[targetID]
	return ok
}

// activeTab is the tab every observation and action applies to.
func (a *attachment) activeTab() (*tab, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t := a.tabs[a.active]; t != nil {
		return t, nil
	}
	return nil, errf(proto.ErrNoSession, "no tab")
}

// tabList is the wire view, active flag included.
func (a *attachment) tabList() []proto.Tab {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]proto.Tab, 0, len(a.order))
	for _, id := range a.order {
		t := a.tabs[id]
		if t == nil {
			continue
		}
		out = append(out, proto.Tab{ID: id, URL: t.url, Title: t.title, Active: id == a.active})
	}
	return out
}

// waitForTab blocks until at least one tab is attached, or d passes.
func (a *attachment) waitForTab(ctx context.Context, d time.Duration) {
	a.waitTabs(ctx, d, func() bool { return len(a.order) > 0 })
}

// waitTabs blocks until ready holds, or d passes, and reports whether it held.
// ready runs with a.mu held and is asked again whenever a tab is attached.
func (a *attachment) waitTabs(ctx context.Context, d time.Duration, ready func() bool) bool {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	for {
		a.mu.Lock()
		ok := ready()
		if a.tabWake == nil {
			a.tabWake = make(chan struct{})
		}
		wake := a.tabWake
		a.mu.Unlock()
		if ok {
			return true
		}
		select {
		case <-wake:
		case <-deadline.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// freshTab is OpenRequest.Fresh: one new about:blank tab becomes the active
// one, and every tab the agent had before is closed. A run that starts here
// starts where no other run left off — not on its page, not with its history
// behind the back button, not under the dialog it left open — while the
// profile, which is the browser's and not the tab's, keeps every login.
//
// The new tab is attached by the browser-level auto-attach like any window;
// the old ones are closed only once it is, so the agent is never without one.
func (a *attachment) freshTab(ctx context.Context) error {
	a.mu.Lock()
	old := append([]string(nil), a.order...)
	a.mu.Unlock()

	id, err := a.bs.CreateTarget(ctx, "about:blank")
	if err != nil {
		return browserErr(ctx, err, "new tab")
	}
	if !a.waitTabs(ctx, openTabWait, func() bool { return a.tabs[id] != nil }) {
		if ctx.Err() != nil {
			return errf(proto.ErrTimeout, "new tab")
		}
		return errf(proto.ErrBrowser, "new tab")
	}

	a.mu.Lock()
	a.active = id
	for _, o := range old {
		a.dropTabLocked(o)
	}
	// A dialog belongs to a page that is about to go.
	a.dialog = ""
	a.mu.Unlock()

	for _, o := range old {
		if err := a.bs.CloseTarget(ctx, o); err != nil {
			a.s.e.log.Warn("browser: close a previous tab", "agent", a.s.agent, "err", err)
		}
	}
	return nil
}

// wakeTabs releases waitForTab. The caller holds a.mu.
func (a *attachment) wakeTabsLocked() {
	if a.tabWake != nil {
		close(a.tabWake)
		a.tabWake = nil
	}
}

// current reports whether this attachment is still the session's: an event
// that arrives after a restart belongs to a browser nobody uses any more.
func (a *attachment) current() bool {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	return a.s.at == a
}

// onAttached configures a freshly attached target. parent is the session id of
// the target that auto-attached it, "" at the browser level.
func (a *attachment) onAttached(parent string, ev cdp.AttachedToTargetEvent) {
	if !a.current() || ev.SessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), attachTimeout)
	defer cancel()

	switch ev.TargetInfo.Type {
	case "page":
		if err := a.setupPage(ctx, ev); err != nil {
			a.s.e.log.Warn("browser: page setup failed", "agent", a.s.agent, "err", err)
		}
	case "iframe":
		if err := a.setupFrame(ctx, parent, ev); err != nil {
			a.s.e.log.Warn("browser: frame setup failed", "agent", a.s.agent, "err", err)
		}
	}
	// Whatever it was, it must not stay paused: the input event that opened it
	// is not acknowledged until it runs.
	if ev.WaitingForDebugger {
		if err := a.conn.Session(ev.SessionID).RunIfWaitingForDebugger(ctx); err != nil {
			a.s.e.log.Warn("browser: release target", "agent", a.s.agent, "err", err)
		}
	}
}

// setupPage turns a new page target into a tab: lifecycle events, the
// allow-list on its documents, and its own auto-attach for iframes.
func (a *attachment) setupPage(ctx context.Context, ev cdp.AttachedToTargetEvent) error {
	info := ev.TargetInfo
	a.mu.Lock()
	if t := a.tabs[info.TargetID]; t != nil {
		t.sessionID = ev.SessionID
		a.mu.Unlock()
		return nil
	}
	t := &tab{
		targetID:  info.TargetID,
		sessionID: ev.SessionID,
		opener:    info.OpenerID,
		url:       info.URL,
		title:     info.Title,
		frames:    map[string]string{},
	}
	a.tabs[info.TargetID] = t
	a.order = append(a.order, info.TargetID)
	// A new window is what a person would be looking at, so it becomes the
	// active tab; a popup the allow-list refuses is closed again below.
	a.active = info.TargetID
	a.wakeTabsLocked()
	a.mu.Unlock()

	sess := a.conn.Session(ev.SessionID)
	if err := sess.PageEnable(ctx); err != nil {
		return err
	}
	if err := sess.SetLifecycleEventsEnabled(ctx, true); err != nil {
		return err
	}
	if err := sess.FetchEnable(ctx, documentPatterns); err != nil {
		return err
	}
	if err := sess.SetAutoAttach(ctx, true, true); err != nil {
		return err
	}
	a.subscribePage(sess, t)
	return nil
}

// setupFrame attaches an out-of-process iframe to its tab: it is a document of
// its own, so the allow-list applies to it too.
func (a *attachment) setupFrame(ctx context.Context, parent string, ev cdp.AttachedToTargetEvent) error {
	a.mu.Lock()
	var owner *tab
	for _, t := range a.tabs {
		if t.sessionID == parent {
			owner = t
			break
		}
		if _, ok := t.frames[parent]; ok {
			owner = t
			break
		}
	}
	if owner != nil {
		owner.frames[ev.SessionID] = ev.TargetInfo.TargetID
	}
	a.mu.Unlock()
	if owner == nil {
		return nil
	}
	sess := a.conn.Session(ev.SessionID)
	if err := sess.FetchEnable(ctx, documentPatterns); err != nil {
		return err
	}
	if err := sess.SetAutoAttach(ctx, true, true); err != nil {
		return err
	}
	a.addCancels(
		cdp.On(sess, cdp.EventRequestPaused, func(ev cdp.RequestPausedEvent) {
			a.onRequestPaused(sess, owner, ev)
		}),
		cdp.On(sess, cdp.EventAttachedToTarget, func(child cdp.AttachedToTargetEvent) {
			a.onAttached(sess.ID(), child)
		}),
	)
	return nil
}

// subscribePage registers the per-tab handlers.
func (a *attachment) subscribePage(sess *cdp.Session, t *tab) {
	a.addCancels(
		cdp.On(sess, cdp.EventLifecycleEvent, func(ev cdp.LifecycleEvent) {
			a.onLifecycle(t, ev)
		}),
		cdp.On(sess, cdp.EventFrameNavigated, func(ev cdp.FrameNavigatedEvent) {
			if ev.Frame.ParentID != "" {
				return
			}
			a.mu.Lock()
			t.url, t.navigated = ev.Frame.URL, true
			a.mu.Unlock()
		}),
		cdp.On(sess, cdp.EventJavaScriptDialogOpening, func(ev cdp.JavaScriptDialogOpeningEvent) {
			// The kind is a closed set; the message is page content and is
			// deliberately dropped here.
			a.mu.Lock()
			a.dialog = ev.Type
			a.mu.Unlock()
		}),
		cdp.On(sess, cdp.EventRequestPaused, func(ev cdp.RequestPausedEvent) {
			a.onRequestPaused(sess, t, ev)
		}),
		cdp.On(sess, cdp.EventAttachedToTarget, func(child cdp.AttachedToTargetEvent) {
			a.onAttached(sess.ID(), child)
		}),
	)
}

// onLifecycle keeps Observation.Loading honest and wakes the settle waits.
func (a *attachment) onLifecycle(t *tab, ev cdp.LifecycleEvent) {
	a.mu.Lock()
	switch ev.Name {
	case "init":
		t.loading = true
	case "load", "networkIdle":
		t.loading = false
	}
	waiters := append([]chan string(nil), t.waiters...)
	a.mu.Unlock()
	for _, ch := range waiters {
		select {
		case ch <- ev.Name:
		default:
		}
	}
}

func (a *attachment) onTargetInfo(info cdp.TargetInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t := a.tabs[info.TargetID]; t != nil {
		t.url, t.title = info.URL, info.Title
	}
}

func (a *attachment) onDetached(sessionID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.tabs {
		delete(t.frames, sessionID)
	}
}

func (a *attachment) onDestroyed(targetID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dropTabLocked(targetID)
}

// dropTabLocked removes a tab and picks a new active one. The caller holds mu.
func (a *attachment) dropTabLocked(targetID string) {
	if _, ok := a.tabs[targetID]; !ok {
		return
	}
	delete(a.tabs, targetID)
	for i, id := range a.order {
		if id == targetID {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
	if a.active == targetID {
		a.active = ""
		if len(a.order) > 0 {
			a.active = a.order[len(a.order)-1]
		}
	}
}

// watch registers a lifecycle listener for one tab and returns its channel and
// a cancel. Used by the settle waits.
func (a *attachment) watch(t *tab) (<-chan string, func()) {
	ch := make(chan string, 16)
	a.mu.Lock()
	t.waiters = append(t.waiters, ch)
	a.mu.Unlock()
	return ch, func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		for i, w := range t.waiters {
			if w == ch {
				t.waiters = append(t.waiters[:i], t.waiters[i+1:]...)
				return
			}
		}
	}
}

// takeDialog returns the open dialog's kind and clears it.
func (a *attachment) takeDialog() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	kind := a.dialog
	a.dialog = ""
	return kind
}

func (a *attachment) dialogKind() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dialog
}

// ── errors ───────────────────────────────────────────────────────────────────

// errf builds the wire error. Detail is fixed words and codes: page content
// never reaches it.
func errf(code proto.ErrorCode, detail string) *proto.Error {
	return &proto.Error{Code: code, Detail: detail}
}

// browserErr classifies a failed CDP call. A cancelled context is the caller's
// timeout, not the browser's fault.
func browserErr(ctx context.Context, err error, what string) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
		return errf(proto.ErrTimeout, what)
	}
	if errors.Is(err, cdp.ErrClosed) {
		return errf(proto.ErrNoSession, what)
	}
	return errf(proto.ErrBrowser, what)
}

// startErr turns a Manager failure into a wire error. The message of a start
// failure can hold a profile path, so only the code travels.
func startErr(err error) error {
	switch {
	case errors.Is(err, chromium.ErrTooManyProcesses):
		return errf(proto.ErrBrowser, "too many browsers")
	case errors.Is(err, chromium.ErrBadAgent):
		return errf(proto.ErrBadAction, "agent name")
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return errf(proto.ErrTimeout, "start")
	default:
		return errf(proto.ErrBrowser, "start")
	}
}

// rawCall is the escape hatch for the two protocol methods internal/browser/cdp
// has no typed helper for (Page.getLayoutMetrics, Browser.cancelDownload). It
// goes through Conn.Call, so the stealth rule still applies.
func rawCall(ctx context.Context, conn *cdp.Conn, sessionID, method string, params, result any) error {
	return conn.Call(ctx, sessionID, method, params, result)
}

// jsonNumber reads a float out of a raw JSON object field, 0 when absent.
func jsonNumber(m map[string]json.RawMessage, key string) float64 {
	var f float64
	if raw, ok := m[key]; ok {
		_ = json.Unmarshal(raw, &f)
	}
	return f
}
