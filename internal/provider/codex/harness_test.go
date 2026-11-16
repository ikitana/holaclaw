package codex

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/event"
)

// Fake credentials used as *input* state (what auth.toml already holds).
// Upstream *answers* live in testdata/ instead. Every value is distinctive so
// the redaction tests can search for it verbatim.
const (
	jwtHeader = "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0"
	// payload {"exp":1600000000,"https://api.openai.com/auth":{"chatgpt_account_id":"acct-old"}}
	stalePayload = "eyJleHAiOjE2MDAwMDAwMDAsImh0dHBzOi8vYXBpLm9wZW5haS5jb20vYXV0aCI6eyJjaGF0Z3B0X2FjY291bnRfaWQiOiJhY2N0LW9sZCJ9fQ"
	// payload {"iss":"https://auth.openai.com"} — neither exp nor account id
	noClaimsPayload = "eyJpc3MiOiJodHRwczovL2F1dGgub3BlbmFpLmNvbSJ9"

	staleAccess    = jwtHeader + "." + stalePayload + ".FAKEsigSTALEaccess"
	noClaimsAccess = jwtHeader + "." + noClaimsPayload + ".FAKEsigNOCLAIMS"
	storedRefresh  = "FAKErefreshTOKENstored0"
	storedAccount  = "acct-stored-9"

	// What testdata/refresh_success.json and testdata/token_exchange.json carry.
	rotatedAccess  = jwtHeader + ".eyJleHAiOjE4OTM0NTYwMDAsImh0dHBzOi8vYXBpLm9wZW5haS5jb20vYXV0aCI6eyJjaGF0Z3B0X2FjY291bnRfaWQiOiJhY2N0LWZpeHR1cmUtbG9naW4ifX0.FAKEsigACCESSROTATED"
	rotatedRefresh = "FAKErefreshTOKENrotated2"
	loginAccount   = "acct-fixture-login"
)

// clockAt is the fake "now" of every test: after the stale token expired, long
// before the rotated one does.
var clockAt = time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)

// clock is an injectable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: clockAt} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fixture reads a recorded upstream answer.
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(b)
}

// --- secrets ----------------------------------------------------------------

// newSecrets opens an empty auth.toml in a temp directory.
func newSecrets(t *testing.T) *secrets.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.toml")
	if err := os.WriteFile(path, []byte("version = 1\n"), 0o600); err != nil {
		t.Fatalf("write auth.toml: %v", err)
	}
	f, err := secrets.Open(path)
	if err != nil {
		t.Fatalf("open secrets: %v", err)
	}
	return f
}

// seed writes one [providers.<name>] entry.
func seed(t *testing.T, f *secrets.File, name string, kv map[string]string) {
	t.Helper()
	if err := f.Set(secrets.Providers, name, kv); err != nil {
		t.Fatalf("seed secrets: %v", err)
	}
}

// seedStale seeds credentials whose access token is long expired, so the next
// token() call must refresh.
func seedStale(t *testing.T, f *secrets.File, name string) {
	t.Helper()
	seed(t, f, name, map[string]string{
		keyAccessToken:  staleAccess,
		keyRefreshToken: storedRefresh,
		keyAccountID:    storedAccount,
		keyExpiresAt:    clockAt.Add(-time.Hour).Format(time.RFC3339),
	})
}

// seedFresh seeds credentials that are valid for another hour, so a call goes
// straight out without a refresh.
func seedFresh(t *testing.T, f *secrets.File, name string) {
	t.Helper()
	seed(t, f, name, map[string]string{
		keyAccessToken:  noClaimsAccess,
		keyRefreshToken: storedRefresh,
		keyAccountID:    storedAccount,
		keyExpiresAt:    clockAt.Add(time.Hour).Format(time.RFC3339),
	})
}

// failingStore wraps a secrets file and makes Set fail, to check that a write
// failure is surfaced and the old state kept.
type failingStore struct {
	inner secretStore
	err   error
	sets  int
}

func (s *failingStore) Get(sec secrets.Section, name, key string) (string, bool) {
	return s.inner.Get(sec, name, key)
}

func (s *failingStore) Set(sec secrets.Section, name string, kv map[string]string) error {
	s.sets++
	if s.err != nil {
		return s.err
	}
	return s.inner.Set(sec, name, kv)
}

// --- fake upstream ----------------------------------------------------------

// record is one request the fake upstream received.
type record struct {
	Method string
	Path   string
	Header http.Header
	Form   url.Values
	Body   string
}

// fake is the whole unofficial surface behind one httptest server: the two
// device-auth endpoints, /oauth/token, the model list and the Responses
// endpoint. Handlers are per-test; unset ones are a test failure.
type fake struct {
	t   *testing.T
	srv *httptest.Server

	mu    sync.Mutex
	reqs  []record
	calls map[string]int

	usercode  http.HandlerFunc
	device    http.HandlerFunc
	oauth     http.HandlerFunc
	models    http.HandlerFunc
	inference http.HandlerFunc
}

const inferencePath = "/backend-api/codex/responses"

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{t: t, calls: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc(pathUserCode, f.wrap(func() http.HandlerFunc { return f.usercode }))
	mux.HandleFunc(pathDeviceToken, f.wrap(func() http.HandlerFunc { return f.device }))
	mux.HandleFunc(pathOAuthToken, f.wrap(func() http.HandlerFunc { return f.oauth }))
	mux.HandleFunc("/backend-api/codex/models", f.wrap(func() http.HandlerFunc { return f.models }))
	mux.HandleFunc(inferencePath, f.wrap(func() http.HandlerFunc { return f.inference }))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// wrap records every request before handing it to the current handler.
func (f *fake) wrap(pick func() http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec := record{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: string(body)}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			rec.Form, _ = url.ParseQuery(string(body))
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, rec)
		f.calls[r.URL.Path]++
		f.mu.Unlock()

		h := pick()
		if h == nil {
			f.t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		h(w, r)
	}
}

func (f *fake) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[path]
}

func (f *fake) records(path string) []record {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []record
	for _, r := range f.reqs {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// last returns the most recent request to path.
func (f *fake) last(path string) record {
	recs := f.records(path)
	if len(recs) == 0 {
		f.t.Fatalf("no request to %s", path)
	}
	return recs[len(recs)-1]
}

func (f *fake) authBase() string     { return f.srv.URL }
func (f *fake) inferenceURL() string { return f.srv.URL + inferencePath }
func (f *fake) client() *http.Client { return f.srv.Client() }

// json answers with a JSON body.
func jsonHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// sequence answers with each handler in turn, repeating the last one.
func sequence(hs ...http.HandlerFunc) http.HandlerFunc {
	var mu sync.Mutex
	n := 0
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		h := hs[min(n, len(hs)-1)]
		n++
		mu.Unlock()
		h(w, r)
	}
}

// sseHandler streams a recorded SSE body.
func sseHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, frame := range strings.SplitAfter(body, "\n\n") {
			if frame == "" {
				continue
			}
			_, _ = io.WriteString(w, frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// --- provider under test ----------------------------------------------------

// newProvider wires a Provider at the fake upstream with a fake clock and a
// recording event sink.
func newProvider(t *testing.T, f *fake, sec *secrets.File, c *clock, sink event.Sink) *Provider {
	t.Helper()
	p := New(Options{
		Name:         "codex",
		Secrets:      sec,
		Events:       sink,
		HTTPClient:   f.client(),
		Now:          c.now,
		AuthBaseURL:  f.authBase(),
		InferenceURL: f.inferenceURL(),
		ClientID:     "test-client-id",
	})
	// Tests must never wait out a real poll interval.
	p.login.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return p
}

// sleepRecorder is a sleep seam that records the waits instead of taking them.
type sleepRecorder struct {
	mu   sync.Mutex
	took []time.Duration
}

func (s *sleepRecorder) sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.took = append(s.took, d)
	s.mu.Unlock()
	return ctx.Err()
}

func (s *sleepRecorder) all() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.took...)
}

// containsAny reports whether s holds any of the needles.
func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// secretValues are every fake credential a test may hold; no error, event or
// log line may contain one.
func secretValues() []string {
	return []string{
		staleAccess, noClaimsAccess, storedRefresh, rotatedAccess, rotatedRefresh,
		"FAKEdeviceCODE0001", "FAKEauthzCODE0001", "FAKEcodeVERIFIER0001",
		"FAKErefreshTOKENlogin1",
	}
}

// recorder collects emitted events.
type recorder struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *recorder) Emit(_ context.Context, e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) all() []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]event.Event(nil), r.events...)
}
