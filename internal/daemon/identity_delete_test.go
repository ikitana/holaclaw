package daemon

import (
	"slices"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/memory"
	"holaclaw/internal/testmodel"
)

// Deleting an identity forgets its own memory and nothing else, and the
// account it held is a stranger again (issue #48, docs/identity.md §2).
func TestDeleteIdentityForgetsItsMemoryAndFreesItsAliases(t *testing.T) {
	e := startTG(t, tgOpts{dm: "open"}, testmodel.Step{Text: "hi"}, testmodel.Step{Text: "hi again"})
	evs := e.watch("")
	e.fake.pushDM(mallory, 777, "hello")
	evs.await(event.Reply)
	first := guestIDs(e)
	if len(first) != 1 {
		t.Fatalf("guests = %v, want one", first)
	}
	guest := event.ID(first[0])

	d := e.daemon
	own := memory.Provenance{Agent: "main", Partition: UserPartition(guest), UserID: guest, Role: "user", Time: time.Now()}
	room := own
	room.Partition = RoomPartition("R1")
	for _, it := range []memory.Item{
		{Layer: memory.LayerNote, Text: "mallory likes tea", Visibility: memory.VisOrigin, Prov: own},
		{Layer: memory.LayerNote, Text: "the office opens at nine", Visibility: memory.VisShared, Prov: own},
		{Layer: memory.LayerNote, Text: "the room meets on mondays", Visibility: memory.VisOrigin, Prov: room},
	} {
		if _, err := d.App.Memory.Write(e.ctx, it); err != nil {
			t.Fatal(err)
		}
	}

	sessID := event.ID(e.tgSession("777").Id)

	// Over the socket, as `holaclaw identity delete` does: the owner deletes.
	del, err := e.client.DeleteIdentity(e.ctx, string(guest), false)
	if err != nil {
		t.Fatal(err)
	}
	if del.MemoryForgotten != 1 || del.MemoryKept || len(del.Aliases) == 0 || !strings.HasPrefix(del.Aliases[0], "tg:") {
		t.Fatalf("deleted = %+v, want one row forgotten and the tg aliases removed", del)
	}
	ev := evs.await(event.IdentityDeleted)
	owner, err := d.Auth.Owner(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Attrs == nil || (*ev.Attrs)["by"] != string(owner.ID) {
		t.Fatalf("identity.deleted attrs = %v, want by the owner %s", ev.Attrs, owner.ID)
	}
	if left := memoryTexts(t, e); !slices.Equal(left, []string{"the office opens at nine", "the room meets on mondays"}) {
		t.Fatalf("memory left = %q", left)
	}

	// A compaction finishing after the delete writes no recall of the
	// conversation: the session belongs to nobody now.
	sess, err := d.App.Store.GetSession(e.ctx, sessID)
	if err != nil || sess.UserID != "" {
		t.Fatalf("session after delete = %+v, %v; want it kept with no user", sess, err)
	}
	d.ingestRecall(e.ctx, sess, "mallory asked about the green build", false)
	if left := memoryTexts(t, e); len(left) != 2 {
		t.Fatalf("recall written for a deleted user: %q", left)
	}

	// The same account writes again: a new pending identity, as for anyone.
	e.fake.pushDM(mallory, 777, "hello again")
	evs.await(event.Reply)
	second := guestIDs(e)
	if len(second) != 1 || second[0] == first[0] {
		t.Fatalf("guests after delete = %v, want one new one (was %v)", second, first)
	}
}

func guestIDs(e *tgEnv) []string {
	var out []string
	for _, u := range e.users() {
		if u.Role != nil && *u.Role == "guest" {
			out = append(out, u.Id)
		}
	}
	return out
}

func memoryTexts(t *testing.T, e *tgEnv) []string {
	t.Helper()
	rows, err := e.daemon.App.Store.DB().QueryContext(e.ctx, `SELECT text FROM memory_items ORDER BY text`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck // test
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}
