// Package sites serves the synthetic web sites the browser eval drives
// (docs/browser.md). Handler mounts one net/http mux with a
// distinct path prefix per site; every page is self-contained HTML with
// inline CSS/JS only — no external requests, no fonts, no images bigger than
// a 1x1 data URI. Server-side state is in-memory and keyed by the ?run=<id>
// query the runner passes on every start URL, so parallel eval runs never
// share data; Reset clears one run's state between attempts.
//
// Only invented data lives here, per tools/eval/README.md's hard rule: example
// names, 555 numbers, fake addresses and fake credentials. Nothing here talks
// to a real network or a real service.
package sites

import (
	"net/http"
	"regexp"
)

// Handler returns the full mux: one prefix per site below, mountable on an
// httptest.Server or a loopback listener.
func Handler() http.Handler {
	mux := http.NewServeMux()
	registerLogin(mux)
	registerCookies(mux)
	registerForm(mux)
	registerSearch(mux)
	registerTable(mux)
	registerInject(mux)
	registerCheckout(mux)
	registerDownload(mux)
	registerCaptcha(mux)
	registerPopup(mux)
	registerAutocomplete(mux)
	return mux
}

// Site describes one task the eval harness can hand to the browser executor
// (browser.Task.Intent), plus what a passing run must look like.
type Site struct {
	Name   string // short id, matches the path prefix
	Path   string // starting path, relative, without the run query
	Intent string // the task, in the words the runner gives browser.do
	Expect string // the expected outcome, for a human reading the report

	// Blocker is non-"" when success without a takeover requires stopping at
	// this blocker (browser.Blocker's closed set).
	Blocker string
	// Risk is non-"" when the task's key step must be classified with this
	// risk and go through approval (browser.RiskKind's closed set).
	Risk string

	// Answer, when set, is what the extracted answer of a passing run must
	// match. Only the sites whose answer does not depend on the run id carry
	// one; the rest are judged by Check alone.
	Answer *regexp.Regexp

	// Check inspects this site's per-run server state after a run and
	// reports whether it matches Expect; nil state (a site with none) is
	// always fine.
	Check func(run string) error
}

// Sites lists every synthetic task, in the order of the M9 plan bullet.
func Sites() []Site {
	return []Site{
		loginSite(),
		cookiesSite(),
		formSite(),
		searchSite(),
		tableSite(),
		injectSite(),
		checkoutSite(),
		downloadSite(),
		captchaSite(),
		popupSite(),
		autocompleteSite(),
	}
}
