// Package oauthtest is a fake protected resource and authorization server
// on one TLS listener for tests: resource metadata at the path form, AS
// metadata at the RFC 8414 root, and the registration and token endpoints.
// It records every request. Fields tune it while no request is running;
// Handle changes a route while requests may be running.
package oauthtest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Paths the server answers on.
const (
	PathPRM       = "/.well-known/oauth-protected-resource/mcp"
	PathASM       = "/.well-known/oauth-authorization-server"
	PathAuthorize = "/authorize" // listed in the metadata; nothing answers it: tests build the redirect themselves
	PathRegister  = "/register"
	PathToken     = "/token"
)

// Server is a running fake. New closes it with t.Cleanup.
type Server struct {
	*httptest.Server
	// PRM is the protected resource metadata; its resource defaults to
	// ServerURL. ASM is the authorization server metadata, CIMD advertised.
	PRM map[string]any
	ASM map[string]any
	// Routes override any path. PathRegister answers client id
	// "dcr-client-1" and PathToken a fixed token pair by default.
	Routes map[string]http.HandlerFunc

	mu   sync.Mutex
	reqs map[string][]Request
}

// Request is one request as the server saw it. Form is set for a form body.
type Request struct {
	Method string
	Header http.Header
	Form   url.Values
	Body   []byte
}

// New starts a server and registers its Close with t.Cleanup.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{Routes: map[string]http.HandlerFunc{}, reqs: map[string][]Request{}}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	base := s.URL
	s.PRM = map[string]any{
		"resource":              base + "/mcp",
		"authorization_servers": []string{base},
		"scopes_supported":      []string{"repo", "issues:write"},
	}
	s.ASM = map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + PathAuthorize,
		"token_endpoint":                        base + PathToken,
		"registration_endpoint":                 base + PathRegister,
		"code_challenge_methods_supported":      []string{"S256"},
		"client_id_metadata_document_supported": true,
	}
	s.Routes[PathRegister] = JSONHandler(http.StatusCreated, `{"client_id":"dcr-client-1"}`)
	s.Routes[PathToken] = JSONHandler(http.StatusOK, `{"access_token":"FAKEaccess1","token_type":"Bearer","refresh_token":"FAKErefresh1","expires_in":3600,"scope":"repo"}`)
	return s
}

// Handle replaces the handler of path; safe while requests run.
func (s *Server) Handle(path string, h http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Routes[path] = h
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body)) // routes read it again
	rec := Request{Method: r.Method, Header: r.Header.Clone(), Body: body}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		rec.Form, _ = url.ParseQuery(string(body))
	}
	s.mu.Lock()
	s.reqs[r.URL.Path] = append(s.reqs[r.URL.Path], rec)
	h, ok := s.Routes[r.URL.Path]
	s.mu.Unlock()

	if ok {
		h(w, r)
		return
	}
	switch r.URL.Path {
	case PathPRM:
		WriteJSON(w, s.PRM)
	case PathASM:
		WriteJSON(w, s.ASM)
	default:
		http.NotFound(w, r)
	}
}

// ServerURL is the default protected resource, the PRM's resource.
func (s *Server) ServerURL() string { return s.URL + "/mcp" }

// Header is a 401's WWW-Authenticate naming the resource metadata.
func (s *Server) Header() string {
	return `Bearer resource_metadata="` + s.URL + PathPRM + `"`
}

// Count is the number of requests seen for path.
func (s *Server) Count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs[path])
}

// Last is the latest request for path; none fails the test.
func (s *Server) Last(t testing.TB, path string) Request {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.reqs[path]
	if len(rs) == 0 {
		t.Fatalf("no request to %s", path)
	}
	return rs[len(rs)-1]
}

// WriteJSON answers v as JSON with status 200.
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// JSONHandler always answers body with status.
func JSONHandler(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}
