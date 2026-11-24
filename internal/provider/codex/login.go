package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"holaclaw/internal/provider"
)

// Device-flow outcomes, as typed errors so the API and the CLI can tell a poll
// that simply has not happened yet from one that will never succeed.
var (
	// ErrAuthorizationPending means the user has not finished yet: poll again.
	ErrAuthorizationPending = errors.New("codex: authorization pending")
	// ErrSlowDown is a pending answer that also widens the poll interval.
	ErrSlowDown = errors.New("codex: polling too fast")
	// ErrDeviceCodeExpired means the code timed out; start a new login.
	ErrDeviceCodeExpired = errors.New("codex: device code expired")
	// ErrAccessDenied means the user refused the authorization.
	ErrAccessDenied = errors.New("codex: access denied")
	// ErrNoLogin is returned by Poll when no login is in flight.
	ErrNoLogin = errors.New("codex: no login in progress")
)

// slowDownStep is how much a slow_down answer widens the interval.
const slowDownStep = 5 * time.Second

// defaultInterval is used when the device response gives none.
const defaultInterval = 5 * time.Second

// deviceCodeSetting is the account setting the login needs. With it off,
// OpenAI's page refuses the code and points at the Codex CLI, and the polls
// stay pending until the code expires: nothing tells the two apart here.
const deviceCodeSetting = "your ChatGPT account must have device code sign-in enabled (ChatGPT Security Settings)"

// deviceCodeDocs is the documentation page that shows where the setting is.
const deviceCodeDocs = "https://holaclaw.ai/docs/server/first-chat#step-2-sign-in-to-chatgpt"

// defaultDeviceLifetime is used when the device response gives no expiry.
const defaultDeviceLifetime = 15 * time.Minute

// LoginStatus is the state of the device-code login of one provider instance.
type LoginStatus string

const (
	LoginNone    LoginStatus = "none"
	LoginPending LoginStatus = "pending"
	LoginDone    LoginStatus = "done"
	LoginFailed  LoginStatus = "failed"
)

// DeviceCode is what the user needs: a code and the page to type it on. The
// device code itself is a credential and stays inside Login.
type DeviceCode struct {
	UserCode        string
	VerificationURL string
	ExpiresAt       time.Time
	Interval        time.Duration
}

// LoginState is the queryable state of a login, for the control API.
type LoginState struct {
	Status     LoginStatus
	DeviceCode DeviceCode
	Detail     string // what a pending login needs, or why it failed; never a token
}

// Login drives the device-code flow for one provider instance. One login at a
// time: a second Start while one is pending returns the same code.
type Login struct {
	auth  *auth
	store *store
	sleep func(context.Context, time.Duration) error

	mu       sync.Mutex
	status   LoginStatus
	detail   string
	code     DeviceCode
	device   string // device_code, a credential
	verifier string // PKCE verifier, when the device response supplied one
	interval time.Duration
	nextPoll time.Time
}

// State reports what the login is doing.
func (l *Login) State() LoginState {
	l.mu.Lock()
	defer l.mu.Unlock()
	status := l.status
	if status == "" {
		status = LoginNone
	}
	return LoginState{Status: status, DeviceCode: l.code, Detail: l.detail}
}

// Start requests a user code. While one login is pending and unexpired, the
// same code comes back and no request is made.
func (l *Login) Start(ctx context.Context) (DeviceCode, error) {
	l.mu.Lock()
	if l.status == LoginPending && l.auth.now().Before(l.code.ExpiresAt) {
		code := l.code
		l.mu.Unlock()
		return code, nil
	}
	l.mu.Unlock()

	dr, err := l.postDevice(ctx)
	if err != nil {
		l.fail(err)
		return DeviceCode{}, err
	}
	if dr.UserCode == "" || dr.DeviceCode == "" {
		err := &provider.Error{
			Kind: provider.ErrServer, Provider: l.auth.name,
			Message: "device authorization response has no user code",
		}
		l.fail(err)
		return DeviceCode{}, err
	}

	now := l.auth.now()
	seconds, err := dr.Interval.Int64()
	if err != nil && dr.Interval != "" {
		l.fail(err)
		return DeviceCode{}, err
	}
	interval := time.Duration(seconds) * time.Second
	if interval <= 0 {
		interval = defaultInterval
	}
	lifetime := time.Duration(dr.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = defaultDeviceLifetime
	}
	code := DeviceCode{
		UserCode:        dr.UserCode,
		VerificationURL: verificationURL(dr),
		ExpiresAt:       now.Add(lifetime).UTC(),
		Interval:        interval,
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.status = LoginPending
	l.detail = deviceCodeSetting + ", see " + deviceCodeDocs
	l.code = code
	l.device = dr.DeviceCode
	l.verifier = dr.CodeVerifier
	l.interval = interval
	l.nextPoll = now.Add(interval)
	return code, nil
}

// Poll performs one poll step: it waits out the current interval, asks once and
// reports whether the login is complete. A pending answer is ErrAuthorizationPending
// or ErrSlowDown — not a failure.
func (l *Login) Poll(ctx context.Context) (bool, error) {
	l.mu.Lock()
	if l.status == LoginDone {
		l.mu.Unlock()
		return true, nil
	}
	if l.status != LoginPending {
		l.mu.Unlock()
		return false, ErrNoLogin
	}
	userCode := l.code.UserCode
	device, wait, expires := l.device, l.nextPoll.Sub(l.auth.now()), l.code.ExpiresAt
	l.mu.Unlock()

	if wait > 0 {
		if err := l.sleep(ctx, wait); err != nil {
			return false, err
		}
	}
	if !l.auth.now().Before(expires) {
		l.fail(ErrDeviceCodeExpired)
		return false, ErrDeviceCodeExpired
	}

	raw, err := l.postJSON(ctx, pathDeviceToken, map[string]string{
		"device_auth_id": device,
		"user_code":      userCode,
	})
	var tr tokenResponse
	if err == nil {
		if err = json.Unmarshal(raw, &tr); err != nil {
			// A body that is not JSON will not become one on the next poll.
			l.fail(err)
		} else if errorCode(raw) != "" {
			// An OAuth error in a 200 body may just be "pending": pollError
			// decides whether it is terminal.
			err = decodeOAuthError(http.StatusOK, raw)
		}
	}
	l.schedule()
	if err != nil {
		return false, l.pollError(l.redact(err))
	}
	if err := l.complete(ctx, tr); err != nil {
		// The token exchange quotes what it was given back on failure, so the
		// detail this records goes through the same redaction as a poll error:
		// LoginState.Detail is read by the API and the CLI.
		err = l.redact(err)
		l.fail(err)
		return false, err
	}
	return true, nil
}

// Run starts a login, hands the code to onCode and polls until it completes,
// the code expires or ctx ends.
func (l *Login) Run(ctx context.Context, onCode func(DeviceCode)) error {
	code, err := l.Start(ctx)
	if err != nil {
		return err
	}
	if onCode != nil {
		onCode(code)
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		done, err := l.Poll(ctx)
		switch {
		case done:
			return nil
		case errors.Is(err, ErrAuthorizationPending), errors.Is(err, ErrSlowDown):
			continue
		case err != nil:
			return err
		}
	}
}

// pollError maps an upstream answer onto a typed error and moves the state
// machine. slow_down widens the interval for every later poll.
func (l *Login) pollError(err error) error {
	var oe *oauthError
	if !errors.As(err, &oe) {
		return err // transport or provider error, already typed
	}
	if oe.Status == http.StatusForbidden || oe.Status == http.StatusNotFound {
		return ErrAuthorizationPending
	}
	switch oe.Code {
	case "authorization_pending":
		return ErrAuthorizationPending
	case "slow_down":
		l.mu.Lock()
		l.interval += slowDownStep
		l.nextPoll = l.auth.now().Add(l.interval)
		l.code.Interval = l.interval
		l.mu.Unlock()
		return ErrSlowDown
	case "expired_token":
		l.fail(ErrDeviceCodeExpired)
		return ErrDeviceCodeExpired
	case "access_denied":
		l.fail(ErrAccessDenied)
		return ErrAccessDenied
	default:
		l.fail(err)
		return err
	}
}

// complete turns a finished authorization into stored tokens: exchange the
// authorization code when the poll answered with one, then persist before the
// tokens are made current.
func (l *Login) complete(ctx context.Context, tr tokenResponse) error {
	if tr.AccessToken == "" {
		code := tr.authorizationCode()
		if code == "" {
			return &provider.Error{
				Kind: provider.ErrServer, Provider: l.auth.name,
				Message: "device authorization completed without a code",
			}
		}
		l.mu.Lock()
		verifier := l.verifier
		if tr.CodeVerifier != "" {
			verifier = tr.CodeVerifier
		}
		l.mu.Unlock()

		exchanged, err := postForm(ctx, l.auth.hc, l.auth.name, l.auth.authBase+pathOAuthToken,
			exchangeForm(l.auth.clientID, code, verifier, DefaultRedirectURI))
		if err != nil {
			var oe *oauthError
			if errors.As(err, &oe) {
				oe.Detail = redactSecrets(oe.Detail, code, verifier)
			}
			return err
		}
		tr = exchanged
	}
	if tr.AccessToken == "" {
		return &provider.Error{
			Kind: provider.ErrServer, Provider: l.auth.name,
			Message: "token exchange returned no access token",
		}
	}

	t := mergeTokens(l.auth.now, tokens{}, tr)
	if err := l.store.save(t); err != nil {
		return &provider.Error{Kind: provider.ErrServer, Provider: l.auth.name, Message: err.Error(), Err: err}
	}
	l.auth.setTokens(t)

	l.mu.Lock()
	l.status = LoginDone
	l.detail = ""
	l.device = ""
	l.verifier = ""
	l.mu.Unlock()
	return nil
}

// redact strips the device credentials out of an upstream error text.
func (l *Login) redact(err error) error {
	var oe *oauthError
	if errors.As(err, &oe) {
		l.mu.Lock()
		device, verifier, userCode := l.device, l.verifier, l.code.UserCode
		l.mu.Unlock()
		oe.Detail = redactSecrets(oe.Detail, device, verifier, userCode)
	}
	return err
}

// schedule sets the earliest time of the next poll.
func (l *Login) schedule() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.nextPoll = l.auth.now().Add(l.interval)
}

// fail records a terminal failure. The detail is the error text, which the auth
// helpers keep free of token values. A code nobody approved may be one OpenAI
// refused, so that failure names the setting too.
func (l *Login) fail(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.status = LoginFailed
	l.detail = err.Error()
	if errors.Is(err, ErrDeviceCodeExpired) {
		l.detail += ": " + deviceCodeSetting + "; then " + loginHint + " again, see " + deviceCodeDocs
	}
	l.device = ""
	l.verifier = ""
}

// deviceResponse is the user-code answer of the device-auth endpoint.
type deviceResponse struct {
	DeviceCode              string      `json:"device_auth_id"`
	UserCode                string      `json:"user_code"`
	VerificationURI         string      `json:"verification_uri"`
	VerificationURIComplete string      `json:"verification_uri_complete"`
	ExpiresIn               int64       `json:"expires_in"`
	Interval                json.Number `json:"interval"`
	CodeVerifier            string      `json:"code_verifier"`
}

// verificationURL prefers what the server sent and falls back to the page the
// user types the code into; the page takes no code in its query.
func verificationURL(dr deviceResponse) string {
	switch {
	case dr.VerificationURIComplete != "":
		return dr.VerificationURIComplete
	case dr.VerificationURI != "":
		return dr.VerificationURI
	default:
		return DefaultVerificationURL
	}
}

// postDevice requests a user code from the JSON device endpoint.
func (l *Login) postDevice(ctx context.Context) (deviceResponse, error) {
	raw, err := l.postJSON(ctx, pathUserCode, map[string]string{"client_id": l.auth.clientID})
	if err != nil {
		return deviceResponse{}, err
	}
	var dr deviceResponse
	if err := json.Unmarshal(raw, &dr); err != nil {
		return deviceResponse{}, &provider.Error{
			Kind: provider.ErrServer, Provider: l.auth.name,
			Message: "device authorization response is not JSON",
		}
	}
	return dr, nil
}

// Device endpoints use JSON; OAuth token exchange stays form encoded.
func (l *Login) postJSON(ctx context.Context, path string, body map[string]string) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.auth.authBase+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return authRequest(l.auth.hc, req, l.auth.name, "application/json")
}
