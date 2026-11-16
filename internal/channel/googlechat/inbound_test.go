package googlechat

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

func one(t *testing.T, envs []event.Envelope) event.Envelope {
	t.Helper()
	if len(envs) != 1 {
		t.Fatalf("envelopes = %d, want 1", len(envs))
	}
	return envs[0]
}

func TestDMMapping(t *testing.T) {
	f := newFake(t)
	a, _ := newAdapter(t, f, nil)
	env := one(t, deliver(t, a, msgEvent("spaces/D1", "DIRECT_MESSAGE", "m1", t0, "  hello  ", nil)))
	o := env.Origin
	if o.Channel != "gchat" || o.ChannelType != "googlechat" || o.Conversation != "spaces/D1" ||
		o.ConversationKind != event.ConversationDM || o.Thread != "" || o.PlatformMsgID != "spaces/D1/messages/m1" {
		t.Fatalf("origin = %+v", o)
	}
	if !env.Mentioned {
		t.Error("a DM addresses the app")
	}
	if env.Message.Text != "hello" || !env.Message.CreatedAt.Equal(t0) || env.Message.Role != event.RoleUser {
		t.Errorf("message = %+v", env.Message)
	}
	s := env.Sender
	if strings.Join(s.Aliases, ",") != "users/111,alice@example.com" || s.DisplayName != "Alice" || s.FromSelf {
		t.Errorf("sender = %+v", s)
	}
	if len(f.records("ack")) != 1 {
		t.Error("message not acked")
	}
}

func TestLegacyDMForms(t *testing.T) {
	for name, space := range map[string]map[string]any{
		"type DM":         {"name": "spaces/D2", "type": "DM"},
		"singleUserBotDm": {"name": "spaces/D3", "singleUserBotDm": true},
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := newAdapter(t, newFake(t), nil)
			ev := msgEvent("x", "", "m1", t0, "hi", nil)
			ev["space"] = space
			env := one(t, deliver(t, a, ev))
			if env.Origin.ConversationKind != event.ConversationDM {
				t.Fatalf("kind = %s", env.Origin.ConversationKind)
			}
		})
	}
}

func TestGroupMentionThreadAndArgumentText(t *testing.T) {
	a, _ := newAdapter(t, newFake(t), nil)
	mention := func(m map[string]any) {
		m["argumentText"] = "  what's up?"
		m["annotations"] = []any{map[string]any{
			"type":        "USER_MENTION",
			"userMention": map[string]any{"user": map[string]any{"name": "users/999", "type": "BOT"}, "type": "MENTION"},
		}}
	}
	env := one(t, deliver(t, a, msgEvent("spaces/S1", "SPACE", "m1", t0, "@holaclaw what's up?", mention)))
	if env.Origin.ConversationKind != event.ConversationRoom || !env.Mentioned {
		t.Fatalf("kind=%s mentioned=%v", env.Origin.ConversationKind, env.Mentioned)
	}
	if env.Message.Text != "what's up?" {
		t.Errorf("text = %q, want argumentText trimmed", env.Message.Text)
	}
	if env.Origin.Thread != "" {
		t.Errorf("a top-level message has no Thread, got %q", env.Origin.Thread)
	}

	reply := func(m map[string]any) { mention(m); m["threadReply"] = true }
	env = one(t, deliver(t, a, msgEvent("spaces/S1", "SPACE", "m2", t0, "@holaclaw x", reply)))
	if env.Origin.Thread != "spaces/S1/threads/tm2" {
		t.Errorf("thread reply Thread = %q", env.Origin.Thread)
	}
}

func TestGroupWithoutAppMention(t *testing.T) {
	a, _ := newAdapter(t, newFake(t), nil)
	other := func(m map[string]any) {
		m["annotations"] = []any{map[string]any{
			"type":        "USER_MENTION",
			"userMention": map[string]any{"user": map[string]any{"name": "users/222", "type": "HUMAN"}},
		}}
	}
	env := one(t, deliver(t, a, msgEvent("spaces/S1", "GROUP_CHAT", "m1", t0, "@bob hi", other)))
	if env.Mentioned {
		t.Error("a mention of a person is not a mention of the app")
	}
	if env.Message.Text != "@bob hi" {
		t.Errorf("without argumentText the text is used, got %q", env.Message.Text)
	}
	slash := func(m map[string]any) { m["slashCommand"] = map[string]any{"commandId": "1"} }
	if env := one(t, deliver(t, a, msgEvent("spaces/S1", "SPACE", "m2", t0, "/help", slash))); !env.Mentioned {
		t.Error("a slash command addresses the app")
	}
}

func TestBotSenderIsFromSelf(t *testing.T) {
	a, _ := newAdapter(t, newFake(t), nil)
	bot := func(m map[string]any) {
		m["sender"] = map[string]any{"name": "users/999", "type": "BOT", "displayName": "Bot"}
	}
	env := one(t, deliver(t, a, msgEvent("spaces/S1", "SPACE", "m1", t0, "x", bot)))
	if !env.Sender.FromSelf || strings.Join(env.Sender.Aliases, ",") != "users/999" {
		t.Fatalf("sender = %+v", env.Sender)
	}
}

func TestNoSenderDroppedWithReason(t *testing.T) {
	a, logs := newAdapter(t, newFake(t), nil)
	ev := msgEvent("spaces/S1", "SPACE", "m1", t0, "secret words", func(m map[string]any) { delete(m, "sender") })
	delete(ev, "user")
	if envs := deliver(t, a, ev); len(envs) != 0 {
		t.Fatalf("envelopes = %d, want none", len(envs))
	}
	if !strings.Contains(logs.String(), "no sender") {
		t.Fatalf("drop reason not logged:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "secret words") {
		t.Fatalf("message content reached the log:\n%s", logs.String())
	}
}

func TestBatchSortedByCreateTime(t *testing.T) {
	a, _ := newAdapter(t, newFake(t), nil)
	envs := deliver(t, a,
		msgEvent("spaces/D1", "DIRECT_MESSAGE", "c", t0.Add(2*time.Second), "c", nil),
		msgEvent("spaces/D1", "DIRECT_MESSAGE", "a", t0, "a", nil),
		msgEvent("spaces/D1", "DIRECT_MESSAGE", "b", t0.Add(time.Second), "b", nil),
	)
	var got []string
	for _, e := range envs {
		got = append(got, e.Message.Text)
	}
	if strings.Join(got, "") != "abc" {
		t.Fatalf("order = %v", got)
	}
}

func TestTimestampObjectForm(t *testing.T) {
	a, _ := newAdapter(t, newFake(t), nil)
	ev := msgEvent("spaces/D1", "DIRECT_MESSAGE", "m1", t0, "x", func(m map[string]any) {
		m["createTime"] = map[string]any{"seconds": t0.Unix(), "nanos": 5}
	})
	env := one(t, deliver(t, a, ev))
	if !env.Message.CreatedAt.Equal(t0.Add(5)) {
		t.Fatalf("created = %v", env.Message.CreatedAt)
	}
}

func TestAddedToSpace(t *testing.T) {
	f := newFake(t)
	var mu sync.Mutex
	var joined []string
	a, _ := newAdapter(t, f, func(o *Options) {
		o.OnJoined = func(_ context.Context, space string, by []string) {
			mu.Lock()
			defer mu.Unlock()
			joined = append(joined, space+"|"+strings.Join(by, ","))
		}
	})
	added := map[string]any{
		"type": "ADDED_TO_SPACE", "eventTime": t0.Format(time.RFC3339),
		"space": map[string]any{"name": "spaces/S1", "spaceType": "SPACE"}, "user": alice,
	}
	if envs := deliver(t, a, added); len(envs) != 0 {
		t.Fatal("a plain join is no envelope")
	}
	// Added through an @-mention: the join and the message.
	withMsg := msgEvent("spaces/S2", "SPACE", "m1", t0, "@holaclaw code", func(m map[string]any) { m["argumentText"] = " code" })
	withMsg["type"] = "ADDED_TO_SPACE"
	env := one(t, deliver(t, a, withMsg))
	if env.Message.Text != "code" || env.Origin.Conversation != "spaces/S2" {
		t.Fatalf("env = %+v", env)
	}
	// Redelivered: no second join, no second envelope.
	if envs := deliver(t, a, withMsg); len(envs) != 0 {
		t.Fatal("redelivery produced an envelope")
	}
	// A DM is not a space to approve.
	dm := map[string]any{"type": "ADDED_TO_SPACE", "space": map[string]any{"name": "spaces/D9", "spaceType": "DIRECT_MESSAGE"}, "user": alice}
	deliver(t, a, dm)

	mu.Lock()
	defer mu.Unlock()
	want := "spaces/S1|users/111,alice@example.com;spaces/S2|users/111,alice@example.com"
	if strings.Join(joined, ";") != want {
		t.Fatalf("joined = %v", joined)
	}
	if acks := len(f.records("ack")); acks != 4 {
		t.Fatalf("acks = %d, want 4", acks)
	}
}

func TestRemovedFromSpaceAcked(t *testing.T) {
	f := newFake(t)
	a, _ := newAdapter(t, f, nil)
	envs := deliver(t, a, map[string]any{"type": "REMOVED_FROM_SPACE", "space": map[string]any{"name": "spaces/S1"}, "user": alice})
	if len(envs) != 0 || len(f.records("ack")) != 1 {
		t.Fatalf("envs=%d acks=%d", len(envs), len(f.records("ack")))
	}
}

func withAttachment(atts ...map[string]any) func(m map[string]any) {
	return func(m map[string]any) {
		list := make([]any, len(atts))
		for i, x := range atts {
			list[i] = x
		}
		m["attachment"] = list
	}
}

func TestUploadedAttachmentDownloaded(t *testing.T) {
	f := newFake(t)
	f.media["res/abc=="] = []byte("png-bytes")
	var gotMIME string
	a, _ := newAdapter(t, f, func(o *Options) {
		put := o.PutBlob
		o.PutBlob = func(ctx context.Context, r io.Reader, m string) (string, int64, error) {
			gotMIME = m
			return put(ctx, r, m)
		}
	})
	att := map[string]any{"name": "spaces/D1/messages/m1/attachments/a1", "contentName": "../pic.png", "contentType": "image/png",
		"source": "UPLOADED_CONTENT", "attachmentDataRef": map[string]any{"resourceName": "res/abc=="}}
	env := one(t, deliver(t, a, msgEvent("spaces/D1", "DIRECT_MESSAGE", "m1", t0, "look", withAttachment(att))))
	if len(env.Message.Attachments) != 1 {
		t.Fatalf("attachments = %+v, text %q", env.Message.Attachments, env.Message.Text)
	}
	got := env.Message.Attachments[0]
	if got.Name != "pic.png" || got.MIME != "image/png" || got.Kind != event.KindImage || got.Size != 9 || got.BlobID != "blob-9" || gotMIME != "image/png" {
		t.Fatalf("attachment = %+v", got)
	}
	r := f.records("media")[0]
	if r.Query.Get("alt") != "media" || !strings.HasPrefix(r.Auth, "Bearer ya29.") {
		t.Fatalf("media request = %+v", r)
	}
	if env.Message.Text != "look" {
		t.Errorf("text = %q", env.Message.Text)
	}
}

func TestAttachmentNotes(t *testing.T) {
	f := newFake(t)
	f.media["big"] = []byte(strings.Repeat("x", 11))
	f.media["exact"] = []byte(strings.Repeat("x", 10))
	a, _ := newAdapter(t, f, func(o *Options) { o.MaxDownload = 10 })
	ref := func(name, res string) map[string]any {
		return map[string]any{"contentName": name, "contentType": "text/plain", "attachmentDataRef": map[string]any{"resourceName": res}}
	}
	drive := map[string]any{"contentName": "plan.gdoc", "source": "DRIVE_FILE", "driveDataRef": map[string]any{"driveFileId": "F1"}}
	env := one(t, deliver(t, a, msgEvent("spaces/D1", "DIRECT_MESSAGE", "m1", t0, "files", withAttachment(
		ref("big.txt", "big"), ref("exact.txt", "exact"), drive, ref("gone.txt", "missing"), ref("evil.txt", "a/../../spaces"),
	))))
	if len(env.Message.Attachments) != 1 || env.Message.Attachments[0].Name != "exact.txt" {
		t.Fatalf("attachments = %+v", env.Message.Attachments)
	}
	want := "files\n[file too large: big.txt]\n[Drive file not downloaded: plan.gdoc]\n[file unavailable: gone.txt]\n[file unavailable: evil.txt]"
	if env.Message.Text != want {
		t.Fatalf("text =\n%s\nwant\n%s", env.Message.Text, want)
	}
	for _, r := range f.records("media") {
		if strings.Contains(r.Path, "..") || strings.Contains(r.Path, "spaces") {
			t.Fatalf("path traversal reached the server: %s", r.Path)
		}
	}
}

// TestDuplicateAcked: a message core already handled is acked like a stored
// one, so Pub/Sub does not redeliver it.
func TestDuplicateAcked(t *testing.T) {
	f := newFake(t)
	a, _ := newAdapter(t, f, nil)
	var msg pulledMessage
	msg.AckID = "ack-0"
	msg.Message.Data = encode(msgEvent("spaces/D1", "DIRECT_MESSAGE", "m1", t0, "hello", nil))
	calls := 0
	a.handleBatch(context.Background(), func(context.Context, event.Envelope) error {
		calls++
		return channel.ErrDuplicate
	}, []pulledMessage{msg})
	if calls != 1 || len(f.records("ack")) != 1 {
		t.Fatalf("calls=%d acks=%d, want 1 and 1", calls, len(f.records("ack")))
	}
}
