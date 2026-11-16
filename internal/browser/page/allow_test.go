package page

import (
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/browser/cdp"
	"holaclaw/internal/browser/proto"
)

func TestHostAllowed(t *testing.T) {
	allow := normaliseAllow([]string{"Example.COM", ".news.example.org:443", "", "  "})
	if len(allow) != 2 {
		t.Fatalf("normalised %v, want two entries", allow)
	}
	cases := []struct {
		host string
		want bool
	}{
		{"example.com", true},
		{"www.example.com", true},
		{"deep.sub.example.com", true},
		{"example.com.", true},
		{"notexample.com", false},
		{"example.com.evil.net", false},
		{"news.example.org", true},
		{"example.org", false},
		{"127.0.0.1", false},
	}
	for _, c := range cases {
		if got := hostAllowed(allow, c.host); got != c.want {
			t.Errorf("hostAllowed(%q) = %v, want %v", c.host, got, c.want)
		}
	}
	// No list means any site (docs/browser.md: unset or empty = no list).
	if !hostAllowed(nil, "anything.example") {
		t.Error("an empty list must allow every host")
	}
}

// An IPv6 literal is written in brackets in a URL and bare in url.Hostname(),
// so the entry is normalised to the bare form or it would never match.
func TestNormaliseAllowIPv6(t *testing.T) {
	cases := []struct {
		entry string
		want  string
	}{
		{"[::1]", "::1"},
		{"[::1]:8080", "::1"},
		{"[2001:DB8::1]:443", "2001:db8::1"},
		{"2001:db8::1", "2001:db8::1"},
		{"[]", ""},
		{"example.com:8443", "example.com"},
	}
	for _, c := range cases {
		got := normaliseAllow([]string{c.entry})
		switch {
		case c.want == "":
			if len(got) != 0 {
				t.Errorf("normaliseAllow(%q) = %v, want nothing", c.entry, got)
			}
		case len(got) != 1 || got[0] != c.want:
			t.Errorf("normaliseAllow(%q) = %v, want [%q]", c.entry, got, c.want)
		}
	}

	allow := normaliseAllow([]string{"[::1]", "[2001:db8::1]:443"})
	for _, host := range []string{"::1", "2001:db8::1"} {
		if !hostAllowed(allow, host) {
			t.Errorf("hostAllowed(%q) = false, want the bracketed entry to match", host)
		}
	}
	if hostAllowed(allow, "::2") {
		t.Error("an unlisted address was allowed")
	}
	// And through the pre-flight, where the URL carries the brackets.
	if err := checkNavigable(allow, "http://[::1]:8080/x"); err != nil {
		t.Errorf("checkNavigable on the listed address = %v", err)
	}
	if err := checkNavigable(allow, "http://[::2]/x"); !isCode(err, proto.ErrNotAllowed) {
		t.Errorf("checkNavigable off the list = %v", err)
	}
}

func TestCheckNavigable(t *testing.T) {
	allow := normaliseAllow([]string{"example.com"})
	cases := []struct {
		url  string
		code proto.ErrorCode
	}{
		{"https://example.com/x", ""},
		{"http://sub.example.com/", ""},
		{"about:blank", ""},
		{"https://evil.example/", proto.ErrNotAllowed},
		{"file:///etc/passwd", proto.ErrBadAction},
		{"javascript:alert(1)", proto.ErrBadAction},
		{"", proto.ErrBadAction},
		{"://nope", proto.ErrBadAction},
	}
	for _, c := range cases {
		err := checkNavigable(allow, c.url)
		if c.code == "" {
			if err != nil {
				t.Errorf("checkNavigable(%q) = %v, want nil", c.url, err)
			}
			continue
		}
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != c.code {
			t.Errorf("checkNavigable(%q) = %v, want %s", c.url, err, c.code)
		}
	}
}

// The allow-list is enforced on the network: a document request for a host
// that is not on the list is failed, not merely discouraged.
func TestRequestPausedBlocksAndContinues(t *testing.T) {
	f := newFakeCDP(t)
	e := testEngineNoBrowser(t)
	s := &session{e: e, agent: "alpha", allow: normaliseAllow([]string{"good.example"})}
	at := &attachment{s: s, conn: f.conn, bs: f.conn.Session(""), tabs: map[string]*tab{}, refs: map[string]elemRef{}}
	s.at = at
	sess := f.conn.Session("S1")
	page := &tab{targetID: "T1", sessionID: "S1", frames: map[string]string{}}
	at.tabs["T1"] = page
	at.order = []string{"T1"}

	at.onRequestPaused(sess, page, cdp.RequestPausedEvent{
		RequestID: "r1", ResourceType: "Document",
		Request: cdp.Request{URL: "https://good.example/page"},
	})
	if _, ok := f.sent("Fetch.continueRequest"); !ok {
		t.Fatalf("an allowed document was not continued: %v", f.methods())
	}

	at.onRequestPaused(sess, page, cdp.RequestPausedEvent{
		RequestID: "r2", ResourceType: "Document",
		Request: cdp.Request{URL: "https://evil.example/page"},
	})
	params, ok := f.sent("Fetch.failRequest")
	if !ok {
		t.Fatalf("a disallowed document was not failed: %v", f.methods())
	}
	var p struct {
		RequestID   string `json:"requestId"`
		ErrorReason string `json:"errorReason"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		t.Fatal(err)
	}
	if p.RequestID != "r2" || p.ErrorReason != "BlockedByClient" {
		t.Fatalf("failRequest params = %+v", p)
	}
	// The tab was not a popup, so nothing is closed.
	if _, ok := f.sent("Target.closeTarget"); ok {
		t.Error("an ordinary tab was closed on a blocked navigation")
	}
}

// A window the page opened that never reached an allowed document is closed:
// window.open is not a way around the list.
func TestRequestPausedClosesBlockedPopup(t *testing.T) {
	f := newFakeCDP(t)
	e := testEngineNoBrowser(t)
	s := &session{e: e, agent: "alpha", allow: normaliseAllow([]string{"good.example"})}
	at := &attachment{s: s, conn: f.conn, bs: f.conn.Session(""), tabs: map[string]*tab{}, refs: map[string]elemRef{}}
	s.at = at
	popup := &tab{targetID: "T2", sessionID: "S2", opener: "T1", frames: map[string]string{}}
	at.tabs["T2"] = popup
	at.order = []string{"T2"}

	at.onRequestPaused(f.conn.Session("S2"), popup, cdp.RequestPausedEvent{
		RequestID: "r1", ResourceType: "Document",
		Request: cdp.Request{URL: "https://evil.example/popup"},
	})
	params, ok := f.sent("Target.closeTarget")
	if !ok {
		t.Fatalf("the blocked popup was not closed: %v", f.methods())
	}
	var p struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		t.Fatal(err)
	}
	if p.TargetID != "T2" {
		t.Fatalf("closed %q, want the popup", p.TargetID)
	}
	if len(at.tabList()) != 0 {
		t.Fatalf("tabs = %v, want none", at.tabList())
	}
}

// Sub-resources are not navigations: they are continued without a decision.
func TestRequestPausedIgnoresSubresources(t *testing.T) {
	f := newFakeCDP(t)
	e := testEngineNoBrowser(t)
	s := &session{e: e, agent: "alpha", allow: normaliseAllow([]string{"good.example"})}
	at := &attachment{s: s, conn: f.conn, bs: f.conn.Session(""), tabs: map[string]*tab{}, refs: map[string]elemRef{}}
	s.at = at
	at.onRequestPaused(f.conn.Session("S1"), nil, cdp.RequestPausedEvent{
		RequestID: "r1", ResourceType: "Image",
		Request: cdp.Request{URL: "https://evil.example/pixel.png"},
	})
	if _, ok := f.sent("Fetch.failRequest"); ok {
		t.Error("an image was failed by the navigation allow-list")
	}
	if _, ok := f.sent("Fetch.continueRequest"); !ok {
		t.Errorf("an image was not continued: %v", f.methods())
	}
}

func TestDocumentPatterns(t *testing.T) {
	if len(documentPatterns) != 1 ||
		documentPatterns[0].ResourceType != "Document" ||
		documentPatterns[0].RequestStage != "Request" {
		t.Fatalf("patterns = %+v", documentPatterns)
	}
}

// Nothing a page said reaches a log line: the handler logs a fixed sentence
// and the agent name, never the URL it blocked.
func TestBlockedNavigationLogsNoURL(t *testing.T) {
	f := newFakeCDP(t)
	var buf syncBuffer
	e := testEngineNoBrowser(t)
	e.log = slog.New(slog.NewTextHandler(&buf, nil))
	s := &session{e: e, agent: "alpha", allow: normaliseAllow([]string{"good.example"})}
	at := &attachment{s: s, conn: f.conn, bs: f.conn.Session(""), tabs: map[string]*tab{}, refs: map[string]elemRef{}}
	s.at = at
	at.onRequestPaused(f.conn.Session("S1"), nil, cdp.RequestPausedEvent{
		RequestID: "r1", ResourceType: "Document",
		Request: cdp.Request{URL: "https://secret.example/path?token=hunter2"},
	})
	time.Sleep(10 * time.Millisecond)
	if got := buf.String(); strings.Contains(got, "secret.example") || strings.Contains(got, "hunter2") {
		t.Fatalf("the log holds page content: %q", got)
	}
}
