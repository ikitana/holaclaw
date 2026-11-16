package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// owners counts the owner rows behind s.
func owners(t *testing.T, s *Service) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), `SELECT count(*) FROM identities WHERE role = 'owner'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateOwnerOnce(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	b := s.Bootstrap()
	if ok, err := b.Initialised(ctx); err != nil || ok {
		t.Fatalf("Initialised = %v, %v on an empty database", ok, err)
	}

	boot, err := s.CreateOwner(ctx, true, nil)
	secret, tok := boot.Secret, boot.Token
	if err != nil {
		t.Fatalf("CreateOwner: %v", err)
	}
	if tok.Scope != ScopeAdmin || tok.IdentityID == "" || tok.Binding == "" {
		t.Fatalf("token = %+v, want admin scope tied to a user", tok)
	}
	p, err := s.Authenticate(ctx, secret)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Role != store.RoleOwner || p.UserID != tok.IdentityID || p.Scope != ScopeAdmin {
		t.Fatalf("principal = %+v, want the owner with admin scope", p)
	}
	// The owner holds holaclaw:cli for the socket and an init binding for the
	// token.
	bindings, err := s.Bindings(ctx, tok.IdentityID)
	if err != nil || len(bindings) != 2 {
		t.Fatalf("owner bindings = %+v, err %v", bindings, err)
	}
	for _, b := range bindings {
		switch {
		case b.Channel != event.ChannelHolaclaw:
			t.Fatalf("owner binding %+v is not holaclaw", b)
		case b.Alias == SocketAlias:
		case b.Alias != tok.Binding || b.Display != initTokenName:
			t.Fatalf("init binding = %+v, want %s named %q", b, tok.Binding, initTokenName)
		}
	}
	if ok, err := b.Initialised(ctx); err != nil || !ok {
		t.Fatalf("Initialised = %v, %v after CreateOwner", ok, err)
	}
	if _, err := s.CreateOwner(ctx, true, nil); !errors.Is(err, ErrInitialised) {
		t.Fatalf("second CreateOwner = %v, want ErrInitialised", err)
	}
	if n := owners(t, s); n != 1 {
		t.Fatalf("%d owners, want 1", n)
	}
}

// An owner written by another process (`holaclaw init` on the database) is
// seen on the next check.
func TestBootstrapSeesOwnerCreatedElsewhere(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	b := s.Bootstrap()
	if ok, err := b.Initialised(ctx); err != nil || ok {
		t.Fatalf("Initialised = %v, %v on an empty database", ok, err)
	}
	// A second service on the same store stands in for `holaclaw init`.
	if _, err := New(s.st).CreateOwner(ctx, true, nil); err != nil {
		t.Fatal(err)
	}
	if ok, err := b.Initialised(ctx); err != nil || !ok {
		t.Fatalf("Initialised = %v, %v after an outside owner", ok, err)
	}
}

func TestCreateOwnerConcurrentOneWins(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	const n = 8
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	for range n {
		wg.Go(func() {
			_, err := s.CreateOwner(ctx, true, nil)
			switch {
			case err == nil:
				mu.Lock()
				wins++
				mu.Unlock()
			case !errors.Is(err, ErrInitialised):
				t.Errorf("CreateOwner: %v", err)
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d calls won, want exactly 1", wins)
	}
	if n := owners(t, s); n != 1 {
		t.Fatalf("%d owners, want 1", n)
	}
}

func TestBootstrapWait(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	b := s.Bootstrap()

	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := b.Wait(short, time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait without an owner = %v, want the deadline", err)
	}

	done := make(chan error, 1)
	go func() { done <- b.Wait(ctx, 5*time.Millisecond) }()
	if _, err := s.CreateOwner(ctx, true, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not notice the owner")
	}
}

// A token that never reaches anyone is no bootstrap: the owner rolls back
// and the next call can create it.
func TestCreateOwnerRollsBackWhenDeliveryFails(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	lost := errors.New("disk full")

	var got Bootstrapped
	_, err := s.CreateOwner(ctx, true, func(b Bootstrapped) error {
		got = b
		return lost
	})
	if !errors.Is(err, lost) {
		t.Fatalf("CreateOwner = %v, want the delivery error", err)
	}
	if n := owners(t, s); n != 0 {
		t.Fatalf("%d owners after a failed delivery, want 0", n)
	}
	if got.Secret == "" {
		t.Fatalf("delivered %+v, want a token", got)
	}
	if _, err := s.Authenticate(ctx, got.Secret); err == nil {
		t.Fatal("the undelivered token authenticates")
	}
	if ok, err := s.Bootstrap().Initialised(ctx); err != nil || ok {
		t.Fatalf("Initialised = %v, %v after a failed delivery", ok, err)
	}

	boot, err := s.CreateOwner(ctx, true, nil)
	secret := boot.Secret
	if err != nil {
		t.Fatalf("CreateOwner after a failed one: %v", err)
	}
	if _, err := s.Authenticate(ctx, secret); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
}

// A write that fails halfway leaves no owner without its token.
func TestCreateOwnerRollsBackWhenTheTokenInsertFails(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_token BEFORE INSERT ON api_tokens
		BEGIN SELECT RAISE(ABORT, 'disk on fire'); END`); err != nil {
		t.Fatal(err)
	}
	delivered := false
	if _, err := s.CreateOwner(ctx, true, func(Bootstrapped) error { delivered = true; return nil }); err == nil {
		t.Fatal("CreateOwner succeeded with the token insert failing")
	}
	if delivered {
		t.Fatal("a token was delivered for a failed bootstrap")
	}
	if n := owners(t, s); n != 0 {
		t.Fatalf("%d owners after a failed token insert, want 0", n)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER fail_token`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOwner(ctx, true, nil); err != nil {
		t.Fatalf("CreateOwner after a failed one: %v", err)
	}
}

// Once an owner exists, CreateOwner does nothing: deliver is not called.
func TestCreateOwnerAfterSuccessDeliversNothing(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if _, err := s.CreateOwner(ctx, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOwner(ctx, true, func(Bootstrapped) error {
		t.Error("deliver called with an owner present")
		return nil
	}); !errors.Is(err, ErrInitialised) {
		t.Fatalf("CreateOwner = %v, want ErrInitialised", err)
	}
	if n := owners(t, s); n != 1 {
		t.Fatalf("%d owners, want 1", n)
	}
}

// initOwner does what `holaclaw init --token-file` does and returns the owner
// it made.
func initOwner(ctx context.Context, s *Service) (store.Identity, error) {
	if _, err := s.CreateOwner(ctx, true, nil); err != nil {
		return store.Identity{}, err
	}
	return s.Owner(ctx)
}

// Owner only reads: before init there is none, and asking makes none.
func TestOwnerBeforeInit(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	if _, err := s.Owner(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Owner before init: err = %v, want ErrNotFound", err)
	}
	if n := owners(t, s); n != 0 {
		t.Fatalf("owners = %d after Owner, want 0", n)
	}
	boot, err := s.CreateOwner(ctx, true, nil)
	secret, tok := boot.Secret, boot.Token
	if err != nil {
		t.Fatal(err)
	}
	i, err := s.Owner(ctx)
	if err != nil || i.ID != tok.IdentityID || i.Role != store.RoleOwner {
		t.Fatalf("Owner after init = %+v, %v; want the token's user %q", i, err, tok.IdentityID)
	}
	if p, err := s.Authenticate(ctx, secret); err != nil || p.UserID != i.ID {
		t.Fatalf("the init token authenticates as %+v, %v", p, err)
	}
}
