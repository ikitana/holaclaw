package oauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"holaclaw/internal/connector/oauth/oauthtest"
)

// fake is the oauthtest server under the short names these tests use. The
// maps are the server's own, so writing them tunes it.
type fake struct {
	*oauthtest.Server
	srv    *httptest.Server
	prm    map[string]any
	asm    map[string]any
	routes map[string]http.HandlerFunc
}

const (
	pathPRM      = oauthtest.PathPRM
	pathASM      = oauthtest.PathASM
	pathRegister = oauthtest.PathRegister
	pathToken    = oauthtest.PathToken
)

var (
	writeJSON   = oauthtest.WriteJSON
	jsonHandler = oauthtest.JSONHandler
)

func newFake(t *testing.T) *fake {
	t.Helper()
	s := oauthtest.New(t)
	return &fake{Server: s, srv: s.Server, prm: s.PRM, asm: s.ASM, routes: s.Routes}
}

func (f *fake) client() *Client {
	return &Client{HTTP: f.srv.Client(), now: func() time.Time { return testNow }}
}

func (f *fake) serverURL() string { return f.ServerURL() }

func (f *fake) header() string { return f.Header() }

func (f *fake) count(path string) int { return f.Count(path) }

func (f *fake) last(t *testing.T, path string) oauthtest.Request {
	t.Helper()
	return f.Last(t, path)
}

var testNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
