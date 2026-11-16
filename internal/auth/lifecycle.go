package auth

import (
	"context"
	"database/sql"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// removedPending is what deletePendingIn removed.
type removedPending struct {
	bindings []string
	tokens   []Token
	memory   int
}

// deletePendingIn deletes a pending user identity with its memory, inside the
// caller's transaction (the last unbind). A room is ErrRoomIdentity: only an
// admin's delete removes one, with its room partition. Memory goes as
// ForgetPartition has it: everything in its DM partition, in every agent, except
// shared notes. The FTS index follows by trigger and the vectors by cascade, and
// memory's own writers wait on tx like any other writer; a queued embedding of a
// deleted row is the same no-op it is after ForgetPartition.
//
// Its tasks are cancelled and their grants revoked here too, in SQL: the
// BeforeDeleteUser hook runs outside any transaction, and a task must not outlive
// its creator. Its tokens are revoked, its bindings and invites go by cascade,
// and sessions stay with no identity.
func deletePendingIn(ctx context.Context, tx *sql.Tx, id event.ID, now time.Time, by event.ID) (removedPending, error) {
	i, err := store.IdentityIn(ctx, tx, id)
	if err != nil {
		return removedPending{}, err
	}
	if i.Kind == store.KindRoom {
		return removedPending{}, ErrRoomIdentity
	}
	if !i.Pending() {
		return removedPending{}, ErrNotPending
	}
	var r removedPending
	held, err := identitiesIn(ctx, tx, id)
	if err != nil {
		return removedPending{}, err
	}
	r.bindings = bindingRefs(held)
	if r.tokens, err = revokeIdentityTokensIn(ctx, tx, id, now, by); err != nil {
		return removedPending{}, err
	}
	if r.memory, err = exec(ctx, tx,
		`DELETE FROM memory_items WHERE "partition" = ? AND NOT (layer = 'note' AND visibility = 'shared')`,
		"dm/"+string(id)); err != nil {
		return removedPending{}, err
	}
	if err := cancelTasksIn(ctx, tx, id); err != nil {
		return removedPending{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM identities WHERE id = ?`, string(id)); err != nil {
		return removedPending{}, err
	}
	return r, nil
}

// cancelTasksIn cancels the active tasks an identity created and revokes their
// grants, for an identity about to be deleted. They are never re-owned: a task
// a guest created must not gain another user's role. The scheduler isn't woken:
// it finds the task cancelled at its next look. A run already in flight isn't
// aborted either; with the task's grants gone, its tool calls fail policy.
func cancelTasksIn(ctx context.Context, tx *sql.Tx, id event.ID) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM grants WHERE scope = 'task' AND task_id IN
		 (SELECT id FROM tasks WHERE created_by = ? AND status = 'active')`, string(id)); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE tasks SET status = 'cancelled', next_run = NULL WHERE created_by = ? AND status = 'active'`, string(id))
	return err
}

// emitPendingDeleted reports what deletePendingIn removed; how carries who did it
// or why.
func (s *Service) emitPendingDeleted(ctx context.Context, id event.ID, r removedPending, how map[string]any) {
	attrs := map[string]any{
		"identity": id, "role": store.RoleGuest, "bindings": r.bindings, "tokens_revoked": len(r.tokens),
		"memory_forgotten": r.memory, "memory_kept": false,
	}
	for k, v := range how {
		attrs[k] = v
	}
	s.emit(ctx, event.IdentityDeleted, attrs)
}

// Actor names a caller for an event: its identity, which the socket and every
// admin or client token have through their binding, else its token
// (`token:<id>`), else the owner.
func (s *Service) Actor(ctx context.Context, p Principal) event.ID {
	return actorIn(ctx, s.db, p)
}

// actorIn is Actor as q sees it.
func actorIn(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, p Principal) event.ID {
	switch {
	case p.UserID != "":
		return p.UserID
	case p.TokenID != "":
		return "token:" + p.TokenID
	}
	if owner, err := ownerIn(ctx, q); err == nil {
		return owner.ID
	}
	return ""
}
