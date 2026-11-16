package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"holaclaw/internal/event"
)

// CreateIdentity inserts an identity. CreatedAt defaults to now, ID to a fresh
// ULID, Kind to a user and Status to confirmed; a confirmed user's Role
// defaults to RoleUser. A pending identity or a room with a role, or any
// identity with RoleGuest, is an error.
func (s *SQLite) CreateIdentity(ctx context.Context, i Identity) error {
	if i.Role == RoleGuest {
		return errors.New("store: create identity: guest is not an identity role")
	}
	if i.ID == "" {
		i.ID = event.NewID()
	}
	if i.Kind == "" {
		i.Kind = KindUser
	}
	if i.Status == "" {
		i.Status = StatusConfirmed
	}
	if i.Kind == KindUser && i.Status == StatusConfirmed && i.Role == "" {
		i.Role = RoleUser
	}
	if i.Pending() && i.Role != "" {
		return errors.New("store: create identity: a pending identity has no role")
	}
	if i.Kind == KindRoom && i.Role != "" {
		return errors.New("store: create identity: a room has no role")
	}
	_, err := s.exec(ctx, `INSERT INTO identities (id, kind, name, role, status, approved_at, approved_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(i.ID), string(i.Kind), i.Name, nullRole(i.Role), string(i.Status), nullTime(i.ApprovedAt), nullID(i.ApprovedBy), ms(i.CreatedAt))
	if err != nil {
		return fmt.Errorf("store: create identity: %w", err)
	}
	return nil
}

func nullRole(r Role) any {
	if r == "" {
		return nil
	}
	return string(r)
}

const identityColumns = `id, kind, name, role, status, approved_at, approved_by, created_at`

func scanIdentity(sc interface{ Scan(...any) error }) (Identity, error) {
	var i Identity
	var id, kind, status string
	var role sql.NullString
	var approved sql.NullInt64
	var approvedBy sql.NullString
	var created int64
	if err := sc.Scan(&id, &kind, &i.Name, &role, &status, &approved, &approvedBy, &created); err != nil {
		return Identity{}, err
	}
	i.ID, i.Kind, i.Role, i.Status, i.CreatedAt = event.ID(id), IdentityKind(kind), Role(role.String), IdentityStatus(status), fromMS(created)
	i.ApprovedAt, i.ApprovedBy = timePtr(approved), event.ID(approvedBy.String)
	return i, nil
}

func (s *SQLite) GetIdentity(ctx context.Context, id event.ID) (Identity, error) {
	i, err := IdentityIn(ctx, s.db, id)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Identity{}, fmt.Errorf("store: get identity: %w", err)
	}
	return i, err
}

// IdentityIn is GetIdentity as q sees it.
func IdentityIn(ctx context.Context, q Querier, id event.ID) (Identity, error) {
	i, err := scanIdentity(q.QueryRowContext(ctx, `SELECT `+identityColumns+` FROM identities WHERE id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, ErrNotFound
	}
	return i, err
}

func (s *SQLite) ListIdentities(ctx context.Context) ([]Identity, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+identityColumns+` FROM identities ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Identity
	for rows.Next() {
		i, err := scanIdentity(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list identities: %w", err)
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// DeleteIdentity removes the identity; its bindings, API tokens and invites
// cascade, sessions keep their rows with a null user.
func (s *SQLite) DeleteIdentity(ctx context.Context, id event.ID) error {
	res, err := s.exec(ctx, `DELETE FROM identities WHERE id = ?`, string(id))
	if err != nil {
		return fmt.Errorf("store: delete identity: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// BindingIdentityIn returns the identity holding one binding as q sees it, in
// one query, or ErrNotFound when no binding holds the alias.
func BindingIdentityIn(ctx context.Context, q Querier, channel, alias string) (Identity, error) {
	i, err := scanIdentity(q.QueryRowContext(ctx, `SELECT i.id, i.kind, i.name, i.role, i.status, i.approved_at, i.approved_by, i.created_at
		FROM bindings b JOIN identities i ON i.id = b.identity_id WHERE b.channel = ? AND b.alias = ?`, channel, alias))
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, ErrNotFound
	}
	return i, err
}

// GetRoom returns the room identity bound to a conversation.
func (s *SQLite) GetRoom(ctx context.Context, channel, conversation string) (Identity, error) {
	i, err := roomIn(ctx, s.db, channel, conversation)
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrAliasLinked) {
		return Identity{}, fmt.Errorf("store: get room: %w", err)
	}
	return i, err
}

// roomIn is GetRoom as q sees it. The binding is read first and the identity
// by id, so a binding a user holds is told apart from no binding at all.
func roomIn(ctx context.Context, q Querier, channel, conversation string) (Identity, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT identity_id FROM bindings WHERE channel = ? AND alias = ?`,
		channel, conversation).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, ErrNotFound
	}
	if err != nil {
		return Identity{}, err
	}
	i, err := IdentityIn(ctx, q, event.ID(id))
	if err != nil {
		return Identity{}, err
	}
	if i.Kind != KindRoom {
		return Identity{}, fmt.Errorf("%w: %s/%s is not a room's", ErrAliasLinked, channel, conversation)
	}
	return i, nil
}

// EnsureRoom returns the room of a conversation, creating it pending, and
// reports whether this call created it.
func (s *SQLite) EnsureRoom(ctx context.Context, channel, conversation string, at time.Time) (Identity, bool, error) {
	var (
		i       Identity
		created bool
	)
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		var err error
		i, created, err = ensureRoomIn(ctx, tx, channel, conversation, at)
		return err
	})
	if err != nil && !errors.Is(err, ErrAliasLinked) {
		return Identity{}, false, fmt.Errorf("store: ensure room: %w", err)
	}
	return i, created, err
}

// ensureRoomIn is EnsureRoom inside a transaction. The room is named after its
// conversation until someone confirms it.
func ensureRoomIn(ctx context.Context, tx *sql.Tx, channel, conversation string, at time.Time) (Identity, bool, error) {
	if channel == "" || conversation == "" {
		return Identity{}, false, errors.New("channel and conversation are required")
	}
	i, err := roomIn(ctx, tx, channel, conversation)
	if !errors.Is(err, ErrNotFound) {
		return i, false, err
	}
	created := fromMS(ms(at))
	i = Identity{ID: event.NewIDAt(created), Kind: KindRoom, Name: conversation, Status: StatusPending, CreatedAt: created}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO identities (id, kind, name, role, status, approved_at, approved_by, created_at) VALUES (?, ?, ?, NULL, ?, NULL, NULL, ?)`,
		string(i.ID), string(i.Kind), i.Name, string(i.Status), ms(at)); err != nil {
		return Identity{}, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO bindings (channel, alias, identity_id, display, created_at) VALUES (?, ?, ?, '', ?)`,
		channel, conversation, string(i.ID), ms(at)); err != nil {
		return Identity{}, false, err
	}
	return i, true, nil
}

// RoomChange says what approving a room did to it.
type RoomChange struct {
	// Created reports that the room did not exist and approving created it.
	Created bool
	// Approved reports that this call approved it.
	Approved bool
}

// ApproveRoom approves the room of a conversation, creating it when needed.
func (s *SQLite) ApproveRoom(ctx context.Context, channel, conversation string, by event.ID, at time.Time) (Identity, RoomChange, error) {
	var (
		i      Identity
		change RoomChange
	)
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		var err error
		i, change, err = ApproveRoomIn(ctx, tx, channel, conversation, by, at)
		return err
	})
	if err != nil && !errors.Is(err, ErrAliasLinked) && !errors.Is(err, ErrRoomDisabled) {
		return Identity{}, RoomChange{}, fmt.Errorf("store: approve room: %w", err)
	}
	return i, change, err
}

// ApproveRoomIn is ApproveRoom inside a transaction, so a room invite and the room
// it approves stand or fall together.
func ApproveRoomIn(ctx context.Context, tx *sql.Tx, channel, conversation string, by event.ID, at time.Time) (Identity, RoomChange, error) {
	i, created, err := ensureRoomIn(ctx, tx, channel, conversation, at)
	switch {
	case err != nil:
		return Identity{}, RoomChange{}, err
	case i.Disabled():
		return i, RoomChange{}, ErrRoomDisabled
	case i.Approved():
		return i, RoomChange{}, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE identities SET approved_at = ?, approved_by = ? WHERE id = ?`, ms(at), nullID(by), string(i.ID)); err != nil {
		return Identity{}, RoomChange{}, err
	}
	t := fromMS(ms(at))
	i.ApprovedAt, i.ApprovedBy = &t, by
	return i, RoomChange{Created: created, Approved: true}, nil
}

// Querier is a *sql.DB or a *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ResolveAliases matches one envelope's aliases on channel as q sees them
// (docs/identity.md §2). The owners of the aliases are tried in envelope
// order and the first trusted one wins. An owner is not trusted when one of
// the envelope's aliases it does not hold falls in a namespace where it
// already holds another: that alias would be its second id there, the mark of
// a recycled phone number. namespaces are the channel's declared alias
// prefixes; an alias is in the longest one it starts with, and an alias in
// none is never a conflict. ErrNotFound when no owner is trusted.
//
// An alias a room holds never resolves a sender and isn't free either: a
// conversation id on someone's envelope must not speak as the room or take its
// binding.
//
// The owners and each owner's held aliases are read in separate queries, and
// they must see one snapshot: an alias linked by a concurrent envelope between
// the two would be free in one and held in the other, and the owner would be
// refused as recycled. Given a *sql.DB, the reads run in one read transaction.
func ResolveAliases(ctx context.Context, q Querier, channel string, aliases, namespaces []string) (Resolution, error) {
	var r Resolution
	aliases = uniqueAliases(aliases)
	if channel == "" || len(aliases) == 0 {
		return r, ErrNotFound
	}
	if db, ok := q.(*sql.DB); ok {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return r, fmt.Errorf("store: resolve aliases: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		q = tx
	}
	owners, rooms, err := aliasOwners(ctx, q, channel, aliases)
	if err != nil {
		return r, fmt.Errorf("store: resolve aliases: %w", err)
	}
	tried := map[event.ID]bool{}
	for _, a := range aliases {
		iid, ok := owners[a]
		if !ok || tried[iid] || rooms[iid] {
			continue
		}
		tried[iid] = true
		held, err := heldAliases(ctx, q, channel, iid)
		if err != nil {
			return r, fmt.Errorf("store: resolve aliases: %w", err)
		}
		if c := recycled(aliases, owners, iid, held, namespaces); len(c) > 0 {
			r.Conflicts = append(r.Conflicts, c...)
			continue
		}
		i, err := IdentityIn(ctx, q, iid)
		if err != nil {
			return r, fmt.Errorf("store: resolve aliases: %w", err)
		}
		r.Identity, r.Matched = i, a
		break
	}
	for _, a := range aliases {
		iid, ok := owners[a]
		switch {
		case !ok:
			r.Free = append(r.Free, a)
		case r.Identity.ID != "" && iid != r.Identity.ID && !tried[iid]:
			// An owner tried and not trusted is already reported.
			r.Conflicts = append(r.Conflicts, AliasConflict{Alias: a, IdentityID: iid})
		}
	}
	if r.Identity.ID == "" {
		return r, ErrNotFound
	}
	return r, nil
}

// ResolveAndLink is ResolveAliases followed, in the same transaction, by
// linking the free aliases to the identity found. Ids from one envelope come
// from one authenticated sender; ids from different envelopes are never
// joined. A disabled identity links nothing.
func (s *SQLite) ResolveAndLink(ctx context.Context, channel string, aliases []string, display string, namespaces []string) (Resolution, error) {
	// Most messages have nothing to link: answer those without the write
	// lock, and redo the whole resolve under it otherwise.
	r, err := ResolveAliases(ctx, s.db, channel, aliases, namespaces)
	switch {
	case errors.Is(err, ErrNotFound):
		return r, err
	case err != nil:
		return Resolution{}, err
	case len(r.Free) == 0 || r.Identity.Disabled():
		return r, nil
	}
	now := time.Now().UnixMilli()
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		var err error
		if r, err = ResolveAliases(ctx, tx, channel, aliases, namespaces); err != nil {
			return err
		}
		if r.Identity.Disabled() {
			return nil
		}
		for _, a := range r.Free {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO bindings (channel, alias, identity_id, display, created_at) VALUES (?, ?, ?, ?, ?)`,
				channel, a, string(r.Identity.ID), display, now); err != nil {
				return fmt.Errorf("store: resolve and link: %w", err)
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Resolution{}, err
	}
	return r, err
}

func uniqueAliases(aliases []string) []string {
	out := make([]string, 0, len(aliases))
	seen := map[string]bool{}
	for _, a := range aliases {
		if a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

// aliasOwners maps each linked alias to its identity, and says which of those
// identities are rooms.
func aliasOwners(ctx context.Context, q Querier, channel string, aliases []string) (map[string]event.ID, map[event.ID]bool, error) {
	args := make([]any, 0, len(aliases)+1)
	args = append(args, channel)
	for _, a := range aliases {
		args = append(args, a)
	}
	rows, err := q.QueryContext(ctx, `SELECT b.alias, b.identity_id, i.kind
	      FROM bindings b JOIN identities i ON i.id = b.identity_id
	      WHERE b.channel = ? AND b.alias IN (`+placeholders(len(aliases))+`)`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	out, rooms := map[string]event.ID{}, map[event.ID]bool{}
	for rows.Next() {
		var alias, iid, kind string
		if err := rows.Scan(&alias, &iid, &kind); err != nil {
			return nil, nil, err
		}
		out[alias] = event.ID(iid)
		rooms[event.ID(iid)] = IdentityKind(kind) == KindRoom
	}
	return out, rooms, rows.Err()
}

// heldAliases lists the aliases an identity holds on channel.
func heldAliases(ctx context.Context, q Querier, channel string, iid event.ID) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT alias FROM bindings WHERE channel = ? AND identity_id = ? ORDER BY alias`, channel, string(iid))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// recycled lists the envelope aliases iid does not hold whose namespace it
// already holds another alias in.
func recycled(aliases []string, owners map[string]event.ID, iid event.ID, held, namespaces []string) []AliasConflict {
	var out []AliasConflict
	for _, a := range aliases {
		ns := namespaceOf(a, namespaces)
		if ns == "" || owners[a] == iid {
			continue
		}
		for _, h := range held {
			if namespaceOf(h, namespaces) == ns {
				out = append(out, AliasConflict{Alias: a, IdentityID: iid, Held: h})
				break
			}
		}
	}
	return out
}

// namespaceOf returns the longest declared namespace alias starts with, so
// "wa:lid:1" is in "wa:lid:" and not in "wa:".
func namespaceOf(alias string, namespaces []string) string {
	best := ""
	for _, ns := range namespaces {
		if ns != "" && len(ns) > len(best) && strings.HasPrefix(alias, ns) {
			best = ns
		}
	}
	return best
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
