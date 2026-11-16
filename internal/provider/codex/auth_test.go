package codex

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// Fifty callers arriving at an expired token must spend exactly one refresh
// token: refresh is single-use with reuse detection upstream (docs/models.md).
func TestRefreshIsStrictlySerialized(t *testing.T) {
	f := newFake(t)
	f.oauth = jsonHandler(http.StatusOK, fixture(t, "refresh_success.json"))
	sec := newSecrets(t)
	seedStale(t, sec, "codex")
	p := newProvider(t, f, sec, newClock(), event.Discard)

	const n = 50
	var wg sync.WaitGroup
	got := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got[i], errs[i] = p.auth.token(context.Background())
		}()
	}
	close(start)
	wg.Wait()

	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if got[i] != rotatedAccess {
			t.Fatalf("caller %d got a different token", i)
		}
	}
	if c := f.count(pathOAuthToken); c != 1 {
		t.Fatalf("POST %s happened %d times, want 1", pathOAuthToken, c)
	}
}

// The refresh request carries the fields the upstream flow expects.
func TestRefreshRequestShape(t *testing.T) {
	f := newFake(t)
	f.oauth = jsonHandler(http.StatusOK, fixture(t, "refresh_success.json"))
	sec := newSecrets(t)
	seedStale(t, sec, "codex")
	p := newProvider(t, f, sec, newClock(), event.Discard)

	if _, err := p.auth.token(context.Background()); err != nil {
		t.Fatalf("token: %v", err)
	}
	rec := f.last(pathOAuthToken)
	if rec.Method != http.MethodPost {
		t.Errorf("method = %s", rec.Method)
	}
	want := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     "test-client-id",
		"refresh_token": storedRefresh,
	}
	for k, v := range want {
		if rec.Form.Get(k) != v {
			t.Errorf("form %s = %q, want %q", k, rec.Form.Get(k), v)
		}
	}
	if ct := rec.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		t.Errorf("content-type = %q", ct)
	}
}

// The new refresh token reaches auth.toml before the new access token is used,
// and a write failure is surfaced without discarding the old state.
func TestRefreshPersistsBeforeUse(t *testing.T) {
	t.Run("persisted", func(t *testing.T) {
		f := newFake(t)
		f.oauth = jsonHandler(http.StatusOK, fixture(t, "refresh_success.json"))
		sec := newSecrets(t)
		seedStale(t, sec, "codex")
		p := newProvider(t, f, sec, newClock(), event.Discard)

		if _, err := p.auth.token(context.Background()); err != nil {
			t.Fatalf("token: %v", err)
		}
		if v, _ := sec.Get(secrets.Providers, "codex", keyRefreshToken); v != rotatedRefresh {
			t.Fatalf("stored refresh token = %q", v)
		}
		if v, _ := sec.Get(secrets.Providers, "codex", keyAccessToken); v != rotatedAccess {
			t.Fatalf("stored access token not rotated")
		}
		if v, _ := sec.Get(secrets.Providers, "codex", keyAccountID); v != loginAccount {
			t.Fatalf("stored account id = %q, want %q", v, loginAccount)
		}
	})

	t.Run("write failure keeps the old state", func(t *testing.T) {
		f := newFake(t)
		f.oauth = jsonHandler(http.StatusOK, fixture(t, "refresh_success.json"))
		sec := newSecrets(t)
		seedStale(t, sec, "codex")
		sink := &recorder{}
		p := newProvider(t, f, sec, newClock(), sink)
		fs := &failingStore{inner: sec, err: errors.New("disk full")}
		p.auth.store.sec = fs

		_, err := p.auth.token(context.Background())
		if err == nil {
			t.Fatal("want an error when the write fails")
		}
		if provider.KindOf(err) == provider.ErrAuth {
			t.Errorf("a failed write is not an auth failure: %v", err)
		}
		if fs.sets != 1 {
			t.Errorf("Set calls = %d, want 1", fs.sets)
		}
		if p.auth.cur.Access != staleAccess {
			t.Error("old token was discarded although the write failed")
		}
		if len(sink.all()) != 0 {
			t.Error("auth.refreshed emitted although nothing was persisted")
		}
	})
}

// A rotation is visible to another handle on the same file after a reload:
// the file, not the process, is the source of truth.
func TestRotationVisibleToSecondHandle(t *testing.T) {
	f := newFake(t)
	f.oauth = jsonHandler(http.StatusOK, fixture(t, "refresh_success.json"))
	sec := newSecrets(t)
	seedStale(t, sec, "codex")
	other, err := secrets.Open(sec.Path())
	if err != nil {
		t.Fatalf("second handle: %v", err)
	}
	p := newProvider(t, f, sec, newClock(), event.Discard)

	if _, err := p.auth.token(context.Background()); err != nil {
		t.Fatalf("token: %v", err)
	}
	if err := other.Reload(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if v, _ := other.Get(secrets.Providers, "codex", keyRefreshToken); v != rotatedRefresh {
		t.Fatalf("second handle sees %q, want the rotated token", v)
	}
}

// Expiry skew: a token due within five minutes is refreshed, one further out is
// not. The access token here carries no exp claim, so the stored expires_at is
// what counts — the documented fallback.
func TestExpirySkew(t *testing.T) {
	cases := []struct {
		name      string
		remaining time.Duration
		want      int
	}{
		{"inside the skew", 4 * time.Minute, 1},
		{"outside the skew", 6 * time.Minute, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.oauth = jsonHandler(http.StatusOK, fixture(t, "refresh_success.json"))
			sec := newSecrets(t)
			c := newClock()
			seed(t, sec, "codex", map[string]string{
				keyAccessToken:  noClaimsAccess,
				keyRefreshToken: storedRefresh,
				keyAccountID:    storedAccount,
				keyExpiresAt:    c.now().Add(tc.remaining).Format(time.RFC3339),
			})
			p := newProvider(t, f, sec, c, event.Discard)

			if _, err := p.auth.token(context.Background()); err != nil {
				t.Fatalf("token: %v", err)
			}
			if got := f.count(pathOAuthToken); got != tc.want {
				t.Fatalf("refreshes = %d, want %d", got, tc.want)
			}
			if tc.want == 0 {
				// The stored account id is the fallback when the JWT has no claim.
				if id := p.auth.accountID(); id != storedAccount {
					t.Fatalf("account id = %q, want the stored one", id)
				}
			}
		})
	}
}

// invalid_grant means the refresh token is spent or revoked: logged out, typed
// auth error with the login hint, and no automatic retry.
func TestLoggedOutAfterInvalidGrant(t *testing.T) {
	f := newFake(t)
	f.oauth = jsonHandler(http.StatusBadRequest, fixture(t, "refresh_invalid_grant.json"))
	sec := newSecrets(t)
	seedStale(t, sec, "codex")
	p := newProvider(t, f, sec, newClock(), event.Discard)

	for i := range 3 {
		_, err := p.auth.token(context.Background())
		if provider.KindOf(err) != provider.ErrAuth {
			t.Fatalf("call %d: kind = %q, want auth (%v)", i, provider.KindOf(err), err)
		}
		if !strings.Contains(err.Error(), "holaclaw login codex") {
			t.Fatalf("call %d: error has no login hint: %v", i, err)
		}
	}
	if c := f.count(pathOAuthToken); c != 1 {
		t.Fatalf("refresh attempts = %d, want 1: a logged-out provider must not retry", c)
	}
}

// A successful refresh emits auth.refreshed with the provider and the new
// expiry — and with no token anywhere in the event.
func TestAuthRefreshedEvent(t *testing.T) {
	f := newFake(t)
	f.oauth = jsonHandler(http.StatusOK, fixture(t, "refresh_success.json"))
	sec := newSecrets(t)
	seedStale(t, sec, "codex")
	sink := &recorder{}
	p := newProvider(t, f, sec, newClock(), sink)

	if _, err := p.auth.token(context.Background()); err != nil {
		t.Fatalf("token: %v", err)
	}
	evs := sink.all()
	if len(evs) != 1 || evs[0].Type != event.AuthRefreshed {
		t.Fatalf("events = %v, want one auth.refreshed", evs)
	}
	attrs := string(evs[0].Attrs)
	if !strings.Contains(attrs, `"provider":"codex"`) {
		t.Errorf("attrs have no provider: %s", attrs)
	}
	if !strings.Contains(attrs, `"expires_at":"2030-01-01T00:00:00Z"`) {
		t.Errorf("attrs have no expiry: %s", attrs)
	}
	if containsAny(attrs, secretValues()...) {
		t.Errorf("event attrs leak a token: %s", attrs)
	}
}

// Two 401s that arrive while one refresh is already in flight spend one refresh
// token between them: the second caller's 401 is answered by the first
// caller's new token.
func TestForceRefreshDeduplicates(t *testing.T) {
	f := newFake(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	f.oauth = func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		jsonHandler(http.StatusOK, fixture(t, "refresh_success.json"))(w, r)
	}
	sec := newSecrets(t)
	seedStale(t, sec, "codex")
	p := newProvider(t, f, sec, newClock(), event.Discard)

	first := make(chan error, 1)
	go func() { first <- p.auth.forceRefresh(context.Background()) }()
	<-entered // the first refresh is on the wire

	second := make(chan error, 1)
	go func() {
		// Reaches forceRefresh while the first one holds the mutex.
		second <- p.auth.forceRefresh(context.Background())
	}()
	time.Sleep(20 * time.Millisecond) // let the second caller queue on the mutex
	close(release)

	if err := <-first; err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("second: %v", err)
	}
	if c := f.count(pathOAuthToken); c != 1 {
		t.Fatalf("refreshes = %d, want 1", c)
	}
}

// A refresh failure never quotes a credential.
func TestRefreshErrorIsRedacted(t *testing.T) {
	f := newFake(t)
	f.oauth = jsonHandler(http.StatusInternalServerError,
		`{"error":"server_error","error_description":"token `+storedRefresh+` could not be checked"}`)
	sec := newSecrets(t)
	seedStale(t, sec, "codex")
	p := newProvider(t, f, sec, newClock(), event.Discard)

	_, err := p.auth.token(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if containsAny(err.Error(), secretValues()...) {
		t.Fatalf("error leaks a credential: %v", err)
	}
}
