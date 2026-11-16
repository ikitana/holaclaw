package connector

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/config"
	"holaclaw/internal/connector/mcp"
	"holaclaw/internal/connector/oauth"
	"holaclaw/internal/store"
)

// intent is one open authorization, in memory only (docs/connectors.md §2).
type intent struct {
	create bool             // a new connector: nothing is in connectors.toml yet
	cfg    config.Connector // the entry connectors.toml gets on success
	secret string           // pre-registered client secret from the request
	ready  bool             // discovery is done and the URL is out
	used   bool             // a code arrived: single use

	meta    oauth.Metadata
	ident   oauth.Identity
	auth    oauth.Authorization
	expires time.Time

	// prevState and prevDetail are where a reconnect returns when it fails;
	// the old tokens serve meanwhile.
	prevState  store.ConnectorState
	prevDetail string
}

// CreateRequest is the body of a create (docs/connectors.md §8).
type CreateRequest struct {
	ID  string
	URL string
	// Auth is "none" or "oauth"; "" takes what discovery finds. Given, it
	// makes the create fail when the server disagrees.
	Auth   string
	Scopes []string // nil = every scope the server offers
	// RedirectURI is the client's loopback listener, required unless the
	// server is open.
	RedirectURI string
	// ClientID and ClientSecret are a pre-registered client; the secret
	// goes to auth.toml.
	ClientID     string
	ClientSecret string
	// AllowInsecureTransport is the entry's allow_insecure_transport: the
	// URL may be plain http on any host, or https with a certificate that
	// does not verify.
	AllowInsecureTransport bool
}

// ReconnectRequest is the body of a reconnect.
type ReconnectRequest struct {
	Scopes      []string // nil keeps the connector's scopes
	RedirectURI string   // required unless the server is open
	// ClientSecret replaces the pre-registered client's secret. It is
	// required when the stored one cannot be reused: the authorization
	// server changed issuer, or the client id is not the one the tokens
	// were granted to.
	ClientSecret string
}

// Started answers a create or a reconnect.
type Started struct {
	Connector Connector
	// AuthorizationURL is what the person opens; empty when the connector
	// is connected already (an open server).
	AuthorizationURL string
	ExpiresAt        time.Time // of the intent; zero without a URL
}

// Create opens a new connector: it refuses an id in use, runs discovery
// and either connects an open server at once or answers the authorization
// URL of a fresh intent. A failed create leaves nothing and emits state
// "deleted" with the reason.
func (m *Manager) Create(ctx context.Context, r CreateRequest) (Started, error) {
	if err := validCreate(r); err != nil {
		return Started{}, err
	}
	cfg := config.Connector{
		ID: r.ID, URL: r.URL, Auth: r.Auth, Scopes: slices.Clone(r.Scopes), ClientID: r.ClientID,
		AllowInsecureTransport: r.AllowInsecureTransport,
	}
	in := &intent{create: true, cfg: cfg, secret: r.ClientSecret}
	c := &conn{id: r.ID, row: store.Connector{ID: r.ID}, intent: in}

	m.mu.Lock()
	if _, ok := m.conns[r.ID]; ok {
		m.mu.Unlock()
		return Started{}, ErrExists
	}
	m.conns[r.ID] = c
	m.mu.Unlock()
	if inFile, err := m.hasEntry(r.ID); err != nil || inFile {
		m.forget(c)
		if err == nil {
			err = ErrExists
		}
		return Started{}, err
	}

	c.mu.Lock()
	err := m.updateLocked(ctx, c, func(row *store.Connector) { row.State = store.ConnectorPending })
	if err != nil && !c.deleted { // deleted: the Delete cleaned up already
		c.deleted = true
		_ = ignoreNotFound(m.o.Store.DeleteConnector(ctx, r.ID))
	}
	c.mu.Unlock()
	if err != nil {
		m.forget(c)
		if errors.Is(err, ErrNotFound) { // a Delete of the id got in first
			err = ErrExists
		}
		return Started{}, err
	}
	return m.open(ctx, c, in, r.RedirectURI)
}

// Reconnect opens a new intent on a connector, optionally with new
// scopes. The old tokens serve until the new code is exchanged; a failed
// or expired intent returns the connector to its state. It is also how an
// entry written by hand gets credentials. An open server is redialed.
func (m *Manager) Reconnect(ctx context.Context, id string, r ReconnectRequest) (Started, error) {
	if r.Scopes != nil && !validScopes(r.Scopes) {
		return Started{}, fmt.Errorf("%w: bad scopes", ErrInvalid)
	}
	if r.RedirectURI != "" && !validRedirect(r.RedirectURI) {
		return Started{}, errBadRedirect
	}
	c, err := m.conn(id)
	if err != nil {
		return Started{}, err
	}
	c.mu.Lock()
	m.sweepLocked(ctx, c)
	if c.deleted {
		c.mu.Unlock()
		return Started{}, ErrNotFound
	}
	old := c.intent
	if old != nil && (!old.ready || old.used) {
		c.mu.Unlock()
		return Started{}, ErrBusy
	}
	cfg := c.cfg
	in := &intent{prevState: c.row.State, prevDetail: c.row.Detail}
	if old != nil {
		cfg, in.create, in.secret = old.cfg, old.create, old.secret
		in.prevState, in.prevDetail = old.prevState, old.prevDetail
	}
	if cfg.Auth == "none" && old == nil {
		c.mu.Unlock()
		return m.redialOpen(ctx, c)
	}
	switch {
	case r.RedirectURI == "":
		c.mu.Unlock()
		return Started{}, fmt.Errorf("%w: redirect_uri is required", ErrInvalid)
	case r.ClientSecret != "" && cfg.ClientID == "":
		c.mu.Unlock()
		return Started{}, fmt.Errorf("%w: client_secret needs a pre-registered client_id", ErrInvalid)
	case r.ClientSecret != "":
		in.secret = r.ClientSecret
	}
	if r.Scopes != nil {
		cfg.Scopes = slices.Clone(r.Scopes)
	}
	in.cfg = cfg
	c.intent = in
	err = m.updateLocked(ctx, c, func(row *store.Connector) { row.State, row.Detail = store.ConnectorAuthorizing, "" })
	c.mu.Unlock()
	if err != nil {
		return Started{}, err
	}
	return m.open(ctx, c, in, r.RedirectURI)
}

// open runs discovery for in: an unauthenticated initialize, then the
// metadata, the client identity and the authorization URL. The auth it
// finds goes to in.cfg under c.mu only: List and Get read it meanwhile.
func (m *Manager) open(ctx context.Context, c *conn, in *intent, redirect string) (Started, error) {
	cfg := in.cfg
	cl, www, err := m.probe(ctx, c, cfg)
	switch {
	case err != nil:
		return Started{}, m.failOpen(ctx, c, in, DetailUnreachable, fmt.Errorf("%w: %s", ErrUnreachable, cause(err)))
	case cl != nil && cfg.Auth != "oauth":
		return m.finishOpen(ctx, c, in, cl)
	case cl != nil:
		_ = cl.Close() // auth = "oauth" on a server that answered without a token: look for metadata
	case cfg.Auth == "none":
		return Started{}, m.failOpen(ctx, c, in, DetailAuthMismatch, ErrAuthMismatch)
	}
	if redirect == "" {
		return Started{}, m.failOpen(ctx, c, in, DetailClientIdentity, fmt.Errorf("%w: redirect_uri is required", ErrInvalid))
	}

	meta, err := m.oauth.Discover(ctx, cfg.URL, www)
	if err != nil && cl != nil { // it answered without a token and names no metadata
		return Started{}, m.failOpen(ctx, c, in, DetailAuthMismatch, ErrAuthMismatch)
	}
	if err != nil {
		return Started{}, m.failOpen(ctx, c, in, DetailDiscovery, fmt.Errorf("%w: %w", ErrDiscovery, err))
	}
	opts, err := m.identityOptions(c, in, meta, redirect)
	if err != nil {
		return Started{}, m.failOpen(ctx, c, in, DetailClientIdentity, err)
	}
	ident, err := m.oauth.Identify(ctx, meta, opts)
	if err != nil {
		return Started{}, m.failOpen(ctx, c, in, DetailClientIdentity, fmt.Errorf("%w: %w", ErrDiscovery, err))
	}
	auth, err := oauth.Authorize(meta, ident, cfg.Scopes)
	if err != nil {
		return Started{}, m.failOpen(ctx, c, in, DetailClientIdentity, fmt.Errorf("%w: %w", ErrDiscovery, err))
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deleted || c.intent != in {
		return Started{}, ErrNoIntent
	}
	in.cfg.Auth = "oauth"
	in.meta, in.ident, in.auth = meta, ident, auth
	in.expires = m.now().Add(IntentTTL)
	in.ready = true
	if in.create {
		raw, err := marshalCapped(authMeta{Metadata: meta, IdentityKind: ident.Kind, ServerURL: in.cfg.URL}, maxAuthMetaJSON)
		if err != nil {
			m.endIntentLocked(ctx, c, in, DetailDiscovery)
			return Started{}, fmt.Errorf("%w: auth metadata too large", ErrDiscovery)
		}
		if err := m.updateLocked(ctx, c, func(r *store.Connector) {
			r.State, r.Detail, r.AuthMeta = store.ConnectorAuthorizing, "", raw
		}); err != nil {
			m.endIntentLocked(ctx, c, in, DetailCacheWrite)
			return Started{}, err
		}
	}
	return Started{Connector: c.viewLocked(), AuthorizationURL: auth.URL, ExpiresAt: in.expires}, nil
}

// probe calls the server without a token: initialize, then tools/list. An
// open server answers both and its client is returned; a protected one
// answers 401 to either, and its WWW-Authenticate starts discovery.
func (m *Manager) probe(ctx context.Context, c *conn, cfg config.Connector) (*mcp.Client, string, error) {
	cl, err := m.dial(ctx, c, config.Connector{URL: cfg.URL, Auth: "none", AllowInsecureTransport: cfg.AllowInsecureTransport})
	var ue *mcp.UnauthorizedError
	if errors.As(err, &ue) {
		return nil, ue.WWWAuthenticate, nil
	}
	if err != nil {
		return nil, "", err
	}
	if _, err := cl.ListTools(ctx); err != nil {
		_ = cl.Close()
		if errors.As(err, &ue) {
			return nil, ue.WWWAuthenticate, nil
		}
		return nil, "", err
	}
	return cl, "", nil
}

// identityOptions are the inputs of Identify for in: the pre-registered
// client, the client registered before when it is of the same issuer, and
// whether the metadata document was refused. The stored client secret
// only goes back to the issuer and client it was issued for: a
// pre-registered client meeting another issuer, or a client id the tokens
// were not granted to, needs the secret in the request; after a registered
// client's grant it goes without one unless the request names it. An entry
// never granted (written by hand) uses what the admin stored.
func (m *Manager) identityOptions(c *conn, in *intent, meta oauth.Metadata, redirect string) (oauth.IdentityOptions, error) {
	o := oauth.IdentityOptions{ClientID: in.cfg.ClientID, RedirectURI: redirect}
	stored, _ := m.o.Secrets.Get(secrets.Connectors, c.id, secrets.ConnectorClientSecret)
	c.mu.Lock()
	defer c.mu.Unlock()
	var am *authMeta // of the grant the tokens belong to
	if !in.create {
		am = parseAuthMeta(c.row.AuthMeta)
	}
	sameIssuer := am != nil && am.Metadata.Issuer == meta.Issuer
	if o.ClientID != "" {
		o.ClientSecret = in.secret
		switch {
		case o.ClientSecret != "" || stored == "" || in.create:
		case am == nil:
			m.log.Warn("connector reuses a client_secret left in auth.toml for its pre-registered client", "connector", c.id)
			o.ClientSecret = stored
		case sameIssuer && am.IdentityKind == oauth.IdentityPreRegistered && c.row.ClientID == o.ClientID:
			o.ClientSecret = stored
		case sameIssuer && am.IdentityKind == oauth.IdentityDCR && c.row.ClientID == o.ClientID:
			o.ClientSecret = stored // the registered client, now named by hand
		case am.IdentityKind == oauth.IdentityDCR:
			// The stored secret is the registered client's, not this one's:
			// a public client goes without.
		default:
			return o, fmt.Errorf("%w: client_secret is required: the stored one belongs to another issuer or client", ErrInvalid)
		}
	}
	if sameIssuer && am.IdentityKind == oauth.IdentityDCR {
		o.RegisteredClientID, o.RegisteredClientSecret = c.row.ClientID, stored
		o.RegisteredRedirectURI = am.RegisteredRedirect
	}
	m.mu.Lock()
	o.CIMDRefused = c.row.CIMDRefused || m.refused[meta.Issuer]
	m.mu.Unlock()
	return o, nil
}

// finishOpen connects an open server found by a create.
func (m *Manager) finishOpen(ctx context.Context, c *conn, in *intent, cl *mcp.Client) (Started, error) {
	if err := m.refreshInfo(ctx, c, cl, true); err != nil {
		_ = cl.Close()
		detail, err := connectFailure(err)
		return Started{}, m.failOpen(ctx, c, in, detail, err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deleted || c.intent != in || m.closed.Load() {
		go func() { _ = cl.Close() }()
		m.endIntentLocked(ctx, c, in, DetailUnreachable)
		return Started{}, ErrNotFound
	}
	in.cfg.Auth = "none"
	if err := m.writeEntry(in.cfg); err != nil {
		go func() { _ = cl.Close() }()
		m.endIntentLocked(ctx, c, in, DetailConfigWrite)
		return Started{}, handManaged(err, c.id, in.create)
	}
	c.intent, c.cfg, c.inConfig = nil, in.cfg, true
	if err := m.adoptLocked(c, cl); err != nil {
		return Started{}, err
	}
	if err := m.connectedLocked(ctx, c, cl); err != nil {
		return Started{}, err
	}
	return Started{Connector: c.viewLocked()}, nil
}

// connectFailure maps a failed dial or refreshInfo after a connect to its
// detail and error.
func connectFailure(err error) (string, error) {
	if errors.Is(err, errTooLarge) || errors.Is(err, mcp.ErrListTooLarge) {
		return DetailToolsTooLarge, fmt.Errorf("%w: tool list too large", ErrUnreachable)
	}
	return DetailUnreachable, fmt.Errorf("%w: %s", ErrUnreachable, cause(err))
}

// handManaged says what to do when connectors.toml is a symlink. A create
// kept nothing, its tokens included; a reconnect kept the new tokens but
// its entry still has the old scopes.
func handManaged(err error, id string, create bool) error {
	switch {
	case !errors.Is(err, ErrHandManaged):
		return err
	case create:
		return fmt.Errorf("%w: nothing was kept; add [connectors.%s] there by hand, restart the daemon, then reconnect %s", err, id, id)
	}
	return fmt.Errorf("%w: the new sign-in is stored, but [connectors.%s] keeps its old scopes; set them there by hand and restart the daemon", err, id)
}

// sameEntry reports whether writeEntry would write b's keys as a has them.
func sameEntry(a, b config.Connector) bool {
	return a.URL == b.URL && a.Auth == b.Auth && a.ClientID == b.ClientID &&
		(a.Scopes == nil) == (b.Scopes == nil) && slices.Equal(a.Scopes, b.Scopes)
}

// failOpen ends in with detail and returns err.
func (m *Manager) failOpen(ctx context.Context, c *conn, in *intent, detail string, err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	m.endIntentLocked(ctx, c, in, detail)
	return err
}

// endIntentLocked drops in when it is still c's intent. A create leaves
// nothing behind; a reconnect returns the connector to its state with
// detail. c.mu is held.
func (m *Manager) endIntentLocked(ctx context.Context, c *conn, in *intent, detail string) {
	if c.intent != in || c.deleted {
		return
	}
	c.intent = nil
	if in.create {
		// c stays in the map, deleted, until its row and secrets are gone:
		// a create of the same id meanwhile gets ErrExists.
		c.deleted = true
		_ = ignoreNotFound(m.o.Store.DeleteConnector(ctx, c.id))
		// A grant stored before the failure. A create owns the id, so an
		// auth.toml entry written by hand goes too.
		_ = m.o.Secrets.Delete(secrets.Connectors, c.id)
		m.forget(c)
		m.emit(ctx, c.id, StateDeleted, detail, nil)
		return
	}
	if detail == "" {
		detail = in.prevDetail
	}
	if err := m.updateLocked(ctx, c, func(r *store.Connector) { r.State, r.Detail = in.prevState, detail }); err != nil {
		m.log.Warn("connector state not saved", "connector", c.id, "err", err)
	}
}

// forget removes c from the map when it is still the one under its id.
func (m *Manager) forget(c *conn) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conns[c.id] == c {
		delete(m.conns, c.id)
	}
}

// usedGrace is how long past its TTL a used intent may still be finishing
// its exchange before the sweep drops it anyway.
const usedGrace = 5 * time.Minute

// sweepLocked drops c's intent once its TTL passed, or a used one stuck
// usedGrace beyond. c.mu is held.
func (m *Manager) sweepLocked(ctx context.Context, c *conn) {
	in := c.intent
	if in == nil || !in.ready {
		return
	}
	deadline := in.expires
	if in.used {
		deadline = deadline.Add(usedGrace)
	}
	if !m.now().Before(deadline) {
		m.endIntentLocked(ctx, c, in, DetailIntentExpired)
	}
}

// Code finishes the open intent with the redirect's query: it checks the
// state (a mismatch drops the intent), exchanges the code, writes
// auth.toml, runs initialize and tools/list, writes connectors.toml and
// moves to connected. It returns once all of that is done. A server that
// fails after the exchange leaves the connector degraded, credentials
// kept.
func (m *Manager) Code(ctx context.Context, id string, cb oauth.Callback) (Connector, error) {
	c, err := m.conn(id)
	if err != nil {
		return Connector{}, err
	}
	c.mu.Lock()
	m.sweepLocked(ctx, c)
	in := c.intent
	if c.deleted {
		c.mu.Unlock()
		return Connector{}, ErrNotFound
	}
	if in == nil || !in.ready || in.used {
		c.mu.Unlock()
		return Connector{}, ErrNoIntent
	}
	in.used = true
	c.mu.Unlock()
	// From here every failure ends the intent (failCode, storeGrant, the
	// branches below); sweepLocked is the backstop. Nothing is cancelled
	// with ctx: a code is single use.
	ctx = context.WithoutCancel(ctx)

	// Held until the row names the new grant, so no refresh mixes the old
	// metadata with the new tokens.
	c.refreshMu.Lock()
	tok, err := m.oauth.Exchange(ctx, in.meta, in.ident, in.auth, cb)
	if err != nil {
		c.refreshMu.Unlock()
		return Connector{}, m.failCode(ctx, c, in, err)
	}
	err = m.storeGrant(ctx, c, in, tok)
	c.refreshMu.Unlock()
	if err != nil {
		return Connector{}, err
	}

	cl, dialErr := m.dial(ctx, c, in.cfg)
	if dialErr == nil {
		if dialErr = m.refreshInfo(ctx, c, cl, true); dialErr != nil {
			_ = cl.Close()
			cl = nil
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deleted || c.intent != in || m.closed.Load() {
		if cl != nil {
			go func() { _ = cl.Close() }()
		}
		m.endIntentLocked(ctx, c, in, DetailUnreachable)
		return Connector{}, ErrNoIntent
	}
	// A reconnect that keeps the entry does not write it: it works over a
	// symlinked connectors.toml.
	if in.create || !sameEntry(in.cfg, c.cfg) {
		if err := m.writeEntry(in.cfg); err != nil {
			if cl != nil {
				go func() { _ = cl.Close() }()
			}
			m.endIntentLocked(ctx, c, in, DetailConfigWrite)
			return Connector{}, handManaged(err, c.id, in.create)
		}
	}
	c.intent, c.cfg, c.inConfig = nil, in.cfg, true
	if cl != nil {
		if err := m.adoptLocked(c, cl); err != nil {
			return Connector{}, err
		}
	} else if old := c.client; old != nil {
		c.client = nil // the old grant's client
		go func() { _ = old.Close() }()
	}
	if cl == nil {
		detail, _ := connectFailure(dialErr)
		err = m.updateLocked(ctx, c, func(r *store.Connector) {
			r.State, r.Detail = store.ConnectorDegraded, detail
			r.Failures++
			now := m.now()
			r.LastCheck = &now
		})
	} else {
		err = m.connectedLocked(ctx, c, cl)
	}
	if err != nil {
		return Connector{}, err
	}
	return c.viewLocked(), nil
}

// failCode ends in after a failed exchange and maps the error.
func (m *Manager) failCode(ctx context.Context, c *conn, in *intent, err error) error {
	detail, out := DetailAuthorization, fmt.Errorf("%w: %w", ErrAuthorization, err)
	refused := errors.Is(err, oauth.ErrCIMDRefused)
	switch {
	case errors.Is(err, oauth.ErrStateMismatch):
		detail, out = DetailStateMismatch, ErrStateMismatch
	case refused:
		detail = DetailCIMDRefused
		m.mu.Lock()
		m.refused[in.meta.Issuer] = true
		m.mu.Unlock()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if refused && !in.create {
		// Remembered in the row: the next intent registers instead. The
		// intent is still open, so this does not change the state.
		_ = m.updateLocked(ctx, c, func(r *store.Connector) { r.CIMDRefused = true })
	}
	m.endIntentLocked(ctx, c, in, detail)
	return out
}

// storeGrant writes the new tokens to auth.toml and the grant to the row:
// metadata, identity kind and client id, expiry and scopes. The row stays
// authorizing; the old tokens are replaced only now.
func (m *Manager) storeGrant(ctx context.Context, c *conn, in *intent, tok oauth.Token) error {
	am := authMeta{Metadata: in.meta, IdentityKind: in.ident.Kind, ServerURL: in.cfg.URL}
	if in.ident.Kind == oauth.IdentityDCR {
		am.RegisteredRedirect = in.ident.RedirectURI
	}
	raw, err := marshalCapped(am, maxAuthMetaJSON)
	if err != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		m.endIntentLocked(ctx, c, in, DetailDiscovery)
		return fmt.Errorf("%w: auth metadata too large", ErrDiscovery)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deleted || c.intent != in {
		return ErrNoIntent
	}
	prev := map[string]string{}
	for _, k := range grantKeys {
		if v, ok := m.o.Secrets.Get(secrets.Connectors, c.id, k); ok {
			prev[k] = v
		}
	}
	next := map[string]string{
		secrets.ConnectorAccessToken: tok.AccessToken, secrets.ConnectorRefreshToken: tok.RefreshToken,
		secrets.ConnectorClientSecret: in.ident.ClientSecret,
	}
	if err := m.writeGrant(c.id, next); err != nil {
		m.restoreGrant(c.id, prev)
		m.endIntentLocked(ctx, c, in, DetailConfigWrite)
		return fmt.Errorf("connector %s: write auth.toml: %w", c.id, err)
	}
	m.mu.Lock()
	refused := m.refused[in.meta.Issuer]
	m.mu.Unlock()
	if err := m.updateLocked(ctx, c, func(r *store.Connector) {
		r.AuthMeta, r.ClientID = raw, in.ident.ClientID
		r.CIMDRefused = r.CIMDRefused || refused
		r.Scopes = slices.Clone(tok.Scopes)
		r.TokenExpiry = expiry(tok.Expiry)
	}); err != nil {
		// The row still names the old grant: give it back its tokens.
		m.restoreGrant(c.id, prev)
		m.endIntentLocked(ctx, c, in, DetailCacheWrite)
		return err
	}
	c.gen++
	c.refreshFailed = time.Time{}
	return nil
}

// restoreGrant writes back the grant storeGrant replaced.
func (m *Manager) restoreGrant(id string, prev map[string]string) {
	if err := m.writeGrant(id, prev); err != nil {
		m.log.Warn("connector grant not restored", "connector", id, "err", err)
	}
}

// grantKeys are the auth.toml keys of one grant.
var grantKeys = []string{secrets.ConnectorAccessToken, secrets.ConnectorRefreshToken, secrets.ConnectorClientSecret}

// writeGrant makes id's grant keys exactly kv: an empty or missing value
// is removed.
func (m *Manager) writeGrant(id string, kv map[string]string) error {
	set := map[string]string{}
	var drop []string
	for _, k := range grantKeys {
		if v := kv[k]; v != "" {
			set[k] = v
		} else {
			drop = append(drop, k)
		}
	}
	if len(set) > 0 {
		if err := m.o.Secrets.Set(secrets.Connectors, id, set); err != nil {
			return err
		}
	}
	if len(drop) > 0 {
		return m.o.Secrets.Delete(secrets.Connectors, id, drop...)
	}
	return nil
}

func expiry(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// refreshInfo reads the server name and tools from cl into c's row. With
// force the row is written even when cl is not c's client yet.
func (m *Manager) refreshInfo(ctx context.Context, c *conn, cl *mcp.Client, force bool) error {
	listed, err := cl.ListTools(ctx)
	if err != nil {
		return err
	}
	tools, raw, err := m.cacheTools(c, listed)
	if err != nil {
		return err
	}
	si := cl.ServerInfo()
	name, version, session := capInfo(si.Name), capInfo(si.Version), capSession(cl.SessionID())
	c.mu.Lock()
	defer c.mu.Unlock()
	if !force && c.client != cl {
		return nil
	}
	r := c.row
	if bytes.Equal(raw, r.Tools) && name == r.ServerName && version == r.ServerVersion && session == r.SessionID {
		return nil
	}
	if err := m.updateLocked(ctx, c, func(r *store.Connector) {
		r.ServerName, r.ServerVersion, r.SessionID, r.Tools = name, version, session, raw
	}); err != nil {
		return err
	}
	c.tools = tools
	return nil
}

// connectedLocked marks a successful check or connect. c.mu is held.
func (m *Manager) connectedLocked(ctx context.Context, c *conn, cl *mcp.Client) error {
	now := m.now()
	return m.updateLocked(ctx, c, func(r *store.Connector) {
		r.State, r.Detail, r.Failures, r.LastCheck = store.ConnectorConnected, "", 0, &now
		r.SessionID = capSession(cl.SessionID())
	})
}

// Inspection is what Inspect learned; nothing of it is stored.
type Inspection struct {
	// Open means initialize and tools/list worked without a token.
	Open          bool
	ServerName    string
	ServerVersion string
	Tools         []mcp.Tool
	// Metadata is the discovered auth metadata of a protected server.
	Metadata *oauth.Metadata
	// IdentityKind is how a create with a pinned redirect and no client id
	// would name itself; "" means a pre-registered client id is needed.
	IdentityKind oauth.IdentityKind
}

// inspectTimeout bounds a whole Inspect: dial, listing and discovery. A
// variable for tests.
var inspectTimeout = 90 * time.Second

// Inspect asks a server what it is, without a token, and stores nothing.
// It never registers a client. Each probe is logged with the host and a
// fixed word for the outcome, never the path, query or server text.
func (m *Manager) Inspect(ctx context.Context, serverURL string) (Inspection, error) {
	if !validURL(serverURL, false) {
		return Inspection{}, fmt.Errorf("%w: url must be https, or http on a loopback host", ErrInvalid)
	}
	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	in, err := m.inspect(ctx, serverURL)
	u, _ := url.Parse(serverURL) // validURL parsed it
	m.log.Info("connector inspect", "host", u.Host, "result", inspectResult(ctx, in, err))
	return in, err
}

// inspectResult is the outcome word of an inspect.
func inspectResult(ctx context.Context, in Inspection, err error) string {
	switch {
	case err == nil && in.Open:
		return "open"
	case err == nil:
		return "protected"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrUnreachable):
		return "unreachable"
	}
	return "discovery_failed"
}

func (m *Manager) inspect(ctx context.Context, serverURL string) (Inspection, error) {
	cl, err := mcp.Dial(ctx, mcp.Options{URL: serverURL, HTTPClient: m.o.HTTPClient})
	www := ""
	var ue *mcp.UnauthorizedError
	switch {
	case errors.As(err, &ue):
		www = ue.WWWAuthenticate
	case err != nil:
		return Inspection{}, fmt.Errorf("%w: %s", ErrUnreachable, cause(err))
	default:
		defer func() { _ = cl.Close() }()
		si := cl.ServerInfo()
		tools, err := cl.ListTools(ctx)
		switch {
		case err == nil:
			// As a create would cache them.
			return Inspection{Open: true, ServerName: capInfo(si.Name), ServerVersion: capInfo(si.Version), Tools: usableTools(tools)}, nil
		case errors.As(err, &ue):
			www = ue.WWWAuthenticate
		default:
			return Inspection{}, fmt.Errorf("%w: %s", ErrUnreachable, cause(err))
		}
	}
	meta, err := m.oauth.Discover(ctx, serverURL, www)
	if err != nil {
		return Inspection{}, fmt.Errorf("%w: %w", ErrDiscovery, err)
	}
	// As identityOptions sees it for a create: no client id, a pinned
	// redirect, the issuer's refusal of the document remembered.
	m.mu.Lock()
	refused := m.refused[meta.Issuer]
	m.mu.Unlock()
	kind := oauth.IdentityKindFor(meta, oauth.IdentityOptions{RedirectURI: oauth.PinnedRedirects()[0], CIMDRefused: refused})
	return Inspection{Metadata: &meta, IdentityKind: kind}, nil
}

func validCreate(r CreateRequest) error {
	switch {
	case !config.ValidConnectorID(r.ID):
		return fmt.Errorf("%w: id must be [a-z0-9-], at most 32 characters", ErrInvalid)
	case !validURL(r.URL, r.AllowInsecureTransport):
		return fmt.Errorf("%w: url must be https, or http on a loopback host without allow_insecure_transport", ErrInvalid)
	case r.Auth != "" && r.Auth != "none" && r.Auth != "oauth":
		return fmt.Errorf("%w: auth must be none or oauth", ErrInvalid)
	case r.Scopes != nil && !validScopes(r.Scopes):
		return fmt.Errorf("%w: bad scopes", ErrInvalid)
	case r.ClientSecret != "" && r.ClientID == "":
		return fmt.Errorf("%w: client_secret needs client_id", ErrInvalid)
	case r.ClientID != "" && (r.Auth == "none" || !printable(r.ClientID)):
		return fmt.Errorf("%w: bad client_id", ErrInvalid)
	case r.Auth == "oauth" && r.RedirectURI == "":
		return fmt.Errorf("%w: redirect_uri is required", ErrInvalid)
	case r.RedirectURI != "" && !validRedirect(r.RedirectURI):
		return errBadRedirect
	}
	return nil
}

var errBadRedirect = fmt.Errorf("%w: redirect_uri must be http on 127.0.0.1 or [::1] with a port", ErrInvalid)

// maxRedirect caps a redirect URI.
const maxRedirect = 512

// validRedirect is a loopback listener: http, 127.0.0.1 or [::1], an
// explicit port, no user info, query or fragment.
func validRedirect(raw string) bool {
	if len(raw) > maxRedirect {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if h := u.Hostname(); h != "127.0.0.1" && h != "::1" {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	return err == nil && port > 0 && port <= 65535
}

// validURL is the rule of CON003: https, or http on a loopback host or with
// insecure. A loopback host is localhost or a loopback IP; a name such as
// 127.evil.com is not.
func validURL(raw string, insecure bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		if insecure || strings.EqualFold(h, "localhost") {
			return true
		}
		ip := net.ParseIP(h)
		return ip != nil && ip.IsLoopback()
	}
	return false
}

func validScopes(scopes []string) bool {
	for _, s := range scopes {
		if s == "" || !printable(s) || strings.ContainsAny(s, `"\`) {
			return false
		}
	}
	return len(scopes) <= 64
}

// printable is non-empty visible ASCII, at most 256 bytes.
func printable(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
