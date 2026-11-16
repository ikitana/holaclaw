package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/store"
)

// gated is the API on a database with no owner yet, behind the bootstrap gate.
type gated struct {
	st *store.SQLite
	h  http.Handler
}

func newGated(t *testing.T, dataDir string) *gated {
	t.Helper()
	st, err := store.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := auth.New(st)
	b := &Backend{
		Config: testConfig(), Store: st, Auth: svc, Bootstrap: svc.Bootstrap(),
		Version: "test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return &gated{st: st, h: NewHandler(b)}
}

// do sends GET /v1/sessions, over the socket or with token, and returns the
// status and error code.
func (g *gated) do(t *testing.T, socket bool, token string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, BaseURL+"/sessions", nil)
	if socket {
		r = r.WithContext(markSocket(r.Context()))
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	g.h.ServeHTTP(w, r)
	var e gen.Error
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	return w.Code, e.Code
}

// Until `holaclaw init` runs every request is 503 not_initialised, before any
// token is looked at; once it has, from another process, the same handler
// authenticates as usual.
func TestGateUntilInit(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	g := newGated(t, dataDir)

	for _, c := range []struct {
		name   string
		socket bool
		token  string
	}{{"no token", false, ""}, {"bad token", false, "hb_a_nope"}, {"socket", true, ""}} {
		if code, errCode := g.do(t, c.socket, c.token); code != http.StatusServiceUnavailable || errCode != CodeNotInitialised {
			t.Fatalf("%s before init: %d %q, want 503 %q", c.name, code, errCode, CodeNotInitialised)
		}
	}

	// `holaclaw init` writes the owner through a store of its own.
	other, err := store.Open(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	boot, err := auth.New(other).CreateOwner(context.Background(), true, nil)
	secret := boot.Secret
	_ = other.Close()
	if err != nil {
		t.Fatal(err)
	}

	if code, errCode := g.do(t, false, ""); code != http.StatusUnauthorized {
		t.Fatalf("no token after init: %d %q, want 401", code, errCode)
	}
	if code, errCode := g.do(t, false, secret); code != http.StatusOK {
		t.Fatalf("init token after init: %d %q, want 200", code, errCode)
	}
	if code, errCode := g.do(t, true, ""); code != http.StatusOK {
		t.Fatalf("socket after init: %d %q, want 200", code, errCode)
	}
}

// A gate that cannot read the database answers 500, not 503: the way out is
// not `holaclaw init`.
func TestGateCheckFails(t *testing.T) {
	g := newGated(t, filepath.Join(t.TempDir(), "data"))
	if err := g.st.Close(); err != nil {
		t.Fatal(err)
	}
	if code, errCode := g.do(t, false, ""); code != http.StatusInternalServerError || errCode != CodeInternal {
		t.Fatalf("closed store: %d %q, want 500 %q", code, errCode, CodeInternal)
	}
}
