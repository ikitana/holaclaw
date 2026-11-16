package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/config"
	"holaclaw/internal/connector"
	"holaclaw/internal/connector/mcp/mcptest"
	"holaclaw/internal/connector/oauth/oauthtest"
)

const (
	connRedirect = "http://127.0.0.1:26500/callback"
	connSecret   = "FAKEclient-secret-never-answered"
)

// connectorFakes is a protected MCP server behind the fake authorization
// server, whose default token endpoint issues FAKEaccess1, and an open one.
type connectorFakes struct {
	as        *oauthtest.Server
	protected *mcptest.Server
	open      *mcptest.Server
	dir       string // the manager's config directory, where connectors.toml goes
}

// withConnectors gives the harness the real manager over its store, as the
// daemon wires it.
func (h *harness) withConnectors() connectorFakes {
	h.t.Helper()
	return h.withConnectorsFrom(nil, nil)
}

// withConnectorsFrom is withConnectors with the open server offering
// openTools (an echo tool when nil) and the manager loading the entries
// cfg returns, as connectors.toml at start.
func (h *harness) withConnectorsFrom(openTools []mcptest.Tool, cfg func(connectorFakes) map[string]config.Connector) connectorFakes {
	h.t.Helper()
	f := connectorFakes{as: oauthtest.New(h.t), dir: h.t.TempDir()}
	tools := []mcptest.Tool{mcptest.TextTool("echo", "hi")}
	if openTools == nil {
		openTools = tools
	}
	f.protected = mcptest.New(h.t, mcptest.Options{Tools: tools, Wrap: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer FAKEaccess1" {
				w.Header().Set("WWW-Authenticate", f.as.Header())
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}})
	f.as.PRM["resource"] = f.protected.URL
	f.open = mcptest.New(h.t, mcptest.Options{Tools: openTools})
	var entries map[string]config.Connector
	if cfg != nil {
		entries = cfg(f)
	}
	m, err := connector.New(context.Background(), connector.Options{
		Dir: f.dir, Connectors: entries, Store: h.st, Secrets: h.b.Secrets, Events: h.sink,
		HTTPClient: f.as.Client(), Logger: h.b.Logger,
	})
	if err != nil {
		h.t.Fatalf("connector manager: %v", err)
	}
	h.t.Cleanup(func() { _ = m.Close() })
	h.b.Connectors = m
	return f
}

// create opens the protected connector gh and returns the answer.
func (h *harness) createConnector(f connectorFakes, extra func(*gen.ConnectorCreate)) gen.ConnectorStarted {
	h.t.Helper()
	in := gen.ConnectorCreate{Id: "gh", Url: f.protected.URL, RedirectUri: ptr(connRedirect)}
	if extra != nil {
		extra(&in)
	}
	return decode[gen.ConnectorStarted](h.t, h.do(http.MethodPost, "/connectors", h.adminToken, in), http.StatusAccepted)
}

// stateOf is the state parameter of an authorization URL.
func stateOf(t *testing.T, st gen.ConnectorStarted) string {
	t.Helper()
	if st.AuthorizationUrl == nil {
		t.Fatalf("no authorization url: %+v", st)
	}
	u, err := url.Parse(*st.AuthorizationUrl)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("state")
}

func TestConnectorCreateAndCode(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	s := h.stream(h.adminToken, "", "")

	resp := h.do(http.MethodPost, "/connectors", h.adminToken, gen.ConnectorCreate{
		Id: "gh", Url: f.protected.URL, RedirectUri: ptr(connRedirect), Auth: ptr(gen.ConnectorCreateAuthOauth),
		Scopes: &[]string{"repo"}, ClientId: ptr("pre-registered"), ClientSecret: ptr(connSecret),
	})
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || strings.Contains(string(raw), connSecret) {
		t.Fatalf("create = %d %s", resp.StatusCode, raw)
	}
	resp.Body = io.NopCloser(strings.NewReader(string(raw)))
	st := decode[gen.ConnectorStarted](t, resp, http.StatusAccepted)
	c := st.Connector
	if c.Id != "gh" || c.State != "authorizing" || st.ExpiresAt == nil || deref(c.ClientId) != "pre-registered" ||
		c.Scopes == nil || strings.Join(*c.Scopes, ",") != "repo" {
		t.Fatalf("create = %+v", st)
	}
	state := stateOf(t, st)
	if state == "" || !strings.Contains(*st.AuthorizationUrl, url.QueryEscape(connRedirect)) {
		t.Fatalf("authorization url = %s", *st.AuthorizationUrl)
	}

	got := decode[gen.Connector](t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: state}), http.StatusAccepted)
	if got.State != "connected" || got.ServerName == nil || got.GrantedScopes == nil {
		t.Fatalf("code = %+v", got)
	}
	// The stream carries every step, connected last.
	var frames []string
	for state := ""; state != "connected"; {
		fr := s.next(t, 5*time.Second)
		if fr.Type != "connector.status" {
			continue
		}
		frames = append(frames, string(fr.Data))
		var e gen.Event
		if err := json.Unmarshal(fr.Data, &e); err != nil || e.Attrs == nil {
			t.Fatalf("event %s: %v", fr.Data, err)
		}
		state, _ = (*e.Attrs)["state"].(string)
	}
	for _, leak := range []string{connSecret, "FAKEaccess", "FAKErefresh", "FAKEcode"} {
		if strings.Contains(strings.Join(frames, ""), leak) || strings.Contains(h.logs.String(), leak) {
			t.Errorf("%s leaked to an event or a log", leak)
		}
	}

	// The code is single use.
	errorBody(t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: state}), http.StatusConflict, CodeConflict)
	// The id is in use.
	errorBody(t, h.do(http.MethodPost, "/connectors", h.adminToken,
		gen.ConnectorCreate{Id: "gh", Url: f.open.URL}), http.StatusConflict, CodeConflict)
}

func TestConnectorCreateOpenServer(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	st := decode[gen.ConnectorStarted](t, h.do(http.MethodPost, "/connectors", h.adminToken,
		gen.ConnectorCreate{Id: "docs", Url: f.open.URL}), http.StatusOK)
	if st.Connector.State != "connected" || st.AuthorizationUrl != nil || st.ExpiresAt != nil || deref(st.Connector.Auth) != "none" {
		t.Fatalf("create = %+v", st)
	}
	// Reconnecting an open server redials it.
	st = decode[gen.ConnectorStarted](t, h.do(http.MethodPost, "/connectors/docs/reconnect", h.adminToken,
		gen.ConnectorReconnect{}), http.StatusOK)
	if st.Connector.State != "connected" {
		t.Fatalf("reconnect = %+v", st)
	}
}

// allow_insecure_transport in a create goes to the entry, and only there.
func TestConnectorCreateInsecure(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	for id, insecure := range map[string]*bool{"lab": ptr(true), "docs": nil} {
		st := decode[gen.ConnectorStarted](t, h.do(http.MethodPost, "/connectors", h.adminToken,
			gen.ConnectorCreate{Id: id, Url: f.open.URL, AllowInsecureTransport: insecure}), http.StatusOK)
		if st.Connector.State != "connected" {
			t.Fatalf("%s: create = %+v", id, st)
		}
	}
	raw, err := os.ReadFile(filepath.Join(f.dir, config.FileConnectors))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := config.ParseConnectors(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !cs["lab"].AllowInsecureTransport || cs["docs"].AllowInsecureTransport {
		t.Errorf("connectors.toml:\n%s", raw)
	}
}

func TestConnectorCreateRefused(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	for name, tc := range map[string]struct {
		body   any
		status int
		code   string
	}{
		"bad id":         {gen.ConnectorCreate{Id: "Not Valid", Url: f.open.URL}, http.StatusBadRequest, CodeBadRequest},
		"bad url":        {gen.ConnectorCreate{Id: "x", Url: "http://example.com/mcp"}, http.StatusBadRequest, CodeBadRequest},
		"bad auth":       {map[string]any{"id": "x", "url": f.open.URL, "auth": "basic"}, http.StatusBadRequest, CodeBadRequest},
		"bad redirect":   {gen.ConnectorCreate{Id: "x", Url: f.open.URL, RedirectUri: ptr("https://example.com/cb")}, http.StatusBadRequest, CodeBadRequest},
		"unknown field":  {map[string]any{"id": "x", "url": f.open.URL, "verifier": "x"}, http.StatusBadRequest, CodeBadRequest},
		"auth mismatch":  {gen.ConnectorCreate{Id: "x", Url: f.protected.URL, Auth: ptr(gen.ConnectorCreateAuthNone)}, http.StatusBadRequest, CodeBadRequest},
		"no redirect":    {gen.ConnectorCreate{Id: "x", Url: f.protected.URL}, http.StatusBadRequest, CodeBadRequest},
		"unreachable":    {gen.ConnectorCreate{Id: "x", Url: deadURL(t)}, http.StatusBadGateway, CodeInternal},
		"secret, no id":  {gen.ConnectorCreate{Id: "x", Url: f.open.URL, ClientSecret: ptr(connSecret)}, http.StatusBadRequest, CodeBadRequest},
		"not json":       {"[", http.StatusBadRequest, CodeBadRequest},
		"discovery fail": {gen.ConnectorCreate{Id: "x", Url: otherOrigin(f.protected.URL), RedirectUri: ptr(connRedirect)}, http.StatusBadGateway, CodeInternal},
	} {
		t.Run(name, func(t *testing.T) {
			errorBody(t, h.do(http.MethodPost, "/connectors", h.adminToken, tc.body), tc.status, tc.code)
		})
	}
	if strings.Contains(h.logs.String(), connSecret) {
		t.Error("the client secret reached a log")
	}
}

// deadURL is a loopback address nothing listens on.
func deadURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

// otherOrigin is u's server under another origin, so its resource metadata
// names a server it is not.
func otherOrigin(u string) string {
	return strings.Replace(u, "127.0.0.1", "localhost", 1)
}

func TestConnectorCodeRefused(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	errorBody(t, h.do(http.MethodPost, "/connectors/nope/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: "s"}), http.StatusNotFound, CodeNotFound)

	st := h.createConnector(f, nil)
	errorBody(t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		map[string]any{"state": stateOf(t, st)}), http.StatusBadRequest, CodeBadRequest)
	// A mismatch drops the authorization: the right state is too late.
	errorBody(t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: "wrong"}), http.StatusBadRequest, CodeBadRequest)
	errorBody(t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: stateOf(t, st)}), http.StatusNotFound, CodeNotFound)

	// The authorization server refusing the code is its side.
	st = h.createConnector(f, nil)
	f.as.Handle(oauthtest.PathToken, oauthtest.JSONHandler(http.StatusBadRequest, `{"error":"invalid_grant"}`))
	errorBody(t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: stateOf(t, st)}), http.StatusBadGateway, CodeInternal)
}

// An authorization server that promises iss (RFC 9207) gets a missing or
// wrong one refused as a bad request, like a state mismatch.
func TestConnectorCodeIssuer(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	f.as.ASM["authorization_response_iss_parameter_supported"] = true
	for name, iss := range map[string]*string{"missing iss": nil, "wrong iss": ptr("https://evil.example")} {
		st := h.createConnector(f, nil)
		e := decode[gen.Error](t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
			gen.ConnectorCode{Code: "FAKEcode", State: stateOf(t, st), Iss: iss}), http.StatusBadRequest)
		if e.Code != CodeBadRequest || !strings.Contains(e.Message, "issuer mismatch") {
			t.Errorf("%s = %+v", name, e)
		}
	}
	if n := f.as.Count(oauthtest.PathToken); n != 0 {
		t.Errorf("token endpoint called %d times on a mismatch", n)
	}
	st := h.createConnector(f, nil)
	got := decode[gen.Connector](t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: stateOf(t, st), Iss: ptr(f.as.URL)}), http.StatusAccepted)
	if got.State != "connected" {
		t.Fatalf("right iss = %+v", got)
	}
}

func TestConnectorReconnect(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	errorBody(t, h.do(http.MethodPost, "/connectors/nope/reconnect", h.adminToken,
		gen.ConnectorReconnect{RedirectUri: ptr(connRedirect)}), http.StatusNotFound, CodeNotFound)

	st := h.createConnector(f, nil)
	decode[gen.Connector](t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: stateOf(t, st)}), http.StatusAccepted)

	// An OAuth connector needs the redirect the browser goes to.
	e := decode[gen.Error](t, h.do(http.MethodPost, "/connectors/gh/reconnect", h.adminToken,
		gen.ConnectorReconnect{}), http.StatusBadRequest)
	if e.Code != CodeBadRequest || !strings.Contains(e.Message, "redirect_uri is required") {
		t.Errorf("reconnect without redirect_uri = %+v", e)
	}
	// A client secret needs a pre-registered client.
	e = decode[gen.Error](t, h.do(http.MethodPost, "/connectors/gh/reconnect", h.adminToken,
		gen.ConnectorReconnect{ClientSecret: ptr(connSecret), RedirectUri: ptr(connRedirect)}), http.StatusBadRequest)
	if e.Code != CodeBadRequest || !strings.Contains(e.Message, "client_id") || strings.Contains(e.Message, connSecret) {
		t.Errorf("reconnect with a secret = %+v", e)
	}

	re := decode[gen.ConnectorStarted](t, h.do(http.MethodPost, "/connectors/gh/reconnect", h.adminToken,
		gen.ConnectorReconnect{RedirectUri: ptr(connRedirect), Scopes: &[]string{"repo", "issues:write"}}), http.StatusAccepted)
	if re.Connector.State != "authorizing" || re.ExpiresAt == nil || stateOf(t, re) == stateOf(t, st) ||
		!strings.Contains(*re.AuthorizationUrl, "issues%3Awrite") {
		t.Fatalf("reconnect = %+v", re)
	}
	got := decode[gen.Connector](t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: stateOf(t, re)}), http.StatusAccepted)
	if got.State != "connected" {
		t.Fatalf("code after reconnect = %+v", got)
	}
}

func TestConnectorsNeedAdmin(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	for _, tc := range []struct{ path string }{{"/connectors"}, {"/connectors/gh/code"}, {"/connectors/gh/reconnect"}} {
		errorBody(t, h.do(http.MethodPost, tc.path, h.ownerToken, gen.ConnectorCreate{Id: "gh", Url: f.open.URL}),
			http.StatusForbidden, CodeForbidden)
	}
}

func TestConnectorsNotImplemented(t *testing.T) {
	h := newHarness(t)
	errorBody(t, h.do(http.MethodPost, "/connectors", h.adminToken, gen.ConnectorCreate{Id: "gh", Url: "http://127.0.0.1:1"}),
		http.StatusNotImplemented, CodeNotImplemented)
}

// readRaw is the body of an answer of the given status, for leak checks.
func readRaw(t *testing.T, resp *http.Response, status int) string {
	t.Helper()
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d: %s", resp.StatusCode, status, raw)
	}
	return string(raw)
}

func TestConnectorListEmpty(t *testing.T) {
	h := newHarness(t)
	h.withConnectors()
	raw := readRaw(t, h.do(http.MethodGet, "/connectors", h.adminToken, nil), http.StatusOK)
	if strings.TrimSpace(raw) != `{"connectors":[]}` {
		t.Errorf("list = %s", raw)
	}
}

func TestConnectorListAndGet(t *testing.T) {
	h := newHarness(t)
	long := strings.Repeat("a long description of the tool ", 400)
	openTools := []mcptest.Tool{
		{Name: "search", Description: "Search the docs.\nSecond line.", Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}},
		{Name: "drop", Description: long, Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true)}},
		mcptest.TextTool("echo", "hi"),
	}
	f := h.withConnectorsFrom(openTools, func(f connectorFakes) map[string]config.Connector {
		return map[string]config.Connector{"wiki": {ID: "wiki", URL: f.open.URL, Auth: "none", Trusted: true}}
	})
	h.b.Connectors.(*connector.Manager).Check(context.Background())

	// gh connected through a pre-registered client with a secret, and pr
	// still authorizing.
	st := h.createConnector(f, func(in *gen.ConnectorCreate) {
		in.Scopes, in.ClientId, in.ClientSecret = &[]string{"repo"}, ptr("pre-registered"), ptr(connSecret)
	})
	state := stateOf(t, st)
	decode[gen.Connector](t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: state}), http.StatusAccepted)
	pending := h.createConnector(f, func(in *gen.ConnectorCreate) { in.Id = "pr" })

	raw := readRaw(t, h.do(http.MethodGet, "/connectors", h.adminToken, nil), http.StatusOK)
	var list struct {
		Connectors []gen.Connector `json:"connectors"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Connectors) != 3 {
		t.Fatalf("list = %s", raw)
	}
	gh, pr, wiki := list.Connectors[0], list.Connectors[1], list.Connectors[2]
	if gh.Id != "gh" || gh.State != "connected" || gh.ToolCount != 1 || gh.LastCheck == nil || gh.ServerName == nil ||
		gh.GrantedScopes == nil || deref(gh.Auth) != "oauth" || gh.Detail != nil {
		t.Errorf("gh = %+v", gh)
	}
	if pr.Id != "pr" || pr.State != "authorizing" || pr.ToolCount != 0 || pr.LastCheck != nil {
		t.Errorf("pr = %+v", pr)
	}
	if wiki.Id != "wiki" || wiki.State != "connected" || wiki.ToolCount != 3 || wiki.LastCheck == nil ||
		deref(wiki.Detail) != connector.DetailDescriptionsCut {
		t.Errorf("wiki = %+v", wiki)
	}

	// show: tools by name with their class, trust, the cut description.
	raw = readRaw(t, h.do(http.MethodGet, "/connectors/wiki", h.adminToken, nil), http.StatusOK)
	var d gen.ConnectorDetail
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	if d.Connector.Id != "wiki" || !d.Trusted || d.AuthMetadata != nil || len(d.Tools) != 3 {
		t.Fatalf("wiki = %s", raw)
	}
	drop, echo, search := d.Tools[0], d.Tools[1], d.Tools[2]
	if drop.Name != "drop" || drop.Risk != "external" || !drop.AlwaysAsk || !drop.DescriptionTruncated ||
		!strings.HasSuffix(deref(drop.Description), "[description cut at 400 tokens]") {
		t.Errorf("drop = %+v", drop)
	}
	if echo.Name != "echo" || echo.Risk != "external" || echo.AlwaysAsk || echo.DescriptionTruncated {
		t.Errorf("echo = %+v", echo)
	}
	if search.Name != "search" || search.Risk != "read" || search.AlwaysAsk || search.DescriptionTruncated ||
		deref(search.Description) != "Search the docs.\nSecond line." {
		t.Errorf("search = %+v", search)
	}

	// gh: untrusted, with the public metadata of its grant and nothing else.
	raw = readRaw(t, h.do(http.MethodGet, "/connectors/gh", h.adminToken, nil), http.StatusOK)
	d = gen.ConnectorDetail{}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	am := d.AuthMetadata
	if d.Trusted || am == nil || am.Issuer == "" || deref(am.TokenEndpoint) == "" || deref(am.AuthorizationEndpoint) == "" ||
		deref(am.IdentityKind) != "pre-registered" || deref(am.ClientId) != "pre-registered" ||
		deref(am.Resource) != f.protected.URL || len(d.Tools) != 1 {
		t.Errorf("gh = %s", raw)
	}
	pd := decode[gen.ConnectorDetail](t, h.do(http.MethodGet, "/connectors/pr", h.adminToken, nil), http.StatusOK)
	if pd.AuthMetadata == nil || pd.AuthMetadata.ClientId != nil || pd.Connector.State != "authorizing" {
		t.Errorf("pr = %+v", pd)
	}

	// No answer carries a credential, a code or a state.
	for _, path := range []string{"/connectors", "/connectors/gh", "/connectors/pr", "/connectors/wiki"} {
		raw := readRaw(t, h.do(http.MethodGet, path, h.adminToken, nil), http.StatusOK)
		for _, leak := range []string{connSecret, "FAKEaccess", "FAKErefresh", "FAKEcode", state, stateOf(t, pending),
			"verifier", "secret", "\"state=", "access_token", "refresh_token"} {
			if strings.Contains(raw, leak) {
				t.Errorf("%s answers %q: %s", path, leak, raw)
			}
		}
	}
}

// A connector of connectors.toml with an open reconnect shows its entry,
// and nothing of the authorization it is waiting for.
func TestConnectorGetDuringReconnect(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectorsFrom(nil, func(f connectorFakes) map[string]config.Connector {
		return map[string]config.Connector{"gh": {ID: "gh", URL: f.protected.URL, Auth: "oauth", ClientID: "pre-registered", Trusted: true}}
	})
	re := decode[gen.ConnectorStarted](t, h.do(http.MethodPost, "/connectors/gh/reconnect", h.adminToken,
		gen.ConnectorReconnect{RedirectUri: ptr(connRedirect), ClientSecret: ptr(connSecret)}), http.StatusAccepted)
	state := stateOf(t, re)

	raw := readRaw(t, h.do(http.MethodGet, "/connectors/gh", h.adminToken, nil), http.StatusOK)
	var d gen.ConnectorDetail
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	c := d.Connector
	if c.Id != "gh" || c.Url != f.protected.URL || deref(c.Auth) != "oauth" || deref(c.ClientId) != "pre-registered" ||
		!d.Trusted || c.State != re.Connector.State || c.ToolCount != 0 || len(d.Tools) != 0 {
		t.Errorf("gh = %s", raw)
	}
	for _, leak := range []string{state, connSecret, "verifier", "secret", *re.AuthorizationUrl} {
		if strings.Contains(raw, leak) {
			t.Errorf("the detail answers %q: %s", leak, raw)
		}
	}
}

// An authorizing create through a pre-registered client stores the auth
// metadata but no grant yet: the detail answers the configured client and
// no auth_metadata.client_id.
func TestConnectorGetAuthorizingHasNoGrantClient(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	h.createConnector(f, func(in *gen.ConnectorCreate) { in.ClientId = ptr("pre-registered") })

	raw := readRaw(t, h.do(http.MethodGet, "/connectors/gh", h.adminToken, nil), http.StatusOK)
	var d struct {
		gen.ConnectorDetail
		AuthMetadata map[string]any `json:"auth_metadata"`
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	if d.Connector.State != "authorizing" || deref(d.Connector.ClientId) != "pre-registered" {
		t.Errorf("gh = %s", raw)
	}
	if d.AuthMetadata == nil || d.AuthMetadata["issuer"] == nil || d.AuthMetadata["resource"] != f.protected.URL {
		t.Fatalf("auth_metadata = %s", raw)
	}
	if v, ok := d.AuthMetadata["client_id"]; ok {
		t.Errorf("auth_metadata.client_id = %v before any grant: %s", v, raw)
	}
}

// listFails is a backend whose List fails.
type listFails struct{ ConnectorBackend }

func (listFails) List(context.Context) ([]connector.Connector, error) {
	return nil, errors.New("FAKE list failure")
}

func TestConnectorListFails(t *testing.T) {
	h := newHarness(t)
	h.b.Connectors = listFails{}
	errorBody(t, h.do(http.MethodGet, "/connectors", h.adminToken, nil), http.StatusInternalServerError, CodeInternal)
}

func TestConnectorGetUnknown(t *testing.T) {
	h := newHarness(t)
	h.withConnectors()
	errorBody(t, h.do(http.MethodGet, "/connectors/nope", h.adminToken, nil), http.StatusNotFound, CodeNotFound)
}

func TestConnectorReadsNeedAdmin(t *testing.T) {
	h := newHarness(t)
	h.withConnectors()
	for _, path := range []string{"/connectors", "/connectors/gh"} {
		for _, token := range []string{h.ownerToken} {
			errorBody(t, h.do(http.MethodGet, path, token, nil), http.StatusForbidden, CodeForbidden)
		}
	}
}

func TestConnectorReadsNotImplemented(t *testing.T) {
	h := newHarness(t)
	for _, path := range []string{"/connectors", "/connectors/gh"} {
		errorBody(t, h.do(http.MethodGet, path, h.adminToken, nil), http.StatusNotImplemented, CodeNotImplemented)
	}
}
