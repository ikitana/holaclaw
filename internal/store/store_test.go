package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/event"
)

func newStore(t *testing.T, opts ...Option) *SQLite {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir(), opts...)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s
}

// mustIdentity creates a confirmed user identity with role, or a pending one
// when role is empty.
func mustIdentity(t *testing.T, s *SQLite, name string, role Role) Identity {
	t.Helper()
	i := Identity{ID: event.NewID(), Name: name, Role: role}
	if role == "" {
		i.Status = StatusPending
	}
	if err := s.CreateIdentity(context.Background(), i); err != nil {
		t.Fatalf("create identity: %v", err)
	}
	got, err := s.GetIdentity(context.Background(), i.ID)
	if err != nil {
		t.Fatalf("get identity: %v", err)
	}
	return got
}

func mustSession(t *testing.T, s *SQLite, key string) Session {
	t.Helper()
	sess, _, err := s.EnsureSession(context.Background(), Session{
		Key: key, Agent: "main", Kind: SessionChat,
		Origin: event.Origin{Channel: "tg-main", ChannelType: "telegram", Conversation: "42", ConversationKind: event.ConversationDM},
	})
	if err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	return sess
}

func TestIdentityRows(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	u := mustIdentity(t, s, "ana", RoleOwner)
	if u.Role != RoleOwner || u.Name != "ana" || u.Kind != KindUser || u.Status != StatusConfirmed || u.Pending() {
		t.Fatalf("got %+v", u)
	}
	if u.CreatedAt.IsZero() {
		t.Fatal("created_at not set")
	}
	// A confirmed identity defaults to the user role.
	if err := s.CreateIdentity(ctx, Identity{Name: "bo"}); err != nil {
		t.Fatal(err)
	}

	list, err := s.ListIdentities(ctx)
	if err != nil || len(list) != 2 || list[1].Role != RoleUser || list[1].Status != StatusConfirmed {
		t.Fatalf("list identities = %+v, err %v", list, err)
	}

	if _, err := s.GetIdentity(ctx, event.NewID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing identity: %v", err)
	}
	if err := s.DeleteIdentity(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteIdentity(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	// A duplicate id is rejected by the primary key.
	other := mustIdentity(t, s, "cy", "")
	if err := s.CreateIdentity(ctx, Identity{ID: other.ID, Name: "dup", Role: RoleUser}); err == nil {
		t.Fatal("duplicate id accepted")
	}
	// The check constraints hold; no identity is a guest.
	for _, bad := range []Identity{
		{Name: "x", Role: Role("root")},
		{Name: "x", Role: RoleGuest},
		{Name: "x", Kind: IdentityKind("bot")},
		{Name: "x", Status: IdentityStatus("maybe")},
	} {
		if err := s.CreateIdentity(ctx, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

// A pending identity has no role, and a confirmed one always has one.
func TestPendingHasNoRole(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	p := mustIdentity(t, s, "guest", "")
	if !p.Pending() || p.Role != "" {
		t.Fatalf("got %+v", p)
	}
	if err := s.CreateIdentity(ctx, Identity{Name: "x", Status: StatusPending, Role: RoleUser}); err == nil {
		t.Fatal("pending identity with a role accepted")
	}
	if err := s.CreateIdentity(ctx, Identity{Name: "x", Role: RoleGuest}); err == nil || strings.Contains(err.Error(), "CHECK") {
		t.Fatalf("identity with role guest: err = %v, want a refusal before the schema", err)
	}
	// The schema refuses both shapes, whatever writes the row.
	for _, q := range []string{
		`INSERT INTO identities (id, name, role, status, created_at) VALUES ('a', 'x', 'user', 'pending', 1)`,
		`INSERT INTO identities (id, name, role, status, created_at) VALUES ('b', 'x', NULL, 'confirmed', 1)`,
		`INSERT INTO identities (id, name, role, status, created_at) VALUES ('c', 'x', NULL, 'disabled', 1)`,
		`UPDATE identities SET status = 'confirmed' WHERE id = '` + string(p.ID) + `'`,
		`UPDATE identities SET role = 'admin' WHERE id = '` + string(p.ID) + `'`,
	} {
		if _, err := s.DB().ExecContext(ctx, q); err == nil || !strings.Contains(err.Error(), "CHECK") {
			t.Fatalf("%s: err = %v, want a CHECK failure", q, err)
		}
	}
	// Confirming sets both in one statement.
	if _, err := s.DB().ExecContext(ctx, `UPDATE identities SET status = 'confirmed', role = 'user' WHERE id = ?`, string(p.ID)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetIdentity(ctx, p.ID); err != nil || got.Pending() || got.Role != RoleUser {
		t.Fatalf("got %+v, err %v", got, err)
	}
	// A disabled identity keeps its role.
	if _, err := s.DB().ExecContext(ctx, `UPDATE identities SET status = 'disabled' WHERE id = ?`, string(p.ID)); err != nil {
		t.Fatal(err)
	}
}

// A binding never exists without its identity: not without one, not on an
// identity that does not exist, and not after its identity is deleted.
func TestBindingNeedsIdentity(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	for q, want := range map[string]string{
		`INSERT INTO bindings (channel, alias, identity_id, created_at) VALUES ('tg', '1', NULL, 1)`:     "NOT NULL",
		`INSERT INTO bindings (channel, alias, identity_id, created_at) VALUES ('tg', '1', 'nobody', 1)`: "FOREIGN KEY",
	} {
		if _, err := s.DB().ExecContext(ctx, q); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want a %s failure", q, err, want)
		}
	}

	a := mustIdentity(t, s, "ana", RoleUser)
	mustLink(t, s, "tg", a, "1")
	if err := s.DeleteIdentity(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB().QueryRowContext(ctx, `SELECT count(*) FROM bindings`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("bindings left after deleting the identity: %d, err %v", n, err)
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	ana := mustIdentity(t, s, "ana", RoleOwner)

	in := Session{
		Key: "main/tg-main/42", Agent: "main", Kind: SessionChat, UserID: ana.ID, Title: "hi",
		Origin: event.Origin{Channel: "tg-main", ChannelType: "telegram", Conversation: "42", ConversationKind: event.ConversationDM},
	}
	sess, created, err := s.EnsureSession(ctx, in)
	if err != nil || !created {
		t.Fatalf("created = %v, err %v", created, err)
	}
	if sess.Origin.Conversation != "42" || sess.UserID != ana.ID || sess.Kind != SessionChat {
		t.Fatalf("got %+v", sess)
	}
	again, created, err := s.EnsureSession(ctx, Session{Key: in.Key, Agent: "other"})
	if err != nil || created {
		t.Fatalf("created = %v, err %v", created, err)
	}
	if again.ID != sess.ID || again.Agent != "main" {
		t.Fatalf("existing row replaced: %+v", again)
	}
	if _, _, err := s.EnsureSession(ctx, Session{}); err == nil {
		t.Fatal("empty key accepted")
	}

	if _, err := s.GetSession(ctx, event.NewID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing session: %v", err)
	}
	list, err := s.ListSessions(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, err %v", list, err)
	}

	summary := event.NewID()
	if err := s.UpdateSessionContext(ctx, sess.ID, "hash-1", summary, 2); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PrefixHash != "hash-1" || got.SummaryMsgID != summary || got.CompactFails != 2 {
		t.Fatalf("got %+v", got)
	}
	if err := s.UpdateSessionContext(ctx, event.NewID(), "", "", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing session: %v", err)
	}
	// A session keeps its row when the user goes away.
	if err := s.DeleteIdentity(ctx, ana.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetSession(ctx, sess.ID); err != nil || got.UserID != "" {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestTurns(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sess := mustSession(t, s, "main/tg-main/turns")

	token, gen, err := s.BeginTurn(ctx, sess.ID)
	if err != nil || token == "" || gen != 1 {
		t.Fatalf("token %q gen %d err %v", token, gen, err)
	}
	if _, _, err := s.BeginTurn(ctx, sess.ID); !errors.Is(err, ErrTurnActive) {
		t.Fatalf("err = %v, want ErrTurnActive", err)
	}
	if _, _, err := s.BeginTurn(ctx, event.NewID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := s.EndTurn(ctx, sess.ID, "wrong"); !errors.Is(err, ErrTurnMismatch) {
		t.Fatalf("err = %v, want ErrTurnMismatch", err)
	}
	if err := s.EndTurn(ctx, event.NewID(), token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if err := s.EndTurn(ctx, sess.ID, token); err != nil {
		t.Fatal(err)
	}
	// The generation is monotonic across turns.
	_, gen2, err := s.BeginTurn(ctx, sess.ID)
	if err != nil || gen2 != 2 {
		t.Fatalf("gen = %d, err %v", gen2, err)
	}

	// HealTurns clears what a crash left behind, once.
	healed, err := s.HealTurns(ctx)
	if err != nil || len(healed) != 1 || healed[0] != sess.ID {
		t.Fatalf("healed = %v, err %v", healed, err)
	}
	healed, err = s.HealTurns(ctx)
	if err != nil || len(healed) != 0 {
		t.Fatalf("healed = %v, err %v", healed, err)
	}
	if _, _, err := s.BeginTurn(ctx, sess.ID); err != nil {
		t.Fatalf("session not claimable after heal: %v", err)
	}
}

func msg(sessionID, traceID event.ID, role event.Role, text string) event.Message {
	return event.Message{
		Schema: event.SchemaVersion, ID: event.NewID(), SessionID: sessionID, TraceID: traceID,
		Role: role, Text: text, CreatedAt: time.Now().UTC(),
	}
}

func TestMessages(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sess := mustSession(t, s, "main/tg-main/msgs")
	trace := event.NewID()

	m1 := msg(sess.ID, trace, event.RoleUser, "hello")
	m1.Sender = "ana"
	inserted, err := s.AppendMessage(ctx, m1, false)
	if err != nil || !inserted {
		t.Fatalf("inserted = %v, err %v", inserted, err)
	}
	// Idempotency: same id, no second row.
	inserted, err = s.AppendMessage(ctx, m1, false)
	if err != nil || inserted {
		t.Fatalf("second append inserted = %v, err %v", inserted, err)
	}

	got, err := s.GetMessage(ctx, m1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "hello" || got.Sender != "ana" || got.Role != event.RoleUser || got.SessionID != sess.ID {
		t.Fatalf("round trip lost fields: %+v", got)
	}
	if _, err := s.GetMessage(ctx, event.NewID()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing message: %v", err)
	}
	if _, err := s.AppendMessage(ctx, event.Message{ID: event.NewID()}, false); err == nil {
		t.Fatal("message without session accepted")
	}
	if _, err := s.AppendMessage(ctx, msg(event.NewID(), trace, event.RoleUser, "x"), false); err == nil {
		t.Fatal("message for unknown session accepted")
	}

	m2 := msg(sess.ID, trace, event.RoleAssistant, "hi there")
	if _, err := s.AppendMessage(ctx, m2, false); err != nil {
		t.Fatal(err)
	}
	pending := msg(sess.ID, trace, event.RoleUser, "and one more")
	if _, err := s.AppendMessage(ctx, pending, true); err != nil {
		t.Fatal(err)
	}

	// Transcript: ordered, pending excluded.
	list, err := s.ListMessages(ctx, sess.ID, "", 0)
	if err != nil || len(list) != 2 || list[0].ID != m1.ID || list[1].ID != m2.ID {
		t.Fatalf("list = %+v, err %v", list, err)
	}
	if list, err = s.ListMessages(ctx, sess.ID, m1.ID, 0); err != nil || len(list) != 1 || list[0].ID != m2.ID {
		t.Fatalf("after: %+v, err %v", list, err)
	}
	if list, err = s.ListMessages(ctx, sess.ID, "", 1); err != nil || len(list) != 1 {
		t.Fatalf("limit: %+v, err %v", list, err)
	}

	// Mailbox.
	withPending, err := s.SessionsWithPending(ctx)
	if err != nil || len(withPending) != 1 || withPending[0] != sess.ID {
		t.Fatalf("pending sessions = %v, err %v", withPending, err)
	}
	taken, err := s.TakePending(ctx, sess.ID, false)
	if err != nil || len(taken) != 1 || taken[0].ID != pending.ID {
		t.Fatalf("taken = %+v, err %v", taken, err)
	}
	if taken, err = s.TakePending(ctx, sess.ID, false); err != nil || len(taken) != 0 {
		t.Fatalf("second take = %+v, err %v", taken, err)
	}
	if withPending, err = s.SessionsWithPending(ctx); err != nil || len(withPending) != 0 {
		t.Fatalf("pending sessions = %v, err %v", withPending, err)
	}
	// Consumed messages join the transcript.
	if list, err = s.ListMessages(ctx, sess.ID, "", 0); err != nil || len(list) != 3 {
		t.Fatalf("list after take = %+v, err %v", list, err)
	}

	trMsgs, err := s.ListTraceMessages(ctx, trace)
	if err != nil || len(trMsgs) != 3 {
		t.Fatalf("trace messages = %+v, err %v", trMsgs, err)
	}
}

// A message that arrives while a turn runs has an id older than that turn's
// answer, yet it sorts after the answer once the next turn takes it.
func TestTakePendingPlacesAfterTranscript(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sess := mustSession(t, s, "main/tg-main/pos")

	first := msg(sess.ID, event.NewID(), event.RoleUser, "rain in Sevilla?")
	if _, err := s.AppendMessage(ctx, first, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TakePending(ctx, sess.ID, false); err != nil {
		t.Fatal(err)
	}
	// The second question arrives mid-turn, before the answer is stored.
	second := msg(sess.ID, event.NewID(), event.RoleUser, "USD-EUR?")
	if _, err := s.AppendMessage(ctx, second, true); err != nil {
		t.Fatal(err)
	}
	answer := msg(sess.ID, first.TraceID, event.RoleAssistant, "no rain")
	if _, err := s.AppendMessage(ctx, answer, false); err != nil {
		t.Fatal(err)
	}
	if second.ID >= answer.ID {
		t.Fatal("fixture: the second question must have the older id")
	}

	taken, err := s.TakePending(ctx, sess.ID, false)
	if err != nil || len(taken) != 1 || taken[0].ID != second.ID || taken[0].Pos <= answer.ID {
		t.Fatalf("taken = %+v, err %v", taken, err)
	}
	list, err := s.ListMessages(ctx, sess.ID, "", 0)
	if err != nil || len(list) != 3 {
		t.Fatalf("list = %+v, err %v", list, err)
	}
	for i, want := range []event.ID{first.ID, answer.ID, second.ID} {
		if list[i].ID != want {
			t.Fatalf("list[%d] = %s %q, want %s", i, list[i].ID, list[i].Text, want)
		}
	}
	if list[2].Pos != taken[0].Pos || list[1].Pos != answer.ID {
		t.Fatalf("positions not filled: %+v", list)
	}
	got, err := s.GetMessage(ctx, second.ID)
	if err != nil || got.Pos != taken[0].Pos {
		t.Fatalf("get = %+v, err %v", got, err)
	}

	// The cursor resumes past the cursor message's place, not its id.
	after, err := s.ListMessages(ctx, sess.ID, answer.ID, 0)
	if err != nil || len(after) != 1 || after[0].ID != second.ID {
		t.Fatalf("after answer = %+v, err %v", after, err)
	}
	if after, err = s.ListMessages(ctx, sess.ID, second.ID, 0); err != nil || len(after) != 0 {
		t.Fatalf("after second = %+v, err %v", after, err)
	}

	// The newest message is the one placed last.
	if ok, err := s.RequeueLastUser(ctx, sess.ID); err != nil || !ok {
		t.Fatalf("requeue = %v, err %v", ok, err)
	}
	if taken, err = s.TakePending(ctx, sess.ID, false); err != nil || len(taken) != 1 || taken[0].ID != second.ID {
		t.Fatalf("requeued = %+v, err %v", taken, err)
	}
}

// perMention takes up to the first mention: chatter before it (queued only
// with reply = "always") rides along, the next mention waits for its own turn.
func TestTakePendingPerMention(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sess := mustSession(t, s, "main/tg-main/room/-100")
	var ids []event.ID
	for _, in := range []struct {
		text      string
		mentioned bool
	}{{"by the way", false}, {"rain in Sevilla?", true}, {"USD-EUR?", true}, {"thanks", false}} {
		m := msg(sess.ID, event.NewID(), event.RoleUser, in.text)
		m.Mentioned = in.mentioned
		if _, err := s.AppendMessage(ctx, m, true); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
	}
	for _, want := range [][]event.ID{{ids[0], ids[1]}, {ids[2]}, {ids[3]}, nil} {
		taken, err := s.TakePending(ctx, sess.ID, true)
		if err != nil || len(taken) != len(want) {
			t.Fatalf("taken = %+v, want %v, err %v", taken, want, err)
		}
		for i := range want {
			if taken[i].ID != want[i] {
				t.Fatalf("taken[%d] = %+v, want %s", i, taken[i], want[i])
			}
		}
	}
	if got, err := s.GetMessage(ctx, ids[1]); err != nil || !got.Mentioned {
		t.Fatalf("mention not stored: %+v, err %v", got, err)
	}
	if list, err := s.ListMessages(ctx, sess.ID, "", 0); err != nil || len(list) != 4 {
		t.Fatalf("list = %+v, err %v", list, err)
	}
}

// A clock behind the transcript still places the taken message after it.
func TestPosAfterClockBehind(t *testing.T) {
	last := event.NewIDAt(time.Now().Add(time.Hour))
	if p := posAfter(last); p <= last {
		t.Fatalf("posAfter(%s) = %s", last, p)
	}
}

// After the clock steps back, an answer stored with a fresh id still sorts
// after the question its turn took; an explicit Pos is kept as given.
func TestAppendMessageAfterClockStepBack(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sess := mustSession(t, s, "main/tg-main/stepback")

	// The transcript ends an hour ahead of the clock.
	ahead := msg(sess.ID, event.NewID(), event.RoleAssistant, "earlier answer")
	ahead.ID = event.NewIDAt(time.Now().Add(time.Hour))
	if _, err := s.AppendMessage(ctx, ahead, false); err != nil {
		t.Fatal(err)
	}
	q := msg(sess.ID, event.NewID(), event.RoleUser, "rain in Sevilla?")
	if _, err := s.AppendMessage(ctx, q, true); err != nil {
		t.Fatal(err)
	}
	taken, err := s.TakePending(ctx, sess.ID, false)
	if err != nil || len(taken) != 1 {
		t.Fatalf("taken = %+v, err %v", taken, err)
	}
	answer := msg(sess.ID, q.TraceID, event.RoleAssistant, "no rain")
	if _, err := s.AppendMessage(ctx, answer, false); err != nil {
		t.Fatal(err)
	}
	if answer.ID >= taken[0].Pos {
		t.Fatal("fixture: the answer's id must sort before the question's place")
	}
	early := msg(sess.ID, q.TraceID, event.RoleSystem, "[summary]")
	early.ID = event.NewIDAt(time.Now().Add(-time.Hour))
	early.Pos = early.ID
	if _, err := s.AppendMessage(ctx, early, false); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListMessages(ctx, sess.ID, "", 0)
	if err != nil || len(list) != 4 {
		t.Fatalf("list = %+v, err %v", list, err)
	}
	for i, want := range []event.ID{early.ID, ahead.ID, q.ID, answer.ID} {
		if list[i].ID != want {
			t.Fatalf("list[%d] = %s %q, want %s", i, list[i].ID, list[i].Text, want)
		}
	}
}

func TestEvents(t *testing.T) {
	ctx := context.Background()
	var logs bytes.Buffer
	s := newStore(t, WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	sess := mustSession(t, s, "main/tg-main/events")
	trace := event.NewID()

	e1 := event.New(event.ChannelReceived, trace, sess.ID, "", map[string]string{"channel": "tg-main"})
	e1.Duration = 3 * time.Millisecond
	s.Emit(ctx, e1)
	e2 := event.New(event.RunFinished, trace, sess.ID, e1.ID, nil)
	s.Emit(ctx, e2)
	// Ephemeral events are never persisted.
	s.Emit(ctx, event.New(event.ModelToken, trace, sess.ID, e1.ID, map[string]string{"t": "a"}))
	// Another trace, for the filters.
	otherTrace := event.NewID()
	s.Emit(ctx, event.New(event.RunStarted, otherTrace, "", "", nil))

	evs, err := s.ListEvents(ctx, trace)
	if err != nil || len(evs) != 2 {
		t.Fatalf("events = %+v, err %v", evs, err)
	}
	if evs[0].Type != event.ChannelReceived || evs[0].Duration != 3*time.Millisecond {
		t.Fatalf("got %+v", evs[0])
	}
	var attrs map[string]string
	if err := json.Unmarshal(evs[0].Attrs, &attrs); err != nil || attrs["channel"] != "tg-main" {
		t.Fatalf("attrs = %s, err %v", evs[0].Attrs, err)
	}
	if evs[1].ParentID != e1.ID || string(evs[1].Attrs) != "{}" {
		t.Fatalf("got %+v", evs[1])
	}

	all, err := s.ListEventsAfter(ctx, "", "", 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("all = %d, err %v", len(all), err)
	}
	page, err := s.ListEventsAfter(ctx, all[0].ID, "", 1)
	if err != nil || len(page) != 1 || page[0].ID != all[1].ID {
		t.Fatalf("page = %+v, err %v", page, err)
	}
	bySession, err := s.ListEventsAfter(ctx, "", sess.ID, 0)
	if err != nil || len(bySession) != 2 {
		t.Fatalf("by session = %+v, err %v", bySession, err)
	}
	// A failing write is logged, not returned: Emit has no error to check.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s.Emit(ctx, event.New(event.RunAborted, trace, sess.ID, "", nil))
	if !strings.Contains(logs.String(), "emit event") {
		t.Fatalf("failed emit was not logged: %q", logs.String())
	}
}

func TestRequeueLastUser(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sess, _, err := s.EnsureSession(ctx, Session{ID: event.NewID(), Key: "a/cli/dm/x", Agent: "a", Kind: SessionChat})
	if err != nil {
		t.Fatal(err)
	}
	add := func(role event.Role) {
		t.Helper()
		m := event.Message{Schema: 1, ID: event.NewID(), SessionID: sess.ID, TraceID: event.NewID(), Role: role, Text: "x", CreatedAt: time.Now()}
		if _, err := s.AppendMessage(ctx, m, false); err != nil {
			t.Fatal(err)
		}
	}
	add(event.RoleUser)
	add(event.RoleAssistant)
	if ok, err := s.RequeueLastUser(ctx, sess.ID); err != nil || ok {
		t.Fatalf("answered session requeued: %v %v", ok, err)
	}
	add(event.RoleUser)
	if ok, err := s.RequeueLastUser(ctx, sess.ID); err != nil || !ok {
		t.Fatalf("unanswered user message not requeued: %v %v", ok, err)
	}
	if ids, _ := s.SessionsWithPending(ctx); len(ids) != 1 {
		t.Fatalf("pending sessions = %v", ids)
	}
	if ok, _ := s.RequeueLastUser(ctx, sess.ID); ok {
		t.Fatal("requeued twice")
	}
}

func TestSetDerived(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sess, _, err := s.EnsureSession(ctx, Session{ID: event.NewID(), Key: "a/cli/dm/y", Agent: "a", Kind: SessionChat})
	if err != nil {
		t.Fatal(err)
	}
	att := event.Attachment{ID: event.NewID(), BlobID: "b", MIME: "audio/ogg", Kind: event.KindAudio}
	m := event.Message{Schema: 1, ID: event.NewID(), SessionID: sess.ID, TraceID: event.NewID(), Role: event.RoleUser, Attachments: []event.Attachment{att}, CreatedAt: time.Now()}
	if _, err := s.AppendMessage(ctx, m, false); err != nil {
		t.Fatal(err)
	}
	d := event.Derived{Text: "hola", By: event.DerivedByTranscribe}
	if err := s.SetDerived(ctx, m.ID, att.ID, d); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMessage(ctx, m.ID)
	if err != nil || got.Attachments[0].Derived == nil || *got.Attachments[0].Derived != d {
		t.Fatalf("derived not stored: %+v %v", got.Attachments, err)
	}
	if err := s.SetDerived(ctx, m.ID, event.NewID(), d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown attachment: %v", err)
	}
	if err := s.SetDerived(ctx, event.NewID(), att.ID, d); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown message: %v", err)
	}
}

func TestOpenEscapesOddDataDirs(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	for _, name := range []string{"a#1", "a#2", "p%20q", "q?x=1"} {
		dir := filepath.Join(base, name)
		s, err := Open(ctx, dir)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := s.CreateIdentity(ctx, Identity{Name: name, Role: RoleOwner}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_ = s.Close()
		if _, err := os.Stat(filepath.Join(dir, "holaclaw.db")); err != nil {
			t.Fatalf("%s: database is not inside its own directory: %v", name, err)
		}
	}
}

func TestPruneEvents(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	trace := event.NewID()
	old := event.New(event.RunStarted, trace, "", "", nil)
	old.Time = time.Now().Add(-48 * time.Hour)
	fresh := event.New(event.RunFinished, trace, "", "", nil)
	s.Emit(ctx, old)
	s.Emit(ctx, fresh)
	n, err := s.PruneEvents(ctx, time.Now().Add(-24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, err %v", n, err)
	}
	left, _ := s.ListEvents(ctx, trace)
	if len(left) != 1 || left[0].Type != event.RunFinished {
		t.Fatalf("left = %+v", left)
	}
}

func TestReceivedTrace(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	older, newer := event.NewID(), event.NewID()
	for _, tc := range []struct {
		trace   event.ID
		channel string
	}{{older, "slack"}, {newer, "slack"}, {event.NewID(), "other"}} {
		s.Emit(ctx, event.New(event.ChannelReceived, tc.trace, "", "", map[string]any{
			"channel": tc.channel, "platform_msg_id": "C1:100.1",
		}))
	}
	if got, err := s.ReceivedTrace(ctx, "slack", "C1:100.1"); err != nil || got != newer {
		t.Fatalf("ReceivedTrace = %s, %v, want %s", got, err, newer)
	}
	if _, err := s.ReceivedTrace(ctx, "slack", "C1:9.9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown message: err = %v, want ErrNotFound", err)
	}
}
