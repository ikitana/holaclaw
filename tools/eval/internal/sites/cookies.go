package sites

import (
	"html/template"
	"net/http"
	"regexp"
)

// Task: find the phone number on the contact page. Expected: done, answer
// has the number. Trap: a consent dialog overlays the page and the real
// content starts aria-hidden; a step that reads the page before dismissing
// the banner (or that clicks blindly through it without the dialog's own
// controls) never sees the phone number at all.
var cookiesTmpl = template.Must(template.New("cookies").Parse(head("Example Contact") + `
<div role="dialog" aria-label="Cookie consent" id="consent-banner">
  <p>We use cookies to remember your preferences.</p>
  <button type="button" id="accept" onclick="acceptCookies()">Accept all</button>
  <button type="button" id="manage" onclick="acceptCookies()">Manage</button>
</div>
<div id="content" aria-hidden="true">
  <h1>Contact Example Co.</h1>
  <p>Address: 42 Example Street, Springfield, ST 55555</p>
  <p>Phone: +1-555-0142</p>
</div>
<script>
function acceptCookies(){
  document.getElementById('content').setAttribute('aria-hidden','false');
  document.getElementById('consent-banner').hidden = true;
  history.pushState({consent:'accepted'}, '', location.pathname + location.search);
}
</script>
` + foot))

func registerCookies(mux *http.ServeMux) {
	mux.HandleFunc("GET /cookies/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = cookiesTmpl.Execute(w, nil)
	})
}

func cookiesSite() Site {
	return Site{
		Name:   "cookies",
		Path:   "/cookies/",
		Intent: "Find the phone number on this contact page.",
		Expect: "done, answer has +1-555-0142",
		Answer: regexp.MustCompile(`\+?1[- ]?555[- ]?0142`),
		Check:  func(run string) error { return nil },
	}
}
