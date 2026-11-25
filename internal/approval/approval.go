// Package approval persists the approvals framework of docs/security.md: the
// grants that turn an Ask into an Allow, and the human decisions a suspended
// tool call waits on.
//
// A Service is a tool.Approver and a tool.GrantMatcher, so the policy gate
// depends on the interfaces in package tool and never on this package.
package approval

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/tool"
)

// DefaultTimeout is how long a tool approval stays pending. It holds a turn
// open, so it is short.
const DefaultTimeout = 5 * time.Minute

var (
	// ErrNotFound reports an unknown approval id.
	ErrNotFound = errors.New("approval: not found")
	// ErrNotPending reports a second resolution of the same approval: the
	// first decision stands.
	ErrNotPending = errors.New("approval: already resolved")
	// ErrOnceOnly reports a session or always answer to an approval that
	// covers one call only. Nothing changed: it is still pending.
	ErrOnceOnly = errors.New("approval: can only be approved once")
)

// Service stores grants and approvals in the shared database and wakes the
// caller blocked on one. Safe for concurrent use.
type Service struct {
	db     *sql.DB
	events event.Sink

	now          func() time.Time
	timeout      time.Duration
	grantPattern func(tool.Approval) string

	writeMu sync.Mutex // serialises this process's writers, like store.SQLite

	mu      sync.Mutex // guards waiters; never held while waiting
	waiters map[event.ID]chan tool.Approval
}

// Option configures New.
type Option func(*Service)

// WithNow replaces the clock (tests).
func WithNow(fn func() time.Time) Option { return func(s *Service) { s.now = fn } }

// WithTimeout sets how long a tool approval stays pending (default
// DefaultTimeout). Timeout is a deny.
func WithTimeout(d time.Duration) Option { return func(s *Service) { s.timeout = d } }

// WithGrantPattern sets how an approved "session" or "always" choice narrows
// into a grant pattern: for an exec tool the exact command, "" (any argument)
// for everything else. Without it, approvals grant the whole tool.
func WithGrantPattern(fn func(tool.Approval) string) Option {
	return func(s *Service) { s.grantPattern = fn }
}

// New builds a Service over the shared handle from store.SQLite.DB().
func New(db *sql.DB, sink event.Sink, opts ...Option) *Service {
	if sink == nil {
		sink = event.Discard
	}
	s := &Service{
		db: db, events: sink, now: time.Now, timeout: DefaultTimeout,
		waiters: map[event.ID]chan tool.Approval{},
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

var (
	_ tool.Approver     = (*Service)(nil)
	_ tool.GrantMatcher = (*Service)(nil)
)

func (s *Service) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.db.ExecContext(ctx, q, args...)
}

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }
func nullMS(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMS(v.Int64)
	return &t
}
