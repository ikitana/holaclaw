package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// initTokenName names the admin token minted at bootstrap.
const initTokenName = "init"

// Bootstrap is the state of a daemon that may have no owner yet
// (docs/identity.md §3). The owner comes from `holaclaw init`, which calls
// CreateOwner on the database directly; the daemon only watches for it. Once
// an owner exists it stays that way for the life of the database.
type Bootstrap struct {
	s *Service

	once  sync.Once
	ready chan struct{} // closed once an owner is known to exist
}

// Bootstrap returns the bootstrap state for the database behind s.
func (s *Service) Bootstrap() *Bootstrap {
	return &Bootstrap{s: s, ready: make(chan struct{})}
}

// Initialised reports whether an owner exists. Until one does, every call
// reads the database, so an owner created by another process is seen on the
// next call; after that the answer is cached.
func (b *Bootstrap) Initialised(ctx context.Context) (bool, error) {
	select {
	case <-b.ready:
		return true, nil
	default:
	}
	var n int
	err := b.s.db.QueryRowContext(ctx, `SELECT count(*) FROM identities WHERE role = 'owner'`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("auth: check owner: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	b.done()
	return true, nil
}

// Bootstrapped is what CreateOwner made. The secret exists only here.
type Bootstrapped struct {
	Owner store.Identity
	// Secret is the admin token of the `init` binding, and Token describes it;
	// both are empty unless the token was asked for.
	Secret string
	Token  Token
}

// Deliver hands what the bootstrap made to whoever asked for it. It runs
// inside the bootstrap transaction, after the inserts and before the commit,
// while the service holds its write lock: an error rolls the owner back, so a
// token that never reached anyone leaves the database uninitialised and the
// bootstrap callable again.
//
// The transaction holds the database write lock, across processes, for as
// long as Deliver runs: a running daemon's writes wait on it and fail after
// the busy timeout. Deliver should not block for long, so a pipe nobody reads
// (`holaclaw init | less`) is a poor place to deliver to; a file is a good one.
type Deliver func(Bootstrapped) error

// CreateOwner creates the owner with holaclaw:cli, the binding the unix socket
// arrives as, in one transaction (docs/identity.md §2). With withToken it also
// creates an `init` binding holding one admin token. It hands what it made to
// deliver (which may be nil) before committing, and returns it once. Any
// failure, deliver's included, rolls everything back. The owner's devices pair
// later, with a holaclaw invite.
// The transaction checks for an owner first, so of concurrent callers, in
// this process or another, exactly one wins and the rest are ErrInitialised
// without deliver being called.
func (s *Service) CreateOwner(ctx context.Context, withToken bool, deliver Deliver) (Bootstrapped, error) {
	owner := store.Identity{ID: event.NewID(), Kind: store.KindUser, Name: ownerName, Role: store.RoleOwner, Status: store.StatusConfirmed, CreatedAt: s.now()}
	out := Bootstrapped{Owner: owner}
	if withToken {
		var err error
		if out.Secret, out.Token, err = s.newToken(initTokenName, ScopeAdmin, OriginInit, "", ""); err != nil {
			return Bootstrapped{}, err
		}
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := ownerIn(ctx, tx); err == nil {
			return ErrInitialised
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO identities (id, kind, name, role, status, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			string(owner.ID), string(owner.Kind), owner.Name, string(owner.Role), string(owner.Status), ms(owner.CreatedAt)); err != nil {
			return err
		}
		if _, err := linkIn(ctx, tx, event.ChannelHolaclaw, []string{SocketAlias}, owner.ID, "", owner.CreatedAt); err != nil {
			return err
		}
		if withToken {
			if err := s.bindTokenIn(ctx, tx, &out.Token, hashSecret(out.Secret), owner.ID); err != nil {
				return err
			}
		}
		if deliver != nil {
			if err := deliver(out); err != nil {
				return fmt.Errorf("deliver the owner's token: %w", err)
			}
		}
		return nil
	})
	if errors.Is(err, ErrInitialised) {
		return Bootstrapped{}, err
	}
	if err != nil {
		return Bootstrapped{}, fmt.Errorf("auth: bootstrap: %w", err)
	}
	bindings := []string{bindingRef(event.ChannelHolaclaw, SocketAlias)}
	if withToken {
		bindings = append(bindings, bindingRef(event.ChannelHolaclaw, out.Token.Binding))
	}
	s.emitCreated(ctx, owner, bindings, "")
	if withToken {
		s.emitTokenCreated(ctx, out.Token)
	}
	return out, nil
}

// Wait blocks until an owner exists, checking the database every interval
// so an owner created by another process is noticed too. It returns
// ctx.Err() when ctx ends first.
func (b *Bootstrap) Wait(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if ok, err := b.Initialised(ctx); err == nil && ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-b.ready:
			return nil
		case <-t.C:
		}
	}
}

// done marks the database initialised.
func (b *Bootstrap) done() {
	b.once.Do(func() { close(b.ready) })
}
