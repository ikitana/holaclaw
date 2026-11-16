package auth

import (
	"errors"

	"holaclaw/internal/event"
)

var (
	// ErrTokenInvalid covers every rejected token: malformed, unknown or
	// revoked. The caller cannot tell them apart on purpose.
	ErrTokenInvalid = errors.New("auth: invalid token")
	// ErrInviteInvalid covers unknown, used, revoked and expired invite
	// codes — indistinguishable to the redeemer.
	ErrInviteInvalid = errors.New("auth: invalid invite")
	// ErrInviteChannel is returned when a channel-restricted invite is
	// redeemed on another channel instance.
	ErrInviteChannel = errors.New("auth: invite is for another channel")
	// ErrInvalidInvite reports an invite spec that does not fit its kind.
	// CreateInvite returns it as an *InviteSpecError, which says why.
	ErrInvalidInvite = errors.New("auth: invalid invite spec")
	// ErrRateLimited is returned after too many failed redeem attempts from
	// the same (channel, alias).
	ErrRateLimited = errors.New("auth: too many attempts")
	// ErrOwner refuses to delete an owner: the owner is never removed.
	ErrOwner = errors.New("auth: the owner cannot be deleted")
	// ErrInvalidScope reports a scope outside admin | client.
	ErrInvalidScope = errors.New("auth: invalid scope")
	// ErrInvalidRole reports a role the operation does not allow.
	ErrInvalidRole = errors.New("auth: invalid role")
	// ErrInitialised refuses a second bootstrap: an owner exists.
	ErrInitialised = errors.New("auth: already initialised")
	// ErrNotPending is returned by Confirm and Assign when the identity is not
	// pending: there is nothing pending to confirm or move.
	ErrNotPending = errors.New("auth: identity is not pending")
	// ErrPending refuses a role change or a token on a pending identity, or one
	// as the target of Assign: confirm it first, which sets the role in the same
	// step.
	ErrPending = errors.New("auth: identity is pending confirmation")
	// ErrOwnerRole refuses to change the owner's role.
	ErrOwnerRole = errors.New("auth: the owner's role cannot be changed")
	// ErrOwnerOnly refuses to act on the owner on behalf of anyone but an owner:
	// a linked account would act with the owner's role and memory.
	ErrOwnerOnly = errors.New("auth: only the owner can do this to the owner")
	// ErrOwnerProtected refuses to disable or enable the owner: the owner is
	// never disabled.
	ErrOwnerProtected = errors.New("auth: the owner cannot be disabled")
	// ErrDisabled refuses a disabled identity as the target of Assign, where
	// bindings move only onto a confirmed one, or of a new token.
	ErrDisabled = errors.New("auth: identity is disabled")
	// ErrIdentityDisabled refuses a valid token whose identity is disabled. The
	// token is not revoked: enabling the identity makes it good again.
	ErrIdentityDisabled = errors.New("auth: the token's identity is disabled")
	// ErrAssignSelf refuses to assign an identity onto itself.
	ErrAssignSelf = errors.New("auth: cannot assign an identity to itself")
	// ErrRoomIdentity refuses what only a user identity can do: a room is never
	// assigned, never holds a token or an invite, and keeps its one binding.
	ErrRoomIdentity = errors.New("auth: not allowed on a room identity")
	// ErrInvalidIdentity reports an empty name or an update with nothing to
	// change, or a token for no identity.
	ErrInvalidIdentity = errors.New("auth: invalid identity")
	// ErrSocketBinding refuses to unbind holaclaw:cli, the binding the unix
	// socket arrives as: without it the host could not reach the daemon.
	ErrSocketBinding = errors.New("auth: holaclaw:cli is the unix socket and stays")
	// ErrScopeAboveRole refuses a token scope above what the identity's role allows:
	// admin needs the role owner or admin.
	ErrScopeAboveRole = errors.New("auth: scope above the identity's role")
	// ErrRevoked refuses to change or rotate a revoked token: revoking is final.
	ErrRevoked = errors.New("auth: token is revoked")
	// ErrNoToken refuses a token operation from a caller with no token: the unix
	// socket arrives by file permission.
	ErrNoToken = errors.New("auth: the caller has no token")
)

// InviteSpecError is an ErrInvalidInvite whose Reason a caller can show as is.
type InviteSpecError struct{ Reason string }

func (e *InviteSpecError) Error() string { return ErrInvalidInvite.Error() + ": " + e.Reason }

// Is makes an *InviteSpecError match ErrInvalidInvite.
func (e *InviteSpecError) Is(target error) bool { return target == ErrInvalidInvite }

// RotatedError refuses to revoke a token a rotation already replaced: it is
// revoked, and Into, the active token its rotations led to, is the one to
// revoke.
type RotatedError struct{ Into event.ID }

func (e *RotatedError) Error() string { return "auth: token was rotated into " + string(e.Into) }

// Is makes a *RotatedError match ErrRevoked.
func (e *RotatedError) Is(target error) bool { return target == ErrRevoked }
