package slack

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

func eventsPayloadFor(eventID string, ev map[string]any) map[string]any {
	return map[string]any{"event_id": eventID, "team_id": "T1", "event": ev}
}

func dmEvent(text string) map[string]any {
	return map[string]any{
		"type": "message", "channel": "D1", "channel_type": "im",
		"user": "U2", "text": text, "ts": "100.1",
	}
}

func channelEvent(text string, extra map[string]any) map[string]any {
	ev := map[string]any{
		"type": "message", "channel": "C1", "channel_type": "channel",
		"user": "U2", "text": text, "ts": "200.2",
	}
	for k, v := range extra {
		ev[k] = v
	}
	return ev
}

func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// deliver runs one events_api payload through the adapter and returns every
// envelope that reached core.
func deliver(t *testing.T, a *Adapter, payloads ...map[string]any) []event.Envelope {
	t.Helper()
	var got []event.Envelope
	for _, p := range payloads {
		a.handleEventsAPI(context.Background(), func(_ context.Context, e event.Envelope) error {
			got = append(got, e)
			return nil
		}, rawJSON(t, p))
	}
	return got
}

func mappingAdapter(t *testing.T, f *fakeSlack, tweak func(*Options)) *Adapter {
	t.Helper()
	a, _ := newAdapter(t, f, tweak)
	a.setIdentity("UBOT", "T1")
	return a
}

func TestDirectMessageMapping(t *testing.T) {
	f := newFakeSlack(t)
	a := mappingAdapter(t, f, nil)

	got := deliver(t, a, eventsPayloadFor("Ev1", dmEvent("hello there")))
	if len(got) != 1 {
		t.Fatalf("delivered %d envelopes", len(got))
	}
	e := got[0]
	if e.Origin.ConversationKind != event.ConversationDM || e.Origin.Conversation != "D1" {
		t.Fatalf("origin = %+v", e.Origin)
	}
	if e.Origin.Thread != "" {
		t.Fatalf("dm thread = %q, want empty", e.Origin.Thread)
	}
	if e.Origin.PlatformMsgID != "D1:100.1" || e.Origin.ChannelType != "slack" || e.Origin.Channel != "slack-test" {
		t.Fatalf("origin = %+v", e.Origin)
	}
	if !e.Mentioned {
		t.Fatal("a DM is always addressed to the bot")
	}
	if len(e.Sender.Aliases) != 1 || e.Sender.Aliases[0] != "slack:T1:U2" {
		t.Fatalf("aliases = %v", e.Sender.Aliases)
	}
	if e.Sender.DisplayName != "Alice" {
		t.Fatalf("display name = %q", e.Sender.DisplayName)
	}
	if e.Message.Text != "hello there" || e.Message.Role != event.RoleUser {
		t.Fatalf("message = %+v", e.Message)
	}
	if e.Message.CreatedAt.Unix() != 100 {
		t.Fatalf("created at = %v", e.Message.CreatedAt)
	}
}

func TestChannelMentionStaysInChannelAndStripsToken(t *testing.T) {
	f := newFakeSlack(t)
	a := mappingAdapter(t, f, nil)

	got := deliver(t, a, eventsPayloadFor("Ev1", channelEvent("<@UBOT> ping <@U9>", nil)))
	if len(got) != 1 {
		t.Fatalf("delivered %d envelopes", len(got))
	}
	e := got[0]
	if e.Origin.ConversationKind != event.ConversationRoom {
		t.Fatalf("kind = %q", e.Origin.ConversationKind)
	}
	if !e.Mentioned {
		t.Fatal("mention not detected")
	}
	if e.Origin.Thread != "" || e.Origin.ThreadRoot != "" || e.Origin.PlatformMsgID != "C1:200.2" {
		t.Fatalf("origin = %+v, want the channel session and the mention's id", e.Origin)
	}
	if e.Message.Text != "ping @Alice" {
		t.Fatalf("text = %q", e.Message.Text)
	}
}

func TestChannelWithoutMention(t *testing.T) {
	f := newFakeSlack(t)
	a := mappingAdapter(t, f, nil)

	got := deliver(t, a, eventsPayloadFor("Ev1", channelEvent("just talking", nil)))
	if len(got) != 1 {
		t.Fatalf("delivered %d envelopes", len(got))
	}
	if got[0].Mentioned {
		t.Fatal("mentioned should be false")
	}
	if got[0].Origin.Thread != "" {
		t.Fatalf("thread = %q, want empty", got[0].Origin.Thread)
	}
}

func TestThreadReplyKeepsThreadTS(t *testing.T) {
	f := newFakeSlack(t)
	a := mappingAdapter(t, f, nil)

	got := deliver(t, a, eventsPayloadFor("Ev1", channelEvent("<@UBOT> in thread", map[string]any{"thread_ts": "50.0"})))
	if o := got[0].Origin; o.Thread != "50.0" || o.ThreadRoot != "C1:50.0" {
		t.Fatalf("origin = %+v, want thread 50.0 rooted at C1:50.0", o)
	}
}

func TestDeduplicatesMessageAndAppMention(t *testing.T) {
	f := newFakeSlack(t)
	a := mappingAdapter(t, f, nil)

	mention := map[string]any{
		"type": "app_mention", "channel": "C1", "user": "U2",
		"text": "<@UBOT> ping", "ts": "200.2",
	}
	got := deliver(t, a,
		eventsPayloadFor("Ev1", channelEvent("<@UBOT> ping", nil)),
		eventsPayloadFor("Ev2", mention),
	)
	if len(got) != 1 {
		t.Fatalf("delivered %d envelopes, want 1", len(got))
	}
}

func TestDeduplicatesEventRetry(t *testing.T) {
	f := newFakeSlack(t)
	a := mappingAdapter(t, f, nil)

	p := eventsPayloadFor("Ev1", dmEvent("hi"))
	got := deliver(t, a, p, p)
	if len(got) != 1 {
		t.Fatalf("delivered %d envelopes, want 1", len(got))
	}
}

func TestIgnoredEvents(t *testing.T) {
	cases := []struct {
		name string
		ev   map[string]any
	}{
		{"own bot user", channelEvent("hi", map[string]any{"user": "UBOT"})},
		{"bot_id set", channelEvent("hi", map[string]any{"bot_id": "B9"})},
		{"bot_message", channelEvent("hi", map[string]any{"subtype": "bot_message", "user": "U2"})},
		{"message_changed", channelEvent("hi", map[string]any{"subtype": "message_changed"})},
		{"message_deleted", channelEvent("hi", map[string]any{"subtype": "message_deleted"})},
		{"channel_join", channelEvent("hi", map[string]any{"subtype": "channel_join"})},
		{"other event type", map[string]any{"type": "reaction_added", "channel": "C1", "ts": "200.2", "user": "U2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeSlack(t)
			a := mappingAdapter(t, f, nil)
			if got := deliver(t, a, eventsPayloadFor("Ev1", tc.ev)); len(got) != 0 {
				t.Fatalf("delivered %d envelopes, want 0", len(got))
			}
		})
	}
}

func TestAcceptedSubtypes(t *testing.T) {
	for _, sub := range []string{"", "file_share", "thread_broadcast"} {
		t.Run("subtype "+sub, func(t *testing.T) {
			f := newFakeSlack(t)
			a := mappingAdapter(t, f, nil)
			ev := dmEvent("hi")
			if sub != "" {
				ev["subtype"] = sub
			}
			if got := deliver(t, a, eventsPayloadFor("Ev1", ev)); len(got) != 1 {
				t.Fatalf("delivered %d envelopes, want 1", len(got))
			}
		})
	}
}

// authSpy records the Authorization header of the last request.
type authSpy struct {
	mu   sync.Mutex
	last string
}

func (s *authSpy) set(v string) {
	s.mu.Lock()
	s.last = v
	s.mu.Unlock()
}

func (s *authSpy) get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// fileServer serves one private file and records the request it saw.
func fileServer(t *testing.T, body []byte) (*httptest.Server, *authSpy) {
	t.Helper()
	spy := &authSpy{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spy.set(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, spy
}

func TestFileDownloadAttaches(t *testing.T) {
	f := newFakeSlack(t)
	files, spy := fileServer(t, []byte("PNGDATA-1234"))
	a := mappingAdapter(t, f, nil)

	ev := dmEvent("look")
	ev["subtype"] = "file_share"
	ev["files"] = []any{map[string]any{
		"id": "F1", "name": "shot.png", "mimetype": "image/png",
		"url_private_download": files.URL + "/download",
	}}
	got := deliver(t, a, eventsPayloadFor("Ev1", ev))
	if len(got) != 1 || len(got[0].Message.Attachments) != 1 {
		t.Fatalf("envelopes = %+v", got)
	}
	att := got[0].Message.Attachments[0]
	if att.Kind != event.KindImage || att.MIME != "image/png" || att.Name != "shot.png" {
		t.Fatalf("attachment = %+v", att)
	}
	if att.Size != int64(len("PNGDATA-1234")) || att.BlobID == "" || att.SHA256 != att.BlobID {
		t.Fatalf("attachment = %+v", att)
	}
	if spy.get() != "Bearer "+fakeBotToken {
		t.Fatalf("download auth header = %q", spy.get())
	}
}

func TestFileDownloadCapRefuses(t *testing.T) {
	f := newFakeSlack(t)
	files, _ := fileServer(t, []byte(strings.Repeat("x", 4096)))
	a := mappingAdapter(t, f, func(o *Options) { o.MaxDownload = 16 })

	ev := dmEvent("big")
	ev["files"] = []any{map[string]any{
		"id": "F1", "name": "big.bin", "mimetype": "application/octet-stream",
		"url_private_download": files.URL + "/download",
	}}
	got := deliver(t, a, eventsPayloadFor("Ev1", ev))
	if len(got) != 1 {
		t.Fatalf("envelopes = %d, want the message itself", len(got))
	}
	if n := len(got[0].Message.Attachments); n != 0 {
		t.Fatalf("attachments = %d, want 0 (over the cap)", n)
	}
}

func TestFileDownloadRefusesCrossHostRedirect(t *testing.T) {
	f := newFakeSlack(t)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("token followed the redirect to another host")
		}
		_, _ = io.WriteString(w, "elsewhere")
	}))
	defer other.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer redir.Close()

	a := mappingAdapter(t, f, nil)
	ev := dmEvent("redirect")
	ev["files"] = []any{map[string]any{
		"id": "F1", "name": "x.bin", "url_private_download": redir.URL + "/download",
	}}
	got := deliver(t, a, eventsPayloadFor("Ev1", ev))
	if len(got) != 1 || len(got[0].Message.Attachments) != 0 {
		t.Fatalf("attachments = %+v, want none", got)
	}
}

func TestFetchFileErrorKeepsTokensOut(t *testing.T) {
	f := newFakeSlack(t)
	a := mappingAdapter(t, f, nil)
	_, err := a.fetchFile(context.Background(), slackFile{ID: "F1", URLPrivateDownload: "http://127.0.0.1:1/x"})
	if err == nil {
		t.Fatal("want an error")
	}
	assertNoTokens(t, err.Error())
}

// TestDuplicateIsNotAnError: a message core already handled is not logged as
// a rejected inbound.
func TestDuplicateIsNotAnError(t *testing.T) {
	f := newFakeSlack(t)
	a, logs := newAdapter(t, f, nil)
	a.setIdentity("UBOT", "T1")
	calls := 0
	a.handleEventsAPI(context.Background(), func(context.Context, event.Envelope) error {
		calls++
		return channel.ErrDuplicate
	}, rawJSON(t, eventsPayloadFor("Ev1", dmEvent("hi"))))
	if calls != 1 {
		t.Fatalf("Inbound called %d times", calls)
	}
	if strings.Contains(logs.String(), "inbound rejected") {
		t.Fatalf("duplicate logged as rejected:\n%s", logs.String())
	}
}
