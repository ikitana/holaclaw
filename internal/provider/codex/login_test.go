package codex

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/event"
)

// loginProvider wires a provider with no stored credentials: the state a fresh
// install is in.
func loginProvider(t *testing.T, f *fake, c *clock) (*Provider, *secrets.File) {
	t.Helper()
	sec := newSecrets(t)
	return newProvider(t, f, sec, c, event.Discard), sec
}

// The happy path: user code, a pending poll, a success poll, the exchange, and
// the tokens on disk.
func TestLoginRun(t *testing.T) {
	f := newFake(t)
	f.usercode = jsonHandler(http.StatusOK, fixture(t, "usercode.json"))
	f.device = sequence(
		jsonHandler(http.StatusOK, fixture(t, "poll_pending.json")),
		jsonHandler(http.StatusOK, fixture(t, "poll_success.json")),
	)
	f.oauth = jsonHandler(http.StatusOK, fixture(t, "token_exchange.json"))
	p, sec := loginProvider(t, f, newClock())

	var shown DeviceCode
	if err := p.Login().Run(context.Background(), func(d DeviceCode) { shown = d }); err != nil {
		t.Fatalf("run: %v", err)
	}
	if shown.UserCode != "WXYZ-1234" {
		t.Errorf("user code = %q", shown.UserCode)
	}
	if shown.VerificationURL == "" || shown.Interval != 5*time.Second {
		t.Errorf("device code = %+v", shown)
	}
	if st := p.Login().State(); st.Status != LoginDone {
		t.Errorf("status = %q, want done", st.Status)
	}
	if v, _ := sec.Get(secrets.Providers, "codex", keyRefreshToken); v != "FAKErefreshTOKENlogin1" {
		t.Errorf("refresh token not persisted: %q", v)
	}
	if v, _ := sec.Get(secrets.Providers, "codex", keyAccountID); v != loginAccount {
		t.Errorf("account id = %q, want %q", v, loginAccount)
	}
	if v, _ := sec.Get(secrets.Providers, "codex", keyIDToken); v == "" {
		t.Error("id_token not persisted")
	}
	// The exchange sends the PKCE verifier the device response supplied.
	rec := f.last(pathOAuthToken)
	if got := rec.Form.Get("grant_type"); got != "authorization_code" {
		t.Errorf("grant_type = %q", got)
	}
	if got := rec.Form.Get("code"); got != "FAKEauthzCODE0001" {
		t.Errorf("code = %q", got)
	}
	if got := rec.Form.Get("code_verifier"); got != "FAKEcodeVERIFIER0001" {
		t.Errorf("code_verifier = %q", got)
	}
	if got := rec.Form.Get("redirect_uri"); got != DefaultRedirectURI {
		t.Errorf("redirect_uri = %q", got)
	}
}

// A second Start while one login is pending returns the same code and makes no
// request: one login at a time per provider.
func TestLoginDoubleStart(t *testing.T) {
	f := newFake(t)
	f.usercode = jsonHandler(http.StatusOK, fixture(t, "usercode.json"))
	p, _ := loginProvider(t, f, newClock())

	first, err := p.Login().Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	second, err := p.Login().Start(context.Background())
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if first != second {
		t.Errorf("second Start returned a different code: %+v vs %+v", first, second)
	}
	if c := f.count(pathUserCode); c != 1 {
		t.Errorf("user-code requests = %d, want 1", c)
	}
	if st := p.Login().State(); st.Status != LoginPending {
		t.Errorf("status = %q, want pending", st.Status)
	}
}

// slow_down widens the poll interval for every later poll.
func TestLoginSlowDown(t *testing.T) {
	f := newFake(t)
	f.usercode = jsonHandler(http.StatusOK, fixture(t, "usercode.json"))
	f.device = jsonHandler(http.StatusOK, fixture(t, "poll_slow_down.json"))
	p, _ := loginProvider(t, f, newClock())
	sr := &sleepRecorder{}
	p.login.sleep = sr.sleep

	if _, err := p.Login().Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	for i := range 2 {
		done, err := p.Login().Poll(context.Background())
		if done || !errors.Is(err, ErrSlowDown) {
			t.Fatalf("poll %d: done=%v err=%v, want slow down", i, done, err)
		}
	}
	waits := sr.all()
	if len(waits) != 2 || waits[0] != 5*time.Second || waits[1] != 10*time.Second {
		t.Fatalf("waits = %v, want [5s 10s]", waits)
	}
	// Two slow_down answers: 5s → 10s → 15s.
	if st := p.Login().State(); st.Status != LoginPending || st.DeviceCode.Interval != 15*time.Second {
		t.Fatalf("state = %+v, want pending with a widened interval", st)
	}
}

// Terminal poll answers move the state machine to failed with a typed error.
func TestLoginTerminalAnswers(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		want    error
	}{
		{"expired", "poll_expired.json", ErrDeviceCodeExpired},
		{"denied", "poll_denied.json", ErrAccessDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.usercode = jsonHandler(http.StatusOK, fixture(t, "usercode.json"))
			f.device = jsonHandler(http.StatusOK, fixture(t, tc.fixture))
			p, _ := loginProvider(t, f, newClock())

			if _, err := p.Login().Start(context.Background()); err != nil {
				t.Fatalf("start: %v", err)
			}
			// A pending login says what the account needs before the user
			// opens the page.
			if st := p.Login().State(); st.Status != LoginPending || st.Detail != deviceCodeSetting {
				t.Fatalf("state = %+v, want pending with the setting", st)
			}
			done, err := p.Login().Poll(context.Background())
			if done || !errors.Is(err, tc.want) {
				t.Fatalf("poll: done=%v err=%v, want %v", done, err, tc.want)
			}
			st := p.Login().State()
			if st.Status != LoginFailed || !strings.HasPrefix(st.Detail, tc.want.Error()) {
				t.Fatalf("state = %+v, want failed with the cause", st)
			}
			// Only a code nobody approved may be one OpenAI's page refused.
			hint := ": " + deviceCodeSetting + "; then run `holaclaw login codex` again"
			if got := strings.HasSuffix(st.Detail, hint); got != (tc.want == ErrDeviceCodeExpired) {
				t.Fatalf("detail = %q, setting hint = %v", st.Detail, got)
			}
			if containsAny(st.Detail, secretValues()...) {
				t.Fatalf("state detail leaks a credential: %q", st.Detail)
			}
			// A failed login must not keep polling.
			if _, err := p.Login().Poll(context.Background()); !errors.Is(err, ErrNoLogin) {
				t.Fatalf("poll after failure = %v, want ErrNoLogin", err)
			}
		})
	}
}

// The device code expires on the clock, without asking upstream again.
func TestLoginDeviceCodeExpires(t *testing.T) {
	f := newFake(t)
	f.usercode = jsonHandler(http.StatusOK, fixture(t, "usercode.json"))
	f.device = jsonHandler(http.StatusOK, fixture(t, "poll_pending.json"))
	c := newClock()
	p, _ := loginProvider(t, f, c)

	if _, err := p.Login().Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	c.advance(20 * time.Minute) // the fixture lives 900s
	done, err := p.Login().Poll(context.Background())
	if done || !errors.Is(err, ErrDeviceCodeExpired) {
		t.Fatalf("poll: done=%v err=%v, want expired", done, err)
	}
	if c := f.count(pathDeviceToken); c != 0 {
		t.Fatalf("polled upstream %d times after expiry, want 0", c)
	}
	if st := p.Login().State(); st.Status != LoginFailed || !strings.Contains(st.Detail, deviceCodeSetting) {
		t.Fatalf("state = %+v, want failed with the setting", st)
	}
}

// Run honours context cancellation between polls.
func TestLoginContextCancel(t *testing.T) {
	f := newFake(t)
	f.usercode = jsonHandler(http.StatusOK, fixture(t, "usercode.json"))
	f.device = jsonHandler(http.StatusOK, fixture(t, "poll_pending.json"))
	p, _ := loginProvider(t, f, newClock())

	ctx, cancel := context.WithCancel(context.Background())
	polls := 0
	p.login.sleep = func(ctx context.Context, _ time.Duration) error {
		polls++
		if polls == 2 {
			cancel()
		}
		return ctx.Err()
	}
	err := p.Login().Run(ctx, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run = %v, want context.Canceled", err)
	}
	if st := p.Login().State(); st.Status != LoginPending {
		t.Fatalf("status = %q: a cancelled run leaves the login pending", st.Status)
	}
	cancel()
}

// Poll before Start is not a crash.
func TestLoginPollWithoutStart(t *testing.T) {
	f := newFake(t)
	p, _ := loginProvider(t, f, newClock())
	if st := p.Login().State(); st.Status != LoginNone {
		t.Fatalf("status = %q, want none", st.Status)
	}
	if _, err := p.Login().Poll(context.Background()); !errors.Is(err, ErrNoLogin) {
		t.Fatalf("poll = %v, want ErrNoLogin", err)
	}
}

// After a login the provider is usable without a refresh: the fresh tokens are
// already current.
func TestLoginMakesProviderUsable(t *testing.T) {
	f := newFake(t)
	f.usercode = jsonHandler(http.StatusOK, fixture(t, "usercode.json"))
	f.device = jsonHandler(http.StatusOK, fixture(t, "poll_success.json"))
	f.oauth = jsonHandler(http.StatusOK, fixture(t, "token_exchange.json"))
	p, _ := loginProvider(t, f, newClock())

	if err := p.Login().Run(context.Background(), nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := p.auth.token(context.Background()); err != nil {
		t.Fatalf("token: %v", err)
	}
	if c := f.count(pathOAuthToken); c != 1 {
		t.Fatalf("oauth calls = %d, want just the exchange", c)
	}
	if id := p.auth.accountID(); id != loginAccount {
		t.Fatalf("account id = %q", id)
	}
}

// A failing token exchange quotes back what it was given. Whatever ends up in
// LoginState.Detail goes through the same redaction as a failed poll: the API
// and the CLI read that field (ARCHITECTURE.md §4 — secrets never leave).
func TestLoginExchangeFailureIsRedacted(t *testing.T) {
	const device = "FAKEdeviceCODE0001" // the device_code of usercode.json
	f := newFake(t)
	f.usercode = jsonHandler(http.StatusOK, fixture(t, "usercode.json"))
	f.device = jsonHandler(http.StatusOK, fixture(t, "poll_success.json"))
	f.oauth = jsonHandler(http.StatusBadRequest,
		`{"error":"invalid_grant","error_description":"device_code `+device+` was already exchanged"}`)
	p, _ := loginProvider(t, f, newClock())

	err := p.Login().Run(context.Background(), nil)
	if err == nil {
		t.Fatal("the exchange failed but Run succeeded")
	}
	if strings.Contains(err.Error(), device) {
		t.Errorf("the device code leaked into the returned error: %q", err)
	}
	st := p.Login().State()
	if st.Status != LoginFailed {
		t.Fatalf("status = %q, want failed", st.Status)
	}
	if st.Detail == "" || !strings.Contains(st.Detail, "invalid_grant") {
		t.Errorf("detail = %q, want it to still name the cause", st.Detail)
	}
	if strings.Contains(st.Detail, device) {
		t.Fatalf("the device code leaked into LoginState.Detail: %q", st.Detail)
	}
}

// Without a URL in the device response, the user is sent to the page as is:
// it takes no code in its query.
func TestVerificationURLFallsBackToTheBarePage(t *testing.T) {
	if got := verificationURL(deviceResponse{UserCode: "WXYZ-1234"}); got != DefaultVerificationURL {
		t.Errorf("verification url = %q, want %q", got, DefaultVerificationURL)
	}
}
