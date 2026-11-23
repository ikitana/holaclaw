package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// Upstream endpoints and identifiers of the unofficial ChatGPT-subscription
// flow (docs/models.md). Every one of them is overridable through Options so
// tests never touch the network.
const (
	DefaultAuthBaseURL = "https://auth.openai.com"
	// DefaultVerificationURL is the page the user opens to type the code. The
	// device-code response does not name one.
	DefaultVerificationURL = "https://auth.openai.com/codex/device"
	// DefaultClientID is the official Codex CLI client id.
	DefaultClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// DefaultInferenceURL is the Responses endpoint of the ChatGPT backend.
	DefaultInferenceURL = "https://chatgpt.com/backend-api/codex/responses"
	// Originator identifies the client to the backend.
	Originator = "codex_cli_rs"

	pathUserCode    = "/api/accounts/deviceauth/usercode"
	pathDeviceToken = "/api/accounts/deviceauth/token"
	pathOAuthToken  = "/oauth/token"

	// DefaultRedirectURI is the device callback the Codex CLI registers; the
	// authorization-code exchange echoes it back.
	DefaultRedirectURI = "https://auth.openai.com/deviceauth/callback"
)

// refreshSkew is how long before expiry a token is refreshed proactively.
const refreshSkew = 5 * time.Minute

// maxBody caps what we read from an auth response, error bodies included.
const maxBody = 1 << 20

// loginHint is the operator-facing remedy. It is part of every auth error.
const loginHint = "run `holaclaw login codex`"

// authState says whether the daemon still holds usable credentials.
type authState int

const (
	stateUnknown   authState = iota // nothing tried yet
	stateOK                         // tokens present
	stateLoggedOut                  // refresh rejected: only a new login helps
)

// auth owns the rotating tokens of one provider instance. Refresh tokens are
// single-use with reuse detection upstream, so exactly one mutex owns refresh:
// concurrent callers block on it and reuse the result of the refresh that was
// already in flight (ARCHITECTURE.md §4, docs/models.md).
type auth struct {
	store    *store
	name     string
	hc       *http.Client
	now      func() time.Time
	authBase string
	clientID string
	events   event.Sink

	mu sync.Mutex
	// gen is bumped by every successful refresh. It is atomic so a caller can
	// read it WITHOUT the mutex — the refresh in flight is holding it.
	gen   atomic.Uint64
	state authState
	cur   tokens
}

// token returns a valid access token, refreshing when the current one expires
// within refreshSkew. The whole path is under the one mutex, so a second caller
// arriving during a refresh waits and then sees the fresh token.
func (a *auth) token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.reload(); err != nil {
		return "", err
	}
	if a.cur.Access != "" && !a.expiring(a.cur) {
		return a.cur.Access, nil
	}
	if err := a.refreshLocked(ctx); err != nil {
		return "", err
	}
	return a.cur.Access, nil
}

// forceRefresh refreshes even when the token looks fresh: the transport calls
// it on a 401. The generation is read before queueing for the mutex, so a
// refresh that completed while this caller waited already answers this 401 —
// several simultaneous 401s therefore spend exactly one refresh token, which
// matters because reuse is detected upstream.
func (a *auth) forceRefresh(ctx context.Context) error {
	seen := a.gen.Load() // read without the mutex: the refresh in flight holds it
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.gen.Load() != seen { // another caller already rotated: this 401 is answered
		return nil
	}
	if err := a.reload(); err != nil {
		return err
	}
	return a.refreshLocked(ctx)
}

// accessToken is the current token without any network call; it is what the
// Responses transport puts in the Authorization header. It blocks while a
// refresh is running, which is exactly the intent.
func (a *auth) accessToken() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur.Access == "" {
		_ = a.reload()
	}
	return a.cur.Access
}

// accountID is the ChatGPT account the tokens belong to, for the
// chatgpt-account-id header.
func (a *auth) accountID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur.Access == "" && a.cur.AccountID == "" {
		_ = a.reload()
	}
	return a.cur.AccountID
}

// reload picks the tokens up from the secrets file the first time they are
// needed. Once loaded, this process is their single owner, so a later read is
// served from memory.
func (a *auth) reload() error {
	if a.state != stateUnknown {
		if a.state == stateLoggedOut {
			return a.loggedOutError()
		}
		return nil
	}
	t := a.store.load()
	if t.Access == "" && t.Refresh == "" {
		return a.authError("codex is not logged in: " + loginHint)
	}
	a.cur = t
	a.state = stateOK
	return nil
}

// setTokens records a freshly obtained set — used by the login flow, which
// persists first and only then makes them current.
func (a *auth) setTokens(t tokens) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cur = t
	a.state = stateOK
	a.gen.Add(1)
}

// adopt runs write, which stores a set of tokens in the secrets file, and makes
// what the file then holds current. The mutex is held across both, so a
// refresh in flight ends first and cannot save the tokens it rotated over the
// new ones. When write fails the current tokens stay in use.
func (a *auth) adopt(write func() error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	access, account := a.store.get(keyAccessToken), a.store.get(keyAccountID)
	if err := write(); err != nil {
		return err
	}
	t := a.store.load()
	// A write may name the tokens alone. The account id the entry then still
	// holds is that of the access token it replaced, so only the new token's
	// own claim counts.
	if t.Access != access && a.store.get(keyAccountID) == account {
		c, _ := decodeClaims(t.Access)
		t.AccountID = c.AccountID
	}
	a.cur = t
	a.state = stateOK
	a.gen.Add(1)
	return nil
}

func (a *auth) expiring(t tokens) bool {
	if t.ExpiresAt.IsZero() {
		return true // unknown expiry: treat as due, the refresh is cheap
	}
	return t.ExpiresAt.Sub(a.now()) < refreshSkew
}

// refreshLocked performs one refresh. The caller holds a.mu.
//
// Order matters: the new refresh token is persisted BEFORE the new access token
// is used. The old refresh token is spent the moment the server answers, so a
// crash between answer and write would leave a dead file; if the write fails we
// surface the error and keep the old in-memory state rather than discarding it
// silently.
func (a *auth) refreshLocked(ctx context.Context) error {
	if a.state == stateLoggedOut {
		return a.loggedOutError()
	}
	if a.cur.Refresh == "" {
		return a.authError("codex has no refresh token: " + loginHint)
	}

	form := refreshForm(a.clientID, a.cur.Refresh)
	tr, err := a.postToken(ctx, pathOAuthToken, form)
	if err != nil {
		err = a.redact(err)
		var re *oauthError
		if errors.As(err, &re) && re.reuseDetected() {
			// Single-use token replayed or revoked: no automatic retry loop —
			// only a new login can fix this.
			a.state = stateLoggedOut
			return a.loggedOutError()
		}
		return err
	}

	next := mergeTokens(a.now, a.cur, tr)
	if err := a.store.save(next); err != nil {
		// The old state stays current: the operator sees the write error.
		return &provider.Error{Kind: provider.ErrServer, Provider: a.name, Message: err.Error(), Err: err}
	}
	a.cur = next
	a.state = stateOK
	a.gen.Add(1)
	a.emitRefreshed(ctx, next)
	return nil
}

// mergeTokens folds a token response into the current set: a field the server
// left out keeps its previous value (some refreshes return no id_token). The
// JWT wins over expires_in for the expiry, and carries the account id.
func mergeTokens(now func() time.Time, prev tokens, tr tokenResponse) tokens {
	next := prev
	if tr.AccessToken != "" {
		next.Access = tr.AccessToken
	}
	if tr.RefreshToken != "" {
		next.Refresh = tr.RefreshToken
	}
	if tr.IDToken != "" {
		next.ID = tr.IDToken
	}
	next.ExpiresAt = time.Time{}
	if tr.ExpiresIn > 0 {
		next.ExpiresAt = now().Add(time.Duration(tr.ExpiresIn) * time.Second).UTC()
	}
	if c, err := decodeClaims(next.Access); err == nil {
		if !c.ExpiresAt.IsZero() {
			next.ExpiresAt = c.ExpiresAt
		}
		if c.AccountID != "" {
			next.AccountID = c.AccountID
		}
	}
	return next
}

// emitRefreshed reports the rotation. Attrs carry the provider name and the new
// expiry — never a token (ARCHITECTURE.md §4).
func (a *auth) emitRefreshed(ctx context.Context, t tokens) {
	if a.events == nil {
		return
	}
	attrs := map[string]any{"provider": a.name}
	if !t.ExpiresAt.IsZero() {
		attrs["expires_at"] = t.ExpiresAt.UTC().Format(time.RFC3339)
	}
	a.events.Emit(ctx, event.New(event.AuthRefreshed, "", "", "", attrs))
}

// redact strips the credentials this provider holds out of an error that came
// back from upstream: an error_description may quote the token it rejected.
func (a *auth) redact(err error) error {
	var oe *oauthError
	if errors.As(err, &oe) {
		oe.Detail = redactSecrets(oe.Detail, a.cur.Access, a.cur.Refresh, a.cur.ID)
	}
	return err
}

// redactSecrets replaces every non-trivial value with a marker.
func redactSecrets(s string, values ...string) string {
	for _, v := range values {
		if len(v) >= 8 {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return s
}

func (a *auth) authError(msg string) error {
	return &provider.Error{Kind: provider.ErrAuth, Provider: a.name, Message: msg}
}

func (a *auth) loggedOutError() error {
	return a.authError("codex login expired: " + loginHint)
}

// --- the wire ---------------------------------------------------------------

// tokenResponse is what /oauth/token and the device-token endpoint answer with.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	// The device-token endpoint answers a completed authorization with a code
	// to exchange rather than with tokens.
	AuthorizationCode string `json:"authorization_code"`
	Code              string `json:"code"`
	CodeVerifier      string `json:"code_verifier"`
}

// authorizationCode is the code to exchange, whichever field carried it.
func (t tokenResponse) authorizationCode() string {
	if t.AuthorizationCode != "" {
		return t.AuthorizationCode
	}
	return t.Code
}

// refreshForm and exchangeForm are the two bodies we send to /oauth/token.
//
// THIS IS THE PART MOST LIKELY TO DRIFT. The flow is unofficial: field names,
// the presence of redirect_uri and the PKCE verifier are all observed from the
// Codex CLI, not documented. When upstream changes, these two functions and the
// fixtures under testdata/ are what change.
func refreshForm(clientID, refreshToken string) url.Values {
	return url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"scope":         {"openid profile email"},
	}
}

func exchangeForm(clientID, code, verifier, redirectURI string) url.Values {
	v := url.Values{
		"client_id":    {clientID},
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {redirectURI},
	}
	if verifier != "" { // PKCE, only when the device response supplied one
		v.Set("code_verifier", verifier)
	}
	return v
}

// oauthError is an OAuth-shaped error body, e.g. {"error":"invalid_grant"}.
type oauthError struct {
	Status int
	Code   string
	Detail string
}

func (e *oauthError) Error() string {
	s := "codex: " + e.Code
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

// reuseDetected reports the two answers that mean the refresh token is gone for
// good: a rejected grant, or an explicit reuse detection.
func (e *oauthError) reuseDetected() bool {
	switch e.Code {
	case "invalid_grant", "invalid_request", "refresh_token_reuse_detected", "unauthorized_client":
		return true
	}
	return strings.Contains(strings.ToLower(e.Detail), "reuse")
}

// postToken posts a form to an auth endpoint and decodes the token response.
// An error body is turned into *oauthError; nothing of the response is logged.
func (a *auth) postToken(ctx context.Context, path string, form url.Values) (tokenResponse, error) {
	return postForm(ctx, a.hc, a.name, a.authBase+path, form)
}

// postForm posts a form and decodes a token response. A 200 may still carry an
// OAuth error — the device poll answers `authorization_pending` that way — so
// both the decoded body and the error are returned.
func postForm(ctx context.Context, hc *http.Client, name, endpoint string, form url.Values) (tokenResponse, error) {
	body, err := postFormRaw(ctx, hc, name, endpoint, form)
	if err != nil {
		return tokenResponse{}, err
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return tokenResponse{}, &provider.Error{
			Kind: provider.ErrServer, Provider: name,
			Message: "auth response is not JSON",
		}
	}
	if code := errorCode(body); code != "" {
		return tr, &oauthError{Status: http.StatusOK, Code: code, Detail: errorDetail(body)}
	}
	return tr, nil
}

// postFormRaw is the one HTTP call of the auth flow. Only the OAuth error code
// and description are ever lifted out of a body: the rest may quote a token.
func postFormRaw(ctx context.Context, hc *http.Client, name, endpoint string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrBadRequest, Provider: name, Message: "bad auth url", Err: err}
	}
	return authRequest(hc, req, name, "application/x-www-form-urlencoded")
}

func authRequest(hc *http.Client, req *http.Request, name, contentType string) ([]byte, error) {
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("originator", Originator)

	resp, err := hc.Do(req)
	if err != nil {
		return nil, &provider.Error{
			Kind: provider.ErrNetwork, Provider: name,
			Message: "auth request failed", Err: err,
		}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, &provider.Error{
			Kind: provider.ErrNetwork, Provider: name,
			Message: "reading auth response", Err: err,
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, decodeOAuthError(resp.StatusCode, body)
	}
	return body, nil
}

func decodeOAuthError(status int, body []byte) error {
	code := errorCode(body)
	if code == "" {
		code = fmt.Sprintf("http_%d", status)
	}
	return &oauthError{Status: status, Code: code, Detail: errorDetail(body)}
}

// errorCode and errorDetail read the OAuth error envelope. Only these two
// fields are ever taken from an auth body: a body may quote a token, and the
// rest of it never leaves this function.
func errorCode(body []byte) string {
	var env struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	switch e := env.Error.(type) {
	case string:
		return e
	case map[string]any:
		if s, ok := e["code"].(string); ok && s != "" {
			return s
		}
		if s, ok := e["type"].(string); ok {
			return s
		}
	}
	return ""
}

func errorDetail(body []byte) string {
	var env struct {
		Description string          `json:"error_description"`
		Error       json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil {
		return ""
	}
	if env.Description != "" {
		return env.Description
	}
	var nested struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(env.Error, &nested)
	return nested.Message
}
