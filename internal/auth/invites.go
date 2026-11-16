package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

const (
	// DefaultInviteTTL is the lifetime of an invite when the spec leaves TTL
	// unset, and MaxInviteTTL caps it: a code is a one-shot credential.
	// MaxFirstInviteTTL caps an invite onto an identity with no bindings yet,
	// whose person may not be at hand when it is created (docs/identity.md §2).
	DefaultInviteTTL  = 15 * time.Minute
	MaxInviteTTL      = time.Hour
	MaxFirstInviteTTL = 24 * time.Hour

	// inviteCodeLen is the length of a generated code, in Crockford base32
	// characters (~50 bits).
	inviteCodeLen = 10

	// redeemFailures failed attempts within redeemWindow, per (channel,
	// alias), turn further attempts into ErrRateLimited.
	redeemFailures = 5
	redeemWindow   = 10 * time.Minute
)

// crockford is Crockford's base32 alphabet: no I, L, O or U, so a code read
// aloud or typed from a screen survives.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// InviteKind says what redeeming an invite does. The kinds never redeem for
// each other (docs/identity.md).
type InviteKind string

const (
	// InviteDM binds the redeemer to an existing identity. It is the default.
	InviteDM InviteKind = "dm"
	// InviteRoom approves the room it is redeemed in. It grants its redeemer
	// nothing: approving a room is not binding a person.
	InviteRoom InviteKind = "room"
)

// InviteSpec describes an invite to create.
type InviteSpec struct {
	// Kind defaults to InviteDM.
	Kind InviteKind
	// Channel is the one channel instance the code redeems on. Required.
	Channel string
	// UserID is the confirmed user identity a DM invite binds to; the binding
	// gets its role. Required for a DM invite, empty for a room invite.
	UserID event.ID
	// Scope is the token scope a holaclaw invite pairs a device with: client,
	// the default, or admin, which the identity's role must allow. Refused on
	// any other channel.
	Scope Scope
	// TTL defaults to DefaultInviteTTL. Above MaxInviteTTL it is refused,
	// except up to MaxFirstInviteTTL onto an identity with no bindings.
	TTL time.Duration
	// CreatedBy is the identity who issued the invite.
	CreatedBy event.ID
	// CreatorRole is the issuer's role: only an owner may mint an invite for
	// the owner (ErrOwnerOnly).
	CreatorRole store.Role
}

// Invite is a one-shot code as stored; the code itself is not part of it.
type Invite struct {
	ID        event.ID
	Kind      InviteKind
	Channel   string
	UserID    event.ID // empty on a room invite
	Scope     Scope    // set on a holaclaw invite only
	CreatedBy event.ID
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
	RevokedAt *time.Time
}

// Used reports whether the invite has been redeemed.
func (i Invite) Used() bool { return i.UsedAt != nil }

// CreateInvite mints a one-shot code. The code is returned once and stored
// only as a hash. The identity is checked and the invite stored in one
// transaction.
func (s *Service) CreateInvite(ctx context.Context, spec InviteSpec) (string, Invite, error) {
	kind := spec.Kind
	if kind == "" {
		kind = InviteDM
	}
	// Bound to one channel instance: an unbound code, leaked, would bind or
	// approve on any of them.
	if spec.Channel == "" {
		return "", Invite{}, &InviteSpecError{Reason: "an invite must name a channel"}
	}
	if spec.TTL < 0 {
		return "", Invite{}, &InviteSpecError{Reason: "ttl must be positive"}
	}
	ttl := spec.TTL
	if ttl == 0 {
		ttl = DefaultInviteTTL
	}
	pairing := kind == InviteDM && spec.Channel == event.ChannelHolaclaw
	if spec.Scope != "" && !pairing {
		return "", Invite{}, &InviteSpecError{Reason: "a scope applies only to holaclaw invites"}
	}
	switch kind {
	case InviteDM:
		if spec.UserID == "" {
			return "", Invite{}, &InviteSpecError{Reason: "an invite must name an identity"}
		}
		if !spec.UserID.Valid() {
			return "", Invite{}, &InviteSpecError{Reason: fmt.Sprintf("%q is not an identity id", spec.UserID)}
		}
	case InviteRoom:
		if spec.UserID != "" {
			return "", Invite{}, &InviteSpecError{Reason: "room invites name no identity"}
		}
		if ttl > MaxInviteTTL {
			return "", Invite{}, ttlError(MaxInviteTTL, "")
		}
	default:
		return "", Invite{}, &InviteSpecError{Reason: fmt.Sprintf("kind %q", kind)}
	}
	var scope Scope
	if pairing {
		scope = spec.Scope
		if scope == "" {
			scope = ScopeClient
		}
		if scope != ScopeClient && scope != ScopeAdmin {
			return "", Invite{}, fmt.Errorf("%w: %q", ErrInvalidScope, scope)
		}
	}
	code, err := newInviteCode()
	if err != nil {
		return "", Invite{}, fmt.Errorf("auth: create invite: %w", err)
	}
	now := s.now()
	inv := Invite{
		ID:        event.NewID(),
		Kind:      kind,
		Channel:   spec.Channel,
		UserID:    spec.UserID,
		Scope:     scope,
		CreatedBy: spec.CreatedBy,
		CreatedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if kind == InviteDM {
			if err := inviteTargetIn(ctx, tx, spec, scope, ttl); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO invites (id, hash, kind, channel, user_id, scope, created_by, created_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(inv.ID), hashSecret(code), string(inv.Kind), inv.Channel, idArg(inv.UserID), scopeArg(inv.Scope),
			string(inv.CreatedBy), ms(inv.CreatedAt), ms(inv.ExpiresAt))
		return err
	})
	if err != nil {
		var bad *InviteSpecError
		if errors.As(err, &bad) || errors.Is(err, store.ErrNotFound) || errors.Is(err, ErrRoomIdentity) ||
			errors.Is(err, ErrPending) || errors.Is(err, ErrOwnerOnly) || errors.Is(err, ErrInvalidScope) {
			return "", Invite{}, err
		}
		return "", Invite{}, fmt.Errorf("auth: create invite: %w", err)
	}
	return code, inv, nil
}

// inviteTargetIn checks the identity a DM invite binds to, inside tx: a
// confirmed user identity, which only an owner invites when it is the owner,
// whose role allows the scope, and whose bindings set the TTL cap.
func inviteTargetIn(ctx context.Context, tx *sql.Tx, spec InviteSpec, scope Scope, ttl time.Duration) error {
	i, err := store.IdentityIn(ctx, tx, spec.UserID)
	if err != nil {
		return err
	}
	switch {
	case i.Kind == store.KindRoom:
		return ErrRoomIdentity
	case i.Pending():
		return fmt.Errorf("%w: %q; confirm it or assign it instead", ErrPending, spec.UserID)
	case i.Disabled():
		return &InviteSpecError{Reason: fmt.Sprintf("%q is disabled", spec.UserID)}
	case i.Role == store.RoleOwner && spec.CreatorRole != store.RoleOwner:
		return ErrOwnerOnly
	case scope == ScopeAdmin && i.Role == store.RoleUser:
		return fmt.Errorf("%w: admin needs the role owner or admin; %q has user", ErrInvalidScope, spec.UserID)
	}
	if ttl <= MaxInviteTTL {
		return nil
	}
	var bound bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM bindings WHERE identity_id = ?)`, string(spec.UserID)).Scan(&bound); err != nil {
		return err
	}
	switch {
	case bound:
		return ttlError(MaxInviteTTL, fmt.Sprintf(" while %q has bindings", spec.UserID))
	case ttl > MaxFirstInviteTTL:
		return ttlError(MaxFirstInviteTTL, "")
	}
	return nil
}

// ttlError refuses a TTL above max; why says what set the cap, if anything.
func ttlError(max time.Duration, why string) error {
	d := strings.TrimSuffix(strings.TrimSuffix(max.String(), "0s"), "0m")
	return &InviteSpecError{Reason: "ttl must be at most " + d + why}
}

// scopeArg stores an empty scope as NULL.
func scopeArg(s Scope) any {
	if s == "" {
		return nil
	}
	return string(s)
}

// newInviteCode returns inviteCodeLen uniform Crockford base32 characters.
func newInviteCode() (string, error) {
	b, err := randomBytes(inviteCodeLen)
	if err != nil {
		return "", err
	}
	out := make([]byte, inviteCodeLen)
	for i, v := range b {
		out[i] = crockford[v&31] // 256 is a multiple of 32: no modulo bias
	}
	return string(out), nil
}

// normaliseCode accepts what a human retypes: lower case, separators, and the
// letters Crockford maps onto digits.
func normaliseCode(code string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch r {
		case '-', ' ', '\t':
		case 'I', 'L':
			sb.WriteRune('1')
		case 'O':
			sb.WriteRune('0')
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// RedeemInvite consumes a code for the aliases on channel: it marks the invite
// used and binds the aliases to the invited identity, in one transaction, so
// two concurrent redeems of one code leave exactly one winner and a failed
// bind consumes nothing. A holaclaw invite never redeems here: it pairs a
// device through the API, never through a message.
func (s *Service) RedeemInvite(ctx context.Context, code, channel string, aliases []string, display string) (store.Identity, error) {
	if channel == "" || len(aliases) == 0 {
		return store.Identity{}, errors.New("auth: redeem invite: channel and aliases are required")
	}
	if s.rateLimited(channel, aliases) {
		return store.Identity{}, ErrRateLimited
	}
	i, err := s.redeem(ctx, code, channel, aliases, display)
	if err != nil {
		if errors.Is(err, ErrInviteInvalid) || errors.Is(err, ErrInviteChannel) {
			s.recordFailure(channel, aliases)
		}
		return store.Identity{}, err
	}
	return i, nil
}

// consumed is the row of an invite consume just marked used.
type consumed struct {
	id        string
	userID    sql.NullString
	scope     sql.NullString // a holaclaw invite's
	createdBy string
}

// consume marks the invite behind code used, inside tx. An invite of another
// kind, or of a disabled creator, answers like an unknown code and stays
// unused: the redeemer learns nothing, and the invite still works where it
// belongs.
func consume(ctx context.Context, tx *sql.Tx, code string, kind InviteKind, channel string, now time.Time) (consumed, error) {
	var (
		c            consumed
		invKind      string
		invChannel   string
		expires      int64
		used, revoke sql.NullInt64
	)
	err := tx.QueryRowContext(ctx,
		`SELECT id, kind, channel, user_id, scope, created_by, expires_at, used_at, revoked_at
		 FROM invites WHERE hash = ?`, hashSecret(normaliseCode(code))).
		Scan(&c.id, &invKind, &invChannel, &c.userID, &c.scope, &c.createdBy, &expires, &used, &revoke)
	if errors.Is(err, sql.ErrNoRows) {
		return consumed{}, ErrInviteInvalid
	}
	if err != nil {
		return consumed{}, err
	}
	if InviteKind(invKind) != kind || used.Valid || revoke.Valid || !now.Before(fromMS(expires)) {
		return consumed{}, ErrInviteInvalid
	}
	if invChannel != channel {
		return consumed{}, ErrInviteChannel
	}
	// A disabled admin's invites are on hold with it: they answer like an
	// unknown code and stay unused, to work again once it is enabled.
	var off bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM identities WHERE id = ? AND status = 'disabled')`, c.createdBy).Scan(&off); err != nil {
		return consumed{}, err
	}
	if off {
		return consumed{}, ErrInviteInvalid
	}

	// The guard is the WHERE clause, not the SELECT above: a concurrent
	// redeem that committed first leaves no row to update.
	res, err := tx.ExecContext(ctx,
		`UPDATE invites SET used_at = ? WHERE id = ? AND used_at IS NULL AND revoked_at IS NULL`,
		ms(now), c.id)
	if err != nil {
		return consumed{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return consumed{}, err
	} else if n == 0 {
		return consumed{}, ErrInviteInvalid
	}
	return c, nil
}

// redeem binds the aliases to the invited identity, with its role. Aliases
// already held by a pending identity are moved with it, as Assign would
// (docs/identity.md §2): the code proves the guest is that person. Aliases a
// confirmed identity holds, the invited one included, are refused with
// store.ErrAliasLinked. An invite for an identity disabled since it was minted
// is ErrInviteInvalid.
func (s *Service) redeem(ctx context.Context, code, channel string, aliases []string, display string) (store.Identity, error) {
	if channel == event.ChannelHolaclaw {
		return store.Identity{}, ErrInviteChannel
	}
	now := s.now()
	var (
		i        store.Identity
		inviteID string
		moved    *Assigned
		added    []string
	)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		c, err := consume(ctx, tx, code, InviteDM, channel, now)
		if err != nil {
			return err
		}
		inviteID = c.id
		guest, err := guestIn(ctx, tx, channel, aliases, s.namespaces(channel))
		if err != nil {
			return err
		}
		i, err = store.IdentityIn(ctx, tx, event.ID(c.userID.String))
		if errors.Is(err, store.ErrNotFound) {
			return ErrInviteInvalid // deleting the identity deletes its invites, too
		}
		if err != nil {
			return err
		}
		if i.Disabled() {
			return ErrInviteInvalid
		}
		if guest != "" {
			a, err := assignIn(ctx, tx, guest, i.ID, now)
			if err != nil {
				return err
			}
			moved = &a
		}
		added, err = linkIn(ctx, tx, channel, aliases, i.ID, display, now)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrInviteInvalid) || errors.Is(err, ErrInviteChannel) || errors.Is(err, store.ErrAliasLinked) {
			return store.Identity{}, err
		}
		return store.Identity{}, fmt.Errorf("auth: redeem invite: %w", err)
	}
	if moved != nil {
		s.emitAssigned(ctx, *moved, "", event.ID(inviteID))
	}
	for _, alias := range added {
		s.emit(ctx, event.IdentityBound, map[string]any{
			"identity": i.ID, "binding": bindingRef(channel, alias), "invite": inviteID,
		})
	}
	return i, nil
}

// RedeemRoomInvite consumes a room code sent in a room on channel. approve
// approves the room inside the same transaction, so a failed approve consumes
// nothing; it is handed the invite's creator, who stands as the approver. The
// sender is not linked: its aliases, like the conversation, only key the rate
// limit, so neither one account nor one room full of them can guess for long.
// It returns the id of the invite it spent.
func (s *Service) RedeemRoomInvite(ctx context.Context, code, channel, conversation string, aliases []string,
	approve func(tx *sql.Tx, by event.ID) error) (event.ID, error) {
	if channel == "" || conversation == "" || approve == nil {
		return "", errors.New("auth: redeem room invite: channel, conversation and approve are required")
	}
	keys := append([]string{"room:" + conversation}, aliases...)
	if s.rateLimited(channel, keys) {
		return "", ErrRateLimited
	}
	now := s.now()
	var id string
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		c, err := consume(ctx, tx, code, InviteRoom, channel, now)
		if err != nil {
			return err
		}
		id = c.id
		return approve(tx, event.ID(c.createdBy))
	})
	switch {
	case err == nil:
		return event.ID(id), nil
	case errors.Is(err, ErrInviteInvalid), errors.Is(err, ErrInviteChannel):
		s.recordFailure(channel, keys)
		return "", err
	default:
		return "", fmt.Errorf("auth: redeem room invite: %w", err)
	}
}

// ListInvites returns every invite, oldest first. Hashes stay in the database.
func (s *Service) ListInvites(ctx context.Context) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, channel, user_id, scope, created_by, created_at, expires_at, used_at, revoked_at
		 FROM invites ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("auth: list invites: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Invite
	for rows.Next() {
		var (
			i                Invite
			id, kind         string
			userID, scope    sql.NullString
			created, expires int64
			used, revoked    sql.NullInt64
		)
		if err := rows.Scan(&id, &kind, &i.Channel, &userID, &scope, &i.CreatedBy, &created, &expires, &used, &revoked); err != nil {
			return nil, fmt.Errorf("auth: list invites: %w", err)
		}
		i.ID, i.Kind, i.UserID, i.Scope = event.ID(id), InviteKind(kind), nullID(userID), Scope(scope.String)
		i.CreatedAt, i.ExpiresAt = fromMS(created), fromMS(expires)
		i.UsedAt, i.RevokedAt = nullMS(used), nullMS(revoked)
		out = append(out, i)
	}
	return out, rows.Err()
}

// RevokeInvite makes an unused invite unredeemable.
func (s *Service) RevokeInvite(ctx context.Context, id event.ID) error {
	var n int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE invites SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, ms(s.now()), string(id))
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	if err != nil {
		return fmt.Errorf("auth: revoke invite: %w", err)
	}
	if n == 0 {
		var exists int
		err := s.db.QueryRowContext(ctx, `SELECT 1 FROM invites WHERE id = ?`, string(id)).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("auth: revoke invite: %w", err)
		}
	}
	return nil
}

// rateLimited reports whether any of the aliases has burnt its attempts on
// this channel. The window is in memory: a restart forgives.
func (s *Service) rateLimited(channel string, aliases []string) bool {
	now := s.now()
	s.rlMu.Lock()
	defer s.rlMu.Unlock()
	for _, alias := range aliases {
		if len(s.pruneLocked(channel+"\x00"+alias, now)) >= redeemFailures {
			return true
		}
	}
	return false
}

func (s *Service) recordFailure(channel string, aliases []string) {
	now := s.now()
	s.rlMu.Lock()
	defer s.rlMu.Unlock()
	for _, alias := range aliases {
		key := channel + "\x00" + alias
		s.rl[key] = append(s.pruneLocked(key, now), now)
	}
	s.sweepLocked(now)
}

// reserveAttempt counts an attempt for key before it is made, and refuses it
// once key has burnt its attempts; the check and the count are one step, so a
// burst of concurrent attempts cannot all pass the check before any of them
// counts. The returned release takes the attempt back, for one that did not
// fail.
func (s *Service) reserveAttempt(key string) (release func(), ok bool) {
	now := s.now()
	s.rlMu.Lock()
	defer s.rlMu.Unlock()
	if len(s.pruneLocked(key, now)) >= redeemFailures {
		return nil, false
	}
	s.rl[key] = append(s.rl[key], now)
	s.sweepLocked(now)
	return func() {
		s.rlMu.Lock()
		defer s.rlMu.Unlock()
		kept := s.rl[key]
		for i, t := range kept {
			if t.Equal(now) {
				s.rl[key] = append(kept[:i], kept[i+1:]...)
				break
			}
		}
		if len(s.rl[key]) == 0 {
			delete(s.rl, key)
		}
	}, true
}

// rlSweepAt is the number of keys above which a new attempt sweeps the whole
// map: a key is otherwise pruned only when it is touched again, and an
// unauthenticated caller chooses its key (an IPv6 /64 out of a whole range).
const rlSweepAt = 1024

// sweepLocked drops every key whose attempts are all outside the window once
// the map has grown past rlSweepAt, so it holds at most the keys of one
// window; rlMu must be held.
func (s *Service) sweepLocked(now time.Time) {
	if len(s.rl) <= rlSweepAt {
		return
	}
	for key := range s.rl {
		s.pruneLocked(key, now)
	}
}

// pruneLocked drops attempts older than the window; rlMu must be held.
func (s *Service) pruneLocked(key string, now time.Time) []time.Time {
	kept := s.rl[key][:0]
	for _, t := range s.rl[key] {
		if now.Sub(t) < redeemWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(s.rl, key)
		return nil
	}
	s.rl[key] = kept
	return kept
}
