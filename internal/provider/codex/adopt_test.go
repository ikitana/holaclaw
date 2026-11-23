package codex

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// What a client that signed in elsewhere hands over: another account, valid
// for an hour after clockAt.
const (
	putRefresh = "FAKErefreshTOKENput3"
	putAccount = "acct-put-7"
)

var putAccess = jwtHeader + "." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil,
	`{"exp":%d,"https://api.openai.com/auth":{"chatgpt_account_id":%q}}`,
	clockAt.Add(time.Hour).Unix(), putAccount)) + ".FAKEsigPUT"

// putTokens is the write a credentials PUT hands to Adopt.
func putTokens(sec secretStore) func() error {
	return func() error {
		return sec.Set(secrets.Providers, "codex", map[string]string{
			keyAccessToken:  putAccess,
			keyRefreshToken: putRefresh,
		})
	}
}

// chat runs one request to the end and returns what the upstream received.
func chat(t *testing.T, f *fake, p *Provider) record {
	t.Helper()
	stream, err := p.Chat(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if _, err := provider.Collect(stream, nil); err != nil {
		t.Fatalf("collect: %v", err)
	}
	return f.last(inferencePath)
}

// wantPut checks that a request carried the adopted tokens.
func wantPut(t *testing.T, rec record) {
	t.Helper()
	if got := rec.Header.Get("Authorization"); got != "Bearer "+putAccess {
		t.Errorf("the request does not carry the adopted access token")
	}
	if got := rec.Header.Get("chatgpt-account-id"); got != putAccount {
		t.Errorf("account id = %q, want %q", got, putAccount)
	}
}

// Tokens stored while the provider already serves others from memory are used
// from the next request, and it is their refresh token the next refresh spends.
func TestAdoptReplacesLoadedTokens(t *testing.T) {
	f := newFake(t)
	f.inference = sseHandler(fixture(t, "inference_tool_call.sse"))
	f.oauth = jsonHandler(http.StatusOK, fixture(t, "refresh_success.json"))
	sec := newSecrets(t)
	seedFresh(t, sec, "codex")
	c := newClock()
	p := newProvider(t, f, sec, c, event.Discard)

	if got := chat(t, f, p).Header.Get("Authorization"); got != "Bearer "+noClaimsAccess {
		t.Fatalf("the first request does not carry the stored token")
	}
	if err := p.Adopt(putTokens(sec)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	wantPut(t, chat(t, f, p))
	if n := f.count(pathOAuthToken); n != 0 {
		t.Fatalf("refreshes = %d, want 0", n)
	}

	c.advance(2 * time.Hour)
	if _, err := p.auth.token(context.Background()); err != nil {
		t.Fatalf("token: %v", err)
	}
	if got := f.last(pathOAuthToken).Form.Get("refresh_token"); got != putRefresh {
		t.Fatalf("the refresh spent %q, want the adopted refresh token", got)
	}
}

// A provider whose refresh token was rejected comes back without a restart.
func TestAdoptBringsBackALoggedOutProvider(t *testing.T) {
	f := newFake(t)
	f.oauth = jsonHandler(http.StatusBadRequest, fixture(t, "refresh_invalid_grant.json"))
	f.inference = sseHandler(fixture(t, "inference_tool_call.sse"))
	sec := newSecrets(t)
	seedStale(t, sec, "codex")
	p := newProvider(t, f, sec, newClock(), event.Discard)

	if _, err := p.Chat(context.Background(), chatRequest()); provider.KindOf(err) != provider.ErrAuth {
		t.Fatalf("kind = %q, want auth (%v)", provider.KindOf(err), err)
	}
	if err := p.Adopt(putTokens(sec)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	wantPut(t, chat(t, f, p))
	if n := f.count(pathOAuthToken); n != 1 {
		t.Fatalf("refresh attempts = %d, want 1", n)
	}
}

// A provider that never loaded tokens uses the stored ones as well.
func TestAdoptBeforeAnyTokensWereLoaded(t *testing.T) {
	f := newFake(t)
	f.inference = sseHandler(fixture(t, "inference_tool_call.sse"))
	sec := newSecrets(t)
	p := newProvider(t, f, sec, newClock(), event.Discard)

	if err := p.Adopt(putTokens(sec)); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	wantPut(t, chat(t, f, p))
}

// A write that names only the tokens, with an access token that carries no
// account claim, does not send the new token under the account id stored for
// the token it replaced. An account id the write names itself is used.
func TestAdoptDropsTheAccountOfTheReplacedToken(t *testing.T) {
	const otherAccess = jwtHeader + "." + noClaimsPayload + ".FAKEsigOTHER"
	put := func(sec secretStore, kv map[string]string) func() error {
		return func() error { return sec.Set(secrets.Providers, "codex", kv) }
	}
	cases := []struct {
		name string
		kv   map[string]string
		want string
	}{
		{"tokens alone", map[string]string{keyAccessToken: otherAccess, keyRefreshToken: putRefresh}, ""},
		{"with an account id", map[string]string{keyAccessToken: otherAccess, keyRefreshToken: putRefresh, keyAccountID: putAccount}, putAccount},
		// The same access token is the same account: nothing to drop.
		{"another field only", map[string]string{keyRefreshToken: putRefresh}, storedAccount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.inference = sseHandler(fixture(t, "inference_tool_call.sse"))
			sec := newSecrets(t)
			seedFresh(t, sec, "codex")
			p := newProvider(t, f, sec, newClock(), event.Discard)
			if got := chat(t, f, p).Header.Get("chatgpt-account-id"); got != storedAccount {
				t.Fatalf("account id = %q, want the stored one", got)
			}

			if err := p.Adopt(put(sec, tc.kv)); err != nil {
				t.Fatalf("adopt: %v", err)
			}
			if got := chat(t, f, p).Header.Get("chatgpt-account-id"); got != tc.want {
				t.Fatalf("account id = %q, want %q", got, tc.want)
			}
		})
	}
}

// A write that arrives during a refresh waits for it, so the tokens that
// refresh rotated cannot land on top of the new ones, in memory or on disk.
func TestAdoptWaitsForARefreshInFlight(t *testing.T) {
	f := newFake(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f.oauth = func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		jsonHandler(http.StatusOK, fixture(t, "refresh_success.json"))(w, r)
	}
	sec := newSecrets(t)
	seedStale(t, sec, "codex")
	p := newProvider(t, f, sec, newClock(), event.Discard)

	refreshed := make(chan error, 1)
	go func() {
		_, err := p.auth.token(context.Background())
		refreshed <- err
	}()
	<-entered

	wrote := make(chan struct{})
	adopted := make(chan error, 1)
	go func() {
		adopted <- p.Adopt(func() error {
			defer close(wrote)
			return putTokens(sec)()
		})
	}()
	select {
	case <-wrote:
		t.Fatal("the write ran while the refresh was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-refreshed; err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if err := <-adopted; err != nil {
		t.Fatalf("adopt: %v", err)
	}

	if p.auth.accessToken() != putAccess || p.auth.accountID() != putAccount {
		t.Error("the refreshed tokens are current, want the adopted ones")
	}
	if v, _ := sec.Get(secrets.Providers, "codex", keyAccessToken); v != putAccess {
		t.Error("auth.toml does not hold the adopted access token")
	}
	if v, _ := sec.Get(secrets.Providers, "codex", keyRefreshToken); v != putRefresh {
		t.Errorf("stored refresh token = %q, want the adopted one", v)
	}
}

// A write that fails changes nothing: the tokens in use stay in use, and a
// logged-out provider stays logged out.
func TestAdoptWriteFailureKeepsTheCurrentTokens(t *testing.T) {
	boom := errors.New("disk full")

	t.Run("loaded", func(t *testing.T) {
		f := newFake(t)
		f.inference = sseHandler(fixture(t, "inference_tool_call.sse"))
		sec := newSecrets(t)
		seedFresh(t, sec, "codex")
		p := newProvider(t, f, sec, newClock(), event.Discard)
		chat(t, f, p)

		fs := &failingStore{inner: sec, err: boom}
		if err := p.Adopt(putTokens(fs)); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the write error", err)
		}
		rec := chat(t, f, p)
		if got := rec.Header.Get("Authorization"); got != "Bearer "+noClaimsAccess {
			t.Error("the request does not carry the tokens that were in use")
		}
		if got := rec.Header.Get("chatgpt-account-id"); got != storedAccount {
			t.Errorf("account id = %q, want %q", got, storedAccount)
		}
	})

	t.Run("logged out", func(t *testing.T) {
		f := newFake(t)
		f.oauth = jsonHandler(http.StatusBadRequest, fixture(t, "refresh_invalid_grant.json"))
		sec := newSecrets(t)
		seedStale(t, sec, "codex")
		p := newProvider(t, f, sec, newClock(), event.Discard)
		_, _ = p.auth.token(context.Background())

		fs := &failingStore{inner: sec, err: boom}
		if err := p.Adopt(putTokens(fs)); !errors.Is(err, boom) {
			t.Fatalf("err = %v, want the write error", err)
		}
		if _, err := p.auth.token(context.Background()); provider.KindOf(err) != provider.ErrAuth {
			t.Fatalf("kind = %q, want auth (%v)", provider.KindOf(err), err)
		}
	})
}
