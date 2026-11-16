package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"holaclaw/internal/event"
)

// A user's role follows its status, a room never has one.
func TestIdentityKindCheck(t *testing.T) {
	s := newStore(t)
	for name, q := range map[string]string{
		"pending user with a role":  `INSERT INTO identities (id, kind, name, role, status, created_at) VALUES ('x', 'user', 'x', 'user', 'pending', 1)`,
		"confirmed user, no role":   `INSERT INTO identities (id, kind, name, role, status, created_at) VALUES ('x', 'user', 'x', NULL, 'confirmed', 1)`,
		"confirmed room with role":  `INSERT INTO identities (id, kind, name, role, status, created_at) VALUES ('x', 'room', 'x', 'user', 'confirmed', 1)`,
		"pending room with a role":  `INSERT INTO identities (id, kind, name, role, status, created_at) VALUES ('x', 'room', 'x', 'admin', 'pending', 1)`,
		"disabled room with a role": `INSERT INTO identities (id, kind, name, role, status, created_at) VALUES ('x', 'room', 'x', 'user', 'disabled', 1)`,
	} {
		if _, err := s.DB().Exec(q); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for name, q := range map[string]string{
		"confirmed room": `INSERT INTO identities (id, kind, name, role, status, created_at) VALUES ('r1', 'room', 'x', NULL, 'confirmed', 1)`,
		"disabled room":  `INSERT INTO identities (id, kind, name, role, status, created_at) VALUES ('r2', 'room', 'x', NULL, 'disabled', 1)`,
		"disabled user":  `INSERT INTO identities (id, kind, name, role, status, created_at) VALUES ('r3', 'user', 'x', 'user', 'disabled', 1)`,
	} {
		if _, err := s.DB().Exec(q); err != nil {
			t.Errorf("%s refused: %v", name, err)
		}
	}
}

func TestRoomLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	at := time.UnixMilli(1_700_000_000_000)

	if _, err := s.GetRoom(ctx, "tg", "-100"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get: %v, want ErrNotFound", err)
	}
	room, created, err := s.EnsureRoom(ctx, "tg", "-100", at)
	if err != nil || !created {
		t.Fatalf("ensure: created %v, %v", created, err)
	}
	if room.Kind != KindRoom || !room.Pending() || room.Name != "-100" || room.Approved() || !room.ID.Valid() {
		t.Fatalf("room = %+v", room)
	}
	if again, created, err := s.EnsureRoom(ctx, "tg", "-100", at.Add(time.Hour)); err != nil || created || again.ID != room.ID {
		t.Fatalf("again = %+v, created %v, %v", again, created, err)
	}
	if _, _, err := s.EnsureRoom(ctx, "", "-100", at); err == nil {
		t.Fatal("a room with no channel")
	}
	if _, _, err := s.EnsureRoom(ctx, "tg", "", at); err == nil {
		t.Fatal("a room with no conversation")
	}

	approved, change, err := s.ApproveRoom(ctx, "tg", "-100", "adm", at)
	if err != nil || change != (RoomChange{Approved: true}) || approved.ID != room.ID || !approved.Approved() || !approved.Pending() {
		t.Fatalf("approve = %+v, %+v, %v", approved, change, err)
	}
	if _, change, err := s.ApproveRoom(ctx, "tg", "-100", "other", at.Add(time.Hour)); err != nil || change != (RoomChange{}) {
		t.Fatalf("approve twice: change %+v, err %v", change, err)
	}
	stored, err := s.GetRoom(ctx, "tg", "-100")
	if err != nil || stored.ApprovedAt == nil || !stored.ApprovedAt.Equal(at) || stored.ApprovedBy != "adm" {
		t.Fatalf("stored = %+v, %v; want the first time and approver kept", stored, err)
	}

	// Approving creates the room when it has none.
	made, change, err := s.ApproveRoom(ctx, "tg", "-200", "", at)
	if err != nil || change != (RoomChange{Created: true, Approved: true}) || !made.Pending() || !made.Approved() || made.ApprovedBy != "" {
		t.Fatalf("made = %+v, %+v, %v", made, change, err)
	}

	// Approving never revives a disabled room.
	if _, err := s.DB().Exec(`UPDATE identities SET status = 'disabled', approved_at = NULL WHERE id = ?`, string(room.ID)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ApproveRoom(ctx, "tg", "-100", "adm", at); !errors.Is(err, ErrRoomDisabled) {
		t.Fatalf("approve a disabled room: %v, want ErrRoomDisabled", err)
	}
	if got, _ := s.GetRoom(ctx, "tg", "-100"); !got.Disabled() || got.Approved() {
		t.Fatalf("disabled room = %+v", got)
	}
}

// The approver is provenance, not a link: no foreign key, an empty one is NULL,
// and deleting the approver leaves the room approved and the record intact.
func TestRoomApproverOutlivesTheApprover(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	adm := Identity{ID: event.NewID(), Name: "ada", Role: RoleAdmin}
	if err := s.CreateIdentity(ctx, adm); err != nil {
		t.Fatal(err)
	}
	room, change, err := s.ApproveRoom(ctx, "tg", "-100", adm.ID, time.Now())
	if err != nil || !change.Approved || room.ApprovedBy != adm.ID {
		t.Fatalf("approve = %+v, %+v, %v", room, change, err)
	}
	if err := s.DeleteIdentity(ctx, adm.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRoom(ctx, "tg", "-100")
	if err != nil || !got.Approved() || got.ApprovedBy != adm.ID {
		t.Fatalf("room after deleting the approver = %+v, %v", got, err)
	}
}

// Only a room is ever approved.
func TestUserIdentityCannotBeApproved(t *testing.T) {
	s := newStore(t)
	u := Identity{ID: event.NewID(), Name: "ugo", Role: RoleUser}
	if err := s.CreateIdentity(context.Background(), u); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE identities SET approved_by = 'x' WHERE id = ?`, string(u.ID)); err == nil {
		t.Fatal("a user identity took an approver")
	}
}

// A conversation whose binding a user holds is never adopted as a room.
func TestRoomLookupNeverAdoptsAUser(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	u := Identity{ID: event.NewID(), Name: "ugo", Role: RoleUser}
	if err := s.CreateIdentity(ctx, u); err != nil {
		t.Fatal(err)
	}
	mustLink(t, s, "tg", u, "shared")
	if _, err := s.GetRoom(ctx, "tg", "shared"); !errors.Is(err, ErrAliasLinked) {
		t.Fatalf("get: %v", err)
	}
	if _, _, err := s.EnsureRoom(ctx, "tg", "shared", time.Now()); !errors.Is(err, ErrAliasLinked) {
		t.Fatalf("ensure: %v", err)
	}
	if _, _, err := s.ApproveRoom(ctx, "tg", "shared", "adm", time.Now()); !errors.Is(err, ErrAliasLinked) {
		t.Fatalf("approve: %v", err)
	}
	if got := ownerOf(t, s, "tg", "shared"); got != u.ID {
		t.Fatalf("binding moved to %q", got)
	}
}

// A room's binding never resolves a sender and isn't free for anyone to take,
// pending room or not.
func TestRoomAliasIsNeverASender(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	room, _, err := s.EnsureRoom(ctx, "tg", "-100", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	r, err := ResolveAliases(ctx, s.DB(), "tg", []string{"-100"}, nil)
	if !errors.Is(err, ErrNotFound) || r.Identity.ID != "" || len(r.Free) != 0 {
		t.Fatalf("resolve = %+v, %v; want no match and nothing free", r, err)
	}

	// Beside a real alias the room's stays out of Free, and is left alone.
	u := Identity{ID: event.NewID(), Name: "ugo", Role: RoleUser}
	if err := s.CreateIdentity(ctx, u); err != nil {
		t.Fatal(err)
	}
	mustLink(t, s, "tg", u, "11")
	r, err = s.ResolveAndLink(ctx, "tg", []string{"-100", "11", "12"}, "", nil)
	if err != nil || r.Identity.ID != u.ID {
		t.Fatalf("resolve and link = %+v, %v", r, err)
	}
	if !reflect.DeepEqual(r.Free, []string{"12"}) {
		t.Fatalf("free = %v", r.Free)
	}
	if len(r.Conflicts) != 1 || r.Conflicts[0].IdentityID != room.ID {
		t.Fatalf("conflicts = %+v", r.Conflicts)
	}
	if got := ownerOf(t, s, "tg", "-100"); got != room.ID {
		t.Fatalf("binding moved to %q", got)
	}
}

func TestCreateRoomIdentityTakesNoDefaultRole(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	at := time.UnixMilli(1_700_000_000_000)
	id := event.NewID()
	if err := s.CreateIdentity(ctx, Identity{ID: id, Kind: KindRoom, Name: "ops", ApprovedAt: &at}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetIdentity(ctx, id)
	if err != nil || got.Role != "" || got.Status != StatusConfirmed || got.ApprovedAt == nil || !got.ApprovedAt.Equal(at) {
		t.Fatalf("room = %+v, %v", got, err)
	}
	if err := s.CreateIdentity(ctx, Identity{Kind: KindRoom, Name: "ops", Role: RoleUser}); err == nil {
		t.Fatal("a room with a role")
	}
}
