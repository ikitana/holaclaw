package responses

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/provider"
)

const (
	fakeKey     = "sk-fake-abcdef123456"
	fakeAccount = "acct-fake-987654321"
)

// jsonHandler replies with a status and a JSON body.
func jsonHandler(status int, body string, hdr map[string]string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range hdr {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func providerError(t *testing.T, err error) *provider.Error {
	t.Helper()
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("not a provider error: %v", err)
	}
	return pe
}

// --- HTTP status mapping ---------------------------------------------------

func TestHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		hdr    map[string]string
		want   provider.ErrorKind
		retry  time.Duration
	}{
		{"unauthorized", 401, `{"error":{"message":"invalid key"}}`, nil, provider.ErrAuth, 0},
		{"forbidden", 403, `{"error":{"message":"no access"}}`, nil, provider.ErrAuth, 0},
		{"rate limit", 429, `{"error":{"code":"rate_limit_exceeded","message":"slow down"}}`,
			map[string]string{"Retry-After": "7"}, provider.ErrRateLimit, 7 * time.Second},
		{"context", 400, `{"error":{"code":"context_length_exceeded","message":"too long"}}`, nil, provider.ErrContext, 0},
		{"context by wording", 400, `{"error":{"message":"input is too long for this model"}}`, nil, provider.ErrContext, 0},
		{"bad request", 400, `{"error":{"message":"unknown field"}}`, nil, provider.ErrBadRequest, 0},
		{"server", 503, `{"error":{"message":"upstream down"}}`, nil, provider.ErrServer, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, jsonHandler(tc.status, tc.body, tc.hdr), Options{})
			_, _, err := chatOnce(t, c, testRequest())
			pe := providerError(t, err)
			if pe.Kind != tc.want {
				t.Errorf("kind = %q, want %q", pe.Kind, tc.want)
			}
			if pe.Status != tc.status {
				t.Errorf("status = %d", pe.Status)
			}
			if pe.RetryAfter != tc.retry {
				t.Errorf("retry-after = %v, want %v", pe.RetryAfter, tc.retry)
			}
		})
	}
}

func TestUsageLimitFrom429Body(t *testing.T) {
	reset := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	body := `{"error":{"code":"usage_limit_reached","message":"weekly limit","resets_at":` +
		strconv.FormatInt(reset.Unix(), 10) + `}}`
	c := newTestClient(t, jsonHandler(http.StatusTooManyRequests, body, nil), Options{})
	_, _, err := chatOnce(t, c, testRequest())
	pe := providerError(t, err)
	if pe.Kind != provider.ErrUsageLimit {
		t.Fatalf("kind = %q, want usage_limit", pe.Kind)
	}
	if !pe.ResetAt.Equal(reset.UTC()) {
		t.Errorf("reset = %v, want %v", pe.ResetAt, reset.UTC())
	}
}

// --- in-stream errors ------------------------------------------------------

func TestStreamUsageLimitEventWithResetAt(t *testing.T) {
	c := newTestClient(t, sseHandler(t, fixture(t, "usage_limit.sse")), Options{})
	_, _, err := chatOnce(t, c, testRequest())
	pe := providerError(t, err)
	if pe.Kind != provider.ErrUsageLimit {
		t.Fatalf("kind = %q", pe.Kind)
	}
	if want := time.Unix(1789000000, 0).UTC(); !pe.ResetAt.Equal(want) {
		t.Errorf("reset = %v, want %v", pe.ResetAt, want)
	}
}

func TestStreamUsageLimitEventWithResetIn(t *testing.T) {
	c := newTestClient(t, sseHandler(t, fixture(t, "usage_limit_in.sse")), Options{})
	_, _, err := chatOnce(t, c, testRequest())
	pe := providerError(t, err)
	if pe.Kind != provider.ErrUsageLimit {
		t.Fatalf("kind = %q", pe.Kind)
	}
	if d := time.Until(pe.ResetAt); d < 55*time.Minute || d > 65*time.Minute {
		t.Errorf("reset in %v, want about an hour", d)
	}
}

func TestStreamFailedEvents(t *testing.T) {
	cases := []struct {
		fixture string
		want    provider.ErrorKind
	}{
		{"failed_rate_limit.sse", provider.ErrRateLimit},
		{"failed_context.sse", provider.ErrContext},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			c := newTestClient(t, sseHandler(t, fixture(t, tc.fixture)), Options{})
			_, _, err := chatOnce(t, c, testRequest())
			pe := providerError(t, err)
			if pe.Kind != tc.want {
				t.Errorf("kind = %q, want %q", pe.Kind, tc.want)
			}
			if pe.Message == "" {
				t.Error("message must not be empty")
			}
		})
	}
}

// --- transport -------------------------------------------------------------

func TestNetworkError(t *testing.T) {
	c := New(Options{Name: "dead", BaseURL: "http://127.0.0.1:1", APIKey: func() string { return fakeKey }})
	_, _, err := chatOnce(t, c, testRequest())
	pe := providerError(t, err)
	if pe.Kind != provider.ErrNetwork {
		t.Errorf("kind = %q", pe.Kind)
	}
}

func TestTimeoutError(t *testing.T) {
	block := make(chan struct{})
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}), Options{})
	t.Cleanup(func() { close(block) })
	req := testRequest()
	req.Model.Timeout = 50 * time.Millisecond
	_, _, err := chatOnce(t, c, req)
	pe := providerError(t, err)
	if pe.Kind != provider.ErrTimeout {
		t.Errorf("kind = %q", pe.Kind)
	}
}

// --- 401 refresh hook ------------------------------------------------------

func TestUnauthorizedRefreshRetrySucceeds(t *testing.T) {
	var mu sync.Mutex
	var calls, refreshed int
	token := "stale"
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fresh" {
			jsonHandler(http.StatusUnauthorized, `{"error":{"message":"expired token"}}`, nil)(w, r)
			return
		}
		sseHandler(t, fixture(t, "text.sse"))(w, r)
	}), Options{
		APIKey: func() string {
			mu.Lock()
			defer mu.Unlock()
			return token
		},
		OnUnauthorized: func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			refreshed++
			token = "fresh"
			return nil
		},
	})

	resp, _, err := chatOnce(t, c, testRequest())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Text != "Hola, Angel" {
		t.Errorf("text = %q", resp.Text)
	}
	if refreshed != 1 {
		t.Errorf("OnUnauthorized called %d times, want exactly 1", refreshed)
	}
	if calls != 2 {
		t.Errorf("HTTP attempts = %d, want 2", calls)
	}
}

func TestUnauthorizedRefreshStillFails(t *testing.T) {
	var mu sync.Mutex
	var calls, refreshed int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		jsonHandler(http.StatusUnauthorized, `{"error":{"message":"expired token"}}`, nil)(w, r)
	}), Options{
		OnUnauthorized: func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			refreshed++
			return nil
		},
	})

	_, _, err := chatOnce(t, c, testRequest())
	pe := providerError(t, err)
	if pe.Kind != provider.ErrAuth {
		t.Errorf("kind = %q", pe.Kind)
	}
	if refreshed != 1 {
		t.Errorf("OnUnauthorized called %d times, want exactly 1", refreshed)
	}
	if calls != 2 {
		t.Errorf("HTTP attempts = %d, want 2", calls)
	}
}

func TestUnauthorizedHookFailureIsAuthError(t *testing.T) {
	c := newTestClient(t, jsonHandler(http.StatusUnauthorized, `{"error":{"message":"nope"}}`, nil), Options{
		APIKey:         func() string { return fakeKey },
		OnUnauthorized: func(context.Context) error { return errors.New("refresh failed: " + fakeKey) },
	})
	_, _, err := chatOnce(t, c, testRequest())
	pe := providerError(t, err)
	if pe.Kind != provider.ErrAuth {
		t.Errorf("kind = %q", pe.Kind)
	}
	if strings.Contains(pe.Error(), fakeKey) {
		t.Error("refresh error leaked the api key")
	}
}

// --- redaction -------------------------------------------------------------

func TestErrorsNeverLeakSecrets(t *testing.T) {
	body := `{"error":{"message":"bad Authorization: Bearer ` + fakeKey +
		` for account ` + fakeAccount + `"}}`
	c := newTestClient(t, jsonHandler(http.StatusBadRequest, body, nil), Options{
		APIKey: func() string { return fakeKey },
		Headers: func(provider.Request) map[string]string {
			return map[string]string{"chatgpt-account-id": fakeAccount}
		},
	})
	_, _, err := chatOnce(t, c, testRequest())
	pe := providerError(t, err)
	for _, secret := range []string{fakeKey, fakeAccount, "Bearer "} {
		if strings.Contains(pe.Error(), secret) {
			t.Errorf("error leaked %q: %s", secret, pe.Error())
		}
	}
}

func TestStreamErrorsNeverLeakSecrets(t *testing.T) {
	stream := "event: error\n" +
		`data: {"type":"error","code":"server_error","message":"account ` + fakeAccount +
		` key ` + fakeKey + ` rejected"}` + "\n\n"
	c := newTestClient(t, sseHandler(t, stream), Options{
		APIKey: func() string { return fakeKey },
		Headers: func(provider.Request) map[string]string {
			return map[string]string{"chatgpt-account-id": fakeAccount}
		},
	})
	_, _, err := chatOnce(t, c, testRequest())
	pe := providerError(t, err)
	if strings.Contains(pe.Error(), fakeKey) || strings.Contains(pe.Error(), fakeAccount) {
		t.Errorf("stream error leaked a secret: %s", pe.Error())
	}
}
