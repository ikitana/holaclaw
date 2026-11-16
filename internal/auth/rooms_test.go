package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

func newRoom(t *testing.T, s *Service, conversation string) store.Identity {
	t.Helper()
	room, _, err := s.st.EnsureRoom(context.Background(), "tg", conversation, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	return room
}

func TestConfirmRoom(t *testing.T) {
	ctx := context.Background()

	t.Run("names and approves it", func(t *testing.T) {
		s, _ := newService(t)
		rec := &recordSink{}
		s.Events = rec
		room := newRoom(t, s, "-100")

		i, err := s.Confirm(ctx, room.ID, "ops", "", "admin1")
		if err != nil {
			t.Fatal(err)
		}
		if i.Kind != store.KindRoom || i.Name != "ops" || i.Role != "" || i.Status != store.StatusConfirmed || !i.Approved() {
			t.Fatalf("confirmed = %+v", i)
		}
		stored, err := s.st.GetIdentity(ctx, room.ID)
		if err != nil || stored.Name != "ops" || stored.Role != "" || stored.Pending() || !stored.Approved() || stored.ApprovedBy != "admin1" {
			t.Fatalf("stored = %+v, %v", stored, err)
		}
		// Its one binding stays, and the grant names the approver.
		if r, err := s.st.GetRoom(ctx, "tg", "-100"); err != nil || r.ID != room.ID {
			t.Fatalf("room = %+v, %v", r, err)
		}
		e := rec.last()
		var attrs map[string]any
		if err := json.Unmarshal(e.Attrs, &attrs); err != nil {
			t.Fatal(err)
		}
		if e.Type != event.AccessGranted || attrs["approved_by"] != "admin1" || attrs["conversation"] != "-100" || attrs["channel"] != "tg" {
			t.Fatalf("event = %s %v", e.Type, attrs)
		}
	})

	t.Run("keeps an earlier approval and approver and grants nothing new", func(t *testing.T) {
		s, _ := newService(t)
		rec := &recordSink{}
		s.Events = rec
		room := newRoom(t, s, "-100")
		at := time.UnixMilli(1_700_000_000_000)
		if _, _, err := s.st.ApproveRoom(ctx, "tg", "-100", "first", at); err != nil {
			t.Fatal(err)
		}
		i, err := s.Confirm(ctx, room.ID, "ops", "", "admin1")
		if err != nil || i.ApprovedAt == nil || !i.ApprovedAt.Equal(at) {
			t.Fatalf("confirmed = %+v, %v; want approved at %v", i, err, at)
		}
		if stored, _ := s.st.GetIdentity(ctx, room.ID); stored.ApprovedAt == nil || !stored.ApprovedAt.Equal(at) || stored.ApprovedBy != "first" {
			t.Fatalf("stored = %+v", stored)
		}
		if e := rec.last(); e.Type != event.IdentityConfirmed {
			t.Fatalf("last event = %s, want no second grant", e.Type)
		}
	})

	t.Run("an empty approver is stored as NULL", func(t *testing.T) {
		s, _ := newService(t)
		room := newRoom(t, s, "-100")
		if _, err := s.Confirm(ctx, room.ID, "ops", "", EmptyEventID); err != nil {
			t.Fatal(err)
		}
		var raw sql.NullString
		if err := s.st.DB().QueryRow(`SELECT approved_by FROM identities WHERE id = ?`, string(room.ID)).Scan(&raw); err != nil || raw.Valid {
			t.Fatalf("approved_by = %+v, %v; want NULL", raw, err)
		}
	})

	t.Run("a role is refused", func(t *testing.T) {
		s, _ := newService(t)
		room := newRoom(t, s, "-100")
		if _, err := s.Confirm(ctx, room.ID, "ops", store.RoleUser, "admin1"); !errors.Is(err, ErrInvalidRole) {
			t.Fatalf("err = %v, want ErrInvalidRole", err)
		}
		if stored, _ := s.st.GetIdentity(ctx, room.ID); !stored.Pending() || stored.Approved() || stored.Name != "-100" {
			t.Fatalf("stored = %+v, want untouched", stored)
		}
	})

	t.Run("a user still defaults to role user", func(t *testing.T) {
		s, _ := newService(t)
		g, err := s.EnsureGuest(ctx, "tg", []string{"77"}, "Dan")
		if err != nil {
			t.Fatal(err)
		}
		i, err := s.Confirm(ctx, g.ID, "Dan", "", "admin1")
		if err != nil || i.Role != store.RoleUser || i.Kind != store.KindUser || i.Approved() {
			t.Fatalf("confirmed = %+v, %v", i, err)
		}
	})
}

func TestUpdateRoom(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	room := newRoom(t, s, "-100")
	if _, err := s.Confirm(ctx, room.ID, "ops", "", "admin1"); err != nil {
		t.Fatal(err)
	}
	role := store.RoleUser
	if _, err := s.Update(ctx, room.ID, nil, &role, EmptyEventID); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("role on a room: %v, want ErrInvalidRole", err)
	}
	name := "platform-ops"
	i, err := s.Update(ctx, room.ID, &name, nil, EmptyEventID)
	if err != nil || i.Name != name || i.Role != "" || !i.Approved() || i.Kind != store.KindRoom {
		t.Fatalf("renamed = %+v, %v", i, err)
	}
}

// What only a user identity can do is refused on a room, with the one sentinel.
func TestRoomIdentityRefusals(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	owner, err := initOwner(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	by := Principal{UserID: owner.ID, Role: store.RoleOwner}
	room := newRoom(t, s, "-100")
	guest, err := s.EnsureGuest(ctx, "tg", []string{"77"}, "Dan")
	if err != nil {
		t.Fatal(err)
	}

	for name, call := range map[string]func() error{
		"assign a room": func() error { _, err := s.Assign(ctx, room.ID, owner.ID, by); return err },
		"assign onto a room": func() error {
			_, err := s.Assign(ctx, guest.ID, room.ID, by)
			return err
		},
		"assign a room as an admin": func() error {
			_, err := s.Assign(ctx, room.ID, owner.ID, Principal{Role: store.RoleAdmin})
			return err
		},
		"unbind its binding": func() error { _, err := s.Unbind(ctx, "tg", "-100", by); return err },
		"delete it as a pending identity": func() error {
			return s.withTx(ctx, func(tx *sql.Tx) error { _, err := deletePendingIn(ctx, tx, room.ID, s.now(), ""); return err })
		},
		"invite to a room": func() error {
			_, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: room.ID, CreatedBy: owner.ID, CreatorRole: store.RoleOwner})
			return err
		},
		"mint a token": func() error { _, _, err := s.MintToken(ctx, by, "t", ScopeClient, room.ID); return err },
		"create a token": func() error {
			_, _, err := s.CreateToken(ctx, "t", ScopeClient, room.ID)
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrRoomIdentity) {
			t.Errorf("%s: err = %v, want ErrRoomIdentity", name, err)
		}
	}
	if r, err := s.st.GetRoom(ctx, "tg", "-100"); err != nil || r.ID != room.ID {
		t.Fatalf("room after the refusals = %+v, %v", r, err)
	}
	if g, err := s.st.GetIdentity(ctx, guest.ID); err != nil || !g.Pending() {
		t.Fatalf("guest after the refusals = %+v, %v", g, err)
	}
}

// A room is disabled pending or confirmed, keeps its approval, comes back pending
// whatever it was, and approving doesn't revive it. A pending user stays refused.
func TestDisableRoom(t *testing.T) {
	ctx := context.Background()

	for _, confirmed := range []bool{false, true} {
		name := "pending"
		if confirmed {
			name = "confirmed"
		}
		t.Run(name, func(t *testing.T) {
			s, _ := newService(t)
			rec := &recordSink{}
			s.Events = rec
			enabled := 0
			s.AfterEnable = func(context.Context, event.ID) { enabled++ }
			room := newRoom(t, s, "-100")
			if _, _, err := s.st.ApproveRoom(ctx, "tg", "-100", "first", time.Now()); err != nil {
				t.Fatal(err)
			}
			if confirmed {
				if _, err := s.Confirm(ctx, room.ID, "ops", "", "admin1"); err != nil {
					t.Fatal(err)
				}
			}

			i, err := s.Disable(ctx, room.ID, "admin1")
			if err != nil || !i.Disabled() {
				t.Fatalf("disable = %+v, %v", i, err)
			}
			if e := rec.last(); e.Type != event.IdentityDisabled {
				t.Fatalf("last event = %s", e.Type)
			}
			if _, _, err := s.st.ApproveRoom(ctx, "tg", "-100", "other", time.Now()); !errors.Is(err, store.ErrRoomDisabled) {
				t.Fatalf("approve a disabled room: %v", err)
			}
			if _, err := s.Confirm(ctx, room.ID, "x", "", "admin1"); !errors.Is(err, ErrNotPending) {
				t.Fatalf("confirm a disabled room: %v", err)
			}
			events := len(rec.evts)
			if _, err := s.Disable(ctx, room.ID, "admin1"); err != nil || len(rec.evts) != events {
				t.Fatalf("disable twice: %v, events %d -> %d", err, events, len(rec.evts))
			}

			i, err = s.Enable(ctx, room.ID, "admin1")
			if err != nil || !i.Pending() || i.Role != "" || !i.Approved() || i.ApprovedBy != "first" || enabled != 1 {
				t.Fatalf("enable = %+v, %v, hook %d", i, err, enabled)
			}
			stored, err := s.st.GetIdentity(ctx, room.ID)
			if err != nil || !stored.Pending() || stored.Role != "" || !stored.Approved() {
				t.Fatalf("stored = %+v, %v", stored, err)
			}
			if _, err := s.Enable(ctx, room.ID, "admin1"); err != nil || enabled != 1 {
				t.Fatalf("enable twice: %v, hook %d", err, enabled)
			}
		})
	}

	t.Run("a pending user is refused", func(t *testing.T) {
		s, _ := newService(t)
		g, err := s.EnsureGuest(ctx, "tg", []string{"77"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Disable(ctx, g.ID, "admin1"); !errors.Is(err, ErrPending) {
			t.Fatalf("disable: %v, want ErrPending", err)
		}
		if _, err := s.Enable(ctx, g.ID, "admin1"); !errors.Is(err, ErrPending) {
			t.Fatalf("enable: %v, want ErrPending", err)
		}
	})

	t.Run("a confirmed user comes back confirmed", func(t *testing.T) {
		s, _ := newService(t)
		u, err := s.CreateIdentity(ctx, "Ada", store.RoleUser, "admin1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Disable(ctx, u.ID, "admin1"); err != nil {
			t.Fatal(err)
		}
		i, err := s.Enable(ctx, u.ID, "admin1")
		if err != nil || i.Status != store.StatusConfirmed || i.Role != store.RoleUser {
			t.Fatalf("enable = %+v, %v", i, err)
		}
	})
}

// Unbinding a pending room's one binding deletes nothing, on any kind of state.
func TestUnbindRoomLeavesItsPartition(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	room := newRoom(t, s, "-100")
	by := Principal{UserID: "admin", Role: store.RoleAdmin}
	if _, err := s.Unbind(ctx, "tg", "-100", by); !errors.Is(err, ErrRoomIdentity) {
		t.Fatalf("unbind: %v, want ErrRoomIdentity", err)
	}
	if r, err := s.st.GetRoom(ctx, "tg", "-100"); err != nil || r.ID != room.ID {
		t.Fatalf("room = %+v, %v", r, err)
	}
}

// A room's conversation id on a sender's envelope never resolves, is never
// taken into a guest, and can't redeem its way onto anyone.
func TestRoomAliasIsNotASendersToTake(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	owner, err := initOwner(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	room := newRoom(t, s, "-100")

	if _, err := s.Resolve(ctx, "tg", []string{"-100"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := s.EnsureGuest(ctx, "tg", []string{"-100"}, ""); !errors.Is(err, store.ErrAliasLinked) {
		t.Fatalf("ensure guest: %v", err)
	}

	code, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: owner.ID, CreatedBy: owner.ID, CreatorRole: store.RoleOwner})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemInvite(ctx, code, "tg", []string{"-100"}, ""); !errors.Is(err, store.ErrAliasLinked) {
		t.Fatalf("redeem: %v, want ErrAliasLinked", err)
	}
	if r, err := s.st.GetRoom(ctx, "tg", "-100"); err != nil || r.ID != room.ID {
		t.Fatalf("room = %+v, %v", r, err)
	}
}
