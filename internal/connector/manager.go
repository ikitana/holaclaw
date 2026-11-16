// Package connector owns every connector end to end (docs/connectors.md §2,
// §7): the state machine kept in the store's cache row, the open OAuth
// intents in memory, the tokens in auth.toml, the entries of
// connectors.toml, one MCP client per connector, token refresh and the
// health check. Every state change is a connector.status event. Tokens,
// codes, the PKCE verifier and client secrets never reach an event, an
// error or a log, and no error carries server text.
//
// The API handlers are thin wrappers over Create, Code, Reconnect, List,
// Get, Inspect and Delete. Executor is an agent's connector tools, over
// Tools and CallTool. The daemon calls Check once at start and then every
// CheckInterval.
package connector

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
	"holaclaw/internal/connector/mcp"
	"holaclaw/internal/connector/oauth"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

const (
	// IntentTTL is how long an authorization stays open.
	IntentTTL = 15 * time.Minute
)

// Options configure New.
type Options struct {
	// Dir is the config directory; connectors.toml is written there.
	Dir string
	// Connectors is connectors.toml as loaded at start (config.Config).
	Connectors map[string]config.Connector
	Store      store.ConnectorStore
	Secrets    *secrets.File
	// Events receives connector.status and auth.refreshed; nil drops them.
	Events event.Sink
	// HTTPClient sends every MCP and OAuth request; nil is the default.
	HTTPClient *http.Client
	// Now is the clock of intents, due refreshes and checks; nil is
	// time.Now. Token expiries come from the OAuth client's wall clock, so
	// a test clock starts at time.Now and moves forward.
	Now    func() time.Time
	Logger *slog.Logger
}

// Manager runs the connectors. Safe for concurrent use.
type Manager struct {
	o     Options
	oauth *oauth.Client
	log   *slog.Logger

	ctx    context.Context // cancelled by Close: background tool refreshes
	cancel context.CancelFunc
	closed atomic.Bool // set by Close before any client is closed

	listInterval time.Duration // listChangedInterval; tests shorten it

	fileMu sync.Mutex // connectors.toml, with the flock

	mu      sync.Mutex // the maps; never held while taking a conn's mu
	conns   map[string]*conn
	refused map[string]bool // issuers that refused the metadata document
}

// conn is one connector. Lock order: refreshMu, then mu, then Manager.mu.
// Network calls never run under mu.
type conn struct {
	id        string
	refreshMu sync.Mutex // serializes refreshes and the code exchange

	mu       sync.Mutex
	row      store.Connector // the cache row as last written
	tools    []mcp.Tool      // row.Tools, parsed
	cfg      config.Connector
	inConfig bool // false while a create is open
	intent   *intent
	client   *mcp.Client
	gen      uint64 // bumped by every token change
	deleted  bool

	refreshFailed time.Time   // last failed refresh other than a dead grant
	lastList      time.Time   // last tools/list after list_changed
	listTimer     *time.Timer // a trailing list_changed refresh is due
}

// New loads every connector of o.Connectors with its cache row. It makes
// no network call: an entry without a row starts pending (auth = "none")
// or expired with DetailNeedsReconnect (auth = "oauth", no credentials
// yet); a row with no entry is removed.
func New(ctx context.Context, o Options) (*Manager, error) {
	if o.Dir == "" || o.Store == nil || o.Secrets == nil {
		return nil, errors.New("connector: Dir, Store and Secrets are required")
	}
	if o.Events == nil {
		o.Events = event.Discard
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	m := &Manager{
		o: o, oauth: &oauth.Client{HTTP: o.HTTPClient}, log: o.Logger,
		listInterval: listChangedInterval,
		conns:        map[string]*conn{}, refused: map[string]bool{},
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	rows, err := o.Store.ListConnectors(ctx)
	if err != nil {
		return nil, fmt.Errorf("connector: %w", err)
	}
	byID := map[string]store.Connector{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	for id, cfg := range o.Connectors {
		if !config.ValidConnectorID(id) {
			continue // CFG011
		}
		row, ok := byID[id]
		delete(byID, id)
		if !ok {
			row = store.Connector{ID: id}
		}
		c := &conn{id: id, row: row, cfg: cfg, inConfig: true}
		c.tools = parseTools(row.Tools)
		state, detail := m.startState(c)
		// A row cached before the description cap is cut by parseTools:
		// its detail says so from the start, not from the first check.
		detail = toolsDetail(state, detail, c.tools)
		if row.State != state || row.Detail != detail || !ok {
			if err := m.updateLocked(ctx, c, func(r *store.Connector) { r.State, r.Detail = state, detail }); err != nil {
				return nil, err
			}
		}
		m.conns[id] = c
	}
	for id := range byID {
		if err := o.Store.DeleteConnector(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("connector: %w", err)
		}
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m, nil
}

// startState is where a loaded connector starts. An intent never survives
// a restart, so pending and authorizing fall back to pending for the first
// check to settle.
func (m *Manager) startState(c *conn) (store.ConnectorState, string) {
	r := c.row
	if c.cfg.Auth == "oauth" {
		am := parseAuthMeta(r.AuthMeta)
		tok, _ := m.o.Secrets.Get(secrets.Connectors, c.id, secrets.ConnectorRefreshToken)
		if access, _ := m.o.Secrets.Get(secrets.Connectors, c.id, secrets.ConnectorAccessToken); access != "" {
			tok = access
		}
		switch {
		case am == nil || tok == "":
			return store.ConnectorExpired, DetailNeedsReconnect
		case !sameURL(cmp.Or(am.ServerURL, am.Metadata.Resource), c.cfg.URL):
			return store.ConnectorExpired, DetailURLChanged
		}
	}
	switch r.State {
	case store.ConnectorConnected, store.ConnectorDegraded, store.ConnectorExpired:
		return r.State, r.Detail
	}
	return store.ConnectorPending, ""
}

// Close stops every MCP client. The manager is unusable afterwards.
func (m *Manager) Close() error {
	m.closed.Store(true)
	m.cancel()
	for _, c := range m.snapshot() {
		c.mu.Lock()
		cl := c.client
		c.client = nil
		if c.listTimer != nil {
			c.listTimer.Stop()
		}
		c.mu.Unlock()
		if cl != nil {
			_ = cl.Close()
		}
	}
	return nil
}

func (m *Manager) now() time.Time { return m.o.Now() }

func (m *Manager) conn(id string) (*conn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[id]
	if !ok {
		return nil, ErrNotFound
	}
	return c, nil
}

func (m *Manager) snapshot() []*conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*conn, 0, len(m.conns))
	for _, c := range m.conns {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b *conn) int { return strings.Compare(a.id, b.id) })
	return out
}

// Connector is one connector as the API shows it.
type Connector struct {
	ID       string
	URL      string
	Auth     string   // "none" | "oauth"
	Scopes   []string // requested, as in connectors.toml; nil = all offered
	ClientID string   // pre-registered client id
	Trusted  bool

	State         store.ConnectorState
	Detail        string
	ServerName    string
	ServerVersion string
	GrantedScopes []string
	Tools         []mcp.Tool
	// Metadata is the discovered auth metadata; nil for an open server.
	Metadata     *oauth.Metadata
	IdentityKind oauth.IdentityKind
	// GrantedClientID is the client id the tokens were granted to, whatever
	// its kind; empty before a grant.
	GrantedClientID string
	CIMDRefused     bool
	LastCheck       *time.Time
	TokenExpiry     *time.Time
	Failures        int
}

// List returns every connector, by id. It does not fail today; the error
// keeps it in line with Get.
func (m *Manager) List(ctx context.Context) ([]Connector, error) {
	var out []Connector
	for _, c := range m.snapshot() {
		c.mu.Lock()
		m.sweepLocked(ctx, c)
		if !c.deleted {
			out = append(out, c.viewLocked())
		}
		c.mu.Unlock()
	}
	return out, nil
}

// Get returns one connector.
func (m *Manager) Get(ctx context.Context, id string) (Connector, error) {
	c, err := m.conn(id)
	if err != nil {
		return Connector{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m.sweepLocked(ctx, c)
	if c.deleted {
		return Connector{}, ErrNotFound
	}
	return c.viewLocked(), nil
}

func (c *conn) viewLocked() Connector {
	cfg := c.cfg
	if in := c.intent; in != nil && !c.inConfig {
		cfg = in.cfg
	}
	v := Connector{
		ID: c.id, URL: cfg.URL, Auth: cfg.Auth, Scopes: slices.Clone(cfg.Scopes), ClientID: cfg.ClientID, Trusted: cfg.Trusted,
		State: c.row.State, Detail: c.row.Detail, ServerName: c.row.ServerName, ServerVersion: c.row.ServerVersion,
		GrantedScopes: slices.Clone(c.row.Scopes), Tools: slices.Clone(c.tools), CIMDRefused: c.row.CIMDRefused,
		LastCheck: c.row.LastCheck, TokenExpiry: c.row.TokenExpiry, Failures: c.row.Failures,
	}
	if am := parseAuthMeta(c.row.AuthMeta); am != nil {
		md := am.Metadata
		v.Metadata, v.IdentityKind, v.GrantedClientID = &md, am.IdentityKind, c.row.ClientID
	}
	return v
}

// Delete removes a connector in any state: its connectors.toml entry, its
// auth.toml entry and its cache row, then emits state "deleted". An open
// intent is dropped. An id known nowhere is ErrNotFound, and so is one
// another Delete or a failed create removed while this one waited.
//
// The entry goes first. When connectors.toml cannot be written (a symlink
// is ErrHandManaged, one that does not parse ErrBadFile) nothing is
// changed, so the connector, the answer and a retry agree. Once the entry
// is gone the connector is, and nothing is cancelled with ctx: a failure
// to remove the auth.toml keys or the row still forgets it and emits
// "deleted", and is ErrLeftovers; a retry removes them. The conn stays in
// the map until then: a create of the same id meanwhile gets ErrExists.
func (m *Manager) Delete(ctx context.Context, id string) error {
	if !config.ValidConnectorID(id) {
		return ErrNotFound
	}
	// Locked before it is in the map, so a second Delete finds it only
	// once this one is done.
	hold := &conn{id: id, deleted: true} // holds the id while it is cleaned
	hold.mu.Lock()
	m.mu.Lock()
	c, ok := m.conns[id]
	if !ok {
		c = hold
		m.conns[id] = c
	}
	m.mu.Unlock()
	if ok {
		hold.mu.Unlock()
		c.mu.Lock()
		m.mu.Lock()
		gone := m.conns[id] != c
		m.mu.Unlock()
		if gone || c.deleted {
			c.mu.Unlock()
			return ErrNotFound
		}
	}
	inFile, err := m.hasEntry(id)
	if err == nil && !ok && !inFile {
		// Written by hand after start, or a leftover: clean what is there.
		_, gerr := m.o.Store.GetConnector(ctx, id)
		switch {
		case gerr != nil && !errors.Is(gerr, store.ErrNotFound):
			err = gerr // nothing is changed
		case gerr != nil && len(m.o.Secrets.Keys(secrets.Connectors, id)) == 0:
			err = ErrNotFound
		}
	}
	if err == nil && inFile {
		err = m.removeEntry(id)
	}
	if err != nil {
		c.mu.Unlock()
		if !ok {
			m.forget(c)
		}
		switch {
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrBadFile):
			return err
		case errors.Is(err, ErrHandManaged):
			return fmt.Errorf("%w: remove [connectors.%s] there by hand, then delete again", err, id)
		}
		return fmt.Errorf("connector %s: delete: %w", id, err)
	}
	ctx = context.WithoutCancel(ctx)
	c.deleted, c.intent = true, nil
	cl := c.client
	c.client = nil
	if c.listTimer != nil {
		c.listTimer.Stop()
	}
	err = errors.Join(
		m.o.Secrets.Delete(secrets.Connectors, id),
		ignoreNotFound(m.o.Store.DeleteConnector(ctx, id)),
	)
	c.mu.Unlock()
	// Announced while c still holds the id: a create of it comes after.
	m.emit(ctx, id, StateDeleted, "", nil)
	m.forget(c)
	if cl != nil {
		_ = cl.Close()
	}
	if err != nil {
		// The cause stays in the log: a store or auth.toml error is no
		// answer for the client. The id is free now, so a create of it
		// would be the connector a later delete removes.
		m.log.Warn("connector deleted, leftovers remain", "connector", id, "err", err)
		return fmt.Errorf("%w; delete %s again before creating it again", ErrLeftovers, id)
	}
	return nil
}

func ignoreNotFound(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}

// adoptLocked makes cl c's client, or closes it when c is deleted or the
// manager closed. c.mu is held.
func (m *Manager) adoptLocked(c *conn, cl *mcp.Client) error {
	switch {
	case m.closed.Load():
		go func() { _ = cl.Close() }()
		return ErrClosed
	case c.deleted:
		go func() { _ = cl.Close() }()
		return ErrNotFound
	}
	old := c.client
	c.client = cl
	if old != nil && old != cl {
		go func() { _ = old.Close() }()
	}
	return nil
}

// dial connects to cfg's server, with c's token source for OAuth.
// tools/list_changed refreshes the cache row once the client is c's
// (toolsChanged ignores any other, such as a probe's).
func (m *Manager) dial(ctx context.Context, c *conn, cfg config.Connector) (*mcp.Client, error) {
	if m.closed.Load() {
		return nil, ErrClosed
	}
	var self atomic.Pointer[mcp.Client]
	o := mcp.Options{URL: cfg.URL, HTTPClient: m.o.HTTPClient, Insecure: cfg.AllowInsecureTransport}
	o.OnToolsChanged = func() {
		if cl := self.Load(); cl != nil {
			m.toolsChanged(c, cl)
		}
	}
	if cfg.Auth == "oauth" {
		o.Token = m.tokenSource(c)
	}
	cl, err := mcp.Dial(ctx, o)
	if err != nil {
		return nil, err
	}
	self.Store(cl)
	return cl, nil
}

// cacheTools keeps the usable tools, logs how many were dropped or had
// their description cut, and marshals them under the cap.
func (m *Manager) cacheTools(c *conn, listed []mcp.Tool) ([]mcp.Tool, json.RawMessage, error) {
	tools := usableTools(listed)
	if n := len(listed) - len(tools); n > 0 {
		m.log.Warn("connector tools dropped: bad or duplicate names", "connector", c.id, "count", n)
	}
	if n := descriptionsCut(tools); n > 0 {
		m.log.Warn("connector tool descriptions cut", "connector", c.id, "count", n, "cap_tokens", MaxDescriptionTokens)
	}
	raw, err := marshalCapped(tools, maxToolsJSON)
	return tools, raw, err
}

// usableTools keeps the tools whose names can follow "<id>.", the first
// of duplicates, each description cut to MaxDescriptionTokens.
func usableTools(listed []mcp.Tool) []mcp.Tool {
	tools := make([]mcp.Tool, 0, len(listed))
	seen := map[string]bool{}
	for _, t := range listed {
		if toolName.MatchString(t.Name) && !seen[t.Name] {
			seen[t.Name] = true
			t.Description = serverDescription(t.Description)
			tools = append(tools, t)
		}
	}
	return tools
}

// toolName is a tool name that can follow "<id>." in a tool name.
var toolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// sameURL compares two server URLs as discovery does (oauth.CanonicalURL):
// case in scheme and host, a default port and a trailing slash do not
// count.
func sameURL(a, b string) bool {
	ca, okA := oauth.CanonicalURL(a)
	cb, okB := oauth.CanonicalURL(b)
	return okA && okB && ca == cb
}
