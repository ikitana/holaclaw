package googlechat

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAssertionSignedAndClaims(t *testing.T) {
	f := newFake(t)
	a, _ := newAdapter(t, f, nil)
	if _, err := a.credentials(context.Background()); err != nil {
		t.Fatalf("credentials: %v", err)
	}
	recs := f.records("token")
	if len(recs) != 1 {
		t.Fatalf("token calls = %d, want 1 (token_uri in the key must be ignored)", len(recs))
	}
	form, err := url.ParseQuery(string(recs[0].Body))
	if err != nil {
		t.Fatal(err)
	}
	if got := form.Get("grant_type"); got != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		t.Fatalf("grant_type = %q", got)
	}
	parts := strings.Split(form.Get("assertion"), ".")
	if len(parts) != 3 {
		t.Fatalf("assertion has %d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testRSA.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	var hdr map[string]string
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	_ = json.Unmarshal(hb, &hdr)
	if hdr["alg"] != "RS256" || hdr["typ"] != "JWT" || hdr["kid"] != testKeyID {
		t.Fatalf("header = %v", hdr)
	}
	var claims map[string]any
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(cb, &claims)
	if claims["iss"] != testEmail {
		t.Errorf("iss = %v", claims["iss"])
	}
	if claims["aud"] != f.srv.URL+"/token" {
		t.Errorf("aud = %v", claims["aud"])
	}
	if claims["scope"] != "https://www.googleapis.com/auth/chat.bot https://www.googleapis.com/auth/pubsub" {
		t.Errorf("scope = %v", claims["scope"])
	}
	iat, exp := claims["iat"].(float64), claims["exp"].(float64)
	if exp-iat != 3600 || time.Since(time.Unix(int64(iat), 0)) > time.Minute {
		t.Errorf("iat/exp = %v/%v", iat, exp)
	}
}

func TestTokenCachedAndRefetchedOnKeyChange(t *testing.T) {
	f := newFake(t)
	key := keyJSON(nil)
	a, _ := newAdapter(t, f, func(o *Options) { o.Key = func() string { return key } })
	ctx := context.Background()
	c1, err := a.credentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := a.credentials(ctx)
	if f.tokenCount() != 1 || c1.token != c2.token {
		t.Fatalf("token fetched %d times, want 1 (cached)", f.tokenCount())
	}
	key = keyJSON(func(m map[string]any) { m["private_key_id"] = "kid-rotated" })
	c3, _ := a.credentials(ctx)
	if f.tokenCount() != 2 || c3.token == c1.token {
		t.Fatalf("a rotated key must fetch a fresh token (fetches %d)", f.tokenCount())
	}
}

func TestTokenRefreshedBeforeExpiry(t *testing.T) {
	f := newFake(t)
	f.handle("token", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		f.tokens++
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"access_token": "ya29.short", "expires_in": 1})
	})
	a, _ := newAdapter(t, f, nil)
	_, _ = a.credentials(context.Background())
	time.Sleep(600 * time.Millisecond)
	_, _ = a.credentials(context.Background())
	if f.tokenCount() != 2 {
		t.Fatalf("token fetches = %d, want 2 after expiry", f.tokenCount())
	}
}

func TestUnauthorizedDropsTokenAndRetriesOnce(t *testing.T) {
	f := newFake(t)
	calls := 0
	f.handle("spaces.list", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			writeJSON(w, 401, googleErr(401, "expired"))
			return
		}
		writeJSON(w, 200, map[string]any{})
	})
	a, _ := newAdapter(t, f, nil)
	err := a.call(context.Background(), request{op: "spaces.list", method: http.MethodGet,
		url: func(string) string { return a.chat + "/v1/spaces" }}, nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	recs := f.records("spaces.list")
	if len(recs) != 2 || recs[0].Auth == recs[1].Auth || f.tokenCount() != 2 {
		t.Fatalf("want a retry with a fresh token; calls=%d tokens=%d", len(recs), f.tokenCount())
	}
	if !strings.HasPrefix(recs[1].Auth, "Bearer ya29.") {
		t.Fatalf("auth header = %q", recs[1].Auth)
	}
}

func TestReadKeyRejects(t *testing.T) {
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ecDER, _ := x509.MarshalPKCS8PrivateKey(ecKey)
	ecPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}))
	for name, raw := range map[string]string{
		"empty":      "",
		"not json":   "hello",
		"user creds": keyJSON(func(m map[string]any) { m["type"] = "authorized_user" }),
		"no project": keyJSON(func(m map[string]any) { delete(m, "project_id") }),
		"no email":   keyJSON(func(m map[string]any) { delete(m, "client_email") }),
		"no key":     keyJSON(func(m map[string]any) { delete(m, "private_key") }),
		"not pem":    keyJSON(func(m map[string]any) { m["private_key"] = "garbage" }),
		"bad der": keyJSON(func(m map[string]any) {
			m["private_key"] = "-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"
		}),
		"not rsa (ec)": keyJSON(func(m map[string]any) { m["private_key"] = ecPEM }),
	} {
		t.Run(name, func(t *testing.T) {
			k, err := readKey(raw)
			if err == nil {
				_, err = k.signer()
			}
			if !errors.Is(err, ErrBadKey) {
				t.Fatalf("err = %v, want ErrBadKey", err)
			}
		})
	}
}

func TestPKCS1KeyAccepted(t *testing.T) {
	p1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testRSA)}))
	k, err := readKey(keyJSON(func(m map[string]any) { m["private_key"] = p1 }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.signer(); err != nil {
		t.Fatalf("PKCS#1 key refused: %v", err)
	}
}

func TestTokenEndpointFailures(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{400, ErrAuth}, {401, ErrAuth}, {403, ErrAuth}, {429, ErrRateLimited}, {500, ErrServer},
	} {
		f := newFake(t)
		f.handle("token", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, tc.status, map[string]any{"error": "invalid_grant", "error_description": "Invalid JWT Signature."})
		})
		a, _ := newAdapter(t, f, nil)
		_, err := a.credentials(context.Background())
		if !errors.Is(err, tc.want) {
			t.Errorf("status %d: err = %v, want %v", tc.status, err, tc.want)
		}
	}
	f := newFake(t)
	f.handle("token", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]any{}) })
	a, _ := newAdapter(t, f, nil)
	if _, err := a.credentials(context.Background()); !errors.Is(err, ErrServer) {
		t.Errorf("no access_token: err = %v, want ErrServer", err)
	}
}
