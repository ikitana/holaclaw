package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
	"holaclaw/internal/connector"
	"holaclaw/internal/connector/oauth/oauthtest"
	"holaclaw/internal/store"
)

// deleteConnector sends DELETE /connectors/{id} with token and checks the
// status; 204 has no body.
func (h *harness) deleteConnector(id, token string, want int) {
	h.t.Helper()
	resp := h.do(http.MethodDelete, "/connectors/"+id, token, nil)
	codes := map[int]string{
		http.StatusNotFound: CodeNotFound, http.StatusForbidden: CodeForbidden, http.StatusNotImplemented: CodeNotImplemented,
	}
	if want != http.StatusNoContent {
		errorBody(h.t, resp, want, codes[want])
		return
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || len(raw) != 0 {
		h.t.Fatalf("delete %s = %d %q", id, resp.StatusCode, raw)
	}
}

// connectorsFile is connectors.toml as written, "" when there is none.
func connectorsFile(t testing.TB, f connectorFakes) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, config.FileConnectors))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(raw)
}

// connectorGone asserts nothing of id is left: no row, no auth.toml key,
// no connectors.toml entry, and the API answers 404.
func (h *harness) connectorGone(f connectorFakes, id string) {
	h.t.Helper()
	if _, err := h.st.GetConnector(context.Background(), id); !errors.Is(err, store.ErrNotFound) {
		h.t.Errorf("%s: row left: %v", id, err)
	}
	if keys := h.b.Secrets.Keys(secrets.Connectors, id); len(keys) != 0 {
		h.t.Errorf("%s: auth.toml keys left: %v", id, keys)
	}
	if strings.Contains(connectorsFile(h.t, f), "[connectors."+id+"]") {
		h.t.Errorf("%s: connectors.toml entry left", id)
	}
	errorBody(h.t, h.do(http.MethodGet, "/connectors/"+id, h.adminToken, nil), http.StatusNotFound, CodeNotFound)
}

// awaitDeleted reads the stream up to id's connector.status "deleted",
// failing on a later event of id.
func awaitDeleted(t *testing.T, s *rawStream, id string) {
	t.Helper()
	for {
		fr := s.next(t, 5*time.Second)
		if fr.Type != "connector.status" {
			continue
		}
		var e gen.Event
		if err := json.Unmarshal(fr.Data, &e); err != nil || e.Attrs == nil {
			t.Fatalf("event %s: %v", fr.Data, err)
		}
		if got, _ := (*e.Attrs)["id"].(string); got != id {
			continue
		}
		if state, _ := (*e.Attrs)["state"].(string); state == "deleted" {
			return
		}
	}
}

func TestConnectorDeleteConnected(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	st := h.createConnector(f, nil)
	decode[gen.Connector](t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: stateOf(t, st)}), http.StatusAccepted)
	// Everything is there before, so the checks after can fail.
	if _, err := h.st.GetConnector(context.Background(), "gh"); err != nil {
		t.Fatalf("row: %v", err)
	}
	if len(h.b.Secrets.Keys(secrets.Connectors, "gh")) == 0 {
		t.Fatal("no auth.toml entry")
	}
	if !strings.Contains(connectorsFile(t, f), "[connectors.gh]") {
		t.Fatalf("connectors.toml = %q", connectorsFile(t, f))
	}

	s := h.stream(h.adminToken, "", "")
	h.deleteConnector("gh", h.adminToken, http.StatusNoContent)
	awaitDeleted(t, s, "gh")
	h.connectorGone(f, "gh")
	if raw := readRaw(t, h.do(http.MethodGet, "/connectors", h.adminToken, nil), http.StatusOK); strings.TrimSpace(raw) != `{"connectors":[]}` {
		t.Errorf("list = %s", raw)
	}
	h.deleteConnector("gh", h.adminToken, http.StatusNotFound)
}

// A delete during an open create drops the authorization: its code finds
// no connector.
func TestConnectorDeleteDuringCreate(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	st := h.createConnector(f, nil)
	s := h.stream(h.adminToken, "", "")
	h.deleteConnector("gh", h.adminToken, http.StatusNoContent)
	awaitDeleted(t, s, "gh")
	h.connectorGone(f, "gh")
	errorBody(t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: stateOf(t, st)}), http.StatusNotFound, CodeNotFound)
	if n := f.as.Count(oauthtest.PathToken); n != 0 {
		t.Errorf("the dropped code was exchanged %d times", n)
	}
}

// A delete during a reconnect removes the connector and its old tokens;
// the reconnect's code finds no connector.
func TestConnectorDeleteDuringReconnect(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	st := h.createConnector(f, nil)
	decode[gen.Connector](t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: stateOf(t, st)}), http.StatusAccepted)
	re := decode[gen.ConnectorStarted](t, h.do(http.MethodPost, "/connectors/gh/reconnect", h.adminToken,
		gen.ConnectorReconnect{RedirectUri: ptr(connRedirect)}), http.StatusAccepted)

	s := h.stream(h.adminToken, "", "")
	h.deleteConnector("gh", h.adminToken, http.StatusNoContent)
	awaitDeleted(t, s, "gh")
	h.connectorGone(f, "gh")
	errorBody(t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: stateOf(t, re)}), http.StatusNotFound, CodeNotFound)
}

// An open server, and one named inspect: DELETE reaches it like any id.
func TestConnectorDeleteOpen(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	for _, id := range []string{"docs", "inspect"} {
		decode[gen.ConnectorStarted](t, h.do(http.MethodPost, "/connectors", h.adminToken,
			gen.ConnectorCreate{Id: id, Url: f.open.URL}), http.StatusOK)
	}
	s := h.stream(h.adminToken, "", "")
	for _, id := range []string{"docs", "inspect"} {
		h.deleteConnector(id, h.adminToken, http.StatusNoContent)
		awaitDeleted(t, s, id)
		h.connectorGone(f, id)
	}
	if strings.Contains(connectorsFile(t, f), "[connectors.") {
		t.Errorf("connectors.toml = %q", connectorsFile(t, f))
	}
}

// An entry of connectors.toml at start, never connected: deleted too.
func TestConnectorDeleteFromFile(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectorsFrom(nil, func(f connectorFakes) map[string]config.Connector {
		return map[string]config.Connector{"wiki": {ID: "wiki", URL: f.open.URL, Auth: "none"}}
	})
	h.deleteConnector("wiki", h.adminToken, http.StatusNoContent)
	h.connectorGone(f, "wiki")
}

func TestConnectorDeleteRefused(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	h.deleteConnector("nope", h.adminToken, http.StatusNotFound)
	h.deleteConnector("Not-Valid", h.adminToken, http.StatusNotFound)

	decode[gen.ConnectorStarted](t, h.do(http.MethodPost, "/connectors", h.adminToken,
		gen.ConnectorCreate{Id: "docs", Url: f.open.URL}), http.StatusOK)
	for _, token := range []string{h.ownerToken} {
		h.deleteConnector("docs", token, http.StatusForbidden)
	}
	if _, err := h.st.GetConnector(context.Background(), "docs"); err != nil {
		t.Errorf("a refused delete removed the row: %v", err)
	}
}

// symlinkConnectors moves connectors.toml elsewhere, an empty one when
// there is none, and links it back.
func symlinkConnectors(t *testing.T, f connectorFakes) {
	t.Helper()
	path := filepath.Join(f.dir, config.FileConnectors)
	target := filepath.Join(t.TempDir(), "elsewhere.toml")
	if err := os.Rename(path, target); errors.Is(err, os.ErrNotExist) {
		err = os.WriteFile(target, []byte("version = 1\n"), 0o600)
		if err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}

// wantConflict asserts a 409 with exactly msg.
func wantConflict(t *testing.T, resp *http.Response, msg string) {
	t.Helper()
	e := decode[gen.Error](t, resp, http.StatusConflict)
	if e.Code != CodeConflict || e.Message != msg {
		t.Errorf("error = %+v, want %q", e, msg)
	}
}

// A symlinked connectors.toml is the person's: 409 with what to do, and
// nothing removed.
func TestConnectorDeleteSymlinkedFile(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	decode[gen.ConnectorStarted](t, h.do(http.MethodPost, "/connectors", h.adminToken,
		gen.ConnectorCreate{Id: "docs", Url: f.open.URL}), http.StatusOK)
	symlinkConnectors(t, f)
	wantConflict(t, h.do(http.MethodDelete, "/connectors/docs", h.adminToken, nil),
		"connector: connectors.toml is a symlink the daemon does not write: remove [connectors.docs] there by hand, then delete again")
	if _, err := h.st.GetConnector(context.Background(), "docs"); err != nil {
		t.Errorf("row removed: %v", err)
	}
	decode[gen.ConnectorDetail](t, h.do(http.MethodGet, "/connectors/docs", h.adminToken, nil), http.StatusOK)
}

// A create over a symlinked connectors.toml keeps nothing, the tokens of
// its code included, and says how to add the connector by hand.
func TestConnectorCreateSymlinkedFile(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	symlinkConnectors(t, f)
	const created = "connector: connectors.toml is a symlink the daemon does not write: nothing was kept; " +
		"add [connectors.%[1]s] there by hand, restart the daemon, then reconnect %[1]s"
	wantConflict(t, h.do(http.MethodPost, "/connectors", h.adminToken,
		gen.ConnectorCreate{Id: "docs", Url: f.open.URL}), fmt.Sprintf(created, "docs"))
	errorBody(t, h.do(http.MethodGet, "/connectors/docs", h.adminToken, nil), http.StatusNotFound, CodeNotFound)

	st := h.createConnector(f, nil)
	wantConflict(t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: stateOf(t, st)}), fmt.Sprintf(created, "gh"))
	if n := f.as.Count(oauthtest.PathToken); n != 1 {
		t.Errorf("token requests = %d", n)
	}
	h.connectorGone(f, "gh")
}

// A reconnect over a symlinked connectors.toml works while the entry stays
// as it is; one with new scopes keeps the new tokens and says the scopes
// are to be set by hand.
func TestConnectorReconnectSymlinkedFile(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	st := h.createConnector(f, nil)
	decode[gen.Connector](t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: stateOf(t, st)}), http.StatusAccepted)
	symlinkConnectors(t, f)
	before := connectorsFile(t, f)

	re := decode[gen.ConnectorStarted](t, h.do(http.MethodPost, "/connectors/gh/reconnect", h.adminToken,
		gen.ConnectorReconnect{RedirectUri: ptr(connRedirect)}), http.StatusAccepted)
	c := decode[gen.Connector](t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: stateOf(t, re)}), http.StatusAccepted)
	if c.State != "connected" {
		t.Errorf("after the reconnect: %+v", c)
	}

	re = decode[gen.ConnectorStarted](t, h.do(http.MethodPost, "/connectors/gh/reconnect", h.adminToken,
		gen.ConnectorReconnect{RedirectUri: ptr(connRedirect), Scopes: &[]string{"repo"}}), http.StatusAccepted)
	wantConflict(t, h.do(http.MethodPost, "/connectors/gh/code", h.adminToken,
		gen.ConnectorCode{Code: "FAKEcode", State: stateOf(t, re)}),
		"connector: connectors.toml is a symlink the daemon does not write: the new sign-in is stored, "+
			"but [connectors.gh] keeps its old scopes; set them there by hand and restart the daemon")
	if v, ok := h.b.Secrets.Get(secrets.Connectors, "gh", secrets.ConnectorAccessToken); !ok || v != "FAKEaccess1" {
		t.Error("the new tokens are not stored")
	}
	if got := connectorsFile(t, f); got != before {
		t.Errorf("connectors.toml = %q, want %q", got, before)
	}
	d := decode[gen.ConnectorDetail](t, h.do(http.MethodGet, "/connectors/gh", h.adminToken, nil), http.StatusOK)
	if d.Connector.State != "connected" || d.Connector.Scopes != nil {
		t.Errorf("after the refused reconnect: %+v", d.Connector)
	}
}

// A connectors.toml that does not parse refuses a delete, of a known id or
// not: 409 with the daemon's text, nothing of the file quoted.
func TestConnectorDeleteBrokenFile(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	decode[gen.ConnectorStarted](t, h.do(http.MethodPost, "/connectors", h.adminToken,
		gen.ConnectorCreate{Id: "docs", Url: f.open.URL}), http.StatusOK)
	if err := os.WriteFile(filepath.Join(f.dir, config.FileConnectors), []byte("this is = = not toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"docs", "nope"} {
		wantConflict(t, h.do(http.MethodDelete, "/connectors/"+id, h.adminToken, nil),
			"connector: connectors.toml does not parse; fix it by hand")
	}
	decode[gen.ConnectorDetail](t, h.do(http.MethodGet, "/connectors/docs", h.adminToken, nil), http.StatusOK)
}

// failingDeletes is the store with DeleteConnector failing while fail is
// set.
type failingDeletes struct {
	store.ConnectorStore
	fail atomic.Bool
}

func (s *failingDeletes) DeleteConnector(ctx context.Context, id string) error {
	if s.fail.Load() {
		return errors.New("store: disk full")
	}
	return s.ConnectorStore.DeleteConnector(ctx, id)
}

// A delete whose cache row cannot be removed: the connector is gone and
// announced deleted, the 500 says to delete again before creating the id
// again, and the retry removes the row.
func TestConnectorDeleteLeftovers(t *testing.T) {
	h := newHarness(t)
	f := h.withConnectors()
	fs := &failingDeletes{ConnectorStore: h.st}
	m, err := connector.New(context.Background(), connector.Options{
		Dir: f.dir, Store: fs, Secrets: h.b.Secrets, Events: h.sink, HTTPClient: f.as.Client(), Logger: h.b.Logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	h.b.Connectors = m
	decode[gen.ConnectorStarted](t, h.do(http.MethodPost, "/connectors", h.adminToken,
		gen.ConnectorCreate{Id: "docs", Url: f.open.URL}), http.StatusOK)

	fs.fail.Store(true)
	s := h.stream(h.adminToken, "", "")
	e := decode[gen.Error](t, h.do(http.MethodDelete, "/connectors/docs", h.adminToken, nil), http.StatusInternalServerError)
	if e.Code != CodeInternal || e.Message != "connector: deleted, but removing its sign-in or cache failed; delete docs again before creating it again" {
		t.Errorf("error = %+v", e)
	}
	awaitDeleted(t, s, "docs")
	errorBody(t, h.do(http.MethodGet, "/connectors/docs", h.adminToken, nil), http.StatusNotFound, CodeNotFound)
	if _, err := h.st.GetConnector(context.Background(), "docs"); err != nil {
		t.Fatalf("the row is not left: %v", err)
	}

	fs.fail.Store(false)
	h.deleteConnector("docs", h.adminToken, http.StatusNoContent)
	h.connectorGone(f, "docs")
	h.deleteConnector("docs", h.adminToken, http.StatusNotFound)
}

func TestConnectorDeleteNotImplemented(t *testing.T) {
	h := newHarness(t)
	h.deleteConnector("gh", h.adminToken, http.StatusNotImplemented)
}
