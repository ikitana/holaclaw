package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
	"holaclaw/internal/store/storetest"
)

// putMemory writes one memory row straight into the table, the way the memory
// store would.
func putMemory(t *testing.T, s *Service, agent, layer, partition, userID, hash string) string {
	t.Helper()
	id := string(event.NewID())
	if _, err := s.db.Exec(`INSERT INTO memory_items
		(id, agent, layer, text, visibility, "partition", user_id, role, created_at, source_hash)
		VALUES (?, ?, ?, ?, 'origin', ?, ?, 'guest', 1, ?)`,
		id, agent, layer, "text "+id, partition, userID, hash); err != nil {
		t.Fatal(err)
	}
	return id
}

// memoryOf returns the partition and user of a memory row, or "" when it is
// gone.
func memoryOf(t *testing.T, s *Service, id string) (partition, userID string) {
	t.Helper()
	err := s.db.QueryRow(`SELECT "partition", user_id FROM memory_items WHERE id = ?`, id).Scan(&partition, &userID)
	if err != nil {
		return "", ""
	}
	return partition, userID
}

func putSession(t *testing.T, s *Service, key string, user event.ID, kind event.ConversationKind) store.Session {
	t.Helper()
	sess, _, err := s.st.EnsureSession(context.Background(), store.Session{
		Key: key, Agent: "main", UserID: user,
		Origin: event.Origin{Channel: "tg", Conversation: key, ConversationKind: kind},
	})
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

// guestWithState is a guest on tg with one ID, a DM session, and a note and a
// recall row in its DM partition.
type guestWithState struct {
	identity              store.Identity
	dm                    store.Session
	note, recall, dupe    string
	targetRecall, roomRow string
	adminRow              string // written into the guest's DM by someone else
}

func newGuestWithState(t *testing.T, s *Service, target event.ID) guestWithState {
	t.Helper()
	g, err := s.EnsureGuest(context.Background(), "tg", []string{"77"}, "Stranger")
	if err != nil {
		t.Fatal(err)
	}
	gs := guestWithState{identity: g}
	gs.dm = putSession(t, s, "main/tg/dm/"+string(g.ID), g.ID, event.ConversationDM)
	dm := "dm/" + string(g.ID)
	gs.note = putMemory(t, s, "main", "note", dm, string(g.ID), "")
	gs.recall = putMemory(t, s, "main", "recall", dm, string(g.ID), "h1")
	gs.dupe = putMemory(t, s, "main", "recall", dm, string(g.ID), "h2")
	gs.adminRow = putMemory(t, s, "main", "note", dm, "admin1", "")
	gs.roomRow = putMemory(t, s, "main", "note", "room/R1", string(g.ID), "")
	// The target already remembers the summary behind h2.
	gs.targetRecall = putMemory(t, s, "main", "recall", "dm/"+string(target), string(target), "h2")
	return gs
}

func TestAssign(t *testing.T) {
	ctx := context.Background()

	t.Run("moves IDs, memory and ends sessions", func(t *testing.T) {
		s, _ := newService(t)
		rec := &recordSink{}
		s.Events = rec
		target, err := s.CreateIdentity(ctx, "Ana", store.RoleUser, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		gs := newGuestWithState(t, s, target.ID)

		a, err := s.Assign(ctx, gs.identity.ID, target.ID, Principal{UserID: "admin1", Role: store.RoleAdmin})
		if err != nil {
			t.Fatal(err)
		}
		want := Assigned{From: gs.identity.ID, To: target.ID, IDs: 1, Memory: 3, Dropped: 1, Sessions: 1}
		if a != want {
			t.Fatalf("assigned = %+v, want %+v", a, want)
		}

		if _, err := s.st.GetIdentity(ctx, gs.identity.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("guest still there: %v", err)
		}
		if i, err := s.Resolve(ctx, "tg", []string{"77"}); err != nil || i.ID != target.ID {
			t.Fatalf("tg/77 resolves to %v, %v, want the target", i.ID, err)
		}
		dst := "dm/" + string(target.ID)
		for _, id := range []string{gs.note, gs.recall} {
			if p, uid := memoryOf(t, s, id); p != dst || uid != string(target.ID) {
				t.Fatalf("row %s in %q owned by %q, want %q", id, p, uid, dst)
			}
		}
		if p, _ := memoryOf(t, s, gs.dupe); p != "" {
			t.Fatalf("duplicate recall survived in %q", p)
		}
		if p, _ := memoryOf(t, s, gs.targetRecall); p != dst {
			t.Fatal("the target's own recall row was touched")
		}
		if p, uid := memoryOf(t, s, gs.adminRow); p != dst || uid != "admin1" {
			t.Fatalf("admin's row in %q by %q, want moved with its author kept", p, uid)
		}
		if p, uid := memoryOf(t, s, gs.roomRow); p != "room/R1" || uid != string(gs.identity.ID) {
			t.Fatalf("a note left in a room moved to %q/%q", p, uid)
		}

		dm, err := s.st.GetSession(ctx, gs.dm.ID)
		if err != nil || dm.EndedAt == nil || dm.Key != gs.dm.Key {
			t.Fatalf("dm session = %+v, %v, want ended with its key", dm, err)
		}

		e := rec.last()
		if e.Type != event.IdentityAssigned {
			t.Fatalf("event = %v", e.Type)
		}
		var attrs map[string]any
		if err := json.Unmarshal(e.Attrs, &attrs); err != nil {
			t.Fatal(err)
		}
		if attrs["source"] != string(gs.identity.ID) || attrs["target"] != string(target.ID) ||
			attrs["memory"] != 3.0 || attrs["sessions"] != 1.0 || attrs["by"] != "admin1" {
			t.Fatalf("attrs = %v", attrs)
		}
	})

	t.Run("cancels the guest's tasks instead of re-owning them", func(t *testing.T) {
		s, _ := newService(t)
		target, err := s.CreateIdentity(ctx, "Ana", store.RoleUser, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		g := pending(t, s, "77")
		if _, err := s.db.Exec(`INSERT INTO tasks (id, agent, prompt, schedule_kind, delivery_kind, status, next_run, created_by, created_at)
			VALUES ('t1', 'main', 'p', 'now', 'store', 'active', 1, ?, 1)`, string(g.ID)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO grants (id, agent, tool, scope, task_id, created_at) VALUES ('g1', 'main', 'x', 'task', 't1', 1)`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Assign(ctx, g.ID, target.ID, Principal{UserID: "admin1", Role: store.RoleAdmin}); err != nil {
			t.Fatal(err)
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

	t.Run("guards", func(t *testing.T) {
		s, _ := newService(t)
		owner, err := initOwner(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := s.CreateIdentity(ctx, "Bo", store.RoleUser, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		g1, err := s.EnsureGuest(ctx, "tg", []string{"1"}, "")
		if err != nil {
			t.Fatal(err)
		}
		g2, err := s.EnsureGuest(ctx, "tg", []string{"2"}, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			name     string
			from, to event.ID
			want     error
		}{
			{"to itself", g1.ID, g1.ID, ErrAssignSelf},
			{"from a confirmed user", identity.ID, owner.ID, ErrNotPending},
			{"onto a guest", g1.ID, g2.ID, ErrPending},
			{"unknown source", event.NewID(), owner.ID, store.ErrNotFound},
			{"unknown target", g1.ID, event.NewID(), store.ErrNotFound},
		} {
			if _, err := s.Assign(ctx, c.from, c.to, asOwner); !errors.Is(err, c.want) {
				t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
			}
		}
		if i, err := s.Resolve(ctx, "tg", []string{"1"}); err != nil || i.ID != g1.ID {
			t.Fatalf("refused assigns moved tg/1: %v, %v", i.ID, err)
		}
	})

	t.Run("a failure halfway rolls everything back", func(t *testing.T) {
		s, _ := newService(t)
		target, err := s.CreateIdentity(ctx, "Ana", store.RoleUser, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		gs := newGuestWithState(t, s, target.ID)
		// The guest row is deleted last: fail there.
		if _, err := s.db.Exec(`CREATE TRIGGER no_delete BEFORE DELETE ON identities
			BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Assign(ctx, gs.identity.ID, target.ID, asOwner); err == nil {
			t.Fatal("assign succeeded through the trigger")
		}
		if i, err := s.Resolve(ctx, "tg", []string{"77"}); err != nil || i.ID != gs.identity.ID {
			t.Fatalf("tg/77 resolves to %v, %v, want the guest", i.ID, err)
		}
		src := "dm/" + string(gs.identity.ID)
		for _, id := range []string{gs.note, gs.recall, gs.dupe} {
			if p, uid := memoryOf(t, s, id); p != src || uid != string(gs.identity.ID) {
				t.Fatalf("row %s in %q/%q after rollback", id, p, uid)
			}
		}
		if dm, err := s.st.GetSession(ctx, gs.dm.ID); err != nil || dm.EndedAt != nil {
			t.Fatalf("dm session = %+v, %v, want still open", dm, err)
		}
	})

	t.Run("only the owner assigns onto the owner", func(t *testing.T) {
		s, _ := newService(t)
		owner, err := initOwner(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		g, err := s.EnsureGuest(ctx, "tg", []string{"5"}, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, by := range []Principal{
			{UserID: "admin1", Role: store.RoleAdmin},
			{Role: store.RoleAdmin}, // an admin token tied to nobody
		} {
			if _, err := s.Assign(ctx, g.ID, owner.ID, by); !errors.Is(err, ErrOwnerOnly) {
				t.Fatalf("by %+v: err = %v, want ErrOwnerOnly", by, err)
			}
		}
		if i, err := s.Resolve(ctx, "tg", []string{"5"}); err != nil || i.ID != g.ID {
			t.Fatalf("a refused assign moved tg/5: %v, %v", i.ID, err)
		}
		if _, err := s.Assign(ctx, g.ID, owner.ID, asOwner); err != nil {
			t.Fatalf("owner: %v", err)
		}
	})
}

// asOwner is the owner's principal, as the unix socket or the owner's token
// carries it.
var asOwner = Principal{Role: store.RoleOwner}

func TestRedeemOverGuest(t *testing.T) {
	ctx := context.Background()

	t.Run("an invite takes the guest along onto its identity", func(t *testing.T) {
		s, _ := newService(t)
		rec := &recordSink{}
		s.Events = rec
		owner, err := initOwner(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		gs := newGuestWithState(t, s, owner.ID)
		ana, err := s.CreateIdentity(ctx, "Ana", store.RoleAdmin, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		code, inv, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: ana.ID, CreatedBy: owner.ID})
		if err != nil {
			t.Fatal(err)
		}

		i, err := s.RedeemInvite(ctx, code, "tg", []string{"77", "@new"}, "Ana")
		if err != nil {
			t.Fatal(err)
		}
		if i.ID != ana.ID || i.Role != store.RoleAdmin {
			t.Fatalf("redeemed as %+v, want %s", i, ana.ID)
		}
		for _, alias := range []string{"77", "@new"} {
			if got, err := s.Resolve(ctx, "tg", []string{alias}); err != nil || got.ID != i.ID {
				t.Fatalf("tg/%s resolves to %v, %v", alias, got.ID, err)
			}
		}
		if _, err := s.st.GetIdentity(ctx, gs.identity.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("guest still there: %v", err)
		}
		if p, _ := memoryOf(t, s, gs.note); p != "dm/"+string(i.ID) {
			t.Fatalf("note in %q, want the invited identity's DM", p)
		}
		ev := rec.of(event.IdentityAssigned)
		if len(ev) != 1 || ev[0]["invite"] != string(inv.ID) || ev[0]["target"] != string(i.ID) {
			t.Fatalf("identity.assigned = %v", ev)
		}
	})

	t.Run("an invite for the owner moves the guest onto it", func(t *testing.T) {
		s, _ := newService(t)
		owner, err := initOwner(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		gs := newGuestWithState(t, s, owner.ID)
		code, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: owner.ID, CreatedBy: owner.ID, CreatorRole: store.RoleOwner})
		if err != nil {
			t.Fatal(err)
		}
		i, err := s.RedeemInvite(ctx, code, "tg", []string{"77"}, "Angel")
		if err != nil {
			t.Fatal(err)
		}
		if i.ID != owner.ID {
			t.Fatalf("redeemed as %v, want the owner", i.ID)
		}
		if got, err := s.Resolve(ctx, "tg", []string{"77"}); err != nil || got.ID != owner.ID {
			t.Fatalf("tg/77 resolves to %v, %v", got.ID, err)
		}
		if p, _ := memoryOf(t, s, gs.dupe); p != "" {
			t.Fatal("duplicate recall survived the redeem")
		}
	})

	t.Run("a confirmed user's aliases are still refused", func(t *testing.T) {
		s, _ := newService(t)
		owner, err := initOwner(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		bo, err := s.CreateIdentity(ctx, "Bo", store.RoleUser, EmptyEventID)
		if err != nil {
			t.Fatal(err)
		}
		storetest.Bind(t, s.st, "tg", []string{"88"}, bo.ID, "")
		code, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: newUser(t, s, "Cy").ID, CreatedBy: owner.ID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RedeemInvite(ctx, code, "tg", []string{"88"}, ""); !errors.Is(err, store.ErrAliasLinked) {
			t.Fatalf("err = %v, want ErrAliasLinked", err)
		}
		// A guest alias next to a confirmed one is refused too, and nothing
		// moves: the code is still good.
		g, err := s.EnsureGuest(ctx, "tg", []string{"99"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RedeemInvite(ctx, code, "tg", []string{"99", "88"}, ""); !errors.Is(err, store.ErrAliasLinked) {
			t.Fatalf("mixed: err = %v, want ErrAliasLinked", err)
		}
		if got, err := s.Resolve(ctx, "tg", []string{"99"}); err != nil || got.ID != g.ID {
			t.Fatalf("tg/99 resolves to %v, %v, want the guest", got.ID, err)
		}
		if _, err := s.RedeemInvite(ctx, code, "tg", []string{"99"}, ""); err != nil {
			t.Fatalf("code spent by a refused redeem: %v", err)
		}
	})

	t.Run("aliases of two guests are refused", func(t *testing.T) {
		s, _ := newService(t)
		owner, err := initOwner(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range []string{"1", "2"} {
			if _, err := s.EnsureGuest(ctx, "tg", []string{a}, ""); err != nil {
				t.Fatal(err)
			}
		}
		code, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: newUser(t, s, "Cy").ID, CreatedBy: owner.ID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RedeemInvite(ctx, code, "tg", []string{"1", "2"}, ""); !errors.Is(err, store.ErrAliasLinked) {
			t.Fatalf("err = %v, want ErrAliasLinked", err)
		}
	})
}

// A token acts with its user's role, so minting one is linking an account
// too: only the owner mints one for the owner.
func TestMintTokenOwnerOnly(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	owner, err := initOwner(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []Scope{ScopeAdmin, ScopeClient} {
		if _, _, err := s.MintToken(ctx, Principal{Role: store.RoleAdmin}, "x", scope, owner.ID); !errors.Is(err, ErrOwnerOnly) {
			t.Fatalf("%s by an admin: err = %v, want ErrOwnerOnly", scope, err)
		}
	}
	if _, _, err := s.MintToken(ctx, asOwner, "x", ScopeClient, owner.ID); err != nil {
		t.Fatalf("by the owner: %v", err)
	}
	admin, err := s.CreateIdentity(ctx, "Ada", store.RoleAdmin, EmptyEventID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.MintToken(ctx, Principal{Role: store.RoleAdmin}, "x", ScopeClient, admin.ID); err != nil {
		t.Fatalf("an admin for an admin: %v", err)
	}
	if _, _, err := s.MintToken(ctx, asOwner, "x", ScopeClient, event.NewID()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown user: err = %v, want ErrNotFound", err)
	}
}

// An invite carries no role: the binding gets the role its identity has when
// the code is redeemed.
func TestRedeemTakesTheRoleAtRedeem(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	owner, err := initOwner(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	bo := newUser(t, s, "Bo")
	code, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: bo.ID, CreatedBy: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	setRole(t, s, bo.ID, store.RoleAdmin)
	i, err := s.RedeemInvite(ctx, code, "tg", []string{"9"}, "")
	if err != nil || i.ID != bo.ID || i.Role != store.RoleAdmin {
		t.Fatalf("redeemed as %+v, %v; want bo as admin", i, err)
	}
}
