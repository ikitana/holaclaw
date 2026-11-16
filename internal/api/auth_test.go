package api

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/auth"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

func TestAuthMatrix(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	t.Run("no token", func(t *testing.T) {
		errorBody(t, h.do(http.MethodGet, "/status", "", nil), http.StatusUnauthorized, CodeUnauthorized)
	})
	t.Run("malformed token", func(t *testing.T) {
		errorBody(t, h.do(http.MethodGet, "/status", "not-a-token", nil), http.StatusUnauthorized, CodeUnauthorized)
	})
	t.Run("unknown token", func(t *testing.T) {
		errorBody(t, h.do(http.MethodGet, "/status", "hb_c_deadbeef", nil), http.StatusUnauthorized, CodeUnauthorized)
	})
	t.Run("revoked token", func(t *testing.T) {
		ctx := context.Background()
		secret, tok, err := h.auth.CreateToken(ctx, "doomed", auth.ScopeClient, h.owner.ID)
		if err != nil {
			t.Fatalf("create token: %v", err)
		}
		if resp := h.do(http.MethodGet, "/status", secret, nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("fresh token: status = %d", resp.StatusCode)
		} else {
			_ = resp.Body.Close()
		}
		if err := h.auth.RevokeToken(ctx, auth.Principal{Role: store.RoleOwner}, tok.ID); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		errorBody(t, h.do(http.MethodGet, "/status", secret, nil), http.StatusUnauthorized, CodeUnauthorized)
	})
	t.Run("socket is admin", func(t *testing.T) {
		if rec := h.socket(http.MethodGet, "/identities", nil); rec.Code != http.StatusOK {
			t.Fatalf("socket on an admin op: status = %d, body %s", rec.Code, rec.Body)
		}
	})
	t.Run("a header cannot claim the socket", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, h.srv.URL+BaseURL+"/identities", nil)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("X-Socket", "true")
		resp, err := h.srv.Client().Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		errorBody(t, resp, http.StatusUnauthorized, CodeUnauthorized)
	})
	t.Run("unknown path", func(t *testing.T) {
		errorBody(t, h.do(http.MethodGet, "/nope", h.adminToken, nil), http.StatusNotFound, CodeNotFound)
	})
}

// probe is one request per operation, with the path parameters and the
// mandatory query parameters filled in, so only the scope decides the answer.
type probe struct {
	op     string
	method string
	path   string
}

func probes(t *testing.T) []probe {
	t.Helper()
	id := string(event.NewID())
	out := make([]probe, 0, len(routes))
	for r, op := range routes {
		path := strings.NewReplacer("{id}", id, "{name}", "tg-main", "{provider}", "ollama", "{agent}", "main").Replace(r.Path)
		switch op {
		case "searchMemory":
			path += "?agent=main&partition=p"
		case "listMemoryPartitions", "listSkills", "getSkill":
			path += "?agent=main"
		case "forgetMemory":
			path += "?agent=main&partition=p"
		case "removeSkill", "restoreSkill", "revertSkill":
			path += "?agent=main&space=dm/p"
		case "streamBrowser":
			path += "?ticket=t"
		}
		out = append(out, probe{op: op, method: r.Method, path: path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].op < out[j].op })
	return out
}

// TestScopeMatrix drives every operation from the scopes table with each of
// the two scopes: the gate answers 403 exactly when the scope does not
// allow the operation, and never otherwise.
func TestScopeMatrix(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	holders := []struct {
		name  string
		token string
		scope auth.Scope
	}{
		{"client", h.ownerToken, auth.ScopeClient},
		{"admin", h.adminToken, auth.ScopeAdmin},
	}
	for _, p := range probes(t) {
		required, ok := scopes[p.op]
		if !ok {
			t.Fatalf("no scope for %s", p.op)
		}
		for _, holder := range holders {
			t.Run(p.op+"/"+holder.name, func(t *testing.T) {
				token := holder.token
				if p.op == "rotateOwnToken" {
					// Rotating revokes the token that asked: a throwaway one of
					// the same scope keeps the holder's for the other probes.
					token = h.token("rotated-"+holder.name, holder.scope, h.admin.ID)
				}
				status := h.status(t, p, token)
				forbidden := status == http.StatusForbidden
				want := !holder.scope.Allows(required)
				if p.op == "streamBrowser" {
					// The one operation the spec gives `security: []`: a
					// browser cannot set an Authorization header on a
					// WebSocket, so the ticket in the query is the whole
					// credential and the scope gate steps aside for it. The
					// probe carries a bogus ticket, so every scope gets the
					// handler's answer (401 here, or 501 in a build without a
					// browser backend) and never a 403.
					want = false
				}
				if forbidden != want {
					t.Fatalf("%s %s with %s scope: status %d, forbidden=%v want %v (required %s)",
						p.method, p.path, holder.scope, status, forbidden, want, required)
				}
			})
		}
	}
}

// status runs one probe and returns the status code. The event stream is cut
// short: the scope decision is taken before the first byte.
func (h *harness) status(t *testing.T, p probe, token string) int {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if p.op == "streamEvents" {
		time.AfterFunc(100*time.Millisecond, cancel)
	}
	req, err := http.NewRequestWithContext(ctx, p.method, h.srv.URL+BaseURL+p.path, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if p.op == "streamEvents" {
		// Drain until the server closes, so the connection is not reused half
		// read by the next probe.
		buf := make([]byte, 64)
		for {
			if _, err := resp.Body.Read(buf); err != nil {
				break
			}
		}
	}
	return resp.StatusCode
}

// TestScopesMatchSpec is the drift guard: scopes are part of the contract, so
// the table in this package must equal `x-scope` in api/openapi.yaml, and the
// route table must equal the spec's paths.
func TestScopesMatchSpec(t *testing.T) {
	t.Parallel()
	specScopes, specRoutes := parseSpec(t, "../../api/openapi.yaml")

	if len(specScopes) != len(scopes) {
		t.Errorf("spec has %d operations, the scopes table %d", len(specScopes), len(scopes))
	}
	for op, want := range specScopes {
		got, ok := scopes[op]
		switch {
		case !ok:
			t.Errorf("operation %q is in the spec but not in the scopes table", op)
		case got != want:
			t.Errorf("operation %q: table says %q, the spec says %q", op, got, want)
		}
	}
	for op := range scopes {
		if _, ok := specScopes[op]; !ok {
			t.Errorf("operation %q is in the scopes table but not in the spec", op)
		}
	}

	if len(specRoutes) != len(routes) {
		t.Errorf("spec has %d routes, the route table %d", len(specRoutes), len(routes))
	}
	for r, want := range specRoutes {
		got, ok := routes[r]
		switch {
		case !ok:
			t.Errorf("route %s %s is in the spec but not in the route table", r.Method, r.Path)
		case got != want:
			t.Errorf("route %s %s: table says %q, the spec says %q", r.Method, r.Path, got, want)
		}
	}
	for r := range routes {
		if _, ok := specRoutes[r]; !ok {
			t.Errorf("route %s %s is in the route table but not in the spec", r.Method, r.Path)
		}
	}
}

// parseSpec reads operationId, x-scope and the path of every operation with a
// line scanner: the spec is the contract, and this package carries no YAML
// dependency to read it with.
func parseSpec(t *testing.T, path string) (map[string]auth.Scope, map[route]string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open spec: %v", err)
	}
	defer func() { _ = f.Close() }()

	var (
		inPaths     bool
		currentPath string
		method      string
		op          string
		opScopes    = map[string]auth.Scope{}
		opRoutes    = map[route]string{}
	)
	methods := map[string]bool{"get": true, "post": true, "put": true, "delete": true, "patch": true}

	flush := func() {
		if op == "" || method == "" || currentPath == "" {
			return
		}
		opRoutes[route{Method: strings.ToUpper(method), Path: currentPath}] = op
		op, method = "", ""
	}

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == 0 {
			flush()
			inPaths = trimmed == "paths:"
			currentPath = ""
			continue
		}
		if !inPaths {
			continue
		}
		switch {
		case indent == 2 && strings.HasPrefix(trimmed, "/") && strings.HasSuffix(trimmed, ":"):
			flush()
			currentPath = strings.TrimSuffix(trimmed, ":")
		case indent == 4 && strings.HasSuffix(trimmed, ":") && methods[strings.TrimSuffix(trimmed, ":")]:
			flush()
			method = strings.TrimSuffix(trimmed, ":")
		case indent == 6 && strings.HasPrefix(trimmed, "operationId:"):
			op = strings.TrimSpace(strings.TrimPrefix(trimmed, "operationId:"))
		case indent == 6 && strings.HasPrefix(trimmed, "x-scope:"):
			opScopes[op] = auth.Scope(strings.TrimSpace(strings.TrimPrefix(trimmed, "x-scope:")))
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		t.Fatalf("scan spec: %v", err)
	}
	if len(opScopes) == 0 {
		t.Fatal("parsed no operations from the spec")
	}
	for op, s := range opScopes {
		if !s.Valid() {
			t.Fatalf("operation %q declares an unknown scope %q", op, s)
		}
	}
	return opScopes, opRoutes
}
