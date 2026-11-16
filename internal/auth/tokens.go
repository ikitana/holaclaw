package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// Scope is one of the two fixed API scopes (docs/identity.md). They are part
// of the OpenAPI contract: add, never rename.
type Scope string

const (
	ScopeAdmin  Scope = "admin"
	ScopeClient Scope = "client"
)

// Valid reports whether s is one of the two scopes.
func (s Scope) Valid() bool {
	return s == ScopeAdmin || s == ScopeClient
}

// Allows reports whether a principal holding s may call an endpoint declaring
// required: admin contains client.
func (s Scope) Allows(required Scope) bool {
	if !required.Valid() {
		return false
	}
	switch s {
	case ScopeAdmin:
		return true
	case ScopeClient:
		return required == ScopeClient
	}
	return false
}

// initial is the one-letter scope marker in a token plaintext.
func (s Scope) initial() string { return string(s[0]) }

// Origin is how a token came to be (docs/identity.md §3).
type Origin string

const (
	OriginPair        Origin = "pair" // OriginRef is the invite
	OriginTokenCreate Origin = "token_create"
	OriginRotate      Origin = "rotate" // OriginRef is the token it replaced
	OriginInit        Origin = "init"
)

// tokenPrefix marks every holaclaw token; a value without it is rejected before
// any hashing happens.
const tokenPrefix = "hb_"

// tokenBytes is the entropy behind a generated token, hex encoded.
const tokenBytes = 32

// lastUsedThrottle is the minimum interval between last_used_at writes, so a
// busy client does not turn every request into a write transaction.
const lastUsedThrottle = time.Minute

// SocketAlias is the alias of holaclaw:cli, the binding the unix socket
// arrives as: a binding of the owner with no tokens (docs/identity.md §3).
const SocketAlias = "cli"

// Token is an API token as stored: the plaintext is not part of it.
type Token struct {
	ID    event.ID
	Name  string
	Scope Scope
	// Binding is the alias of the token's holaclaw binding, holaclaw:<Binding>.
	Binding string
	// IdentityID is the binding's identity; empty once the binding is gone,
	// which revoked the token.
	IdentityID event.ID
	Origin     Origin
	// OriginRef is the invite a paired token came from, or the token a
	// rotation replaced; empty for the other origins.
	OriginRef event.ID
	// CreatedBy and RevokedBy name the caller as events do (Actor); empty when
	// nobody is known.
	CreatedBy  event.ID
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
	RevokedBy  event.ID
}

// Revoked reports whether the token has been revoked.
func (t Token) Revoked() bool { return t.RevokedAt != nil }

// Principal is the authenticated caller behind one request.
type Principal struct {
	TokenID event.ID // empty for the unix socket
	// Binding is the alias of the holaclaw binding the caller arrives from:
	// the token's, or SocketAlias.
	Binding string
	Scope   Scope
	UserID  event.ID // the binding's identity
	Role    store.Role
}

// SocketPrincipal resolves the caller on the unix socket, which arrives as
// holaclaw:cli with admin scope by file permission. store.ErrNotFound when the
// binding does not exist, before `holaclaw init`.
func (s *Service) SocketPrincipal(ctx context.Context) (Principal, error) {
	return s.principal(ctx, SocketAlias, ScopeAdmin, "")
}

// principal resolves a holaclaw binding to the caller it signs in, the way
// every message and request resolves (docs/identity.md §1):
//
//  1. binding: the binding holding alias; none is store.ErrNotFound
//  2. identity: the binding's identity
//  3. status: a disabled identity is ErrIdentityDisabled
//  4. role: RoleOf the identity
//  5. scope: the lower of scope and what the role allows (capScope)
//
// Nothing is cached, so a new status or role applies to the next request.
func (s *Service) principal(ctx context.Context, alias string, scope Scope, tokenID event.ID) (Principal, error) {
	// One alias the daemon issued, in no namespace: the recycled-id guard of
	// store.ResolveAliases has nothing to check, and this runs on every request.
	i, err := store.BindingIdentityIn(ctx, s.db, event.ChannelHolaclaw, alias)
	if err != nil {
		return Principal{}, err
	}
	if i.Kind == store.KindRoom {
		// A room's binding never signs anyone in.
		return Principal{}, store.ErrNotFound
	}
	if i.Disabled() {
		return Principal{}, ErrIdentityDisabled
	}
	role := RoleOf(i)
	return Principal{TokenID: tokenID, Binding: alias, Scope: capScope(scope, role), UserID: i.ID, Role: role}, nil
}

// RoleOf is the role an identity acts with, step 4 of the resolution
// (docs/identity.md §1): a pending identity runs as a guest, and a room, which
// never grants anything, as one too. It is its own step, so another source can
// compute the role later without touching the others.
func RoleOf(i store.Identity) store.Role {
	if i.Pending() || i.Kind == store.KindRoom {
		return store.RoleGuest
	}
	return i.Role
}

// capScope is step 5 of the resolution: a token never acts above its
// identity's role. Owner and admin allow admin; any other role caps it at
// client.
func capScope(scope Scope, role store.Role) Scope {
	if scope == ScopeAdmin && role != store.RoleOwner && role != store.RoleAdmin {
		return ScopeClient
	}
	return scope
}

// CreateToken is MintToken with no caller to check: it is for trusted setup,
// such as tests, and may bind a pending identity to run a client as a guest. A
// request on someone's behalf goes through MintToken, which keeps tokens for
// the owner owner-only and refuses a pending identity.
func (s *Service) CreateToken(ctx context.Context, name string, scope Scope, identityID event.ID) (string, Token, error) {
	return s.mint(ctx, Principal{Role: store.RoleOwner}, true, name, scope, identityID)
}

// MintToken mints a token on behalf of a caller (`token create --identity`).
// The plaintext is returned once and never stored.
//
// A token belongs to a new holaclaw binding on identityID, named after the
// token, and created with it. An empty scope is client, and admin needs the
// role owner or admin (ErrScopeAboveRole). A binding acts with its identity's
// role, so only an owner may mint one for the owner (ErrOwnerOnly). An
// identity that does not exist is store.ErrNotFound, none at all
// ErrInvalidIdentity, a room ErrRoomIdentity, a disabled identity ErrDisabled
// and a pending one ErrPending: only its own messages bind a pending identity.
// The checks and the inserts are one transaction.
func (s *Service) MintToken(ctx context.Context, by Principal, name string, scope Scope, identityID event.ID) (string, Token, error) {
	return s.mint(ctx, by, false, name, scope, identityID)
}

func (s *Service) mint(ctx context.Context, by Principal, setup bool, name string, scope Scope, identityID event.ID) (string, Token, error) {
	if identityID == "" {
		return "", Token{}, fmt.Errorf("%w: a token needs an identity", ErrInvalidIdentity)
	}
	var actor event.ID // trusted setup has no caller to record
	if !setup {
		actor = s.Actor(ctx, by)
	}
	plaintext, tok, err := s.newToken(name, scope, OriginTokenCreate, "", actor)
	if err != nil {
		return "", Token{}, err
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		i, err := store.IdentityIn(ctx, tx, identityID)
		if err != nil {
			return err
		}
		if err := mayHoldToken(i, by, tok.Scope, setup); err != nil {
			return err
		}
		return s.bindTokenIn(ctx, tx, &tok, hashSecret(plaintext), identityID)
	})
	if err != nil {
		return "", Token{}, tokenErr("create token", err)
	}
	s.emit(ctx, event.IdentityBound, map[string]any{
		"identity": tok.IdentityID, "binding": bindingRef(event.ChannelHolaclaw, tok.Binding), "token": tok.ID, "by": actor,
	})
	s.emitTokenCreated(ctx, tok)
	return plaintext, tok, nil
}

// AddToken mints a token on an existing holaclaw binding (`token create
// --binding`), for several automations sharing one. The plaintext is returned
// once. It refuses what MintToken refuses, an unknown binding is
// store.ErrNotFound, and holaclaw:cli, which arrives by file permission and
// holds no tokens, ErrSocketBinding.
func (s *Service) AddToken(ctx context.Context, by Principal, alias, name string, scope Scope) (string, Token, error) {
	if alias == SocketAlias {
		return "", Token{}, ErrSocketBinding
	}
	plaintext, tok, err := s.newToken(name, scope, OriginTokenCreate, "", s.Actor(ctx, by))
	if err != nil {
		return "", Token{}, err
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		i, err := store.BindingIdentityIn(ctx, tx, event.ChannelHolaclaw, alias)
		if err != nil {
			return err
		}
		if err := mayHoldToken(i, by, tok.Scope, false); err != nil {
			return err
		}
		tok.Binding, tok.IdentityID = alias, i.ID
		return insertTokenIn(ctx, tx, tok, hashSecret(plaintext))
	})
	if err != nil {
		return "", Token{}, tokenErr("create token", err)
	}
	s.emitTokenCreated(ctx, tok)
	return plaintext, tok, nil
}

// mayHoldToken checks that a new token at scope may be minted on identity i for
// by. setup lets a pending identity through, as a guest.
func mayHoldToken(i store.Identity, by Principal, scope Scope, setup bool) error {
	switch {
	case i.Kind == store.KindRoom:
		return ErrRoomIdentity
	case i.Role == store.RoleOwner && by.Role != store.RoleOwner:
		return ErrOwnerOnly
	case i.Disabled():
		return ErrDisabled
	case i.Pending() && !setup:
		return ErrPending
	case capScope(scope, RoleOf(i)) != scope:
		return ErrScopeAboveRole
	}
	return nil
}

// tokenErr passes the refusals a caller can act on through, and wraps the rest.
func tokenErr(op string, err error) error {
	for _, known := range []error{
		store.ErrNotFound, ErrOwnerOnly, ErrRoomIdentity, ErrDisabled, ErrPending, ErrScopeAboveRole,
		ErrSocketBinding, ErrRevoked,
	} {
		if errors.Is(err, known) {
			return err
		}
	}
	return fmt.Errorf("auth: %s: %w", op, err)
}

// bindTokenIn creates a holaclaw binding on identityID named after the token,
// with a generated alias, and inserts the token on it.
func (s *Service) bindTokenIn(ctx context.Context, tx *sql.Tx, tok *Token, hash string, identityID event.ID) error {
	alias := string(event.NewID())
	if _, err := linkIn(ctx, tx, event.ChannelHolaclaw, []string{alias}, identityID, tok.Name, s.now()); err != nil {
		return err
	}
	tok.Binding, tok.IdentityID = alias, identityID
	return insertTokenIn(ctx, tx, *tok, hash)
}

// newToken generates a plaintext and the row describing it; nothing is
// stored. An empty scope is client.
func (s *Service) newToken(name string, scope Scope, origin Origin, ref, by event.ID) (string, Token, error) {
	if scope == "" {
		scope = ScopeClient
	}
	if !scope.Valid() {
		return "", Token{}, fmt.Errorf("%w: %q", ErrInvalidScope, scope)
	}
	if name == "" {
		return "", Token{}, errors.New("auth: token name is required")
	}
	b, err := randomBytes(tokenBytes)
	if err != nil {
		return "", Token{}, fmt.Errorf("auth: create token: %w", err)
	}
	plaintext := tokenPrefix + scope.initial() + "_" + hex.EncodeToString(b)
	tok := Token{
		ID:        event.NewID(),
		Name:      name,
		Scope:     scope,
		Origin:    origin,
		OriginRef: ref,
		CreatedBy: by,
		CreatedAt: s.now(),
	}
	return plaintext, tok, nil
}

// SetTokenScope raises or lowers a token's scope (`token set`). Raising is
// always this explicit call: a promotion raises no token. admin needs the
// identity's role to be owner or admin (ErrScopeAboveRole), only the owner
// changes the owner's tokens (ErrOwnerOnly), a revoked token is ErrRevoked,
// and one of a disabled identity ErrDisabled, as for a new token. Setting the
// scope it has changes nothing, with no event.
func (s *Service) SetTokenScope(ctx context.Context, by Principal, id event.ID, scope Scope) (Token, error) {
	if !scope.Valid() {
		return Token{}, fmt.Errorf("%w: %q", ErrInvalidScope, scope)
	}
	var (
		t   Token
		old Scope
	)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var (
			i   store.Identity
			err error
		)
		if t, i, err = activeTokenIn(ctx, tx, id); err != nil {
			return err
		}
		switch {
		case i.Role == store.RoleOwner && by.Role != store.RoleOwner:
			return ErrOwnerOnly
		case i.Disabled():
			return ErrDisabled
		case capScope(scope, RoleOf(i)) != scope:
			return ErrScopeAboveRole
		}
		old, t.Scope = t.Scope, scope
		_, err = tx.ExecContext(ctx, `UPDATE api_tokens SET scope = ? WHERE id = ?`, string(scope), string(id))
		return err
	})
	if err != nil {
		return Token{}, tokenErr("set token scope", err)
	}
	if old != scope {
		s.emit(ctx, event.TokenUpdated, map[string]any{
			"identity": t.IdentityID, "binding": bindingRef(event.ChannelHolaclaw, t.Binding), "token": t.ID,
			"old_scope": old, "scope": scope, "by": s.Actor(ctx, by),
		})
	}
	return t, nil
}

// RotateToken replaces a token with a new one on the same binding, with the
// same name and scope, and revokes the old one in the same transaction. The new
// plaintext is returned once. Only the owner rotates the owner's tokens
// (ErrOwnerOnly), a revoked token is ErrRevoked, and one of a disabled
// identity ErrDisabled: no new token is minted for it.
func (s *Service) RotateToken(ctx context.Context, by Principal, id event.ID) (string, Token, error) {
	actor := s.Actor(ctx, by)
	var (
		plaintext string
		tok       Token
	)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		old, i, err := activeTokenIn(ctx, tx, id)
		if err != nil {
			return err
		}
		switch {
		case i.Role == store.RoleOwner && by.Role != store.RoleOwner:
			return ErrOwnerOnly
		case i.Disabled():
			return ErrDisabled
		}
		if plaintext, tok, err = s.newToken(old.Name, old.Scope, OriginRotate, old.ID, actor); err != nil {
			return err
		}
		tok.Binding, tok.IdentityID = old.Binding, old.IdentityID
		n, err := exec(ctx, tx, `UPDATE api_tokens SET revoked_at = ?, revoked_by = ? WHERE id = ? AND revoked_at IS NULL`,
			ms(tok.CreatedAt), string(actor), string(id))
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrRevoked
		}
		return insertTokenIn(ctx, tx, tok, hashSecret(plaintext))
	})
	if err != nil {
		return "", Token{}, tokenErr("rotate token", err)
	}
	s.emitTokenCreated(ctx, tok)
	s.emit(ctx, event.TokenRevoked, map[string]any{
		"identity": tok.IdentityID, "binding": bindingRef(event.ChannelHolaclaw, tok.Binding), "token": id,
		"by": actor, "reason": "rotated", "replaced_by": tok.ID,
	})
	return plaintext, tok, nil
}

// RotateOwnToken is RotateToken on the token the caller signed in with
// (`POST /v1/me/token/rotate`). The unix socket has none (ErrNoToken).
func (s *Service) RotateOwnToken(ctx context.Context, p Principal) (string, Token, error) {
	if p.TokenID == "" {
		return "", Token{}, ErrNoToken
	}
	return s.RotateToken(ctx, p, p.TokenID)
}

// activeTokenIn reads a token that is not revoked and its binding's identity.
// An unknown id is store.ErrNotFound; a revoked token, or one whose binding is
// gone, which revoked it, ErrRevoked.
func activeTokenIn(ctx context.Context, tx *sql.Tx, id event.ID) (Token, store.Identity, error) {
	t, err := getTokenIn(ctx, tx, id)
	if err != nil {
		return Token{}, store.Identity{}, err
	}
	if t.Revoked() || t.IdentityID == "" {
		return Token{}, store.Identity{}, ErrRevoked
	}
	i, err := store.IdentityIn(ctx, tx, t.IdentityID)
	return t, i, err
}

// tokenColumns are read through tokenFrom, which joins each token to its
// binding for the identity.
const (
	tokenColumns = `t.id, t.name, t.scope, t.binding_id, b.identity_id, t.origin, t.origin_ref, t.created_by,
		t.created_at, t.last_used_at, t.revoked_at, t.revoked_by`
	tokenFrom = ` FROM api_tokens t LEFT JOIN bindings b ON b.channel = '` + event.ChannelHolaclaw + `' AND b.alias = t.binding_id`
)

func scanToken(sc interface{ Scan(...any) error }, extra ...any) (Token, error) {
	var (
		t                                Token
		id, scope, origin, createdBy     string
		identityID, originRef, revokedBy sql.NullString
		created                          int64
		lastUsed, revoked                sql.NullInt64
	)
	dest := append(extra[:len(extra):len(extra)], &id, &t.Name, &scope, &t.Binding, &identityID, &origin, &originRef,
		&createdBy, &created, &lastUsed, &revoked, &revokedBy)
	if err := sc.Scan(dest...); err != nil {
		return Token{}, err
	}
	t.ID, t.Scope, t.IdentityID = event.ID(id), Scope(scope), nullID(identityID)
	t.Origin, t.OriginRef, t.CreatedBy, t.RevokedBy = Origin(origin), nullID(originRef), event.ID(createdBy), nullID(revokedBy)
	t.CreatedAt, t.LastUsedAt, t.RevokedAt = fromMS(created), nullMS(lastUsed), nullMS(revoked)
	return t, nil
}

// Authenticate resolves a token plaintext to its principal: the token's
// binding, then the resolution every request goes through (principal).
// Unknown, malformed and revoked tokens, and a token whose binding is gone, are
// all ErrTokenInvalid; a good token of a disabled identity is
// ErrIdentityDisabled, and stays good. On success last_used_at is refreshed at
// most once a minute.
func (s *Service) Authenticate(ctx context.Context, plaintext string) (Principal, error) {
	if !strings.HasPrefix(plaintext, tokenPrefix) {
		return Principal{}, ErrTokenInvalid
	}
	hash := hashSecret(plaintext)
	// The lookup is by hash, so the row is already the only candidate; the
	// constant-time compare keeps the code honest if that ever changes.
	var stored string
	t, err := scanToken(s.db.QueryRowContext(ctx,
		`SELECT t.hash, `+tokenColumns+tokenFrom+` WHERE t.hash = ?`, hash), &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrTokenInvalid
	}
	if err != nil {
		return Principal{}, fmt.Errorf("auth: authenticate: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(hash)) != 1 || t.Revoked() {
		return Principal{}, ErrTokenInvalid
	}
	p, err := s.principal(ctx, t.Binding, t.Scope, t.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Principal{}, ErrTokenInvalid
	case errors.Is(err, ErrIdentityDisabled):
		return Principal{}, err
	case err != nil:
		return Principal{}, fmt.Errorf("auth: authenticate: %w", err)
	}
	if err := s.touchToken(ctx, t); err != nil {
		return Principal{}, err
	}
	return p, nil
}

// touchToken refreshes last_used_at, skipping the write when the stored value
// is younger than lastUsedThrottle.
func (s *Service) touchToken(ctx context.Context, t Token) error {
	now := s.now()
	if t.LastUsedAt != nil && now.Sub(*t.LastUsedAt) < lastUsedThrottle {
		return nil
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, ms(now), string(t.ID))
		return err
	})
	if err != nil {
		return fmt.Errorf("auth: touch token: %w", err)
	}
	return nil
}

// TokenFilter narrows ListTokens. The zero value lists every active token.
type TokenFilter struct {
	// IdentityID keeps the tokens on that identity's bindings. A token whose
	// binding is gone has no identity any more and never matches.
	IdentityID event.ID
	// Binding keeps the tokens of one holaclaw binding, by alias.
	Binding string
	// All includes revoked tokens.
	All bool
}

// ListTokens returns the tokens f keeps, oldest first. Hashes are never
// exposed.
func (s *Service) ListTokens(ctx context.Context, f TokenFilter) ([]Token, error) {
	var (
		where []string
		args  []any
	)
	if !f.All {
		where = append(where, `t.revoked_at IS NULL`)
	}
	if f.IdentityID != "" {
		where, args = append(where, `b.identity_id = ?`), append(args, string(f.IdentityID))
	}
	if f.Binding != "" {
		where, args = append(where, `t.binding_id = ?`), append(args, f.Binding)
	}
	q := `SELECT ` + tokenColumns + tokenFrom
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	return listTokensIn(ctx, s.db, q+` ORDER BY t.created_at, t.id`, args...)
}

func listTokensIn(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, query string, args ...any) ([]Token, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("auth: list tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("auth: list tokens: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ActiveTokens counts the active tokens on each holaclaw binding of an
// identity, by alias. A binding with none is in the map with zero.
func (s *Service) ActiveTokens(ctx context.Context, identityID event.ID) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT b.alias, count(t.id) FROM bindings b
		 LEFT JOIN api_tokens t ON t.binding_id = b.alias AND t.revoked_at IS NULL
		 WHERE b.channel = ? AND b.identity_id = ? GROUP BY b.alias`, event.ChannelHolaclaw, string(identityID))
	if err != nil {
		return nil, fmt.Errorf("auth: count tokens: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var (
			alias string
			n     int
		)
		if err := rows.Scan(&alias, &n); err != nil {
			return nil, fmt.Errorf("auth: count tokens: %w", err)
		}
		out[alias] = n
	}
	return out, rows.Err()
}

// RevokeToken marks a token revoked, recording by; revoking twice keeps the
// first time and the first revoker. Its binding and the binding's other tokens
// stay. Only the owner revokes the owner's tokens (ErrOwnerOnly). A token a
// rotation already replaced, with a replacement still active, is a
// *RotatedError naming it: a quiet success would hide it from whoever meant to
// cut the credential.
func (s *Service) RevokeToken(ctx context.Context, by Principal, id event.ID) error {
	actor := s.Actor(ctx, by)
	var (
		t Token
		n int
	)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		if t, err = getTokenIn(ctx, tx, id); err != nil {
			return err
		}
		if t.IdentityID != "" && by.Role != store.RoleOwner {
			if i, err := store.IdentityIn(ctx, tx, t.IdentityID); err != nil {
				return err
			} else if i.Role == store.RoleOwner {
				return ErrOwnerOnly
			}
		}
		n, err = exec(ctx, tx, `UPDATE api_tokens SET revoked_at = ?, revoked_by = ? WHERE id = ? AND revoked_at IS NULL`,
			ms(s.now()), string(actor), string(id))
		if err != nil || n > 0 {
			return err
		}
		// Already revoked: follow its rotations to the one still active, if any.
		var into string
		err = tx.QueryRowContext(ctx, `WITH RECURSIVE chain(id, revoked_at) AS (
			  SELECT id, revoked_at FROM api_tokens WHERE origin = 'rotate' AND origin_ref = ?
			  UNION ALL
			  SELECT t.id, t.revoked_at FROM api_tokens t JOIN chain c ON t.origin = 'rotate' AND t.origin_ref = c.id)
			SELECT id FROM chain WHERE revoked_at IS NULL LIMIT 1`, string(id)).Scan(&into)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		return &RotatedError{Into: event.ID(into)}
	})
	if err != nil {
		return tokenErr("revoke token", err)
	}
	if n > 0 {
		s.emit(ctx, event.TokenRevoked, map[string]any{
			"identity": t.IdentityID, "binding": bindingRef(event.ChannelHolaclaw, t.Binding), "token": id, "by": actor,
		})
	}
	return nil
}

func getTokenIn(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id event.ID) (Token, error) {
	t, err := scanToken(q.QueryRowContext(ctx, `SELECT `+tokenColumns+tokenFrom+` WHERE t.id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return Token{}, store.ErrNotFound
	}
	return t, err
}

// revokeBindingTokensIn revokes the active tokens of one holaclaw binding, for
// a binding about to go, and returns them as they were.
func revokeBindingTokensIn(ctx context.Context, tx *sql.Tx, alias string, now time.Time, by event.ID) ([]Token, error) {
	return revokeTokensIn(ctx, tx, `t.binding_id = ?`, []any{alias}, now, by)
}

// revokeIdentityTokensIn revokes the active tokens on every holaclaw binding of
// an identity, for an identity about to go, and returns them as they were.
func revokeIdentityTokensIn(ctx context.Context, tx *sql.Tx, id event.ID, now time.Time, by event.ID) ([]Token, error) {
	return revokeTokensIn(ctx, tx, `b.identity_id = ?`, []any{string(id)}, now, by)
}

// revokeTokensIn revokes the active tokens matching where, read before the
// binding they join through goes.
func revokeTokensIn(ctx context.Context, tx *sql.Tx, where string, args []any, now time.Time, by event.ID) ([]Token, error) {
	match := ` WHERE t.revoked_at IS NULL AND ` + where
	toks, err := listTokensIn(ctx, tx, `SELECT `+tokenColumns+tokenFrom+match, args...)
	if err != nil || len(toks) == 0 {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ?, revoked_by = ? WHERE id IN (SELECT t.id`+tokenFrom+match+`)`,
		append([]any{ms(now), string(by)}, args...)...); err != nil {
		return nil, err
	}
	return toks, nil
}

// lowerTokensIn sets the active admin tokens of an identity to client, for a
// role that no longer allows admin, and returns them as they were.
func lowerTokensIn(ctx context.Context, tx *sql.Tx, id event.ID) ([]Token, error) {
	match := ` WHERE t.revoked_at IS NULL AND t.scope = 'admin' AND b.identity_id = ?`
	toks, err := listTokensIn(ctx, tx, `SELECT `+tokenColumns+tokenFrom+match, string(id))
	if err != nil || len(toks) == 0 {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE api_tokens SET scope = 'client' WHERE id IN (SELECT t.id`+tokenFrom+match+`)`, string(id))
	return toks, err
}

// emitTokensRevoked reports the tokens an unbind or a delete revoked; reason
// says which.
func (s *Service) emitTokensRevoked(ctx context.Context, toks []Token, by event.ID, reason string) {
	for _, t := range toks {
		s.emit(ctx, event.TokenRevoked, map[string]any{
			"identity": t.IdentityID, "binding": bindingRef(event.ChannelHolaclaw, t.Binding), "token": t.ID,
			"by": by, "reason": reason,
		})
	}
}

// emitTokenCreated reports a new token, and the token a rotation replaced.
func (s *Service) emitTokenCreated(ctx context.Context, t Token) {
	attrs := map[string]any{
		"identity": t.IdentityID, "binding": bindingRef(event.ChannelHolaclaw, t.Binding), "token": t.ID,
		"scope": t.Scope, "origin": t.Origin, "by": t.CreatedBy,
	}
	switch t.Origin {
	case OriginRotate:
		attrs["replaces"] = t.OriginRef
	case OriginPair:
		attrs["invite"] = t.OriginRef
	}
	s.emit(ctx, event.TokenCreated, attrs)
}

func insertTokenIn(ctx context.Context, tx *sql.Tx, t Token, hash string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO api_tokens (id, name, scope, hash, binding_id, origin, origin_ref, created_by, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(t.ID), t.Name, string(t.Scope), hash, t.Binding, string(t.Origin), idArg(t.OriginRef),
		string(t.CreatedBy), ms(t.CreatedAt))
	return err
}
