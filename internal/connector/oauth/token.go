package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Token is a token endpoint answer. Both tokens are credentials (auth.toml
// only) and never printed or marshalled.
type Token struct {
	AccessToken  string    `json:"-"`
	RefreshToken string    `json:"-"`
	Expiry       time.Time // zero when the server gave no usable lifetime
	Scopes       []string  // granted; nil after a refresh that did not say
}

func (t Token) String() string {
	return fmt.Sprintf("{expiry:%s scopes:%v refresh:%t}", t.Expiry.Format(time.RFC3339), t.Scopes, t.RefreshToken != "")
}

func (t Token) GoString() string { return "oauth.Token" + t.String() }

// tokenResponse is RFC 6749 §5.1.
type tokenResponse struct {
	AccessToken  string          `json:"access_token"`
	TokenType    string          `json:"token_type"`
	RefreshToken string          `json:"refresh_token"`
	ExpiresIn    json.RawMessage `json:"expires_in"` // a number or a string; anything else is no lifetime
	Scope        string          `json:"scope"`
}

// Exchange checks the redirect against the intent (CallbackError) and trades
// its code for tokens. Granted scopes default to the requested ones.
func (c *Client) Exchange(ctx context.Context, m Metadata, id Identity, a Authorization, cb Callback) (Token, error) {
	if err := CallbackError(id, a, cb); err != nil {
		return Token{}, err
	}
	if a.Issuer != m.Issuer || a.RedirectURI != id.RedirectURI {
		return Token{}, ErrIdentityMismatch
	}
	if cb.Code == "" {
		return Token{}, fmt.Errorf("%w: no authorization code", ErrBadResponse)
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {cb.Code},
		"redirect_uri":  {a.RedirectURI},
		"code_verifier": {a.Verifier},
		"resource":      {m.Resource},
	}
	t, err := c.token(ctx, m, id, form)
	if err != nil {
		return Token{}, err
	}
	if t.Scopes == nil {
		t.Scopes = append([]string(nil), a.Scopes...)
	}
	return t, nil
}

// Refresh trades a refresh token for new tokens; the old refresh token is
// kept when none comes back. A dead one matches ErrInvalidGrant.
func (c *Client) Refresh(ctx context.Context, m Metadata, id Identity, refreshToken string) (Token, error) {
	if refreshToken == "" {
		return Token{}, ErrInvalidGrant
	}
	t, err := c.token(ctx, m, id, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"resource":      {m.Resource},
	})
	if err != nil {
		return Token{}, err
	}
	if t.RefreshToken == "" {
		t.RefreshToken = refreshToken
	}
	return t, nil
}

// token posts a grant with the client's authentication: client_secret_basic
// unless the server lists only client_secret_post, and the client id alone
// for a public client. An identity of another issuer is refused before
// anything is sent, so a secret or a refresh token never reaches another
// authorization server.
func (c *Client) token(ctx context.Context, m Metadata, id Identity, form url.Values) (Token, error) {
	if id.Issuer == "" || id.Issuer != m.Issuer {
		return Token{}, ErrIdentityMismatch
	}
	if !isHTTPS(m.TokenEndpoint) {
		return Token{}, ErrInsecureURL
	}
	// A secret, pre-registered or issued by registration, makes the client
	// confidential even though it registered as "none".
	basic := id.ClientSecret != "" && (slices.Contains(m.TokenAuthMethods, "client_secret_basic") ||
		!slices.Contains(m.TokenAuthMethods, "client_secret_post"))
	switch {
	case basic:
	case id.ClientSecret != "":
		form.Set("client_id", id.ClientID)
		form.Set("client_secret", id.ClientSecret)
	default:
		form.Set("client_id", id.ClientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, fmt.Errorf("%w: bad token endpoint", ErrBadMetadata)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basic {
		// RFC 6749 §2.3.1: both parts form-encoded before base64.
		req.SetBasicAuth(url.QueryEscape(id.ClientID), url.QueryEscape(id.ClientSecret))
	}

	var tr tokenResponse
	if err := c.post(req, id, &tr); err != nil {
		return Token{}, err
	}
	if tr.AccessToken == "" {
		return Token{}, fmt.Errorf("%w: no access token", ErrBadResponse)
	}
	if tr.TokenType != "" && !strings.EqualFold(tr.TokenType, "bearer") {
		return Token{}, fmt.Errorf("%w: token type is not bearer", ErrBadResponse)
	}
	t := Token{AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken}
	if secs := lifetime(tr.ExpiresIn); secs > 0 {
		t.Expiry = c.clock().Add(time.Duration(secs) * time.Second).UTC()
	}
	if tr.Scope != "" {
		t.Scopes = strings.Fields(tr.Scope)
		if !validScopes(t.Scopes) {
			return Token{}, fmt.Errorf("%w: bad scope", ErrBadResponse)
		}
	}
	return t, nil
}

// lifetime reads expires_in as seconds, from a number or a numeric string;
// anything else, or an absurd value, is 0.
func lifetime(raw json.RawMessage) int64 {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		s = string(raw)
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || f <= 0 || f >= 1<<31 {
		return 0
	}
	return int64(f)
}
