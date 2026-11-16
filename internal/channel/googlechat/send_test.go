package googlechat

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

func out(conv, thread, text string, atts ...event.Attachment) channel.Outbound {
	return channel.Outbound{
		Target:  event.Origin{Channel: "gchat", Conversation: conv, Thread: thread},
		Message: event.Message{Text: text, Attachments: atts},
	}
}

var clientID = regexp.MustCompile(`^client-[0-9a-z]{26}$`)

func TestSendSplitsAndNamesMessages(t *testing.T) {
	f := newFake(t)
	a, _ := newAdapter(t, f, nil)
	para := strings.Repeat("word ", 700) // 3500 bytes
	text := para + "\n\n" + para + "\n\n" + para
	rec, err := a.Send(context.Background(), out("spaces/S1", "", text))
	if err != nil {
		t.Fatal(err)
	}
	creates := f.records("create")
	if len(creates) != 3 || len(rec.PlatformMsgIDs) != 3 {
		t.Fatalf("creates=%d ids=%v", len(creates), rec.PlatformMsgIDs)
	}
	seen := map[string]bool{}
	for i, c := range creates {
		id := c.Query.Get("messageId")
		if !clientID.MatchString(id) || seen[id] {
			t.Fatalf("messageId %q is not a fresh client id", id)
		}
		seen[id] = true
		if rec.PlatformMsgIDs[i] != "spaces/S1/messages/"+id {
			t.Errorf("receipt id = %q", rec.PlatformMsgIDs[i])
		}
		if c.Query.Has("messageReplyOption") {
			t.Error("no thread: no reply option")
		}
		body := c.json(t)
		if n := len([]rune(body["text"].(string))); n > maxTextChars {
			t.Errorf("chunk %d has %d chars", i, n)
		}
		if _, ok := body["thread"]; ok {
			t.Error("no thread expected in body")
		}
	}
}

func TestSendInThread(t *testing.T) {
	f := newFake(t)
	a, _ := newAdapter(t, f, nil)
	if _, err := a.Send(context.Background(), out("spaces/S1", "spaces/S1/threads/T1", "**hi**")); err != nil {
		t.Fatal(err)
	}
	c := f.records("create")[0]
	if c.Path != "/v1/spaces/S1/messages" || c.Query.Get("messageReplyOption") != "REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD" {
		t.Fatalf("request = %s %v", c.Path, c.Query)
	}
	body := c.json(t)
	if body["text"] != "*hi*" || body["thread"].(map[string]any)["name"] != "spaces/S1/threads/T1" {
		t.Fatalf("body = %v", body)
	}
}

func TestSendRetryReusesMessageID(t *testing.T) {
	f := newFake(t)
	n := 0
	f.handle("create", func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			writeJSON(w, 503, googleErr(503, "unavailable"))
			return
		}
		writeJSON(w, 200, map[string]any{"name": "spaces/S1/messages/server-name"})
	})
	a, _ := newAdapter(t, f, nil)
	rec, err := a.Send(context.Background(), out("spaces/S1", "", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	c := f.records("create")
	if len(c) != 2 || c[0].Query.Get("messageId") != c[1].Query.Get("messageId") {
		t.Fatalf("retry must reuse the messageId: %v", c)
	}
	if rec.PlatformMsgIDs[0] != "spaces/S1/messages/server-name" {
		t.Fatalf("receipt = %v", rec.PlatformMsgIDs)
	}
}

func TestSendConflictOnRetryIsSent(t *testing.T) {
	f := newFake(t)
	n := 0
	f.handle("create", func(w http.ResponseWriter, r *http.Request) {
		n++
		if n == 1 {
			writeJSON(w, 500, googleErr(500, "lost"))
			return
		}
		writeJSON(w, 409, googleErr(409, "ALREADY_EXISTS"))
	})
	a, _ := newAdapter(t, f, nil)
	rec, err := a.Send(context.Background(), out("spaces/S1", "", "hi"))
	if err != nil {
		t.Fatalf("409 on retry is success, got %v", err)
	}
	id := f.records("create")[0].Query.Get("messageId")
	if rec.PlatformMsgIDs[0] != "spaces/S1/messages/"+id {
		t.Fatalf("receipt = %v", rec.PlatformMsgIDs)
	}
}

func TestSendConflictFirstAttemptIsError(t *testing.T) {
	f := newFake(t)
	f.handle("create", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 409, googleErr(409, "ALREADY_EXISTS")) })
	a, _ := newAdapter(t, f, nil)
	if _, err := a.Send(context.Background(), out("spaces/S1", "", "hi")); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestSendHonoursRetryAfter(t *testing.T) {
	f := newFake(t)
	var mu sync.Mutex
	var at []time.Time
	f.handle("create", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		at = append(at, time.Now())
		n := len(at)
		mu.Unlock()
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			writeJSON(w, 429, googleErr(429, "slow down"))
			return
		}
		writeJSON(w, 200, map[string]any{"name": "spaces/S1/messages/x"})
	})
	old := backoffMax
	backoffMax = 2 * time.Second
	t.Cleanup(func() { backoffMax = old })
	a, _ := newAdapter(t, f, nil)
	if _, err := a.Send(context.Background(), out("spaces/S1", "", "hi")); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(at) != 2 || at[1].Sub(at[0]) < 900*time.Millisecond {
		t.Fatalf("calls=%d, Retry-After not honoured", len(at))
	}
}

func TestSendRateLimitExhausted(t *testing.T) {
	f := newFake(t)
	f.handle("create", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		writeJSON(w, 429, googleErr(429, "quota"))
	})
	a, _ := newAdapter(t, f, nil)
	_, err := a.Send(context.Background(), out("spaces/S1", "", "hi"))
	if !errors.Is(err, ErrRateLimited) || len(f.records("create")) != maxSendAttempts {
		t.Fatalf("err=%v attempts=%d", err, len(f.records("create")))
	}
}

func TestSendNonRetryableFailsAtOnce(t *testing.T) {
	f := newFake(t)
	f.handle("create", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 403, googleErr(403, "not a member")) })
	a, _ := newAdapter(t, f, nil)
	if _, err := a.Send(context.Background(), out("spaces/S1", "", "hi")); !errors.Is(err, ErrForbidden) || len(f.records("create")) != 1 {
		t.Fatalf("err = %v", err)
	}
}

func TestSendAttachmentsDegraded(t *testing.T) {
	f := newFake(t)
	a, _ := newAdapter(t, f, nil)
	rec, err := a.Send(context.Background(), out("spaces/S1", "", "",
		event.Attachment{ID: "A1", Name: "chart.png"}, event.Attachment{ID: "A2"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.records("create")) != 0 || len(rec.PlatformMsgIDs) != 0 {
		t.Fatal("nothing to post")
	}
	if len(rec.Degraded) != 2 || !strings.HasPrefix(rec.Degraded[0], "chart.png: ") || !strings.HasPrefix(rec.Degraded[1], "A2: ") {
		t.Fatalf("degraded = %v", rec.Degraded)
	}
	if l := a.Limits(); l.TextChars != 4000 || l.CaptionChars != 0 || len(l.Kinds) != 0 {
		t.Fatalf("limits = %+v", l)
	}
}

func TestSendRejectsBadTarget(t *testing.T) {
	a, _ := newAdapter(t, newFake(t), nil)
	for _, o := range []channel.Outbound{
		out("", "", "x"), out("spaces/S1/../x", "", "x"), out("spaces/S1?x=1", "", "x"), out("spaces/S1", "threads/x", "x"),
	} {
		if _, err := a.Send(context.Background(), o); !errors.Is(err, ErrBadRequest) {
			t.Errorf("%+v: err = %v", o.Target, err)
		}
	}
}

func TestAdapterIdentity(t *testing.T) {
	a := New(Options{Name: "gc"})
	if a.Name() != "gc" || a.Type() != "googlechat" {
		t.Fatal(a.Name(), a.Type())
	}
	var c channel.Channel = a
	if _, ok := c.(channel.Typer); ok {
		t.Fatal("Chat has no typing indicator: the adapter must not be a Typer")
	}
	if _, ok := c.(channel.ApprovalPrompter); !ok {
		t.Fatal("not an ApprovalPrompter")
	}
}

// replyOut answers message M1 of space S1.
func replyOut(thread, text string) channel.Outbound {
	o := out("spaces/S1", thread, text)
	o.Target.PlatformMsgID = "spaces/S1/messages/M1"
	return o
}

func quoted(t *testing.T, c rec) map[string]any {
	t.Helper()
	q, _ := c.json(t)["quotedMessageMetadata"].(map[string]any)
	return q
}

func TestSendReplyToQuotesOnlyTheFirstMessage(t *testing.T) {
	f := newFake(t)
	a, _ := newAdapter(t, f, nil)
	para := strings.Repeat("word ", 700)
	if _, err := a.Send(context.Background(), replyOut("", para+"\n\n"+para)); err != nil {
		t.Fatal(err)
	}
	if g := f.records("get"); len(g) != 1 || g[0].Path != "/v1/spaces/S1/messages/M1" {
		t.Fatalf("get = %+v", g)
	}
	creates := f.records("create")
	if len(creates) != 2 {
		t.Fatalf("creates = %d", len(creates))
	}
	q := quoted(t, creates[0])
	if q["name"] != "spaces/S1/messages/M1" || q["lastUpdateTime"] != "2026-10-19T10:00:00.123456Z" {
		t.Fatalf("quote = %v", q)
	}
	if q := quoted(t, creates[1]); q != nil {
		t.Fatalf("second chunk quotes %v", q)
	}
}

func TestSendReplyToUsesLastUpdateTime(t *testing.T) {
	f := newFake(t)
	f.handle("get", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"createTime": "2026-10-19T10:00:00Z", "lastUpdateTime": "2026-10-19T11:00:00Z"})
	})
	a, _ := newAdapter(t, f, nil)
	if _, err := a.Send(context.Background(), replyOut("spaces/S1/threads/T1", "hi")); err != nil {
		t.Fatal(err)
	}
	c := f.records("create")[0]
	if q := quoted(t, c); q["lastUpdateTime"] != "2026-10-19T11:00:00Z" {
		t.Fatalf("quote = %v", q)
	}
	if c.json(t)["thread"].(map[string]any)["name"] != "spaces/S1/threads/T1" {
		t.Fatal("the user's thread must be kept")
	}
}

func TestSendReplyToLeftOut(t *testing.T) {
	for name, id := range map[string]string{"no message": "", "another space": "spaces/S9/messages/M1", "not a message": "client:01J"} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			a, _ := newAdapter(t, f, nil)
			o := out("spaces/S1", "", "hi")
			o.Target.PlatformMsgID = id
			if _, err := a.Send(context.Background(), o); err != nil {
				t.Fatal(err)
			}
			if len(f.records("get")) != 0 || quoted(t, f.records("create")[0]) != nil {
				t.Fatal("a plain reply must neither read nor quote a message")
			}
		})
	}
}

func TestSendReplyToMessageGone(t *testing.T) {
	f := newFake(t)
	f.handle("get", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 404, googleErr(404, "message not found"))
	})
	a, _ := newAdapter(t, f, nil)
	rec, err := a.Send(context.Background(), replyOut("", "hi"))
	if err != nil || len(rec.PlatformMsgIDs) != 1 {
		t.Fatalf("rec = %+v err = %v", rec, err)
	}
	if q := quoted(t, f.records("create")[0]); q != nil {
		t.Fatalf("quote = %v", q)
	}
}

func TestSendReplyToQuoteRefused(t *testing.T) {
	f := newFake(t)
	f.handle("create", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "quotedMessageMetadata") {
			writeJSON(w, 400, googleErr(400, "quoted message was updated"))
			return
		}
		writeJSON(w, 200, map[string]any{"name": "spaces/S1/messages/" + r.URL.Query().Get("messageId")})
	})
	a, _ := newAdapter(t, f, nil)
	rec, err := a.Send(context.Background(), replyOut("", "hi"))
	if err != nil || len(rec.PlatformMsgIDs) != 1 || len(f.records("create")) != 2 {
		t.Fatalf("rec = %+v err = %v creates = %d", rec, err, len(f.records("create")))
	}
}
