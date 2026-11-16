package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Authorization is one open intent: the URL a person opens and what the code
// exchange needs. Verifier is a credential and State binds the redirect to
// the intent; the manager keeps both in memory only, and neither is printed
// or marshalled.
type Authorization struct {
	URL         string
	State       string `json:"-"`
	Verifier    string `json:"-"`
	RedirectURI string
	Scopes      []string // requested
	Issuer      string   // the redirect's iss must be this (RFC 9207)
	RequireIss  bool     // the issuer promised an iss on the redirect
}

func (a Authorization) String() string {
	return fmt.Sprintf("{redirect:%s scopes:%v issuer:%s}", a.RedirectURI, a.Scopes, a.Issuer)
}

func (a Authorization) GoString() string { return "oauth.Authorization" + a.String() }

// Callback is the query of the redirect to the loopback listener. Code is a
// credential and never printed or marshalled; Issuer and Error are server
// text and printed only as presence and a folded code.
type Callback struct {
	Code   string `json:"-"` // the manager decodes the API body into its own struct
	State  string
	Issuer string // iss (RFC 9207)
	Error  string // error, sent instead of a code
}

func (c Callback) String() string {
	errCode := ""
	if c.Error != "" {
		errCode = foldCode(c.Error)
	}
	return fmt.Sprintf("{iss:%t error:%s code:%t}", c.Issuer != "", errCode, c.Code != "")
}

func (c Callback) GoString() string { return "oauth.Callback" + c.String() }

// Authorize builds the authorization URL for the identity's redirect: PKCE
// S256 with a fresh verifier, a fresh state, the resource and the scopes,
// m.Scopes when scopes is empty and none at all when both are.
func Authorize(m Metadata, id Identity, scopes []string) (Authorization, error) {
	if id.ClientID == "" || id.RedirectURI == "" {
		return Authorization{}, errors.New("oauth: authorize needs a client id and a redirect uri")
	}
	if id.Issuer != m.Issuer || (id.Kind == IdentityCIMD && !PinnedRedirect(id.RedirectURI)) {
		return Authorization{}, ErrIdentityMismatch
	}
	endpoint, err := url.Parse(m.AuthorizationEndpoint)
	if err != nil || !isHTTPS(m.AuthorizationEndpoint) {
		return Authorization{}, ErrInsecureURL
	}
	if len(scopes) == 0 {
		scopes = m.Scopes
	}
	verifier, state := random(), random()
	sum := sha256.Sum256([]byte(verifier))

	q := endpoint.Query()
	q.Set("response_type", "code")
	q.Set("client_id", id.ClientID)
	q.Set("redirect_uri", id.RedirectURI)
	q.Set("state", state)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	q.Set("resource", m.Resource)
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	endpoint.RawQuery = q.Encode()
	return Authorization{
		URL:         endpoint.String(),
		State:       state,
		Verifier:    verifier,
		RedirectURI: id.RedirectURI,
		Scopes:      append([]string(nil), scopes...),
		Issuer:      m.Issuer,
		RequireIss:  m.IssParameterSupported,
	}, nil
}

// CallbackError checks a redirect against its intent, state first and then
// iss, and only then classifies its error parameter: a refused metadata
// document matches ErrCIMDRefused. nil means the redirect is the intent's
// and carries no error.
func CallbackError(id Identity, a Authorization, cb Callback) error {
	if a.State == "" || subtle.ConstantTimeCompare([]byte(a.State), []byte(cb.State)) != 1 {
		return ErrStateMismatch
	}
	if (a.RequireIss || cb.Issuer != "") && cb.Issuer != a.Issuer {
		return ErrIssuerMismatch
	}
	if cb.Error != "" {
		return newError(cb.Error, id)
	}
	return nil
}

// random is 32 bytes from crypto/rand, base64url: 43 characters, as RFC 7636
// asks of a verifier.
func random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	return base64.RawURLEncoding.EncodeToString(b)
}
