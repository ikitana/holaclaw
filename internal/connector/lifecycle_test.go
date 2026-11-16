package connector

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
	"holaclaw/internal/connector/mcp"
	"holaclaw/internal/connector/mcp/mcptest"
	"holaclaw/internal/connector/oauth"
	"holaclaw/internal/connector/oauth/oauthtest"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// The whole OAuth connect: discovery, CIMD, code, tokens in auth.toml,
// initialize and tools/list, connectors.toml, connected; then a call.
func TestConnectOAuth(t *testing.T) {
	h := newHarness(t)
	s := h.create("gh", pinned)
	if clientID(t, s) != oauth.CIMDClientID || s.ExpiresAt.Sub(h.clock.Now()) != IntentTTL {
		t.Errorf("client id %q, expires %v", clientID(t, s), s.ExpiresAt)
	}
	if s.Connector.Metadata == nil || s.Connector.IdentityKind != oauth.IdentityCIMD {
		t.Errorf("connector while authorizing: %+v", s.Connector)
	}
	if _, ok := h.fileConnectors()["gh"]; ok {
		t.Error("connectors.toml written before the code")
	}

	c, err := h.m.Code(h.ctx, "gh", approve(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if c.ServerName != "fake" || c.ServerVersion != "2.0" || len(c.Tools) != 1 || c.Tools[0].Name != "echo" ||
		!slices.Equal(c.GrantedScopes, []string{"repo"}) || c.TokenExpiry == nil {
		t.Errorf("connected: %+v", c)
	}
	if h.secret("gh", secrets.ConnectorAccessToken) != "FAKEaccess1" || h.secret("gh", secrets.ConnectorRefreshToken) != "FAKErefresh1" {
		t.Error("tokens not in auth.toml")
	}
	if e := h.fileConnectors()["gh"]; e.URL != h.srv.URL || e.Auth != "oauth" || e.Scopes != nil {
		t.Errorf("connectors.toml entry = %+v", e)
	}
	row, err := h.st.GetConnector(h.ctx, "gh")
	if err != nil || row.State != store.ConnectorConnected || row.ClientID != oauth.CIMDClientID || parseAuthMeta(row.AuthMeta) == nil {
		t.Errorf("row = %+v, %v", row, err)
	}
	want := []string{"pending/", "authorizing/", "connected/"}
	if got := h.events.statuses("gh"); !slices.Equal(got, want) {
		t.Errorf("events = %v", got)
	}
	if g := h.is.last(); g.Get("resource") != h.srv.URL || g.Get("client_id") != oauth.CIMDClientID || g.Get("code_verifier") == "" {
		t.Errorf("token request = %v", g)
	}

	res, err := h.m.CallTool(h.ctx, "gh", "echo", nil)
	if err != nil || res.Text != "hello" {
		t.Errorf("call = %+v, %v", res, err)
	}
	if list, _ := h.m.List(h.ctx); len(list) != 1 || list[0].ID != "gh" {
		t.Errorf("list = %+v", list)
	}

	// The scopes of the last event are the granted ones.
	if !strings.Contains(h.events.all(), `"scopes":["repo"]`) {
		t.Error("scopes missing from the connected event")
	}
}

// An open server connects at create, with no intent and no auth.toml entry.
func TestOpenServer(t *testing.T) {
	h := newHarness(t)
	h.g.set(func(g *guard) { g.open = true })
	s, err := h.m.Create(h.ctx, CreateRequest{ID: "homes", URL: h.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if s.AuthorizationURL != "" || s.Connector.State != store.ConnectorConnected || s.Connector.Auth != "none" || len(s.Connector.Tools) != 1 {
		t.Errorf("started = %+v", s)
	}
	if e := h.fileConnectors()["homes"]; e.Auth != "none" {
		t.Errorf("entry = %+v", e)
	}
	if len(h.sec.Keys(secrets.Connectors, "homes")) != 0 || h.as.Count(oauthtest.PathPRM) != 0 {
		t.Error("an open server touched auth")
	}
	if _, err := h.m.Code(h.ctx, "homes", oauth.Callback{}); !errors.Is(err, ErrNoIntent) {
		t.Errorf("code on an open server: %v", err)
	}
	if res, err := h.m.CallTool(h.ctx, "homes", "echo", nil); err != nil || res.Text != "hello" {
		t.Errorf("call = %v", err)
	}

	// The server starts asking for a token: degraded at once, with the reason.
	h.g.set(func(g *guard) { g.open = false })
	h.m.Check(h.ctx)
	if st, d := h.state("homes"); st != store.ConnectorDegraded || d != DetailOpenUnauthorized {
		t.Errorf("after 401: %s/%s", st, d)
	}
	if _, err := h.m.CallTool(h.ctx, "homes", "echo", nil); !errors.Is(err, mcp.ErrUnauthorized) || strings.Contains(err.Error(), "SERVERTEXT") {
		t.Errorf("call on 401: %v", err)
	}
	h.g.set(func(g *guard) { g.open = true })
	h.m.Check(h.ctx)
	if st, _ := h.state("homes"); st != store.ConnectorConnected {
		t.Errorf("after recovery: %s", st)
	}

	// Reconnect of an open server redials.
	if s, err := h.m.Reconnect(h.ctx, "homes", ReconnectRequest{}); err != nil || s.Connector.State != store.ConnectorConnected {
		t.Errorf("reconnect open: %+v %v", s, err)
	}
}

// A server that contradicts the requested auth fails the create early.
func TestAuthMismatch(t *testing.T) {
	h := newHarness(t)
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "a", URL: h.srv.URL, Auth: "none"}); !errors.Is(err, ErrAuthMismatch) {
		t.Errorf("none on a protected server: %v", err)
	}
	if h.events.last("a") != "deleted/"+DetailAuthMismatch {
		t.Errorf("events = %v", h.events.statuses("a"))
	}
	if _, err := h.m.Get(h.ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("left after failed create: %v", err)
	}

	h.g.set(func(g *guard) { g.open = true })
	h.as.Handle(oauthtest.PathPRM, http.NotFound)
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "b", URL: h.srv.URL, Auth: "oauth", RedirectURI: pinned}); !errors.Is(err, ErrAuthMismatch) {
		t.Errorf("oauth on an open server: %v", err)
	}
}

func TestCreateRefusals(t *testing.T) {
	h := newHarness(t)
	for name, r := range map[string]CreateRequest{
		"bad id":          {ID: "Bad_ID", URL: h.srv.URL},
		"long id":         {ID: strings.Repeat("a", 33), URL: h.srv.URL},
		"plain http":      {ID: "a", URL: "http://example.com/mcp"},
		"http on 127.x":   {ID: "a", URL: "http://127.evil.com/mcp"},
		"bad auth":        {ID: "a", URL: h.srv.URL, Auth: "basic"},
		"secret alone":    {ID: "a", URL: h.srv.URL, ClientSecret: "FAKEs"},
		"client id, none": {ID: "a", URL: h.srv.URL, Auth: "none", ClientID: "x"},
		"bad scope":       {ID: "a", URL: h.srv.URL, Scopes: []string{"a b"}},
	} {
		if _, err := h.m.Create(h.ctx, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "a", URL: h.srv.URL, Auth: "oauth"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("no redirect: %v", err)
	}
	// Discovery decides that a redirect is needed.
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "a", URL: h.srv.URL}); !errors.Is(err, ErrInvalid) {
		t.Errorf("no redirect, auth from discovery: %v", err)
	}
	if h.events.last("a") != "deleted/"+DetailClientIdentity {
		t.Errorf("events = %v", h.events.statuses("a"))
	}
	for _, r := range []string{
		"http://localhost:26500/callback", "https://127.0.0.1:26500/cb", "http://127.0.0.1/cb",
		"http://127.0.0.1:0/cb", "http://127.0.0.1:26500/cb?x=1", "http://127.0.0.1:26500/cb#f",
		"http://u@127.0.0.1:26500/cb", "http://10.0.0.1:26500/cb", "http://127.0.0.1:26500/" + strings.Repeat("a", 512),
	} {
		if _, err := h.m.Create(h.ctx, CreateRequest{ID: "r", URL: h.srv.URL, RedirectURI: r}); !errors.Is(err, ErrInvalid) {
			t.Errorf("redirect %q: %v", r, err)
		}
	}
	if !validRedirect("http://[::1]:26500/callback") || !validRedirect(pinned) || h.events.last("r") != "" {
		t.Error("loopback redirect refused, or a refused one reached the network")
	}

	// Unreachable server.
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "u", URL: "http://127.0.0.1:1/mcp", RedirectURI: pinned}); !errors.Is(err, ErrUnreachable) {
		t.Errorf("unreachable: %v", err)
	}
	if h.events.last("u") != "deleted/"+DetailUnreachable {
		t.Errorf("events = %v", h.events.statuses("u"))
	}

	// Discovery failure: the authorization server has no S256.
	h.as.ASM["code_challenge_methods_supported"] = []string{"plain"}
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "d", URL: h.srv.URL, RedirectURI: pinned}); !errors.Is(err, ErrDiscovery) || !errors.Is(err, oauth.ErrNoS256) {
		t.Errorf("no S256: %v", err)
	}
	if h.events.last("d") != "deleted/"+DetailDiscovery {
		t.Errorf("events = %v", h.events.statuses("d"))
	}
}

func TestValidURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://mcp.example.com/mcp":    true,
		"https://127.evil.com/mcp":       true,
		"http://127.0.0.1/mcp":           true,
		"http://127.1.2.3:8080/mcp":      true,
		"http://[::1]/mcp":               true,
		"http://[::1]:8080/mcp":          true,
		"http://localhost:9000/mcp":      true,
		"http://LOCALHOST:9000/mcp":      true,
		"http://127.evil.com/mcp":        false,
		"http://127.0.0.1.nip.io/mcp":    false,
		"http://localhost.evil.example/": false,
		"http://10.0.0.1/mcp":            false,
		"http://example.com/mcp":         false,
		"ftp://127.0.0.1/mcp":            false,
		"http://u@127.0.0.1/mcp":         false,
		"http://127.0.0.1/mcp#f":         false,
		"http:///mcp":                    false,
	} {
		if got := validURL(raw, false); got != want {
			t.Errorf("validURL(%q, false) = %v", raw, got)
		}
	}
	// insecure lets http reach any host, never a malformed URL.
	for raw, want := range map[string]bool{
		"http://127.evil.com/mcp": true,
		"http://10.0.0.1/mcp":     true,
		"ftp://10.0.0.1/mcp":      false,
		"http://u@10.0.0.1/mcp":   false,
	} {
		if got := validURL(raw, true); got != want {
			t.Errorf("validURL(%q, true) = %v", raw, got)
		}
	}
}

func TestDuplicateID(t *testing.T) {
	h := newHarness(t)
	h.create("gh", pinned)
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "gh", URL: h.srv.URL, RedirectURI: pinned}); !errors.Is(err, ErrExists) {
		t.Errorf("during intent: %v", err)
	}
	h.connect("other", pinned)
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "other", URL: h.srv.URL, RedirectURI: pinned}); !errors.Is(err, ErrExists) {
		t.Errorf("connected: %v", err)
	}
	// Written by hand after start: still in use.
	h.writeConnectors(config.Connector{ID: "late", URL: "https://example.com/", Auth: "none"},
		config.Connector{ID: "other", URL: h.srv.URL, Auth: "oauth"})
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "late", URL: h.srv.URL, RedirectURI: pinned}); !errors.Is(err, ErrExists) {
		t.Errorf("hand-written: %v", err)
	}
	if _, err := h.m.Get(h.ctx, "late"); !errors.Is(err, ErrNotFound) {
		t.Errorf("refused create left a connector: %v", err)
	}
}

// The intent lives IntentTTL: a create is dropped, a reconnect returns to
// its state.
func TestIntentTTL(t *testing.T) {
	h := newHarness(t)
	s := h.create("gh", pinned)
	h.clock.advance(IntentTTL)
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrNoIntent) {
		t.Errorf("code after TTL: %v", err)
	}
	if h.events.last("gh") != "deleted/"+DetailIntentExpired {
		t.Errorf("events = %v", h.events.statuses("gh"))
	}
	if _, err := h.st.GetConnector(h.ctx, "gh"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("row left: %v", err)
	}
	if h.is.count("authorization_code") != 0 {
		t.Error("an expired intent was exchanged")
	}

	// Swept by the health check too, and by List.
	h.create("gh2", pinned)
	h.clock.advance(IntentTTL + time.Second)
	h.m.Check(h.ctx)
	if h.events.last("gh2") != "deleted/"+DetailIntentExpired {
		t.Errorf("events = %v", h.events.statuses("gh2"))
	}

	h.connect("rc", pinned)
	s, err := h.m.Reconnect(h.ctx, "rc", ReconnectRequest{RedirectURI: pinned})
	if err != nil {
		t.Fatal(err)
	}
	h.clock.advance(IntentTTL)
	if list, _ := h.m.List(h.ctx); len(list) != 1 || list[0].State != store.ConnectorConnected || list[0].Detail != DetailIntentExpired {
		t.Errorf("after reconnect TTL: %+v", list)
	}
	if _, err := h.m.Code(h.ctx, "rc", approve(t, s)); !errors.Is(err, ErrNoIntent) {
		t.Errorf("code after reconnect TTL: %v", err)
	}
}

// A state mismatch drops the intent: the right code cannot follow.
func TestStateMismatch(t *testing.T) {
	h := newHarness(t)
	s := h.create("gh", pinned)
	cb := approve(t, s)
	bad := cb
	bad.State = "another"
	_, err := h.m.Code(h.ctx, "gh", bad)
	if !errors.Is(err, ErrStateMismatch) || strings.Contains(err.Error(), fakeHit) {
		t.Errorf("mismatch: %v", err)
	}
	if _, err := h.m.Code(h.ctx, "gh", cb); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrNoIntent) {
		t.Errorf("replay after mismatch: %v", err)
	}
	if h.is.count("authorization_code") != 0 {
		t.Error("exchanged after a mismatch")
	}
	if h.events.last("gh") != "deleted/"+DetailStateMismatch {
		t.Errorf("events = %v", h.events.statuses("gh"))
	}
	// Single use: a second code on a used intent.
	s = h.create("gh", pinned)
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); !errors.Is(err, ErrNoIntent) {
		t.Errorf("second code: %v", err)
	}
	// A refused authorization.
	s = h.create("gh3", pinned)
	cb = approve(t, s)
	cb.Code, cb.Error = "", "access_denied"
	if _, err := h.m.Code(h.ctx, "gh3", cb); !errors.Is(err, ErrAuthorization) {
		t.Errorf("access denied: %v", err)
	}
	if h.events.last("gh3") != "deleted/"+DetailAuthorization {
		t.Errorf("events = %v", h.events.statuses("gh3"))
	}
}

// A used intent stuck in its exchange is swept usedGrace past its TTL.
func TestSweepUsedIntent(t *testing.T) {
	h := newHarness(t)
	h.create("gh", pinned)
	c, _ := h.m.conn("gh")
	c.mu.Lock()
	c.intent.used = true
	c.mu.Unlock()
	h.clock.advance(IntentTTL)
	if _, err := h.m.Get(h.ctx, "gh"); err != nil {
		t.Fatalf("swept while its exchange may run: %v", err)
	}
	h.clock.advance(usedGrace)
	if list, _ := h.m.List(h.ctx); len(list) != 0 || h.events.last("gh") != "deleted/"+DetailIntentExpired {
		t.Errorf("list %+v, events %v", list, h.events.statuses("gh"))
	}
}

// The old tokens serve until the new code is exchanged.
func TestReconnectKeepsOldTokens(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	s, err := h.m.Reconnect(h.ctx, "gh", ReconnectRequest{Scopes: []string{"repo", "issues:write"}, RedirectURI: pinned})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.AuthorizationURL, "scope=repo+issues%3Awrite") {
		t.Errorf("url scopes: %s", s.AuthorizationURL)
	}
	if res, err := h.m.CallTool(h.ctx, "gh", "echo", nil); err != nil || res.Text != "hello" {
		t.Errorf("call during reconnect: %v", err)
	}
	if h.secret("gh", secrets.ConnectorAccessToken) != "FAKEaccess1" {
		t.Error("old token replaced before the code")
	}
	// A second reconnect replaces the intent.
	s2, err := h.m.Reconnect(h.ctx, "gh", ReconnectRequest{RedirectURI: pinned})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); !errors.Is(err, ErrStateMismatch) {
		t.Errorf("code of the replaced intent: %v", err)
	}
	if st, d := h.state("gh"); st != store.ConnectorConnected || d != DetailStateMismatch {
		t.Errorf("after mismatch on reconnect: %s/%s", st, d)
	}
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s2)); !errors.Is(err, ErrNoIntent) {
		t.Errorf("the mismatch dropped the intent: %v", err)
	}

	s, err = h.m.Reconnect(h.ctx, "gh", ReconnectRequest{Scopes: []string{"repo", "issues:write"}, RedirectURI: pinned})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); err != nil {
		t.Fatal(err)
	}
	if h.secret("gh", secrets.ConnectorAccessToken) != "FAKEaccess2" {
		t.Error("new token not stored")
	}
	if e := h.fileConnectors()["gh"]; !slices.Equal(e.Scopes, []string{"repo", "issues:write"}) {
		t.Errorf("scopes not written: %+v", e)
	}
	want := []string{"pending/", "authorizing/", "connected/", "authorizing/", "connected/" + DetailStateMismatch, "authorizing/", "connected/"}
	if got := h.events.statuses("gh"); !slices.Equal(got, want) {
		t.Errorf("events = %v", got)
	}
}

// A registered client is reused for its own redirect only, and survives a
// restart for refresh.
func TestDCRReuse(t *testing.T) {
	h := newHarness(t)
	h.as.ASM["client_id_metadata_document_supported"] = false
	h.connect("gh", other)
	if h.registrations() != 1 {
		t.Fatalf("registrations = %d", h.registrations())
	}
	if c, _ := h.m.Get(h.ctx, "gh"); c.IdentityKind != oauth.IdentityDCR {
		t.Errorf("kind = %s", c.IdentityKind)
	}
	s, err := h.m.Reconnect(h.ctx, "gh", ReconnectRequest{RedirectURI: other})
	if err != nil {
		t.Fatal(err)
	}
	if h.registrations() != 1 || clientID(t, s) != "dcr-client-1" {
		t.Errorf("same redirect: registrations %d, client %s", h.registrations(), clientID(t, s))
	}
	s, err = h.m.Reconnect(h.ctx, "gh", ReconnectRequest{RedirectURI: other2})
	if err != nil {
		t.Fatal(err)
	}
	if h.registrations() != 2 || clientID(t, s) != "dcr-client-2" {
		t.Errorf("other redirect: registrations %d, client %s", h.registrations(), clientID(t, s))
	}
	// Until the code, the old grant's client refreshes.
	if row, _ := h.st.GetConnector(h.ctx, "gh"); row.ClientID != "dcr-client-1" {
		t.Errorf("client id before code = %s", row.ClientID)
	}
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); err != nil {
		t.Fatal(err)
	}

	// After a restart the identity is rebuilt from the row.
	h.restart()
	h.clock.advance(59*time.Minute + 30*time.Second)
	if _, err := h.m.CallTool(h.ctx, "gh", "echo", nil); err != nil {
		t.Fatal(err)
	}
	if g := h.is.last(); g.Get("grant_type") != "refresh_token" || g.Get("client_id") != "dcr-client-2" {
		t.Errorf("refresh after restart = %v", g)
	}
	if am := parseAuthMeta(mustRow(t, h, "gh").AuthMeta); am == nil || am.RegisteredRedirect != other2 || am.IdentityKind != oauth.IdentityDCR {
		t.Errorf("auth meta = %+v", am)
	}
}

// A refused metadata document is remembered: the next intent registers.
func TestCIMDRefused(t *testing.T) {
	h := newHarness(t)
	s := h.create("gh", pinned)
	h.is.setFail("invalid_client")
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); !errors.Is(err, ErrAuthorization) || !errors.Is(err, oauth.ErrCIMDRefused) {
		t.Fatalf("refused: %v", err)
	}
	if h.events.last("gh") != "deleted/"+DetailCIMDRefused {
		t.Errorf("events = %v", h.events.statuses("gh"))
	}
	h.is.setFail("")
	s = h.create("gh", pinned)
	if clientID(t, s) != "dcr-client-1" {
		t.Errorf("after refusal: client %s", clientID(t, s))
	}
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); err != nil {
		t.Fatal(err)
	}
	if !mustRow(t, h, "gh").CIMDRefused {
		t.Error("refusal not in the row")
	}

	// On a reconnect the refusal lands in the row of the connector.
	h2 := newHarness(t)
	h2.connect("x", pinned)
	s, err := h2.m.Reconnect(h2.ctx, "x", ReconnectRequest{RedirectURI: pinned})
	if err != nil {
		t.Fatal(err)
	}
	h2.is.setFail("unauthorized_client")
	if _, err := h2.m.Code(h2.ctx, "x", approve(t, s)); !errors.Is(err, oauth.ErrCIMDRefused) {
		t.Fatalf("reconnect refused: %v", err)
	}
	if r := mustRow(t, h2, "x"); !r.CIMDRefused || r.State != store.ConnectorConnected || r.Detail != DetailCIMDRefused {
		t.Errorf("row = %+v", r)
	}
	h2.is.setFail("")
	h2.restart()
	s, err = h2.m.Reconnect(h2.ctx, "x", ReconnectRequest{RedirectURI: pinned})
	if err != nil || clientID(t, s) != "dcr-client-1" {
		t.Errorf("after restart: %v %s", err, clientID(t, s))
	}
}

// The stored secret of a pre-registered client goes back only to its own
// issuer and client.
func TestPreRegisteredSecret(t *testing.T) {
	h := newHarness(t)
	s, err := h.m.Create(h.ctx, CreateRequest{ID: "gh", URL: h.srv.URL, RedirectURI: pinned, ClientID: "Iv1.app", ClientSecret: "FAKEappsecret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); err != nil {
		t.Fatal(err)
	}
	if h.secret("gh", secrets.ConnectorClientSecret) != "FAKEappsecret" || h.fileConnectors()["gh"].ClientID != "Iv1.app" {
		t.Fatal("pre-registered client not stored")
	}
	// Same issuer: the stored secret is reused.
	if s, err = h.m.Reconnect(h.ctx, "gh", ReconnectRequest{RedirectURI: pinned}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); err != nil {
		t.Fatal(err)
	}
	if got := h.as.Last(t, oauthtest.PathToken).Header.Get("Authorization"); got != basic("Iv1.app", "FAKEappsecret") {
		t.Errorf("token auth = %q", got)
	}

	// The resource moves to another authorization server.
	as2 := oauthtest.New(t)
	h.as.PRM["authorization_servers"] = []string{as2.URL}
	_, err = h.m.Reconnect(h.ctx, "gh", ReconnectRequest{RedirectURI: pinned})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "client_secret is required") {
		t.Fatalf("new issuer without a secret: %v", err)
	}
	if as2.Count(oauthtest.PathToken) != 0 {
		t.Error("the old secret reached the new issuer")
	}
	if st, d := h.state("gh"); st != store.ConnectorConnected || d != DetailClientIdentity {
		t.Errorf("state = %s/%s", st, d)
	}
	if s, err = h.m.Reconnect(h.ctx, "gh", ReconnectRequest{RedirectURI: pinned, ClientSecret: "FAKEnewsecret"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); err != nil {
		t.Fatal(err)
	}
	if got := as2.Last(t, oauthtest.PathToken).Header.Get("Authorization"); got != basic("Iv1.app", "FAKEnewsecret") {
		t.Errorf("token auth at the new issuer = %q", got)
	}
	if h.secret("gh", secrets.ConnectorClientSecret) != "FAKEnewsecret" {
		t.Error("new secret not stored")
	}
	if _, err := h.m.Reconnect(h.ctx, "gh", ReconnectRequest{RedirectURI: pinned, Scopes: []string{"a b"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad scopes: %v", err)
	}
}

// A client id added by hand to a DCR connector does not get the DCR secret:
// without a secret in the request it is a public client.
func TestPreRegisteredKindSwitch(t *testing.T) {
	h := newHarness(t)
	h.as.ASM["client_id_metadata_document_supported"] = false
	h.as.Handle(oauthtest.PathRegister, oauthtest.JSONHandler(http.StatusCreated, `{"client_id":"dcr-x","client_secret":"FAKEdcrsecret"}`))
	h.connect("gh", other)
	if _, err := h.m.Reconnect(h.ctx, "gh", ReconnectRequest{RedirectURI: other, ClientSecret: "FAKEx"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("secret without a client id: %v", err)
	}
	conns := h.fileConnectors()
	cfg := conns["gh"]
	cfg.ClientID = "Iv1.app"
	conns["gh"] = cfg
	_ = h.m.Close()
	h.m = h.start(conns)
	s, err := h.m.Reconnect(h.ctx, "gh", ReconnectRequest{RedirectURI: other})
	if err != nil || clientID(t, s) != "Iv1.app" {
		t.Fatalf("kind switch without a secret: %v", err)
	}
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); err != nil {
		t.Fatal(err)
	}
	r := h.as.Last(t, oauthtest.PathToken)
	if r.Header.Get("Authorization") != "" || strings.Contains(string(r.Body), "FAKEdcrsecret") || h.is.last().Get("client_id") != "Iv1.app" {
		t.Errorf("public client token request: %v %s", r.Header, r.Body)
	}
	if h.secret("gh", secrets.ConnectorClientSecret) != "" {
		t.Error("the DCR secret outlived the switch")
	}

	// A pre-registered secret of another client still needs a new one.
	conns = h.fileConnectors()
	cfg = conns["gh"]
	cfg.ClientID = "Iv1.other"
	conns["gh"] = cfg
	if err := h.sec.Set(secrets.Connectors, "gh", map[string]string{secrets.ConnectorClientSecret: "FAKEapp"}); err != nil {
		t.Fatal(err)
	}
	_ = h.m.Close()
	h.m = h.start(conns)
	if _, err := h.m.Reconnect(h.ctx, "gh", ReconnectRequest{RedirectURI: other}); !errors.Is(err, ErrInvalid) {
		t.Errorf("other client without a secret: %v", err)
	}
}

// An entry written by hand is picked up at start and needs a reconnect for
// credentials; an open one is connected by the first check.
func TestHandWrittenEntry(t *testing.T) {
	h := newHarness(t)
	h.g.set(func(g *guard) { g.ok["FAKEnever"] = true })
	h.writeConnectors(config.Connector{ID: "gh", URL: h.srv.URL, Auth: "oauth", Scopes: []string{"repo"}, Trusted: true})
	h.restart()
	if st, d := h.state("gh"); st != store.ConnectorExpired || d != DetailNeedsReconnect {
		t.Fatalf("hand-written oauth: %s/%s", st, d)
	}
	if _, err := h.m.CallTool(h.ctx, "gh", "echo", nil); !errors.Is(err, ErrNeedsReconnect) {
		t.Errorf("call without credentials: %v", err)
	}
	s, err := h.m.Reconnect(h.ctx, "gh", ReconnectRequest{RedirectURI: pinned})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.AuthorizationURL, "scope=repo") || s.Connector.State != store.ConnectorAuthorizing {
		t.Errorf("reconnect: %+v", s)
	}
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); err != nil {
		t.Fatal(err)
	}
	e := h.fileConnectors()["gh"]
	if !e.Trusted || !slices.Equal(e.Scopes, []string{"repo"}) || e.Auth != "oauth" {
		t.Errorf("rewritten entry lost keys: %+v", e)
	}
	if c, _ := h.m.Get(h.ctx, "gh"); !c.Trusted || c.State != store.ConnectorConnected {
		t.Errorf("after code: %+v", c)
	}

	// An open entry by hand: pending until the first check.
	h.g.set(func(g *guard) { g.open = true })
	conns := h.fileConnectors()
	conns["homes"] = config.Connector{ID: "homes", URL: h.srv.URL, Auth: "none"}
	_ = h.m.Close()
	h.m = h.start(conns)
	if st, _ := h.state("homes"); st != store.ConnectorPending {
		t.Errorf("open by hand: %s", st)
	}
	h.m.Check(h.ctx)
	if c, _ := h.m.Get(h.ctx, "homes"); c.State != store.ConnectorConnected || len(c.Tools) != 1 {
		t.Errorf("open after check: %+v", c)
	}

	// Its row survives a restart; a row whose entry is gone does not.
	delete(conns, "homes")
	_ = h.m.Close()
	h.m = h.start(conns)
	if _, err := h.st.GetConnector(h.ctx, "homes"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("orphan row kept: %v", err)
	}
	if st, _ := h.state("gh"); st != store.ConnectorConnected {
		t.Errorf("gh after restart: %s", st)
	}

	// The URL changed by hand: the tokens are bound to the old one.
	conns["gh"] = config.Connector{ID: "gh", URL: h.srv.URL + "/other", Auth: "oauth"}
	_ = h.m.Close()
	h.m = h.start(conns)
	if st, d := h.state("gh"); st != store.ConnectorExpired || d != DetailURLChanged {
		t.Errorf("url changed: %s/%s", st, d)
	}
}

// Home Assistant's shape: the server at /api/mcp names its origin as the
// resource. It connects, the token is asked for the origin, a restart
// keeps it, and only a change of the URL itself needs a reconnect.
func TestResourceIsOrigin(t *testing.T) {
	h := newHarness(t)
	u := h.srv.URL + "/api/mcp"
	s, err := h.m.Create(h.ctx, CreateRequest{ID: "ha", URL: u, Auth: "oauth", RedirectURI: pinned})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, _ := url.Parse(s.AuthorizationURL); got.Query().Get("resource") != h.srv.URL {
		t.Errorf("authorize resource = %q", got.Query().Get("resource"))
	}
	if c, err := h.m.Code(h.ctx, "ha", approve(t, s)); err != nil || c.State != store.ConnectorConnected {
		t.Fatalf("code: %+v, %v", c, err)
	}
	if r := h.as.Last(t, oauthtest.PathToken); r.Form.Get("resource") != h.srv.URL {
		t.Errorf("token resource = %q", r.Form.Get("resource"))
	}

	h.restart()
	if st, d := h.state("ha"); st != store.ConnectorConnected {
		t.Errorf("after restart: %s/%s", st, d)
	}
	_ = h.m.Close()
	h.m = h.start(map[string]config.Connector{"ha": {ID: "ha", URL: h.srv.URL + "/api/other", Auth: "oauth"}})
	if st, d := h.state("ha"); st != store.ConnectorExpired || d != DetailURLChanged {
		t.Errorf("url changed: %s/%s", st, d)
	}
}

// allow_insecure_transport: an open server whose certificate does not
// verify connects only with it, from the file or a create, and a reconnect
// keeps the key.
func TestAllowInsecureTransport(t *testing.T) {
	h := newHarness(t)
	h.g.set(func(g *guard) { g.open = true })
	// Not httptest's own certificate, which the harness's client trusts.
	self := httptest.NewUnstartedServer(h.srv.Config.Handler)
	self.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}}
	self.StartTLS()
	t.Cleanup(self.Close) // after the restarted manager's Close
	h.writeConnectors(
		config.Connector{ID: "strict", URL: self.URL, Auth: "none"},
		config.Connector{ID: "lab", URL: self.URL, Auth: "none", AllowInsecureTransport: true},
	)
	h.restart()
	h.m.Check(h.ctx)
	if c, _ := h.m.Get(h.ctx, "lab"); c.State != store.ConnectorConnected || len(c.Tools) != 1 {
		t.Errorf("insecure: %+v", c)
	}
	if c, _ := h.m.Get(h.ctx, "strict"); c.State == store.ConnectorConnected {
		t.Errorf("unverified certificate accepted: %+v", c)
	}
	if _, err := h.m.Reconnect(h.ctx, "lab", ReconnectRequest{}); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if !h.fileConnectors()["lab"].AllowInsecureTransport {
		t.Error("reconnect dropped allow_insecure_transport")
	}

	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "made", URL: self.URL}); !errors.Is(err, ErrUnreachable) {
		t.Errorf("create, unverified certificate: %v", err)
	}
	st, err := h.m.Create(h.ctx, CreateRequest{ID: "made", URL: self.URL, AllowInsecureTransport: true})
	if err != nil || st.Connector.State != store.ConnectorConnected {
		t.Fatalf("create, insecure: %+v, %v", st, err)
	}
	if !h.fileConnectors()["made"].AllowInsecureTransport {
		t.Error("create did not write allow_insecure_transport")
	}
	if h.fileConnectors()["strict"].AllowInsecureTransport {
		t.Error("allow_insecure_transport on an entry without it")
	}
}

// allow_insecure_transport lifts CON003 for a create only: plain http on
// any host, still nothing but http or https.
func TestCreateInsecureURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://10.0.4.20:8765/mcp": true,
		"https://10.0.4.20/mcp":     true,
		"ftp://10.0.4.20/mcp":       false,
		"http://u@10.0.4.20/mcp":    false,
		"http:///mcp":               false,
	} {
		if got := validCreate(CreateRequest{ID: "lab", URL: raw, AllowInsecureTransport: true}) == nil; got != want {
			t.Errorf("%s: valid = %v, want %v", raw, got, want)
		}
	}
	if err := validCreate(CreateRequest{ID: "lab", URL: "http://10.0.4.20:8765/mcp"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("plain http without the key: %v", err)
	}
}

// selfSigned is a certificate for 127.0.0.1 that nothing trusts.
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// A hand-written pre-registered entry uses the secret the admin stored.
func TestHandWrittenPreRegistered(t *testing.T) {
	h := newHarness(t)
	h.writeConnectors(config.Connector{ID: "gh", URL: h.srv.URL, Auth: "oauth", ClientID: "Iv1.app"})
	if err := h.sec.Set(secrets.Connectors, "gh", map[string]string{secrets.ConnectorClientSecret: "FAKEadmin"}); err != nil {
		t.Fatal(err)
	}
	h.restart()
	s, err := h.m.Reconnect(h.ctx, "gh", ReconnectRequest{RedirectURI: pinned})
	if err != nil || clientID(t, s) != "Iv1.app" {
		t.Fatalf("reconnect: %v", err)
	}
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); err != nil {
		t.Fatal(err)
	}
	if got := h.as.Last(t, oauthtest.PathToken).Header.Get("Authorization"); got != basic("Iv1.app", "FAKEadmin") {
		t.Errorf("token auth = %q", got)
	}
}

// A row write that fails after the exchange gives the old grant its tokens
// back and ends the intent.
func TestStoreGrantRollback(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	s, err := h.m.Reconnect(h.ctx, "gh", ReconnectRequest{RedirectURI: pinned})
	if err != nil {
		t.Fatal(err)
	}
	h.fs.failWhen(failAll)
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); err == nil {
		t.Fatal("code succeeded with a failing store")
	}
	h.fs.failWhen(nil)
	if h.secret("gh", secrets.ConnectorAccessToken) != "FAKEaccess1" || h.secret("gh", secrets.ConnectorRefreshToken) != "FAKErefresh1" {
		t.Error("old tokens not restored")
	}
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); !errors.Is(err, ErrNoIntent) {
		t.Errorf("intent still open: %v", err)
	}
	if res, err := h.m.CallTool(h.ctx, "gh", "echo", nil); err != nil || res.Text != "hello" {
		t.Errorf("old grant does not serve: %v", err)
	}
	h.m.Check(h.ctx)
	if st, _ := h.state("gh"); st != store.ConnectorConnected {
		t.Errorf("after check: %s", st)
	}

	// A create leaves nothing.
	s = h.create("new", pinned)
	h.fs.failWhen(failAll)
	if _, err := h.m.Code(h.ctx, "new", approve(t, s)); err == nil {
		t.Fatal("code succeeded with a failing store")
	}
	h.fs.failWhen(nil)
	h.gone("new")
	if h.events.last("new") != "deleted/"+DetailCacheWrite {
		t.Errorf("events = %v", h.events.statuses("new"))
	}
}

// The row write of a ready create intent fails: the intent ends.
func TestOpenRowWriteFailure(t *testing.T) {
	h := newHarness(t)
	h.fs.failWhen(func(c store.Connector) bool { return c.State == store.ConnectorAuthorizing })
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "gh", URL: h.srv.URL, RedirectURI: pinned}); err == nil {
		t.Fatal("create succeeded")
	}
	if _, err := h.m.Get(h.ctx, "gh"); !errors.Is(err, ErrNotFound) || h.events.last("gh") != "deleted/"+DetailCacheWrite {
		t.Errorf("get %v, events %v", err, h.events.statuses("gh"))
	}
}

// Server-controlled JSON is capped before the row is written.
func TestCaps(t *testing.T) {
	h := newHarness(t)
	tools, meta := maxToolsJSON, maxAuthMetaJSON
	defer func() { maxToolsJSON, maxAuthMetaJSON = tools, meta }()

	h.g.set(func(g *guard) { g.open = true })
	maxToolsJSON = 10
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "big", URL: h.srv.URL}); !errors.Is(err, ErrUnreachable) {
		t.Errorf("tools over cap: %v", err)
	}
	if h.events.last("big") != "deleted/"+DetailToolsTooLarge {
		t.Errorf("events = %v", h.events.statuses("big"))
	}
	maxToolsJSON = tools
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "ok", URL: h.srv.URL}); err != nil {
		t.Fatal(err)
	}
	maxToolsJSON = 10
	for range maxFailures {
		h.m.Check(h.ctx)
	}
	if st, d := h.state("ok"); st != store.ConnectorDegraded || d != DetailToolsTooLarge {
		t.Errorf("check over cap: %s/%s", st, d)
	}
	maxToolsJSON = 6 << 20

	h.g.set(func(g *guard) { g.open = false })
	maxAuthMetaJSON = 10
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "meta", URL: h.srv.URL, RedirectURI: pinned}); !errors.Is(err, ErrDiscovery) {
		t.Errorf("auth meta over cap: %v", err)
	}
}

func TestInspect(t *testing.T) {
	h := newHarness(t)
	in, err := h.m.Inspect(h.ctx, h.srv.URL)
	if err != nil || in.Open || in.Metadata == nil || in.IdentityKind != oauth.IdentityCIMD {
		t.Errorf("cimd: %+v %v", in, err)
	}
	h.as.ASM["client_id_metadata_document_supported"] = false
	if in, err = h.m.Inspect(h.ctx, h.srv.URL); err != nil || in.IdentityKind != oauth.IdentityDCR {
		t.Errorf("dcr: %+v %v", in, err)
	}
	delete(h.as.ASM, "registration_endpoint")
	if in, err = h.m.Inspect(h.ctx, h.srv.URL); err != nil || in.IdentityKind != "" {
		t.Errorf("pre-registered only: %+v %v", in, err)
	}
	if h.registrations() != 0 {
		t.Error("inspect registered a client")
	}
	h.g.set(func(g *guard) { g.open = true })
	if in, err = h.m.Inspect(h.ctx, h.srv.URL); err != nil || !in.Open || in.ServerName != "fake" || len(in.Tools) != 1 {
		t.Errorf("open: %+v %v", in, err)
	}
	if _, err := h.m.Inspect(h.ctx, "http://127.0.0.1:1/mcp"); !errors.Is(err, ErrUnreachable) {
		t.Errorf("unreachable: %v", err)
	}
	if _, err := h.m.Inspect(h.ctx, "ftp://x"); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad url: %v", err)
	}
	if list, _ := h.m.List(h.ctx); len(list) != 0 {
		t.Errorf("inspect stored: %+v", list)
	}
}

// Delete works in every state and leaves nothing.
func TestDeleteInEachState(t *testing.T) {
	h := newHarness(t)

	h.connect("conn", pinned)
	h.connect("degr", pinned)
	h.connect("expi", pinned)
	h.g.set(func(g *guard) { g.down = true })
	for range maxFailures {
		h.m.Check(h.ctx)
	}
	h.g.set(func(g *guard) { g.down = false })
	if st, _ := h.state("degr"); st != store.ConnectorDegraded {
		t.Fatalf("degr: %s", st)
	}
	h.is.setFail("invalid_grant")
	h.clock.advance(55 * time.Minute)
	h.m.Check(h.ctx)
	if st, _ := h.state("expi"); st != store.ConnectorExpired {
		t.Fatalf("expi: %s", st)
	}
	// reconnecting: an open intent on an existing connector.
	h.is.setFail("")
	if _, err := h.m.Reconnect(h.ctx, "conn", ReconnectRequest{RedirectURI: pinned}); err != nil {
		t.Fatal(err)
	}
	h.create("authz", pinned)

	for _, id := range []string{"authz", "conn", "degr", "expi"} {
		if err := h.m.Delete(h.ctx, id); err != nil {
			t.Errorf("delete %s: %v", id, err)
		}
		h.gone(id)
		if h.events.last(id) != StateDeleted+"/" {
			t.Errorf("%s events = %v", id, h.events.statuses(id))
		}
	}
	if err := h.m.Delete(h.ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete unknown: %v", err)
	}
	// Written by hand after start: removed from the file.
	h.writeConnectors(config.Connector{ID: "late", URL: "https://example.com/", Auth: "none"})
	if err := h.m.Delete(h.ctx, "late"); err != nil {
		t.Errorf("delete hand-written: %v", err)
	}
	h.gone("late")
}

// The states TestDeleteInEachState cannot reach after start: an entry
// still pending its first check, and leftovers of an id the manager does
// not know (a row, auth.toml keys) with no entry.
func TestDeletePendingAndLeftovers(t *testing.T) {
	h := newHarness(t)
	h.writeConnectors(config.Connector{ID: "pend", URL: h.srv.URL, Auth: "none"})
	h.restart()
	if st, _ := h.state("pend"); st != store.ConnectorPending {
		t.Fatalf("pend: %s", st)
	}
	if err := h.m.Delete(h.ctx, "pend"); err != nil {
		t.Errorf("delete pending: %v", err)
	}
	h.gone("pend")

	if err := h.st.PutConnector(h.ctx, store.Connector{ID: "row", State: store.ConnectorConnected}); err != nil {
		t.Fatal(err)
	}
	if err := h.sec.Set(secrets.Connectors, "keys", map[string]string{secrets.ConnectorAccessToken: "FAKEaccess"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"row", "keys"} {
		if err := h.m.Delete(h.ctx, id); err != nil {
			t.Errorf("delete %s: %v", id, err)
		}
		h.gone(id)
		if h.events.last(id) != StateDeleted+"/" {
			t.Errorf("%s events = %v", id, h.events.statuses(id))
		}
	}
}

// kept asserts a failed delete changed nothing: the connector is listed
// connected, its row and auth.toml keys are there, and no event says
// deleted.
func (h *harness) kept(id string) {
	h.t.Helper()
	if c, err := h.m.Get(h.ctx, id); err != nil || c.State != store.ConnectorConnected {
		h.t.Errorf("after a failed delete: %+v %v", c, err)
	}
	if list, _ := h.m.List(h.ctx); len(list) != 1 {
		h.t.Error("not listed after a failed delete")
	}
	mustRow(h.t, h, id)
	if h.secret(id, secrets.ConnectorAccessToken) == "" {
		h.t.Error("auth.toml keys removed by a failed delete")
	}
	if !strings.Contains(h.events.all(), id) || slices.Contains(h.events.statuses(id), StateDeleted+"/") {
		h.t.Errorf("events = %v", h.events.statuses(id))
	}
}

// connectors.toml as a symlink: the delete removes nothing and says to
// edit the file; once the entry is gone by hand, the delete finishes.
func TestDeleteSymlinkedFile(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	path := filepath.Join(h.dir, config.FileConnectors)
	target := filepath.Join(t.TempDir(), "elsewhere.toml")
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	for range 2 { // a retry answers the same
		err := h.m.Delete(h.ctx, "gh")
		if !errors.Is(err, ErrHandManaged) || !strings.Contains(err.Error(), "remove [connectors.gh] there") {
			t.Fatalf("delete through a symlink: %v", err)
		}
		h.kept("gh")
	}
	// The person removes the entry from the file the link points to.
	if err := os.WriteFile(target, []byte(connectorsTOML(t)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Delete(h.ctx, "gh"); err != nil {
		t.Fatalf("delete after the hand edit: %v", err)
	}
	h.gone("gh")
	if h.events.last("gh") != StateDeleted+"/" {
		t.Errorf("events = %v", h.events.statuses("gh"))
	}
}

// A config directory that cannot be written: nothing is removed, and the
// retry once it can succeeds.
func TestDeleteUnwritableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	h := newHarness(t)
	h.connect("gh", pinned)
	if err := os.Chmod(h.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.dir, 0o700) })
	err := h.m.Delete(h.ctx, "gh")
	if err == nil || errors.Is(err, ErrHandManaged) || errors.Is(err, ErrNotFound) {
		t.Fatalf("delete in a read-only directory: %v", err)
	}
	h.kept("gh")
	if _, ok := h.fileConnectors()["gh"]; !ok {
		t.Error("entry removed by a failed delete")
	}
	if err := os.Chmod(h.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := h.m.Delete(h.ctx, "gh"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	h.gone("gh")
}

// Once the entry is gone the connector is: a row that cannot be removed
// still ends in "deleted", the error says so, and a retry removes it.
func TestDeleteLeftoverRow(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	h.fs.failDeletes(errors.New("store: disk full"))
	err := h.m.Delete(h.ctx, "gh")
	if !errors.Is(err, ErrLeftovers) || !strings.Contains(err.Error(), "delete gh again before creating it again") ||
		strings.Contains(err.Error(), "disk full") {
		t.Fatalf("delete with a failing store: %v", err)
	}
	if _, gerr := h.m.Get(h.ctx, "gh"); !errors.Is(gerr, ErrNotFound) {
		t.Errorf("get = %v", gerr)
	}
	if _, ok := h.fileConnectors()["gh"]; ok || h.secret("gh", secrets.ConnectorAccessToken) != "" {
		t.Error("entry or keys left")
	}
	if h.events.last("gh") != StateDeleted+"/" {
		t.Errorf("events = %v", h.events.statuses("gh"))
	}
	h.fs.failDeletes(nil)
	if err := h.m.Delete(h.ctx, "gh"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	h.gone("gh")
}

// Two deletes of one id: the second waits for the first and finds nothing,
// for a connector the manager knows and for leftovers it does not.
func TestDeleteTwice(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	if err := h.st.PutConnector(h.ctx, store.Connector{ID: "row", State: store.ConnectorConnected}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"gh", "row"} {
		inDelete, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		h.fs.beforeDelete(func(string) {
			once.Do(func() {
				close(inDelete)
				<-release
			})
		})
		first, second := make(chan error, 1), make(chan error, 1)
		go func() { first <- h.m.Delete(h.ctx, id) }()
		wait(t, inDelete, "the row delete")
		go func() { second <- h.m.Delete(h.ctx, id) }()
		select {
		case err := <-second:
			t.Fatalf("%s: second delete answered %v while the first held the id", id, err)
		case <-time.After(50 * time.Millisecond):
		}
		close(release)
		if err := <-first; err != nil {
			t.Errorf("%s: first delete: %v", id, err)
		}
		if err := <-second; !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: second delete: %v", id, err)
		}
		h.gone(id)
		if got := h.events.statuses(id); slices.Index(got, StateDeleted+"/") != len(got)-1 {
			t.Errorf("%s events = %v", id, got)
		}
	}
}

// Once the entry is gone a cancelled request still removes the rest.
func TestDeleteOutlivesItsContext(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	h.fs.beforeDelete(func(string) { cancel() })
	if err := h.m.Delete(ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	h.gone("gh")
	if h.events.last("gh") != StateDeleted+"/" {
		t.Errorf("events = %v", h.events.statuses("gh"))
	}
}

// A connectors.toml that does not parse refuses a delete, of a known id
// or not, with nothing removed and nothing of the file quoted.
func TestDeleteBrokenFile(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	const garbage = "this is = = not toml"
	h.writeFile(garbage)
	for _, id := range []string{"gh", "nope"} {
		if err := h.m.Delete(h.ctx, id); !errors.Is(err, ErrBadFile) || strings.Contains(err.Error(), "= =") {
			t.Errorf("delete %s: %v", id, err)
		}
	}
	h.kept("gh")
}

// A store that cannot tell whether an unknown id has a row fails the
// delete of its leftovers with nothing changed; the retry cleans them.
func TestDeleteLeftoversStoreFailure(t *testing.T) {
	h := newHarness(t)
	if err := h.sec.Set(secrets.Connectors, "keys", map[string]string{secrets.ConnectorAccessToken: "FAKEaccess"}); err != nil {
		t.Fatal(err)
	}
	h.fs.failGets(errors.New("store: disk full"))
	err := h.m.Delete(h.ctx, "keys")
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrLeftovers) {
		t.Fatalf("delete with a failing store: %v", err)
	}
	if h.secret("keys", secrets.ConnectorAccessToken) == "" || len(h.events.statuses("keys")) != 0 {
		t.Errorf("changed: keys %v, events %v", h.sec.Keys(secrets.Connectors, "keys"), h.events.statuses("keys"))
	}
	h.fs.failGets(nil)
	if err := h.m.Delete(h.ctx, "keys"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	h.gone("keys")
}

func TestDeleteDuringCode(t *testing.T) {
	h := newHarness(t)
	s := h.create("gh", pinned)
	blocked, release := h.g.block()
	defer release()
	done := make(chan error, 1)
	go func() {
		_, err := h.m.Code(h.ctx, "gh", approve(t, s))
		done <- err
	}()
	wait(t, blocked, "the dial after the exchange")
	if err := h.m.Delete(h.ctx, "gh"); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; !errors.Is(err, ErrNoIntent) {
		t.Errorf("code = %v", err)
	}
	h.gone("gh")
}

// connectors.toml that does not parse is never overwritten.
func TestBrokenConnectorsFile(t *testing.T) {
	h := newHarness(t)
	const garbage = "this is = = not toml"
	h.writeFile(garbage)
	h.g.set(func(g *guard) { g.open = true })
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "homes", URL: h.srv.URL}); err == nil {
		t.Error("create over a broken file")
	}
	h.g.set(func(g *guard) { g.open = false })
	_ = os.Remove(filepath.Join(h.dir, config.FileConnectors))
	s := h.create("gh", pinned)
	h.writeFile(garbage)
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); err == nil {
		t.Error("code over a broken file")
	}
	if b, _ := os.ReadFile(filepath.Join(h.dir, config.FileConnectors)); string(b) != garbage {
		t.Errorf("file touched: %q", b)
	}
	if h.events.last("gh") != "deleted/"+DetailConfigWrite || len(h.sec.Keys(secrets.Connectors, "gh")) != 0 {
		t.Errorf("events %v, keys %v", h.events.statuses("gh"), h.sec.Keys(secrets.Connectors, "gh"))
	}
}

// The rewrite is atomic, 0600, keeps the version and leaves no temp file.
func TestAtomicWrite(t *testing.T) {
	h := newHarness(t)
	h.writeConnectors()
	if err := os.Chmod(filepath.Join(h.dir, config.FileConnectors), 0o644); err != nil {
		t.Fatal(err)
	}
	h.connect("gh", pinned)
	info, err := os.Stat(filepath.Join(h.dir, config.FileConnectors))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v", info.Mode(), err)
	}
	if tmp, _ := filepath.Glob(filepath.Join(h.dir, ".connectors-*")); len(tmp) != 0 {
		t.Errorf("temp files left: %v", tmp)
	}
	_ = h.fileConnectors() // checks the version
}

func TestCauseOfStateError(t *testing.T) {
	err := fmt.Errorf("mcp: tools/call: token: %w", &StateError{ID: "x", State: store.ConnectorExpired})
	if got := cause(err); got != "connector x needs to be reconnected by an admin" {
		t.Errorf("cause = %q", got)
	}
}

// List and Get read a create's intent while it probes and discovers.
func TestReadDuringCreate(t *testing.T) {
	for _, open := range []bool{true, false} {
		h := newHarness(t)
		h.g.set(func(g *guard) { g.open = open })
		blocked, release := h.g.block()
		done := make(chan error, 1)
		go func() {
			_, err := h.m.Create(h.ctx, CreateRequest{ID: "gh", URL: h.srv.URL, RedirectURI: pinned})
			done <- err
		}()
		wait(t, blocked, "the probe")
		for range 3 {
			_, _ = h.m.List(h.ctx)
			_, _ = h.m.Get(h.ctx, "gh")
		}
		release()
		var err error
	reading:
		for {
			select {
			case err = <-done:
				break reading
			default:
				_, _ = h.m.List(h.ctx)
				_, _ = h.m.Get(h.ctx, "gh")
			}
		}
		if err != nil {
			t.Fatalf("open %v: %v", open, err)
		}
		want := map[bool]string{true: "none", false: "oauth"}[open]
		if c, err := h.m.Get(h.ctx, "gh"); err != nil || c.Auth != want {
			t.Errorf("open %v: %+v %v", open, c, err)
		}
	}
}

// The registered client named by hand as client_id keeps its secret.
func TestPreRegisteredIsRegisteredClient(t *testing.T) {
	h := newHarness(t)
	h.as.ASM["client_id_metadata_document_supported"] = false
	h.as.Handle(oauthtest.PathRegister, oauthtest.JSONHandler(http.StatusCreated, `{"client_id":"dcr-x","client_secret":"FAKEdcrsecret"}`))
	h.connect("gh", other)
	conns := h.fileConnectors()
	cfg := conns["gh"]
	cfg.ClientID = "dcr-x"
	conns["gh"] = cfg
	_ = h.m.Close()
	h.m = h.start(conns)
	s, err := h.m.Reconnect(h.ctx, "gh", ReconnectRequest{RedirectURI: other})
	if err != nil || clientID(t, s) != "dcr-x" {
		t.Fatalf("reconnect: %v", err)
	}
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); err != nil {
		t.Fatal(err)
	}
	r := h.as.Last(t, oauthtest.PathToken)
	if r.Header.Get("Authorization") != basic("dcr-x", "FAKEdcrsecret") && r.Form.Get("client_secret") != "FAKEdcrsecret" {
		t.Errorf("token request without the registered secret: %v", r.Header)
	}
}

// A listing over mcp's own cap is tools_too_large too.
func TestConnectFailureDetail(t *testing.T) {
	for err, want := range map[error]string{
		fmt.Errorf("x: %w", mcp.ErrListTooLarge): DetailToolsTooLarge,
		errTooLarge:                              DetailToolsTooLarge,
		errors.New("refused"):                    DetailUnreachable,
	} {
		if d, e := connectFailure(err); d != want || !errors.Is(e, ErrUnreachable) {
			t.Errorf("%v: %s %v", err, d, e)
		}
	}
}

// A URL that differs from the resource only by its default port, case or
// a trailing slash is the same URL after a restart.
func TestStartStateCanonicalURL(t *testing.T) {
	h := newHarness(t)
	raw, err := json.Marshal(authMeta{Metadata: oauth.Metadata{Issuer: "https://as.example", Resource: "https://x.example/mcp"}, IdentityKind: oauth.IdentityCIMD})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.sec.Set(secrets.Connectors, "gh", map[string]string{secrets.ConnectorAccessToken: "FAKEaccess"}); err != nil {
		t.Fatal(err)
	}
	for u, want := range map[string]string{
		"https://X.example:443/mcp/": "",
		"https://x.example:8443/mcp": DetailURLChanged,
	} {
		if err := h.st.PutConnector(h.ctx, store.Connector{ID: "gh", State: store.ConnectorConnected, AuthMeta: raw}); err != nil {
			t.Fatal(err)
		}
		_ = h.m.Close()
		h.m = h.start(map[string]config.Connector{"gh": {ID: "gh", URL: u, Auth: "oauth"}})
		if _, d := h.state("gh"); d != want {
			t.Errorf("%s: detail %q, want %q", u, d, want)
		}
	}
}

func TestSameURL(t *testing.T) {
	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"https://x.example/mcp", "https://x.example/mcp", true},
		{"HTTPS://X.Example/mcp/", "https://x.example/mcp", true},
		{"https://x.example:443/mcp", "https://x.example/mcp", true},
		{"http://127.0.0.1:80/mcp", "http://127.0.0.1/mcp", true},
		{"https://x.example/mcp?a=1", "https://x.example/mcp?a=1", true},
		{"https://x.example:8443/mcp", "https://x.example/mcp", false},
		{"http://x.example/mcp", "https://x.example/mcp", false},
		{"https://x.example/MCP", "https://x.example/mcp", false},
		{"https://x.example/mcp?a=1", "https://x.example/mcp", false},
		{"https://x.example/mcp/other", "https://x.example/mcp", false},
		{"", "", false},
		{"/mcp", "/mcp", false},
	} {
		if got := sameURL(c.a, c.b); got != c.same {
			t.Errorf("sameURL(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

// While a Delete cleans up, the id stays taken: a create gets ErrExists
// and List shows nothing half-deleted. Afterwards the id is free.
func TestCreateDuringDelete(t *testing.T) {
	h := newHarness(t)
	h.connect("gh", pinned)
	// "deleted" is announced while the id is still held, so no create's
	// events can come before it.
	held := make(chan bool, 1)
	h.events.hook(func(e event.Event) {
		var a struct{ ID, State string }
		if json.Unmarshal(e.Attrs, &a) == nil && a.ID == "gh" && a.State == StateDeleted {
			_, err := h.m.conn("gh")
			select {
			case held <- err == nil:
			default:
			}
		}
	})
	inDelete, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.fs.beforeDelete(func(string) {
		once.Do(func() {
			close(inDelete)
			<-release
		})
	})
	done := make(chan error, 1)
	go func() { done <- h.m.Delete(h.ctx, "gh") }()
	wait(t, inDelete, "the row delete")
	if _, err := h.m.Create(h.ctx, CreateRequest{ID: "gh", URL: h.srv.URL, RedirectURI: pinned}); !errors.Is(err, ErrExists) {
		t.Errorf("create during delete: %v", err)
	}
	listed := make(chan []Connector, 1)
	go func() {
		l, _ := h.m.List(h.ctx)
		listed <- l
	}()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !<-held {
		t.Error(`"deleted" announced after the id was freed`)
	}
	h.events.hook(nil)
	if l := <-listed; len(l) != 0 {
		t.Errorf("list during delete: %+v", l)
	}
	if err := h.m.Delete(h.ctx, "gh"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	h.gone("gh")
	if got := h.events.statuses("gh"); got[len(got)-1] != StateDeleted+"/" || slices.Contains(got[:len(got)-1], StateDeleted+"/") {
		t.Errorf("events = %v", got)
	}
	h.connect("gh", pinned)
}

// A tool list over the cap after the code is tools_too_large, not
// unreachable.
func TestCodeToolsTooLarge(t *testing.T) {
	h := newHarness(t)
	tools := maxToolsJSON
	defer func() { maxToolsJSON = tools }()
	s := h.create("gh", pinned)
	maxToolsJSON = 10
	c, err := h.m.Code(h.ctx, "gh", approve(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if c.State != store.ConnectorDegraded || c.Detail != DetailToolsTooLarge {
		t.Errorf("after code: %s/%s", c.State, c.Detail)
	}
}

// An auth.toml that cannot be written fails the code with
// config_write_failed; the old tokens stay.
func TestStoreGrantWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only directory")
	}
	h := newHarness(t)
	h.connect("gh", pinned)
	s, err := h.m.Reconnect(h.ctx, "gh", ReconnectRequest{RedirectURI: pinned})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(h.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(h.dir, 0o700) }()
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); err == nil {
		t.Fatal("code succeeded with a read-only auth.toml")
	}
	if st, d := h.state("gh"); st != store.ConnectorConnected || d != DetailConfigWrite {
		t.Errorf("state = %s/%s", st, d)
	}
	if h.secret("gh", secrets.ConnectorAccessToken) != "FAKEaccess1" {
		t.Error("old token lost")
	}
}

// After an issuer refused the metadata document, inspect answers the
// registration the next create uses.
func TestInspectAfterCIMDRefused(t *testing.T) {
	h := newHarness(t)
	if in, err := h.m.Inspect(h.ctx, h.srv.URL); err != nil || in.IdentityKind != oauth.IdentityCIMD {
		t.Fatalf("before: %+v %v", in, err)
	}
	s := h.create("gh", pinned)
	h.is.setFail("invalid_client")
	if _, err := h.m.Code(h.ctx, "gh", approve(t, s)); !errors.Is(err, oauth.ErrCIMDRefused) {
		t.Fatalf("refused: %v", err)
	}
	h.is.setFail("")
	if in, err := h.m.Inspect(h.ctx, h.srv.URL); err != nil || in.IdentityKind != oauth.IdentityDCR {
		t.Errorf("after: %+v %v", in, err)
	}
	if s = h.create("gh", pinned); clientID(t, s) != "dcr-client-1" {
		t.Errorf("create after: client %s", clientID(t, s))
	}
}

// A server that never answers ends the inspect at its bound.
func TestInspectTimeout(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(hung.Close)
	t.Cleanup(func() { close(release) })
	old := inspectTimeout
	inspectTimeout = 50 * time.Millisecond
	t.Cleanup(func() { inspectTimeout = old })
	if _, err := h.m.Inspect(h.ctx, hung.URL); !errors.Is(err, ErrUnreachable) {
		t.Errorf("hung: %v", err)
	}
}

// An authorization server whose metadata never comes ends the inspect at its
// bound too.
func TestInspectTimeoutInDiscovery(t *testing.T) {
	h := newHarness(t)
	h.as.Handle(oauthtest.PathASM, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	old := inspectTimeout
	inspectTimeout = 500 * time.Millisecond
	t.Cleanup(func() { inspectTimeout = old })
	logs := &syncBuffer{}
	h.m.log = slog.New(slog.NewTextHandler(logs, nil))
	start := time.Now()
	_, err := h.m.Inspect(h.ctx, h.srv.URL)
	if !errors.Is(err, ErrDiscovery) || !errors.Is(err, context.DeadlineExceeded) || h.as.Count(oauthtest.PathASM) == 0 {
		t.Errorf("hung metadata: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("inspect took %v", d)
	}
	if !strings.Contains(logs.String(), "result=timeout") {
		t.Errorf("log: %s", logs.String())
	}
}

// Each inspect logs the host and a fixed outcome word, never the path or
// query.
func TestInspectLogsHostOnly(t *testing.T) {
	h := newHarness(t)
	check := func(raw, result string) {
		t.Helper()
		logs := &syncBuffer{}
		h.m.log = slog.New(slog.NewTextHandler(logs, nil))
		_, _ = h.m.Inspect(h.ctx, raw)
		line := logs.String()
		u, _ := url.Parse(raw)
		want := `msg="connector inspect" host=` + u.Host + " result=" + result + "\n"
		if !strings.HasSuffix(line, want) || strings.Count(line, "\n") != 1 ||
			strings.Contains(line, "/mcp") || strings.Contains(line, "secret") {
			t.Errorf("%s: %q", raw, line)
		}
	}
	check(h.srv.URL, "protected")
	h.g.set(func(g *guard) { g.open = true })
	check(h.srv.URL+"/mcp?token=secret", "open")
	check("http://127.0.0.1:1/mcp?token=secret", "unreachable")
	h.g.set(func(g *guard) { g.open = false })
	h.as.ASM["code_challenge_methods_supported"] = []string{"plain"}
	check(h.srv.URL, "discovery_failed")
}

// Inspect shows what a create would keep: server info cut to its cap, no
// tool with a bad or duplicate name.
func TestInspectFiltersLikeCreate(t *testing.T) {
	h := newHarness(t)
	long := strings.Repeat("n", maxServerInfo+10)
	srv := mcptest.New(t, mcptest.Options{
		Name: long, Version: "1",
		Tools: []mcptest.Tool{mcptest.TextTool("echo", "hello"), mcptest.TextTool("bad.name", "x")},
	})
	in, err := h.m.Inspect(h.ctx, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !in.Open || in.ServerName != long[:maxServerInfo] || len(in.Tools) != 1 || in.Tools[0].Name != "echo" {
		t.Errorf("inspect: open %v, name %d bytes, tools %+v", in.Open, len(in.ServerName), in.Tools)
	}
	got := usableTools([]mcp.Tool{{Name: "a", Description: "first"}, {Name: "a", Description: "second"}, {Name: "b c"}, {Name: "b"}})
	if len(got) != 2 || got[0].Description != "first" || got[1].Name != "b" {
		t.Errorf("usableTools = %+v", got)
	}
}
