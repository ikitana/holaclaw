package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"holaclaw/internal/agent"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// maxPlatformLen caps the platform a device reports: it is a short word
// ("ios", "macos"), shown as is.
const maxPlatformLen = 32

// ErrInvalidDevice refuses a pairing with no device name, or a platform that is
// not a short lower-case word.
var ErrInvalidDevice = errors.New("auth: invalid device")

// ErrScopeAboveCaller refuses a pairing link above the scope of the client
// that asks for it: self-service never grants more than the caller holds.
var ErrScopeAboveCaller = errors.New("auth: scope above the caller's")

// Pair redeems a holaclaw invite for a device (`POST /v1/pair`): the code is
// the credential. It creates a new holaclaw binding on the invited identity,
// named after the device, with its first token, and returns the token once.
// The token has the invite's scope, lowered to what the identity's role allows
// now; its origin is pair, with the invite.
//
// source is the caller's network address. Failed codes are limited per source
// as redeems are per alias (ErrRateLimited); an attempt counts from the moment
// it starts, so concurrent ones share the limit. An unknown, used, revoked or
// expired code, one for another channel, and one whose identity has since gone
// or been disabled, are all ErrInviteInvalid; only a code that pairs is spent.
func (s *Service) Pair(ctx context.Context, code, device, platform, source string) (string, Token, error) {
	device = agent.SanitizeDisplay(device)
	if device == "" {
		return "", Token{}, fmt.Errorf("%w: a device name is required", ErrInvalidDevice)
	}
	if !validPlatform(platform) {
		return "", Token{}, fmt.Errorf("%w: the platform is a lower-case word of at most %d characters", ErrInvalidDevice, maxPlatformLen)
	}
	release, ok := s.reserveAttempt(event.ChannelHolaclaw + "\x00addr:" + source)
	if !ok {
		return "", Token{}, ErrRateLimited
	}
	var (
		plaintext string
		tok       Token
	)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		c, err := consume(ctx, tx, code, InviteDM, event.ChannelHolaclaw, s.now())
		if err != nil {
			return err
		}
		i, err := store.IdentityIn(ctx, tx, event.ID(c.userID.String))
		switch {
		case errors.Is(err, store.ErrNotFound):
			return ErrInviteInvalid
		case err != nil:
			return err
		case i.Kind == store.KindRoom, i.Pending(), i.Disabled():
			// The invite was checked when it was minted; an identity that
			// changed since answers like a spent code, and keeps it.
			return ErrInviteInvalid
		}
		scope := capScope(Scope(c.scope.String), RoleOf(i))
		if plaintext, tok, err = s.newToken(device, scope, OriginPair, event.ID(c.id), event.ID(c.createdBy)); err != nil {
			return err
		}
		if err := s.bindTokenIn(ctx, tx, &tok, hashSecret(plaintext), i.ID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE bindings SET platform = ? WHERE channel = ? AND alias = ?`,
			platform, event.ChannelHolaclaw, tok.Binding)
		return err
	})
	// The attempt was counted up front; only a wrong code keeps it.
	switch {
	case errors.Is(err, ErrInviteInvalid), errors.Is(err, ErrInviteChannel):
		return "", Token{}, ErrInviteInvalid
	case err != nil:
		release()
		return "", Token{}, fmt.Errorf("auth: pair: %w", err)
	}
	release()
	s.emit(ctx, event.IdentityBound, map[string]any{
		"identity": tok.IdentityID, "binding": bindingRef(event.ChannelHolaclaw, tok.Binding), "invite": tok.OriginRef,
		"token": tok.ID, "platform": platform,
	})
	s.emitTokenCreated(ctx, tok)
	return plaintext, tok, nil
}

// validPlatform reports whether p is a short word of lower-case letters,
// digits and dashes.
func validPlatform(p string) bool {
	if p == "" || len(p) > maxPlatformLen {
		return false
	}
	for _, r := range p {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// Device is one holaclaw binding of an identity, as its own clients see it.
type Device struct {
	store.Binding
	// Tokens counts the binding's active tokens.
	Tokens int
	// LastUsedAt is the latest use of any of the binding's tokens, revoked
	// ones included; nil when none was ever used.
	LastUsedAt *time.Time
}

// Devices lists the holaclaw bindings of the caller's identity, oldest first
// (`GET /v1/me/devices`): its devices, its automations and, for the owner,
// holaclaw:cli. One statement reads the bindings with their tokens, so the
// counts always match the list.
func (s *Service) Devices(ctx context.Context, by Principal) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT b.alias, b.display, b.platform, b.created_at,
		        count(CASE WHEN t.id IS NOT NULL AND t.revoked_at IS NULL THEN 1 END), max(t.last_used_at)
		 FROM bindings b LEFT JOIN api_tokens t ON t.binding_id = b.alias
		 WHERE b.channel = ? AND b.identity_id = ?
		 GROUP BY b.alias ORDER BY b.created_at, b.alias`, event.ChannelHolaclaw, string(by.UserID))
	if err != nil {
		return nil, fmt.Errorf("auth: list devices: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Device{}
	for rows.Next() {
		var (
			d        Device
			created  int64
			lastUsed sql.NullInt64
		)
		if err := rows.Scan(&d.Alias, &d.Display, &d.Platform, &created, &d.Tokens, &lastUsed); err != nil {
			return nil, fmt.Errorf("auth: list devices: %w", err)
		}
		d.Channel, d.IdentityID, d.CreatedAt, d.LastUsedAt = event.ChannelHolaclaw, by.UserID, fromMS(created), nullMS(lastUsed)
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetToken returns one token as stored, revoked or not; an unknown id is
// store.ErrNotFound.
func (s *Service) GetToken(ctx context.Context, id event.ID) (Token, error) {
	return getTokenIn(ctx, s.db, id)
}

// InviteDevice mints a holaclaw invite onto the caller's own identity, for a
// signed-in client to pair another device (`POST /v1/me/devices`). An empty
// scope is client; a scope above the caller's own is ErrScopeAboveCaller. The
// invite has the default TTL and is refused as CreateInvite refuses one.
func (s *Service) InviteDevice(ctx context.Context, by Principal, scope Scope) (string, Invite, error) {
	if scope == "" {
		scope = ScopeClient
	}
	if !scope.Valid() {
		return "", Invite{}, fmt.Errorf("%w: %q", ErrInvalidScope, scope)
	}
	if !by.Scope.Allows(scope) {
		return "", Invite{}, ErrScopeAboveCaller
	}
	return s.CreateInvite(ctx, InviteSpec{
		Channel: event.ChannelHolaclaw, UserID: by.UserID, Scope: scope, CreatedBy: by.UserID, CreatorRole: by.Role,
	})
}

// UnbindDevice is Unbind on a holaclaw binding of the caller's own identity
// (`DELETE /v1/me/devices/{binding}`). Any other identity's binding is
// store.ErrNotFound, like one that does not exist; the caller may remove the
// binding it signs in from.
func (s *Service) UnbindDevice(ctx context.Context, by Principal, alias string) (Unbound, error) {
	if by.UserID == "" {
		return Unbound{}, store.ErrNotFound
	}
	return s.unbind(ctx, event.ChannelHolaclaw, alias, by, by.UserID)
}
