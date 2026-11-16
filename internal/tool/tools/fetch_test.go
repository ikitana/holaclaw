package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"holaclaw/internal/tool"
)

func runTool(t *testing.T, tl tool.Tool, args string) tool.Result {
	t.Helper()
	res, err := tl.Run(context.Background(), tool.Call{
		CallID: "c1", Name: tl.Spec().Name, Args: json.RawMessage(args), Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("%s returned a Go error: %v", tl.Spec().Name, err)
	}
	return res
}

func fetchArgs(t *testing.T, u string, maxBytes int) string {
	t.Helper()
	m := map[string]any{"url": u}
	if maxBytes > 0 {
		m["max_bytes"] = maxBytes
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestClassifyIP(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1":              "loopback address",
		"127.1.2.3":              "loopback address",
		"::1":                    "loopback address",
		"::ffff:127.0.0.1":       "loopback address",
		"10.0.0.1":               "private address",
		"::ffff:10.0.0.1":        "private address",
		"172.16.5.5":             "private address",
		"172.31.255.255":         "private address",
		"192.168.1.1":            "private address",
		"169.254.169.254":        "link-local address",
		"::ffff:169.254.169.254": "link-local address",
		"fe80::1":                "link-local address",
		"100.64.0.1":             "carrier-grade NAT address",
		"100.127.255.255":        "carrier-grade NAT address",
		"fc00::1":                "unique-local address",
		"fd00::abcd":             "unique-local address",
		"0.0.0.0":                "unspecified address",
		"::":                     "unspecified address",
		"224.0.0.1":              "multicast address",
		"ff02::1":                "multicast address",
		// Public addresses must pass.
		"93.184.216.34":   "",
		"8.8.8.8":         "",
		"172.32.0.1":      "",
		"100.128.0.1":     "",
		"2001:4860::8888": "",
	}
	for in, want := range cases {
		ip, err := netip.ParseAddr(in)
		if err != nil {
			t.Fatalf("bad test address %q: %v", in, err)
		}
		if got := classifyIP(ip); got != want {
			t.Errorf("classifyIP(%s) = %q, want %q", in, got, want)
		}
	}
	if got := classifyIP(netip.Addr{}); got != "invalid address" {
		t.Errorf("classifyIP(zero) = %q", got)
	}
}

func TestFetchHTMLToTextAndTaint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, `<html><head><title>x</title><style>p{color:red}</style></head>
<body><script>var secret=1;</script><h1>Title</h1><p>Hello&nbsp;&amp; welcome</p></body></html>`)
	}))
	defer srv.Close()

	f := newFetch(true) // TESTS ONLY: reach loopback
	res := runTool(t, f, fetchArgs(t, srv.URL, 0))
	if res.IsError {
		t.Fatalf("fetch failed: %s", res.Text)
	}
	if !res.Tainted {
		t.Error("fetch result is not tainted")
	}
	if !strings.Contains(res.Text, "Title") || !strings.Contains(res.Text, "Hello & welcome") {
		t.Errorf("stripped text = %q", res.Text)
	}
	for _, bad := range []string{"secret", "color:red", "<h1>", "<title>"} {
		if strings.Contains(res.Text, bad) {
			t.Errorf("stripped text still contains %q: %q", bad, res.Text)
		}
	}
}

func TestFetchNonTextRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte{0x89, 'P', 'N', 'G'}) //nolint:errcheck // test server
	}))
	defer srv.Close()

	res := runTool(t, newFetch(true), fetchArgs(t, srv.URL, 0))
	if !res.IsError || !strings.Contains(res.Text, "image/png") {
		t.Errorf("non-text fetch = %q (err=%v)", res.Text, res.IsError)
	}
	if !res.Tainted {
		t.Error("failed fetch is not tainted")
	}
}

func TestFetchSizeCapAndStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, strings.Repeat("x", 100<<10))
	}))
	defer srv.Close()

	f := newFetch(true)
	res := runTool(t, f, fetchArgs(t, srv.URL, 100))
	if res.IsError {
		t.Fatalf("fetch failed: %s", res.Text)
	}
	if !strings.Contains(res.Text, "[response cut at 100 bytes]") || len(res.Text) > 200 {
		t.Errorf("size cap = %q (len %d)", res.Text, len(res.Text))
	}
	res = runTool(t, f, fetchArgs(t, srv.URL+"/missing", 0))
	if !res.IsError || !strings.Contains(res.Text, "404") {
		t.Errorf("404 fetch = %q (err=%v)", res.Text, res.IsError)
	}
}

func TestFetchSchemeRefused(t *testing.T) {
	f := newFetch(true)
	for _, u := range []string{"file:///etc/passwd", "ftp://example.com/x", "gopher://example.com", "data:text/plain,hi"} {
		res := runTool(t, f, fetchArgs(t, u, 0))
		if !res.IsError || !strings.Contains(res.Text, "only http and https") {
			t.Errorf("fetch(%q) = %q (err=%v)", u, res.Text, res.IsError)
		}
	}
	if res := runTool(t, f, fetchArgs(t, "http://", 0)); !res.IsError {
		t.Errorf("fetch of a hostless URL succeeded: %q", res.Text)
	}
	if res := runTool(t, f, `{"url":"http://example.com"`+"}x"); !res.IsError {
		t.Errorf("bad JSON arguments accepted: %q", res.Text)
	}
}

func TestFetchPrivateAddressRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "should never be read")
	}))
	defer srv.Close()

	res := runTool(t, newFetch(false), fetchArgs(t, srv.URL, 0))
	if !res.IsError || !strings.Contains(res.Text, "loopback address") {
		t.Errorf("loopback fetch = %q (err=%v)", res.Text, res.IsError)
	}
}

// TestFetchRedirectToPrivateRefused exempts only the first hop from the
// address guard (a test hook), so the redirect target — loopback again — is
// judged by the real classifier at dial time.
func TestFetchRedirectToPrivateRefused(t *testing.T) {
	var target string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target, http.StatusFound)
	}))
	defer srv.Close()
	target = "http://127.0.0.1:9/private"

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	f := newFetch(false)
	f.allowAddr = func(addr string) bool { return addr == u.Host }

	res := runTool(t, f, fetchArgs(t, srv.URL, 0))
	if !res.IsError || !strings.Contains(res.Text, "loopback address") {
		t.Errorf("redirect to loopback = %q (err=%v)", res.Text, res.IsError)
	}
}

func TestCheckRedirect(t *testing.T) {
	req := func(raw string) *http.Request {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Request{URL: u}
	}
	if err := checkRedirect(req("https://example.com/b"), []*http.Request{req("https://example.com/a")}); err != nil {
		t.Errorf("one redirect refused: %v", err)
	}
	via := make([]*http.Request, fetchMaxRedirects)
	if err := checkRedirect(req("https://example.com/b"), via); err == nil {
		t.Error("redirect chain over the limit was allowed")
	}
	if err := checkRedirect(req("file:///etc/passwd"), nil); err == nil {
		t.Error("redirect to file:// was allowed")
	}
}

func TestCheckAddr(t *testing.T) {
	f := newFetch(false)
	for _, addr := range []string{"127.0.0.1:80", "[::1]:80", "10.1.2.3:443", "169.254.169.254:80", "[::ffff:192.168.0.1]:80"} {
		if err := f.checkAddr("tcp", addr); err == nil {
			t.Errorf("checkAddr(%q) allowed", addr)
		}
	}
	if err := f.checkAddr("tcp", "93.184.216.34:443"); err != nil {
		t.Errorf("checkAddr of a public address: %v", err)
	}
	if err := f.checkAddr("unix", "/var/run/docker.sock"); err == nil {
		t.Error("checkAddr allowed a unix socket")
	}
	if err := f.checkAddr("tcp", "not-an-address"); err == nil {
		t.Error("checkAddr allowed a malformed address")
	}
	if err := newFetch(true).checkAddr("tcp", "127.0.0.1:80"); err != nil {
		t.Errorf("AllowPrivate did not allow loopback: %v", err)
	}
}

func TestTextLike(t *testing.T) {
	yes := []string{"text/html; charset=utf-8", "text/plain", "application/json", "application/xml", "application/ld+json", "image/svg+xml", "", "application/x-ndjson"}
	no := []string{"image/png", "application/pdf", "application/octet-stream", "audio/ogg", "video/mp4", "application/zip"}
	for _, ct := range yes {
		if !textLike(ct) {
			t.Errorf("textLike(%q) = false", ct)
		}
	}
	for _, ct := range no {
		if textLike(ct) {
			t.Errorf("textLike(%q) = true", ct)
		}
	}
}

func TestHTMLToText(t *testing.T) {
	cases := map[string]string{
		"<p>a</p><p>b</p>":                  "a\nb",
		"<b>bold</b> text":                  "bold text",
		"<!-- comment -->kept":              "kept",
		"<script>drop()</script>kept":       "kept",
		"<style>.a{}</style>kept":           "kept",
		"<head><title>t</title></head>body": "body",
		"a &amp; b &lt;c&gt; &quot;d&quot;": `a & b <c> "d"`,
		"a\n\n\n\n\nb":                      "a\n\nb",
		"spaced      out":                   "spaced out",
		"<ul><li>one</li><li>two</li></ul>": "one\ntwo",
		"<div>unterminated <span":           "unterminated",
		"<SCRIPT>x</SCRIPT>keep":            "keep",
	}
	for in, want := range cases {
		if got := htmlToText(in); got != want {
			t.Errorf("htmlToText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClassifyIPReservedAndTransitionRanges(t *testing.T) {
	for _, s := range []string{
		"64:ff9b::a00:1", "2002:a00:1::1", "2001:0:4136:e378:8000:63bf:3fff:fdd2",
		"::10.0.0.1", "192.0.0.8", "198.18.0.1", "240.0.0.1", "0.1.2.3", "2001:db8::1",
	} {
		if classifyIP(netip.MustParseAddr(s)) == "" {
			t.Errorf("%s must be refused", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "2606:4700:4700::1111", "93.184.216.34"} {
		if why := classifyIP(netip.MustParseAddr(s)); why != "" {
			t.Errorf("%s refused: %s", s, why)
		}
	}
}
