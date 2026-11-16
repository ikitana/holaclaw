// Contract test for the unofficial ChatGPT-subscription flow.
//
// This one test drives the whole life cycle — device-code login, poll, token
// exchange, inference, refresh — against an httptest server that replays the
// recorded fixtures in testdata/, and asserts the exact shape of every request
// we send. It is the tripwire for upstream drift: if the real service changes,
// this file and testdata/ are what change, and nothing else in holaclaw has to.
//
// How to re-record when upstream drifts:
//  1. Run the official Codex CLI against the real service with an HTTP proxy
//     (e.g. `mitmproxy --set flow_detail=3`) in front of it and complete a
//     login, one inference call and one refresh.
//  2. For each interaction, copy the response body into the matching file in
//     testdata/ — usercode.json, poll_*.json, token_exchange.json,
//     refresh_*.json, inference_tool_call.sse, unauthorized.json,
//     usage_limit.json, models.json. Replace every real credential with a
//     distinctive FAKE… placeholder; the JWT payloads here are unsigned and
//     carry only `exp` and the account claim.
//  3. Update the request-shape assertions below to whatever the CLI now sends,
//     and with them the form builders in auth.go (refreshForm / exchangeForm)
//     and the JSON device requests in login.go.
//  4. `go test ./internal/provider/codex/` must pass with no other package
//     touched.
package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/provider"
)

func TestContractLifecycle(t *testing.T) {
	f := newFake(t)
	c := newClock()
	sec := newSecrets(t)
	sink := &recorder{}
	p := newProvider(t, f, sec, c, sink)

	f.usercode = jsonHandler(http.StatusOK, fixture(t, "usercode.json"))
	f.device = sequence(
		jsonHandler(http.StatusOK, fixture(t, "poll_pending.json")),
		jsonHandler(http.StatusOK, fixture(t, "poll_slow_down.json")),
		jsonHandler(http.StatusOK, fixture(t, "poll_success.json")),
	)
	f.oauth = sequence(
		jsonHandler(http.StatusOK, fixture(t, "token_exchange.json")),
		jsonHandler(http.StatusOK, fixture(t, "refresh_success.json")),
		jsonHandler(http.StatusBadRequest, fixture(t, "refresh_invalid_grant.json")),
	)
	f.inference = sequence(
		sseHandler(fixture(t, "inference_tool_call.sse")),
		jsonHandler(http.StatusTooManyRequests, fixture(t, "usage_limit.json")),
		jsonHandler(http.StatusUnauthorized, fixture(t, "unauthorized.json")),
		sseHandler(fixture(t, "inference_tool_call.sse")),
	)

	// --- 1. login ----------------------------------------------------------
	if err := p.Login().Run(context.Background(), nil); err != nil {
		t.Fatalf("login: %v", err)
	}
	assertUserCodeRequest(t, f.last(pathUserCode))
	assertPollRequest(t, f.last(pathDeviceToken))
	assertExchangeRequest(t, f.records(pathOAuthToken)[0])
	if c := f.count(pathDeviceToken); c != 3 {
		t.Fatalf("polls = %d, want 3 (pending, slow_down, success)", c)
	}
	if v, _ := sec.Get(secrets.Providers, "codex", keyRefreshToken); v != "FAKErefreshTOKENlogin1" {
		t.Fatalf("login did not persist the refresh token")
	}

	// --- 2. inference ------------------------------------------------------
	stream, err := p.Chat(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	resp, err := provider.Collect(stream, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if resp.Finish != provider.FinishToolCalls || len(resp.ToolCalls) != 1 {
		t.Fatalf("response = %+v, want one tool call", resp)
	}
	if got := resp.ToolCalls[0]; got.Name != "read_file" || got.Arguments != `{"path":"README.md"}` {
		t.Fatalf("tool call = %+v", got)
	}
	assertInferenceRequest(t, f.last(inferencePath), "Bearer "+loginAccessToken(t), loginAccount)

	// --- 3. a usage limit is a quota, not an auth problem -------------------
	_, err = p.Chat(context.Background(), chatRequest())
	if provider.KindOf(err) != provider.ErrUsageLimit {
		t.Fatalf("kind = %q, want usage_limit (%v)", provider.KindOf(err), err)
	}
	if c := f.count(pathOAuthToken); c != 1 {
		t.Fatalf("oauth calls = %d: a usage limit must not touch the tokens", c)
	}

	// --- 4. a 401 forces one refresh and one retry --------------------------
	stream, err = p.Chat(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("chat after 401: %v", err)
	}
	if _, err := provider.Collect(stream, nil); err != nil {
		t.Fatalf("collect after 401: %v", err)
	}
	assertRefreshRequest(t, f.records(pathOAuthToken)[1], "FAKErefreshTOKENlogin1")
	assertInferenceRequest(t, f.last(inferencePath), "Bearer "+rotatedAccess, loginAccount)
	if v, _ := sec.Get(secrets.Providers, "codex", keyRefreshToken); v != rotatedRefresh {
		t.Fatalf("the rotated refresh token was not persisted")
	}

	// auth.refreshed is emitted once, with no token in it.
	evs := sink.all()
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1 auth.refreshed", len(evs))
	}
	if containsAny(string(evs[0].Attrs), secretValues()...) {
		t.Fatalf("event leaks a credential: %s", evs[0].Attrs)
	}

	// --- 5. the refresh token is rejected: logged out, no retry -------------
	// Past the rotated token's own expiry claim, so the next call refreshes.
	c.advance(10 * 365 * 24 * time.Hour)
	_, err = p.Chat(context.Background(), chatRequest())
	if provider.KindOf(err) != provider.ErrAuth {
		t.Fatalf("kind = %q, want auth (%v)", provider.KindOf(err), err)
	}
	if !strings.Contains(err.Error(), "codex login expired: run `holaclaw login codex`") {
		t.Fatalf("error = %v, want the login hint", err)
	}
	before := f.count(pathOAuthToken)
	if _, err := p.Chat(context.Background(), chatRequest()); provider.KindOf(err) != provider.ErrAuth {
		t.Fatalf("second call: kind = %q, want auth", provider.KindOf(err))
	}
	if after := f.count(pathOAuthToken); after != before {
		t.Fatalf("oauth calls went %d → %d: a logged-out provider must not retry", before, after)
	}
}

// loginAccessToken is the access token testdata/token_exchange.json carries.
func loginAccessToken(t *testing.T) string {
	t.Helper()
	var tr tokenResponse
	if err := json.Unmarshal([]byte(fixture(t, "token_exchange.json")), &tr); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return tr.AccessToken
}

func assertUserCodeRequest(t *testing.T, rec record) {
	t.Helper()
	if rec.Method != http.MethodPost || rec.Path != pathUserCode {
		t.Errorf("user code request = %s %s", rec.Method, rec.Path)
	}
	body := deviceBody(t, rec)
	if got := body["client_id"]; got != "test-client-id" || len(body) != 1 {
		t.Errorf("client_id = %q", got)
	}
	if got := rec.Header.Get("originator"); got != Originator {
		t.Errorf("originator = %q, want %q", got, Originator)
	}
}

func assertPollRequest(t *testing.T, rec record) {
	t.Helper()
	if rec.Method != http.MethodPost || rec.Path != pathDeviceToken {
		t.Errorf("poll request = %s %s", rec.Method, rec.Path)
	}
	body := deviceBody(t, rec)
	if got := body["device_auth_id"]; got != "FAKEdeviceCODE0001" {
		t.Errorf("device_auth_id = %q", got)
	}
	if got := body["user_code"]; got != "WXYZ-1234" || len(body) != 2 {
		t.Errorf("unexpected poll body: %v", body)
	}
}

func deviceBody(t *testing.T, rec record) map[string]string {
	t.Helper()
	if rec.Header.Get("Content-Type") != "application/json" {
		t.Fatal("device request must use JSON")
	}
	var body map[string]string
	if err := json.Unmarshal([]byte(rec.Body), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func assertExchangeRequest(t *testing.T, rec record) {
	t.Helper()
	want := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     "test-client-id",
		"code":          "FAKEauthzCODE0001",
		"code_verifier": "FAKEcodeVERIFIER0001",
		"redirect_uri":  DefaultRedirectURI,
	}
	for k, v := range want {
		if got := rec.Form.Get(k); got != v {
			t.Errorf("exchange form %s = %q, want %q", k, got, v)
		}
	}
}

func assertRefreshRequest(t *testing.T, rec record, refreshToken string) {
	t.Helper()
	want := map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     "test-client-id",
		"refresh_token": refreshToken,
	}
	for k, v := range want {
		if got := rec.Form.Get(k); got != v {
			t.Errorf("refresh form %s = %q, want %q", k, got, v)
		}
	}
}

// assertInferenceRequest checks the shape docs/models.md fixes: the three
// headers, store:false, non-empty instructions and no max_output_tokens.
func assertInferenceRequest(t *testing.T, rec record, authorization, account string) {
	t.Helper()
	if rec.Method != http.MethodPost || rec.Path != inferencePath {
		t.Errorf("inference request = %s %s", rec.Method, rec.Path)
	}
	if got := rec.Header.Get("Authorization"); got != authorization {
		t.Errorf("authorization header is not the current access token")
	}
	if got := rec.Header.Get("chatgpt-account-id"); got != account {
		t.Errorf("chatgpt-account-id = %q, want %q", got, account)
	}
	if got := rec.Header.Get("originator"); got != Originator {
		t.Errorf("originator = %q, want %q", got, Originator)
	}
	if got := rec.Header.Get("session_id"); got != "sess-abc" {
		t.Errorf("session_id = %q, want the request's session", got)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(rec.Body), &body); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if store, ok := body["store"].(bool); !ok || store {
		t.Errorf("store = %v, want false and always present", body["store"])
	}
	if instr, _ := body["instructions"].(string); instr == "" {
		t.Errorf("instructions must be non-empty")
	}
	if _, ok := body["max_output_tokens"]; ok {
		t.Errorf("max_output_tokens must be absent")
	}
	if got, _ := body["model"].(string); got != "gpt-6.1-sol" {
		t.Errorf("model = %q", got)
	}
	if stream, ok := body["stream"].(bool); !ok || !stream {
		t.Errorf("stream = %v, want true", body["stream"])
	}
}
