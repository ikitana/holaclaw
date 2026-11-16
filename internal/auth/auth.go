// Package auth owns who may talk to holaclaw: API tokens with three fixed
// scopes, the bootstrap owner, users and their channel identities, one-shot
// invite codes and open-access guests (docs/identity.md, docs/security.md).
//
// Secrets (token plaintexts, invite codes) exist exactly once, in the return
// value of the call that mints them; the database keeps only a SHA-256 hash,
// and no error or log line ever carries the secret.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"sync"
	"time"

	"holaclaw/internal/agent"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// Service is the access layer built on the shared store: it uses the Store
// methods for users and identities and the same database handle for its own
// tables. Writes are serialised in-process by mu, the way the store serialises
// its own; other processes are covered by busy_timeout.
type Service struct {
	st *store.SQLite
	db *sql.DB

	// Now is the clock used for expiry, throttling and rate limiting. Tests
	// replace it; it must be set before the service is used concurrently.
	Now func() time.Time

	// BeforeDeleteUser, when set, runs before a user is removed; an error aborts
	// the deletion. The daemon uses it to cancel the user's tasks, so nothing
	// keeps acting for a deleted user.
	BeforeDeleteUser func(ctx context.Context, id event.ID) error

	// BeforeEnable, when set, runs before a disabled identity is enabled, while
	// it still reads as disabled; an error aborts the enable. The daemon uses it
	// to apply the missed-run policy to the identity's tasks, which were on
	// hold: once it reads as enabled, the scheduler would fire them from their
	// stale next_run.
	BeforeEnable func(ctx context.Context, id event.ID) error

	// AfterEnable, when set, runs after a disabled identity was enabled, only
	// when the status actually changed. The daemon uses it to wake the
	// scheduler, which now sees the identity's re-armed tasks.
	AfterEnable func(ctx context.Context, id event.ID)

	// ForgetUserMemory, when set, forgets the user's own memory partition
	// before the user is removed, unless the caller keeps it; it returns the
	// rows forgotten and an error aborts the deletion.
	ForgetUserMemory func(ctx context.Context, id event.ID) (int64, error)

	// Events receives identity lifecycle events, always after the commit.
	// Defaults to event.Discard; the daemon wires it to the real sink.
	Events event.Sink

	// Namespaces returns the alias namespaces a channel instance declares
	// (channel.AliasNamespacer), for the recycled-id guard of every resolve.
	// Nil means none.
	Namespaces func(channel string) []string

	mu sync.Mutex // serialises writes

	rlMu sync.Mutex
	rl   map[string][]time.Time // (channel, alias) -> recent redeem failures
}

// New returns a service over an open store.
func New(st *store.SQLite) *Service {
	return &Service{st: st, db: st.DB(), Now: time.Now, rl: map[string][]time.Time{}, Events: event.Discard}
}

func (s *Service) now() time.Time { return s.Now().UTC() }

// hashSecret is the one-way function behind every stored secret.
func hashSecret(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// randomBytes panics only if the system CSPRNG fails, which is fatal anyway.
func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

// withTx runs fn in a write transaction under the service write lock.
func (s *Service) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ms converts a time to the unix milliseconds used by every timestamp column.
func ms(t time.Time) int64 { return t.UnixMilli() }

// fromMS converts a stored timestamp back to UTC time.
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func nullMS(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMS(v.Int64)
	return &t
}

func nullID(v sql.NullString) event.ID { return event.ID(v.String) }

// idArg renders an optional id for a nullable column.
func idArg(id event.ID) any {
	if id == "" {
		return nil
	}
	return string(id)
}

// Resolve returns the first trusted user owning one of the aliases on
// channel (store.ResolveAliases), or store.ErrNotFound. It links nothing.
func (s *Service) Resolve(ctx context.Context, channel string, aliases []string) (store.Identity, error) {
	r, err := store.ResolveAliases(ctx, s.db, channel, aliases, s.namespaces(channel))
	return r.Identity, err
}

// ResolveAndLink resolves a sender like Resolve and links the sender's free
// aliases to the user found, in one transaction (store.ResolveAndLink). The
// bindings carry the display name sanitized (agent.SanitizeDisplay).
func (s *Service) ResolveAndLink(ctx context.Context, channel string, sender event.Sender) (store.Resolution, error) {
	return s.st.ResolveAndLink(ctx, channel, sender.Aliases, agent.SanitizeDisplay(sender.DisplayName), s.namespaces(channel))
}

func (s *Service) namespaces(channel string) []string {
	if s.Namespaces == nil {
		return nil
	}
	return s.Namespaces(channel)
}
