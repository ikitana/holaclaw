package page

import (
	"context"
	"time"

	"holaclaw/internal/browser/proto"
)

// Waiting for a page, as ActWait means it (docs/browser.md). Nothing here is
// a fixed sleep the caller hopes is long enough: the browser says when it is
// done, in two stages.
//
//  1. Chromium's own lifecycle events — "load", then "networkIdle" — which is
//     what tells a navigation from a page that has already arrived. A tab that
//     is not loading passes this stage without waiting at all.
//  2. A quiet window: the document is read twice, and the page is quiet when
//     readyState is "complete" and nothing has changed between the two
//     readings — no resource arrived, no text and no element was added. This
//     is the stage that catches what a lifecycle event cannot, because
//     Chromium fires those once per document: the XHR after load, the list
//     that fills in, the skeleton that turns into content.
//
// The reading is made in an isolated world (never the main world, never
// Runtime.enable — docs/browser.md's stealth rule), and it asks the page for nothing it
// does not already publish about itself.
const (
	// quietWindow is how far apart the two readings are: the page is quiet
	// when it did not change across it. Chromium's own networkIdle uses half a
	// second of no connections; this is the same idea, measured on the
	// document, and it ends as soon as the page has stopped.
	quietWindow = 250 * time.Millisecond
	// changeWindow is how often a WaitChange wait looks for the first sign of
	// life. It is a sampling rate, not a wait: the wait ends when something
	// arrives.
	changeWindow = 50 * time.Millisecond
	// minQuietWindow is how short the quiet window may be squeezed by a caller
	// in a hurry. Below this two readings are so close together that a page
	// mid-change looks settled.
	minQuietWindow = 40 * time.Millisecond
)

// window is the gap between the two readings that have to agree. It is
// quietWindow, or half of what little time the caller gave us — a 250 ms
// wait that spent all 250 ms on one window could never confirm anything, and
// would always cost its whole bound; half of it lets a settled page come back
// at once and still leaves room for a second look.
func window(ctx context.Context) time.Duration {
	dl, ok := ctx.Deadline()
	if !ok {
		return quietWindow
	}
	half := time.Until(dl) / 2
	switch {
	case half >= quietWindow:
		return quietWindow
	case half < minQuietWindow:
		return minQuietWindow
	}
	return half
}

// pageState is what one reading of the document says about whether it is still
// filling in. Every field is a count or a fixed word, never page text.
type pageState struct {
	Ready     string `json:"ready"`
	Resources int    `json:"resources"`
	Text      int    `json:"text"`
	Elements  int    `json:"elements"`
}

// done reports whether a page in this state has finished arriving.
func (s pageState) done() bool { return s.Ready == "complete" }

// stateExpr reads the four numbers above. It touches nothing a page can be
// surprised by: readyState, its own resource timeline, and the size of the
// document.
const stateExpr = `(() => {
  const b = document.body;
  return {
    ready: document.readyState,
    resources: performance.getEntriesByType('resource').length,
    text: b ? b.textContent.length : 0,
    elements: document.getElementsByTagName('*').length,
  };
})()`

// waitQuiet is ActWait. It returns true when the page reached the state that
// was asked for, false when ctx — the action's own timeout — came first.
func (e *Engine) waitQuiet(ctx context.Context, at *attachment, t *tab, lifecycle <-chan string, until proto.WaitUntil) bool {
	if !at.waitLoaded(ctx, t, lifecycle) {
		return false
	}
	frame, err := at.conn.Session(t.sessionID).GetFrameTree(ctx)
	if err != nil {
		// No frame to read: the lifecycle stage is the whole of the answer.
		return ctx.Err() == nil
	}
	read := func() (pageState, bool) {
		var s pageState
		if err := at.conn.EvalIsolated(ctx, t.sessionID, frame.Frame.ID, stateExpr, &s); err != nil {
			return pageState{}, false
		}
		return s, true
	}

	prev, ok := read()
	if !ok {
		return ctx.Err() == nil
	}
	if until == proto.WaitChange {
		for {
			if !sleepUntil(ctx, changeWindow) {
				return false
			}
			cur, ok := read()
			if !ok {
				return false
			}
			if cur != prev {
				prev = cur
				break
			}
		}
	}
	quiet := window(ctx)
	for {
		if !sleepUntil(ctx, quiet) {
			return false
		}
		cur, ok := read()
		if !ok {
			return false
		}
		if cur == prev && cur.done() {
			return true
		}
		prev = cur
	}
}

// waitLoaded blocks while the tab is mid-navigation, waking on every lifecycle
// event the tab reports; onLifecycle is what keeps t.loading honest. A tab that
// is not loading returns at once, which is what makes a wait on a page that has
// already arrived cost nothing.
func (a *attachment) waitLoaded(ctx context.Context, t *tab, lifecycle <-chan string) bool {
	for {
		a.mu.Lock()
		loading := t.loading
		a.mu.Unlock()
		if !loading {
			return true
		}
		select {
		case <-lifecycle:
		case <-ctx.Done():
			return false
		}
	}
}

// sleepUntil waits d, or reports false when the action's timeout came first.
func sleepUntil(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
