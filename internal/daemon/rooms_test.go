package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
	"holaclaw/internal/memory"
	"holaclaw/internal/store"
	"holaclaw/internal/testmodel"
	"holaclaw/internal/tool"
)

func roomOrigin(conversation string) event.Origin {
	return event.Origin{Channel: "fake", ChannelType: "fake", Conversation: conversation, ConversationKind: event.ConversationRoom}
}

func TestPartitionFollowsTheOwner(t *testing.T) {
	t.Parallel()
	const id = "01J9Q4ABCDEFGHJKMNPQRSTVWX"
	dm := event.Origin{Channel: "tg", Conversation: "11", ConversationKind: event.ConversationDM}
	spaces := event.Origin{Channel: "gc", Conversation: "spaces/AAA", ConversationKind: event.ConversationRoom}
	for _, tc := range []struct {
		name string
		sess store.Session
		want string
	}{
		{"a room", store.Session{UserID: id, Origin: spaces, Kind: store.SessionChat}, "room/" + id},
		// A room mid-delete reads and writes neither its old space nor a person's.
		{"an ownerless room", store.Session{Origin: spaces, Kind: store.SessionChat}, "room/gc/spaces/AAA"},
		{"a dm", store.Session{UserID: id, Origin: dm, Kind: store.SessionChat}, "dm/" + id},
		{"a task", store.Session{ID: "T", UserID: id, Origin: spaces, Kind: store.SessionTask}, "task/T"},
	} {
		if got := Partition(tc.sess); got != tc.want {
			t.Errorf("%s: partition = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// roomFixture is an owner, a plain user and a pending sender with a room's
// session, on a real daemon.
type roomFixture struct {
	e                  *env
	sess               store.Session
	room               store.Identity
	owner, user, guest store.Identity
}

func newRoomFixture(t *testing.T, conversation string) *roomFixture {
	t.Helper()
	e := start(t, opts{}, testmodel.Step{Text: "ok"})
	ctx := context.Background()
	st := e.daemon.App.Store
	f := &roomFixture{e: e}
	var err error
	if f.room, _, err = st.EnsureRoom(ctx, "fake", conversation, time.Now()); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, role store.Role, status store.IdentityStatus) store.Identity {
		t.Helper()
		i := store.Identity{ID: event.NewID(), Name: name, Role: role, Status: status, CreatedAt: time.Now()}
		if err := st.CreateIdentity(ctx, i); err != nil {
			t.Fatal(err)
		}
		return i
	}
	f.owner = mk("own", store.RoleAdmin, store.StatusConfirmed)
	f.user = mk("usr", store.RoleUser, store.StatusConfirmed)
	f.guest = mk("gst", "", store.StatusPending)
	origin := roomOrigin(conversation)
	if f.sess, _, err = st.EnsureSession(ctx, store.Session{
		Key: "main/fake/room/" + conversation, Agent: "main", Kind: store.SessionChat, UserID: f.room.ID, Origin: origin,
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *roomFixture) prepare(t *testing.T, msgs ...event.Message) tool.Call {
	t.Helper()
	in, err := f.e.daemon.prepare(context.Background(), f.sess, msgs)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return in.Call
}

func from(id event.ID) event.Message {
	return event.Message{
		Schema: event.SchemaVersion, ID: event.NewID(), TraceID: event.NewID(),
		Role: event.RoleUser, UserID: id, Text: "tell me something about the deploy",
	}
}

// A room adds no role and takes none away: every sender acts as itself, and a
// fallback to the session row, the room, is a guest.
func TestSenderInARoomKeepsItsOwnRole(t *testing.T) {
	f := newRoomFixture(t, "c1")
	ctx := context.Background()

	c := f.prepare(t, from(f.owner.ID))
	if c.UserID != f.owner.ID || c.Role != string(store.RoleAdmin) {
		t.Errorf("admin in a room: %q %q", c.UserID, c.Role)
	}
	c = f.prepare(t, from(f.user.ID))
	if c.UserID != f.user.ID || c.Role != string(store.RoleUser) {
		t.Errorf("user in a room: %q %q", c.UserID, c.Role)
	}
	c = f.prepare(t, from(f.owner.ID), from(f.guest.ID))
	if c.UserID != f.guest.ID || c.Role != string(store.RoleGuest) {
		t.Errorf("coalesced admin and pending: %q %q, want the guest", c.UserID, c.Role)
	}
	if c.Partition != "room/"+string(f.room.ID) {
		t.Errorf("partition = %q", c.Partition)
	}

	// Messages with no sender fall back to the room, pending or confirmed: it
	// grants nothing, and a confirmed room's role is empty.
	c = f.prepare(t, from(""))
	if c.Role != string(store.RoleGuest) {
		t.Errorf("fallback to a pending room: role %q", c.Role)
	}
	if _, err := f.e.daemon.Auth.Confirm(ctx, f.room.ID, "ops", "", f.owner.ID); err != nil {
		t.Fatal(err)
	}
	c = f.prepare(t, from(""))
	if c.Role != string(store.RoleGuest) {
		t.Errorf("fallback to a confirmed room: role %q", c.Role)
	}
}

// A group DM is a room: its turns read and write the room's partition and never
// a member's dm/ one.
func TestGroupDMNeverReadsAMembersPartition(t *testing.T) {
	f := newRoomFixture(t, "mpim1")
	ctx := context.Background()
	mem := f.e.daemon.App.Memory
	for partition, text := range map[string]string{
		UserPartition(f.owner.ID): "personal deploy secret",
		RoomPartition(f.room.ID):  "the room deploy schedule",
	} {
		if _, err := mem.Write(ctx, memory.Item{
			Layer: memory.LayerNote, Text: text, Visibility: memory.VisOrigin,
			Prov: memory.Provenance{Agent: "main", Partition: partition, UserID: f.owner.ID, Role: "admin", Time: time.Now()},
		}); err != nil {
			t.Fatal(err)
		}
	}
	in, err := f.e.daemon.prepare(ctx, f.sess, []event.Message{from(f.owner.ID)})
	if err != nil {
		t.Fatal(err)
	}
	var injected []string
	for _, inj := range in.Injections {
		injected = append(injected, inj.Text)
	}
	all := strings.Join(injected, "\n")
	if !strings.Contains(all, "the room deploy schedule") {
		t.Errorf("the room's memory was not read: %q", all)
	}
	if strings.Contains(all, "personal deploy secret") {
		t.Errorf("a member's dm/ partition leaked: %q", all)
	}
}

// Deleting a room forgets its room/ partition, and a turn caught in the delete
// writes no recall anywhere.
func TestDeleteRoomForgetsItsPartition(t *testing.T) {
	f := newRoomFixture(t, "c1")
	ctx := context.Background()
	d := f.e.daemon
	note := func(partition, text string, vis memory.Visibility) {
		t.Helper()
		if _, err := d.App.Memory.Write(ctx, memory.Item{
			Layer: memory.LayerNote, Text: text, Visibility: vis,
			Prov: memory.Provenance{Agent: "main", Partition: partition, UserID: f.owner.ID, Role: "admin", Time: time.Now()},
		}); err != nil {
			t.Fatal(err)
		}
	}
	note(RoomPartition(f.room.ID), "the room meets on mondays", memory.VisOrigin)
	note(RoomPartition(f.room.ID), "the office opens at nine", memory.VisShared)
	note(UserPartition(f.room.ID), "not the room's", memory.VisOrigin)
	note(UserPartition(f.owner.ID), "the owner's own", memory.VisOrigin)

	del, err := d.Auth.DeleteIdentity(ctx, f.room.ID, false, f.owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if del.Forgotten != 1 {
		t.Fatalf("forgotten = %d, want the one private room note", del.Forgotten)
	}
	left := map[string]bool{}
	rows, err := d.App.Store.DB().QueryContext(ctx, `SELECT text FROM memory_items`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck // test
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		left[s] = true
	}
	if left["the room meets on mondays"] || !left["the office opens at nine"] || !left["the owner's own"] {
		t.Fatalf("memory left = %v", left)
	}

	sess, err := d.App.Store.GetSession(ctx, f.sess.ID)
	if err != nil || sess.UserID != "" {
		t.Fatalf("session after the delete = %+v, %v", sess, err)
	}
	before := len(left)
	d.ingestRecall(ctx, sess, "a summary of a deleted room", false)
	var n int
	if err := d.App.Store.DB().QueryRowContext(ctx, `SELECT count(*) FROM memory_items`).Scan(&n); err != nil || n != before {
		t.Fatalf("recall written for an ownerless room: %d rows, %v", n, err)
	}
}

// An approval or a takeover in a room is shown when the turn acts as an owner or
// admin, read from the trace: the strictest coalesced sender must qualify, and
// no sender on the trace means no. A DM still goes by its session's user.
func TestTurnIsAdminInARoom(t *testing.T) {
	f := newRoomFixture(t, "c1")
	ctx := context.Background()
	d := f.e.daemon
	cs := &channels{d: d}
	resolved := func(trace event.ID, users ...event.ID) {
		t.Helper()
		for _, u := range users {
			d.App.Store.Emit(ctx, event.New(event.RouterResolved, trace, f.sess.ID, "", map[string]any{"user": string(u)}))
		}
	}
	for _, tc := range []struct {
		name  string
		users []event.ID
		want  bool
	}{
		{"an admin's turn", []event.ID{f.owner.ID}, true},
		{"a user's turn", []event.ID{f.user.ID}, false},
		{"a pending sender's turn", []event.ID{f.guest.ID}, false},
		{"the room itself", []event.ID{f.room.ID}, false},
		{"admin and pending coalesced", []event.ID{f.owner.ID, f.guest.ID}, false},
		{"pending and admin coalesced", []event.ID{f.guest.ID, f.owner.ID}, false},
		{"no sender on the trace", nil, false},
		{"a sender since deleted", []event.ID{"01J9Q4ABCDEFGHJKMNPQRSTVWX"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trace := event.NewID()
			resolved(trace, tc.users...)
			if got := cs.turnIsAdmin(ctx, f.sess, trace); got != tc.want {
				t.Fatalf("turnIsAdmin = %v, want %v", got, tc.want)
			}
		})
	}

	// A DM's session user decides, trace or no trace.
	dm := store.Session{Kind: store.SessionChat, UserID: f.owner.ID, Origin: event.Origin{ConversationKind: event.ConversationDM}}
	if !cs.turnIsAdmin(ctx, dm, "") {
		t.Fatal("an admin's DM")
	}
	dm.UserID = f.user.ID
	if cs.turnIsAdmin(ctx, dm, "") {
		t.Fatal("a user's DM")
	}
	// A task run keeps going by its creator, like before.
	task := store.Session{Kind: store.SessionTask, UserID: f.owner.ID, Origin: roomOrigin("c1")}
	if !cs.turnIsAdmin(ctx, task, "") {
		t.Fatal("an admin's task in a room")
	}
}

// A coalesced room turn's trace is the last message's, so prepare puts the
// earlier senders on it.
func TestPrepareTracesCoalescedRoomSenders(t *testing.T) {
	f := newRoomFixture(t, "c1")
	ctx := context.Background()
	d := f.e.daemon
	cs := &channels{d: d}

	first, last := from(f.guest.ID), from(f.owner.ID)
	d.App.Store.Emit(ctx, event.New(event.RouterResolved, last.TraceID, f.sess.ID, "", map[string]any{"user": string(f.owner.ID)}))
	if !cs.turnIsAdmin(ctx, f.sess, last.TraceID) {
		t.Fatal("the last sender alone is an admin")
	}
	f.prepare(t, first, last)
	if cs.turnIsAdmin(ctx, f.sess, last.TraceID) {
		t.Fatal("a pending sender coalesced into the turn did not count")
	}

	// A sender of the turn's own trace needs no second event.
	evs, err := d.App.Store.ListEvents(ctx, last.TraceID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Type == event.RouterResolved {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("router.resolved on the trace = %d, want the sender and the coalesced one", n)
	}
}

// A takeover in a room goes by the turn, not the session row.
func TestTakeoverNoticeInARoom(t *testing.T) {
	f := newRoomFixture(t, "c1")
	ctx := context.Background()
	d := f.e.daemon
	notice := func(trace event.ID) event.Event {
		return event.New(event.BrowserTakeoverRequest, trace, f.sess.ID, "", map[string]any{"agent": "main", "why": "login"})
	}

	ch := &fakeChannel{}
	cs := &channels{d: d, byName: map[string]channel.Channel{"fake": ch}}
	trace := event.NewID()
	d.App.Store.Emit(ctx, event.New(event.RouterResolved, trace, f.sess.ID, "", map[string]any{"user": string(f.user.ID)}))
	cs.takeoverNotice(ctx, notice(trace))
	if msgs := ch.messages(); len(msgs) != 0 {
		t.Fatalf("a user's turn was told to take over: %+v", msgs)
	}

	trace = event.NewID()
	d.App.Store.Emit(ctx, event.New(event.RouterResolved, trace, f.sess.ID, "", map[string]any{"user": string(f.owner.ID)}))
	cs.takeoverNotice(ctx, notice(trace))
	if msgs := ch.messages(); len(msgs) != 1 {
		t.Fatalf("an admin's turn got %d notices", len(msgs))
	}
}
