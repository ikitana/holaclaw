package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"holaclaw/internal/agent"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// ownerName is the name given to the user created at bootstrap.
const ownerName = "owner"

// EmptyEventID is the by of a change no identity made: its event names nobody.
const EmptyEventID event.ID = ""

// Owner returns the oldest owner, or store.ErrNotFound before `holaclaw init`
// has run. It never creates one: CreateOwner is the only way in.
func (s *Service) Owner(ctx context.Context) (store.Identity, error) {
	i, err := ownerIn(ctx, s.db)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Identity{}, fmt.Errorf("auth: owner: %w", err)
	}
	return i, err
}

// ownerIn returns the oldest owner as q sees it, or store.ErrNotFound.
func ownerIn(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (store.Identity, error) {
	var (
		i       store.Identity
		id, rl  string
		created int64
	)
	err := q.QueryRowContext(ctx,
		`SELECT id, name, role, created_at FROM identities WHERE role = 'owner' ORDER BY created_at, id LIMIT 1`).
		Scan(&id, &i.Name, &rl, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Identity{}, store.ErrNotFound
	}
	if err != nil {
		return store.Identity{}, err
	}
	i.ID, i.Kind, i.Role, i.Status, i.CreatedAt = event.ID(id), store.KindUser, store.Role(rl), store.StatusConfirmed, fromMS(created)
	return i, nil
}

// CreateIdentity creates a confirmed user identity with no bindings, role admin
// or user (the default). The owner is created once, by CreateOwner; a pending
// identity comes from an unknown sender, by EnsureGuest. by is the admin creating
// it, for the identity.created event.
func (s *Service) CreateIdentity(ctx context.Context, name string, role store.Role, by event.ID) (store.Identity, error) {
	if strings.TrimSpace(name) == "" {
		return store.Identity{}, fmt.Errorf("%w: a name is required", ErrInvalidIdentity)
	}
	if role == "" {
		role = store.RoleUser
	}
	if !grantable(role) {
		return store.Identity{}, fmt.Errorf("%w: %q", ErrInvalidRole, role)
	}
	i := store.Identity{ID: event.NewID(), Kind: store.KindUser, Name: name, Role: role, Status: store.StatusConfirmed, CreatedAt: s.now()}
	if err := s.st.CreateIdentity(ctx, i); err != nil {
		return store.Identity{}, err
	}
	s.emitCreated(ctx, i, nil, by)
	return i, nil
}

// grantable reports whether r is a role an admin-scope caller may grant:
// never owner (bootstrap only) and never guest (a pending identity has no role).
func grantable(r store.Role) bool { return r == store.RoleUser || r == store.RoleAdmin }

// ListIdentities returns every identity, or, when pending, only those waiting
// to be confirmed (docs/identity.md).
func (s *Service) ListIdentities(ctx context.Context, pending bool) ([]store.Identity, error) {
	all, err := s.st.ListIdentities(ctx)
	if err != nil {
		return nil, fmt.Errorf("auth: list users: %w", err)
	}
	if !pending {
		return all, nil
	}
	out := all[:0]
	for _, i := range all {
		if i.Pending() {
			out = append(out, i)
		}
	}
	return out, nil
}

// Confirm names a pending identity. A user also gets a role, user by default,
// turning it into a real user; a room takes none (ErrInvalidRole), and
// confirming it approves it. Its bindings keep their rows: only the identity's
// name, role, status and approved time change.
func (s *Service) Confirm(ctx context.Context, id event.ID, name string, role store.Role, by event.ID) (store.Identity, error) {
	if strings.TrimSpace(name) == "" {
		return store.Identity{}, fmt.Errorf("%w: a name is required", ErrInvalidIdentity)
	}
	if role != "" && !grantable(role) {
		return store.Identity{}, fmt.Errorf("%w: %q", ErrInvalidRole, role)
	}
	var (
		i        store.Identity
		approved []store.Binding // the room's binding, when this approved it
	)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		cur, err := store.IdentityIn(ctx, tx, id)
		if err != nil {
			return err
		}
		if !cur.Pending() {
			return ErrNotPending
		}
		i = cur
		i.Name, i.Status = name, store.StatusConfirmed
		if cur.Kind != store.KindRoom {
			if role == "" {
				role = store.RoleUser
			}
			i.Role = role
			_, err := tx.ExecContext(ctx, `UPDATE identities SET name = ?, role = ?, status = 'confirmed' WHERE id = ?`,
				name, string(role), string(id))
			return err
		}
		if role != "" {
			return fmt.Errorf("%w: a room has no role", ErrInvalidRole)
		}
		now := fromMS(ms(s.now()))
		var approver any // NULL when nobody is known, as for the socket
		if by != "" {
			approver = string(by)
		}
		if !cur.Approved() {
			i.ApprovedAt, i.ApprovedBy = &now, by
			if approved, err = identitiesIn(ctx, tx, id); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE identities SET name = ?, status = 'confirmed', approved_at = COALESCE(approved_at, ?), approved_by = CASE WHEN approved_at IS NULL THEN ? ELSE approved_by END WHERE id = ?`,
			name, ms(now), approver, string(id))
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, ErrNotPending) || errors.Is(err, ErrInvalidRole) {
			return store.Identity{}, err
		}
		return store.Identity{}, fmt.Errorf("auth: confirm: %w", err)
	}
	s.emitConfirmed(ctx, i, approved, by)
	return i, nil
}

// Disable switches an identity off (docs/identity.md §2): it keeps its bindings,
// tokens, memory and sessions, and nothing it sends is answered or accepted. A
// confirmed user or a room, pending or confirmed, can be; the owner is
// ErrOwnerProtected and a pending user ErrPending. Approving never revives a
// disabled room, and its approval stays. Disabling twice is a no-op, with no
// event. by is the admin, for the event.
func (s *Service) Disable(ctx context.Context, id, by event.ID) (store.Identity, error) {
	return s.switchStatus(ctx, id, true, event.IdentityDisabled, by)
}

// Enable switches a disabled identity back on. What came due for its tasks
// meanwhile goes through BeforeEnable first; if that fails the identity stays
// disabled, since enabled its stale tasks would fire at once. A user comes back confirmed; a room comes
// back pending, whatever it was, with its name and approval: nothing records
// the earlier status, and confirming it again is harmless. The same refusals as
// Disable apply, and enabling twice is a no-op, with no event.
func (s *Service) Enable(ctx context.Context, id, by event.ID) (store.Identity, error) {
	return s.switchStatus(ctx, id, false, event.IdentityEnabled, by)
}

// switchStatus disables or enables an identity, and emits t when that changed
// anything. The refusals and the write see one snapshot.
func (s *Service) switchStatus(ctx context.Context, id event.ID, disable bool, t event.Type, by event.ID) (store.Identity, error) {
	// Outside the transaction, which would hold the hook's writes off. The
	// tasks stay on hold until the flip, so nothing fires in between.
	if !disable && s.BeforeEnable != nil {
		if i, err := s.st.GetIdentity(ctx, id); err == nil && i.Disabled() {
			if err := s.BeforeEnable(ctx, id); err != nil {
				return store.Identity{}, fmt.Errorf("auth: enable: %w", err)
			}
		}
	}
	var (
		i       store.Identity
		changed bool
	)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		if i, err = store.IdentityIn(ctx, tx, id); err != nil {
			return err
		}
		switch {
		case i.Role == store.RoleOwner:
			return ErrOwnerProtected
		case i.Kind != store.KindRoom && i.Pending():
			return ErrPending
		case disable == i.Disabled():
			return nil
		}
		switch {
		case disable:
			i.Status = store.StatusDisabled
		case i.Kind == store.KindRoom:
			i.Status = store.StatusPending
		default:
			i.Status = store.StatusConfirmed
		}
		changed = true
		_, err = tx.ExecContext(ctx, `UPDATE identities SET status = ? WHERE id = ?`, string(i.Status), string(id))
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, ErrOwnerProtected) || errors.Is(err, ErrPending) {
			return store.Identity{}, err
		}
		return store.Identity{}, fmt.Errorf("auth: set status: %w", err)
	}
	if changed {
		s.emit(ctx, t, map[string]any{"identity": id, "by": by})
		// After the commit, so the scheduler's wake sees the creator enabled.
		if !disable && s.AfterEnable != nil {
			s.AfterEnable(ctx, id)
		}
	}
	return i, nil
}

// Update renames a user, changes their role, or both. Renaming the owner or
// a pending guest is allowed; changing a role is not: the owner's role is
// fixed, and a guest's role is set by Confirm. A room has no role to change
// (ErrInvalidRole). A role that no longer allows admin lowers the identity's
// active admin tokens to client in the same transaction, so a stored scope is
// never above the role and a later promotion raises nothing until `token set`
// (docs/identity.md §3). by is the caller, for their token.updated events.
func (s *Service) Update(ctx context.Context, id event.ID, name *string, role *store.Role, by event.ID) (store.Identity, error) {
	if name == nil && role == nil {
		return store.Identity{}, ErrInvalidIdentity
	}
	if name != nil && strings.TrimSpace(*name) == "" {
		return store.Identity{}, fmt.Errorf("%w: a name is required", ErrInvalidIdentity)
	}
	if role != nil && !grantable(*role) {
		return store.Identity{}, fmt.Errorf("%w: %q", ErrInvalidRole, *role)
	}
	var (
		i       store.Identity
		lowered []Token
	)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		if i, err = store.IdentityIn(ctx, tx, id); err != nil {
			return err
		}
		if role != nil {
			switch {
			case i.Kind == store.KindRoom:
				return fmt.Errorf("%w: a room has no role", ErrInvalidRole)
			case i.Role == store.RoleOwner:
				return ErrOwnerRole
			case i.Pending():
				return ErrPending
			}
			i.Role = *role
			if capScope(ScopeAdmin, i.Role) != ScopeAdmin {
				if lowered, err = lowerTokensIn(ctx, tx, id); err != nil {
					return err
				}
			}
		}
		if name != nil {
			i.Name = *name
		}
		roleArg := sql.NullString{String: string(i.Role), Valid: i.Role != ""}
		_, err = tx.ExecContext(ctx, `UPDATE identities SET name = ?, role = ? WHERE id = ?`,
			i.Name, roleArg, string(id))
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, ErrOwnerRole) || errors.Is(err, ErrPending) ||
			errors.Is(err, ErrInvalidRole) {
			return store.Identity{}, err
		}
		return store.Identity{}, fmt.Errorf("auth: update: %w", err)
	}
	for _, t := range lowered {
		s.emit(ctx, event.TokenUpdated, map[string]any{
			"identity": id, "binding": bindingRef(event.ChannelHolaclaw, t.Binding), "token": t.ID,
			"old_scope": ScopeAdmin, "scope": ScopeClient, "by": by, "reason": "demoted",
		})
	}
	return i, nil
}

// Deleted is what DeleteIdentity removed.
type Deleted struct {
	User    store.Identity
	Aliases []store.Binding
	// Forgotten counts the memory rows forgotten; zero when memory was kept.
	Forgotten int64
	// TokensRevoked counts the active tokens that went with it.
	TokensRevoked int
}

// DeleteIdentity removes an identity with its aliases and invites, and revokes
// its tokens; its sessions stay with no user. Unless keepMemory, its own memory is forgotten
// first (ForgetUserMemory), outside the delete transaction: a delete that then
// fails has already cancelled the tasks and forgotten the memory, and a retry
// finishes it. An owner is never deleted (docs/identity.md §1).
// by is the admin deleting it, for the identity.deleted event.
func (s *Service) DeleteIdentity(ctx context.Context, id event.ID, keepMemory bool, by event.ID) (Deleted, error) {
	ident, err := s.st.GetIdentity(ctx, id)
	if err != nil {
		return Deleted{}, err
	}
	if ident.Role == store.RoleOwner {
		return Deleted{}, ErrOwner
	}
	if s.BeforeDeleteUser != nil {
		if err := s.BeforeDeleteUser(ctx, id); err != nil {
			return Deleted{}, fmt.Errorf("auth: delete user: %w", err)
		}
	}
	var forgotten int64
	if !keepMemory && s.ForgetUserMemory != nil {
		if forgotten, err = s.ForgetUserMemory(ctx, id); err != nil {
			return Deleted{}, fmt.Errorf("auth: delete user: %w", err)
		}
	}
	var (
		aliases []store.Binding
		tokens  []Token
	)
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		role, _, err := roleIn(ctx, tx, id)
		if err != nil {
			return err
		}
		if role == store.RoleOwner {
			return ErrOwner
		}
		if aliases, err = identitiesIn(ctx, tx, id); err != nil {
			return err
		}
		if tokens, err = revokeIdentityTokensIn(ctx, tx, id, s.now(), by); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM identities WHERE id = ?`, string(id))
		return err
	})
	if err != nil {
		if errors.Is(err, ErrOwner) || errors.Is(err, store.ErrNotFound) {
			return Deleted{}, err
		}
		return Deleted{}, fmt.Errorf("auth: delete user: %w", err)
	}
	attrs := map[string]any{
		"identity": id, "bindings": bindingRefs(aliases), "tokens_revoked": len(tokens),
		"memory_forgotten": forgotten, "memory_kept": keepMemory, "by": by,
	}
	if role := deletedRole(ident); role != "" {
		attrs["role"] = role
	}
	s.emit(ctx, event.IdentityDeleted, attrs)
	s.emitTokensRevoked(ctx, tokens, by, "identity_deleted")
	return Deleted{User: ident, Aliases: aliases, Forgotten: forgotten, TokensRevoked: len(tokens)}, nil
}

// deletedRole is the role identity.deleted reports: a room has none, and a
// pending user has none either and ran as a guest.
func deletedRole(i store.Identity) store.Role {
	switch {
	case i.Kind == store.KindRoom:
		return ""
	case i.Pending():
		return store.RoleGuest
	}
	return i.Role
}

// Bindings lists the bindings of an identity, oldest first.
func (s *Service) Bindings(ctx context.Context, identityID event.ID) ([]store.Binding, error) {
	return identitiesIn(ctx, s.db, identityID)
}

// identitiesIn is Identities as q sees it.
func identitiesIn(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, identityID event.ID) ([]store.Binding, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT channel, alias, identity_id, display, platform, created_at FROM bindings
		 WHERE identity_id = ? ORDER BY created_at, channel, alias`, string(identityID))
	if err != nil {
		return nil, fmt.Errorf("auth: list identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []store.Binding
	for rows.Next() {
		var (
			i       store.Binding
			uid     string
			created int64
		)
		if err := rows.Scan(&i.Channel, &i.Alias, &uid, &i.Display, &i.Platform, &created); err != nil {
			return nil, fmt.Errorf("auth: list identities: %w", err)
		}
		i.IdentityID, i.CreatedAt = event.ID(uid), fromMS(created)
		out = append(out, i)
	}
	return out, rows.Err()
}

// Unbound is what removing a binding changed.
type Unbound struct {
	IdentityID event.ID
	// PendingDeleted reports that the holder was pending and, left with no
	// bindings, was deleted; Memory counts the rows forgotten with it.
	PendingDeleted bool
	Memory         int
	// Tokens counts the active tokens revoked: the binding's own, and those of
	// a deleted pending holder.
	Tokens int
}

// Unbind removes one binding, found by its ref alone, from the user identity
// holding it (docs/identity.md §2). An unknown ref is store.ErrNotFound; a room's
// one binding stays (ErrRoomIdentity), and so does holaclaw:cli
// (ErrSocketBinding). A holaclaw binding's tokens are revoked with it. A
// confirmed identity keeps its memory, sessions and role even with no bindings
// left; a pending one left with none is deleted with its memory, in the same
// transaction. Only an owner unbinds the owner (ErrOwnerOnly). The next message
// from that account resolves to nobody and goes through the channel's access
// knob again.
func (s *Service) Unbind(ctx context.Context, channel, alias string, by Principal) (Unbound, error) {
	return s.unbind(ctx, channel, alias, by, "")
}

// unbind is Unbind; a non-empty only is the identity the binding must belong
// to, and any other holder answers store.ErrNotFound, like an unknown ref.
func (s *Service) unbind(ctx context.Context, channel, alias string, by Principal, only event.ID) (Unbound, error) {
	if channel == "" || alias == "" {
		return Unbound{}, store.ErrNotFound
	}
	var (
		u       Unbound
		removed removedPending
		revoked []Token // the binding's own tokens
	)
	actor := s.Actor(ctx, by)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var holder string
		err := tx.QueryRowContext(ctx, `SELECT identity_id FROM bindings WHERE channel = ? AND alias = ?`, channel, alias).Scan(&holder)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && only != "" && holder != string(only)) {
			return store.ErrNotFound
		}
		if err != nil {
			return err
		}
		u.IdentityID = event.ID(holder)
		i, err := store.IdentityIn(ctx, tx, u.IdentityID)
		if err != nil {
			return err
		}
		if i.Kind == store.KindRoom {
			return ErrRoomIdentity
		}
		if i.Role == store.RoleOwner && by.Role != store.RoleOwner {
			return ErrOwnerOnly
		}
		if channel == event.ChannelHolaclaw {
			if alias == SocketAlias {
				return ErrSocketBinding
			}
			if revoked, err = revokeBindingTokensIn(ctx, tx, alias, s.now(), actor); err != nil {
				return err
			}
			u.Tokens = len(revoked)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM bindings WHERE channel = ? AND alias = ?`, channel, alias); err != nil {
			return err
		}
		if !i.Pending() {
			return nil
		}
		var left int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM bindings WHERE identity_id = ?`, holder).Scan(&left); err != nil {
			return err
		}
		if left > 0 {
			return nil
		}
		if removed, err = deletePendingIn(ctx, tx, u.IdentityID, s.now(), actor); err != nil {
			return err
		}
		u.PendingDeleted, u.Memory, u.Tokens = true, removed.memory, len(revoked)+len(removed.tokens)
		return nil
	})
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, ErrOwnerOnly) || errors.Is(err, ErrRoomIdentity) ||
		errors.Is(err, ErrSocketBinding) {
		return Unbound{}, err
	}
	if err != nil {
		return Unbound{}, fmt.Errorf("auth: unbind: %w", err)
	}
	s.emit(ctx, event.IdentityUnbound, map[string]any{
		"identity": u.IdentityID, "binding": bindingRef(channel, alias), "tokens_revoked": len(revoked), "by": actor,
	})
	s.emitTokensRevoked(ctx, revoked, actor, "unbound")
	if u.PendingDeleted {
		s.emitPendingDeleted(ctx, u.IdentityID, removed, map[string]any{"by": actor, "reason": "pending_unbound"})
		s.emitTokensRevoked(ctx, removed.tokens, actor, "identity_deleted")
	}
	return u, nil
}

// EnsureGuest returns the user behind the aliases on channel, creating a guest
// and linking the aliases when there is none (open access, docs/identity.md).
// It resolves like Resolve, so a match the recycled-id guard does not trust
// is no match: the guest gets only the aliases nobody holds, and the aliases
// of the untrusted identity stay where they are. With no free alias left it
// is store.ErrAliasLinked. A new guest is named by placeholderName; display only
// goes on the bindings, sanitized (linkIn).
func (s *Service) EnsureGuest(ctx context.Context, channel string, aliases []string, display string) (store.Identity, error) {
	if channel == "" || len(aliases) == 0 {
		return store.Identity{}, errors.New("auth: ensure guest: channel and aliases are required")
	}
	if i, err := s.Resolve(ctx, channel, aliases); err == nil {
		return i, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return store.Identity{}, err
	}
	var (
		i       store.Identity
		created bool
	)
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		// Re-check inside the lock: a concurrent EnsureGuest may have linked
		// the same alias in between.
		r, err := store.ResolveAliases(ctx, tx, channel, aliases, s.namespaces(channel))
		if err == nil {
			i = r.Identity
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if len(r.Free) == 0 {
			return fmt.Errorf("%w: %s: no free alias", store.ErrAliasLinked, channel)
		}
		aliases = r.Free
		id := event.NewID()
		i = store.Identity{ID: id, Kind: store.KindUser, Name: placeholderName(id), Status: store.StatusPending, CreatedAt: s.now()}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO identities (id, kind, name, role, status, created_at) VALUES (?, ?, ?, NULL, ?, ?)`,
			string(i.ID), string(i.Kind), i.Name, string(i.Status), ms(i.CreatedAt)); err != nil {
			return err
		}
		created = true
		_, err = linkIn(ctx, tx, channel, aliases, i.ID, display, s.now())
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrAliasLinked) {
			return store.Identity{}, err
		}
		return store.Identity{}, fmt.Errorf("auth: ensure guest: %w", err)
	}
	if created {
		refs := make([]string, 0, len(aliases))
		for _, alias := range aliases {
			refs = append(refs, bindingRef(channel, alias))
		}
		s.emitCreated(ctx, i, refs, "")
	}
	return i, nil
}

// placeholderName is what an identity is called until an admin names it: the
// last characters of its id, which are the random part of a ULID. A name never
// comes from the platform: a display name is attacker-chosen and an identity's
// name reaches the model (the skill provenance line, App.Teacher).
func placeholderName(id event.ID) string {
	s := string(id)
	if len(s) > 6 {
		s = s[len(s)-6:]
	}
	return "guest-" + strings.ToLower(s)
}

// linkIn binds aliases on one channel to an identity inside the caller's
// transaction. An alias already bound to the same identity only refreshes the
// display name; one owned by another user fails the whole transaction
// (store.ErrAliasLinked), so the caller's other writes roll back too. A room
// takes no alias (ErrRoomIdentity). display is sanitized here, so no caller
// stores a platform's name raw (agent.SanitizeDisplay). It returns the aliases
// it newly bound.
func linkIn(ctx context.Context, tx *sql.Tx, channel string, aliases []string, identityID event.ID, display string, now time.Time) ([]string, error) {
	var kind string
	err := tx.QueryRowContext(ctx, `SELECT kind FROM identities WHERE id = ?`, string(identityID)).Scan(&kind)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, store.ErrNotFound
	case err != nil:
		return nil, err
	case store.IdentityKind(kind) == store.KindRoom:
		return nil, ErrRoomIdentity
	}
	display = agent.SanitizeDisplay(display)
	var added []string
	for _, alias := range aliases {
		if alias == "" {
			return nil, errors.New("auth: empty alias")
		}
		var owner string
		err := tx.QueryRowContext(ctx,
			`SELECT identity_id FROM bindings WHERE channel = ? AND alias = ?`, channel, alias).Scan(&owner)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO bindings (channel, alias, identity_id, display, created_at) VALUES (?, ?, ?, ?, ?)`,
				channel, alias, string(identityID), display, ms(now)); err != nil {
				return nil, err
			}
			added = append(added, alias)
		case err != nil:
			return nil, err
		case owner != string(identityID):
			return nil, fmt.Errorf("%w: %s/%s", store.ErrAliasLinked, channel, alias)
		default:
			if _, err := tx.ExecContext(ctx,
				`UPDATE bindings SET display = ? WHERE channel = ? AND alias = ?`,
				display, channel, alias); err != nil {
				return nil, err
			}
		}
	}
	return added, nil
}

// bindingRef is how a binding is written everywhere: <channel>:<alias>.
func bindingRef(channel, alias string) string { return channel + ":" + alias }

func bindingRefs(bindings []store.Binding) []string {
	refs := make([]string, 0, len(bindings))
	for _, b := range bindings {
		refs = append(refs, bindingRef(b.Channel, b.Alias))
	}
	return refs
}

// emit reports a lifecycle change. Callers do it after the commit, so the
// caller going away mustn't drop the event: ctx's cancellation is ignored.
func (s *Service) emit(ctx context.Context, t event.Type, attrs map[string]any) {
	s.Events.Emit(context.WithoutCancel(ctx), event.New(t, event.NewID(), "", "", attrs))
}

// emitCreated reports a new identity: the bindings it came with and the admin
// who created it.
func (s *Service) emitCreated(ctx context.Context, i store.Identity, bindings []string, by event.ID) {
	attrs := map[string]any{"identity": i.ID, "kind": i.Kind, "status": i.Status}
	if len(bindings) > 0 {
		attrs["bindings"] = bindings
	}
	if by != "" {
		attrs["by"] = by
	}
	s.emit(ctx, event.IdentityCreated, attrs)
}

// emitConfirmed reports a confirmed identity, and what confirming a room approved.
func (s *Service) emitConfirmed(ctx context.Context, i store.Identity, approved []store.Binding, by event.ID) {
	s.emit(ctx, event.IdentityConfirmed, map[string]any{"identity": i.ID, "name": i.Name, "role": i.Role, "by": by})
	for _, b := range approved {
		s.emit(ctx, event.AccessGranted, map[string]any{
			"channel": b.Channel, "conversation": b.Alias,
			"conversation_kind": string(event.ConversationRoom), "approved_by": by,
		})
	}
}
