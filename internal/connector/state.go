package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"holaclaw/internal/config"
	"holaclaw/internal/connector/mcp"
	"holaclaw/internal/connector/oauth"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// Details are the closed set of strings a connector.status event and the
// cache row carry in detail: daemon text, never server text or an error.
const (
	DetailIntentExpired    = "intent_expired"       // the authorization was not finished within the TTL
	DetailStateMismatch    = "state_mismatch"       // a code came back with another intent's state
	DetailAuthorization    = "authorization_failed" // the authorization server refused the code or the redirect carried an error
	DetailCIMDRefused      = "client_metadata_refused"
	DetailDiscovery        = "discovery_failed"       // no usable resource or authorization server metadata
	DetailClientIdentity   = "client_identity_failed" // no client id applies, or registration failed
	DetailAuthMismatch     = "auth_mismatch"          // the server does not use the auth the request named
	DetailUnreachable      = "server_unreachable"     // initialize did not succeed
	DetailPingFailed       = "ping_failed"
	DetailToolsFailed      = "tools_list_failed"
	DetailToolsTooLarge    = "tools_too_large"
	DetailRefreshFailed    = "refresh_failed"
	DetailRefreshRejected  = "refresh_rejected" // invalid_grant: reconnect
	DetailUnauthorized     = "unauthorized"     // a 401 survived a refresh
	DetailOpenUnauthorized = "open_server_unauthorized"
	DetailNeedsReconnect   = "needs_reconnect"     // an oauth entry without credentials, e.g. written by hand
	DetailURLChanged       = "url_changed"         // the tokens are bound to another URL
	DetailConfigWrite      = "config_write_failed" // connectors.toml or auth.toml could not be written
	DetailCacheWrite       = "cache_write_failed"  // the cache row could not be written
	// DetailDescriptionsCut is the detail of a connected connector with a
	// tool description over MaxDescriptionTokens, cut when cached.
	DetailDescriptionsCut = "descriptions_truncated"
)

// StateDeleted is the state of the last connector.status event of a
// connector; it never has a row.
const StateDeleted = "deleted"

var (
	// ErrInvalid is a request the manager refuses before any network call.
	ErrInvalid = errors.New("connector: invalid request")
	// ErrExists means the id is in use, in connectors.toml or by an open
	// create, or is still being deleted.
	ErrExists = errors.New("connector: id in use")
	// ErrNotFound means no connector has the id.
	ErrNotFound = errors.New("connector: no such connector")
	// ErrLeftovers means a delete removed the connector but not all of its
	// auth.toml keys or its cache row; deleting the id again removes them.
	ErrLeftovers = errors.New("connector: deleted, but removing its sign-in or cache failed")
	// ErrHandManaged means connectors.toml is a symlink, which the daemon
	// never writes through: the person edits it. The caller says how.
	ErrHandManaged = errors.New("connector: " + config.FileConnectors + " is a symlink the daemon does not write")
	// ErrBadFile means connectors.toml does not parse, or has a top-level
	// key config would refuse; the daemon never overwrites it.
	ErrBadFile = errors.New("connector: " + config.FileConnectors + " does not parse; fix it by hand")
	// ErrNoIntent means no authorization is open for the connector, or it
	// expired, or its code was already posted.
	ErrNoIntent = errors.New("connector: no open authorization")
	// ErrBusy means an authorization is still being opened or its code is
	// being exchanged.
	ErrBusy = errors.New("connector: authorization in progress")
	// ErrStateMismatch means the code's state is not the intent's; the
	// intent is dropped.
	ErrStateMismatch = errors.New("connector: state mismatch")
	// ErrAuthorization wraps a failed code exchange or redirect error.
	ErrAuthorization = errors.New("connector: authorization failed")
	// ErrAuthMismatch means the server contradicts the requested auth: a
	// 401 on auth = "none", or no OAuth on auth = "oauth".
	ErrAuthMismatch = errors.New("connector: server does not use the requested auth")
	// ErrDiscovery wraps a discovery or client identity failure.
	ErrDiscovery = errors.New("connector: discovery failed")
	// ErrUnreachable means initialize or tools/list did not succeed.
	ErrUnreachable = errors.New("connector: server unreachable")
	// ErrNeedsReconnect matches a StateError of an expired connector.
	ErrNeedsReconnect = errors.New("connector: needs reconnect")
	// ErrNotConnected matches a StateError of a connector still being created.
	ErrNotConnected = errors.New("connector: not connected")
	// ErrClosed means the manager was closed.
	ErrClosed = errors.New("connector: manager closed")
)

// StateError is a call on a connector whose state refuses it. Its text is
// meant for the model: it names the connector and what has to happen.
type StateError struct {
	ID    string
	State store.ConnectorState
}

func (e *StateError) Error() string {
	if e.State == store.ConnectorExpired {
		return "connector " + e.ID + " needs to be reconnected by an admin"
	}
	return "connector " + e.ID + " is not connected yet"
}

func (e *StateError) Is(target error) bool {
	if e.State == store.ConnectorExpired {
		return target == ErrNeedsReconnect
	}
	return target == ErrNotConnected
}

// cause folds an MCP error into words of our own: the SDK's text can quote
// the server.
func cause(err error) string {
	var se *StateError
	if errors.As(err, &se) {
		return se.Error()
	}
	switch {
	case errors.Is(err, mcp.ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, mcp.ErrTooLarge):
		return "response too large"
	case errors.Is(err, mcp.ErrSessionLost):
		return "session lost"
	case errors.Is(err, mcp.ErrClosed), errors.Is(err, ErrClosed):
		return "client closed"
	case errors.Is(err, errRefresh):
		return "token refresh failed"
	}
	return "request failed"
}

// Caps on the server's self-description in the cache row.
const (
	maxServerInfo = 256
	maxSessionID  = 4 << 10
)

// capInfo cuts a server name or version at maxServerInfo bytes, on a
// UTF-8 boundary.
func capInfo(s string) string {
	if len(s) <= maxServerInfo {
		return s
	}
	return strings.ToValidUTF8(s[:maxServerInfo], "")
}

// capSession drops a session id over maxSessionID: a cut one is useless.
func capSession(s string) string {
	if len(s) > maxSessionID {
		return ""
	}
	return s
}

// Caps on server-controlled JSON before PutConnector: the store does not
// bound them (docs/connectors.md, Appendix). Every writer of Tools or
// AuthMeta goes through marshalCapped. Variables for tests.
//
// maxToolsJSON is mcp's cap on a listing's names, descriptions and
// schemas, with no room for JSON overhead: a listing near that cap can
// exceed this one once re-marshalled, and then ends in tools_too_large.
// oauth caps every metadata field; maxAuthMetaJSON leaves room for the
// overhead.
var (
	maxToolsJSON    = mcp.MaxListBytes
	maxAuthMetaJSON = 64 << 10
)

var errTooLarge = errors.New("connector: cached JSON over its cap")

// authMeta is the AuthMeta JSON of the cache row: the discovered metadata
// of the grant the tokens in auth.toml belong to, how the client named
// itself, and the redirect a registered client was made for (reused only
// for that one). The granted client id is store.Connector.ClientID.
type authMeta struct {
	Metadata           oauth.Metadata     `json:"metadata"`
	IdentityKind       oauth.IdentityKind `json:"identity_kind,omitempty"`
	RegisteredRedirect string             `json:"registered_redirect,omitempty"`
	// ServerURL is the URL the grant was made for; the resource may be a
	// prefix of it. Empty on rows from before it was kept.
	ServerURL string `json:"server_url,omitempty"`
}

// parseAuthMeta reads a row's AuthMeta; nil when there is none.
func parseAuthMeta(raw json.RawMessage) *authMeta {
	var am authMeta
	if len(raw) == 0 || json.Unmarshal(raw, &am) != nil || am.Metadata.Issuer == "" {
		return nil
	}
	return &am
}

// parseTools reads a row's Tools; a bad cache is an empty list. The
// descriptions are capped again, for a row cached before the cap: a no-op on
// any other.
func parseTools(raw json.RawMessage) []mcp.Tool {
	var tools []mcp.Tool
	if len(raw) > 0 && json.Unmarshal(raw, &tools) != nil {
		return nil
	}
	for i := range tools {
		tools[i].Description = capDescription(tools[i].Description)
	}
	return tools
}

func marshalCapped(v any, limit int) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, errTooLarge
	}
	return b, nil
}

// updateLocked applies fn to c's cache row, persists it and emits
// connector.status when the state or the detail changed. A connected row
// says DetailDescriptionsCut while its tools carry a cut description, and
// "" once they no longer do. While a reconnect intent is open the row stays
// authorizing and the state fn sets becomes where the connector returns
// when the intent ends. c.mu is held.
func (m *Manager) updateLocked(ctx context.Context, c *conn, fn func(*store.Connector)) error {
	if c.deleted {
		return ErrNotFound
	}
	next := c.row
	next.Scopes = slices.Clone(c.row.Scopes)
	fn(&next)
	tools := c.tools // parsed from c.row.Tools
	if !bytes.Equal(next.Tools, c.row.Tools) {
		tools = parseTools(next.Tools)
	}
	next.Detail = toolsDetail(next.State, next.Detail, tools)
	if in := c.intent; in != nil && !in.create {
		if next.State != store.ConnectorAuthorizing || next.Detail != "" {
			in.prevState, in.prevDetail = next.State, next.Detail
		}
		next.State, next.Detail = store.ConnectorAuthorizing, ""
	}
	next.ID = c.id
	next.UpdatedAt = m.now()
	if err := m.o.Store.PutConnector(ctx, next); err != nil {
		return fmt.Errorf("connector %s: %w", c.id, err)
	}
	prev := c.row
	c.row = next
	if prev.State != next.State || prev.Detail != next.Detail {
		m.emit(ctx, c.id, string(next.State), next.Detail, next.Scopes)
	}
	return nil
}

// toolsDetail is the detail of a row in state with detail and tools: a
// connected row says DetailDescriptionsCut exactly while a description is
// cut; any other state keeps its own detail.
func toolsDetail(state store.ConnectorState, detail string, tools []mcp.Tool) string {
	if state != store.ConnectorConnected || (detail != "" && detail != DetailDescriptionsCut) {
		return detail
	}
	if descriptionsCut(tools) > 0 {
		return DetailDescriptionsCut
	}
	return ""
}

// emit sends one connector.status event. Nothing in it is a credential.
func (m *Manager) emit(ctx context.Context, id, state, detail string, scopes []string) {
	attrs := map[string]any{"id": id, "state": state, "detail": detail}
	if len(scopes) > 0 {
		attrs["scopes"] = scopes
	}
	m.o.Events.Emit(ctx, event.New(event.ConnectorStatus, event.NewID(), "", "", attrs))
}
