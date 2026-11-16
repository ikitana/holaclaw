package page

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"time"

	"holaclaw/internal/browser/cdp"
	"holaclaw/internal/browser/chromium"
	"holaclaw/internal/browser/proto"
)

// Acting is input, never script (docs/browser.md): every step below ends in an
// Input.dispatch* event, which is what makes the page see isTrusted = true.
// Pacing — a curved pointer path, key jitter, a dwell before the press — is
// here rather than in cdp, and scales with Options.Pacing so tests can switch
// it off.

// Act runs one action on the active tab and reports what it did. What it did
// is also recorded as an Activity for whoever is watching the live view
// (activity.go) — without the text it typed or the key it pressed.
func (e *Engine) Act(ctx context.Context, agent string, action proto.Action) (proto.ActResult, error) {
	start := time.Now()
	s, err := e.session(agent)
	if err != nil {
		return fail(proto.ActResult{}, start, err)
	}
	// The ref is named from the observation that minted it, so the name is
	// read before the action can supersede it.
	target := ""
	s.mu.Lock()
	at := s.at
	s.mu.Unlock()
	if at != nil {
		target = at.describeRef(action.Ref)
	}
	res, err := e.act(ctx, s, action)
	s.record(actActivity(start, action, res, target))
	return res, err
}

// act is Act on a session that exists.
func (e *Engine) act(ctx context.Context, s *session, action proto.Action) (proto.ActResult, error) {
	start := time.Now()
	res := proto.ActResult{}

	s.act.Lock()
	defer s.act.Unlock()

	timeout := DefaultActTimeout
	if action.TimeoutMS > 0 {
		timeout = time.Duration(action.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	at, release, err := s.ensure(ctx)
	if err != nil {
		return fail(res, start, err)
	}
	defer release()

	t, err := at.activeTab()
	if err != nil {
		return fail(res, start, err)
	}
	sess := at.conn.Session(t.sessionID)

	// An open dialog blocks the renderer, so it is dismissed before anything
	// else: refused, with no prompt text (docs/browser.md — nothing is typed on a page
	// unless the executor asked for it).
	if kind := at.takeDialog(); kind != "" {
		if err := sess.HandleJavaScriptDialog(ctx, false, ""); err != nil {
			return fail(res, start, browserErr(ctx, err, "dialog"))
		}
	}

	s.rateLimit(ctx)

	before := at.tabURL(ctx, t)
	mark := s.dl.mark()
	lifecycle, unwatch := at.watch(t)
	defer unwatch()

	navigating, err := e.run(ctx, at, s, t, sess, action)
	if err != nil {
		res.URL = at.tabURL(ctx, t)
		return fail(res, start, err)
	}
	if action.Kind == proto.ActWait {
		// A wait is the one action whose whole job is the waiting, so it gets
		// the real thing rather than settle's short window.
		res.Quiet = e.waitQuiet(ctx, at, t, lifecycle, action.Until)
	} else {
		at.settle(ctx, lifecycle, navigating)
	}

	// A client-side redirect (a meta refresh, location.href) is blocked on the
	// wire like any other, but the tab still commits Chromium's error page for
	// it and keeps the off-list URL. Reporting OK there would hand the
	// executor an error page to decide on, so the settled URL is checked
	// against the list again and the tab is taken back to somewhere allowed.
	if ended := at.tabURL(ctx, t); !s.resultAllowed(ended) {
		e.leaveDisallowed(ctx, at, sess, t)
		res.URL = at.tabURL(ctx, t)
		res.Downloads = s.dl.since(mark)
		return fail(res, start, errf(proto.ErrNotAllowed, "host"))
	}

	// A bot wall or Chromium's own error page is recognised right after a
	// navigation, so the record is of where the action actually landed.
	if navigating {
		e.checkBotWall(ctx, at, s, t)
	}

	res.OK = true
	res.URL = at.tabURL(ctx, t)
	res.Navigated = res.URL != before
	res.Downloads = s.dl.since(mark)
	res.TookMS = time.Since(start).Milliseconds()
	return res, nil
}

// run performs one action and says whether it started a navigation.
func (e *Engine) run(ctx context.Context, at *attachment, s *session, t *tab, sess *cdp.Session, a proto.Action) (bool, error) {
	switch a.Kind {
	case proto.ActNavigate:
		return true, e.navigate(ctx, s, sess, a.URL)
	case proto.ActClick:
		return false, e.click(ctx, at, sess, a.Ref)
	case proto.ActType:
		return false, e.typeText(ctx, at, sess, a)
	case proto.ActKey:
		return false, e.key(ctx, at, sess, a)
	case proto.ActScroll:
		return false, e.scroll(ctx, at, sess, a)
	case proto.ActWait:
		return false, nil // waitQuiet does the waiting, and it is not a navigation
	case proto.ActBack:
		return true, e.back(ctx, sess)
	default:
		return false, errf(proto.ErrBadAction, "kind")
	}
}

// navigate refuses a URL outside the allow-list before a request is made. The
// network enforcement in allow.go still stands behind this: it is what catches
// redirects, popups and iframes.
func (e *Engine) navigate(ctx context.Context, s *session, sess *cdp.Session, raw string) error {
	s.mu.Lock()
	allow := s.allow
	s.mu.Unlock()
	if err := checkNavigable(allow, raw); err != nil {
		return err
	}
	_, errText, err := sess.Navigate(ctx, raw)
	if err != nil {
		// The protocol error text is Chromium's own, never the page's.
		e.log.Warn("browser: navigate failed", "agent", s.agent, "err", err)
		return browserErr(ctx, err, "navigate")
	}
	switch {
	case errText == "":
		return nil
	case strings.Contains(errText, "BLOCKED_BY_CLIENT"):
		return errf(proto.ErrNotAllowed, "blocked")
	case strings.Contains(errText, "ABORTED"):
		return nil // a download, or a navigation the page replaced
	default:
		return errf(proto.ErrBrowser, "navigate")
	}
}

// click aims at the centre of the element's first content quad, moves there
// along a curve, dwells, then presses and releases.
func (e *Engine) click(ctx context.Context, at *attachment, sess *cdp.Session, ref string) error {
	r, err := at.resolve(ref)
	if err != nil {
		return err
	}
	return e.clickNode(ctx, at, sess, at.conn.Session(r.session), cdp.NodeRef{BackendNodeID: r.backend})
}

// clickNode is the click itself, on a node the executor's ref resolved to.
// target is the session the node's document belongs to — an iframe has one of
// its own — and sess is the tab the input is dispatched to.
func (e *Engine) clickNode(ctx context.Context, at *attachment, sess, target *cdp.Session, node cdp.NodeRef) error {
	if err := target.ScrollIntoViewIfNeeded(ctx, node); err != nil {
		return nodeErr(ctx, err, "scroll")
	}
	quads, err := target.GetContentQuads(ctx, node)
	if err != nil {
		return nodeErr(ctx, err, "quads")
	}
	if len(quads) == 0 {
		return errf(proto.ErrStaleRef, "no box")
	}
	x, y := quads[0].Center()
	if err := e.movePointer(ctx, at, sess, x, y); err != nil {
		return browserErr(ctx, err, "move")
	}
	e.pause(ctx, 60*time.Millisecond)
	for _, ev := range []cdp.MouseEvent{
		{Type: "mousePressed", X: x, Y: y, Button: "left", Buttons: 1, ClickCount: 1},
		{Type: "mouseReleased", X: x, Y: y, Button: "left", Buttons: 0, ClickCount: 1},
	} {
		if err := sess.DispatchMouseEvent(ctx, ev); err != nil {
			return browserErr(ctx, err, "click")
		}
		e.pause(ctx, 40*time.Millisecond)
	}
	return nil
}

// typeText focuses the element, optionally clears it, then commits the text in
// chunks with key-timing jitter.
func (e *Engine) typeText(ctx context.Context, at *attachment, sess *cdp.Session, a proto.Action) error {
	r, err := at.resolve(a.Ref)
	if err != nil {
		return err
	}
	target := at.conn.Session(r.session)
	node := cdp.NodeRef{BackendNodeID: r.backend}
	if err := target.ScrollIntoViewIfNeeded(ctx, node); err != nil {
		return nodeErr(ctx, err, "scroll")
	}
	if err := target.Focus(ctx, node); err != nil {
		return nodeErr(ctx, err, "focus")
	}
	if a.Clear {
		// Select all and delete with real key events: the field's own
		// handlers run exactly as they would for a person.
		if err := dispatchKeys(ctx, sess, keyEvents(namedKey{key: "a", code: "KeyA", vk: 65, text: "a"}, modCtrl)); err != nil {
			return browserErr(ctx, err, "clear")
		}
		del, _ := lookupKey("delete")
		if err := dispatchKeys(ctx, sess, keyEvents(del, 0)); err != nil {
			return browserErr(ctx, err, "clear")
		}
		e.pause(ctx, 40*time.Millisecond)
	}
	for _, chunk := range chunks(a.Text, typeChunk) {
		if err := sess.InsertText(ctx, chunk); err != nil {
			return browserErr(ctx, err, "type")
		}
		e.pause(ctx, time.Duration(60+rand.IntN(70))*time.Millisecond)
	}
	if a.Submit {
		enter, _ := lookupKey("enter")
		if err := dispatchKeys(ctx, sess, keyEvents(enter, 0)); err != nil {
			return browserErr(ctx, err, "submit")
		}
	}
	return nil
}

// key presses one named key. An unknown name is refused: model text never
// becomes a key code.
func (e *Engine) key(ctx context.Context, at *attachment, sess *cdp.Session, a proto.Action) error {
	k, ok := lookupKey(a.Key)
	if !ok {
		return errf(proto.ErrBadAction, "key")
	}
	if a.Ref != "" {
		r, err := at.resolve(a.Ref)
		if err != nil {
			return err
		}
		if err := at.conn.Session(r.session).Focus(ctx, cdp.NodeRef{BackendNodeID: r.backend}); err != nil {
			return nodeErr(ctx, err, "focus")
		}
	}
	if err := dispatchKeys(ctx, sess, keyEvents(k, 0)); err != nil {
		return browserErr(ctx, err, "key")
	}
	return nil
}

// scroll sends a wheel event at the ref's centre, or at the middle of the
// viewport when there is no ref.
func (e *Engine) scroll(ctx context.Context, at *attachment, sess *cdp.Session, a proto.Action) error {
	x, y := 0.0, 0.0
	if a.Ref != "" {
		r, err := at.resolve(a.Ref)
		if err != nil {
			return err
		}
		quads, err := at.conn.Session(r.session).GetContentQuads(ctx, cdp.NodeRef{BackendNodeID: r.backend})
		if err != nil {
			return nodeErr(ctx, err, "quads")
		}
		if len(quads) == 0 {
			return errf(proto.ErrStaleRef, "no box")
		}
		x, y = quads[0].Center()
	} else {
		v := viewport(ctx, at.conn, sess.ID())
		if v.empty() {
			v = rect{w: chromium.DefaultWindowWidth, h: chromium.DefaultWindowHeight}
		}
		x, y = v.w/2, v.h/2
	}
	// The pointer moves to where it scrolls first: measured on Chromium 153, a
	// wheel event over a point the pointer never visited does not scroll.
	if err := e.movePointer(ctx, at, sess, x, y); err != nil {
		return browserErr(ctx, err, "move")
	}
	// Both deltas go on the wire even when one is zero (cdp: measured).
	err := sess.DispatchMouseEvent(ctx, cdp.MouseEvent{
		Type: "mouseWheel", X: x, Y: y, DeltaX: 0, DeltaY: float64(a.DeltaY),
	})
	if err != nil {
		return browserErr(ctx, err, "scroll")
	}
	return nil
}

// back walks one entry down the navigation history.
func (e *Engine) back(ctx context.Context, sess *cdp.Session) error {
	current, entries, err := sess.GetNavigationHistory(ctx)
	if err != nil {
		return browserErr(ctx, err, "history")
	}
	if current <= 0 || current >= len(entries) {
		return errf(proto.ErrBadAction, "no history")
	}
	if err := sess.NavigateToHistoryEntry(ctx, entries[current-1].ID); err != nil {
		return browserErr(ctx, err, "history")
	}
	return nil
}

// ── pacing and plumbing ──────────────────────────────────────────────────────

const (
	// typeChunk is how many bytes one InsertText carries: a few characters at
	// a time, the way a person's input method commits them.
	typeChunk = 6
	// modCtrl is CDP's modifier bit for control.
	modCtrl = 2
	// pointerSteps is how many mouseMoved events a pointer path has.
	pointerSteps = 5
)

// pause sleeps for a delay scaled by Options.Pacing. Pacing 0 — what tests set
// — returns at once.
func (e *Engine) pause(ctx context.Context, d time.Duration) {
	if e.pace <= 0 {
		return
	}
	// ±25% of jitter, so the rhythm is not a fingerprint of its own.
	scaled := time.Duration(float64(d) * e.pace * (0.75 + rand.Float64()/2))
	timer := time.NewTimer(scaled)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// rateLimit holds one agent to at most one action per actInterval of wall
// clock, whatever the pacing. The caller holds s.act.
func (s *session) rateLimit(ctx context.Context) {
	if !s.last.IsZero() {
		if wait := actInterval - time.Since(s.last); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
			timer.Stop()
		}
	}
	s.last = time.Now()
}

// movePointer walks a curved path to the target, a few events at a time.
func (e *Engine) movePointer(ctx context.Context, at *attachment, sess *cdp.Session, x, y float64) error {
	at.mu.Lock()
	fx, fy := at.px, at.py
	at.px, at.py = x, y
	at.mu.Unlock()
	if fx == 0 && fy == 0 {
		fx, fy = x-140, y-90 // the pointer comes from somewhere, not from nowhere
	}
	// One control point off the straight line: a hand does not move on a ruler.
	cx := (fx+x)/2 + (y-fy)*0.12
	cy := (fy+y)/2 - (x-fx)*0.12
	for i := 1; i <= pointerSteps; i++ {
		t := float64(i) / pointerSteps
		u := 1 - t
		px := u*u*fx + 2*u*t*cx + t*t*x
		py := u*u*fy + 2*u*t*cy + t*t*y
		err := sess.DispatchMouseEvent(ctx, cdp.MouseEvent{Type: "mouseMoved", X: px, Y: py, Button: "none"})
		if err != nil {
			return err
		}
		e.pause(ctx, 12*time.Millisecond)
	}
	return nil
}

// dispatchKeys sends a down/up pair.
func dispatchKeys(ctx context.Context, sess *cdp.Session, evs []cdp.KeyEvent) error {
	for _, ev := range evs {
		if err := sess.DispatchKeyEvent(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}

// chunks splits text into pieces of at most n bytes, never inside a rune.
func chunks(s string, n int) []string {
	var out []string
	for len(s) > 0 {
		cut := n
		if cut >= len(s) {
			out = append(out, s)
			break
		}
		for cut > 0 && !isRuneStart(s[cut]) {
			cut--
		}
		if cut == 0 {
			cut = len(s)
		}
		out = append(out, s[:cut])
		s = s[cut:]
	}
	return out
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// resolve turns an opaque ref into the node of the last observation. A ref
// from an older observation is stale by construction: the map is replaced.
func (a *attachment) resolve(ref string) (elemRef, error) {
	if ref == "" {
		return elemRef{}, errf(proto.ErrBadAction, "ref")
	}
	a.mu.Lock()
	r, ok := a.refs[ref]
	a.mu.Unlock()
	if !ok {
		return elemRef{}, errf(proto.ErrStaleRef, "unknown ref")
	}
	return r, nil
}

// tabURL asks the browser process where the tab is; it answers even while the
// renderer is busy.
func (a *attachment) tabURL(ctx context.Context, t *tab) string {
	if info, err := a.bs.GetTargetInfo(ctx, t.targetID); err == nil {
		a.mu.Lock()
		t.url, t.title = info.URL, info.Title
		a.mu.Unlock()
		return info.URL
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return t.url
}

// settle waits for the page to go quiet after an action: a navigation waits
// for networkIdle within the action's own timeout, anything else waits a short
// window and returns as soon as the page reports idle.
func (a *attachment) settle(ctx context.Context, lifecycle <-chan string, navigating bool) {
	window := settleWindow
	if navigating {
		window = time.Hour // bounded by ctx, which carries the action's timeout
	}
	timer := time.NewTimer(window)
	defer timer.Stop()
	loaded := !navigating
	for {
		select {
		case name := <-lifecycle:
			switch name {
			case "load":
				loaded = true
			case "networkIdle":
				if loaded {
					return
				}
			}
		case <-timer.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// fail fills the result with the code the executor acts on and returns the
// same failure as an error for the HTTP layer. Detail is fixed words only.
func fail(res proto.ActResult, start time.Time, err error) (proto.ActResult, error) {
	var pe *proto.Error
	if !errors.As(err, &pe) {
		pe = errf(proto.ErrBrowser, "act")
	}
	res.OK = false
	res.Code = pe.Code
	res.Detail = pe.Detail
	res.TookMS = time.Since(start).Milliseconds()
	return res, pe
}

// nodeErr is browserErr for the node methods: a node that is gone means the
// observation the ref came from no longer describes the page.
func nodeErr(ctx context.Context, err error, what string) error {
	var pe *cdp.Error
	if errors.As(err, &pe) && strings.Contains(strings.ToLower(pe.Message), "node") {
		return errf(proto.ErrStaleRef, what)
	}
	return browserErr(ctx, err, what)
}
