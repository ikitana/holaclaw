package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/store/storetest"
)

var (
	asAdmin = Principal{UserID: "admin1", Role: store.RoleAdmin}
)

func (r *recordSink) of(t event.Type) []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, e := range r.evts {
		if e.Type == t {
			var m map[string]any
			if err := json.Unmarshal(e.Attrs, &m); err != nil {
				panic(err)
			}
			out = append(out, m)
		}
	}
	return out
}

func newRecorded(t *testing.T) (*Service, *recordSink) {
	t.Helper()
	s, _ := newService(t)
	rec := &recordSink{}
	s.Events = rec
	return s, rec
}

func confirmed(t *testing.T, s *Service, name string, role store.Role, aliases ...string) store.Identity {
	t.Helper()
	i, err := s.CreateIdentity(context.Background(), name, role, EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) > 0 {
		storetest.Bind(t, s.st, "tg", aliases, i.ID, name)
	}
	return i
}

func pending(t *testing.T, s *Service, aliases ...string) store.Identity {
	t.Helper()
	i, err := s.EnsureGuest(context.Background(), "tg", aliases, "Stranger")
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func exists(t *testing.T, s *Service, id event.ID) bool {
	t.Helper()
	_, err := s.st.GetIdentity(context.Background(), id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	return err == nil
}

// sharedNote writes a shared note into a partition, the one kind of row a
// pending identity's deletion leaves.
func sharedNote(t *testing.T, s *Service, partition string) string {
	t.Helper()
	id := string(event.NewID())
	if _, err := s.db.Exec(`INSERT INTO memory_items (id, agent, layer, text, visibility, "partition", created_at)
		VALUES (?, 'main', 'note', 'the office opens at nine', 'shared', ?, 1)`, id, partition); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestCreateIdentityThenFetch(t *testing.T) {
	ctx := context.Background()
	s, rec := newRecorded(t)
	i, err := s.CreateIdentity(ctx, "Carla", "", "admin1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.st.GetIdentity(ctx, i.ID)
	if err != nil || got.Name != "Carla" || got.Role != store.RoleUser || got.Status != store.StatusConfirmed {
		t.Fatalf("fetched = %+v, %v", got, err)
	}
	if b, err := s.Bindings(ctx, i.ID); err != nil || len(b) != 0 {
		t.Fatalf("bindings = %v, %v, want none", b, err)
	}
	ev := rec.of(event.IdentityCreated)
	if len(ev) != 1 || ev[0]["identity"] != string(i.ID) || ev[0]["kind"] != "user" || ev[0]["status"] != "confirmed" ||
		ev[0]["by"] != "admin1" || ev[0]["bindings"] != nil {
		t.Fatalf("identity.created = %v", ev)
	}
	for _, role := range []store.Role{store.RoleOwner, store.RoleGuest, "nope"} {
		if _, err := s.CreateIdentity(ctx, "x", role, EmptyEventID); !errors.Is(err, ErrInvalidRole) {
			t.Fatalf("role %q: err = %v, want ErrInvalidRole", role, err)
		}
	}
}

// Every path that creates an identity reports it, once.
func TestIdentityCreatedFromEveryPath(t *testing.T) {
	ctx := context.Background()

	t.Run("a stranger, only when it actually created one", func(t *testing.T) {
		s, rec := newRecorded(t)
		g := pending(t, s, "1", "2")
		pending(t, s, "1")
		ev := rec.of(event.IdentityCreated)
		if len(ev) != 1 || ev[0]["identity"] != string(g.ID) || ev[0]["status"] != "pending" || ev[0]["by"] != nil {
			t.Fatalf("identity.created = %v", ev)
		}
		refs, _ := ev[0]["bindings"].([]any)
		if !slices.Equal(refs, []any{"tg:1", "tg:2"}) {
			t.Fatalf("bindings = %v", refs)
		}
		if b := rec.of(event.IdentityBound); len(b) != 0 {
			t.Fatalf("a pending's creation bindings were reported again: %v", b)
		}
	})

	t.Run("the owner", func(t *testing.T) {
		s, rec := newRecorded(t)
		o, err := initOwner(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		ev := rec.of(event.IdentityCreated)
		if len(ev) != 1 || ev[0]["identity"] != string(o.ID) || ev[0]["status"] != "confirmed" {
			t.Fatalf("identity.created = %v", ev)
		}
		if _, err := s.CreateOwner(ctx, true, nil); !errors.Is(err, ErrInitialised) {
			t.Fatalf("second owner: %v", err)
		}
		if ev := rec.of(event.IdentityCreated); len(ev) != 1 {
			t.Fatalf("a refused bootstrap emitted: %v", ev)
		}
	})

	t.Run("never an invite", func(t *testing.T) {
		s, rec := newRecorded(t)
		i := confirmed(t, s, "Carla", store.RoleUser)
		code, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: i.ID, CreatedBy: "admin1"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RedeemInvite(ctx, code, "tg", []string{"5"}, "Five"); err != nil {
			t.Fatal(err)
		}
		if ev := rec.of(event.IdentityCreated); len(ev) != 1 || ev[0]["identity"] != string(i.ID) {
			t.Fatalf("identity.created = %v, want only Carla's own", ev)
		}
	})

	t.Run("an invite onto an identity binds only what is new", func(t *testing.T) {
		s, rec := newRecorded(t)
		i := confirmed(t, s, "Carla", store.RoleUser)
		pending(t, s, "held")
		code, inv, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: i.ID, CreatedBy: "admin1"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RedeemInvite(ctx, code, "tg", []string{"held", "new"}, "Carla"); err != nil {
			t.Fatal(err)
		}
		if ev := rec.of(event.IdentityAssigned); len(ev) != 1 || ev[0]["invite"] != string(inv.ID) || ev[0]["by"] != nil {
			t.Fatalf("identity.assigned = %v", ev)
		}
		ev := rec.of(event.IdentityBound)
		if len(ev) != 1 || ev[0]["identity"] != string(i.ID) || ev[0]["binding"] != "tg:new" || ev[0]["invite"] != string(inv.ID) {
			t.Fatalf("identity.bound = %v", ev)
		}
	})
}

func TestDisableEnable(t *testing.T) {
	ctx := context.Background()

	t.Run("switches and reports, repeating is silent", func(t *testing.T) {
		s, rec := newRecorded(t)
		i := confirmed(t, s, "Carla", store.RoleUser)
		var hook int
		s.AfterEnable = func(context.Context, event.ID) { hook++ }

		got, err := s.Disable(ctx, i.ID, "admin1")
		if err != nil || !got.Disabled() {
			t.Fatalf("disable = %+v, %v", got, err)
		}
		if _, err := s.Disable(ctx, i.ID, "admin1"); err != nil {
			t.Fatal(err)
		}
		ev := rec.of(event.IdentityDisabled)
		if len(ev) != 1 || ev[0]["identity"] != string(i.ID) || ev[0]["by"] != "admin1" {
			t.Fatalf("identity.disabled = %v", ev)
		}
		if stored, _ := s.st.GetIdentity(ctx, i.ID); !stored.Disabled() || stored.Role != store.RoleUser {
			t.Fatalf("stored = %+v, want disabled keeping its role", stored)
		}

		got, err = s.Enable(ctx, i.ID, "admin2")
		if err != nil || got.Status != store.StatusConfirmed {
			t.Fatalf("enable = %+v, %v", got, err)
		}
		if _, err := s.Enable(ctx, i.ID, "admin2"); err != nil {
			t.Fatal(err)
		}
		ev = rec.of(event.IdentityEnabled)
		if len(ev) != 1 || ev[0]["by"] != "admin2" || hook != 1 {
			t.Fatalf("identity.enabled = %v, hook ran %d times", ev, hook)
		}
	})

	t.Run("the owner, a pending identity and an unknown id are refused", func(t *testing.T) {
		s, rec := newRecorded(t)
		owner, err := initOwner(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		g := pending(t, s, "1")
		for _, fn := range []func(context.Context, event.ID, event.ID) (store.Identity, error){s.Disable, s.Enable} {
			if _, err := fn(ctx, owner.ID, "x"); !errors.Is(err, ErrOwnerProtected) {
				t.Fatalf("owner: err = %v", err)
			}
			if _, err := fn(ctx, g.ID, "x"); !errors.Is(err, ErrPending) {
				t.Fatalf("pending: err = %v", err)
			}
			if _, err := fn(ctx, event.NewID(), "x"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("unknown: err = %v", err)
			}
		}
		if ev := rec.of(event.IdentityDisabled); len(ev) != 0 {
			t.Fatalf("events = %v", ev)
		}
	})

	t.Run("re-arming runs before the flip, the wake after, neither on a no-op", func(t *testing.T) {
		s, _ := newRecorded(t)
		i := confirmed(t, s, "Carla", store.RoleUser)
		var seen []string
		s.BeforeEnable = func(ctx context.Context, id event.ID) error {
			got, _ := s.st.GetIdentity(ctx, id)
			seen = append(seen, "before:"+string(got.Status))
			return nil
		}
		s.AfterEnable = func(ctx context.Context, id event.ID) {
			got, _ := s.st.GetIdentity(ctx, id)
			seen = append(seen, "after:"+string(got.Status))
		}
		if _, err := s.Enable(ctx, i.ID, "a"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Disable(ctx, i.ID, "a"); err != nil {
			t.Fatal(err)
		}
		if len(seen) != 0 {
			t.Fatalf("hooks ran on a no-op enable or a disable: %v", seen)
		}
		if _, err := s.Enable(ctx, i.ID, "a"); err != nil {
			t.Fatal(err)
		}
		if want := []string{"before:disabled", "after:confirmed"}; !slices.Equal(seen, want) {
			t.Fatalf("hooks saw %v, want %v", seen, want)
		}
	})

	t.Run("a failed re-arm leaves it disabled", func(t *testing.T) {
		s, rec := newRecorded(t)
		i := confirmed(t, s, "Carla", store.RoleUser)
		if _, err := s.Disable(ctx, i.ID, "a"); err != nil {
			t.Fatal(err)
		}
		boom := errors.New("boom")
		s.BeforeEnable = func(context.Context, event.ID) error { return boom }
		woken := false
		s.AfterEnable = func(context.Context, event.ID) { woken = true }
		if _, err := s.Enable(ctx, i.ID, "a"); !errors.Is(err, boom) {
			t.Fatalf("err = %v", err)
		}
		if stored, _ := s.st.GetIdentity(ctx, i.ID); !stored.Disabled() {
			t.Fatalf("stored = %+v", stored)
		}
		if ev := rec.of(event.IdentityEnabled); len(ev) != 0 || woken {
			t.Fatalf("events = %v, woken = %v", ev, woken)
		}
	})
}

// Unbind, revoke and delete still work on a disabled identity; confirm doesn't.
func TestDisabledKeepsItsManagement(t *testing.T) {
	ctx := context.Background()
	s, _ := newRecorded(t)
	i := confirmed(t, s, "Carla", store.RoleUser, "1", "2")
	_, tok, err := s.CreateToken(ctx, "t", ScopeClient, i.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Disable(ctx, i.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Confirm(ctx, i.ID, "X", "", "a"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("confirm: %v", err)
	}
	if _, err := s.Unbind(ctx, "tg", "1", asAdmin); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeToken(ctx, ownerP, tok.ID); err != nil {
		t.Fatal(err)
	}
	d, err := s.DeleteIdentity(ctx, i.ID, false, "a")
	// tg:2 and the token's holaclaw binding.
	if err != nil || len(d.Aliases) != 2 {
		t.Fatalf("delete = %+v, %v", d, err)
	}
}

// A disabled identity's token is refused, not revoked, and good again after.
func TestDisabledToken(t *testing.T) {
	ctx := context.Background()
	s, _ := newRecorded(t)
	i := confirmed(t, s, "Carla", store.RoleUser)
	secret, tok, err := s.CreateToken(ctx, "t", ScopeClient, i.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Disable(ctx, i.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, secret); !errors.Is(err, ErrIdentityDisabled) {
		t.Fatalf("err = %v, want ErrIdentityDisabled", err)
	}
	stored, err := getTokenIn(ctx, s.db, tok.ID)
	if err != nil || stored.Revoked() || stored.LastUsedAt != nil {
		t.Fatalf("token after refusal = %+v, %v; want untouched", stored, err)
	}
	if _, err := s.Enable(ctx, i.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Authenticate(ctx, secret); err != nil || p.UserID != i.ID {
		t.Fatalf("after enable = %+v, %v", p, err)
	}
}

func TestDisabledInvites(t *testing.T) {
	ctx := context.Background()
	s, _ := newRecorded(t)
	i := confirmed(t, s, "Carla", store.RoleUser)
	code, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: i.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Disable(ctx, i.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: i.ID}); !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("create: err = %v, want an invite spec error", err)
	}
	if _, err := s.RedeemInvite(ctx, code, "tg", []string{"9"}, "x"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("redeem: err = %v, want ErrInviteInvalid", err)
	}
	if _, err := s.Resolve(ctx, "tg", []string{"9"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused redeem linked the alias: %v", err)
	}
	if _, err := s.Enable(ctx, i.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemInvite(ctx, code, "tg", []string{"9"}, "x"); err != nil {
		t.Fatalf("redeem after enable: %v", err)
	}
}

// The invites a disabled admin created are on hold with it: refused, unspent,
// and good again once it is enabled.
func TestDisabledCreatorsInvites(t *testing.T) {
	ctx := context.Background()
	s, _ := newRecorded(t)
	admin := confirmed(t, s, "Ada", store.RoleAdmin)
	target := confirmed(t, s, "Carla", store.RoleUser)
	forTarget, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: target.ID, CreatedBy: admin.ID})
	if err != nil {
		t.Fatal(err)
	}
	forRoom, _, err := s.CreateInvite(ctx, InviteSpec{Kind: InviteRoom, Channel: "tg", CreatedBy: admin.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Disable(ctx, admin.ID, "a"); err != nil {
		t.Fatal(err)
	}
	var a approved
	if _, err := s.RedeemInvite(ctx, forTarget, "tg", []string{"9"}, "x"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("dm: err = %v, want ErrInviteInvalid", err)
	}
	if _, err := s.RedeemRoomInvite(ctx, forRoom, "tg", "g1", []string{"9"}, a.fn); !errors.Is(err, ErrInviteInvalid) || a.calls != 0 {
		t.Fatalf("room: err = %v, approved %d times", err, a.calls)
	}
	if _, err := s.Resolve(ctx, "tg", []string{"9"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a refused redeem linked the alias: %v", err)
	}

	if _, err := s.Enable(ctx, admin.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemInvite(ctx, forTarget, "tg", []string{"9"}, "x"); err != nil {
		t.Fatalf("redeem after enable: %v", err)
	}
	if _, err := s.RedeemRoomInvite(ctx, forRoom, "tg", "g1", []string{"9"}, a.fn); err != nil || a.by != admin.ID {
		t.Fatalf("room after enable: err = %v, by %s", err, a.by)
	}
}

// No token is minted for a disabled identity, by either path.
func TestDisabledGetsNoToken(t *testing.T) {
	ctx := context.Background()
	s, _ := newRecorded(t)
	i := confirmed(t, s, "Carla", store.RoleUser)
	if _, err := s.Disable(ctx, i.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateToken(ctx, "t", ScopeClient, i.ID); !errors.Is(err, ErrDisabled) {
		t.Fatalf("create: err = %v, want ErrDisabled", err)
	}
	if _, _, err := s.MintToken(ctx, Principal{Role: store.RoleOwner}, "t", ScopeClient, i.ID); !errors.Is(err, ErrDisabled) {
		t.Fatalf("mint: err = %v, want ErrDisabled", err)
	}
	if list, err := s.ListTokens(ctx, TokenFilter{All: true}); err != nil || len(list) != 0 {
		t.Fatalf("tokens = %v, %v", list, err)
	}
}

func TestAssignRules(t *testing.T) {
	ctx := context.Background()
	s, _ := newRecorded(t)
	source := confirmed(t, s, "Ana", store.RoleUser)
	target := confirmed(t, s, "Bo", store.RoleUser)
	off := confirmed(t, s, "Off", store.RoleUser)
	if _, err := s.Disable(ctx, off.ID, "a"); err != nil {
		t.Fatal(err)
	}
	g, g2 := pending(t, s, "1"), pending(t, s, "2")
	for _, c := range []struct {
		name     string
		from, to event.ID
		want     error
	}{
		{"from a confirmed source", source.ID, target.ID, ErrNotPending},
		{"from a disabled source", off.ID, target.ID, ErrNotPending},
		{"onto a pending target", g.ID, g2.ID, ErrPending},
		{"onto a disabled target", g.ID, off.ID, ErrDisabled},
	} {
		if _, err := s.Assign(ctx, c.from, c.to, asAdmin); !errors.Is(err, c.want) {
			t.Fatalf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	if got, err := s.Resolve(ctx, "tg", []string{"1"}); err != nil || got.ID != g.ID {
		t.Fatalf("binding moved: %+v, %v", got, err)
	}
}

func TestAssignEventKeys(t *testing.T) {
	s, rec := newRecorded(t)
	target := confirmed(t, s, "Bo", store.RoleUser)
	g := pending(t, s, "1", "2")
	if _, err := s.Assign(context.Background(), g.ID, target.ID, asAdmin); err != nil {
		t.Fatal(err)
	}
	ev := rec.of(event.IdentityAssigned)
	if len(ev) != 1 || ev[0]["source"] != string(g.ID) || ev[0]["target"] != string(target.ID) ||
		ev[0]["bindings"] != float64(2) || ev[0]["by"] != "admin1" || ev[0]["ids"] != nil {
		t.Fatalf("identity.assigned = %v", ev)
	}
}

func TestUnbind(t *testing.T) {
	ctx := context.Background()

	t.Run("the last binding deletes a pending identity with its memory", func(t *testing.T) {
		s, rec := newRecorded(t)
		g := pending(t, s, "1", "2")
		partition := "dm/" + string(g.ID)
		note := putMemory(t, s, "main", "note", partition, string(g.ID), "")
		recall := putMemory(t, s, "main", "recall", partition, string(g.ID), "h1")
		shared := sharedNote(t, s, partition)
		sess := putSession(t, s, "main/tg/dm/"+string(g.ID), g.ID, event.ConversationDM)

		u, err := s.Unbind(ctx, "tg", "1", asAdmin)
		if err != nil || u.PendingDeleted || !exists(t, s, g.ID) {
			t.Fatalf("first unbind = %+v, %v; want the pending kept", u, err)
		}
		if p, _ := memoryOf(t, s, note); p == "" {
			t.Fatal("memory went with a binding that wasn't the last")
		}

		u, err = s.Unbind(ctx, "tg", "2", asAdmin)
		if err != nil || !u.PendingDeleted || u.IdentityID != g.ID || u.Memory != 2 {
			t.Fatalf("last unbind = %+v, %v", u, err)
		}
		if exists(t, s, g.ID) {
			t.Fatal("pending identity survived")
		}
		for _, id := range []string{note, recall} {
			if p, _ := memoryOf(t, s, id); p != "" {
				t.Fatalf("memory %s survived", id)
			}
		}
		if p, _ := memoryOf(t, s, shared); p == "" {
			t.Fatal("the shared note went too")
		}
		var fts int
		if err := s.db.QueryRow(`SELECT count(*) FROM memory_fts WHERE memory_fts MATCH 'text'`).Scan(&fts); err != nil || fts != 0 {
			t.Fatalf("FTS rows left = %d, %v", fts, err)
		}
		if got, err := s.st.GetSession(ctx, sess.ID); err != nil || got.UserID != "" {
			t.Fatalf("session = %+v, %v; want kept with no identity", got, err)
		}
		unbound := rec.of(event.IdentityUnbound)
		if len(unbound) != 2 || unbound[1]["binding"] != "tg:2" || unbound[1]["by"] != "admin1" {
			t.Fatalf("identity.unbound = %v", unbound)
		}
		del := rec.of(event.IdentityDeleted)
		if len(del) != 1 || del[0]["identity"] != string(g.ID) || del[0]["reason"] != "pending_unbound" ||
			del[0]["by"] != "admin1" || del[0]["memory_forgotten"] != float64(2) || del[0]["role"] != "guest" {
			t.Fatalf("identity.deleted = %v", del)
		}
	})

	t.Run("the last binding cancels the pending identity's tasks and revokes their grants", func(t *testing.T) {
		s, _ := newRecorded(t)
		g := pending(t, s, "1")
		if _, err := s.db.Exec(`INSERT INTO tasks (id, agent, prompt, schedule_kind, delivery_kind, status, next_run, created_by, created_at)
			VALUES ('t1', 'main', 'p', 'now', 'store', 'active', 1, ?, 1)`, string(g.ID)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO grants (id, agent, tool, scope, task_id, created_at) VALUES ('g1', 'main', 'x', 'task', 't1', 1)`); err != nil {
			t.Fatal(err)
		}
		if u, err := s.Unbind(ctx, "tg", "1", asAdmin); err != nil || !u.PendingDeleted {
			t.Fatalf("unbind = %+v, %v", u, err)
		}
		var status string
		var next sql.NullInt64
		if err := s.db.QueryRow(`SELECT status, next_run FROM tasks WHERE id = 't1'`).Scan(&status, &next); err != nil || status != "cancelled" || next.Valid {
			t.Fatalf("task = %s, %v, %v", status, next, err)
		}
		var grants int
		if err := s.db.QueryRow(`SELECT count(*) FROM grants`).Scan(&grants); err != nil || grants != 0 {
			t.Fatalf("grants = %d, %v", grants, err)
		}
	})

	t.Run("the memory's vectors go with it", func(t *testing.T) {
		s, _ := newRecorded(t)
		g := pending(t, s, "1")
		note := putMemory(t, s, "main", "note", "dm/"+string(g.ID), string(g.ID), "")
		if _, err := s.db.Exec(`INSERT INTO memory_vectors (item_id, model, dims, vec) VALUES (?, 'm', 1, x'00000000')`, note); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Unbind(ctx, "tg", "1", asAdmin); err != nil {
			t.Fatal(err)
		}
		var vectors int
		if err := s.db.QueryRow(`SELECT count(*) FROM memory_vectors`).Scan(&vectors); err != nil || vectors != 0 {
			t.Fatalf("vectors = %d, %v", vectors, err)
		}
	})

	t.Run("a failed delete keeps the binding, the memory and the identity", func(t *testing.T) {
		s, rec := newRecorded(t)
		g := pending(t, s, "1")
		note := putMemory(t, s, "main", "note", "dm/"+string(g.ID), string(g.ID), "")
		// Fails the transaction after the binding and the memory are deleted.
		if _, err := s.db.Exec(`CREATE TRIGGER fail_delete BEFORE DELETE ON identities BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Unbind(ctx, "tg", "1", asAdmin); err == nil {
			t.Fatal("unbind succeeded")
		}
		if !exists(t, s, g.ID) {
			t.Fatal("identity went")
		}
		if got, err := s.Resolve(ctx, "tg", []string{"1"}); err != nil || got.ID != g.ID {
			t.Fatalf("binding gone: %+v, %v", got, err)
		}
		if p, _ := memoryOf(t, s, note); p == "" {
			t.Fatal("memory went")
		}
		if ev := rec.of(event.IdentityUnbound); len(ev) != 0 {
			t.Fatalf("identity.unbound = %v", ev)
		}
	})

	t.Run("a confirmed identity is never deleted", func(t *testing.T) {
		s, rec := newRecorded(t)
		i := confirmed(t, s, "Carla", store.RoleUser, "1")
		note := putMemory(t, s, "main", "note", "dm/"+string(i.ID), string(i.ID), "")
		u, err := s.Unbind(ctx, "tg", "1", asAdmin)
		if err != nil || u.PendingDeleted || !exists(t, s, i.ID) {
			t.Fatalf("unbind = %+v, %v", u, err)
		}
		if p, _ := memoryOf(t, s, note); p == "" {
			t.Fatal("memory went")
		}
		if ev := rec.of(event.IdentityDeleted); len(ev) != 0 {
			t.Fatalf("identity.deleted = %v", ev)
		}
	})

	t.Run("only the owner unbinds the owner", func(t *testing.T) {
		s, rec := newRecorded(t)
		owner, err := initOwner(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		storetest.Bind(t, s.st, "tg", []string{"1"}, owner.ID, "")
		if _, err := s.Unbind(ctx, "tg", "1", asAdmin); !errors.Is(err, ErrOwnerOnly) {
			t.Fatalf("err = %v, want ErrOwnerOnly", err)
		}
		if got, err := s.Resolve(ctx, "tg", []string{"1"}); err != nil || got.ID != owner.ID {
			t.Fatalf("binding gone: %+v, %v", got, err)
		}
		if ev := rec.of(event.IdentityUnbound); len(ev) != 0 {
			t.Fatalf("identity.unbound = %v", ev)
		}
		sock, err := s.SocketPrincipal(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Unbind(ctx, "tg", "1", sock); err != nil {
			t.Fatal(err)
		}
		if ev := rec.of(event.IdentityUnbound); len(ev) != 1 || ev[0]["by"] != string(owner.ID) {
			t.Fatalf("identity.unbound = %v, want by the owner the socket acts as", ev)
		}
	})
}

// ctxSink records whether each event's context was already cancelled.
type ctxSink struct{ cancelled []bool }

func (c *ctxSink) Emit(ctx context.Context, _ event.Event) {
	c.cancelled = append(c.cancelled, ctx.Err() != nil)
}

// An event is reported after the commit, even when the caller has gone away.
func TestEmitIgnoresCancellation(t *testing.T) {
	s, _ := newService(t)
	sink := &ctxSink{}
	s.Events = sink
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.emit(ctx, event.IdentityUnbound, map[string]any{})
	if !slices.Equal(sink.cancelled, []bool{false}) {
		t.Fatalf("cancelled = %v, want the event emitted with a live context", sink.cancelled)
	}
}

// Unbinding a holaclaw binding revokes every token on it and leaves the
// identity's other bindings and their tokens alone. holaclaw:cli stays.
func TestUnbindHolaclawRevokesItsTokens(t *testing.T) {
	ctx := context.Background()
	s, rec := newRecorded(t)
	owner, err := initOwner(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	i := confirmed(t, s, "Carla", store.RoleUser)
	ci, tok, err := s.CreateToken(ctx, "automations", ScopeClient, i.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A second token on the same binding, as `token create --binding` would add.
	backup, second, err := s.CreateToken(ctx, "backup", ScopeClient, i.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE api_tokens SET binding_id = ? WHERE id = ?`, tok.Binding, string(second.ID)); err != nil {
		t.Fatal(err)
	}
	phone, _, err := s.CreateToken(ctx, "iPhone", ScopeClient, i.ID)
	if err != nil {
		t.Fatal(err)
	}

	u, err := s.Unbind(ctx, event.ChannelHolaclaw, tok.Binding, asAdmin)
	if err != nil || u.Tokens != 2 || u.PendingDeleted || u.IdentityID != i.ID {
		t.Fatalf("unbind = %+v, %v", u, err)
	}
	for _, secret := range []string{ci, backup} {
		if _, err := s.Authenticate(ctx, secret); !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("token of the unbound binding: err = %v, want ErrTokenInvalid", err)
		}
	}
	// Revoked, not deleted: kept for traceability.
	if kept := mustToken(t, s, tok.ID); !kept.Revoked() || kept.Binding != tok.Binding || kept.IdentityID != "" {
		t.Fatalf("token after unbind = %+v", kept)
	}
	if p, err := s.Authenticate(ctx, phone); err != nil || p.UserID != i.ID {
		t.Fatalf("other binding's token: %+v, %v", p, err)
	}
	if ev := rec.of(event.IdentityUnbound); len(ev) != 1 || ev[0]["tokens_revoked"] != float64(2) ||
		ev[0]["binding"] != "holaclaw:"+tok.Binding {
		t.Fatalf("identity.unbound = %v", ev)
	}

	sock, err := s.SocketPrincipal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Unbind(ctx, event.ChannelHolaclaw, SocketAlias, sock); !errors.Is(err, ErrSocketBinding) {
		t.Fatalf("unbind holaclaw:cli: err = %v, want ErrSocketBinding", err)
	}
	if p, err := s.SocketPrincipal(ctx); err != nil || p.UserID != owner.ID {
		t.Fatalf("socket after a refused unbind = %+v, %v", p, err)
	}
}

// Deleting an identity revokes its tokens and keeps them.
func TestDeleteRevokesTokens(t *testing.T) {
	ctx := context.Background()
	s, _ := newRecorded(t)
	i := confirmed(t, s, "Carla", store.RoleUser)
	secret, tok, err := s.CreateToken(ctx, "ci", ScopeClient, i.ID)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.DeleteIdentity(ctx, i.ID, false, "a")
	if err != nil || d.TokensRevoked != 1 {
		t.Fatalf("delete = %+v, %v", d, err)
	}
	if _, err := s.Authenticate(ctx, secret); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("err = %v, want ErrTokenInvalid", err)
	}
	if kept := mustToken(t, s, tok.ID); !kept.Revoked() {
		t.Fatalf("token after delete = %+v, want revoked and kept", kept)
	}
}
