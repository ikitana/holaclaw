package sites

import (
	"net/http"
	"net/url"
	"strings"
)

// runOf returns the run id a request carries: the ?run= query parameter that
// every link and form action in this package sets explicitly. "" means the
// caller forgot to scope the run, which every handler treats as "no state".
func runOf(r *http.Request) string {
	return r.URL.Query().Get("run")
}

// withRun appends run as a query parameter, so links and form actions
// between pages of the same site stay scoped to it.
func withRun(path, run string) string {
	if run == "" {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "run=" + url.QueryEscape(run)
}

// last4 is the trap answer for the captcha site: the last 4 characters of
// run, or all of it when shorter. Deterministic, so tests and a takeover
// person both can compute it without hitting the server twice.
func last4(run string) string {
	if len(run) <= 4 {
		return run
	}
	return run[len(run)-4:]
}

// siteCSS is the one stylesheet shared by every page: enough to make labels,
// buttons and tables legible, nothing more. [aria-hidden="true"] backs the
// cookie banner's content hiding.
const siteCSS = `
body{font-family:system-ui,sans-serif;max-width:40rem;margin:2rem auto;padding:0 1rem;line-height:1.5;color:#1a1a1a}
label{display:block;margin-top:.75rem;font-weight:600}
input,button{font-size:1rem;padding:.4rem .6rem;margin-top:.25rem}
button{cursor:pointer}
table{border-collapse:collapse;width:100%}
th,td{border:1px solid #ccccdd;padding:.35rem .6rem;text-align:left}
.error{color:#b00020}
nav a{margin-right:.75rem}
[aria-hidden="true"]{display:none}
[role="dialog"]{border:1px solid #888;padding:1rem;margin-top:1rem;max-width:24rem}
`

// head opens the HTML document for title; every page is otherwise its own
// html/template so that the invented data it fills in is escaped.
func head(title string) string {
	return "<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">" +
		"<title>" + title + "</title><style>" + siteCSS + "</style></head><body>\n"
}

// foot closes what head opened.
const foot = "\n</body></html>\n"

// tiny1x1 is a 1x1 transparent PNG as a data URI: the only "image" any site
// here ever uses (never an external request, never a real photo).
const tiny1x1 = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
