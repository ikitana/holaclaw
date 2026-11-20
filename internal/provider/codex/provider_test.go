package codex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// chatRequest is the request every inference test sends.
func chatRequest() provider.Request {
	return provider.Request{
		Model:     provider.Model{Name: "codex", Provider: "codex", ID: "gpt-6.1-sol", Tools: true},
		System:    "You are holaclaw.",
		Messages:  []provider.Message{provider.TextMessage(provider.RoleUser, "hello")},
		MaxTokens: 512,
		SessionID: "sess-abc",
	}
}

// A provider that was never logged in fails before any request goes out.
func TestChatWithoutLogin(t *testing.T) {
	f := newFake(t)
	sec := newSecrets(t)
	p := newProvider(t, f, sec, newClock(), event.Discard)

	_, err := p.Chat(context.Background(), chatRequest())
	if provider.KindOf(err) != provider.ErrAuth {
		t.Fatalf("kind = %q, want auth (%v)", provider.KindOf(err), err)
	}
	if !strings.Contains(err.Error(), "holaclaw login codex") {
		t.Fatalf("error has no login hint: %v", err)
	}
	if n := f.count(inferencePath) + f.count(pathOAuthToken); n != 0 {
		t.Fatalf("%d requests were made, want none", n)
	}
}

// usage_limit_reached arrives already typed from the transport: it is a quota
// event with a reset time, never a refresh and never a re-login (docs/models.md).
func TestUsageLimitDoesNotRefresh(t *testing.T) {
	f := newFake(t)
	f.inference = jsonHandler(http.StatusTooManyRequests, fixture(t, "usage_limit.json"))
	sec := newSecrets(t)
	seedFresh(t, sec, "codex")
	p := newProvider(t, f, sec, newClock(), event.Discard)

	_, err := p.Chat(context.Background(), chatRequest())
	if provider.KindOf(err) != provider.ErrUsageLimit {
		t.Fatalf("kind = %q, want usage_limit (%v)", provider.KindOf(err), err)
	}
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("not a provider error: %v", err)
	}
	if want := time.Unix(1893456000, 0).UTC(); !pe.ResetAt.Equal(want) {
		t.Errorf("ResetAt = %v, want %v", pe.ResetAt, want)
	}
	if c := f.count(pathOAuthToken); c != 0 {
		t.Fatalf("refreshes = %d: a usage limit must not touch the tokens", c)
	}
	if c := f.count(inferencePath); c != 1 {
		t.Fatalf("inference attempts = %d, want 1", c)
	}
	// Still logged in.
	if _, err := p.auth.token(context.Background()); err != nil {
		t.Fatalf("token after a usage limit: %v", err)
	}
}

// A 401 forces one refresh even though the token is not near expiry, and the
// request is retried once.
func TestUnauthorizedForcesOneRefresh(t *testing.T) {
	f := newFake(t)
	f.oauth = jsonHandler(http.StatusOK, fixture(t, "refresh_success.json"))
	f.inference = sequence(
		jsonHandler(http.StatusUnauthorized, fixture(t, "unauthorized.json")),
		sseHandler(fixture(t, "inference_tool_call.sse")),
	)
	sec := newSecrets(t)
	seedFresh(t, sec, "codex")
	p := newProvider(t, f, sec, newClock(), event.Discard)

	stream, err := p.Chat(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	resp, err := provider.Collect(stream, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.ToolCalls))
	}
	if c := f.count(pathOAuthToken); c != 1 {
		t.Fatalf("refreshes = %d, want exactly 1", c)
	}
	if c := f.count(inferencePath); c != 2 {
		t.Fatalf("inference attempts = %d, want 2", c)
	}
	// The retry carried the rotated token.
	recs := f.records(inferencePath)
	if got := recs[1].Header.Get("Authorization"); got != "Bearer "+rotatedAccess {
		t.Fatalf("retry authorization was not refreshed")
	}
}

// Two 401s in a row are an auth failure, not a refresh loop.
func TestUnauthorizedTwiceIsAuthError(t *testing.T) {
	f := newFake(t)
	f.oauth = jsonHandler(http.StatusOK, fixture(t, "refresh_success.json"))
	f.inference = jsonHandler(http.StatusUnauthorized, fixture(t, "unauthorized.json"))
	sec := newSecrets(t)
	seedFresh(t, sec, "codex")
	p := newProvider(t, f, sec, newClock(), event.Discard)

	_, err := p.Chat(context.Background(), chatRequest())
	if provider.KindOf(err) != provider.ErrAuth {
		t.Fatalf("kind = %q, want auth (%v)", provider.KindOf(err), err)
	}
	if c := f.count(inferencePath); c != 2 {
		t.Fatalf("inference attempts = %d, want 2", c)
	}
	if c := f.count(pathOAuthToken); c != 1 {
		t.Fatalf("refreshes = %d, want 1", c)
	}
}

func TestModels(t *testing.T) {
	t.Run("live", func(t *testing.T) {
		f := newFake(t)
		f.models = jsonHandler(http.StatusOK, fixture(t, "models.json"))
		sec := newSecrets(t)
		seedFresh(t, sec, "codex")
		p := newProvider(t, f, sec, newClock(), event.Discard)

		models, source, err := p.ModelsWithSource(context.Background())
		if err != nil {
			t.Fatalf("models: %v", err)
		}
		if source != SourceLive {
			t.Errorf("source = %q, want live", source)
		}
		if len(models) != 2 || models[0] != "gpt-6.1-sol" {
			t.Errorf("models = %v", models)
		}
		rec := f.last("/backend-api/codex/models")
		if rec.Header.Get("originator") != Originator {
			t.Errorf("originator header missing")
		}
		if rec.Header.Get("chatgpt-account-id") != storedAccount {
			t.Errorf("account header = %q", rec.Header.Get("chatgpt-account-id"))
		}
	})

	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"server error", jsonHandler(http.StatusInternalServerError, `{"error":"nope"}`)},
		{"empty list", jsonHandler(http.StatusOK, `{"data":[]}`)},
		{"garbage", jsonHandler(http.StatusOK, `not json at all`)},
	}
	for _, tc := range cases {
		t.Run("pinned on "+tc.name, func(t *testing.T) {
			f := newFake(t)
			f.models = tc.handler
			sec := newSecrets(t)
			seedFresh(t, sec, "codex")
			p := newProvider(t, f, sec, newClock(), event.Discard)

			models, source, err := p.ModelsWithSource(context.Background())
			if err != nil {
				t.Fatalf("models: %v", err)
			}
			if source != SourcePinned {
				t.Errorf("source = %q, want pinned", source)
			}
			if len(models) != len(PinnedModels) || models[0] != PinnedModels[0] {
				t.Errorf("models = %v, want the pinned list", models)
			}
		})
	}

	t.Run("pinned when not logged in", func(t *testing.T) {
		f := newFake(t)
		sec := newSecrets(t)
		p := newProvider(t, f, sec, newClock(), event.Discard)
		models, source, err := p.ModelsWithSource(context.Background())
		if err != nil || source != SourcePinned || len(models) == 0 {
			t.Fatalf("models = %v, source = %q, err = %v", models, source, err)
		}
	})
}

// `holaclaw check` lists the models without owning the tokens: nothing is
// refreshed and auth.toml is left as it was.
func TestStoredModels(t *testing.T) {
	t.Run("live", func(t *testing.T) {
		f := newFake(t)
		f.models = jsonHandler(http.StatusOK, fixture(t, "models.json"))
		sec := newSecrets(t)
		seedFresh(t, sec, "codex")
		p := newProvider(t, f, sec, newClock(), event.Discard)

		models, source, err := p.StoredModels(context.Background())
		if err != nil || source != SourceLive || len(models) != 2 {
			t.Fatalf("models = %v, source = %q, err = %v", models, source, err)
		}
		rec := f.last("/backend-api/codex/models")
		if rec.Header.Get("Authorization") != "Bearer "+noClaimsAccess {
			t.Errorf("the stored access token was not sent")
		}
		if rec.Header.Get("chatgpt-account-id") != storedAccount {
			t.Errorf("account header = %q", rec.Header.Get("chatgpt-account-id"))
		}
	})

	t.Run("an expired token is not refreshed", func(t *testing.T) {
		f := newFake(t) // f.oauth is unset: a refresh fails the test
		f.models = jsonHandler(http.StatusUnauthorized, fixture(t, "unauthorized.json"))
		sec := newSecrets(t)
		seedStale(t, sec, "codex")
		p := newProvider(t, f, sec, newClock(), event.Discard)

		models, source, err := p.StoredModels(context.Background())
		if err != nil || source != SourcePinned || len(models) != len(PinnedModels) {
			t.Fatalf("models = %v, source = %q, err = %v", models, source, err)
		}
		if c := f.count(pathOAuthToken); c != 0 {
			t.Errorf("refreshes = %d, want 0", c)
		}
		if got, _ := sec.Get(secrets.Providers, "codex", keyRefreshToken); got != storedRefresh {
			t.Errorf("the stored refresh token changed")
		}
	})

	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"server error", jsonHandler(http.StatusInternalServerError, `{"error":"nope"}`)},
		{"empty list", jsonHandler(http.StatusOK, `{"data":[]}`)},
		{"garbage", jsonHandler(http.StatusOK, `not json at all`)},
	} {
		t.Run("pinned on "+tc.name, func(t *testing.T) {
			f := newFake(t)
			f.models = tc.handler
			sec := newSecrets(t)
			seedFresh(t, sec, "codex")
			p := newProvider(t, f, sec, newClock(), event.Discard)

			models, source, err := p.StoredModels(context.Background())
			if err != nil || source != SourcePinned || len(models) != len(PinnedModels) {
				t.Fatalf("models = %v, source = %q, err = %v", models, source, err)
			}
		})
	}

	t.Run("not logged in", func(t *testing.T) {
		f := newFake(t)
		p := newProvider(t, f, newSecrets(t), newClock(), event.Discard)
		models, _, err := p.StoredModels(context.Background())
		if provider.KindOf(err) != provider.ErrAuth || models != nil {
			t.Fatalf("models = %v, err = %v, want an auth error", models, err)
		}
	})
}

// A request without a session id gets the provider's own stable one.
func TestSessionIDFallback(t *testing.T) {
	f := newFake(t)
	f.inference = sseHandler(fixture(t, "inference_tool_call.sse"))
	sec := newSecrets(t)
	seedFresh(t, sec, "codex")
	p := newProvider(t, f, sec, newClock(), event.Discard)

	req := chatRequest()
	req.SessionID = ""
	for range 2 {
		stream, err := p.Chat(context.Background(), req)
		if err != nil {
			t.Fatalf("chat: %v", err)
		}
		if _, err := provider.Collect(stream, nil); err != nil {
			t.Fatalf("collect: %v", err)
		}
	}
	recs := f.records(inferencePath)
	first, second := recs[0].Header.Get("session_id"), recs[1].Header.Get("session_id")
	if first == "" || first != second {
		t.Fatalf("session ids = %q, %q; want one stable non-empty id", first, second)
	}
	if first == "sess-abc" {
		t.Fatal("the fallback id must not be the request id")
	}
}

// api-key mode is the plain Responses transport with the stored key.
func TestAPIKeyMode(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		sseHandler(fixture(t, "inference_tool_call.sse"))(w, r)
	}))
	defer srv.Close()

	sec := newSecrets(t)
	seed(t, sec, "codex-key", map[string]string{keyAPIKey: "FAKEapiKEYsk0001"})
	p := NewAPIKey("codex-key", sec, Options{InferenceURL: srv.URL + "/v1", HTTPClient: srv.Client()})

	stream, err := p.Chat(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if _, err := provider.Collect(stream, nil); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if gotAuth != "Bearer FAKEapiKEYsk0001" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotPath != "/v1/responses" {
		t.Errorf("path = %q", gotPath)
	}
	if p.Name() != "codex-key" {
		t.Errorf("name = %q", p.Name())
	}

	// A rotated key is picked up without rebuilding the provider.
	seed(t, sec, "codex-key", map[string]string{keyAPIKey: "FAKEapiKEYsk0002"})
	stream, err = p.Chat(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if _, err := provider.Collect(stream, nil); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if gotAuth != "Bearer FAKEapiKEYsk0002" {
		t.Errorf("rotated authorization = %q", gotAuth)
	}
}

// No error text anywhere may carry a credential.
func TestErrorsAreRedacted(t *testing.T) {
	f := newFake(t)
	f.inference = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad token `+noClaimsAccess+`"}}`)
	}
	sec := newSecrets(t)
	seedFresh(t, sec, "codex")
	p := newProvider(t, f, sec, newClock(), event.Discard)

	_, err := p.Chat(context.Background(), chatRequest())
	if err == nil {
		t.Fatal("want an error")
	}
	if containsAny(err.Error(), secretValues()...) {
		t.Fatalf("error leaks a credential: %v", err)
	}
}

// The secrets file is the only place the tokens live.
func TestTokensOnlyInSecretsFile(t *testing.T) {
	f := newFake(t)
	f.oauth = jsonHandler(http.StatusOK, fixture(t, "refresh_success.json"))
	sec := newSecrets(t)
	seedStale(t, sec, "codex")
	p := newProvider(t, f, sec, newClock(), event.Discard)
	if _, err := p.auth.token(context.Background()); err != nil {
		t.Fatalf("token: %v", err)
	}
	keys := sec.Keys(secrets.Providers, "codex")
	want := map[string]bool{
		keyAccessToken: true, keyRefreshToken: true, keyAccountID: true, keyExpiresAt: true,
	}
	for _, k := range keys {
		if !want[k] && k != keyIDToken {
			t.Errorf("unexpected key %q in auth.toml", k)
		}
	}
}
