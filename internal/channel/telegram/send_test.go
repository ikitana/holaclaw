package telegram

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

// outbound builds a reply to the test DM.
func outbound(text string, atts ...event.Attachment) channel.Outbound {
	return channel.Outbound{
		Target: event.Origin{
			Channel: "tg-test", ChannelType: "telegram",
			Conversation: "1234", ConversationKind: event.ConversationDM,
		},
		Message: event.Message{Text: text, Attachments: atts},
	}
}

func att(kind event.Kind, mime, name string, size int64) event.Attachment {
	return event.Attachment{ID: event.NewID(), BlobID: "b1", MIME: mime, Name: name, Size: size, Kind: kind}
}

func sendFake(t *testing.T, blob []byte) (*fakeAPI, *Adapter, *logSink) {
	t.Helper()
	f := newFake(t)
	f.on("sendMessage", wireMessage{MessageID: 100})
	for _, m := range []string{"sendPhoto", "sendDocument", "sendAudio", "sendVoice", "sendVideo"} {
		f.on(m, wireMessage{MessageID: 101})
	}
	a, _, sink := newAdapter(t, f, func(o *Options) {
		o.Blobs = memBlobs{data: map[string][]byte{"b1": blob}}
	})
	return f, a, sink
}

func TestSendTextOnly(t *testing.T) {
	f, a, _ := sendFake(t, nil)
	rec, err := a.Send(t.Context(), outbound("**hi** there"))
	if err != nil {
		t.Fatal(err)
	}
	c := f.seen("sendMessage")[0]
	if c.str("text") != "<b>hi</b> there" || c.str("parse_mode") != "HTML" {
		t.Fatalf("body = %+v", c.Body)
	}
	if len(rec.PlatformMsgIDs) != 1 || rec.PlatformMsgIDs[0] != "1234:100" {
		t.Fatalf("receipt = %+v", rec)
	}
}

func TestSendSplitsLongText(t *testing.T) {
	f, a, _ := sendFake(t, nil)
	long := strings.TrimSpace(strings.Repeat("A sentence that goes on. ", 600))
	rec, err := a.Send(t.Context(), outbound(long))
	if err != nil {
		t.Fatal(err)
	}
	calls := f.seen("sendMessage")
	if len(calls) < 4 {
		t.Fatalf("expected the text to be split, got %d messages", len(calls))
	}
	for i, c := range calls {
		if n := visibleLen(c.str("text")); n > maxTextChars {
			t.Fatalf("chunk %d is %d units", i, n)
		}
	}
	if len(rec.PlatformMsgIDs) != len(calls) {
		t.Fatalf("receipt = %+v", rec)
	}
}

func TestSendThreadID(t *testing.T) {
	f, a, _ := sendFake(t, nil)
	out := outbound("hi")
	out.Target.Thread = "42"
	if _, err := a.Send(t.Context(), out); err != nil {
		t.Fatal(err)
	}
	if got := f.seen("sendMessage")[0].str("message_thread_id"); got != "42" {
		t.Fatalf("message_thread_id = %q", got)
	}
}

func TestSendCaptionRule(t *testing.T) {
	t.Run("one attachment and short text travel together", func(t *testing.T) {
		f, a, _ := sendFake(t, []byte("img"))
		if _, err := a.Send(t.Context(), outbound("look", att(event.KindImage, "image/jpeg", "p.jpg", 3))); err != nil {
			t.Fatal(err)
		}
		if f.count("sendMessage") != 0 {
			t.Fatal("the text should have ridden as a caption")
		}
		c := f.seen("sendPhoto")[0]
		if c.str("caption") != "look" || c.str("parse_mode") != "HTML" {
			t.Fatalf("fields = %+v", c.Fields)
		}
	})

	t.Run("text over the caption limit goes first", func(t *testing.T) {
		f, a, _ := sendFake(t, []byte("img"))
		long := strings.Repeat("x", maxCaptionChars+1)
		if _, err := a.Send(t.Context(), outbound(long, att(event.KindImage, "image/jpeg", "p.jpg", 3))); err != nil {
			t.Fatal(err)
		}
		if f.count("sendMessage") != 1 || f.count("sendPhoto") != 1 {
			t.Fatalf("messages = %d photos = %d", f.count("sendMessage"), f.count("sendPhoto"))
		}
		if f.seen("sendPhoto")[0].str("caption") != "" {
			t.Fatal("caption must be empty when the text was sent separately")
		}
	})

	t.Run("two attachments mean text first", func(t *testing.T) {
		f, a, _ := sendFake(t, []byte("img"))
		out := outbound("two", att(event.KindImage, "image/jpeg", "a.jpg", 3), att(event.KindImage, "image/jpeg", "b.jpg", 3))
		if _, err := a.Send(t.Context(), out); err != nil {
			t.Fatal(err)
		}
		if f.count("sendMessage") != 1 || f.count("sendPhoto") != 2 {
			t.Fatalf("messages = %d photos = %d", f.count("sendMessage"), f.count("sendPhoto"))
		}
	})
}

func TestSendMethodByKind(t *testing.T) {
	cases := []struct {
		name   string
		att    event.Attachment
		blob   []byte
		method string
		field  string
	}{
		{"image", att(event.KindImage, "image/jpeg", "p.jpg", 3), []byte("img"), "sendPhoto", "photo"},
		{"large image goes as a document", att(event.KindImage, "image/png", "p.png", 0), make([]byte, maxPhotoBytes+1), "sendDocument", "document"},
		{"voice note", att(event.KindAudio, "audio/ogg; codecs=opus", "v.ogg", 3), []byte("ogg"), "sendVoice", "voice"},
		{"plain audio", att(event.KindAudio, "audio/mpeg", "s.mp3", 3), []byte("mp3"), "sendAudio", "audio"},
		{"ogg without opus is audio", att(event.KindAudio, "audio/ogg", "s.ogg", 3), []byte("ogg"), "sendAudio", "audio"},
		{"video", att(event.KindVideo, "video/mp4", "v.mp4", 3), []byte("mp4"), "sendVideo", "video"},
		{"document", att(event.KindDocument, "text/markdown", "r.md", 3), []byte("md"), "sendDocument", "document"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, a, _ := sendFake(t, c.blob)
			if _, err := a.Send(t.Context(), outbound("", c.att)); err != nil {
				t.Fatal(err)
			}
			if f.count(c.method) != 1 {
				t.Fatalf("%s was not called: %d", c.method, f.count(c.method))
			}
			call := f.seen(c.method)[0]
			if call.FileName != c.att.Name {
				t.Fatalf("uploaded name = %q", call.FileName)
			}
			if string(call.File) != string(c.blob) {
				t.Fatal("uploaded bytes differ from the blob")
			}
		})
	}
}

func TestSendOversizeAttachmentDegrades(t *testing.T) {
	f, a, _ := sendFake(t, nil)
	big := att(event.KindDocument, "application/zip", "huge.zip", 60<<20)
	rec, err := a.Send(t.Context(), outbound("here it is", big))
	if err != nil {
		t.Fatal(err)
	}
	if f.count("sendDocument") != 0 {
		t.Fatal("an oversize file must not be uploaded")
	}
	if len(rec.Degraded) != 1 || !strings.Contains(rec.Degraded[0], "too large for Telegram (60 MB)") {
		t.Fatalf("degraded = %v", rec.Degraded)
	}
	if f.count("sendMessage") != 1 {
		t.Fatal("the text must still be delivered")
	}
}

func TestSendPlainTextFallbackOnParseError(t *testing.T) {
	f, a, sink := sendFake(t, nil)
	var n int
	f.onFunc("sendMessage", func(c call) reply {
		n++
		if n == 1 {
			return reply{Err: &fakeErr{Status: 400, Description: "Bad Request: can't parse entities: unsupported start tag"}}
		}
		return reply{Result: wireMessage{MessageID: 100}}
	})
	if _, err := a.Send(t.Context(), outbound("**hi**")); err != nil {
		t.Fatal(err)
	}
	calls := f.seen("sendMessage")
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	if _, ok := calls[1].Body["parse_mode"]; ok {
		t.Fatal("the retry must carry no parse_mode")
	}
	if calls[1].str("text") != "hi" {
		t.Fatalf("plain text = %q", calls[1].str("text"))
	}
	if strings.Contains(sink.String(), fakeToken) {
		t.Fatal("token leaked")
	}
}

func TestSendHonoursRetryAfter(t *testing.T) {
	f, a, _ := sendFake(t, nil)
	var n int
	f.onFunc("sendMessage", func(c call) reply {
		n++
		if n == 1 {
			return reply{Err: &fakeErr{Status: 429, Description: "Too Many Requests", RetryAfter: 1}}
		}
		return reply{Result: wireMessage{MessageID: 100}}
	})
	begin := time.Now()
	if _, err := a.Send(t.Context(), outbound("hi")); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(begin); d < 800*time.Millisecond {
		t.Fatalf("retry_after not honoured: resent after %v", d)
	}
	if f.count("sendMessage") != 2 {
		t.Fatalf("calls = %d", f.count("sendMessage"))
	}
}

func TestSendGivesUpAfterThreeRateLimits(t *testing.T) {
	f, a, _ := sendFake(t, nil)
	f.on("sendMessage", &fakeErr{Status: 429, Description: "Too Many Requests"})
	_, err := a.Send(t.Context(), outbound("hi"))
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("err = %v", err)
	}
	if f.count("sendMessage") != maxSendAttempts {
		t.Fatalf("attempts = %d, want %d", f.count("sendMessage"), maxSendAttempts)
	}
}

func TestSendBlockedIsTyped(t *testing.T) {
	f, a, _ := sendFake(t, nil)
	f.on("sendMessage", &fakeErr{Status: 403, Description: "Forbidden: bot was blocked by the user"})
	_, err := a.Send(t.Context(), outbound("hi"))
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("err = %v, want ErrForbidden", err)
	}
	if strings.Contains(err.Error(), fakeToken) {
		t.Fatal("token leaked into the error")
	}
}

func TestSendPhotoFallsBackToDocument(t *testing.T) {
	f, a, _ := sendFake(t, []byte("img"))
	f.on("sendPhoto", &fakeErr{Status: 400, Description: "Bad Request: PHOTO_INVALID_DIMENSIONS"})
	rec, err := a.Send(t.Context(), outbound("", att(event.KindImage, "image/jpeg", "p.jpg", 3)))
	if err != nil {
		t.Fatal(err)
	}
	if f.count("sendDocument") != 1 {
		t.Fatal("a rejected photo must be retried as a document")
	}
	if len(rec.Degraded) != 0 {
		t.Fatalf("degraded = %v", rec.Degraded)
	}
}

func TestSendEmptyTargetIsRejected(t *testing.T) {
	_, a, _ := sendFake(t, nil)
	out := outbound("hi")
	out.Target.Conversation = ""
	if _, err := a.Send(t.Context(), out); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("err = %v", err)
	}
}

func TestLimits(t *testing.T) {
	_, a, _ := sendFake(t, nil)
	l := a.Limits()
	if l.TextChars != 4096 || l.CaptionChars != 1024 || l.MaxFileBytes != 50<<20 || len(l.Kinds) != 4 {
		t.Fatalf("limits = %+v", l)
	}
	if a.Type() != "telegram" || a.Name() != "tg-test" {
		t.Fatalf("name = %q type = %q", a.Name(), a.Type())
	}
}

// replyOut is outbound answering message 7 of the test chat.
func replyOut(text string, atts ...event.Attachment) channel.Outbound {
	out := outbound(text, atts...)
	out.Target.PlatformMsgID = "1234:7"
	return out
}

// replyParams decodes reply_parameters from either encoding; nil when absent.
func replyParams(t *testing.T, c call) map[string]any {
	t.Helper()
	if v, ok := c.Body["reply_parameters"].(map[string]any); ok {
		return v
	}
	s, ok := c.Fields["reply_parameters"]
	if !ok {
		return nil
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("reply_parameters = %q: %v", s, err)
	}
	return v
}

func TestSendReplyToMarksOnlyTheFirstMessage(t *testing.T) {
	f, a, _ := sendFake(t, nil)
	long := strings.TrimSpace(strings.Repeat("A sentence that goes on. ", 600))
	if _, err := a.Send(t.Context(), replyOut(long)); err != nil {
		t.Fatal(err)
	}
	calls := f.seen("sendMessage")
	if len(calls) < 2 {
		t.Fatalf("expected a split, got %d messages", len(calls))
	}
	p := replyParams(t, calls[0])
	if p["message_id"] != float64(7) || p["allow_sending_without_reply"] != true {
		t.Fatalf("first reply_parameters = %v", p)
	}
	for i, c := range calls[1:] {
		if p := replyParams(t, c); p != nil {
			t.Fatalf("chunk %d carries reply_parameters %v", i+1, p)
		}
	}
}

func TestSendReplyToOnAttachments(t *testing.T) {
	t.Run("caption", func(t *testing.T) {
		f, a, _ := sendFake(t, []byte("img"))
		if _, err := a.Send(t.Context(), replyOut("look", att(event.KindImage, "image/jpeg", "p.jpg", 3))); err != nil {
			t.Fatal(err)
		}
		if p := replyParams(t, f.seen("sendPhoto")[0]); p["message_id"] != float64(7) {
			t.Fatalf("reply_parameters = %v", p)
		}
	})
	t.Run("text first, then files plain", func(t *testing.T) {
		f, a, _ := sendFake(t, []byte("img"))
		out := replyOut("two", att(event.KindImage, "image/jpeg", "a.jpg", 3), att(event.KindImage, "image/jpeg", "b.jpg", 3))
		if _, err := a.Send(t.Context(), out); err != nil {
			t.Fatal(err)
		}
		if p := replyParams(t, f.seen("sendMessage")[0]); p["message_id"] != float64(7) {
			t.Fatalf("text reply_parameters = %v", p)
		}
		for _, c := range f.seen("sendPhoto") {
			if p := replyParams(t, c); p != nil {
				t.Fatalf("photo carries reply_parameters %v", p)
			}
		}
	})
	t.Run("degraded file leaves the marker for the text", func(t *testing.T) {
		f, a, _ := sendFake(t, []byte("img"))
		big := att(event.KindImage, "image/jpeg", "big.jpg", maxUploadBytes+1)
		if _, err := a.Send(t.Context(), replyOut("look", big)); err != nil {
			t.Fatal(err)
		}
		if p := replyParams(t, f.seen("sendMessage")[0]); p["message_id"] != float64(7) {
			t.Fatalf("reply_parameters = %v", p)
		}
	})
}

func TestSendReplyToLeftOut(t *testing.T) {
	for name, id := range map[string]string{
		"no message":        "",
		"another chat":      "999:7",
		"not a message id":  "1234:abc",
		"not a telegram id": "client:01J",
		"zero message":      "1234:0",
	} {
		t.Run(name, func(t *testing.T) {
			f, a, _ := sendFake(t, nil)
			out := outbound("hi")
			out.Target.PlatformMsgID = id
			if _, err := a.Send(t.Context(), out); err != nil {
				t.Fatal(err)
			}
			if p := replyParams(t, f.seen("sendMessage")[0]); p != nil {
				t.Fatalf("reply_parameters = %v", p)
			}
		})
	}
}

func TestSendReplyToMessageGone(t *testing.T) {
	f, a, _ := sendFake(t, []byte("img"))
	f.onFunc("sendMessage", func(c call) reply {
		if _, ok := c.Body["reply_parameters"]; ok {
			return reply{Err: &fakeErr{Status: 400, Description: "Bad Request: message to be replied not found"}}
		}
		return reply{Result: wireMessage{MessageID: 100}}
	})
	f.onFunc("sendPhoto", func(c call) reply {
		if _, ok := c.Fields["reply_parameters"]; ok {
			return reply{Err: &fakeErr{Status: 400, Description: "Bad Request: message to be replied not found"}}
		}
		return reply{Result: wireMessage{MessageID: 101}}
	})

	rec, err := a.Send(t.Context(), replyOut("hi"))
	if err != nil {
		t.Fatalf("send = %v", err)
	}
	if len(rec.PlatformMsgIDs) != 1 || f.count("sendMessage") != 2 {
		t.Fatalf("receipt = %+v, calls = %d", rec, f.count("sendMessage"))
	}

	rec, err = a.Send(t.Context(), replyOut("look", att(event.KindImage, "image/jpeg", "p.jpg", 3)))
	if err != nil || len(rec.PlatformMsgIDs) != 1 || len(rec.Degraded) != 0 {
		t.Fatalf("photo: rec = %+v err = %v", rec, err)
	}
}
