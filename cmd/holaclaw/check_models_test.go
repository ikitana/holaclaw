package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
)

// MOD003: `check` asks each provider for the model ids it serves
// (docs/models.md §9).

// modelsConfig is the minimal fixture with its one model, gemma, on a
// provider of the given type, and its secrets in the temp dir.
func modelsConfig(t *testing.T, providerTOML, id string) string {
	t.Helper()
	dir := fixture(t, "minimal")
	writeTestFile(t, filepath.Join(dir, config.FileConfig), fmt.Sprintf(`version = 1

[server]
secrets_dir = %q

[providers.main]
%s

[models.gemma]
provider = "main"
id       = %q
context  = 8192
`, filepath.Join(dir, "secrets"), providerTOML, id))
	return dir
}

// compatConfig points the model at an OpenAI-compatible endpoint.
func compatConfig(t *testing.T, baseURL, id string) string {
	t.Helper()
	return modelsConfig(t, fmt.Sprintf("type = \"openai-compatible\"\nbase_url = %q", baseURL+"/v1"), id)
}

// modelsServer answers GET /v1/models, and counts the requests it got.
func modelsServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound) // the MOD002 probes
			return
		}
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

const servedModels = `{"object":"list","data":[{"id":"gemma4:e2b"},{"id":"qwen3:8b"}]}`

func TestCheckModelServed(t *testing.T) {
	srv, hits := modelsServer(t, http.StatusOK, servedModels)
	dir := compatConfig(t, srv.URL, "gemma4:e2b")
	code, out, _ := runCLI(t, "", "check", "--config", dir)
	if code != 0 || strings.Contains(out, "MOD003") {
		t.Fatalf("exit = %d, want 0 and no MOD003\n%s", code, out)
	}
	if hits.Load() != 1 {
		t.Errorf("model list requests = %d, want 1", hits.Load())
	}
}

func TestCheckModelNotServed(t *testing.T) {
	srv, _ := modelsServer(t, http.StatusOK, servedModels)
	dir := compatConfig(t, srv.URL, "gemma4:e4b")
	code, out, _ := runCLI(t, "", "check", "--config", dir)
	if code != 2 {
		t.Fatalf("exit = %d, want 2\n%s", code, out)
	}
	want := `MOD003 error: config.toml: models.gemma.id: main does not serve "gemma4:e4b"; it serves: gemma4:e2b, qwen3:8b`
	if !strings.Contains(out, want) {
		t.Errorf("no MOD003 naming the key and the served ids:\n%s", out)
	}
}

func TestCheckModelOffline(t *testing.T) {
	srv, hits := modelsServer(t, http.StatusOK, servedModels)
	dir := compatConfig(t, srv.URL, "gemma4:e4b")
	code, out, _ := runCLI(t, "", "check", "--config", dir, "--offline")
	if code != 0 || strings.Contains(out, "MOD003") {
		t.Fatalf("exit = %d, want 0 and no MOD003\n%s", code, out)
	}
	if hits.Load() != 0 {
		t.Errorf("--offline still asked for the model list")
	}
}

// An endpoint that cannot say what it serves is never an error.
func TestCheckModelListUnavailable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"non-2xx", http.StatusInternalServerError, `{"error":"boom"}`},
		{"no such route", http.StatusNotFound, `not found`},
		{"malformed body", http.StatusOK, `<html>not json</html>`},
		{"empty list", http.StatusOK, `{"data":[]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := modelsServer(t, tc.status, tc.body)
			dir := compatConfig(t, srv.URL, "gemma4:e4b")
			code, out, _ := runCLI(t, "", "check", "--config", dir)
			if code != 0 {
				t.Fatalf("exit = %d, want 0\n%s", code, out)
			}
			if !strings.Contains(out, "MOD003 info: config.toml: providers.main: could not list the models of main") {
				t.Errorf("no MOD003 info:\n%s", out)
			}
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		srv, _ := modelsServer(t, http.StatusOK, servedModels)
		srv.Close() // nothing listens there any more
		dir := compatConfig(t, srv.URL, "gemma4:e4b")
		code, out, _ := runCLI(t, "", "check", "--config", dir)
		if code != 0 {
			t.Fatalf("exit = %d, want 0\n%s", code, out)
		}
		if !strings.Contains(out, "MOD003 info: config.toml: providers.main: could not list the models of main") {
			t.Errorf("no MOD003 info:\n%s", out)
		}
	})
}

// codexUpstream is a fake ChatGPT backend: only its model list answers, and
// any other request (a token refresh above all) fails the test.
func codexUpstream(t *testing.T, status int, body string) *atomic.Int32 {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/codex/models" {
			t.Errorf("unexpected request to %s", r.URL.Path)
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer FAKEaccessTOKEN" {
			t.Errorf("the stored access token was not sent")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	prev := http.DefaultTransport
	http.DefaultTransport = redirect{host: "chatgpt.com", to: srv.Listener.Addr().String(), next: prev}
	t.Cleanup(func() { http.DefaultTransport = prev })
	return &hits
}

// redirect sends the requests for one host to a test server instead: `check`
// builds its providers from the config alone, with their real addresses.
type redirect struct {
	host, to string
	next     http.RoundTripper
}

func (r redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == r.host {
		req = req.Clone(req.Context())
		req.URL.Scheme, req.URL.Host = "http", r.to
	}
	return r.next.RoundTrip(req)
}

// codexConfig puts the model on a codex subscription; signedIn stores a
// token set whose access token is long expired.
func codexConfig(t *testing.T, id string, signedIn bool) string {
	t.Helper()
	dir := modelsConfig(t, "type = \"codex\"\nauth = \"subscription\"", id)
	if !signedIn {
		return dir
	}
	sec, err := openSecrets(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = sec.Set(secrets.Providers, "main", map[string]string{
		"access_token":  "FAKEaccessTOKEN",
		"refresh_token": "FAKErefreshTOKEN",
		"expires_at":    "2020-01-01T00:00:00Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

const codexLive = `{"data":[{"id":"gpt-6.1-sol"},{"id":"gpt-6-luna"}]}`

func TestCheckCodexModel(t *testing.T) {
	t.Run("served", func(t *testing.T) {
		hits := codexUpstream(t, http.StatusOK, codexLive)
		dir := codexConfig(t, "gpt-6-luna", true)
		code, out, _ := runCLI(t, "", "check", "--config", dir, "--only", "MOD003")
		if code != 0 || strings.Contains(out, "MOD003") || hits.Load() != 1 {
			t.Fatalf("exit = %d, requests = %d, want 0 and 1, no MOD003\n%s", code, hits.Load(), out)
		}
	})

	t.Run("not served: error", func(t *testing.T) {
		codexUpstream(t, http.StatusOK, codexLive)
		dir := codexConfig(t, "gpt-6.1-luna", true)
		code, out, _ := runCLI(t, "", "check", "--config", dir, "--only", "MOD003")
		if code != 2 {
			t.Fatalf("exit = %d, want 2\n%s", code, out)
		}
		want := `MOD003 error: config.toml: models.gemma.id: main does not serve "gpt-6.1-luna"; it serves: gpt-6.1-sol, gpt-6-luna`
		if !strings.Contains(out, want) {
			t.Errorf("no MOD003 naming the key and the served ids:\n%s", out)
		}
	})

	// The tokens are the daemon's: nothing refreshes or rewrites them,
	// expired as they are, and a list that cannot be read is never an error.
	t.Run("list unavailable: info, auth.toml untouched", func(t *testing.T) {
		codexUpstream(t, http.StatusUnauthorized, `{"error":"expired"}`)
		dir := codexConfig(t, "gpt-6.1-luna", true)
		authPath := filepath.Join(dir, "secrets", "auth.toml")
		before, err := os.ReadFile(authPath)
		if err != nil {
			t.Fatal(err)
		}
		code, out, _ := runCLI(t, "", "check", "--config", dir, "--only", "MOD003")
		if code != 0 {
			t.Fatalf("exit = %d, want 0\n%s", code, out)
		}
		if !strings.Contains(out, "MOD003 info: config.toml: providers.main: could not list the models of main") {
			t.Errorf("no MOD003 info:\n%s", out)
		}
		after, err := os.ReadFile(authPath)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(before) {
			t.Errorf("check rewrote auth.toml")
		}
	})

	t.Run("not signed in: info, no request", func(t *testing.T) {
		hits := codexUpstream(t, http.StatusOK, codexLive)
		dir := codexConfig(t, "gpt-6.1-luna", false)
		code, out, _ := runCLI(t, "", "check", "--config", dir, "--only", "MOD003")
		if code != 0 || hits.Load() != 0 {
			t.Fatalf("exit = %d, requests = %d, want 0 and 0\n%s", code, hits.Load(), out)
		}
		if !strings.Contains(out, "MOD003 info: config.toml: providers.main: could not list the models of main") {
			t.Errorf("no MOD003 info:\n%s", out)
		}
	})

	// With an API key the provider cannot list its models: nothing is asked.
	t.Run("no model list: no finding", func(t *testing.T) {
		hits := codexUpstream(t, http.StatusOK, codexLive)
		dir := modelsConfig(t, "type = \"codex\"\nauth = \"api-key\"", "gpt-6.1-luna")
		code, out, _ := runCLI(t, "", "check", "--config", dir, "--only", "MOD003")
		if code != 0 || strings.Contains(out, "MOD003") || hits.Load() != 0 {
			t.Fatalf("exit = %d, requests = %d, want 0 and 0, no MOD003\n%s", code, hits.Load(), out)
		}
	})

	t.Run("offline", func(t *testing.T) {
		hits := codexUpstream(t, http.StatusOK, codexLive)
		dir := codexConfig(t, "gpt-6.1-luna", true)
		code, out, _ := runCLI(t, "", "check", "--config", dir, "--only", "MOD003", "--offline")
		if code != 0 || strings.Contains(out, "MOD003") || hits.Load() != 0 {
			t.Fatalf("exit = %d, requests = %d, want 0 and 0, no MOD003\n%s", code, hits.Load(), out)
		}
	})
}
