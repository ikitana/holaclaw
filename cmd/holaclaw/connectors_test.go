package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/connector/oauth"
)

// connFake is a daemon with the connector endpoints, answering as the real
// one does. A create or reconnect answers an authorization URL carrying
// the posted redirect and state "S1" (expired already with expired), or
// connected when open; a code answers once the connector settled:
// codeState with codeDetail, connected when empty. A GET answers list and
// details, and no channels for status. An inspect answers inspection. A
// DELETE answers 204 for an id of details, else 404.
type connFake struct {
	open       bool
	expired    bool
	codeState  string
	codeDetail string
	refuse     *gen.Error
	status     int
	list       []gen.Connector                // GET /v1/connectors
	listStatus int                            // answers the list with this error status when set
	details    map[string]gen.ConnectorDetail // GET /v1/connectors/{id}
	leftovers  []string                       // ids DELETE removes though GET answers 404
	inspection gen.ConnectorInspection        // POST /v1/connectors/inspect

	mu     sync.Mutex
	calls  []string // "METHOD path"
	bodies map[string]map[string]any
}

func (f *connFake) serve(t *testing.T) string {
	t.Helper()
	f.bodies = map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status" {
			_ = json.NewEncoder(w).Encode(gen.Status{})
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.bodies[r.URL.Path] = body
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if f.refuse != nil {
			w.WriteHeader(f.status)
			_ = json.NewEncoder(w).Encode(f.refuse)
			return
		}
		id := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/connectors/"), "/")[0]
		if r.Method == http.MethodGet {
			f.read(w, r.URL.Path, id)
			return
		}
		if r.Method == http.MethodDelete {
			if _, ok := f.details[id]; !ok && !slices.Contains(f.leftovers, id) {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(gen.Error{Code: "not_found", Message: "not found"})
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		connected := gen.Connector{Id: id, State: "connected", ServerName: ptrTo("GitHub"), GrantedScopes: &[]string{"repo"}}
		switch {
		case r.URL.Path == "/v1/connectors/inspect":
			_ = json.NewEncoder(w).Encode(f.inspection)
		case strings.HasSuffix(r.URL.Path, "/code"):
			got := connected
			if f.codeState != "" {
				got = gen.Connector{Id: id, State: f.codeState, Detail: ptrTo(f.codeDetail)}
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(got)
		case f.open:
			if id == "" {
				connected.Id, _ = body["id"].(string)
			}
			_ = json.NewEncoder(w).Encode(gen.ConnectorStarted{Connector: connected})
		default:
			redirect, _ := body["redirect_uri"].(string)
			au := "https://as.example/authorize?state=S1&redirect_uri=" + url.QueryEscape(redirect)
			exp := time.Now().Add(time.Minute)
			if f.expired {
				exp = time.Now().Add(-time.Minute)
			}
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(gen.ConnectorStarted{
				Connector: gen.Connector{State: "authorizing"}, AuthorizationUrl: &au, ExpiresAt: &exp,
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// read answers the GETs: no channels, list, and details by id (404 for
// any other).
func (f *connFake) read(w http.ResponseWriter, path, id string) {
	switch {
	case path == "/v1/channels":
		_, _ = io.WriteString(w, `{"channels":[]}`)
	case path == "/v1/connectors" && f.listStatus != 0:
		w.WriteHeader(f.listStatus)
		_ = json.NewEncoder(w).Encode(gen.Error{Code: "forbidden", Message: "refused"})
	case path == "/v1/connectors":
		list := f.list
		if list == nil {
			list = []gen.Connector{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"connectors": list})
	default:
		d, ok := f.details[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(gen.Error{Code: "not_found", Message: "not found"})
			return
		}
		_ = json.NewEncoder(w).Encode(d)
	}
}

func (f *connFake) body(path string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[path]
}

func (f *connFake) posted(suffix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasSuffix(c, suffix) {
			return true
		}
	}
	return false
}

// ephemeral makes --listen bind a free 127.0.0.1 port instead of a pinned
// one.
func ephemeral(t *testing.T) {
	old := callbackRedirects
	t.Cleanup(func() { callbackRedirects = old })
	callbackRedirects = func() []string { return []string{"http://127.0.0.1:0/callback"} }
}

// browser replaces openURL with a person who signs in: each query is a
// redirect the browser follows, in order. It records what the listener
// answered to each.
type browser struct {
	mu      sync.Mutex
	opened  string
	answers []int
	page    string
}

func (b *browser) install(t *testing.T, queries ...func(state string) string) {
	t.Helper()
	old := openURL
	t.Cleanup(func() { openURL = old })
	openURL = func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		q := u.Query()
		b.mu.Lock()
		defer b.mu.Unlock()
		b.opened = raw
		for _, query := range queries {
			resp, err := http.Get(q.Get("redirect_uri") + "?" + query(q.Get("state")))
			if err != nil {
				return err
			}
			page, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			b.answers, b.page = append(b.answers, resp.StatusCode), string(page)
		}
		return nil
	}
}

func signIn(state string) string {
	return "code=FAKEcode&state=" + state + "&iss=https%3A%2F%2Fas.example"
}

// noBrowser fails the test when a browser is opened.
func noBrowser(t *testing.T) {
	old := openURL
	t.Cleanup(func() { openURL = old })
	openURL = func(string) error { t.Error("a browser was opened"); return nil }
}

func TestConnectorsCreatePrintsTheURL(t *testing.T) {
	noBrowser(t)
	f := &connFake{}
	code, out, _ := runCLI(t, "", "connectors", "create", "gh", "https://mcp.example/mcp", "--auth", "oauth",
		"--scopes", "repo,issues:write", "--addr", f.serve(t))
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	b := f.body("/v1/connectors")
	if b["id"] != "gh" || b["url"] != "https://mcp.example/mcp" || b["auth"] != "oauth" ||
		b["redirect_uri"] != oauth.PinnedRedirects()[0] || fmt.Sprint(b["scopes"]) != "[repo issues:write]" {
		t.Errorf("body = %v", b)
	}
	if _, ok := b["client_secret"]; ok {
		t.Error("a client secret was sent")
	}
	if !strings.Contains(out, "https://as.example/authorize?state=S1") ||
		!strings.Contains(out, "holaclaw connectors code gh '<address>'") {
		t.Errorf("stdout = %q", out)
	}
	// The time is the daemon's limit only, and is not put as the one to meet.
	if !strings.Contains(out, "and run, right after signing in:") || strings.Contains(out, ", before ") ||
		!regexp.MustCompile(`\nholaclaw takes the address until \d\d:\d\d:\d\d\. The server may stop taking its code sooner\.\n$`).MatchString(out) {
		t.Errorf("stdout = %q", out)
	}
	if f.posted("/code") {
		t.Error("a code was posted")
	}
}

func TestConnectorsCreateOpenServer(t *testing.T) {
	noBrowser(t)
	ephemeral(t)
	f := &connFake{open: true}
	code, out, _ := runCLI(t, "", "connectors", "create", "docs", "https://docs.example/mcp", "--listen", "--addr", f.serve(t))
	if code != 0 || out != "docs: connected to GitHub, scopes repo\n" {
		t.Fatalf("exit = %d, stdout = %q", code, out)
	}
	if _, ok := f.body("/v1/connectors")["scopes"]; ok {
		t.Error("scopes sent without --scopes")
	}
}

func TestConnectorsCreateInsecure(t *testing.T) {
	noBrowser(t)
	f := &connFake{open: true}
	code, _, _ := runCLI(t, "", "connectors", "create", "lab", "http://10.0.4.20:8765/mcp", "--addr", f.serve(t))
	if _, ok := f.body("/v1/connectors")["allow_insecure_transport"]; code != 0 || ok {
		t.Errorf("without the flag: exit = %d, body = %v", code, f.body("/v1/connectors"))
	}
	f = &connFake{open: true}
	code, _, _ = runCLI(t, "", "connectors", "create", "lab", "http://10.0.4.20:8765/mcp", "--allow-insecure-transport", "--addr", f.serve(t))
	if b := f.body("/v1/connectors"); code != 0 || b["allow_insecure_transport"] != true {
		t.Errorf("with the flag: exit = %d, body = %v", code, b)
	}
}

func TestConnectorsCreateRefused(t *testing.T) {
	f := &connFake{refuse: &gen.Error{Code: "conflict", Message: "connector: id in use"}, status: http.StatusConflict}
	code, _, errOut := runCLI(t, "", "connectors", "create", "gh", "https://mcp.example/mcp", "--addr", f.serve(t))
	if code != 1 || !strings.Contains(errOut, "gh: id in use") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
	code, _, errOut = runCLI(t, "", "connectors", "create", "gh", "https://mcp.example/mcp", "--auth", "basic", "--addr", f.serve(t))
	if code != 2 || !strings.Contains(errOut, "--auth must be none or oauth") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
}

// A refused create with --listen returns at once and closes the listener.
func TestConnectorsListenCreateRefused(t *testing.T) {
	noBrowser(t)
	ephemeral(t)
	f := &connFake{refuse: &gen.Error{Code: "conflict", Message: "connector: id in use"}, status: http.StatusConflict}
	code, _, errOut := runCLI(t, "", "connectors", "create", "gh", "https://mcp.example/mcp", "--listen", "--addr", f.serve(t))
	if code != 1 || !strings.Contains(errOut, "gh: id in use") {
		t.Fatalf("exit = %d, stderr = %q", code, errOut)
	}
	assertClosed(t, f.body("/v1/connectors")["redirect_uri"])
}

// assertClosed fails when something still listens at a redirect URI.
func assertClosed(t *testing.T, redirect any) {
	t.Helper()
	u, err := url.Parse(fmt.Sprint(redirect))
	if err != nil || u.Host == "" {
		t.Fatalf("redirect = %v", redirect)
	}
	if c, err := net.Dial("tcp", u.Host); err == nil {
		_ = c.Close()
		t.Errorf("the callback listener on %s is still open", u.Host)
	}
}

func TestConnectorsClientSecretFromStdin(t *testing.T) {
	noBrowser(t)
	f := &connFake{}
	code, out, _ := runCLI(t, "FAKEsecret\n", "connectors", "create", "gh", "https://mcp.example/mcp",
		"--client-id", "pre", "--client-secret-stdin", "--addr", f.serve(t))
	if code != 0 || strings.Contains(out, "FAKEsecret") {
		t.Fatalf("exit = %d, stdout = %q", code, out)
	}
	if b := f.body("/v1/connectors"); b["client_id"] != "pre" || b["client_secret"] != "FAKEsecret" {
		t.Errorf("body = %v", b)
	}
	for stdin, want := range map[string]string{
		"":                                     "stdin is empty",
		strings.Repeat("x", maxClientSecret+1): "over 4096 bytes",
	} {
		f := &connFake{}
		code, _, errOut := runCLI(t, stdin, "connectors", "create", "gh", "https://mcp.example/mcp",
			"--client-id", "pre", "--client-secret-stdin", "--addr", f.serve(t))
		if code != 2 || !strings.Contains(errOut, want) || f.posted("/v1/connectors") {
			t.Errorf("exit = %d, stderr = %q, want %q", code, errOut, want)
		}
	}
	// Leading space does not let an over-long secret through cut.
	f = &connFake{}
	code, _, errOut := runCLI(t, "  \n"+strings.Repeat("x", maxClientSecret+1), "connectors", "create", "gh",
		"https://mcp.example/mcp", "--client-id", "pre", "--client-secret-stdin", "--addr", f.serve(t))
	if code != 2 || !strings.Contains(errOut, "over 4096 bytes") || f.posted("/v1/connectors") {
		t.Errorf("leading space: exit = %d, stderr = %q", code, errOut)
	}
	// Exactly the cap is accepted whole, with or without the newline a
	// shell adds.
	secret := strings.Repeat("x", maxClientSecret)
	for _, stdin := range []string{secret, secret + "\n", secret + "\r\n"} {
		f := &connFake{}
		code, _, _ = runCLI(t, stdin, "connectors", "create", "gh", "https://mcp.example/mcp",
			"--client-id", "pre", "--client-secret-stdin", "--addr", f.serve(t))
		if code != 0 || f.body("/v1/connectors")["client_secret"] != secret {
			t.Errorf("exit = %d with a secret of exactly the cap and %q", code, strings.TrimPrefix(stdin, secret))
		}
	}
}

func TestConnectorsReconnectPrintsTheURL(t *testing.T) {
	noBrowser(t)
	f := &connFake{}
	code, out, _ := runCLI(t, "", "connectors", "reconnect", "gh", "--scopes", "repo", "--addr", f.serve(t))
	if code != 0 || !strings.Contains(out, "holaclaw connectors code gh") {
		t.Fatalf("exit = %d, stdout = %q", code, out)
	}
	if b := f.body("/v1/connectors/gh/reconnect"); b["redirect_uri"] != oauth.PinnedRedirects()[0] || fmt.Sprint(b["scopes"]) != "[repo]" {
		t.Errorf("body = %v", b)
	}
}

func TestConnectorsReconnectUnknown(t *testing.T) {
	f := &connFake{refuse: &gen.Error{Code: "not_found", Message: "not found"}, status: http.StatusNotFound}
	code, _, errOut := runCLI(t, "", "connectors", "reconnect", "gh", "--addr", f.serve(t))
	if code != 1 || !strings.Contains(errOut, `no connector named "gh"`) {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
}

func TestConnectorsCode(t *testing.T) {
	const landed = "http://127.0.0.1:26500/callback?code=FAKEcode&state=S1&iss=https%3A%2F%2Fas.example"
	f := &connFake{}
	addr := f.serve(t)
	code, out, _ := runCLI(t, "", "connectors", "code", "gh", landed, "--addr", addr)
	if code != 0 || out != "gh: connected to GitHub, scopes repo\n" {
		t.Fatalf("exit = %d, stdout = %q", code, out)
	}
	if b := f.body("/v1/connectors/gh/code"); b["code"] != "FAKEcode" || b["state"] != "S1" || b["iss"] != "https://as.example" {
		t.Errorf("body = %v", b)
	}

	for name, tc := range map[string]struct{ url, want string }{
		"no code":   {"http://127.0.0.1:26500/callback?state=S1", "the address has no code"},
		"no state":  {"http://127.0.0.1:26500/callback?code=FAKEcode", "the address has no state"},
		"no query":  {"http://127.0.0.1:26500/callback", "not the address the browser landed on"},
		"refused":   {"http://127.0.0.1:26500/callback?error=access_denied&error_description=no%1b[31m&state=S1", "refused: access_denied: no\ntry again"},
		"not a url": {"%zz", "not the address the browser landed on"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &connFake{}
			code, _, errOut := runCLI(t, "", "connectors", "code", "gh", tc.url, "--addr", f.serve(t))
			if code != 1 || !strings.Contains(errOut, tc.want) {
				t.Errorf("exit = %d, stderr = %q, want %q", code, errOut, tc.want)
			}
			if f.posted("/code") {
				t.Error("a code was posted")
			}
		})
	}

	// The daemon's refusals: no open authorization, with its reason and the
	// way on; another conflict, without; a degraded server.
	f = &connFake{refuse: &gen.Error{Code: "conflict", Message: "connector: no open authorization: it expired"}, status: http.StatusConflict}
	if code, _, errOut := runCLI(t, "", "connectors", "code", "gh", landed, "--addr", f.serve(t)); code != 1 ||
		!strings.Contains(errOut, "gh: no open authorization: it expired\ntry again with `holaclaw connectors reconnect gh`\n") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
	f = &connFake{refuse: &gen.Error{Code: "conflict", Message: "connector: connectors.toml does not parse; fix it by hand"}, status: http.StatusConflict}
	if code, _, errOut := runCLI(t, "", "connectors", "code", "gh", landed, "--addr", f.serve(t)); code != 1 ||
		!strings.Contains(errOut, "gh: connectors.toml does not parse") || strings.Contains(errOut, "try again") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
	f = &connFake{codeState: "degraded", codeDetail: "server_unreachable"}
	if code, _, errOut := runCLI(t, "", "connectors", "code", "gh", landed, "--addr", f.serve(t)); code != 1 || !strings.Contains(errOut, "gh: degraded (server_unreachable)") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
}

// --listen receives the redirect itself and posts the code; the answer is
// the result.
func TestConnectorsListen(t *testing.T) {
	ephemeral(t)
	var b browser
	b.install(t, signIn)
	f := &connFake{}
	code, out, _ := runCLI(t, "", "connectors", "create", "gh", "https://mcp.example/mcp", "--listen", "--addr", f.serve(t))
	if code != 0 || !strings.HasSuffix(out, "gh: connected to GitHub, scopes repo\n") || !strings.Contains(out, "https://as.example/authorize") {
		t.Fatalf("exit = %d, stdout = %q", code, out)
	}
	redirect, _ := f.body("/v1/connectors")["redirect_uri"].(string)
	if !strings.HasPrefix(redirect, "http://127.0.0.1:") || !strings.HasSuffix(redirect, "/callback") ||
		!strings.Contains(b.opened, url.QueryEscape(redirect)) {
		t.Errorf("redirect = %q, opened %q", redirect, b.opened)
	}
	if body := f.body("/v1/connectors/gh/code"); body["code"] != "FAKEcode" || body["state"] != "S1" || body["iss"] != "https://as.example" {
		t.Errorf("code body = %v", body)
	}
	if fmt.Sprint(b.answers) != "[200]" || !strings.Contains(b.page, "close this tab") {
		t.Errorf("browser got %v %q", b.answers, b.page)
	}
	assertClosed(t, redirect)
}

// A redirect with another state, a code or an error, is refused and does
// not end the wait: the real one still connects.
func TestConnectorsListenIgnoresOtherStates(t *testing.T) {
	ephemeral(t)
	var b browser
	b.install(t,
		func(string) string { return "code=FAKEforged&state=other" },
		func(string) string { return "error=access_denied&state=other" },
		signIn)
	f := &connFake{}
	code, out, _ := runCLI(t, "", "connectors", "create", "gh", "https://mcp.example/mcp", "--listen", "--addr", f.serve(t))
	if code != 0 || !strings.HasSuffix(out, "gh: connected to GitHub, scopes repo\n") {
		t.Fatalf("exit = %d, stdout = %q", code, out)
	}
	if fmt.Sprint(b.answers) != "[400 400 200]" {
		t.Errorf("browser got %v", b.answers)
	}
	if body := f.body("/v1/connectors/gh/code"); body["code"] != "FAKEcode" {
		t.Errorf("code body = %v", body)
	}
}

func TestConnectorsListenFailure(t *testing.T) {
	ephemeral(t)
	var b browser
	b.install(t, signIn)
	f := &connFake{codeState: "degraded", codeDetail: "server_unreachable"}
	code, _, errOut := runCLI(t, "", "connectors", "reconnect", "gh", "--listen", "--addr", f.serve(t))
	if code != 1 || !strings.Contains(errOut, "gh: degraded (server_unreachable)") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
}

func TestConnectorsListenRedirectError(t *testing.T) {
	ephemeral(t)
	var b browser
	b.install(t, func(state string) string { return "error=access_denied&state=" + state })
	f := &connFake{}
	code, _, errOut := runCLI(t, "", "connectors", "create", "gh", "https://mcp.example/mcp", "--listen", "--addr", f.serve(t))
	if code != 1 || !strings.Contains(errOut, "refused: access_denied") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
	if f.posted("/code") {
		t.Error("a code was posted")
	}
}

// The person clicks Deny: the browser, which does not wait for the command,
// still gets the whole page, and it says the sign-in was refused.
func TestConnectorsListenRefusedPage(t *testing.T) {
	ephemeral(t)
	old := openURL
	t.Cleanup(func() { openURL = old })
	type answer struct {
		status int
		page   string
		err    error
	}
	got := make(chan answer, 1)
	openURL = func(raw string) error {
		u, _ := url.Parse(raw)
		go func() {
			resp, err := http.Get(u.Query().Get("redirect_uri") + "?error=access_denied&state=" + u.Query().Get("state"))
			if err != nil {
				got <- answer{err: err}
				return
			}
			page, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			got <- answer{resp.StatusCode, string(page), err}
		}()
		return nil
	}
	f := &connFake{}
	code, _, errOut := runCLI(t, "", "connectors", "create", "gh", "https://mcp.example/mcp", "--listen", "--addr", f.serve(t))
	if code != 1 || !strings.Contains(errOut, "refused: access_denied") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
	select {
	case a := <-got:
		if a.err != nil || a.status != http.StatusOK || !strings.Contains(a.page, "The sign-in was refused") {
			t.Errorf("browser got %d %q, %v", a.status, a.page, a.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the browser got no answer")
	}
}

// orderWriter records whether the page was written after the query was
// handed over.
type orderWriter struct {
	*httptest.ResponseRecorder
	cb   *callbackServer
	late bool
}

func (w *orderWriter) Write(b []byte) (int, error) {
	w.late = w.late || len(w.cb.got) > 0
	return w.ResponseRecorder.Write(b)
}

func (w *orderWriter) Flush() {
	w.late = w.late || len(w.cb.got) > 0
	w.ResponseRecorder.Flush()
}

// The page is written and flushed before the query is handed over; a
// second redirect is answered but not handed over.
func TestCallbackPageBeforeHandOver(t *testing.T) {
	ephemeral(t)
	cb, err := listenCallback()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.close()
	if err := cb.expect("https://as.example/authorize?state=S1"); err != nil {
		t.Fatal(err)
	}
	serve := func(query, page string) *orderWriter {
		w := &orderWriter{ResponseRecorder: httptest.NewRecorder(), cb: cb}
		cb.serve(w, httptest.NewRequest(http.MethodGet, "/callback?"+query, nil))
		if w.late || w.Code != http.StatusOK || !strings.Contains(w.Body.String(), page) {
			t.Errorf("%s: late = %v, %d %q", query, w.late, w.Code, w.Body)
		}
		return w
	}
	if w := serve("error=access_denied&state=S1", "The sign-in was refused"); !w.Flushed {
		t.Error("the page was not flushed")
	}
	if q := <-cb.got; q.Get("error") != "access_denied" {
		t.Errorf("handed over %v", q)
	}
	serve("code=x&state=S1", "already got a sign-in")
	select {
	case q := <-cb.got:
		t.Errorf("the second redirect was handed over: %v", q)
	default:
	}
}

// Ctrl-C while waiting for the browser ends the command at once, quietly
// with 130, and closes the listener.
func TestConnectorsListenInterrupted(t *testing.T) {
	ephemeral(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old := openURL
	t.Cleanup(func() { openURL = old })
	openURL = func(string) error { cancel(); return nil }
	f := &connFake{}
	var out, errOut strings.Builder
	done := make(chan int, 1)
	addr := f.serve(t)
	go func() {
		done <- run(ctx, []string{"connectors", "create", "gh", "https://mcp.example/mcp", "--listen", "--addr", addr},
			strings.NewReader(""), &out, &errOut)
	}()
	select {
	case code := <-done:
		if code != 130 || strings.Contains(errOut.String(), "holaclaw:") {
			t.Errorf("exit = %d, stderr = %q", code, errOut.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end on Ctrl-C")
	}
	if f.posted("/code") {
		t.Error("a code was posted")
	}
	assertClosed(t, f.body("/v1/connectors")["redirect_uri"])
}

// An authorization that expires before the browser comes back ends the
// wait.
func TestConnectorsListenExpired(t *testing.T) {
	ephemeral(t)
	var b browser
	b.install(t) // the person never signs in
	f := &connFake{expired: true}
	code, _, errOut := runCLI(t, "", "connectors", "create", "gh", "https://mcp.example/mcp", "--listen", "--addr", f.serve(t))
	if code != 1 || !strings.Contains(errOut, "the authorization expired") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
	if f.posted("/code") {
		t.Error("a code was posted")
	}
}

// A busy pinned port is skipped for the next one. The only test that binds
// the real pinned ports.
func TestConnectorsListenPortFallback(t *testing.T) {
	var busy string
	pinned := oauth.PinnedRedirects()
	for _, r := range pinned[:len(pinned)-1] {
		u, _ := url.Parse(r)
		if l, err := net.Listen("tcp", u.Host); err == nil {
			defer func() { _ = l.Close() }()
			busy = r
			break
		}
	}
	if busy == "" {
		t.Skip("no pinned port is free")
	}
	var b browser
	b.install(t, signIn)
	f := &connFake{}
	code, _, _ := runCLI(t, "", "connectors", "create", "gh", "https://mcp.example/mcp", "--listen", "--addr", f.serve(t))
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if got, _ := f.body("/v1/connectors")["redirect_uri"].(string); got <= busy || !oauth.PinnedRedirect(got) {
		t.Errorf("redirect = %q, want a pinned one after the busy %s", got, busy)
	}
}

// Only GET /callback with the expected state and a code or an error counts.
func TestCallbackServerIgnoresStrays(t *testing.T) {
	ephemeral(t)
	cb, err := listenCallback()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.close()
	base := strings.TrimSuffix(cb.redirect, "/callback")
	get := func(method, path string) int {
		req, _ := http.NewRequest(method, base+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	// Before the URL is known, no state is right.
	if got := get(http.MethodGet, "/callback?code=x&state="); got != http.StatusBadRequest {
		t.Errorf("before expect = %d", got)
	}
	if err := cb.expect("https://as.example/authorize?state=S1"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/favicon.ico", http.StatusNotFound},
		{http.MethodPost, "/callback?code=x&state=S1", http.StatusMethodNotAllowed},
		{http.MethodGet, "/callback?state=S1", http.StatusBadRequest},
		{http.MethodGet, "/callback?code=x&state=S2", http.StatusBadRequest},
		{http.MethodGet, "/callback?error=access_denied", http.StatusBadRequest},
	} {
		if got := get(tc.method, tc.path); got != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, got, tc.want)
		}
	}
	select {
	case q := <-cb.got:
		t.Errorf("a stray was taken: %v", q)
	default:
	}
	if cb.expect("https://as.example/authorize") == nil {
		t.Error("an authorization URL without state was accepted")
	}
}

// esc is a terminal escape a hostile server could put in any string: a
// window title (OSC) and a colour (CSI). printable drops both whole.
const esc = "\x1b]0;owned\x07\x1b[31m"

// The runes printable drops besides the control characters: a right-to-left
// override, a zero-width space, a line separator and an isolate. Built from
// code points so the source shows them.
const (
	bidiOverride = string(rune(0x202e))
	zeroWidth    = string(rune(0x200b))
	lineSep      = string(rune(0x2028))
	isolate      = string(rune(0x2066))
)

func TestConnectorsList(t *testing.T) {
	f := &connFake{}
	addr := f.serve(t)
	code, out, _ := runCLI(t, "", "connectors", "list", "--addr", addr)
	if code != 0 || out != "no connectors\n" {
		t.Fatalf("exit = %d, stdout = %q", code, out)
	}

	check := time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)
	f.list = []gen.Connector{
		{Id: "gh", State: "connected", ServerName: ptrTo("GitHub" + esc), ToolCount: 12,
			GrantedScopes: &[]string{"repo", "read:org"}, LastCheck: &check},
		{Id: "wiki", State: "expired", Detail: ptrTo("refresh_rejected"), ToolCount: 3},
	}
	code, out, _ = runCLI(t, "", "connectors", "list", "--addr", addr)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ID") {
		t.Fatalf("stdout = %q", out)
	}
	for i, want := range []string{
		"gh connected GitHub 12 repo read:org 2026-10-22 09:30:00Z -",
		"wiki expired - 3 - - refresh_rejected",
	} {
		if got := strings.Join(strings.Fields(lines[i+1]), " "); got != want {
			t.Errorf("line %d = %q, want %q", i+1, got, want)
		}
	}
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("a control character reached the terminal: %q", out)
	}
}

func TestConnectorsListRefused(t *testing.T) {
	f := &connFake{listStatus: http.StatusInternalServerError}
	code, _, errOut := runCLI(t, "", "connectors", "list", "--addr", f.serve(t))
	if code != 1 || !strings.Contains(errOut, "connectors: refused") {
		t.Errorf("exit = %d, stderr = %q", code, errOut)
	}
}

func TestConnectorsShow(t *testing.T) {
	check := time.Date(2026, 10, 22, 9, 30, 0, 0, time.UTC)
	cutDesc := "Drops a table." + esc + "\nMore text. [description cut at 400 tokens]"
	f := &connFake{details: map[string]gen.ConnectorDetail{"gh": {
		Connector: gen.Connector{
			Id: "gh", Url: "https://mcp.example/mcp", State: "degraded", Detail: ptrTo("ping_failed"), Auth: ptrTo("oauth"),
			ServerName: ptrTo("Git" + bidiOverride + "Hub"), ServerVersion: ptrTo("1.2" + esc), GrantedScopes: &[]string{"repo"},
			LastCheck: &check, ToolCount: 4,
		},
		Trusted: true,
		AuthMetadata: &gen.ConnectorAuthMetadata{
			Resource: ptrTo("https://mcp.example/mcp" + isolate), Issuer: "https://as.example" + esc,
			TokenEndpoint: ptrTo("https://as.example/token"), IdentityKind: ptrTo("cimd"), ClientId: ptrTo("https://holaclaw.dev/client.json"),
		},
		Tools: []gen.ConnectorTool{
			{Name: "drop", Description: &cutDesc, Risk: "external", AlwaysAsk: true, DescriptionTruncated: true},
			{Name: "list_issues", Description: ptrTo(strings.Repeat("x", 200)), Risk: "read"},
			{Name: "noop", Risk: "external"},
			{Name: "rename_file", Description: ptrTo("Re" + zeroWidth + "names " + bidiOverride + "elif." + lineSep + "More text"), Risk: "read"},
		},
	}}}
	code, out, _ := runCLI(t, "", "connectors", "show", "gh", "--addr", f.serve(t))
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{
		"id: gh\n", "url: https://mcp.example/mcp\n", "state: degraded (ping_failed)\n", "server: GitHub 1.2\n",
		"auth: oauth\n", "scopes granted: repo\n", "trusted: yes\n", "last check: 2026-10-22 09:30:00Z\n",
		"resource: https://mcp.example/mcp\nissuer: https://as.example\n", "token endpoint: https://as.example/token\n",
		"client identity: cimd\n", "client id: https://holaclaw.dev/client.json\n", "tools (4):\n",
		"  drop         external, asks  Drops a table. [cut]\n",
		"  list_issues  read            " + strings.Repeat("x", maxToolDescriptionRunes-1) + "…\n",
		"  noop         external\n",
		"  rename_file  read            Renames elif.\n",
		"[cut]: the server's description was over 400 tokens",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout has no %q:\n%s", want, out)
		}
	}
	if strings.ContainsAny(out, "\x1b\x07"+bidiOverride+zeroWidth+lineSep+isolate) || strings.Contains(out, "More text") ||
		strings.Contains(out, "owned") || strings.Contains(out, "[31m") {
		t.Errorf("stdout = %q", out)
	}
	if strings.Contains(out, "registration endpoint") || strings.Contains(out, "scopes requested") {
		t.Errorf("an empty field was printed:\n%s", out)
	}
}

// A connector still authorizing with a pre-registered client shows the
// configured client id; an open one has no auth metadata lines.
func TestConnectorsShowWithoutGrant(t *testing.T) {
	f := &connFake{details: map[string]gen.ConnectorDetail{
		"gh": {
			Connector: gen.Connector{Id: "gh", Url: "https://mcp.example/mcp", State: "authorizing", Auth: ptrTo("oauth"),
				ClientId: ptrTo("pre-registered")},
			AuthMetadata: &gen.ConnectorAuthMetadata{Issuer: "https://as.example", IdentityKind: ptrTo("pre-registered")},
			Tools:        []gen.ConnectorTool{},
		},
		"docs": {
			Connector: gen.Connector{Id: "docs", Url: "https://docs.example/mcp", State: "connected", Auth: ptrTo("none"), ToolCount: 1},
			Tools:     []gen.ConnectorTool{{Name: "search", Description: ptrTo("Search."), Risk: "read"}},
		},
	}}
	addr := f.serve(t)
	code, out, _ := runCLI(t, "", "connectors", "show", "gh", "--addr", addr)
	if code != 0 || !strings.Contains(out, "state: authorizing\n") || !strings.Contains(out, "issuer: https://as.example\n") ||
		!strings.Contains(out, "client identity: pre-registered\n") || !strings.Contains(out, "client id: pre-registered\n") ||
		!strings.HasSuffix(out, "\nno tools\n") {
		t.Errorf("gh: exit = %d, stdout:\n%s", code, out)
	}
	code, out, _ = runCLI(t, "", "connectors", "show", "docs", "--addr", addr)
	want := "id: docs\nurl: https://docs.example/mcp\nstate: connected\nauth: none\ntrusted: no\n\ntools (1):\n  search  read  Search.\n"
	if code != 0 || out != want {
		t.Errorf("docs: exit = %d, stdout = %q, want %q", code, out, want)
	}
}

func TestConnectorsShowUnknown(t *testing.T) {
	f := &connFake{}
	code, out, errOut := runCLI(t, "", "connectors", "show", "nope", "--addr", f.serve(t))
	if code != 1 || out != "" || !strings.Contains(errOut, `no connector named "nope"`) {
		t.Errorf("exit = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
}

func TestStatusConnectorLines(t *testing.T) {
	f := &connFake{list: []gen.Connector{
		{Id: "gh", State: "connected"},
		{Id: "wiki", State: "expired", Detail: ptrTo("refresh_rejected" + esc)},
		{Id: "docs", State: "pending"},
	}}
	addr := f.serve(t)
	code, out, _ := runCLI(t, "", "status", "--addr", addr)
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "CONNECTOR  STATE    DETAIL\nwiki       expired  refresh_rejected\ndocs       pending  -\n") ||
		strings.Contains(out, "gh ") || strings.ContainsAny(out, "\x1b\x07") {
		t.Errorf("stdout = %q", out)
	}

	// All connected, no connectors, a token without the admin scope, or a
	// daemon without the route or without connectors: no connector lines.
	for name, fk := range map[string]*connFake{
		"connected": {list: []gen.Connector{{Id: "gh", State: "connected"}}},
		"none":      {},
		"forbidden": {listStatus: http.StatusForbidden},
		"older":     {listStatus: http.StatusNotFound},
		"absent":    {listStatus: http.StatusNotImplemented},
	} {
		code, out, errOut := runCLI(t, "", "status", "--addr", fk.serve(t))
		if code != 0 || strings.Contains(out, "CONNECTOR") || !strings.Contains(out, "no channels configured") || errOut != "" {
			t.Errorf("%s: exit = %d, stdout = %q, stderr = %q", name, code, out, errOut)
		}
	}

	// Any other failure is one line on stderr; the rest of status stands.
	fk := &connFake{listStatus: http.StatusInternalServerError}
	code, out, errOut := runCLI(t, "", "status", "--addr", fk.serve(t))
	if code != 0 || !strings.Contains(out, "no channels configured") || strings.Contains(out, "CONNECTOR") ||
		!strings.HasPrefix(errOut, "connectors: ") || !strings.Contains(errOut, "refused") || strings.Count(errOut, "\n") != 1 {
		t.Errorf("exit = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
}

func TestFirstLine(t *testing.T) {
	r := func(c rune) string { return string(c) }
	for _, tc := range []struct{ name, in, want string }{
		{"one line", "Lists issues.", "Lists issues."},
		{"LF", "Lists issues.\nUse with care", "Lists issues."},
		{"CR", "Lists issues.\rUse with care", "Lists issues."},
		{"CRLF", "Lists issues.\r\nUse with care", "Lists issues."},
		{"NEL", "Lists issues." + r(0x85) + "Use with care", "Lists issues."},
		{"line separator", "Lists issues." + r(0x2028) + "Use with care", "Lists issues."},
		{"paragraph separator", "Lists issues." + r(0x2029) + "Use with care", "Lists issues."},
		{"tab is a space", "Lists\tissues.", "Lists issues."},
		{"leading blank lines", "\n  \n\t\nLists issues.\nMore", "Lists issues."},
		{"a line of escapes only", "\x1b[31m\nLists issues.", "Lists issues."},
		{"trimmed", "  Lists issues.  ", "Lists issues."},
		{"empty", "", ""},
		{"blank", " \n\r\n\t", ""},
	} {
		if got := firstLine(tc.in); got != tc.want {
			t.Errorf("%s: firstLine(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestConnectorsInspectOpen(t *testing.T) {
	f := &connFake{inspection: gen.ConnectorInspection{
		Open: true, ServerName: ptrTo("Docs" + lineSep), ServerVersion: ptrTo("2.0" + esc),
		Tools: []gen.ConnectorTool{
			{Name: "drop", Description: ptrTo("Drops it." + bidiOverride + "\nSecond line."), Risk: "external", AlwaysAsk: true},
			{Name: "search" + zeroWidth, Description: ptrTo(strings.Repeat("x", 200)), Risk: "read"},
		},
	}}
	code, out, _ := runCLI(t, "", "connectors", "inspect", "https://docs.example/mcp", "--addr", f.serve(t))
	want := "url: https://docs.example/mcp\nopen: yes, no sign-in needed\nserver: Docs 2.0\n\ntools (2):\n" +
		"  drop    external, asks  Drops it.\n" +
		"  search  read            " + strings.Repeat("x", maxToolDescriptionRunes-1) + "…\n"
	if code != 0 || out != want {
		t.Errorf("exit = %d, stdout = %q, want %q", code, out, want)
	}
	if b := f.body("/v1/connectors/inspect"); b["url"] != "https://docs.example/mcp" || len(b) != 1 {
		t.Errorf("body = %v", b)
	}

	// An open server without tools says so.
	f = &connFake{inspection: gen.ConnectorInspection{Open: true, Tools: []gen.ConnectorTool{}}}
	code, out, _ = runCLI(t, "", "connectors", "inspect", "https://docs.example/mcp", "--addr", f.serve(t))
	if code != 0 || out != "url: https://docs.example/mcp\nopen: yes, no sign-in needed\n\nno tools\n" {
		t.Errorf("exit = %d, stdout = %q", code, out)
	}
}

// A protected server prints its authorization servers, scopes and the
// identity a create would use; every server string is stripped.
func TestConnectorsInspectProtected(t *testing.T) {
	f := &connFake{inspection: gen.ConnectorInspection{
		Tools:                []gen.ConnectorTool{},
		AuthorizationServers: &[]string{"https://as.example" + esc, "https://as2.example" + bidiOverride},
		ScopesSupported:      &[]string{"repo", "issues:write" + isolate},
		AuthMetadata: &gen.ConnectorAuthMetadata{
			Issuer: "https://as.example" + zeroWidth, AuthorizationEndpoint: ptrTo("https://as.example/authorize"),
			TokenEndpoint: ptrTo("https://as.example/token" + lineSep), RegistrationEndpoint: ptrTo("https://as.example/register"),
			Scopes: &[]string{"repo"},
		},
		Identity: ptrTo("cimd"),
	}}
	addr := f.serve(t)
	code, out, _ := runCLI(t, "", "connectors", "inspect", "https://mcp.example/mcp", "--addr", addr)
	want := "url: https://mcp.example/mcp\nopen: no, it needs a sign-in\n" +
		"authorization servers: https://as.example https://as2.example\n" +
		"issuer: https://as.example\nauthorization endpoint: https://as.example/authorize\n" +
		"token endpoint: https://as.example/token\nregistration endpoint: https://as.example/register\n" +
		"scopes supported: repo issues:write\nscopes offered: repo\nclient identity: cimd\n"
	if code != 0 || out != want {
		t.Errorf("exit = %d, stdout = %q, want %q", code, out, want)
	}
	if strings.ContainsAny(out, "\x1b\x07"+bidiOverride+zeroWidth+lineSep+isolate) {
		t.Errorf("stdout = %q", out)
	}

	// Neither CIMD nor DCR: the identity line says what create needs.
	f.inspection = gen.ConnectorInspection{
		Tools:        []gen.ConnectorTool{},
		AuthMetadata: &gen.ConnectorAuthMetadata{Issuer: "https://as.example"}, Identity: ptrTo("pre-registered"),
	}
	code, out, _ = runCLI(t, "", "connectors", "inspect", "https://mcp.example/mcp", "--addr", addr)
	want = "url: https://mcp.example/mcp\nopen: no, it needs a sign-in\nissuer: https://as.example\n" +
		"client identity: pre-registered (the server offers no other: create needs --client-id)\n"
	if code != 0 || out != want {
		t.Errorf("exit = %d, stdout = %q, want %q", code, out, want)
	}
}

func TestConnectorsInspectRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		status    int
		msg, want string
	}{
		"unreachable": {http.StatusBadGateway, "connector: server unreachable: connection refused" + esc,
			"connectors: server unreachable: connection refused\n"},
		"bad url": {http.StatusBadRequest, "connector: invalid request: url must be https, or http on a loopback host",
			"connectors: invalid request: url must be https"},
		"not admin": {http.StatusForbidden, "token scope does not allow this", "connectors: token scope does not allow this"},
	} {
		f := &connFake{refuse: &gen.Error{Code: "x", Message: tc.msg}, status: tc.status}
		code, out, errOut := runCLI(t, "", "connectors", "inspect", "http://mcp.example/mcp", "--addr", f.serve(t))
		if code != 1 || out != "" || !strings.Contains(errOut, tc.want) || strings.ContainsAny(errOut, "\x1b\x07") {
			t.Errorf("%s: exit = %d, stdout = %q, stderr = %q", name, code, out, errOut)
		}
	}
	code, _, errOut := runCLI(t, "", "connectors", "inspect")
	if code != 2 || !strings.Contains(errOut, "accepts 1 arg") {
		t.Errorf("no url: exit = %d, stderr = %q", code, errOut)
	}
}

// called reports whether the fake got exactly "METHOD path".
func (f *connFake) called(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.calls, call)
}

func TestConnectorsDeleteYes(t *testing.T) {
	for _, id := range []string{"gh", "inspect"} {
		f := &connFake{details: map[string]gen.ConnectorDetail{id: {Connector: gen.Connector{Id: id, State: "expired"}}}}
		code, out, errOut := runCLI(t, "", "connectors", "delete", id, "--yes", "--addr", f.serve(t))
		if code != 0 || out != id+": deleted\n" || strings.Contains(errOut, "Type yes") {
			t.Errorf("%s: exit = %d, stdout = %q, stderr = %q", id, code, out, errOut)
		}
		if !f.called("DELETE /v1/connectors/"+id) || f.called("GET /v1/connectors/"+id) {
			t.Errorf("%s: calls = %v", id, f.calls)
		}
	}
}

// Without --yes only a typed yes deletes; anything else, an empty stdin
// included (no terminal), deletes nothing.
func TestConnectorsDeleteAsksFirst(t *testing.T) {
	details := map[string]gen.ConnectorDetail{"gh": {Connector: gen.Connector{Id: "gh", State: "connected"}}}
	f := &connFake{details: details}
	code, out, errOut := runCLI(t, "yes\n", "connectors", "delete", "gh", "--addr", f.serve(t))
	if code != 0 || out != "gh: deleted\n" || !strings.Contains(errOut, "Delete connector gh?") || !strings.Contains(errOut, "Type yes to go on") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	if !f.called("DELETE /v1/connectors/gh") {
		t.Errorf("calls = %v", f.calls)
	}
	for _, answer := range []string{"", "y\n", "no\n", "YES please\n"} {
		f := &connFake{details: details}
		code, out, errOut := runCLI(t, answer, "connectors", "delete", "gh", "--addr", f.serve(t))
		if code != 1 || out != "" || !strings.Contains(errOut, "not deleted") || f.called("DELETE /v1/connectors/gh") {
			t.Errorf("answered %q: exit = %d, stdout = %q, stderr = %q, calls = %v", answer, code, out, errOut, f.calls)
		}
	}
}

// An id the daemon does not list is asked about too, unless --yes; with
// nothing left of it the delete answers 404.
func TestConnectorsDeleteUnknown(t *testing.T) {
	for _, args := range [][]string{{}, {"--yes"}} {
		f := &connFake{}
		code, out, errOut := runCLI(t, "yes\n", append([]string{"connectors", "delete", "nope", "--addr", f.serve(t)}, args...)...)
		if code != 1 || out != "" || !strings.Contains(errOut, `no connector named "nope"`) ||
			strings.Contains(errOut, "Type yes") != (len(args) == 0) {
			t.Errorf("%v: exit = %d, stdout = %q, stderr = %q", args, code, out, errOut)
		}
		if !f.called("DELETE /v1/connectors/nope") {
			t.Errorf("%v: calls = %v", args, f.calls)
		}
	}
	code, _, errOut := runCLI(t, "", "connectors", "delete")
	if code != 2 || !strings.Contains(errOut, "accepts 1 arg") {
		t.Errorf("no id: exit = %d, stderr = %q", code, errOut)
	}
}

// The daemon's refusal is printed stripped of control characters.
func TestConnectorsDeleteRefused(t *testing.T) {
	for _, tc := range []struct {
		status   int
		msg, out string
	}{
		{http.StatusInternalServerError,
			"connector: deleted, but removing its sign-in or cache failed; delete gh again before creating it again" + esc,
			"gh: deleted, but removing its sign-in or cache failed; delete gh again before creating it again\n"},
		{http.StatusConflict,
			"connector: connectors.toml is a symlink the daemon does not write: remove [connectors.gh] there by hand, then delete again",
			"gh: connectors.toml is a symlink the daemon does not write: remove [connectors.gh] there by hand, then delete again\n"},
	} {
		f := &connFake{refuse: &gen.Error{Code: "x", Message: tc.msg}, status: tc.status}
		code, out, errOut := runCLI(t, "", "connectors", "delete", "gh", "--yes", "--addr", f.serve(t))
		if code != 1 || out != "" || !strings.HasSuffix(errOut, tc.out) || strings.ContainsAny(errOut, "\x1b\x07") {
			t.Errorf("%d: exit = %d, stdout = %q, stderr = %q", tc.status, code, out, errOut)
		}
	}
}

// The retry after a failed delete, or an entry written by hand since the
// start: the id is not listed, so the shorter question comes first.
func TestConnectorsDeleteLeftovers(t *testing.T) {
	const asked = "gh is not listed; whatever is left of it in connectors.toml, auth.toml and the cache is removed.\nType yes to go on: "
	f := &connFake{leftovers: []string{"gh"}}
	code, out, errOut := runCLI(t, "yes\n", "connectors", "delete", "gh", "--addr", f.serve(t))
	if code != 0 || out != "gh: deleted\n" || !strings.Contains(errOut, asked) {
		t.Errorf("exit = %d, stdout = %q, stderr = %q", code, out, errOut)
	}
	if !f.called("GET /v1/connectors/gh") || !f.called("DELETE /v1/connectors/gh") {
		t.Errorf("calls = %v", f.calls)
	}
	for _, answer := range []string{"", "no\n"} {
		f := &connFake{leftovers: []string{"gh"}}
		code, out, errOut := runCLI(t, answer, "connectors", "delete", "gh", "--addr", f.serve(t))
		if code != 1 || out != "" || !strings.Contains(errOut, "not deleted") || f.called("DELETE /v1/connectors/gh") {
			t.Errorf("answered %q: exit = %d, stdout = %q, stderr = %q, calls = %v", answer, code, out, errOut, f.calls)
		}
	}
	f = &connFake{leftovers: []string{"gh"}}
	code, out, errOut = runCLI(t, "", "connectors", "delete", "gh", "--yes", "--addr", f.serve(t))
	if code != 0 || out != "gh: deleted\n" || strings.Contains(errOut, "Type yes") || f.called("GET /v1/connectors/gh") {
		t.Errorf("--yes: exit = %d, stdout = %q, stderr = %q, calls = %v", code, out, errOut, f.calls)
	}
}
