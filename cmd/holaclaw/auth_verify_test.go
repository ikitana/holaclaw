package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"holaclaw/internal/auth/secrets"
)

// goodToken has the shape BotFather issues, so it reaches the network step.
const goodToken = "424242:AAHsUpErSeCrEtToKeNvAlUe-not-a-real-one"

// fakeBotAPI serves getMe with a scripted outcome and points auth set at
// itself. status 0 means a normal reply.
func fakeBotAPI(t *testing.T, status int, body string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/getMe") {
			http.Error(w, "unexpected method", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	pointAt(t, srv.URL)
}

// pointAt redirects the token check for one test.
func pointAt(t *testing.T, base string) {
	t.Helper()
	prev := telegramAPIBase
	telegramAPIBase = base
	t.Cleanup(func() { telegramAPIBase = prev })
}

func okGetMe(t *testing.T, user, first string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"ok": true,
		"result": map[string]any{
			"id": 99, "is_bot": true, "username": user, "first_name": first,
		},
	})
	if err != nil {
		t.Fatalf("encode getMe: %v", err)
	}
	return string(b)
}

// storedKeys reports the keys held for a channel instance in auth.toml.
func storedKeys(t *testing.T, dir, name string) []string {
	t.Helper()
	file, err := openSecrets(dir)
	if err != nil {
		t.Fatalf("open secrets: %v", err)
	}
	return file.Keys(secrets.Channels, name)
}

func TestAuthSetVerifiesATelegramToken(t *testing.T) {
	dir := fixture(t, "full")
	fakeBotAPI(t, 0, okGetMe(t, "holaclaw", "Hola"))

	code, out, errOut := runCLI(t, goodToken+"\n", "auth", "set", "--config", dir, "channels.tg-main", "bot_token")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, errOut)
	}
	if !strings.Contains(out, "verified @holaclaw") {
		t.Errorf("stdout = %q, want the verified identity", out)
	}
	if got := storedKeys(t, dir, "tg-main"); len(got) != 1 || got[0] != "bot_token" {
		t.Errorf("stored keys = %v, want [bot_token]", got)
	}
}

// The whole point of the check: a token Telegram refuses never lands in
// auth.toml, where it would pass every offline gate and fail at run time.
func TestAuthSetRefusesARejectedToken(t *testing.T) {
	dir := fixture(t, "full")
	fakeBotAPI(t, http.StatusUnauthorized, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)

	code, _, errOut := runCLI(t, goodToken+"\n", "auth", "set", "--config", dir, "channels.tg-main", "bot_token")
	if code == 0 {
		t.Fatal("exit = 0, want a failure")
	}
	if !strings.Contains(errOut, "--no-verify") {
		t.Errorf("stderr = %q, want the escape hatch named", errOut)
	}
	if got := storedKeys(t, dir, "tg-main"); len(got) != 0 {
		t.Errorf("stored keys = %v, want the token not to have been written", got)
	}
}

// A truncated paste is caught without reaching the network at all.
func TestAuthSetRefusesAMalformedTokenOffline(t *testing.T) {
	dir := fixture(t, "full")
	pointAt(t, "http://127.0.0.1:1") // never dialled

	code, _, errOut := runCLI(t, "424242:short\n", "auth", "set", "--config", dir, "channels.tg-main", "bot_token")
	if code == 0 {
		t.Fatal("exit = 0, want a failure")
	}
	if !strings.Contains(errOut, "BotFather") {
		t.Errorf("stderr = %q, want the expected shape explained", errOut)
	}
	if got := storedKeys(t, dir, "tg-main"); len(got) != 0 {
		t.Errorf("stored keys = %v, want nothing written", got)
	}
}

// A box that cannot reach Telegram yet must still be configurable.
func TestAuthSetStoresTheTokenWhenTelegramIsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()

	dir := fixture(t, "full")
	pointAt(t, addr)

	code, _, errOut := runCLI(t, goodToken+"\n", "auth", "set", "--config", dir, "channels.tg-main", "bot_token")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "could not reach Telegram") {
		t.Errorf("stderr = %q, want a warning that the check was skipped", errOut)
	}
	if got := storedKeys(t, dir, "tg-main"); len(got) != 1 {
		t.Errorf("stored keys = %v, want the token stored unchecked", got)
	}
}

func TestAuthSetNoVerifySkipsTheCheck(t *testing.T) {
	dir := fixture(t, "full")
	pointAt(t, "http://127.0.0.1:1") // never dialled

	code, out, errOut := runCLI(t, "not-a-token\n", "auth", "set", "--config", dir,
		"--no-verify", "channels.tg-main", "bot_token")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, errOut)
	}
	if strings.Contains(out, "verified") {
		t.Errorf("stdout = %q, want no verification", out)
	}
	if got := storedKeys(t, dir, "tg-main"); len(got) != 1 {
		t.Errorf("stored keys = %v, want the token stored", got)
	}
}

// Only the telegram adapter is checked here; a slack bot token has a different
// shape and a different endpoint.
func TestAuthSetDoesNotCheckANonTelegramChannel(t *testing.T) {
	dir := fixture(t, "full")
	pointAt(t, "http://127.0.0.1:1") // never dialled

	code, _, errOut := runCLI(t, "xoxb-1234-5678-abcdefghij\n", "auth", "set", "--config", dir,
		"channels.slack-ops", "bot_token")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, errOut)
	}
	if got := storedKeys(t, dir, "slack-ops"); len(got) != 1 {
		t.Errorf("stored keys = %v, want the token stored", got)
	}
}

// A channel the config does not describe cannot be checked, and must still be
// settable: the secret may well be written before the instance is configured.
func TestAuthSetStoresATokenForAnUnconfiguredChannel(t *testing.T) {
	dir := fixture(t, "full")
	pointAt(t, "http://127.0.0.1:1") // never dialled

	code, _, errOut := runCLI(t, "whatever\n", "auth", "set", "--config", dir, "channels.tg-later", "bot_token")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, errOut)
	}
	if got := storedKeys(t, dir, "tg-later"); len(got) != 1 {
		t.Errorf("stored keys = %v, want the token stored", got)
	}
}

// A provider key is not a bot token and never reaches the check.
func TestAuthSetDoesNotCheckProviderKeys(t *testing.T) {
	dir := fixture(t, "full")
	pointAt(t, "http://127.0.0.1:1") // never dialled

	if code, _, errOut := runCLI(t, "sk-test\n", "auth", "set", "--config", dir,
		"providers.openrouter", "api_key"); code != 0 {
		t.Fatalf("exit = %d, want 0\n%s", code, errOut)
	}
}
