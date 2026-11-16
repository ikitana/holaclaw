package auth

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/store/storetest"
)

// recordSink collects every event emitted to it, safe for concurrent use.
type recordSink struct {
	mu   sync.Mutex
	evts []event.Event
}

func (r *recordSink) Emit(_ context.Context, e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evts = append(r.evts, e)
}

func (r *recordSink) last() event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.evts) == 0 {
		return event.Event{}
	}
	return r.evts[len(r.evts)-1]
}

func TestConfirm(t *testing.T) {
	ctx := context.Background()

	t.Run("names and roles a guest, keeping its id", func(t *testing.T) {
		s, _ := newService(t)
		rec := &recordSink{}
		s.Events = rec
		g, err := s.EnsureGuest(ctx, "cli", []string{"alice"}, "")
		if err != nil {
			t.Fatal(err)
		}

		i, err := s.Confirm(ctx, g.ID, "Alice", "", "admin1")
		if err != nil {
			t.Fatal(err)
		}
		if i.ID != g.ID || i.Name != "Alice" || i.Role != store.RoleUser {
			t.Fatalf("confirmed = %+v", i)
		}
		ids, err := s.Bindings(ctx, g.ID)
		if err != nil || len(ids) != 1 || ids[0].Alias != "alice" {
			t.Fatalf("identities = %v, %v, want alice kept", ids, err)
		}

		e := rec.last()
		if e.Type != event.IdentityConfirmed {
			t.Fatalf("event type = %v, want IdentityConfirmed", e.Type)
		}
		var attrs struct {
			UserID string `json:"identity"`
			Name   string `json:"name"`
			Role   string `json:"role"`
			By     string `json:"by"`
		}
		if err := json.Unmarshal(e.Attrs, &attrs); err != nil {
			t.Fatal(err)
		}
		if attrs.UserID != string(g.ID) || attrs.Name != "Alice" || attrs.Role != "user" || attrs.By != "admin1" {
			t.Fatalf("attrs = %+v", attrs)
		}
	})

	t.Run("role admin is granted", func(t *testing.T) {
		s, _ := newService(t)
		g, err := s.EnsureGuest(ctx, "cli", []string{"bob"}, "")
		if err != nil {
			t.Fatal(err)
		}
		i, err := s.Confirm(ctx, g.ID, "Bob", store.RoleAdmin, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		if i.Role != store.RoleAdmin {
			t.Fatalf("role = %v, want admin", i.Role)
		}
	})

	t.Run("confirming twice fails", func(t *testing.T) {
		s, _ := newService(t)
		g, err := s.EnsureGuest(ctx, "cli", []string{"carl"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Confirm(ctx, g.ID, "Carl", "", EmptyEventID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Confirm(ctx, g.ID, "Carl", "", EmptyEventID); !errors.Is(err, ErrNotPending) {
			t.Fatalf("second confirm = %v, want ErrNotPending", err)
		}
	})

	t.Run("confirming a non-guest fails", func(t *testing.T) {
		s, _ := newService(t)
		i, err := s.CreateIdentity(ctx, "dana", store.RoleUser, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Confirm(ctx, i.ID, "Dana", "", EmptyEventID); !errors.Is(err, ErrNotPending) {
			t.Fatalf("confirm non-guest = %v, want ErrNotPending", err)
		}
	})

	t.Run("owner or guest role is rejected", func(t *testing.T) {
		s, _ := newService(t)
		for _, role := range []store.Role{store.RoleOwner, store.RoleGuest} {
			g, err := s.EnsureGuest(ctx, "cli", []string{"x-" + string(role)}, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Confirm(ctx, g.ID, "X", role, EmptyEventID); !errors.Is(err, ErrInvalidRole) {
				t.Fatalf("confirm role %q = %v, want ErrInvalidRole", role, err)
			}
		}
	})

	t.Run("empty name is rejected", func(t *testing.T) {
		s, _ := newService(t)
		g, err := s.EnsureGuest(ctx, "cli", []string{"erin"}, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"", "  \t"} {
			if _, err := s.Confirm(ctx, g.ID, name, "", EmptyEventID); !errors.Is(err, ErrInvalidIdentity) {
				t.Fatalf("confirm name %q = %v, want ErrInvalidIdentity", name, err)
			}
		}
	})

	t.Run("unknown id", func(t *testing.T) {
		s, _ := newService(t)
		if _, err := s.Confirm(ctx, event.NewID(), "X", "", EmptyEventID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("confirm unknown = %v, want store.ErrNotFound", err)
		}
	})
}

func TestUpdate(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown id", func(t *testing.T) {
		s, _ := newService(t)
		name := "x"
		if _, err := s.Update(ctx, event.NewID(), &name, nil, EmptyEventID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("update unknown = %v, want store.ErrNotFound", err)
		}
	})

	t.Run("owner role cannot change", func(t *testing.T) {
		s, _ := newService(t)
		owner, err := initOwner(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		role := store.RoleAdmin
		if _, err := s.Update(ctx, owner.ID, nil, &role, EmptyEventID); !errors.Is(err, ErrOwnerRole) {
			t.Fatalf("update owner role = %v, want ErrOwnerRole", err)
		}
	})

	t.Run("owner name can change", func(t *testing.T) {
		s, _ := newService(t)
		owner, err := initOwner(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		name := "renamed-owner"
		i, err := s.Update(ctx, owner.ID, &name, nil, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		if i.Name != name || i.Role != store.RoleOwner {
			t.Fatalf("updated owner = %+v", i)
		}
	})

	t.Run("role on a guest is pending", func(t *testing.T) {
		s, _ := newService(t)
		g, err := s.EnsureGuest(ctx, "cli", []string{"frank"}, "")
		if err != nil {
			t.Fatal(err)
		}
		role := store.RoleAdmin
		if _, err := s.Update(ctx, g.ID, nil, &role, EmptyEventID); !errors.Is(err, ErrPending) {
			t.Fatalf("update guest role = %v, want ErrPending", err)
		}
	})

	t.Run("name on a guest is allowed", func(t *testing.T) {
		s, _ := newService(t)
		g, err := s.EnsureGuest(ctx, "cli", []string{"gale"}, "")
		if err != nil {
			t.Fatal(err)
		}
		name := "Gale"
		i, err := s.Update(ctx, g.ID, &name, nil, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		if i.Name != name || !i.Pending() {
			t.Fatalf("updated guest = %+v", i)
		}
	})

	t.Run("owner role value is rejected outright", func(t *testing.T) {
		s, _ := newService(t)
		i, err := s.CreateIdentity(ctx, "hank", store.RoleUser, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		role := store.RoleOwner
		if _, err := s.Update(ctx, i.ID, nil, &role, EmptyEventID); !errors.Is(err, ErrInvalidRole) {
			t.Fatalf("update to owner role = %v, want ErrInvalidRole", err)
		}
	})

	t.Run("nothing to change", func(t *testing.T) {
		s, _ := newService(t)
		i, err := s.CreateIdentity(ctx, "ivy", store.RoleUser, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		empty := ""
		if _, err := s.Update(ctx, i.ID, &empty, nil, EmptyEventID); !errors.Is(err, ErrInvalidIdentity) {
			t.Fatalf("update nothing = %v, want ErrInvalidIdentity", err)
		}
		if _, err := s.Update(ctx, i.ID, nil, nil, EmptyEventID); !errors.Is(err, ErrInvalidIdentity) {
			t.Fatalf("update nil/nil = %v, want ErrInvalidIdentity", err)
		}
		admin := store.RoleAdmin
		if _, err := s.Update(ctx, i.ID, &empty, &admin, EmptyEventID); !errors.Is(err, ErrInvalidIdentity) {
			t.Fatalf("update empty name with role = %v, want ErrInvalidIdentity", err)
		}
		blank := "   "
		if _, err := s.Update(ctx, i.ID, &blank, nil, EmptyEventID); !errors.Is(err, ErrInvalidIdentity) {
			t.Fatalf("update blank name = %v, want ErrInvalidIdentity", err)
		}
	})

	t.Run("user to admin", func(t *testing.T) {
		s, _ := newService(t)
		i, err := s.CreateIdentity(ctx, "jan", store.RoleUser, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		role := store.RoleAdmin
		got, err := s.Update(ctx, i.ID, nil, &role, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Role != store.RoleAdmin || got.Name != "jan" {
			t.Fatalf("updated = %+v", got)
		}
	})
}

func TestCreateIdentityEmptyName(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	for _, name := range []string{"", " \n"} {
		if _, err := s.CreateIdentity(ctx, name, store.RoleUser, EmptyEventID); !errors.Is(err, ErrInvalidIdentity) {
			t.Fatalf("create name %q = %v, want ErrInvalidIdentity", name, err)
		}
	}
}

func TestListUsersPending(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	owner, err := initOwner(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateIdentity(ctx, "kim", store.RoleUser, EmptyEventID); err != nil {
		t.Fatal(err)
	}
	g1, err := s.EnsureGuest(ctx, "cli", []string{"g1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	g2, err := s.EnsureGuest(ctx, "cli", []string{"g2"}, "")
	if err != nil {
		t.Fatal(err)
	}

	all, err := s.ListIdentities(ctx, false)
	if err != nil || len(all) != 4 {
		t.Fatalf("ListIdentities(false) = %v, %v, want 4 users", all, err)
	}

	pending, err := s.ListIdentities(ctx, true)
	if err != nil || len(pending) != 2 {
		t.Fatalf("ListIdentities(true) = %v, %v, want 2 guests", pending, err)
	}
	for _, i := range pending {
		if !i.Pending() {
			t.Fatalf("pending identity %+v is not a guest", i)
		}
		if i.ID != g1.ID && i.ID != g2.ID {
			t.Fatalf("unexpected pending identity %+v", i)
		}
	}
	_ = owner
}

func TestRemoveAlias(t *testing.T) {
	ctx := context.Background()

	t.Run("unlinks the alias and leaves memory alone", func(t *testing.T) {
		s, _ := newService(t)
		rec := &recordSink{}
		s.Events = rec
		i, err := s.CreateIdentity(ctx, "Rosa", store.RoleUser, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		storetest.Bind(t, s.st, "telegram", []string{"111", "222"}, i.ID, "Rosa")
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO memory_items (id, agent, layer, text, visibility, "partition", user_id, created_at)
			 VALUES ('m1', 'main', 'note', 'likes tea', 'shared', 'dm/rosa', ?, 1)`, string(i.ID)); err != nil {
			t.Fatal(err)
		}

		u, err := s.Unbind(ctx, "telegram", "111", Principal{UserID: "admin1", Role: store.RoleAdmin})
		if err != nil {
			t.Fatal(err)
		}
		if u.IdentityID != i.ID || u.PendingDeleted {
			t.Fatalf("unbound = %+v", u)
		}
		if _, err := s.Resolve(ctx, "telegram", []string{"111"}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("resolve removed alias = %v, want ErrNotFound", err)
		}
		if got, err := s.Resolve(ctx, "telegram", []string{"222"}); err != nil || got.ID != i.ID {
			t.Fatalf("resolve kept alias = %+v, %v", got, err)
		}
		var owner string
		if err := s.db.QueryRowContext(ctx, `SELECT user_id FROM memory_items WHERE id = 'm1'`).Scan(&owner); err != nil || owner != string(i.ID) {
			t.Fatalf("memory owner = %q, %v, want %s", owner, err, i.ID)
		}

		e := rec.last()
		if e.Type != event.IdentityUnbound {
			t.Fatalf("event = %s, want %s", e.Type, event.IdentityUnbound)
		}
		var p map[string]any
		if err := json.Unmarshal(e.Attrs, &p); err != nil {
			t.Fatal(err)
		}
		if p["identity"] != string(i.ID) || p["binding"] != "telegram:111" || p["tokens_revoked"] != float64(0) || p["by"] != "admin1" {
			t.Fatalf("payload = %v", p)
		}
	})

	t.Run("the next message makes a new guest", func(t *testing.T) {
		s, _ := newService(t)
		i, err := s.CreateIdentity(ctx, "Rosa", store.RoleUser, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		storetest.Bind(t, s.st, "telegram", []string{"111"}, i.ID, "Rosa")
		if _, err := s.Unbind(ctx, "telegram", "111", Principal{UserID: "admin1", Role: store.RoleAdmin}); err != nil {
			t.Fatal(err)
		}
		g, err := s.EnsureGuest(ctx, "telegram", []string{"111"}, "Rosa")
		if err != nil {
			t.Fatal(err)
		}
		if g.ID == i.ID || !g.Pending() {
			t.Fatalf("after removal = %+v, want a new guest", g)
		}
	})

	t.Run("the owner's channel binding can go and the token still works", func(t *testing.T) {
		s, _ := newService(t)
		boot, err := s.CreateOwner(ctx, true, nil)
		secret := boot.Secret
		if err != nil {
			t.Fatal(err)
		}
		owner, err := s.Owner(ctx)
		if err != nil {
			t.Fatal(err)
		}
		storetest.Bind(t, s.st, "telegram", []string{"1"}, owner.ID, "")
		if _, err := s.Unbind(ctx, "telegram", "1", Principal{UserID: owner.ID, Role: store.RoleOwner}); err != nil {
			t.Fatal(err)
		}
		// What stays is the owner's holaclaw bindings: the socket's and init's.
		if ids, err := s.Bindings(ctx, owner.ID); err != nil || len(ids) != 2 || len(holaclawBindings(t, s, owner.ID)) != 2 {
			t.Fatalf("owner identities = %v, %v, want the two holaclaw ones", ids, err)
		}
		if p, err := s.Authenticate(ctx, secret); err != nil || p.UserID != owner.ID {
			t.Fatalf("owner token = %+v, %v", p, err)
		}
	})

	t.Run("an unknown ref is not found", func(t *testing.T) {
		s, _ := newService(t)
		b, err := s.CreateIdentity(ctx, "b", store.RoleUser, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		rec := &recordSink{}
		s.Events = rec
		storetest.Bind(t, s.st, "telegram", []string{"b1"}, b.ID, "")
		for _, c := range []struct{ channel, alias string }{
			{"telegram", "nobody"}, {"slack", "b1"}, {"", ""},
		} {
			if _, err := s.Unbind(ctx, c.channel, c.alias, Principal{Role: store.RoleAdmin}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("Unbind(%s:%s) = %v, want ErrNotFound", c.channel, c.alias, err)
			}
		}
		if got, err := s.Resolve(ctx, "telegram", []string{"b1"}); err != nil || got.ID != b.ID {
			t.Fatalf("b's alias = %+v, %v, want untouched", got, err)
		}
		if len(rec.evts) != 0 {
			t.Fatalf("events = %v, want none", rec.evts)
		}
	})
}

func TestDeleteUser(t *testing.T) {
	ctx := context.Background()

	t.Run("a pending identity loses its aliases and its memory", func(t *testing.T) {
		s, _ := newService(t)
		rec := &recordSink{}
		s.Events = rec
		var forgot event.ID
		s.ForgetUserMemory = func(_ context.Context, id event.ID) (int64, error) { forgot = id; return 4, nil }
		g, err := s.EnsureGuest(ctx, "tg", []string{"42", "@ana"}, "Ana")
		if err != nil {
			t.Fatal(err)
		}
		d, err := s.DeleteIdentity(ctx, g.ID, false, "admin1")
		if err != nil {
			t.Fatal(err)
		}
		if forgot != g.ID || d.Forgotten != 4 || len(d.Aliases) != 2 || !d.User.Pending() {
			t.Fatalf("deleted = %+v, forgot %q", d, forgot)
		}
		if _, err := s.Resolve(ctx, "tg", []string{"42", "@ana"}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("resolve after delete err = %v, want ErrNotFound", err)
		}
		e := rec.last()
		if e.Type != event.IdentityDeleted {
			t.Fatalf("event type = %v, want IdentityDeleted", e.Type)
		}
		var attrs struct {
			UserID  string   `json:"identity"`
			Role    string   `json:"role"`
			Aliases []string `json:"bindings"`
			Forgot  int64    `json:"memory_forgotten"`
			Kept    bool     `json:"memory_kept"`
			By      string   `json:"by"`
		}
		if err := json.Unmarshal(e.Attrs, &attrs); err != nil {
			t.Fatal(err)
		}
		slices.Sort(attrs.Aliases)
		if attrs.UserID != string(g.ID) || attrs.Role != "guest" || !slices.Equal(attrs.Aliases, []string{"tg:42", "tg:@ana"}) ||
			attrs.Forgot != 4 || attrs.Kept || attrs.By != "admin1" {
			t.Fatalf("attrs = %+v", attrs)
		}
	})

	t.Run("keep memory skips the forget", func(t *testing.T) {
		s, _ := newService(t)
		rec := &recordSink{}
		s.Events = rec
		s.ForgetUserMemory = func(context.Context, event.ID) (int64, error) {
			t.Fatal("memory must be kept")
			return 0, nil
		}
		i, err := s.CreateIdentity(ctx, "Bob", store.RoleUser, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		d, err := s.DeleteIdentity(ctx, i.ID, true, "admin1")
		if err != nil || d.Forgotten != 0 {
			t.Fatalf("deleted = %+v, %v", d, err)
		}
		if _, err := s.st.GetIdentity(ctx, i.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("user still there: %v", err)
		}
		if !strings.Contains(string(rec.last().Attrs), `"memory_kept":true`) {
			t.Fatalf("attrs = %s", rec.last().Attrs)
		}
	})

	t.Run("the owner and unknown ids are refused before anything runs", func(t *testing.T) {
		s, _ := newService(t)
		s.BeforeDeleteUser = func(context.Context, event.ID) error { t.Fatal("hook ran"); return nil }
		s.ForgetUserMemory = func(context.Context, event.ID) (int64, error) { t.Fatal("forget ran"); return 0, nil }
		owner, err := initOwner(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		rec := &recordSink{}
		s.Events = rec
		if _, err := s.DeleteIdentity(ctx, owner.ID, false, owner.ID); !errors.Is(err, ErrOwner) {
			t.Fatalf("err = %v, want ErrOwner", err)
		}
		if _, err := s.DeleteIdentity(ctx, event.NewID(), false, owner.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		if len(rec.evts) != 0 {
			t.Fatalf("events = %v, want none", rec.evts)
		}
	})
}
