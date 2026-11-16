package googlechat

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// tokenScopes is what the app token may do: speak as the Chat app and pull
// from Pub/Sub. Nothing else (docs/channels/googlechat.md §7).
const tokenScopes = "https://www.googleapis.com/auth/chat.bot https://www.googleapis.com/auth/pubsub"

// tokenSlack refreshes a token this long before Google says it expires.
const tokenSlack = 2 * time.Minute

// serviceKey is the part of a service-account key file holaclaw reads.
// token_uri is ignored: the token always goes to Google's endpoint (or the
// test BaseURL), never to a URL the key file names.
type serviceKey struct {
	Type         string `json:"type"`
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
}

// readKey parses the key JSON without touching the private key. It is cheap
// enough to run per request, which keeps the key out of every field.
func readKey(raw string) (serviceKey, error) {
	var k serviceKey
	if strings.TrimSpace(raw) == "" {
		return k, &Error{Op: "key", Kind: KindBadKey, Message: "no service-account key is set"}
	}
	if err := json.Unmarshal([]byte(raw), &k); err != nil {
		// The JSON error is not quoted: it can carry a piece of the input.
		return k, &Error{Op: "key", Kind: KindBadKey, Message: "the key is not valid JSON"}
	}
	switch {
	case k.Type != "service_account":
		return k, &Error{Op: "key", Kind: KindBadKey, Message: "not a service-account key"}
	case k.ProjectID == "":
		return k, &Error{Op: "key", Kind: KindBadKey, Message: "the key has no project_id"}
	case k.ClientEmail == "":
		return k, &Error{Op: "key", Kind: KindBadKey, Project: k.ProjectID, Message: "the key has no client_email"}
	case k.PrivateKey == "":
		return k, &Error{Op: "key", Kind: KindBadKey, Project: k.ProjectID, Message: "the key has no private_key"}
	}
	return k, nil
}

// signer decodes the PEM private key: PKCS#8 as Google issues it, PKCS#1 as a
// fallback.
func (k serviceKey) signer() (*rsa.PrivateKey, error) {
	bad := func(msg string) error {
		return &Error{Op: "key", Kind: KindBadKey, Project: k.ProjectID, Message: msg}
	}
	block, _ := pem.Decode([]byte(k.PrivateKey))
	if block == nil {
		return nil, bad("private_key is not PEM")
	}
	if pk, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := pk.(*rsa.PrivateKey)
		if !ok {
			return nil, bad("private_key is not an RSA key")
		}
		return rk, nil
	}
	rk, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, bad("private_key cannot be parsed")
	}
	return rk, nil
}

// assertion builds the RS256-signed JWT exchanged for an access token.
func (k serviceKey) assertion(aud string, now time.Time) (string, error) {
	rk, err := k.signer()
	if err != nil {
		return "", err
	}
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if k.PrivateKeyID != "" {
		header["kid"] = k.PrivateKeyID
	}
	claims := map[string]any{
		"iss":   k.ClientEmail,
		"scope": tokenScopes,
		"aud":   aud,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(hb) + "." + enc.EncodeToString(cb)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, rk, crypto.SHA256, sum[:])
	if err != nil {
		return "", &Error{Op: "key", Kind: KindBadKey, Project: k.ProjectID, Message: "signing failed"}
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// tokenCache holds the one access token. It is keyed by a hash of the key, so
// a rotated key fetches a fresh token without the key itself being kept.
type tokenCache struct {
	mu      sync.Mutex
	keyHash [32]byte
	token   string
	expires time.Time
}

func (c *tokenCache) get(h [32]byte) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" || c.keyHash != h || time.Now().After(c.expires) {
		return "", false
	}
	return c.token, true
}

func (c *tokenCache) put(h [32]byte, tok string, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keyHash, c.token, c.expires = h, tok, expires
}

// drop forgets tok after Google refused it.
func (c *tokenCache) drop(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == tok {
		c.token = ""
	}
}

// current returns the cached token, for redaction only.
func (c *tokenCache) current() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// creds is one request's view of the key: what it needs, nothing kept.
type creds struct {
	token   string
	project string
}

// credentials reads the key and returns a valid access token for it.
func (a *Adapter) credentials(ctx context.Context) (creds, error) {
	raw := a.opt.Key()
	k, err := readKey(raw)
	if err != nil {
		return creds{}, err
	}
	c := creds{project: k.ProjectID}
	h := sha256.Sum256([]byte(raw))
	if tok, ok := a.tokens.get(h); ok {
		c.token = tok
		return c, nil
	}
	tok, ttl, err := a.fetchToken(ctx, k, raw)
	if err != nil {
		return creds{}, err
	}
	exp := time.Now().Add(ttl - tokenSlack)
	if ttl <= tokenSlack {
		exp = time.Now().Add(ttl / 2)
	}
	a.tokens.put(h, tok, exp)
	c.token = tok
	return c, nil
}

// tokenResponse is the token endpoint's answer, success or failure.
type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// fetchToken exchanges a signed assertion for an access token.
func (a *Adapter) fetchToken(ctx context.Context, k serviceKey, raw string) (string, time.Duration, error) {
	return fetchToken(ctx, a.hc, a.tokenURL, k, raw)
}

func fetchToken(ctx context.Context, hc *http.Client, tokenURL string, k serviceKey, raw string) (string, time.Duration, error) {
	jwt, err := k.assertion(tokenURL, time.Now())
	if err != nil {
		return "", 0, err
	}
	scrub := func(s string) string { return redact(s, append(keySecrets(raw), jwt)...) }
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {jwt},
	}
	rctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, &Error{Op: "token", Kind: KindBadRequest, Project: k.ProjectID, Message: scrub(err.Error())}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, transportError(ctx, "token", k.ProjectID, err, scrub)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", 0, transportError(ctx, "token", k.ProjectID, err, scrub)
	}
	var tr tokenResponse
	_ = json.Unmarshal(body, &tr)
	if resp.StatusCode/100 != 2 {
		e := &Error{Op: "token", Kind: kindOf(resp.StatusCode), Status: resp.StatusCode, Project: k.ProjectID}
		// The token endpoint answers a deleted, disabled or unknown key with
		// 400 invalid_grant / 401 invalid_client: the key itself is refused.
		if resp.StatusCode == 400 || resp.StatusCode == 401 || resp.StatusCode == 403 {
			e.Kind = KindAuth
		}
		if resp.StatusCode == 429 {
			e.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		}
		msg := tr.Error
		if tr.ErrorDescription != "" {
			msg += ": " + tr.ErrorDescription
		}
		e.Message = truncate(scrub(msg), 300)
		return "", 0, e
	}
	if tr.AccessToken == "" {
		return "", 0, &Error{Op: "token", Kind: KindServer, Status: resp.StatusCode, Project: k.ProjectID, Message: "no access_token in response"}
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	return tr.AccessToken, ttl, nil
}

// --- redaction -------------------------------------------------------------

var (
	reJWT        = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}(?:\.[A-Za-z0-9_-]*){0,2}`)
	reOAuthToken = regexp.MustCompile(`ya29\.[A-Za-z0-9._-]+`)
	rePEM        = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`)
)

// redact removes credentials from s: any JWT, any Google OAuth token, any PEM
// private key, and each extra secret given (at least 8 bytes long).
func redact(s string, secrets ...string) string {
	if s == "" {
		return s
	}
	for _, sec := range secrets {
		if len(sec) >= 8 {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	s = rePEM.ReplaceAllString(s, "[redacted]")
	s = reJWT.ReplaceAllString(s, "[redacted]")
	return reOAuthToken.ReplaceAllString(s, "[redacted]")
}

// keySecrets lists the parts of a key that must never appear anywhere: the
// whole key, the PEM, each base64 line of it (an echo may have re-escaped the
// newlines) and the key id.
func keySecrets(raw string) []string {
	out := []string{raw}
	var k serviceKey
	if json.Unmarshal([]byte(raw), &k) != nil {
		return out
	}
	out = append(out, k.PrivateKey, k.PrivateKeyID)
	for _, line := range strings.Split(k.PrivateKey, "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= 16 && !strings.HasPrefix(line, "-----") {
			out = append(out, line)
		}
	}
	return out
}

// scrub is redact with everything this adapter knows: the current key and the
// cached token.
func (a *Adapter) scrub(s string) string {
	return redact(s, append(keySecrets(a.opt.Key()), a.tokens.current())...)
}

// scrubWith is scrub plus the token a failing request actually sent, which a
// refresh or a drop may already have replaced in the cache.
func (a *Adapter) scrubWith(token string) func(string) string {
	return func(s string) string { return redact(a.scrub(s), token) }
}
