package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/event"
)

// waNS are the WhatsApp namespaces: "wa:lid:1" is in "wa:lid:" only.
var waNS = []string{"wa:", "wa:lid:"}

func mustLink(t *testing.T, s *SQLite, channel string, i Identity, aliases ...string) {
	t.Helper()
	for _, a := range aliases {
		if _, err := s.DB().Exec(`INSERT INTO bindings (channel, alias, identity_id, display, created_at) VALUES (?, ?, ?, '', 1)`,
			channel, a, string(i.ID)); err != nil {
			t.Fatalf("link %v: %v", aliases, err)
		}
	}
}

// ownerOf is the identity holding alias on channel, or "".
func ownerOf(t *testing.T, s *SQLite, channel, alias string) event.ID {
	t.Helper()
	var id string
	err := s.DB().QueryRow(`SELECT identity_id FROM bindings WHERE channel = ? AND alias = ?`, channel, alias).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return event.ID(id)
}

func countIDs(t *testing.T, s *SQLite) int {
	t.Helper()
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM bindings`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A person linked by phone number writes with phone and LID: the LID joins
// them, and the next envelope with the LID alone finds them.
func TestResolveAndLinkTwoAliases(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ana := mustIdentity(t, s, "ana", RoleUser)
	mustLink(t, s, "wa", ana, "wa:+34600")

	r, err := s.ResolveAndLink(ctx, "wa", []string{"wa:+34600", "wa:lid:1"}, "Ana", waNS)
	if err != nil {
		t.Fatal(err)
	}
	if r.Identity.ID != ana.ID || r.Matched != "wa:+34600" || !reflect.DeepEqual(r.Free, []string{"wa:lid:1"}) || len(r.Conflicts) != 0 {
		t.Fatalf("resolution = %+v", r)
	}
	if got := ownerOf(t, s, "wa", "wa:lid:1"); got != ana.ID {
		t.Fatalf("lid owner = %q, want %q", got, ana.ID)
	}
	// Linked once: the same envelope again adds nothing.
	r, err = s.ResolveAndLink(ctx, "wa", []string{"wa:lid:1", "wa:+34600"}, "Ana", waNS)
	if err != nil || r.Identity.ID != ana.ID || r.Matched != "wa:lid:1" || len(r.Free) != 0 {
		t.Fatalf("resolution = %+v, err %v", r, err)
	}
	// Other channels see none of it.
	if got := ownerOf(t, s, "wa-2", "wa:lid:1"); got != "" {
		t.Fatalf("lid linked on another channel: %q", got)
	}
}

func TestResolveAndLinkSingleAliasIsNoOp(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ana := mustIdentity(t, s, "ana", RoleUser)
	mustLink(t, s, "wa", ana, "wa:+34600")

	r, err := s.ResolveAndLink(ctx, "wa", []string{"wa:+34600"}, "", waNS)
	if err != nil || r.Identity.ID != ana.ID || len(r.Free) != 0 || len(r.Conflicts) != 0 {
		t.Fatalf("resolution = %+v, err %v", r, err)
	}
	if n := countIDs(t, s); n != 1 {
		t.Fatalf("ids = %d, want 1", n)
	}
}

// An envelope nobody knows links nothing and offers every alias as free.
func TestResolveAndLinkUnknown(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	r, err := s.ResolveAndLink(ctx, "wa", []string{"wa:+1", "", "wa:lid:1", "wa:+1"}, "", waNS)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if !reflect.DeepEqual(r.Free, []string{"wa:+1", "wa:lid:1"}) || r.Identity.ID != "" {
		t.Fatalf("resolution = %+v", r)
	}
	if n := countIDs(t, s); n != 0 {
		t.Fatalf("ids = %d, want 0", n)
	}
	if _, err := s.ResolveAndLink(ctx, "wa", nil, "", waNS); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no aliases: %v", err)
	}
	if _, err := s.ResolveAndLink(ctx, "", []string{"wa:+1"}, "", waNS); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no channel: %v", err)
	}
}

// Guard: an alias another identity holds is left alone, and the message
// resolves through the first match.
func TestResolveAndLinkLeavesOtherIdentitiesAlone(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ana := mustIdentity(t, s, "ana", RoleUser)
	bo := mustIdentity(t, s, "bo", RoleUser)
	mustLink(t, s, "wa", ana, "wa:+1")
	mustLink(t, s, "wa", bo, "wa:lid:2")

	r, err := s.ResolveAndLink(ctx, "wa", []string{"wa:+1", "wa:lid:2"}, "", waNS)
	if err != nil {
		t.Fatal(err)
	}
	want := []AliasConflict{{Alias: "wa:lid:2", IdentityID: bo.ID}}
	if r.Identity.ID != ana.ID || r.Matched != "wa:+1" || len(r.Free) != 0 || !reflect.DeepEqual(r.Conflicts, want) {
		t.Fatalf("resolution = %+v", r)
	}
	if got := ownerOf(t, s, "wa", "wa:lid:2"); got != bo.ID {
		t.Fatalf("wa:lid:2 moved to %q", got)
	}

	// Without namespaces the free alias still joins the first match.
	mustLink(t, s, "tg", ana, "11")
	mustLink(t, s, "tg", bo, "22")
	r, err = s.ResolveAndLink(ctx, "tg", []string{"22", "11", "33"}, "", nil)
	if err != nil || r.Identity.ID != bo.ID || !reflect.DeepEqual(r.Free, []string{"33"}) ||
		!reflect.DeepEqual(r.Conflicts, []AliasConflict{{Alias: "11", IdentityID: ana.ID}}) {
		t.Fatalf("resolution = %+v, err %v", r, err)
	}
	if got := ownerOf(t, s, "tg", "11"); got != ana.ID {
		t.Fatalf("11 moved to %q", got)
	}
}

// Guard: a recycled phone number. The identity behind the number already holds
// another LID, so the match is not trusted and nothing is linked.
func TestResolveAndLinkRecycledNumber(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	old := mustIdentity(t, s, "old", RoleUser)
	mustLink(t, s, "wa", old, "wa:+1", "wa:lid:9")

	r, err := s.ResolveAndLink(ctx, "wa", []string{"wa:+1", "wa:lid:5"}, "", waNS)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	want := []AliasConflict{{Alias: "wa:lid:5", IdentityID: old.ID, Held: "wa:lid:9"}}
	if r.Identity.ID != "" || !reflect.DeepEqual(r.Conflicts, want) || !reflect.DeepEqual(r.Free, []string{"wa:lid:5"}) {
		t.Fatalf("resolution = %+v", r)
	}
	if got := ownerOf(t, s, "wa", "wa:lid:5"); got != "" {
		t.Fatalf("recycled LID linked to %q", got)
	}

	// The new person, once an identity of their own holds the LID, resolves
	// to it; the number stays with the old identity.
	fresh := mustIdentity(t, s, "fresh", "")
	mustLink(t, s, "wa", fresh, "wa:lid:5")
	r, err = s.ResolveAndLink(ctx, "wa", []string{"wa:+1", "wa:lid:5"}, "", waNS)
	if err != nil {
		t.Fatal(err)
	}
	// One conflict for the old identity, not one more for its number.
	want = []AliasConflict{{Alias: "wa:lid:5", IdentityID: old.ID, Held: "wa:lid:9"}}
	if r.Identity.ID != fresh.ID || r.Matched != "wa:lid:5" || len(r.Free) != 0 || !reflect.DeepEqual(r.Conflicts, want) {
		t.Fatalf("resolution = %+v", r)
	}
	if got := ownerOf(t, s, "wa", "wa:+1"); got != old.ID {
		t.Fatalf("number moved to %q", got)
	}
}

// Namespaces nest: "wa:lid:9" is in "wa:lid:", so it does not make "wa:+1" a
// second phone number.
func TestResolveAndLinkLongestNamespace(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ana := mustIdentity(t, s, "ana", RoleUser)
	mustLink(t, s, "wa", ana, "wa:lid:9")

	r, err := s.ResolveAndLink(ctx, "wa", []string{"wa:lid:9", "wa:+1"}, "", waNS)
	if err != nil || r.Identity.ID != ana.ID || !reflect.DeepEqual(r.Free, []string{"wa:+1"}) {
		t.Fatalf("resolution = %+v, err %v", r, err)
	}
	// A second number is a second id in "wa:".
	r, err = s.ResolveAndLink(ctx, "wa", []string{"wa:lid:9", "wa:+2"}, "", waNS)
	if !errors.Is(err, ErrNotFound) || len(r.Conflicts) != 1 || r.Conflicts[0].Held != "wa:+1" {
		t.Fatalf("resolution = %+v, err %v", r, err)
	}
}

// Aliases in no declared namespace never trip the guard: Google Chat's user
// id and email.
func TestResolveAndLinkUndeclaredNamespaceIsExempt(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ana := mustIdentity(t, s, "ana", RoleUser)
	mustLink(t, s, "gchat", ana, "users/1", "ana@old.example")

	r, err := s.ResolveAndLink(ctx, "gchat", []string{"users/1", "ana@new.example"}, "", nil)
	if err != nil || r.Identity.ID != ana.ID || !reflect.DeepEqual(r.Free, []string{"ana@new.example"}) {
		t.Fatalf("resolution = %+v, err %v", r, err)
	}
	// Declared namespaces of another shape do not cover them either.
	r, err = s.ResolveAndLink(ctx, "gchat", []string{"users/1", "ana@third.example"}, "", waNS)
	if err != nil || len(r.Free) != 1 {
		t.Fatalf("resolution = %+v, err %v", r, err)
	}
}

// A pending identity is linked and guarded the same as a confirmed one.
func TestResolveAndLinkPendingIdentity(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	guest := mustIdentity(t, s, "guest", "")
	mustLink(t, s, "wa", guest, "wa:+1")

	r, err := s.ResolveAndLink(ctx, "wa", []string{"wa:+1", "wa:lid:1"}, "", waNS)
	if err != nil || r.Identity.ID != guest.ID || len(r.Free) != 1 {
		t.Fatalf("resolution = %+v, err %v", r, err)
	}
	if _, err := s.ResolveAndLink(ctx, "wa", []string{"wa:+1", "wa:lid:2"}, "", waNS); !errors.Is(err, ErrNotFound) {
		t.Fatalf("recycled guard on pending: %v", err)
	}
}

// Two envelopes for the same person at once: no error, one link.
func TestResolveAndLinkConcurrent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ana := mustIdentity(t, s, "ana", RoleUser)
	mustLink(t, s, "wa", ana, "wa:+1")

	const n = 8
	var wg sync.WaitGroup
	results := make([]Resolution, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = s.ResolveAndLink(ctx, "wa", []string{"wa:+1", "wa:lid:1"}, "", waNS)
		}()
	}
	wg.Wait()
	added := 0
	for i := range n {
		if errs[i] != nil || results[i].Identity.ID != ana.ID {
			t.Fatalf("envelope %d: %+v, err %v", i, results[i], errs[i])
		}
		added += len(results[i].Free)
	}
	if added != 1 {
		t.Fatalf("links reported = %d, want 1", added)
	}
	if c := countIDs(t, s); c != 2 {
		t.Fatalf("ids = %d, want 2", c)
	}
}

// A disabled identity keeps what it holds and takes nothing new.
func TestResolveAndLinkDisabledLinksNothing(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	i := Identity{ID: event.NewID(), Name: "off", Role: RoleUser, Status: StatusDisabled, CreatedAt: time.Now()}
	if err := s.CreateIdentity(ctx, i); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetIdentity(ctx, i.ID); err != nil || !got.Disabled() || got.Role != RoleUser {
		t.Fatalf("stored = %+v, %v", got, err)
	}
	mustLink(t, s, "tg", i, "1")
	r, err := s.ResolveAndLink(ctx, "tg", []string{"1", "2"}, "", nil)
	if err != nil || r.Identity.ID != i.ID || !r.Identity.Disabled() {
		t.Fatalf("resolution = %+v, %v", r, err)
	}
	if _, err := ResolveAliases(ctx, s.db, "tg", []string{"2"}, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the free alias was linked: %v", err)
	}
}
