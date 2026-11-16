package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/connector/mcp/mcptest"
	"holaclaw/internal/connector/oauth/oauthtest"
)

// inspect posts url to the inspect endpoint as admin.
func (h *harness) inspect(url string, want int) gen.ConnectorInspection {
	h.t.Helper()
	return decode[gen.ConnectorInspection](h.t, h.do(http.MethodPost, "/connectors/inspect", h.adminToken,
		gen.ConnectorInspect{Url: url}), want)
}

// An open server answers its name and tools, as show lists them, and no
// auth metadata.
func TestConnectorInspectOpen(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectorsFrom([]mcptest.Tool{
		{Name: "search", Description: "Search the docs.", Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}},
		{Name: "drop", Description: "Drops it.", Annotations: &sdk.ToolAnnotations{DestructiveHint: ptr(true)}},
	}, nil)
	in := h.inspect(f.open.URL, http.StatusOK)
	if !in.Open || deref(in.ServerName) != "mcptest" || deref(in.ServerVersion) != "1.0.0" || len(in.Tools) != 2 {
		t.Fatalf("inspect = %+v", in)
	}
	drop, search := in.Tools[0], in.Tools[1]
	if drop.Name != "drop" || drop.Risk != "external" || !drop.AlwaysAsk || search.Name != "search" || search.Risk != "read" ||
		deref(search.Description) != "Search the docs." {
		t.Errorf("tools = %+v", in.Tools)
	}
	if in.AuthMetadata != nil || in.Identity != nil || in.AuthorizationServers != nil || in.ScopesSupported != nil {
		t.Errorf("an open server has auth fields: %+v", in)
	}
}

// A protected server answers its metadata and the identity a create would
// use: CIMD when advertised, else DCR, else a pre-registered client id.
// Nothing is registered and no token is asked for.
func TestConnectorInspectProtected(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	in := h.inspect(f.protected.URL, http.StatusOK)
	am := in.AuthMetadata
	if in.Open || len(in.Tools) != 0 || in.ServerName != nil || am == nil || am.Issuer != f.as.URL ||
		deref(am.Resource) != f.protected.URL || deref(am.TokenEndpoint) != f.as.URL+oauthtest.PathToken ||
		deref(am.RegistrationEndpoint) != f.as.URL+oauthtest.PathRegister || am.IdentityKind != nil || am.ClientId != nil {
		t.Fatalf("inspect = %+v, metadata %+v", in, am)
	}
	if strings.Join(deref(in.AuthorizationServers), " ") != f.as.URL ||
		strings.Join(deref(in.ScopesSupported), " ") != "repo issues:write" || deref(in.Identity) != "cimd" {
		t.Errorf("servers %v, scopes %v, identity %q", deref(in.AuthorizationServers), deref(in.ScopesSupported), deref(in.Identity))
	}

	f.as.ASM["client_id_metadata_document_supported"] = false
	if in = h.inspect(f.protected.URL, http.StatusOK); deref(in.Identity) != "dcr" {
		t.Errorf("dcr only: identity %q", deref(in.Identity))
	}
	delete(f.as.ASM, "registration_endpoint")
	in = h.inspect(f.protected.URL, http.StatusOK)
	if deref(in.Identity) != "pre-registered" || in.AuthMetadata == nil || in.AuthMetadata.RegistrationEndpoint != nil {
		t.Errorf("pre-registered only: %+v", in)
	}
	if n, m := f.as.Count(oauthtest.PathRegister), f.as.Count(oauthtest.PathToken); n != 0 || m != 0 {
		t.Errorf("inspect registered %d times, asked for %d tokens", n, m)
	}

	// Metadata that a create would refuse is the authorization server's side.
	f.as.ASM["code_challenge_methods_supported"] = []string{"plain"}
	errorBody(t, h.do(http.MethodPost, "/connectors/inspect", h.adminToken, gen.ConnectorInspect{Url: f.protected.URL}),
		http.StatusBadGateway, CodeInternal)
	f.as.ASM["code_challenge_methods_supported"] = []string{"S256"}
	f.as.ASM["issuer"] = "https://elsewhere.example"
	errorBody(t, h.do(http.MethodPost, "/connectors/inspect", h.adminToken, gen.ConnectorInspect{Url: f.protected.URL}),
		http.StatusBadGateway, CodeInternal)
}

func TestConnectorInspectRefused(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	for name, tc := range map[string]struct {
		body   any
		status int
		code   string
	}{
		"http, not loopback": {gen.ConnectorInspect{Url: "http://example.com/mcp"}, http.StatusBadRequest, CodeBadRequest},
		"not http":           {gen.ConnectorInspect{Url: "ftp://example.com/mcp"}, http.StatusBadRequest, CodeBadRequest},
		"user info":          {gen.ConnectorInspect{Url: "https://u:p@example.com/mcp"}, http.StatusBadRequest, CodeBadRequest},
		"no url":             {map[string]any{}, http.StatusBadRequest, CodeBadRequest},
		"unknown field":      {map[string]any{"url": f.open.URL, "id": "x"}, http.StatusBadRequest, CodeBadRequest},
		"not json":           {"[", http.StatusBadRequest, CodeBadRequest},
		"unreachable":        {gen.ConnectorInspect{Url: deadURL(t)}, http.StatusBadGateway, CodeInternal},
		"discovery fail":     {gen.ConnectorInspect{Url: otherOrigin(f.protected.URL)}, http.StatusBadGateway, CodeInternal},
	} {
		t.Run(name, func(t *testing.T) {
			errorBody(t, h.do(http.MethodPost, "/connectors/inspect", h.adminToken, tc.body), tc.status, tc.code)
		})
	}
}

// Inspect leaves no row, no connectors.toml, no auth.toml entry and no
// event: the first connector.status after it is the create that follows.
func TestConnectorInspectStoresNothing(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	s := h.stream(h.adminToken, "", "")
	authBefore, authErr := os.ReadFile(h.b.Secrets.Path())

	h.inspect(f.open.URL, http.StatusOK)
	h.inspect(f.protected.URL, http.StatusOK)
	errorBody(t, h.do(http.MethodPost, "/connectors/inspect", h.adminToken, gen.ConnectorInspect{Url: deadURL(t)}),
		http.StatusBadGateway, CodeInternal)

	if rows, err := h.st.ListConnectors(context.Background()); err != nil || len(rows) != 0 {
		t.Errorf("rows = %+v, %v", rows, err)
	}
	if entries, err := os.ReadDir(f.dir); err != nil || len(entries) != 0 {
		t.Errorf("config dir = %v, %v", entries, err)
	}
	authAfter, err := os.ReadFile(h.b.Secrets.Path())
	if string(authAfter) != string(authBefore) || (err == nil) != (authErr == nil) {
		t.Errorf("auth.toml changed: %q", authAfter)
	}
	if raw := readRaw(t, h.do(http.MethodGet, "/connectors", h.adminToken, nil), http.StatusOK); strings.TrimSpace(raw) != `{"connectors":[]}` {
		t.Errorf("list = %s", raw)
	}

	decode[gen.ConnectorStarted](t, h.do(http.MethodPost, "/connectors", h.adminToken,
		gen.ConnectorCreate{Id: "marker", Url: f.open.URL}), http.StatusOK)
	for {
		fr := s.next(t, 5*time.Second)
		if fr.Type != "connector.status" {
			continue
		}
		var e gen.Event
		if err := json.Unmarshal(fr.Data, &e); err != nil || e.Attrs == nil {
			t.Fatalf("event %s: %v", fr.Data, err)
		}
		if id, _ := (*e.Attrs)["id"].(string); id != "marker" {
			t.Errorf("an event before the create's: %s", fr.Data)
		}
		break
	}
}

// Only POST /connectors/inspect inspects: a connector named inspect is
// reached like any other.
func TestConnectorInspectIsNotAnID(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	decode[gen.ConnectorStarted](t, h.do(http.MethodPost, "/connectors", h.adminToken,
		gen.ConnectorCreate{Id: "inspect", Url: f.open.URL}), http.StatusOK)
	d := decode[gen.ConnectorDetail](t, h.do(http.MethodGet, "/connectors/inspect", h.adminToken, nil), http.StatusOK)
	if d.Connector.Id != "inspect" || d.Connector.State != "connected" {
		t.Errorf("GET /connectors/inspect = %+v", d)
	}
	errorBody(t, h.do(http.MethodPost, "/connectors/inspect/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: "s"}), http.StatusConflict, CodeConflict)
	if in := h.inspect(f.open.URL, http.StatusOK); !in.Open {
		t.Errorf("inspect = %+v", in)
	}
}

func TestConnectorInspectNeedsAdmin(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	for _, token := range []string{h.ownerToken} {
		errorBody(t, h.do(http.MethodPost, "/connectors/inspect", token, gen.ConnectorInspect{Url: f.open.URL}),
			http.StatusForbidden, CodeForbidden)
	}
	if n := len(f.open.Requests()); n != 0 {
		t.Errorf("a refused inspect reached the server %d times", n)
	}
}

func TestConnectorInspectNotImplemented(t *testing.T) {
	h := newHarness(t)
	errorBody(t, h.do(http.MethodPost, "/connectors/inspect", h.adminToken, gen.ConnectorInspect{Url: "http://127.0.0.1:1"}),
		http.StatusNotImplemented, CodeNotImplemented)
}
