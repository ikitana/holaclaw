package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

func target() event.Origin {
	return event.Origin{
		Channel: "slack-test", ChannelType: "slack",
		Conversation: "C1", ConversationKind: event.ConversationRoom, Thread: "200.2",
	}
}

func TestSendPostsMrkdwnInThread(t *testing.T) {
	f := newFakeSlack(t)
	a := mappingAdapter(t, f, nil)

	rec, err := a.Send(context.Background(), channel.Outbound{
		Target:  target(),
		Message: event.Message{Text: "**hi** <there>"},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := f.callsFor("chat.postMessage")
	if len(calls) != 1 {
		t.Fatalf("postMessage calls = %d", len(calls))
	}
	body := calls[0].JSON
	if body["text"] != "*hi* &lt;there&gt;" {
		t.Fatalf("text = %v", body["text"])
	}
	if body["thread_ts"] != "200.2" || body["channel"] != "C1" || body["mrkdwn"] != true {
		t.Fatalf("body = %v", body)
	}
	if _, ok := body["blocks"]; ok {
		t.Fatal("prose must not use Block Kit")
	}
	if len(rec.PlatformMsgIDs) != 1 || rec.PlatformMsgIDs[0] != "C1:111.222" {
		t.Fatalf("receipt = %+v", rec)
	}
}

func TestSendSplitsLongText(t *testing.T) {
	f := newFakeSlack(t)
	a := mappingAdapter(t, f, nil)

	long := strings.TrimSpace(strings.Repeat("paragraph text here\n\n", 700))
	if _, err := a.Send(context.Background(), channel.Outbound{
		Target: target(), Message: event.Message{Text: long},
	}); err != nil {
		t.Fatal(err)
	}
	calls := f.callsFor("chat.postMessage")
	if len(calls) < 3 {
		t.Fatalf("postMessage calls = %d, want several chunks", len(calls))
	}
	for i, c := range calls {
		if txt, _ := c.JSON["text"].(string); len(txt) > chunkChars {
			t.Fatalf("chunk %d is %d chars", i, len(txt))
		}
	}
}

func TestSendUploadsAttachmentInThreeSteps(t *testing.T) {
	f := newFakeSlack(t)
	a := mappingAdapter(t, f, func(o *Options) {
		o.Blobs = memBlobs{data: map[string][]byte{"b1": []byte("filebytes")}}
	})

	rec, err := a.Send(context.Background(), channel.Outbound{
		Target: target(),
		Message: event.Message{
			Text:        "here you go",
			Attachments: []event.Attachment{{ID: "A1", BlobID: "b1", Name: "doc.txt", MIME: "text/plain", Kind: event.KindDocument}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Degraded) != 0 {
		t.Fatalf("degraded = %v", rec.Degraded)
	}
	get := f.callsFor("files.getUploadURLExternal")
	if len(get) != 1 || get[0].Form.Get("filename") != "doc.txt" || get[0].Form.Get("length") != "9" {
		t.Fatalf("getUploadURLExternal = %+v", get)
	}
	put := f.callsFor("upload")
	if len(put) != 1 || put[0].Body != "filebytes" {
		t.Fatalf("upload POST = %+v", put)
	}
	done := f.callsFor("files.completeUploadExternal")
	if len(done) != 1 {
		t.Fatalf("completeUploadExternal calls = %d", len(done))
	}
	body := done[0].JSON
	if body["channel_id"] != "C1" || body["thread_ts"] != "200.2" {
		t.Fatalf("complete body = %v", body)
	}
	if body["initial_comment"] != "here you go" {
		t.Fatalf("short text should ride as initial_comment, got %v", body["initial_comment"])
	}
	if n := len(f.callsFor("chat.postMessage")); n != 0 {
		t.Fatalf("postMessage called %d times, want 0", n)
	}
	files, _ := body["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("files = %v", body["files"])
	}
}

func TestSendDegradesOnUploadFailure(t *testing.T) {
	f := newFakeSlack(t)
	f.set("files.getUploadURLExternal", fail("upload_failed"))
	a := mappingAdapter(t, f, func(o *Options) {
		o.Blobs = memBlobs{data: map[string][]byte{"b1": []byte("filebytes")}}
	})

	rec, err := a.Send(context.Background(), channel.Outbound{
		Target: target(),
		Message: event.Message{
			Text:        "here you go",
			Attachments: []event.Attachment{{ID: "A1", BlobID: "b1", Name: "doc.txt"}},
		},
	})
	if err != nil {
		t.Fatalf("attachment failure must not fail the send: %v", err)
	}
	if len(rec.Degraded) != 1 || !strings.Contains(rec.Degraded[0], "doc.txt") {
		t.Fatalf("degraded = %v", rec.Degraded)
	}
	// the text that would have been the file comment still goes out
	if n := len(f.callsFor("chat.postMessage")); n != 1 {
		t.Fatalf("postMessage calls = %d, want 1", n)
	}
	assertNoTokens(t, rec.Degraded[0])
}

func TestSendDegradesOversizeAttachment(t *testing.T) {
	f := newFakeSlack(t)
	big := make([]byte, maxUploadBytes+1)
	a := mappingAdapter(t, f, func(o *Options) {
		o.Blobs = memBlobs{data: map[string][]byte{"b1": big}}
	})

	rec, err := a.Send(context.Background(), channel.Outbound{
		Target:  target(),
		Message: event.Message{Attachments: []event.Attachment{{ID: "A1", BlobID: "b1", Name: "huge.bin"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Degraded) != 1 || !strings.Contains(rec.Degraded[0], "huge.bin") {
		t.Fatalf("degraded = %v", rec.Degraded)
	}
	if n := len(f.callsFor("files.getUploadURLExternal")); n != 0 {
		t.Fatalf("oversize file still asked for an upload url (%d calls)", n)
	}
}

func TestSendMapsOKFalseErrors(t *testing.T) {
	cases := []struct {
		code string
		want error
	}{
		{"channel_not_found", ErrChannelNotFound},
		{"not_in_channel", ErrNotInChannel},
		{"invalid_auth", ErrInvalidAuth},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			f := newFakeSlack(t)
			f.set("chat.postMessage", fail(tc.code))
			a := mappingAdapter(t, f, nil)
			_, err := a.Send(context.Background(), channel.Outbound{Target: target(), Message: event.Message{Text: "hi"}})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			var se *Error
			if !errors.As(err, &se) || se.Code != tc.code || se.Op != "chat.postMessage" {
				t.Fatalf("typed error = %+v", se)
			}
			assertNoTokens(t, err.Error())
		})
	}
}

func TestLimits(t *testing.T) {
	f := newFakeSlack(t)
	a, _ := newAdapter(t, f, nil)
	l := a.Limits()
	if l.TextChars != 4000 || l.CaptionChars != 0 || l.MaxFileBytes != 1<<30 {
		t.Fatalf("limits = %+v", l)
	}
	if len(l.Kinds) != 4 {
		t.Fatalf("kinds = %v", l.Kinds)
	}
	if a.Name() != "slack-test" || a.Type() != "slack" {
		t.Fatalf("name/type = %q/%q", a.Name(), a.Type())
	}
}

func TestPromptApprovalSendsButtons(t *testing.T) {
	f := newFakeSlack(t)
	a := mappingAdapter(t, f, nil)

	if err := a.PromptApproval(context.Background(), target(), "AP1", "Run `rm`?", []string{"allow", "deny"}); err != nil {
		t.Fatal(err)
	}
	calls := f.callsFor("chat.postMessage")
	if len(calls) != 1 {
		t.Fatalf("postMessage calls = %d", len(calls))
	}
	if txt, _ := calls[0].JSON["text"].(string); txt == "" {
		t.Fatal("no fallback text")
	}
	blocks, _ := calls[0].JSON["blocks"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %v", blocks)
	}
	actions, _ := blocks[1].(map[string]any)
	elements, _ := actions["elements"].([]any)
	if len(elements) != 2 {
		t.Fatalf("elements = %v", elements)
	}
	first, _ := elements[0].(map[string]any)
	if first["action_id"] != approvalActionID || first["value"] != "AP1:allow" {
		t.Fatalf("button = %v", first)
	}
}

func TestInteractiveApprovalFlow(t *testing.T) {
	f := newFakeSlack(t)
	type decision struct {
		id      event.ID
		choice  string
		aliases []string
	}
	got := make(chan decision, 1)
	a := mappingAdapter(t, f, func(o *Options) {
		o.OnApproval = func(_ context.Context, id event.ID, choice string, aliases []string) {
			got <- decision{id, choice, aliases}
		}
	})

	payload := map[string]any{
		"type":    "block_actions",
		"user":    map[string]any{"id": "U2", "name": "bob"},
		"team":    map[string]any{"id": "T1"},
		"channel": map[string]any{"id": "C1"},
		"message": map[string]any{"ts": "300.1", "text": "Run rm?"},
		"actions": []any{map[string]any{"action_id": approvalActionID, "value": "AP1:allow"}},
	}
	a.handleInteractive(context.Background(), rawJSON(t, payload))

	select {
	case d := <-got:
		if d.id != "AP1" || d.choice != "allow" || len(d.aliases) != 1 || d.aliases[0] != "slack:T1:U2" {
			t.Fatalf("decision = %+v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("OnApproval never called")
	}

	upd := f.callsFor("chat.update")
	if len(upd) != 1 {
		t.Fatalf("chat.update calls = %d", len(upd))
	}
	body := upd[0].JSON
	if body["ts"] != "300.1" || body["channel"] != "C1" {
		t.Fatalf("update body = %v", body)
	}
	txt, _ := body["text"].(string)
	if !strings.Contains(txt, "allow") || !strings.Contains(txt, "Run rm?") {
		t.Fatalf("decision text = %q", txt)
	}
	blocks, _ := body["blocks"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("buttons not removed: %v", blocks)
	}
	raw, _ := json.Marshal(blocks)
	if strings.Contains(string(raw), "actions") {
		t.Fatalf("actions block still present: %s", raw)
	}
}

func TestInteractiveIgnoresForeignAction(t *testing.T) {
	f := newFakeSlack(t)
	called := false
	a := mappingAdapter(t, f, func(o *Options) {
		o.OnApproval = func(context.Context, event.ID, string, []string) { called = true }
	})
	payload := map[string]any{
		"type":    "block_actions",
		"actions": []any{map[string]any{"action_id": "someone_else", "value": "x:y"}},
	}
	a.handleInteractive(context.Background(), rawJSON(t, payload))
	if called {
		t.Fatal("foreign action triggered an approval")
	}
	if n := len(f.callsFor("chat.update")); n != 0 {
		t.Fatalf("chat.update called %d times", n)
	}
}

// mentionTarget is a top-level mention in C1 that the reply answers.
func mentionTarget() event.Origin {
	return event.Origin{
		Channel: "slack-test", ChannelType: "slack",
		Conversation: "C1", ConversationKind: event.ConversationRoom, PlatformMsgID: "C1:200.2",
	}
}

func TestSendReplyToThreadsOnTheMention(t *testing.T) {
	f := newFakeSlack(t)
	a := mappingAdapter(t, f, func(o *Options) {
		o.Blobs = memBlobs{data: map[string][]byte{"b1": []byte("filebytes")}}
	})
	long := strings.TrimSpace(strings.Repeat("paragraph text here\n\n", 700))
	if _, err := a.Send(context.Background(), channel.Outbound{
		Target: mentionTarget(),
		Message: event.Message{
			Text:        long,
			Attachments: []event.Attachment{{ID: "A1", BlobID: "b1", Name: "doc.txt"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Slack's reply is the thread, so every chunk and file goes into it.
	calls := f.callsFor("chat.postMessage")
	if len(calls) < 2 {
		t.Fatalf("postMessage calls = %d, want several chunks", len(calls))
	}
	for i, c := range calls {
		if c.JSON["thread_ts"] != "200.2" {
			t.Fatalf("chunk %d thread_ts = %v", i, c.JSON["thread_ts"])
		}
	}
	if done := f.callsFor("files.completeUploadExternal"); len(done) != 1 || done[0].JSON["thread_ts"] != "200.2" {
		t.Fatalf("complete = %+v", done)
	}
}

func TestSendReplyToKeepsTheUsersThread(t *testing.T) {
	f := newFakeSlack(t)
	a := mappingAdapter(t, f, nil)
	tg := mentionTarget()
	tg.Thread, tg.PlatformMsgID = "50.0", "C1:60.0"
	if _, err := a.Send(context.Background(), channel.Outbound{Target: tg, Message: event.Message{Text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if got := f.callsFor("chat.postMessage")[0].JSON["thread_ts"]; got != "50.0" {
		t.Fatalf("thread_ts = %v, want the user's thread", got)
	}
}

func TestSendReplyToLeftOut(t *testing.T) {
	for name, id := range map[string]string{"no message": "", "another channel": "C9:200.2", "not a slack id": "client:01J"} {
		t.Run(name, func(t *testing.T) {
			f := newFakeSlack(t)
			a := mappingAdapter(t, f, nil)
			tg := mentionTarget()
			tg.PlatformMsgID = id
			if _, err := a.Send(context.Background(), channel.Outbound{Target: tg, Message: event.Message{Text: "hi"}}); err != nil {
				t.Fatal(err)
			}
			if _, ok := f.callsFor("chat.postMessage")[0].JSON["thread_ts"]; ok {
				t.Fatalf("plain reply carries thread_ts: %v", f.callsFor("chat.postMessage")[0].JSON)
			}
		})
	}
}

func TestSendReplyToMentionGone(t *testing.T) {
	f := newFakeSlack(t)
	f.set("chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		calls := f.callsFor("chat.postMessage")
		if calls[len(calls)-1].JSON["thread_ts"] != nil {
			fail("thread_not_found")(w, r)
			return
		}
		f.ok(w, `{"channel":"C1","ts":"111.222"}`)
	})
	a := mappingAdapter(t, f, nil)
	long := strings.TrimSpace(strings.Repeat("paragraph text here\n\n", 700))
	rec, err := a.Send(context.Background(), channel.Outbound{Target: mentionTarget(), Message: event.Message{Text: long}})
	if err != nil {
		t.Fatalf("send = %v", err)
	}
	calls := f.callsFor("chat.postMessage")
	// One refused threaded attempt, then every chunk plain in the channel.
	if len(rec.PlatformMsgIDs) < 2 || len(calls) != len(rec.PlatformMsgIDs)+1 {
		t.Fatalf("receipt = %d ids, calls = %d", len(rec.PlatformMsgIDs), len(calls))
	}
}

func TestSendUserThreadGoneStillFails(t *testing.T) {
	f := newFakeSlack(t)
	f.set("chat.postMessage", fail("thread_not_found"))
	a := mappingAdapter(t, f, nil)
	_, err := a.Send(context.Background(), channel.Outbound{Target: target(), Message: event.Message{Text: "hi"}})
	if err == nil || len(f.callsFor("chat.postMessage")) != 1 {
		t.Fatalf("err = %v, calls = %d", err, len(f.callsFor("chat.postMessage")))
	}
}
