package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// Assigned is what moving a pending identity onto a user changed.
type Assigned struct {
	From, To event.ID
	// IDs is the number of bindings moved.
	IDs int
	// Memory is the number of memory rows re-owned; Dropped the recall rows
	// left behind because the target already held the same summary.
	Memory, Dropped int
	// Sessions is the number of the pending identity's DM sessions ended.
	Sessions int
}

// Assign moves a pending identity's bindings onto an existing user: the case
// of a known person writing from a new account (docs/identity.md §2). In one
// transaction the bindings move, the memory in the guest's DM partition is
// re-owned by the target, the guest's DM sessions are ended (never rekeyed)
// and the guest row is deleted. by is the caller; only an owner may assign
// onto the owner (ErrOwnerOnly). A confirmed source is ErrNotPending, a pending
// target ErrPending, a disabled target ErrDisabled. A room on either side is
// ErrRoomIdentity: rooms are never assigned.
func (s *Service) Assign(ctx context.Context, from, to event.ID, by Principal) (Assigned, error) {
	if from == to {
		return Assigned{}, ErrAssignSelf
	}
	var a Assigned
	actor := s.Actor(ctx, by)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		a, err = assignAs(ctx, tx, from, to, by, s.now())
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, ErrNotPending) || errors.Is(err, ErrPending) ||
			errors.Is(err, ErrDisabled) || errors.Is(err, ErrAssignSelf) || errors.Is(err, ErrOwnerOnly) ||
			errors.Is(err, ErrRoomIdentity) {
			return Assigned{}, err
		}
		return Assigned{}, fmt.Errorf("auth: assign: %w", err)
	}
	s.emitAssigned(ctx, a, actor, "")
	return a, nil
}

// assignAs is assignIn on behalf of a caller. A room on either side is refused
// first. Only an owner links an account to the owner. A redeem needs no such
// check: CreateInvite applied it when the invite was minted, and it never names a
// room. The source is checked first, so a refusal names the real problem.
func assignAs(ctx context.Context, tx *sql.Tx, from, to event.ID, by Principal, now time.Time) (Assigned, error) {
	for _, id := range []event.ID{from, to} {
		if i, err := store.IdentityIn(ctx, tx, id); err == nil && i.Kind == store.KindRoom {
			return Assigned{}, ErrRoomIdentity
		}
	}
	_, fromStatus, fromErr := roleIn(ctx, tx, from)
	toRole, _, toErr := roleIn(ctx, tx, to)
	if fromErr == nil && fromStatus == store.StatusPending && toErr == nil && toRole == store.RoleOwner &&
		by.Role != store.RoleOwner {
		return Assigned{}, ErrOwnerOnly
	}
	return assignIn(ctx, tx, from, to, now)
}

// assignIn is Assign inside a transaction, shared with redeem: a failure
// anywhere rolls back every move, and the caller's own writes with it.
func assignIn(ctx context.Context, tx *sql.Tx, from, to event.ID, now time.Time) (Assigned, error) {
	if from == to {
		return Assigned{}, ErrAssignSelf
	}
	_, fromStatus, err := roleIn(ctx, tx, from)
	if err != nil {
		return Assigned{}, err
	}
	if fromStatus != store.StatusPending {
		return Assigned{}, ErrNotPending
	}
	_, toStatus, err := roleIn(ctx, tx, to)
	if err != nil {
		return Assigned{}, fmt.Errorf("%w: target %s", err, to)
	}
	switch toStatus {
	case store.StatusPending:
		return Assigned{}, ErrPending
	case store.StatusDisabled:
		return Assigned{}, ErrDisabled
	}

	a := Assigned{From: from, To: to}
	if a.IDs, err = exec(ctx, tx,
		`UPDATE bindings SET identity_id = ? WHERE identity_id = ?`, string(to), string(from)); err != nil {
		return Assigned{}, err
	}

	// A recall summary the target already holds would break the dedupe index
	// on the move; the guest's copy is the one dropped.
	src, dst := "dm/"+string(from), "dm/"+string(to)
	if a.Dropped, err = exec(ctx, tx,
		`DELETE FROM memory_items AS g WHERE g."partition" = ? AND g.layer = 'recall' AND g.source_hash <> ''
		 AND EXISTS (SELECT 1 FROM memory_items t WHERE t.agent = g.agent AND t."partition" = ?
		             AND t.layer = 'recall' AND t.source_hash = g.source_hash)`, src, dst); err != nil {
		return Assigned{}, err
	}
	// Only the guest's own rows change author: a row someone else wrote into
	// the partition keeps its provenance.
	if a.Memory, err = exec(ctx, tx,
		`UPDATE memory_items SET "partition" = ?,
		 user_id = CASE WHEN user_id = ? THEN ? ELSE user_id END WHERE "partition" = ?`,
		dst, string(from), string(to), src); err != nil {
		return Assigned{}, err
	}

	// DM session keys carry the user id, so the target never lands in these
	// again. A user owns no room session: those belong to the room identity.
	if a.Sessions, err = exec(ctx, tx,
		`UPDATE sessions SET ended_at = ? WHERE user_id = ? AND ended_at IS NULL`,
		ms(now), string(from)); err != nil {
		return Assigned{}, err
	}
	// The guest's tasks end with it: re-owning them would hand the target's
	// role to something a guest scheduled.
	if err := cancelTasksIn(ctx, tx, from); err != nil {
		return Assigned{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM identities WHERE id = ?`, string(from)); err != nil {
		return Assigned{}, err
	}
	return a, nil
}

// guestIn returns the pending identity owning any of the aliases on channel,
// or "" when none of them is linked. An alias linked to a confirmed user, or
// aliases split between two users, fail with store.ErrAliasLinked, and so does
// a pending identity the recycled-id guard does not trust (namespaces, see
// store.ResolveAliases): its ids are not the sender's to take along. An alias a
// room holds, even a pending one, is never a guest's.
func guestIn(ctx context.Context, tx *sql.Tx, channel string, aliases, namespaces []string) (event.ID, error) {
	var guest event.ID
	for _, alias := range aliases {
		var owner, kind, status string
		err := tx.QueryRowContext(ctx,
			`SELECT n.id, n.kind, n.status FROM bindings b JOIN identities n ON n.id = b.identity_id
			 WHERE b.channel = ? AND b.alias = ?`, channel, alias).Scan(&owner, &kind, &status)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", err
		}
		if store.IdentityKind(kind) == store.KindRoom || store.IdentityStatus(status) != store.StatusPending ||
			(guest != "" && guest != event.ID(owner)) {
			return "", fmt.Errorf("%w: %s/%s", store.ErrAliasLinked, channel, alias)
		}
		guest = event.ID(owner)
	}
	if guest != "" {
		// Every linked alias is the guest's, so the guard either trusts it
		// or finds nobody.
		if _, err := store.ResolveAliases(ctx, tx, channel, aliases, namespaces); errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("%w: %s: pending identity not trusted", store.ErrAliasLinked, channel)
		} else if err != nil {
			return "", err
		}
	}
	return guest, nil
}

// roleIn returns the role of an identity as tx sees it (empty while pending)
// and its status, or store.ErrNotFound.
func roleIn(ctx context.Context, tx *sql.Tx, id event.ID) (store.Role, store.IdentityStatus, error) {
	var role sql.NullString
	var status string
	err := tx.QueryRowContext(ctx, `SELECT role, status FROM identities WHERE id = ?`, string(id)).Scan(&role, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", store.ErrNotFound
	}
	return store.Role(role.String), store.IdentityStatus(status), err
}

// exec runs a write and returns the rows it touched.
func exec(ctx context.Context, tx *sql.Tx, query string, args ...any) (int, error) {
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// emitAssigned reports a completed move; invite is set when a redeem did it.
func (s *Service) emitAssigned(ctx context.Context, a Assigned, by, invite event.ID) {
	attrs := map[string]any{
		"source": a.From, "target": a.To, "bindings": a.IDs,
		"memory": a.Memory, "dropped": a.Dropped, "sessions": a.Sessions,
	}
	if by != "" {
		attrs["by"] = by
	}
	if invite != "" {
		attrs["invite"] = invite
	}
	s.emit(ctx, event.IdentityAssigned, attrs)
}
