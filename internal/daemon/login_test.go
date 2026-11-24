package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/app"
	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/provider"
	"holaclaw/internal/provider/codex"
)

// A credentials write for a subscription Codex provider is adopted by the
// provider, which serves its tokens from memory; a failed write leaves the
// tokens in use; any other provider only has the write run.
func TestStoreCredentials(t *testing.T) {
	var mu sync.Mutex
	var bearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		bearer = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest) // the answer is not under test
	}))
	t.Cleanup(srv.Close)
	// sent is the Authorization header of one request through the provider.
	sent := func(p provider.Provider) string {
		t.Helper()
		_, _ = p.Chat(context.Background(), provider.Request{
			Model:    provider.Model{Name: "m", Provider: "codex", ID: "m"},
			Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "hello")},
		})
		mu.Lock()
		defer mu.Unlock()
		return bearer
	}

	path := filepath.Join(t.TempDir(), "auth.toml")
	if err := os.WriteFile(path, []byte("version = 1\n"), 0o600); err != nil {
		t.Fatalf("write auth.toml: %v", err)
	}
	sec, err := secrets.Open(path)
	if err != nil {
		t.Fatalf("open secrets: %v", err)
	}
	set := func(access string) func() error {
		return func() error {
			return sec.Set(secrets.Providers, "codex", map[string]string{
				"access_token":  access,
				"refresh_token": "FAKErefresh-" + access,
				"expires_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
		}
	}
	if err := set("FAKEaccessOLD")(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cp := codex.New(codex.Options{
		Name: "codex", Secrets: sec, HTTPClient: srv.Client(),
		AuthBaseURL: srv.URL, InferenceURL: srv.URL + "/responses",
	})
	d := &Daemon{App: &app.App{Codex: map[string]*codex.Provider{"codex": cp}}}

	if got := sent(cp); got != "Bearer FAKEaccessOLD" {
		t.Fatalf("authorization = %q, want the stored token", got)
	}
	if err := d.storeCredentials("codex", set("FAKEaccessNEW")); err != nil {
		t.Fatalf("store: %v", err)
	}
	if got := sent(cp); got != "Bearer FAKEaccessNEW" {
		t.Fatalf("authorization = %q, want the token just stored", got)
	}

	boom := errors.New("disk full")
	if err := d.storeCredentials("codex", func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the write error", err)
	}
	if got := sent(cp); got != "Bearer FAKEaccessNEW" {
		t.Fatalf("authorization = %q after a failed write, want the token in use", got)
	}

	ran := false
	if err := d.storeCredentials("ollama", func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("another provider: ran = %v, err = %v", ran, err)
	}
	if err := d.storeCredentials("ollama", func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("another provider: err = %v, want the write error", err)
	}
	if got := sent(cp); got != "Bearer FAKEaccessNEW" {
		t.Fatalf("authorization = %q after another provider's write", got)
	}
}
