package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"holaclaw/internal/auth/secrets"
	"holaclaw/internal/connector/mcp"
	"holaclaw/internal/connector/oauth"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

const (
	// CheckInterval is how often the daemon should call Check.
	CheckInterval = 10 * time.Minute
	// listChangedInterval is the least time between two list_changed
	// refreshes of one connector; a change inside it is listed once it
	// ends.
	listChangedInterval = 10 * time.Second
	// refreshMargin: a call refreshes first when the access token expires
	// within it.
	refreshMargin = 60 * time.Second
	// maxFailures consecutive failed checks move a connector to degraded.
	maxFailures = 3
)

// Check is the health hook, for housekeeping every CheckInterval and once
// at start (docs/connectors.md §7). Per connector it refreshes a token
// that expires before the next check, pings, and refreshes the cached tool
// list. One success moves a connector to connected; maxFailures in a row
// move it to degraded; a rejected refresh moves it to expired at once; a
// 401 on an open server moves it to degraded at once. Expired intents are
// dropped. Connectors that are expired or have an open intent are skipped.
func (m *Manager) Check(ctx context.Context) {
	var wg sync.WaitGroup
	for _, c := range m.snapshot() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.check(ctx, c)
		}()
	}
	wg.Wait()
}

func (m *Manager) check(ctx context.Context, c *conn) {
	c.mu.Lock()
	m.sweepLocked(ctx, c)
	skip := c.deleted || !c.inConfig || c.intent != nil || c.row.State == store.ConnectorExpired
	oauthAuth := c.cfg.Auth == "oauth"
	c.mu.Unlock()
	if skip {
		return
	}
	if oauthAuth {
		if err := m.refreshIfDue(ctx, c, CheckInterval+refreshMargin); err != nil {
			if !errors.Is(err, ErrNeedsReconnect) {
				m.failed(ctx, c, DetailRefreshFailed)
			}
			return
		}
	}
	cl, err := m.client(ctx, c)
	if err != nil {
		m.checkFailed(ctx, c, err, DetailUnreachable)
		return
	}
	if err := m.withRefresh(ctx, c, func() error { return cl.Ping(ctx) }); err != nil {
		m.checkFailed(ctx, c, err, DetailPingFailed)
		return
	}
	if err := m.withRefresh(ctx, c, func() error { return m.refreshInfo(ctx, c, cl, false) }); err != nil {
		detail := DetailToolsFailed
		if errors.Is(err, errTooLarge) {
			detail = DetailToolsTooLarge
		}
		m.checkFailed(ctx, c, err, detail)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client == cl {
		_ = m.connectedLocked(ctx, c, cl)
	}
}

// redialOpen is the reconnect of an open server: a fresh client and check.
func (m *Manager) redialOpen(ctx context.Context, c *conn) (Started, error) {
	c.mu.Lock()
	old := c.client
	c.client = nil
	c.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	m.check(ctx, c)
	v, err := m.Get(ctx, c.id)
	if err != nil {
		return Started{}, err
	}
	if v.Detail == DetailOpenUnauthorized {
		return Started{Connector: v}, ErrAuthMismatch
	}
	return Started{Connector: v}, nil
}

// checkFailed records a failed check: a 401 on an open server, a rejected
// refresh (already expired), or one more failure. A check cut short by its
// ctx (the daemon stopping) records nothing.
func (m *Manager) checkFailed(ctx context.Context, c *conn, err error, detail string) {
	switch {
	case ctx.Err() != nil:
		return
	case errors.Is(err, ErrNeedsReconnect) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrClosed):
		return
	case errors.Is(err, errRefresh):
		detail = DetailRefreshFailed
	case errors.Is(err, mcp.ErrUnauthorized):
		c.mu.Lock()
		open := c.cfg.Auth != "oauth"
		c.mu.Unlock()
		if open {
			m.openUnauthorized(ctx, c)
			return
		}
		detail = DetailUnauthorized
	}
	m.failed(ctx, c, detail)
}

// failed counts one failed check; the maxFailures-th in a row moves a
// connected (or not yet checked) connector to degraded. A cancelled check
// is not a failure.
func (m *Manager) failed(ctx context.Context, c *conn, detail string) {
	if ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := m.now()
	_ = m.updateLocked(ctx, c, func(r *store.Connector) {
		r.Failures++
		r.LastCheck = &now
		switch r.State {
		case store.ConnectorConnected, store.ConnectorPending:
			if r.Failures >= maxFailures {
				r.State, r.Detail = store.ConnectorDegraded, detail
			}
		case store.ConnectorDegraded:
			r.Detail = detail
		}
	})
}

// openUnauthorized: a server configured open answered 401. The config
// contradicts the server; it is reported, never guessed around.
func (m *Manager) openUnauthorized(ctx context.Context, c *conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := m.now()
	_ = m.updateLocked(ctx, c, func(r *store.Connector) {
		r.Failures++
		r.LastCheck = &now
		r.State, r.Detail = store.ConnectorDegraded, DetailOpenUnauthorized
	})
}

// withRefresh runs f and, on an OAuth connector's 401, refreshes once and
// runs it again.
func (m *Manager) withRefresh(ctx context.Context, c *conn, f func() error) error {
	c.mu.Lock()
	gen, isOAuth := c.gen, c.cfg.Auth == "oauth"
	c.mu.Unlock()
	err := f()
	if !isOAuth || !errors.Is(err, mcp.ErrUnauthorized) {
		return err
	}
	if rerr := m.refresh(ctx, c, gen); rerr != nil {
		return rerr
	}
	return f()
}

// client returns c's MCP client, dialing one when there is none. An OAuth
// connector whose dial answers 401 refreshes once and dials once more.
func (m *Manager) client(ctx context.Context, c *conn) (*mcp.Client, error) {
	c.mu.Lock()
	cl, cfg, gen := c.client, c.cfg, c.gen
	c.mu.Unlock()
	if cl != nil {
		return cl, nil
	}
	cl, err := m.dial(ctx, c, cfg)
	if errors.Is(err, mcp.ErrUnauthorized) && cfg.Auth == "oauth" {
		if rerr := m.refresh(ctx, c, gen); rerr != nil {
			return nil, rerr
		}
		cl, err = m.dial(ctx, c, cfg)
	}
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil && !c.deleted { // dialed meanwhile
		go func() { _ = cl.Close() }()
		return c.client, nil
	}
	if err := m.adoptLocked(c, cl); err != nil {
		return nil, err
	}
	return cl, nil
}

// tokenSource is the bearer token of an OAuth connector for its MCP
// client, refreshed first when it expires within refreshMargin. A refresh
// that fails for another reason than a dead grant still sends the current
// token.
func (m *Manager) tokenSource(c *conn) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		if err := m.refreshIfDue(ctx, c, refreshMargin); errors.Is(err, ErrNeedsReconnect) || errors.Is(err, ErrNotFound) {
			return "", err
		}
		tok, _ := m.o.Secrets.Get(secrets.Connectors, c.id, secrets.ConnectorAccessToken)
		return tok, nil
	}
}

// refreshIfDue refreshes when the access token expires within d, unless
// a refresh failed less than refreshCooldown ago (a dead grant aside).
func (m *Manager) refreshIfDue(ctx context.Context, c *conn, d time.Duration) error {
	c.mu.Lock()
	exp, gen, failed := c.row.TokenExpiry, c.gen, c.refreshFailed
	c.mu.Unlock()
	now := m.now()
	if exp == nil || exp.Sub(now) >= d || (!failed.IsZero() && now.Sub(failed) < refreshCooldown) {
		return nil
	}
	return m.refresh(ctx, c, gen)
}

// refreshCooldown is how long a refresh that failed for a transient reason
// is not retried before a call or a check; a 401 still refreshes once.
const refreshCooldown = 30 * time.Second

// errRefresh wraps a refresh that failed for another reason than a dead
// grant.
var errRefresh = errors.New("connector: refresh failed")

// refresh trades the refresh token for new tokens, serialized per
// connector: a caller that saw generation seen does nothing when another
// refresh (or a code exchange) happened since. A rejected grant moves the
// connector to expired. Neither the request nor the writes are cancelled
// with ctx: a refresh token is single use and the answer must be stored.
func (m *Manager) refresh(ctx context.Context, c *conn, seen uint64) error {
	ctx = context.WithoutCancel(ctx)
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	c.mu.Lock()
	if c.deleted {
		c.mu.Unlock()
		return ErrNotFound
	}
	if c.gen != seen {
		c.mu.Unlock()
		return nil
	}
	am, clientID := parseAuthMeta(c.row.AuthMeta), c.row.ClientID
	c.mu.Unlock()

	var (
		tok oauth.Token
		err = oauth.ErrInvalidGrant
	)
	if am != nil {
		rt, _ := m.o.Secrets.Get(secrets.Connectors, c.id, secrets.ConnectorRefreshToken)
		secret, _ := m.o.Secrets.Get(secrets.Connectors, c.id, secrets.ConnectorClientSecret)
		id := oauth.Identity{Kind: am.IdentityKind, ClientID: clientID, ClientSecret: secret, Issuer: am.Metadata.Issuer, RedirectURI: am.RegisteredRedirect}
		tok, err = m.oauth.Refresh(ctx, am.Metadata, id, rt)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deleted {
		return ErrNotFound
	}
	if errors.Is(err, oauth.ErrInvalidGrant) {
		_ = m.updateLocked(ctx, c, func(r *store.Connector) {
			r.State, r.Detail = store.ConnectorExpired, DetailRefreshRejected
		})
		return &StateError{ID: c.id, State: store.ConnectorExpired}
	}
	if err != nil {
		c.refreshFailed = m.now()
		return fmt.Errorf("connector %s: %w: %w", c.id, errRefresh, err)
	}
	if err := m.o.Secrets.Set(secrets.Connectors, c.id, map[string]string{
		secrets.ConnectorAccessToken: tok.AccessToken, secrets.ConnectorRefreshToken: tok.RefreshToken,
	}); err != nil {
		c.refreshFailed = m.now()
		return fmt.Errorf("connector %s: %w: write auth.toml: %w", c.id, errRefresh, err)
	}
	c.gen++
	c.refreshFailed = time.Time{}
	if err := m.updateLocked(ctx, c, func(r *store.Connector) {
		r.TokenExpiry = expiry(tok.Expiry)
		if tok.Scopes != nil {
			r.Scopes = slices.Clone(tok.Scopes)
		}
	}); err != nil {
		return err
	}
	attrs := map[string]any{"kind": "connector", "id": c.id}
	if !tok.Expiry.IsZero() {
		attrs["expires_at"] = tok.Expiry.UTC().Format(time.RFC3339)
	}
	m.o.Events.Emit(ctx, event.New(event.AuthRefreshed, "", "", "", attrs))
	return nil
}

// Tools is the cached tool list of a connector, as its server declared it,
// except that every name matches [A-Za-z0-9_-]{1,64} and is unique: a
// tool with another name is dropped when listed, so "<id>.<name>" is
// always a safe tool name. A degraded or expired connector keeps its tools; one still being created
// has none.
func (m *Manager) Tools(id string) ([]mcp.Tool, error) {
	c, err := m.conn(id)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deleted || !c.inConfig {
		return nil, &StateError{ID: id, State: store.ConnectorPending}
	}
	return slices.Clone(c.tools), nil
}

// CallTool runs one tool of a connector for the tool adapter: through the
// connector's shared MCP client (4 in flight, 60 s each), refreshing first
// when the token is about to expire and once after a 401. An expired
// connector answers a StateError the model can read; a timeout is an
// error, not a state change. A server isError is mcp.Result.IsError.
func (m *Manager) CallTool(ctx context.Context, id, tool string, args json.RawMessage) (mcp.Result, error) {
	c, err := m.conn(id)
	if err != nil {
		return mcp.Result{}, err
	}
	c.mu.Lock()
	state := c.row.State
	if in := c.intent; in != nil && !in.create {
		state = in.prevState // the old tokens serve during a reconnect
	}
	ready, open := c.inConfig && !c.deleted, c.cfg.Auth != "oauth"
	c.mu.Unlock()
	switch {
	case !ready:
		return mcp.Result{}, &StateError{ID: id, State: store.ConnectorPending}
	case state == store.ConnectorExpired:
		return mcp.Result{}, &StateError{ID: id, State: state}
	}
	cl, err := m.client(ctx, c)
	if err != nil {
		return mcp.Result{}, callError(id, err)
	}
	var res mcp.Result
	err = m.withRefresh(ctx, c, func() error {
		var err error
		res, err = cl.CallTool(ctx, tool, args)
		return err
	})
	if err != nil {
		var se *StateError
		if errors.As(err, &se) {
			return mcp.Result{}, se
		}
		if errors.Is(err, mcp.ErrUnauthorized) && open {
			m.openUnauthorized(ctx, c)
		}
		return mcp.Result{}, callError(id, err)
	}
	return res, nil
}

// CallError is a failed MCP request. Its text names the connector and a
// cause from a closed set; the MCP error stays in the chain for errors.Is
// (mcp.ErrUnauthorized, context.DeadlineExceeded, ...).
type CallError struct {
	ID    string
	Cause string
	err   error
}

func (e *CallError) Error() string { return "connector " + e.ID + ": " + e.Cause }

func (e *CallError) Unwrap() error { return e.err }

func callError(id string, err error) error {
	return &CallError{ID: id, Cause: cause(err), err: err}
}

// toolsChanged follows tools/list_changed from cl, at most once per
// listChangedInterval. A client that is not c's (a probe's, or one
// replaced by a redial) is ignored; the trailing refresh lists from
// whichever client c has when it is due. The throttle runs on real time,
// as the timer does, not on Options.Now.
func (m *Manager) toolsChanged(c *conn, cl *mcp.Client) {
	c.mu.Lock()
	if c.client != cl || c.listTimer != nil || m.closed.Load() {
		c.mu.Unlock()
		return // not c's client, or a trailing refresh is due already
	}
	if wait := m.listInterval - time.Since(c.lastList); wait > 0 {
		c.listTimer = time.AfterFunc(wait, func() {
			c.mu.Lock()
			c.listTimer = nil
			cur := c.client // nil once deleted or closed
			c.mu.Unlock()
			if cur != nil {
				m.toolsChanged(c, cur)
			}
		})
		c.mu.Unlock()
		return
	}
	c.lastList = time.Now()
	c.mu.Unlock()
	if err := m.refreshTools(m.ctx, c, cl); err != nil && m.ctx.Err() == nil {
		m.log.Warn("connector tools not refreshed", "connector", c.id, "cause", cause(err))
	}
}

// refreshTools lists cl's tools into c's cache row, when cl is still c's
// client.
func (m *Manager) refreshTools(ctx context.Context, c *conn, cl *mcp.Client) error {
	listed, err := cl.ListTools(ctx)
	if err != nil {
		return err
	}
	tools, raw, err := m.cacheTools(c, listed)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != cl || bytes.Equal(raw, c.row.Tools) {
		return nil
	}
	if err := m.updateLocked(ctx, c, func(r *store.Connector) { r.Tools = raw }); err != nil {
		return err
	}
	c.tools = tools
	return nil
}
