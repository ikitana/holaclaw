// Package oauth is the client side of OAuth 2.1 as the MCP authorization
// spec describes it (docs/connectors.md §3, §11): discovery, client
// identity, PKCE authorization, code exchange and refresh. It stores
// nothing; the connector manager persists what it returns.
//
// No error text carries server-provided text, a URL, a token, a code or the
// PKCE verifier: an OAuth error answer is folded into a closed set of codes.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// maxBody caps every response read, metadata and error bodies included.
	maxBody = 256 << 10
	// defaultTimeout bounds a request when the client sets no timeout.
	defaultTimeout = 30 * time.Second
	// maxRedirects bounds the redirects a metadata fetch follows.
	maxRedirects = 5
	// maxURL caps a URL or a client id taken from a server.
	maxURL = 2 << 10
	// maxList and maxItem cap a list from a server, scopes included.
	maxList = 64
	maxItem = 256
)

var (
	// ErrNotProtected means the server named no resource metadata and
	// publishes none at the well-known paths: there is nothing to authorize
	// against (an open server, or not an OAuth one).
	ErrNotProtected = errors.New("oauth: server publishes no protected resource metadata")
	// ErrInsecureURL means a metadata URL, an endpoint or a redirect is not
	// https.
	ErrInsecureURL = errors.New("oauth: url is not https")
	// ErrBadMetadata means resource or authorization server metadata is
	// missing, malformed, out of bounds or answered with an error status.
	ErrBadMetadata = errors.New("oauth: bad authorization metadata")
	// ErrResourceMismatch means the protected resource metadata describes
	// another resource than the server URL.
	ErrResourceMismatch = errors.New("oauth: resource metadata is for another server")
	// ErrIssuerMismatch means authorization server metadata, or the iss of
	// a redirect (RFC 9207), names another issuer than expected.
	ErrIssuerMismatch = errors.New("oauth: authorization server issuer mismatch")
	// ErrIdentityMismatch means a client identity is used with another
	// issuer or redirect than it was made for.
	ErrIdentityMismatch = errors.New("oauth: client identity is for another issuer or redirect")
	// ErrNoS256 means the authorization server does not list PKCE S256; an
	// absent code_challenge_methods_supported counts as no PKCE.
	ErrNoS256 = errors.New("oauth: authorization server does not support PKCE S256")
	// ErrNoRegistration means no client identity applies: no pre-registered
	// client id, no usable metadata document and no registration endpoint.
	ErrNoRegistration = errors.New("oauth: no client identity available")
	// ErrCIMDRefused matches an answer that refuses the client id metadata
	// document; the manager records it and uses registration next time.
	ErrCIMDRefused = errors.New("oauth: client metadata document refused")
	// ErrInvalidGrant matches an invalid_grant answer: the code or the
	// refresh token is dead.
	ErrInvalidGrant = errors.New("oauth: invalid grant")
	// ErrStateMismatch means the state of the redirect is not the intent's.
	ErrStateMismatch = errors.New("oauth: state mismatch")
	// ErrBadResponse means a token or registration answer is not usable.
	ErrBadResponse = errors.New("oauth: bad authorization server response")
	// ErrTooLarge means a successful response body exceeded its cap.
	ErrTooLarge = errors.New("oauth: response too large")
)

var (
	// errRedirectedPost refuses a redirect of a token or registration
	// request, which would resend a code or a secret to another URL.
	errRedirectedPost   = errors.New("oauth: redirect of a post refused")
	errTooManyRedirects = errors.New("oauth: too many redirects")
)

// Error is an OAuth error answer. Code is the "error" member folded into the
// registered codes, anything else being "unknown"; error_description is
// dropped, so the text is always the daemon's.
type Error struct {
	Code string
	cimd bool // the answer refuses the metadata document as a client id
}

func (e *Error) Error() string { return "oauth: authorization server answered " + e.Code }

// Is matches ErrInvalidGrant and, for a refused metadata document,
// ErrCIMDRefused.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrInvalidGrant:
		return e.Code == "invalid_grant"
	case ErrCIMDRefused:
		return e.cimd
	}
	return false
}

// knownCodes are the registered OAuth error codes (RFC 6749, 7591, 8707).
var knownCodes = map[string]bool{
	"invalid_request": true, "invalid_client": true, "invalid_grant": true,
	"unauthorized_client": true, "unsupported_grant_type": true, "invalid_scope": true,
	"access_denied": true, "unsupported_response_type": true, "server_error": true,
	"temporarily_unavailable": true, "invalid_redirect_uri": true,
	"invalid_client_metadata": true, "invalid_software_statement": true,
	"unapproved_software_statement": true, "invalid_target": true,
}

// foldCode maps a server error code onto the registered ones or "unknown".
func foldCode(code string) string {
	if knownCodes[code] {
		return code
	}
	return "unknown"
}

// newError folds a server error code into the closed set. A refusal of the
// client itself, while the client is the metadata document, is a refused
// document.
func newError(code string, id Identity) *Error {
	code = foldCode(code)
	refused := code == "invalid_client" || code == "unauthorized_client"
	return &Error{Code: code, cimd: refused && id.Kind == IdentityCIMD}
}

// transportError is a failed request. Its text names only the cause, never
// the URL or the peer address; the chain stays for errors.Is.
type transportError struct{ err error }

func (e *transportError) Error() string {
	var ne interface{ Timeout() bool }
	cause := "transport error"
	for _, known := range []error{ErrInsecureURL, errRedirectedPost, errTooManyRedirects, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(e.err, known) {
			return "oauth: request failed: " + known.Error()
		}
	}
	if errors.As(e.err, &ne) && ne.Timeout() {
		cause = "timeout"
	}
	return "oauth: request failed: " + cause
}

func (e *transportError) Unwrap() error { return e.err }

func transportErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return &transportError{err: err}
}

// Client talks to authorization servers. The zero value is ready to use.
type Client struct {
	// HTTP sends every request; nil means http.DefaultTransport. Without a
	// Timeout each request gets 30s. It is copied, never changed: its
	// CheckRedirect is replaced so that only a GET follows a redirect, and
	// only to https.
	HTTP *http.Client

	now     func() time.Time // tests
	timeout time.Duration    // tests; 0 is defaultTimeout
}

func (c *Client) clock() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *Client) httpClient() *http.Client {
	var hc http.Client
	if c != nil && c.HTTP != nil {
		hc = *c.HTTP
	}
	hc.CheckRedirect = checkRedirect
	return &hc
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	switch {
	case via[0].Method != http.MethodGet:
		return errRedirectedPost
	case req.URL.Scheme != "https":
		return ErrInsecureURL
	case len(via) >= maxRedirects:
		return errTooManyRedirects
	}
	return nil
}

// send performs one request and reads the capped body. A non-2xx body is
// cut at the cap instead of failing: it is only classified, never used.
func (c *Client) send(req *http.Request) (int, []byte, error) {
	hc := c.httpClient()
	if hc.Timeout == 0 {
		timeout := defaultTimeout
		if c != nil && c.timeout > 0 {
			timeout = c.timeout
		}
		ctx, cancel := context.WithTimeout(req.Context(), timeout)
		defer cancel()
		req = req.WithContext(ctx)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, transportErr(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return 0, nil, transportErr(err)
	}
	if len(body) > maxBody {
		if !success(resp.StatusCode) {
			return resp.StatusCode, body[:maxBody], nil
		}
		return 0, nil, ErrTooLarge
	}
	return resp.StatusCode, body, nil
}

func success(status int) bool { return status >= 200 && status <= 299 }

// getJSON fetches https metadata into v. A non-200 status is returned with
// a nil error so the caller can try the next well-known form.
func (c *Client) getJSON(ctx context.Context, rawURL string, v any) (int, error) {
	if !isHTTPS(rawURL) {
		return 0, ErrInsecureURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: bad url", ErrBadMetadata)
	}
	status, body, err := c.send(req)
	if err != nil || status != http.StatusOK {
		return status, err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return status, fmt.Errorf("%w: not a JSON object", ErrBadMetadata)
	}
	return status, nil
}

// post sends a request body to an https endpoint and decodes a 2xx answer
// into v. An error answer becomes *Error when it carries an error code.
func (c *Client) post(req *http.Request, id Identity, v any) error {
	status, body, err := c.send(req)
	if err != nil {
		return err
	}
	var oe struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &oe)
	if oe.Error != "" {
		return newError(oe.Error, id)
	}
	if !success(status) {
		return fmt.Errorf("%w: status %d", ErrBadResponse, status)
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("%w: not JSON", ErrBadResponse)
	}
	return nil
}

func isHTTPS(rawURL string) bool {
	return len(rawURL) > len("https://") && strings.EqualFold(rawURL[:len("https://")], "https://")
}

// boundedList reports whether a server list is within the caps.
func boundedList(list []string) bool {
	if len(list) > maxList {
		return false
	}
	for _, s := range list {
		if len(s) > maxItem {
			return false
		}
	}
	return true
}

// validScopes reports whether every scope is a scope-token (RFC 6749 §3.3)
// within the caps.
func validScopes(scopes []string) bool {
	if !boundedList(scopes) {
		return false
	}
	for _, s := range scopes {
		if s == "" {
			return false
		}
		for i := 0; i < len(s); i++ {
			if c := s[i]; c < 0x21 || c == 0x22 || c == 0x5c || c > 0x7e {
				return false
			}
		}
	}
	return true
}

// validClientID reports whether a server-issued client id is printable and
// within the cap.
func validClientID(id string) bool {
	if id == "" || len(id) > maxURL || !utf8.ValidString(id) {
		return false
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}
