package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const pinned = "http://127.0.0.1:26502/callback"

func metadata(t *testing.T, f *fake) Metadata {
	t.Helper()
	m, err := f.client().Discover(context.Background(), f.serverURL(), f.header())
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	return m
}

func identify(t *testing.T, f *fake, m Metadata, o IdentityOptions) Identity {
	t.Helper()
	id, err := f.client().Identify(context.Background(), m, o)
	if err != nil {
		t.Fatalf("identify: %v", err)
	}
	return id
}

// dcr is a registered public client of m's issuer.
func dcr(m Metadata) Identity {
	return Identity{Kind: IdentityDCR, ClientID: "c1", Issuer: m.Issuer, RedirectURI: pinned}
}

func authorize(t *testing.T, m Metadata, id Identity, scopes []string) Authorization {
	t.Helper()
	a, err := Authorize(m, id, scopes)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	return a
}

// A pre-registered client wins over everything and makes no request.
func TestIdentifyPreRegistered(t *testing.T) {
	f := newFake(t)
	m := metadata(t, f)
	id := identify(t, f, m, IdentityOptions{
		ClientID: "Iv1.abc", ClientSecret: "FAKEsecret", RegisteredClientID: "old", RedirectURI: pinned,
	})
	want := Identity{Kind: IdentityPreRegistered, ClientID: "Iv1.abc", ClientSecret: "FAKEsecret", Issuer: m.Issuer, RedirectURI: pinned}
	if id != want {
		t.Errorf("identity = %#v", id)
	}
	if f.count(pathRegister) != 0 {
		t.Error("registered although a client id was given")
	}
}

func TestIdentifyCIMD(t *testing.T) {
	f := newFake(t)
	m := metadata(t, f)
	id := identify(t, f, m, IdentityOptions{RedirectURI: pinned, RegisteredClientID: "old"})
	if id.Kind != IdentityCIMD || id.ClientID != CIMDClientID || id.ClientSecret != "" || id.Issuer != m.Issuer || id.RedirectURI != pinned {
		t.Errorf("identity = %#v", id)
	}
	for _, p := range []string{"http://127.0.0.1:26500/callback", "http://127.0.0.1:26504/callback"} {
		if !PinnedRedirect(p) {
			t.Errorf("%s not pinned", p)
		}
	}
}

// A client registered before is reused instead of registering again.
func TestIdentifyReusesRegistered(t *testing.T) {
	f := newFake(t)
	f.asm["client_id_metadata_document_supported"] = false
	m := metadata(t, f)
	o := IdentityOptions{
		RegisteredClientID: "dcr-old", RegisteredClientSecret: "FAKEold",
		RegisteredRedirectURI: "http://127.0.0.1:9/cb", RedirectURI: "http://127.0.0.1:9/cb",
	}
	id := identify(t, f, m, o)
	want := Identity{Kind: IdentityDCR, ClientID: "dcr-old", ClientSecret: "FAKEold", Issuer: m.Issuer, RedirectURI: "http://127.0.0.1:9/cb"}
	if id != want || f.count(pathRegister) != 0 {
		t.Errorf("identity = %#v, registrations %d", id, f.count(pathRegister))
	}

	// Another redirect than the registered one: register anew.
	o.RedirectURI = "http://127.0.0.1:10/cb"
	id = identify(t, f, m, o)
	if id.ClientID != "dcr-client-1" || id.ClientSecret != "" || id.RedirectURI != o.RedirectURI || f.count(pathRegister) != 1 {
		t.Errorf("identity = %#v, registrations %d", id, f.count(pathRegister))
	}
}

// Callback prints neither the code nor server text.
func TestCallbackString(t *testing.T) {
	cb := Callback{Code: "FAKEcode", State: "s", Issuer: "https://SERVERTEXT.example", Error: "SERVERTEXT injected"}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		s := fmt.Sprintf(verb, cb)
		if strings.Contains(s, "SERVERTEXT") || strings.Contains(s, "FAKE") || !strings.Contains(s, "error:unknown") || !strings.Contains(s, "iss:true") {
			t.Errorf("%s: %s", verb, s)
		}
	}
	if s := (Callback{Error: "access_denied"}).String(); !strings.Contains(s, "error:access_denied") || !strings.Contains(s, "iss:false") {
		t.Errorf("known code: %s", s)
	}
}

// DCR when the document does not apply: not advertised, a redirect that is
// not pinned (exact match), or refused before.
func TestIdentifyDCR(t *testing.T) {
	cases := map[string]struct {
		cimd     bool
		redirect string
		refused  bool
	}{
		"not advertised": {false, pinned, false},
		"localhost":      {true, "http://localhost:26502/callback", false},
		"other port":     {true, "http://127.0.0.1:26505/callback", false},
		"trailing slash": {true, pinned + "/", false},
		"refused before": {true, pinned, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.asm["client_id_metadata_document_supported"] = tc.cimd
			m := metadata(t, f)
			id := identify(t, f, m, IdentityOptions{RedirectURI: tc.redirect, CIMDRefused: tc.refused})
			if id.Kind != IdentityDCR || id.ClientID != "dcr-client-1" || id.ClientSecret != "" || id.RedirectURI != tc.redirect {
				t.Fatalf("identity = %#v", id)
			}
			var body map[string]any
			if err := json.Unmarshal(f.last(t, pathRegister).Body, &body); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{
				"client_name":                "HolaClaw",
				"redirect_uris":              []any{tc.redirect},
				"grant_types":                []any{"authorization_code", "refresh_token"},
				"response_types":             []any{"code"},
				"token_endpoint_auth_method": "none",
			}
			if fmt.Sprint(body) != fmt.Sprint(want) {
				t.Errorf("registration body = %v", body)
			}
		})
	}
}

// A secret that comes back from registration is part of the identity.
func TestIdentifyDCRWithSecret(t *testing.T) {
	f := newFake(t)
	f.routes[pathRegister] = jsonHandler(http.StatusOK, `{"client_id":"c2","client_secret":"FAKEdcrsecret"}`)
	id := identify(t, f, metadata(t, f), IdentityOptions{RedirectURI: "http://127.0.0.1:9999/cb"})
	if id.ClientID != "c2" || id.ClientSecret != "FAKEdcrsecret" {
		t.Errorf("identity = %#v", id)
	}
}

func TestIdentifyFailures(t *testing.T) {
	ctx := context.Background()
	redirect := IdentityOptions{RedirectURI: "http://127.0.0.1:1/cb"}

	f := newFake(t)
	f.asm["client_id_metadata_document_supported"] = false
	delete(f.asm, "registration_endpoint")
	m := metadata(t, f)
	if _, err := f.client().Identify(ctx, m, IdentityOptions{RedirectURI: pinned}); !errors.Is(err, ErrNoRegistration) {
		t.Errorf("no registration endpoint: err = %v", err)
	}
	if _, err := f.client().Identify(ctx, m, IdentityOptions{ClientID: "x"}); !errors.Is(err, ErrNoRegistration) {
		t.Errorf("no redirect: err = %v", err)
	}

	f = newFake(t)
	f.routes[pathRegister] = jsonHandler(http.StatusBadRequest,
		`{"error":"invalid_redirect_uri","error_description":"SERVERTEXT you are blocked"}`)
	_, err := f.client().Identify(ctx, metadata(t, f), redirect)
	var oe *Error
	if !errors.As(err, &oe) || oe.Code != "invalid_redirect_uri" || strings.Contains(err.Error(), "SERVERTEXT") {
		t.Errorf("registration refused: err = %v", err)
	}

	for name, body := range map[string]string{
		"no client id": `{}`,
		"space":        `{"client_id":"a b"}`,
		"control":      `{"client_id":"a\u0007b"}`,
		"newline":      `{"client_id":"a\nb"}`,
		"too long":     `{"client_id":"` + strings.Repeat("a", maxURL+1) + `"}`,
	} {
		f = newFake(t)
		f.routes[pathRegister] = jsonHandler(http.StatusCreated, body)
		if _, err := f.client().Identify(ctx, metadata(t, f), redirect); !errors.Is(err, ErrBadResponse) {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	f = newFake(t)
	f.routes[pathRegister] = jsonHandler(http.StatusInternalServerError, `oops`)
	if _, err := f.client().Identify(ctx, metadata(t, f), redirect); !errors.Is(err, ErrBadResponse) {
		t.Errorf("500: err = %v", err)
	}

	// A registration that redirects would resend the request elsewhere.
	f = newFake(t)
	f.routes[pathRegister] = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, f.srv.URL+"/elsewhere", http.StatusTemporaryRedirect)
	}
	_, err = f.client().Identify(ctx, metadata(t, f), redirect)
	if !errors.Is(err, errRedirectedPost) || f.count("/elsewhere") != 0 || strings.Contains(err.Error(), f.srv.URL) {
		t.Errorf("redirected registration: err = %v", err)
	}
}

// The authorization URL carries PKCE S256, a fresh state, the resource and
// the default scopes.
func TestAuthorize(t *testing.T) {
	f := newFake(t)
	f.asm["authorization_endpoint"] = f.srv.URL + "/authorize?tenant=t1"
	f.asm["authorization_response_iss_parameter_supported"] = true
	m := metadata(t, f)
	id := identify(t, f, m, IdentityOptions{RedirectURI: pinned})
	a := authorize(t, m, id, nil)
	u, err := url.Parse(a.URL)
	if err != nil || !strings.HasPrefix(a.URL, f.srv.URL+"/authorize?") {
		t.Fatalf("url = %q", a.URL)
	}
	q := u.Query()
	sum := sha256.Sum256([]byte(a.Verifier))
	want := map[string]string{
		"tenant":                "t1",
		"response_type":         "code",
		"client_id":             CIMDClientID,
		"redirect_uri":          pinned,
		"state":                 a.State,
		"code_challenge":        base64.RawURLEncoding.EncodeToString(sum[:]),
		"code_challenge_method": "S256",
		"resource":              f.serverURL(),
		"scope":                 "repo issues:write",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	if len(a.Verifier) < 43 || len(a.State) < 43 || strings.Contains(a.URL, a.Verifier) {
		t.Errorf("verifier %d chars, state %d chars", len(a.Verifier), len(a.State))
	}
	if strings.Join(a.Scopes, " ") != "repo issues:write" || a.RedirectURI != pinned || a.Issuer != m.Issuer || !a.RequireIss {
		t.Errorf("authorization = %#v", a)
	}

	b := authorize(t, m, id, []string{"repo"})
	if b.State == a.State || b.Verifier == a.Verifier {
		t.Error("state or verifier reused")
	}
	if got, _ := url.Parse(b.URL); got.Query().Get("scope") != "repo" {
		t.Errorf("given scopes: %q", got.Query().Get("scope"))
	}

	// No scope anywhere: the parameter is omitted.
	m.Scopes = nil
	if got, _ := url.Parse(authorize(t, m, id, nil).URL); got.Query().Has("scope") {
		t.Errorf("scope sent without any: %q", got.Query().Get("scope"))
	}
}

func TestAuthorizeRefusals(t *testing.T) {
	f := newFake(t)
	m := metadata(t, f)
	id := dcr(m)
	if _, err := Authorize(m, Identity{Issuer: m.Issuer, RedirectURI: pinned}, nil); err == nil {
		t.Error("authorize without a client id")
	}
	noRedirect := id
	noRedirect.RedirectURI = ""
	if _, err := Authorize(m, noRedirect, nil); err == nil {
		t.Error("authorize without a redirect")
	}
	other := id
	other.Issuer = "https://other.example"
	if _, err := Authorize(m, other, nil); !errors.Is(err, ErrIdentityMismatch) {
		t.Errorf("other issuer: err = %v", err)
	}
	cimd := Identity{Kind: IdentityCIMD, ClientID: CIMDClientID, Issuer: m.Issuer, RedirectURI: "http://127.0.0.1:9/cb"}
	if _, err := Authorize(m, cimd, nil); !errors.Is(err, ErrIdentityMismatch) {
		t.Errorf("cimd on an unpinned redirect: err = %v", err)
	}
	m.AuthorizationEndpoint = "http://auth.example/authorize"
	if _, err := Authorize(m, id, nil); !errors.Is(err, ErrInsecureURL) {
		t.Errorf("http endpoint: err = %v", err)
	}
}

// The code exchange for a public client: verifier, redirect, resource and
// client id in the form, tokens and expiry out.
func TestExchangePublic(t *testing.T) {
	f := newFake(t)
	m := metadata(t, f)
	id := identify(t, f, m, IdentityOptions{RedirectURI: pinned})
	a := authorize(t, m, id, nil)
	tok, err := f.client().Exchange(context.Background(), m, id, a, Callback{Code: "FAKEcode1", State: a.State})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if tok.AccessToken != "FAKEaccess1" || tok.RefreshToken != "FAKErefresh1" ||
		!tok.Expiry.Equal(testNow.Add(time.Hour)) || strings.Join(tok.Scopes, " ") != "repo" {
		t.Errorf("token = %#v", tok)
	}
	rec := f.last(t, pathToken)
	want := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"FAKEcode1"},
		"redirect_uri":  {pinned},
		"code_verifier": {a.Verifier},
		"resource":      {f.serverURL()},
		"client_id":     {CIMDClientID},
	}
	if rec.Form.Encode() != want.Encode() {
		t.Errorf("form = %v", rec.Form)
	}
	if rec.Header.Get("Authorization") != "" {
		t.Error("public client sent credentials")
	}
}

// A pre-registered client with a secret: basic by default, post when the
// server lists only that.
func TestExchangeWithSecret(t *testing.T) {
	f := newFake(t)
	f.routes[pathToken] = jsonHandler(http.StatusOK, `{"access_token":"FAKEaccess2","token_type":"bearer"}`)
	m := metadata(t, f)
	id := identify(t, f, m, IdentityOptions{ClientID: "app:1", ClientSecret: "FAKE secret/1", RedirectURI: pinned})
	a := authorize(t, m, id, []string{"repo"})
	tok, err := f.client().Exchange(context.Background(), m, id, a, Callback{Code: "FAKEcode2", State: a.State})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if !tok.Expiry.IsZero() || strings.Join(tok.Scopes, " ") != "repo" || tok.RefreshToken != "" {
		t.Errorf("token = %#v", tok)
	}
	rec := f.last(t, pathToken)
	r := &http.Request{Header: rec.Header}
	user, pass, ok := r.BasicAuth()
	if !ok || user != url.QueryEscape("app:1") || pass != url.QueryEscape("FAKE secret/1") {
		t.Errorf("basic auth = %q %q %t", user, pass, ok)
	}
	if rec.Form.Has("client_secret") || rec.Form.Has("client_id") {
		t.Errorf("credentials in the form too: %v", rec.Form)
	}

	m.TokenAuthMethods = []string{"client_secret_post"}
	if _, err := f.client().Exchange(context.Background(), m, id, a, Callback{Code: "FAKEcode3", State: a.State}); err != nil {
		t.Fatalf("post exchange: %v", err)
	}
	rec = f.last(t, pathToken)
	if rec.Header.Get("Authorization") != "" || rec.Form.Get("client_id") != "app:1" || rec.Form.Get("client_secret") != "FAKE secret/1" {
		t.Errorf("client_secret_post: header %q form %v", rec.Header.Get("Authorization"), rec.Form)
	}
}

func TestExchangeFailures(t *testing.T) {
	f := newFake(t)
	m := metadata(t, f)
	id := dcr(m)
	a := authorize(t, m, id, nil)
	ctx := context.Background()
	cb := Callback{Code: "FAKEcode", State: a.State}
	exchange := func() error {
		_, err := f.client().Exchange(ctx, m, id, a, cb)
		return err
	}

	if _, err := f.client().Exchange(ctx, m, id, a, Callback{Code: "FAKEcode", State: "other"}); !errors.Is(err, ErrStateMismatch) {
		t.Errorf("state: err = %v", err)
	}
	if _, err := f.client().Exchange(ctx, m, id, Authorization{}, Callback{Code: "FAKEcode"}); !errors.Is(err, ErrStateMismatch) {
		t.Errorf("empty state: err = %v", err)
	}
	if _, err := f.client().Exchange(ctx, m, id, a, Callback{State: a.State}); !errors.Is(err, ErrBadResponse) {
		t.Errorf("no code: err = %v", err)
	}
	_, err := f.client().Exchange(ctx, m, id, a, Callback{State: a.State, Error: "access_denied"})
	if oe := (*Error)(nil); !errors.As(err, &oe) || oe.Code != "access_denied" {
		t.Errorf("callback error: err = %v", err)
	}
	if f.count(pathToken) != 0 {
		t.Error("token endpoint called for a refused callback")
	}

	f.routes[pathToken] = jsonHandler(http.StatusOK, `{"token_type":"bearer"}`)
	if err := exchange(); !errors.Is(err, ErrBadResponse) {
		t.Errorf("no access token: err = %v", err)
	}
	f.routes[pathToken] = jsonHandler(http.StatusOK, `{"access_token":"FAKEa","token_type":"mac"}`)
	if err := exchange(); !errors.Is(err, ErrBadResponse) {
		t.Errorf("mac token: err = %v", err)
	}
	f.routes[pathToken] = jsonHandler(http.StatusOK, `{"access_token":"FAKEa","scope":"`+strings.Repeat("s ", maxList+1)+`"}`)
	if err := exchange(); !errors.Is(err, ErrBadResponse) {
		t.Errorf("too many scopes: err = %v", err)
	}
	f.routes[pathToken] = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat(" ", maxBody+1)))
	}
	if err := exchange(); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized: err = %v", err)
	}
	f.routes[pathToken] = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, f.srv.URL+"/elsewhere", http.StatusPermanentRedirect)
	}
	if err := exchange(); !errors.Is(err, errRedirectedPost) || f.count("/elsewhere") != 0 {
		t.Errorf("redirected token request: err = %v", err)
	}

	// An OAuth error answer keeps only the code, from a closed set.
	f.routes[pathToken] = jsonHandler(http.StatusBadRequest, `{"error":"invalid_grant","error_description":"SERVERTEXT"}`)
	if err := exchange(); !errors.Is(err, ErrInvalidGrant) || errors.Is(err, ErrCIMDRefused) {
		t.Errorf("invalid_grant: err = %v", err)
	}
	f.routes[pathToken] = jsonHandler(http.StatusBadRequest, `{"error":"SERVERTEXT_custom"}`)
	err = exchange()
	var oe *Error
	if !errors.As(err, &oe) || oe.Code != "unknown" || strings.Contains(err.Error(), "SERVERTEXT") {
		t.Errorf("unknown code: err = %v", err)
	}
	// An oversized error answer is still classified, not "too large".
	f.routes[pathToken] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(strings.Repeat(" ", maxBody+1)))
	}
	if err := exchange(); !errors.Is(err, ErrBadResponse) {
		t.Errorf("oversized error: err = %v", err)
	}
	// A 200 carrying an error member is still an error.
	f.routes[pathToken] = jsonHandler(http.StatusOK, `{"error":"invalid_client"}`)
	if err := exchange(); !errors.As(err, &oe) || oe.Code != "invalid_client" || errors.Is(err, ErrCIMDRefused) {
		t.Errorf("200 error: err = %v", err)
	}
}

// expires_in is read leniently: a number or a numeric string, else none.
func TestExchangeLifetime(t *testing.T) {
	cases := map[string]time.Duration{
		`3600`:                              time.Hour,
		`"120"`:                             2 * time.Minute,
		`"soon"`:                            0,
		`""`:                                0,
		`null`:                              0,
		`{"s":1}`:                           0,
		`-5`:                                0,
		`1e40`:                              0,
		`"` + strings.Repeat("9", 40) + `"`: 0,
	}
	for raw, want := range cases {
		f := newFake(t)
		f.routes[pathToken] = jsonHandler(http.StatusOK, `{"access_token":"FAKEa","expires_in":`+raw+`}`)
		m := metadata(t, f)
		id := dcr(m)
		a := authorize(t, m, id, nil)
		tok, err := f.client().Exchange(context.Background(), m, id, a, Callback{Code: "FAKEcode", State: a.State})
		if err != nil {
			t.Errorf("%s: %v", raw, err)
			continue
		}
		if (want == 0 && !tok.Expiry.IsZero()) || (want != 0 && !tok.Expiry.Equal(testNow.Add(want))) {
			t.Errorf("%s: expiry = %v", raw, tok.Expiry)
		}
	}
}

// RFC 9207: the iss of the redirect must be the issuer when the issuer
// promised one or when one is present at all.
func TestExchangeIssParameter(t *testing.T) {
	f := newFake(t)
	f.asm["authorization_response_iss_parameter_supported"] = true
	m := metadata(t, f)
	id := dcr(m)
	a := authorize(t, m, id, nil)
	ctx := context.Background()
	for name, iss := range map[string]string{"missing": "", "other": "https://evil.example", "trailing slash": m.Issuer + "/"} {
		if _, err := f.client().Exchange(ctx, m, id, a, Callback{Code: "FAKEcode", State: a.State, Issuer: iss}); !errors.Is(err, ErrIssuerMismatch) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if f.count(pathToken) != 0 {
		t.Fatal("token endpoint called on an iss mismatch")
	}
	if _, err := f.client().Exchange(ctx, m, id, a, Callback{Code: "FAKEcode", State: a.State, Issuer: m.Issuer}); err != nil {
		t.Errorf("matching iss: %v", err)
	}

	// Not promised: absent is fine, present must still match.
	f = newFake(t)
	m = metadata(t, f)
	id = dcr(m)
	a = authorize(t, m, id, nil)
	if _, err := f.client().Exchange(ctx, m, id, a, Callback{Code: "FAKEcode", State: a.State, Issuer: "https://evil.example"}); !errors.Is(err, ErrIssuerMismatch) {
		t.Errorf("unpromised wrong iss: err = %v", err)
	}
	if _, err := f.client().Exchange(ctx, m, id, a, Callback{Code: "FAKEcode", State: a.State}); err != nil {
		t.Errorf("unpromised absent iss: %v", err)
	}
}

// Credentials are bound to their issuer: after the resource switches
// authorization servers, nothing is posted to the new one.
func TestTokenBoundToIssuer(t *testing.T) {
	f := newFake(t)
	m := metadata(t, f)
	id := identify(t, f, m, IdentityOptions{ClientID: "app", ClientSecret: "FAKEsecret", RedirectURI: pinned})
	a := authorize(t, m, id, nil)
	ctx := context.Background()

	moved := m
	moved.Issuer = "https://new-as.example"
	if _, err := f.client().Refresh(ctx, moved, id, "FAKErefresh"); !errors.Is(err, ErrIdentityMismatch) {
		t.Errorf("refresh: err = %v", err)
	}
	if _, err := f.client().Exchange(ctx, moved, id, a, Callback{Code: "FAKEcode", State: a.State}); !errors.Is(err, ErrIdentityMismatch) {
		t.Errorf("exchange: err = %v", err)
	}
	noIssuer := id
	noIssuer.Issuer = ""
	if _, err := f.client().Refresh(ctx, m, noIssuer, "FAKErefresh"); !errors.Is(err, ErrIdentityMismatch) {
		t.Errorf("no issuer: err = %v", err)
	}
	otherRedirect := id
	otherRedirect.RedirectURI = "http://127.0.0.1:26503/callback"
	if _, err := f.client().Exchange(ctx, m, otherRedirect, a, Callback{Code: "FAKEcode", State: a.State}); !errors.Is(err, ErrIdentityMismatch) {
		t.Errorf("other redirect: err = %v", err)
	}
	if f.count(pathToken) != 0 {
		t.Error("token endpoint called with a mismatched identity")
	}
}

// A refused metadata document, at the token endpoint or on the redirect.
func TestCIMDRefused(t *testing.T) {
	f := newFake(t)
	f.routes[pathToken] = jsonHandler(http.StatusUnauthorized, `{"error":"invalid_client","error_description":"unknown client"}`)
	m := metadata(t, f)
	id := identify(t, f, m, IdentityOptions{RedirectURI: pinned})
	a := authorize(t, m, id, nil)
	_, err := f.client().Exchange(context.Background(), m, id, a, Callback{Code: "FAKEcode", State: a.State})
	if !errors.Is(err, ErrCIMDRefused) || errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("err = %v, want ErrCIMDRefused", err)
	}

	if err := CallbackError(id, a, Callback{State: a.State, Error: "unauthorized_client"}); !errors.Is(err, ErrCIMDRefused) {
		t.Errorf("callback: err = %v", err)
	}
	if err := CallbackError(id, a, Callback{State: a.State, Error: "access_denied"}); errors.Is(err, ErrCIMDRefused) || err == nil {
		t.Errorf("access_denied: %v", err)
	}
	if err := CallbackError(dcr(m), a, Callback{State: a.State, Error: "invalid_client"}); errors.Is(err, ErrCIMDRefused) {
		t.Errorf("DCR client counted as refused document: %v", err)
	}
	// Bound to the intent: a forged redirect cannot mark the document refused.
	if err := CallbackError(id, a, Callback{Error: "invalid_client"}); !errors.Is(err, ErrStateMismatch) || errors.Is(err, ErrCIMDRefused) {
		t.Errorf("stateless callback: err = %v", err)
	}
	if err := CallbackError(id, a, Callback{State: a.State, Issuer: "https://evil.example", Error: "invalid_client"}); !errors.Is(err, ErrIssuerMismatch) {
		t.Errorf("foreign iss: err = %v", err)
	}
	if err := CallbackError(id, a, Callback{Code: "FAKEcode", State: a.State}); err != nil {
		t.Errorf("good callback: %v", err)
	}
}

func TestRefresh(t *testing.T) {
	f := newFake(t)
	f.routes[pathToken] = jsonHandler(http.StatusOK, `{"access_token":"FAKEaccess3","token_type":"Bearer","expires_in":"120"}`)
	m := metadata(t, f)
	id := dcr(m)
	tok, err := f.client().Refresh(context.Background(), m, id, "FAKEold")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tok.AccessToken != "FAKEaccess3" || tok.RefreshToken != "FAKEold" || tok.Scopes != nil ||
		!tok.Expiry.Equal(testNow.Add(2*time.Minute)) {
		t.Errorf("token = %#v", tok)
	}
	want := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {"FAKEold"},
		"resource":      {f.serverURL()},
		"client_id":     {"c1"},
	}
	if got := f.last(t, pathToken).Form; got.Encode() != want.Encode() {
		t.Errorf("form = %v", got)
	}

	f.routes[pathToken] = jsonHandler(http.StatusOK, `{"access_token":"FAKEaccess4","refresh_token":"FAKErotated"}`)
	if tok, _ := f.client().Refresh(context.Background(), m, id, "FAKEold"); tok.RefreshToken != "FAKErotated" {
		t.Errorf("rotated refresh token not taken: %#v", tok)
	}
}

// A rejected refresh is ErrInvalidGrant, and the error text quotes neither
// the token nor the server.
func TestRefreshRejected(t *testing.T) {
	f := newFake(t)
	f.routes[pathToken] = jsonHandler(http.StatusBadRequest, `{"error":"invalid_grant","error_description":"token FAKEold revoked"}`)
	m := metadata(t, f)
	_, err := f.client().Refresh(context.Background(), m, dcr(m), "FAKEold")
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("err = %v, want ErrInvalidGrant", err)
	}
	if strings.Contains(err.Error(), "FAKEold") || strings.Contains(err.Error(), "revoked") {
		t.Errorf("error text leaks: %q", err)
	}
	if _, err := f.client().Refresh(context.Background(), m, dcr(m), ""); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("no refresh token: err = %v", err)
	}
}

// Credentials never show in fmt or JSON output.
func TestNoSecretsInFormatting(t *testing.T) {
	id := Identity{Kind: IdentityPreRegistered, ClientID: "c", ClientSecret: "FAKEsecretX"}
	tok := Token{AccessToken: "FAKEaccessX", RefreshToken: "FAKErefreshX"}
	a := Authorization{URL: "https://a/x", State: "FAKEstateX", Verifier: "FAKEverifierX"}
	cb := Callback{Code: "FAKEcodeX", State: "s"}
	values := []any{id, &id, tok, &tok, a, &a, cb, &cb, []Token{tok}, struct{ T Token }{tok}}
	for _, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			if s := fmt.Sprintf(verb, v); strings.Contains(s, "FAKE") {
				t.Errorf("%s of %T leaks: %s", verb, v, s)
			}
		}
		if b, err := json.Marshal(v); err != nil || strings.Contains(string(b), "FAKE") {
			t.Errorf("json of %T leaks: %s %v", v, b, err)
		}
	}
	// Metadata stays fully marshalable for the cache row.
	m := Metadata{Resource: "r", Issuer: "i", TokenEndpoint: "t", Scopes: []string{"s"}, IssParameterSupported: true}
	b, _ := json.Marshal(m)
	var back Metadata
	if err := json.Unmarshal(b, &back); err != nil || fmt.Sprint(back) != fmt.Sprint(m) {
		t.Errorf("metadata round trip: %s %v", b, err)
	}
}
