package session

import (
	"context"
	"errors"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// API messages skip the gate, so the router is the last line: a disabled
// sender starts nothing and links nothing.
func TestRouteDropsADisabledIdentity(t *testing.T) {
	t.Parallel()
	rh := newRouterHarness(t)
	ctx := context.Background()
	if _, err := rh.st.DB().ExecContext(ctx, `UPDATE identities SET status = 'disabled' WHERE id = ?`, string(rh.identity.ID)); err != nil {
		t.Fatal(err)
	}

	acc, err := rh.r.Route(ctx, aliasEnvelope("tg-main", "11", "33"))
	if !errors.Is(err, ErrIdentityDisabled) {
		t.Fatalf("err = %v, want ErrIdentityDisabled", err)
	}
	if acc.SessionID != "" || acc.MessageID != "" {
		t.Fatalf("accepted = %+v", acc)
	}
	d := rh.ev.of(event.RouterDropped)
	if len(d) != 1 || attrs(t, d[0])["reason"] != "identity_disabled" {
		t.Fatalf("router.dropped = %+v", d)
	}
	if b := rh.ev.of(event.IdentityBound); len(b) != 0 {
		t.Fatalf("identity.bound = %+v", b)
	}
	r, err := store.ResolveAliases(ctx, rh.st.DB(), "tg-main", []string{"33"}, nil)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the free alias was linked: %+v, %v", r, err)
	}

	// Enabled again, the same envelope goes through.
	if _, err := rh.st.DB().ExecContext(ctx, `UPDATE identities SET status = 'confirmed' WHERE id = ?`, string(rh.identity.ID)); err != nil {
		t.Fatal(err)
	}
	if acc, err = rh.r.Route(ctx, aliasEnvelope("tg-main", "11", "33")); err != nil || acc.SessionID == "" {
		t.Fatalf("after enable = %+v, %v", acc, err)
	}
}
