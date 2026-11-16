package agent

import (
	"context"
	"strings"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/store"
)

// One end-to-end run against the real SQLite store: the id scheme, the
// compaction point and ListMessages(after) must agree outside the fakes.
func TestCompactAgainstSQLite(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	sess, _, err := st.EnsureSession(ctx, store.Session{
		ID: event.NewID(), Key: "cli:compact", Agent: "test", Kind: store.SessionChat,
		Origin: event.Origin{Channel: "cli", ChannelType: "cli", Conversation: "c", ConversationKind: event.ConversationDM},
	})
	if err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	msgs := cpChat(30, 150)
	for _, m := range msgs {
		m.SessionID, m.TraceID = sess.ID, event.NewID()
		if _, err := st.AppendMessage(ctx, m, false); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	prov := &fakeProvider{name: "sum", steps: []step{cpSummaryStep("Goal\n- ship compaction")}}
	var recalled string
	c := &Compactor{
		Store:     st,
		Summarize: ModelRef{Model: provider.Model{Name: "sum", Context: 8192}, Provider: prov},
		Events:    event.Discard,
		Blobs: func(_ context.Context, text string) (string, error) {
			b, err := st.PutBlob(ctx, strings.NewReader(text), "text/plain")
			return b.ID, err
		},
		OnSummary: func(_ context.Context, _ store.Session, s string, _ bool) { recalled = s },
	}
	in := CompactInput{
		SessionID: sess.ID, TraceID: event.NewID(),
		Def: Definition{Name: "test"}, Model: provider.Model{Name: "chat", Context: 4096},
	}

	res, err := c.Compact(ctx, in)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !res.Compacted {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(recalled, "ship compaction") {
		t.Fatalf("OnSummary got %q", recalled)
	}

	after, err := st.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.CompactFails != 0 || after.SummaryMsgID == "" {
		t.Fatalf("session = %+v", after)
	}
	tail, err := st.ListMessages(ctx, sess.ID, after.SummaryMsgID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tail[0].Role != event.RoleSystem || tail[0].ID != res.SummaryMsgID {
		t.Fatalf("first kept message = %s/%s, want the summary %s", tail[0].ID, tail[0].Role, res.SummaryMsgID)
	}
	if len(tail) < 2 || tail[1].Role != event.RoleUser {
		t.Fatalf("tail does not start at a user turn: %+v", tail[1])
	}
	p, err := NewBuilder().Build(ctx, BuildInput{Def: in.Def, Model: in.Model, History: tail})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.NeedsCompaction {
		t.Fatalf("still needs compaction: history %d", p.Breakdown.History)
	}
	if !strings.HasPrefix(p.Messages[0].Text(), SummaryPrefix) {
		t.Fatalf("first prompt message = %q", firstRunes(p.Messages[0].Text(), 60))
	}
	// The whole transcript is still there: compaction moves a cursor, it
	// never deletes rows.
	all, err := st.ListMessages(ctx, sess.ID, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(msgs)+1 {
		t.Fatalf("%d rows, want %d", len(all), len(msgs)+1)
	}
}
