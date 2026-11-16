package auth

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// approved records what a room redemption asked approve to approve.
type approved struct {
	calls int
	by    event.ID
	err   error
}

func (a *approved) fn(_ *sql.Tx, by event.ID) error {
	a.calls++
	a.by = by
	return a.err
}

func TestRoomInviteSpecIsValidated(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	owner, err := initOwner(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	for name, spec := range map[string]InviteSpec{
		"no channel": {Kind: InviteRoom},
		"a user":     {Kind: InviteRoom, Channel: "tg", UserID: owner.ID},
		"old kind":   {Kind: "group", Channel: "tg"},
	} {
		_, _, err := s.CreateInvite(ctx, spec)
		var bad *InviteSpecError
		if !errors.Is(err, ErrInvalidInvite) || !errors.As(err, &bad) || bad.Reason == "" {
			t.Errorf("%s: err = %v, want an InviteSpecError", name, err)
		}
	}
	_, inv, err := s.CreateInvite(ctx, InviteSpec{Kind: InviteRoom, Channel: "tg", CreatedBy: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if inv.Kind != InviteRoom || inv.UserID != "" || inv.Scope != "" {
		t.Fatalf("invite = %+v", inv)
	}
	list, err := s.ListInvites(ctx)
	if err != nil || len(list) != 1 || list[0].Kind != InviteRoom {
		t.Fatalf("invites = %+v, err %v", list, err)
	}
	// An invite without a kind is a DM invite, as before the column existed.
	if _, inv, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: owner.ID, CreatorRole: store.RoleOwner}); err != nil || inv.Kind != InviteDM {
		t.Fatalf("default kind = %q, err %v", inv.Kind, err)
	}
}

func TestRedeemRoomInviteApprovesAsTheCreator(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	owner, err := initOwner(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	code, _, err := s.CreateInvite(ctx, InviteSpec{Kind: InviteRoom, Channel: "tg", CreatedBy: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	var a approved
	id, err := s.RedeemRoomInvite(ctx, code, "tg", "g1", []string{"ana"}, a.fn)
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if list, _ := s.ListInvites(ctx); len(list) != 1 || list[0].ID != id {
		t.Fatalf("redeem returned %q, invites %+v", id, list)
	}
	if a.calls != 1 || a.by != owner.ID {
		t.Fatalf("approve = %+v, want one call by %s", a, owner.ID)
	}
	// The sender was not linked: a room code grants nobody anything.
	if _, err := s.Resolve(ctx, "tg", []string{"ana"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resolve sender err = %v, want ErrNotFound", err)
	}
	// One-shot.
	if _, err := s.RedeemRoomInvite(ctx, code, "tg", "g2", []string{"ana"}, a.fn); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("reuse err = %v, want ErrInviteInvalid", err)
	}
	if a.calls != 1 {
		t.Fatal("a used code approved again")
	}
}

// A failed approval rolls the redemption back: the code still works.
func TestRedeemRoomInviteApproveFailureConsumesNothing(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	code, _, err := s.CreateInvite(ctx, InviteSpec{Kind: InviteRoom, Channel: "tg"})
	if err != nil {
		t.Fatal(err)
	}
	a := approved{err: errors.New("boom")}
	if _, err := s.RedeemRoomInvite(ctx, code, "tg", "g1", []string{"ana"}, a.fn); err == nil || errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("err = %v, want the approve error", err)
	}
	a.err = nil
	if _, err := s.RedeemRoomInvite(ctx, code, "tg", "g1", []string{"ana"}, a.fn); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

// The kinds never redeem for each other, and a mismatch leaves the invite
// usable where it belongs.
func TestInviteKindsDoNotCross(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	dmCode, _, err := s.CreateInvite(ctx, InviteSpec{Channel: "tg", UserID: newUser(t, s, "Bo").ID})
	if err != nil {
		t.Fatal(err)
	}
	roomCode, _, err := s.CreateInvite(ctx, InviteSpec{Kind: InviteRoom, Channel: "tg"})
	if err != nil {
		t.Fatal(err)
	}
	var a approved
	if _, err := s.RedeemRoomInvite(ctx, dmCode, "tg", "g1", []string{"ana"}, a.fn); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("DM code in a room: err = %v, want ErrInviteInvalid", err)
	}
	if _, err := s.RedeemInvite(ctx, roomCode, "tg", []string{"bo"}, "Bo"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("room code in a DM: err = %v, want ErrInviteInvalid", err)
	}
	if a.calls != 0 {
		t.Fatal("a DM code approved a room")
	}
	if _, err := s.RedeemInvite(ctx, dmCode, "tg", []string{"cy"}, "Cy"); err != nil {
		t.Fatalf("DM code after the mismatch: %v", err)
	}
	if _, err := s.RedeemRoomInvite(ctx, roomCode, "tg", "g1", []string{"cy"}, a.fn); err != nil {
		t.Fatalf("room code after the mismatch: %v", err)
	}
}

func TestRedeemRoomInviteChannelAndArgs(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	code, _, err := s.CreateInvite(ctx, InviteSpec{Kind: InviteRoom, Channel: "tg"})
	if err != nil {
		t.Fatal(err)
	}
	var a approved
	if _, err := s.RedeemRoomInvite(ctx, code, "tg-other", "g1", nil, a.fn); !errors.Is(err, ErrInviteChannel) {
		t.Fatalf("other channel: err = %v, want ErrInviteChannel", err)
	}
	if _, err := s.RedeemRoomInvite(ctx, code, "tg", "", nil, a.fn); err == nil {
		t.Fatal("no conversation: want an error")
	}
	if _, err := s.RedeemRoomInvite(ctx, code, "tg", "g1", nil, nil); err == nil {
		t.Fatal("no approve: want an error")
	}
	if a.calls != 0 {
		t.Fatalf("approve called %d times", a.calls)
	}
}

// The room is a rate-limit key of its own: rotating accounts inside one room
// does not buy more guesses.
func TestRedeemRoomInviteRateLimitedPerRoom(t *testing.T) {
	ctx := context.Background()
	s, _ := newService(t)
	var a approved
	for i := range redeemFailures {
		alias := []string{string(rune('a' + i))}
		if _, err := s.RedeemRoomInvite(ctx, "ZZZZZZZZZZ", "tg", "g1", alias, a.fn); !errors.Is(err, ErrInviteInvalid) {
			t.Fatalf("attempt %d err = %v, want ErrInviteInvalid", i, err)
		}
	}
	code, _, err := s.CreateInvite(ctx, InviteSpec{Kind: InviteRoom, Channel: "tg"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemRoomInvite(ctx, code, "tg", "g1", []string{"fresh"}, a.fn); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	// Another room is unaffected.
	if _, err := s.RedeemRoomInvite(ctx, code, "tg", "g2", []string{"fresh"}, a.fn); err != nil {
		t.Fatalf("other room: %v", err)
	}
}
