package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// These tests cover the `auth set` path for a Google Chat service-account
// key: the checks docs/channels/googlechat.md §3 promises, run against a
// fake token/Pub/Sub/Chat server, never the real Google APIs
// (internal/channel/googlechat has its own, lower-level tests for Verify
// itself; these only check that `auth set` reads the key, calls Verify and
// maps the result the way the docs table says).

const (
	gcProject = "proj-1"
	gcEmail   = "holaclaw@proj-1.iam.gserviceaccount.com"
)

var gcRSA *rsa.PrivateKey

func gcKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	if gcRSA != nil {
		return gcRSA
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	gcRSA = k
	return k
}

// gcKeyJSON builds a service-account key file as Google issues it, with mod
// applied to the decoded map before it is marshalled back — the way to break
// one field without hand-writing the whole document.
func gcKeyJSON(t *testing.T, mod func(m map[string]any)) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(gcKey(t))
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	m := map[string]any{
		"type":           "service_account",
		"project_id":     gcProject,
		"private_key_id": "kid-1",
		"private_key":    pemKey,
		"client_email":   gcEmail,
		"token_uri":      "https://token.invalid/never",
	}
	if mod != nil {
		mod(m)
	}
	// Indented, like the file Google's console actually hands out: several
	// lines, not one, so a stdin reader that stops at the first newline
	// would truncate it.
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal key json: %v", err)
	}
	return string(b)
}

// gcFakeOpt scripts one route's status/body; zero value means the default
// (a 200 success) handles it.
type gcFakeOpt struct {
	route string // "token", "subcheck", "spaces.list"
	code  int
	body  string // raw JSON body; default is a plausible Google error/success
}

// gcFake serves the token endpoint, subscriptions.get and spaces.list on one
// httptest server, and points googlechatAPIBase at it for the duration of
// the test.
func gcFake(t *testing.T, opts ...gcFakeOpt) *httptest.Server {
	t.Helper()
	by := map[string]gcFakeOpt{}
	for _, o := range opts {
		by[o.route] = o
	}
	var mu sync.Mutex
	calls := map[string]int{}

	sub := "/v1/projects/" + gcProject + "/subscriptions/holaclaw"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var route string
		switch {
		case r.URL.Path == "/token":
			route = "token"
		case r.URL.Path == sub+":testIamPermissions" && r.Method == http.MethodPost:
			route = "subcheck"
		case r.URL.Path == "/v1/spaces" && r.Method == http.MethodGet:
			route = "spaces.list"
		default:
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		calls[route]++
		mu.Unlock()

		if o, ok := by[route]; ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(o.code)
			body := o.body
			if body == "" {
				body = fmt.Sprintf(`{"error":{"code":%d,"message":"scripted","status":"ERROR"}}`, o.code)
			}
			_, _ = w.Write([]byte(body))
			return
		}
		switch route {
		case "token":
			writeJSONGC(w, 200, map[string]any{"access_token": "ya29.fake", "expires_in": 3600, "token_type": "Bearer"})
		case "subcheck":
			writeJSONGC(w, 200, map[string]any{"permissions": []string{"pubsub.subscriptions.consume"}})
		case "spaces.list":
			writeJSONGC(w, 200, map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)

	prev := googlechatAPIBase
	googlechatAPIBase = srv.URL
	t.Cleanup(func() { googlechatAPIBase = prev })
	return srv
}

func writeJSONGC(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestAuthSetVerifiesAGooglechatKey(t *testing.T) {
	dir := fixture(t, "full")
	gcFake(t)
	key := gcKeyJSON(t, nil)

	code, out, errOut := runCLI(t, key, "auth", "set", "--config", dir, "channels.gc-main", "service_account")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, errOut)
	}
	want := fmt.Sprintf("verified %s (project %s)", gcEmail, gcProject)
	if !strings.Contains(out, want) {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	if got := storedKeys(t, dir, "gc-main"); len(got) != 1 || got[0] != "service_account" {
		t.Errorf("stored keys = %v, want [service_account]", got)
	}
}

// The whole JSON key has to be read, including every newline inside the PEM
// block: a one-line stdin read would truncate it after "-----BEGIN...".
func TestAuthSetReadsTheWholeMultilineKeyFromStdin(t *testing.T) {
	dir := fixture(t, "full")
	gcFake(t)
	key := gcKeyJSON(t, nil)
	if !strings.Contains(key, "\n") {
		t.Fatal("test key has no embedded newline to exercise")
	}

	code, _, errOut := runCLI(t, "  \n"+key+"\n  ", "auth", "set", "--config", dir, "channels.gc-main", "service_account")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, errOut)
	}
	if got := storedKeys(t, dir, "gc-main"); len(got) != 1 {
		t.Errorf("stored keys = %v, want [service_account]", got)
	}
}

func TestAuthSetRefusesAGooglechatKeyAsArgument(t *testing.T) {
	dir := fixture(t, "full")
	gcFake(t) // never dialled: the argv value is refused before any check

	code, _, errOut := runCLI(t, "", "auth", "set", "--config", dir, "channels.gc-main", "service_account", "{}")
	if code == 0 {
		t.Fatal("exit = 0, want a failure")
	}
	if !strings.Contains(errOut, "argv") && !strings.Contains(errOut, "argument") {
		t.Errorf("stderr = %q, want the leak explained", errOut)
	}
	if !strings.Contains(errOut, "< key.json") {
		t.Errorf("stderr = %q, want the pipe hint", errOut)
	}
	if got := storedKeys(t, dir, "gc-main"); len(got) != 0 {
		t.Errorf("stored keys = %v, want nothing written", got)
	}
}

func TestAuthSetRefusesANonServiceAccountKey(t *testing.T) {
	dir := fixture(t, "full")
	gcFake(t) // never dialled: readKey fails locally

	code, _, errOut := runCLI(t, `{"type":"authorized_user"}`, "auth", "set", "--config", dir, "channels.gc-main", "service_account")
	if code == 0 {
		t.Fatal("exit = 0, want a failure")
	}
	if !strings.Contains(errOut, "not a service-account key") || !strings.Contains(errOut, "§2 step 3") {
		t.Errorf("stderr = %q, want the step 3 message", errOut)
	}
	if got := storedKeys(t, dir, "gc-main"); len(got) != 0 {
		t.Errorf("stored keys = %v, want nothing written", got)
	}
}

func TestAuthSetRefusesARejectedGooglechatKey(t *testing.T) {
	dir := fixture(t, "full")
	gcFake(t, gcFakeOpt{route: "token", code: 400, body: `{"error":"invalid_grant"}`})

	code, _, errOut := runCLI(t, gcKeyJSON(t, nil), "auth", "set", "--config", dir, "channels.gc-main", "service_account")
	if code == 0 {
		t.Fatal("exit = 0, want a failure")
	}
	if !strings.Contains(errOut, "Google rejected this key") || !strings.Contains(errOut, "§2 step 3") {
		t.Errorf("stderr = %q, want the step 3 rejection message", errOut)
	}
	if got := storedKeys(t, dir, "gc-main"); len(got) != 0 {
		t.Errorf("stored keys = %v, want nothing written", got)
	}
}

func TestAuthSetRefusesAMissingSubscription(t *testing.T) {
	dir := fixture(t, "full")
	gcFake(t, gcFakeOpt{route: "subcheck", code: 404})

	code, _, errOut := runCLI(t, gcKeyJSON(t, nil), "auth", "set", "--config", dir, "channels.gc-main", "service_account")
	if code == 0 {
		t.Fatal("exit = 0, want a failure")
	}
	if !strings.Contains(errOut, "no subscription `holaclaw` in project `"+gcProject+"`") || !strings.Contains(errOut, "§2 step 5") {
		t.Errorf("stderr = %q, want the step 5 not-found message", errOut)
	}
	if got := storedKeys(t, dir, "gc-main"); len(got) != 0 {
		t.Errorf("stored keys = %v, want nothing written", got)
	}
}

// TestAuthSetRefusesAKeyThatCannotPull is a service account without Pub/Sub
// Subscriber on the subscription: testIamPermissions answers 200 and leaves
// the permission out.
func TestAuthSetRefusesAKeyThatCannotPull(t *testing.T) {
	dir := fixture(t, "full")
	gcFake(t, gcFakeOpt{route: "subcheck", code: 200, body: `{}`})

	code, _, errOut := runCLI(t, gcKeyJSON(t, nil), "auth", "set", "--config", dir, "channels.gc-main", "service_account")
	if code == 0 {
		t.Fatal("exit = 0, want a failure")
	}
	if !strings.Contains(errOut, "the service account "+gcEmail+" cannot pull from") || !strings.Contains(errOut, "§2 step 5") {
		t.Errorf("stderr = %q, want the step 5 forbidden message", errOut)
	}
	if got := storedKeys(t, dir, "gc-main"); len(got) != 0 {
		t.Errorf("stored keys = %v, want nothing written", got)
	}
}

func TestAuthSetRefusesAnUnconfiguredChatApp(t *testing.T) {
	dir := fixture(t, "full")
	gcFake(t, gcFakeOpt{route: "spaces.list", code: 404})

	code, _, errOut := runCLI(t, gcKeyJSON(t, nil), "auth", "set", "--config", dir, "channels.gc-main", "service_account")
	if code == 0 {
		t.Fatal("exit = 0, want a failure")
	}
	if !strings.Contains(errOut, "the Chat app is not configured") || !strings.Contains(errOut, "§2 step 6") {
		t.Errorf("stderr = %q, want the step 6 message", errOut)
	}
	if got := storedKeys(t, dir, "gc-main"); len(got) != 0 {
		t.Errorf("stored keys = %v, want nothing written", got)
	}
}

func TestAuthSetStoresTheGooglechatKeyWhenGoogleIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close() // closed: every dial now fails, never a real network call

	dir := fixture(t, "full")
	prev := googlechatAPIBase
	googlechatAPIBase = addr
	t.Cleanup(func() { googlechatAPIBase = prev })

	code, _, errOut := runCLI(t, gcKeyJSON(t, nil), "auth", "set", "--config", dir, "channels.gc-main", "service_account")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "could not reach Google") {
		t.Errorf("stderr = %q, want a warning that the check was skipped", errOut)
	}
	if got := storedKeys(t, dir, "gc-main"); len(got) != 1 {
		t.Errorf("stored keys = %v, want the key stored unchecked", got)
	}
}

func TestAuthSetNoVerifySkipsTheGooglechatCheck(t *testing.T) {
	dir := fixture(t, "full")
	prev := googlechatAPIBase
	googlechatAPIBase = "http://127.0.0.1:1" // never dialled
	t.Cleanup(func() { googlechatAPIBase = prev })

	code, out, errOut := runCLI(t, `{"type":"authorized_user"}`, "auth", "set", "--config", dir,
		"--no-verify", "channels.gc-main", "service_account")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, errOut)
	}
	if strings.Contains(out, "verified") {
		t.Errorf("stdout = %q, want no verification", out)
	}
	if got := storedKeys(t, dir, "gc-main"); len(got) != 1 {
		t.Errorf("stored keys = %v, want the key stored", got)
	}
}
