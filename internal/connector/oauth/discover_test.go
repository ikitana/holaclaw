package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func discover(t *testing.T, f *fake, header string) (Metadata, error) {
	t.Helper()
	return f.client().Discover(context.Background(), f.serverURL(), header)
}

// The header path: resource metadata, then AS metadata, all in the result
// and in its JSON for the cache row.
func TestDiscoverFromHeader(t *testing.T) {
	f := newFake(t)
	f.asm["token_endpoint_auth_methods_supported"] = []string{"none", "client_secret_basic"}
	m, err := discover(t, f, f.header()+`, error="invalid_token"`)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	base := f.srv.URL
	want := Metadata{
		Resource:              base + "/mcp",
		ResourceMetadata:      base + pathPRM,
		Issuer:                base,
		AuthorizationEndpoint: base + "/authorize",
		TokenEndpoint:         base + pathToken,
		RegistrationEndpoint:  base + pathRegister,
		Scopes:                []string{"repo", "issues:write"},
		CodeChallengeMethods:  []string{"S256"},
		TokenAuthMethods:      []string{"none", "client_secret_basic"},
		CIMDSupported:         true,
	}
	gotJSON, _ := json.Marshal(m)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("metadata\n got %s\nwant %s", gotJSON, wantJSON)
	}
	var back Metadata
	if err := json.Unmarshal(gotJSON, &back); err != nil || back.TokenEndpoint != want.TokenEndpoint {
		t.Errorf("round trip: %v %+v", err, back)
	}
	// The resource's lists are for inspect and stay out of the cache row.
	if strings.Join(m.AuthorizationServers, " ") != base || strings.Join(m.ScopesSupported, " ") != "repo issues:write" {
		t.Errorf("listed: %v %v", m.AuthorizationServers, m.ScopesSupported)
	}
	if strings.Contains(string(gotJSON), "authorization_servers") || strings.Contains(string(gotJSON), "scopes_supported") {
		t.Errorf("the listed fields are in the JSON: %s", gotJSON)
	}
}

// Only the first authorization server is used; the others are listed when
// they are https and within the caps, and are never refused.
func TestDiscoverListsAuthorizationServers(t *testing.T) {
	f := newFake(t)
	base := f.srv.URL
	listed := []string{base, "http://plain.example", "https://" + strings.Repeat("a", maxURL) + ".example",
		"https://a.example https://evil.example", "https://a.example\n", "https:///no-host", "https://%zz", "https://other.example"}
	for len(listed) < maxList+10 {
		listed = append(listed, "https://more.example")
	}
	f.prm["authorization_servers"] = listed
	m, err := discover(t, f, f.header())
	if err != nil {
		t.Fatal(err)
	}
	if m.Issuer != base || len(m.AuthorizationServers) != maxList || m.AuthorizationServers[0] != base ||
		m.AuthorizationServers[1] != "https://other.example" {
		t.Errorf("issuer %s, listed %d", m.Issuer, len(m.AuthorizationServers))
	}
}

// An issuer the list would leave out fails discovery before any request to
// it, so the issuer is always the first listed.
func TestDiscoverRefusesUnlistableIssuer(t *testing.T) {
	for _, issuer := range []string{"https://as.example/ x", "https://as.example\n", "https:///no-host",
		"https://" + strings.Repeat("a", maxURL) + ".example", "https://as.example/?q=1", "https://as.example/#f"} {
		f := newFake(t)
		f.prm["authorization_servers"] = []string{issuer, f.srv.URL}
		if _, err := discover(t, f, f.header()); !errors.Is(err, ErrBadMetadata) {
			t.Errorf("issuer %q: err = %v, want ErrBadMetadata", issuer, err)
		}
		if f.count(pathASM) != 0 {
			t.Errorf("issuer %q: metadata fetched", issuer)
		}
	}
}

// No resource_metadata in the header: the path form of the well-known URL,
// then the root form.
func TestDiscoverWellKnownFallback(t *testing.T) {
	f := newFake(t)
	if m, err := discover(t, f, ""); err != nil || m.ResourceMetadata != f.srv.URL+pathPRM {
		t.Fatalf("path form: %+v, %v", m, err)
	}

	f = newFake(t)
	f.routes[pathPRM] = http.NotFound
	prm := f.prm
	f.routes["/.well-known/oauth-protected-resource"] = func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, prm) }
	m, err := discover(t, f, `Basic realm="x"`)
	if err != nil {
		t.Fatalf("root form: %v", err)
	}
	if m.ResourceMetadata != f.srv.URL+"/.well-known/oauth-protected-resource" || f.count(pathPRM) != 1 {
		t.Errorf("resource metadata = %q, path form tried %d times", m.ResourceMetadata, f.count(pathPRM))
	}
}

// An open server publishes no resource metadata: nothing to authorize.
func TestDiscoverOpenServer(t *testing.T) {
	f := newFake(t)
	f.routes[pathPRM] = http.NotFound
	_, err := discover(t, f, "")
	if !errors.Is(err, ErrNotProtected) {
		t.Fatalf("err = %v, want ErrNotProtected", err)
	}
	if f.count(pathASM) != 0 {
		t.Error("authorization server metadata fetched for an open server")
	}
}

// A 5xx from a well-known path is a failure, not "not protected".
func TestDiscoverServerErrorIsNotOpen(t *testing.T) {
	f := newFake(t)
	f.routes[pathPRM] = jsonHandler(http.StatusBadGateway, `{}`)
	if _, err := discover(t, f, ""); !errors.Is(err, ErrBadMetadata) {
		t.Fatalf("err = %v, want ErrBadMetadata", err)
	}
}

// http metadata is refused before any request is made.
func TestDiscoverRefusesHTTPMetadata(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("plain http metadata was fetched")
	}))
	defer plain.Close()
	f := newFake(t)
	_, err := discover(t, f, `Bearer resource_metadata="`+plain.URL+pathPRM+`"`)
	if !errors.Is(err, ErrInsecureURL) {
		t.Fatalf("err = %v, want ErrInsecureURL", err)
	}

	f.prm["authorization_servers"] = []string{"http://auth.example"}
	if _, err := discover(t, f, f.header()); !errors.Is(err, ErrInsecureURL) {
		t.Errorf("http issuer: err = %v", err)
	}

	f = newFake(t)
	f.asm["token_endpoint"] = "http://auth.example/token"
	if _, err := discover(t, f, f.header()); !errors.Is(err, ErrInsecureURL) {
		t.Errorf("http token endpoint: err = %v", err)
	}
}

// Redirects are followed to https only.
func TestDiscoverRedirects(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("redirect to http was followed")
	}))
	defer plain.Close()
	f := newFake(t)
	f.routes["/moved"] = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+pathPRM, http.StatusFound)
	}
	_, err := discover(t, f, `Bearer resource_metadata="`+f.srv.URL+`/moved"`)
	if !errors.Is(err, ErrInsecureURL) {
		t.Fatalf("err = %v, want ErrInsecureURL", err)
	}

	f.routes["/moved"] = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, f.srv.URL+pathPRM, http.StatusFound)
	}
	if _, err := discover(t, f, `Bearer resource_metadata="`+f.srv.URL+`/moved"`); err != nil {
		t.Errorf("https redirect: %v", err)
	}

	f.routes["/loop"] = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, f.srv.URL+"/loop", http.StatusFound)
	}
	if _, err := discover(t, f, `Bearer resource_metadata="`+f.srv.URL+`/loop"`); err == nil {
		t.Error("redirect loop accepted")
	}
}

func TestDiscoverResourceMismatch(t *testing.T) {
	f := newFake(t)
	f.prm["resource"] = "https://other.example/mcp"
	if _, err := discover(t, f, f.header()); !errors.Is(err, ErrResourceMismatch) {
		t.Fatalf("err = %v, want ErrResourceMismatch", err)
	}
	// Canonical forms match: host case, default port and a trailing slash.
	f.prm["resource"] = "HTTPS" + strings.TrimPrefix(f.srv.URL, "https") + "/mcp/"
	if _, err := discover(t, f, f.header()); err != nil {
		t.Errorf("trailing slash: %v", err)
	}
	// Home Assistant: the origin is the resource of /mcp, and is what the
	// token is requested for.
	f.prm["resource"] = f.srv.URL
	if m, err := discover(t, f, f.header()); err != nil || m.Resource != f.srv.URL {
		t.Errorf("origin as resource: %+v, %v", m, err)
	}
	canon := func(raw string) string {
		s, _ := CanonicalURL(raw)
		return s
	}
	if canon("HTTPS://Example.COM:443/mcp/") != canon("https://example.com/mcp") {
		t.Error("canonical forms differ")
	}
	if canon("https://example.com/mcp") == canon("https://example.com/mcp/other") {
		t.Error("different paths compared equal")
	}
	if _, ok := CanonicalURL("/mcp"); ok {
		t.Error("a URL without a host is canonical")
	}
}

func TestCovers(t *testing.T) {
	for _, c := range []struct {
		resource, server string
		want             bool
	}{
		{"https://x.example/mcp", "https://x.example/mcp", true},
		{"HTTPS://X.Example/mcp/", "https://x.example/mcp", true},
		{"https://x.example:443/mcp", "https://x.example/mcp", true},
		{"http://127.0.0.1:80/mcp", "http://127.0.0.1/mcp", true},
		{"https://x.example/mcp?a=1", "https://x.example/mcp?a=1", true},
		{"https://x.example", "https://x.example/api/mcp", true},
		{"https://x.example/", "https://x.example/api/mcp", true},
		{"https://x.example/api", "https://x.example/api/mcp", true},
		{"https://x.example/ap", "https://x.example/api/mcp", false},
		{"https://x.example?a=1", "https://x.example/mcp", false},
		{"https://x.example:8443/mcp", "https://x.example/mcp", false},
		{"https://x.example:8443", "https://x.example/mcp", false},
		{"http://x.example/mcp", "https://x.example/mcp", false},
		{"https://x.example/MCP", "https://x.example/mcp", false},
		{"https://x.example/mcp?a=1", "https://x.example/mcp", false},
		{"https://x.example/mcp/other", "https://x.example/mcp", false},
		{"https://x.example.evil", "https://x.example/mcp", false},
		{"", "", false},
		{"/mcp", "/mcp", false},
	} {
		if got := Covers(c.resource, c.server); got != c.want {
			t.Errorf("Covers(%q, %q) = %v", c.resource, c.server, got)
		}
	}
}

func TestDiscoverIssuerMismatch(t *testing.T) {
	f := newFake(t)
	f.asm["issuer"] = "https://evil.example"
	if _, err := discover(t, f, f.header()); !errors.Is(err, ErrIssuerMismatch) {
		t.Fatalf("err = %v, want ErrIssuerMismatch", err)
	}
}

// S256 must be listed; an absent list means no PKCE and is refused too.
func TestDiscoverRequiresS256(t *testing.T) {
	f := newFake(t)
	f.asm["code_challenge_methods_supported"] = []string{"plain"}
	if _, err := discover(t, f, f.header()); !errors.Is(err, ErrNoS256) {
		t.Errorf("plain only: err = %v", err)
	}
	delete(f.asm, "code_challenge_methods_supported")
	if _, err := discover(t, f, f.header()); !errors.Is(err, ErrNoS256) {
		t.Errorf("absent: err = %v", err)
	}
}

func TestDiscoverOversizedBody(t *testing.T) {
	f := newFake(t)
	f.routes[pathPRM] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"resource":"` + strings.Repeat("a", maxBody) + `"}`))
	}
	if _, err := discover(t, f, f.header()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

// An issuer with a path: RFC 8414 path insertion first, then OpenID.
func TestDiscoverIssuerPathAndOpenID(t *testing.T) {
	f := newFake(t)
	issuer := f.srv.URL + "/tenant1"
	f.prm["authorization_servers"] = []string{issuer}
	f.asm["issuer"] = issuer
	asm := f.asm
	f.routes["/.well-known/openid-configuration/tenant1"] = func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, asm) }
	m, err := discover(t, f, f.header())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if m.Issuer != issuer || f.count("/.well-known/oauth-authorization-server/tenant1") != 1 {
		t.Errorf("issuer %q, 8414 form tried %d", m.Issuer, f.count("/.well-known/oauth-authorization-server/tenant1"))
	}

	f = newFake(t)
	f.routes[pathASM] = http.NotFound
	if _, err := discover(t, f, f.header()); !errors.Is(err, ErrBadMetadata) {
		t.Errorf("no AS metadata: err = %v", err)
	}
	if f.count("/.well-known/openid-configuration") != 1 {
		t.Error("OpenID root form not tried")
	}
}

func TestDiscoverBadMetadata(t *testing.T) {
	f := newFake(t)
	f.routes[pathPRM] = jsonHandler(http.StatusOK, `not json`)
	if _, err := discover(t, f, f.header()); !errors.Is(err, ErrBadMetadata) {
		t.Errorf("not json: err = %v", err)
	}

	f = newFake(t)
	f.prm["authorization_servers"] = []string{}
	if _, err := discover(t, f, f.header()); !errors.Is(err, ErrBadMetadata) {
		t.Errorf("no servers: err = %v", err)
	}

	f = newFake(t)
	f.routes[pathPRM] = jsonHandler(http.StatusUnauthorized, `{}`)
	if _, err := discover(t, f, f.header()); !errors.Is(err, ErrBadMetadata) {
		t.Errorf("named metadata 401: err = %v", err)
	}

	f = newFake(t)
	delete(f.asm, "token_endpoint")
	if _, err := discover(t, f, f.header()); !errors.Is(err, ErrBadMetadata) {
		t.Errorf("no token endpoint: err = %v", err)
	}

	if _, err := f.client().Discover(context.Background(), "not a url", ""); !errors.Is(err, ErrBadMetadata) {
		t.Errorf("bad server url: err = %v", err)
	}
}

func TestDiscoverContextCancelled(t *testing.T) {
	f := newFake(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.client().Discover(ctx, f.serverURL(), f.header()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// The authorization server's scopes are never a default: with none from the
// resource, no scope is requested.
func TestDiscoverScopesFallback(t *testing.T) {
	f := newFake(t)
	delete(f.prm, "scopes_supported")
	f.asm["scopes_supported"] = []string{"openid", "read"}
	m, err := discover(t, f, f.header())
	if err != nil || m.Scopes != nil {
		t.Fatalf("scopes = %v, %v", m.Scopes, err)
	}
}

// The challenge's scope comes first (MCP 2025-11-25).
func TestDiscoverChallengeScope(t *testing.T) {
	f := newFake(t)
	m, err := discover(t, f, f.header()+`, scope="read  write:all"`)
	if err != nil || strings.Join(m.Scopes, " ") != "read write:all" {
		t.Fatalf("scopes = %v, %v", m.Scopes, err)
	}
	// scopes_supported is still there for inspect.
	if strings.Join(m.ScopesSupported, " ") != "repo issues:write" {
		t.Errorf("scopes_supported = %v", m.ScopesSupported)
	}
	for name, scope := range map[string]string{
		"quote char": `a\"b`,
		"too many":   strings.Repeat("s ", maxList+1),
		"too long":   strings.Repeat("s", maxItem+1),
	} {
		if _, err := discover(t, f, f.header()+`, scope="`+scope+`"`); !errors.Is(err, ErrBadMetadata) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// A named resource_metadata that answers 4xx falls through to the
// well-known forms; a 5xx does not.
func TestDiscoverNamedMetadataFallsThrough(t *testing.T) {
	f := newFake(t)
	f.routes["/named"] = http.NotFound
	m, err := discover(t, f, `Bearer resource_metadata="`+f.srv.URL+`/named"`)
	if err != nil || m.ResourceMetadata != f.srv.URL+pathPRM {
		t.Fatalf("fallthrough: %+v, %v", m, err)
	}
	f.routes["/named"] = jsonHandler(http.StatusServiceUnavailable, `{}`)
	if _, err := discover(t, f, `Bearer resource_metadata="`+f.srv.URL+`/named"`); !errors.Is(err, ErrBadMetadata) {
		t.Errorf("5xx: err = %v", err)
	}
	// Named but found nowhere: protected, yet unusable; not "open".
	f.routes["/named"] = http.NotFound
	f.routes[pathPRM] = http.NotFound
	if _, err := discover(t, f, `Bearer resource_metadata="`+f.srv.URL+`/named"`); !errors.Is(err, ErrBadMetadata) || errors.Is(err, ErrNotProtected) {
		t.Errorf("nowhere: err = %v", err)
	}
}

// An oversized error answer on one well-known form moves on to the next.
func TestDiscoverOversized404MovesOn(t *testing.T) {
	f := newFake(t)
	f.routes[pathPRM] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(strings.Repeat("x", 2*maxBody)))
	}
	prm := f.prm
	f.routes["/.well-known/oauth-protected-resource"] = func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, prm) }
	if _, err := discover(t, f, ""); err != nil {
		t.Fatalf("discover: %v", err)
	}
}

func TestDiscoverCaps(t *testing.T) {
	long := "https://a.example/" + strings.Repeat("a", maxURL)
	f := newFake(t)
	if _, err := discover(t, f, `Bearer resource_metadata="`+long+`"`); !errors.Is(err, ErrBadMetadata) {
		t.Errorf("long metadata url: err = %v", err)
	}
	f.prm["scopes_supported"] = []string{"ok", "bad scope"}
	if _, err := discover(t, f, f.header()); !errors.Is(err, ErrBadMetadata) {
		t.Errorf("bad prm scope: err = %v", err)
	}
	f = newFake(t)
	f.prm["authorization_servers"] = []string{long}
	if _, err := discover(t, f, f.header()); !errors.Is(err, ErrBadMetadata) {
		t.Errorf("long issuer: err = %v", err)
	}
	f = newFake(t)
	f.asm["token_endpoint"] = long
	if _, err := discover(t, f, f.header()); !errors.Is(err, ErrBadMetadata) {
		t.Errorf("long endpoint: err = %v", err)
	}
	f = newFake(t)
	f.asm["token_endpoint_auth_methods_supported"] = make([]string, maxList+1)
	if _, err := discover(t, f, f.header()); !errors.Is(err, ErrBadMetadata) {
		t.Errorf("long list: err = %v", err)
	}
}

// Error text never quotes a URL or a peer address, the attacker's included.
func TestErrorsQuoteNoURL(t *testing.T) {
	f := newFake(t)
	f.routes["/moved"] = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://attacker.example/SERVERTEXT", http.StatusFound)
	}
	_, err := discover(t, f, `Bearer resource_metadata="`+f.srv.URL+`/moved"`)
	if !errors.Is(err, ErrInsecureURL) || strings.Contains(err.Error(), "attacker") || strings.Contains(err.Error(), f.srv.URL) {
		t.Errorf("redirect: %v", err)
	}

	dead := httptest.NewTLSServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	c := &Client{HTTP: f.srv.Client()}
	_, err = c.Discover(context.Background(), deadURL+"/mcp", `Bearer resource_metadata="`+deadURL+pathPRM+`"`)
	host := strings.TrimPrefix(deadURL, "https://")
	if err == nil || strings.Contains(err.Error(), host) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("connection refused: %v", err)
	}
}

// Without a client timeout each request still has one; the caller's client
// is left as it was.
func TestDefaultTimeout(t *testing.T) {
	release := make(chan struct{})
	slow := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	hc := slow.Client()
	c := &Client{HTTP: hc, timeout: 50 * time.Millisecond}
	start := time.Now()
	_, err := c.Discover(context.Background(), slow.URL+"/mcp", "")
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Errorf("err = %v after %v", err, time.Since(start))
	}
	if hc.CheckRedirect != nil || hc.Timeout != 0 {
		t.Error("caller's client changed")
	}
}
