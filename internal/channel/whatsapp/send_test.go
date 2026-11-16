package whatsapp

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"

	"holaclaw/internal/channel"
	"holaclaw/internal/channel/textutil"
	"holaclaw/internal/event"
)

const peerConv = "34611111111@s.whatsapp.net"

// sending is a connected adapter with a blob store and a recorded sleep.
type sending struct {
	*env
	f      *fakeClient
	blobs  *blobSink
	mu     sync.Mutex
	sleeps []time.Duration
}

func newSending(t *testing.T) *sending {
	t.Helper()
	e, f := connected(t)
	s := &sending{env: e, f: f, blobs: &blobSink{}}
	e.a.blobs = s.blobs
	e.a.sleep = func(_ context.Context, d time.Duration) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.sleeps = append(s.sleeps, d)
		return true
	}
	return s
}

func (s *sending) blob(data string) string {
	id, _, _ := s.blobs.put(context.Background(), strings.NewReader(data), "")
	return id
}

func (s *sending) send(t *testing.T, msg event.Message) channel.Receipt {
	t.Helper()
	rec, err := s.a.Send(t.Context(), channel.Outbound{Target: event.Origin{Channel: "wa", Conversation: peerConv}, Message: msg})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	return rec
}

func TestSendText(t *testing.T) {
	s := newSending(t)
	rec := s.send(t, event.Message{Text: "**hola** [docs](https://example.com)"})
	msgs := s.f.sentMsgs()
	if len(msgs) != 1 || msgs[0].to.String() != peerConv || msgs[0].msg.GetConversation() != "*hola* docs (https://example.com)" {
		t.Fatalf("sent = %+v", msgs)
	}
	if want := []string{peerConv + "/" + msgs[0].id}; !reflect.DeepEqual(rec.PlatformMsgIDs, want) || msgs[0].id == "" {
		t.Fatalf("receipt = %v, want %v", rec.PlatformMsgIDs, want)
	}
}

func TestSendSplitsLongText(t *testing.T) {
	s := newSending(t)
	para := strings.Repeat("palabra ", 300) // 2400 characters
	rec := s.send(t, event.Message{Text: para + "\n\n" + para + "\n\n" + para})
	msgs := s.f.sentMsgs()
	if len(msgs) < 2 || len(rec.PlatformMsgIDs) != len(msgs) {
		t.Fatalf("%d messages, %d receipt ids", len(msgs), len(rec.PlatformMsgIDs))
	}
	for i, m := range msgs {
		if n := textutil.UTF16Len(m.msg.GetConversation()); n > maxTextChars || n == 0 {
			t.Fatalf("part %d has %d characters", i, n)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sleeps) != len(msgs)-1 || s.sleeps[0] != partGap {
		t.Fatalf("sleeps between parts = %v", s.sleeps)
	}
}

func TestSendRetryReusesTheMessageID(t *testing.T) {
	s := newSending(t)
	s.f.mu.Lock()
	s.f.sendErr = []error{errors.New("timed out"), nil}
	s.f.mu.Unlock()
	rec := s.send(t, event.Message{Text: "once"})
	msgs := s.f.sentMsgs()
	if len(msgs) != 2 || msgs[0].id != msgs[1].id {
		t.Fatalf("attempts = %+v, want two with one id", msgs)
	}
	if len(rec.PlatformMsgIDs) != 1 {
		t.Fatalf("receipt = %v", rec.PlatformMsgIDs)
	}

	s.f.mu.Lock()
	s.f.sendErr = []error{errors.New("a"), errors.New("b"), errors.New("c")}
	s.f.mu.Unlock()
	if _, err := s.a.Send(t.Context(), channel.Outbound{Target: event.Origin{Conversation: peerConv}, Message: event.Message{Text: "lost"}}); err == nil {
		t.Fatal("a send that failed every attempt succeeded")
	}
	if n := len(s.f.sentMsgs()); n != 5 {
		t.Fatalf("attempts = %d, want %d", n-2, maxSendAttempts)
	}
}

func TestSendRefusals(t *testing.T) {
	s := newSending(t)
	if _, err := s.a.Send(t.Context(), channel.Outbound{Target: event.Origin{Conversation: "not a jid@"}}); err == nil {
		t.Fatal("sent to a bad conversation")
	}
	if _, err := New(Options{}).Send(t.Context(), channel.Outbound{Target: event.Origin{Conversation: peerConv}}); err == nil {
		t.Fatal("sent without Run")
	}
}

func TestSendImageWithCaption(t *testing.T) {
	s := newSending(t)
	att := event.Attachment{BlobID: s.blob("png"), MIME: "image/png", Name: "chart.png", Kind: event.KindImage, Size: 3}
	rec := s.send(t, event.Message{Text: "the _chart_", Attachments: []event.Attachment{att}})
	msgs := s.f.sentMsgs()
	if len(msgs) != 1 || msgs[0].msg.GetImageMessage().GetCaption() != "the _chart_" || msgs[0].msg.GetImageMessage().GetURL() == "" {
		t.Fatalf("sent = %+v", msgs)
	}
	if !reflect.DeepEqual(s.f.uploadsDone(), []whatsmeow.MediaType{whatsmeow.MediaImage}) || len(rec.Degraded) != 0 {
		t.Fatalf("uploads %v, degraded %v", s.f.uploadsDone(), rec.Degraded)
	}
}

func TestSendAudioAndDocuments(t *testing.T) {
	s := newSending(t)
	voice := event.Attachment{BlobID: s.blob("OggS"), MIME: "audio/ogg; codecs=opus", Name: "reply.ogg", Kind: event.KindAudio}
	mp3 := event.Attachment{BlobID: s.blob("ID3"), MIME: "audio/mpeg", Name: "song.mp3", Kind: event.KindAudio}
	pdf := event.Attachment{BlobID: s.blob("%PDF"), MIME: "application/pdf", Name: "r.pdf", Kind: event.KindDocument}
	video := event.Attachment{BlobID: s.blob("mp4"), MIME: "video/mp4", Name: "v.mp4", Kind: event.KindVideo}
	// A voice note carries no caption, so the text goes first on its own.
	rec := s.send(t, event.Message{Text: "listen", Attachments: []event.Attachment{voice}})
	msgs := s.f.sentMsgs()
	if len(msgs) != 2 || msgs[0].msg.GetConversation() != "listen" || !msgs[1].msg.GetAudioMessage().GetPTT() {
		t.Fatalf("voice reply = %+v", msgs)
	}
	if len(rec.PlatformMsgIDs) != 2 {
		t.Fatalf("receipt = %v", rec.PlatformMsgIDs)
	}
	s.send(t, event.Message{Attachments: []event.Attachment{mp3, pdf, video}})
	msgs = s.f.sentMsgs()[2:]
	if msgs[0].msg.GetDocumentMessage().GetFileName() != "song.mp3" || msgs[1].msg.GetDocumentMessage().GetMimetype() != "application/pdf" ||
		msgs[2].msg.GetVideoMessage() == nil {
		t.Fatalf("files = %+v", msgs)
	}
	want := []whatsmeow.MediaType{whatsmeow.MediaAudio, whatsmeow.MediaDocument, whatsmeow.MediaDocument, whatsmeow.MediaVideo}
	if !reflect.DeepEqual(s.f.uploadsDone(), want) {
		t.Fatalf("uploads = %v, want %v", s.f.uploadsDone(), want)
	}
}

func TestSendDegradesFilesThatCannotTravel(t *testing.T) {
	s := newSending(t)
	huge := event.Attachment{BlobID: "x", Name: "huge.bin", Kind: event.KindDocument, Size: 200 << 20}
	missing := event.Attachment{BlobID: "missing", Name: "gone.png", Kind: event.KindImage}
	rec := s.send(t, event.Message{Text: "see attached", Attachments: []event.Attachment{huge}})
	if len(rec.Degraded) != 1 || !strings.Contains(rec.Degraded[0], "huge.bin: too large for WhatsApp (200 MB)") {
		t.Fatalf("degraded = %v", rec.Degraded)
	}
	if msgs := s.f.sentMsgs(); len(msgs) != 1 || msgs[0].msg.GetConversation() != "see attached" {
		t.Fatalf("the text did not travel on its own: %+v", msgs)
	}
	s.f.mu.Lock()
	s.f.uploadErr = errors.New("media conn refused")
	s.f.mu.Unlock()
	ok := event.Attachment{BlobID: s.blob("png"), Name: "a.png", Kind: event.KindImage}
	rec = s.send(t, event.Message{Attachments: []event.Attachment{missing, ok}})
	if len(rec.Degraded) != 2 || !strings.Contains(rec.Degraded[0], "gone.png") || !strings.Contains(rec.Degraded[1], "upload failed") {
		t.Fatalf("degraded = %v", rec.Degraded)
	}
}

func TestSendCopiesTheDisappearingTimer(t *testing.T) {
	s := newSending(t)
	one(t, s.env, s.f, dm("E1", extText("shh", &waE2E.ContextInfo{Expiration: ptr(uint32(604800))})))
	s.send(t, event.Message{Text: "ok", Attachments: []event.Attachment{{BlobID: s.blob("png"), Name: "a.png", Kind: event.KindImage}}})
	msgs := s.f.sentMsgs()
	if len(msgs) != 1 || msgs[0].msg.GetImageMessage().GetContextInfo().GetExpiration() != 604800 {
		t.Fatalf("image = %+v", msgs)
	}
	s.send(t, event.Message{Text: "plain"})
	if m := s.f.sentMsgs()[1].msg; m.GetExtendedTextMessage().GetText() != "plain" || m.GetExtendedTextMessage().GetContextInfo().GetExpiration() != 604800 {
		t.Fatalf("text = %+v", m)
	}
}

func TestTypingMarksTheTurnReadAndComposes(t *testing.T) {
	e, f := connected(t)
	eventually(t, "available on connect", func() bool { return len(f.presences()) == 1 })
	if got := f.presences(); got[0] != "available" {
		t.Fatalf("presence on connect = %v, want available", got)
	}
	one(t, e, f, dm("R1", text("first")))
	one(t, e, f, dm("R2", text("second")))
	one(t, e, f, inGroup("R3", text("not for the bot")))

	stop := e.a.Typing(t.Context(), event.Origin{Conversation: peerConv})
	eventually(t, "composing", func() bool { return len(f.presences()) == 2 })
	want := []string{peerConv + "/34611111111:12@s.whatsapp.net/R1", peerConv + "/34611111111:12@s.whatsapp.net/R2"}
	if got := f.readsDone(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reads = %v, want %v", got, want)
	}
	e.clock.fire(t, typingInterval)
	eventually(t, "second composing", func() bool { return len(f.presences()) == 3 })
	stop()
	stop()
	if got := f.presences(); !reflect.DeepEqual(got, []string{"available", "composing", "composing", "paused"}) {
		t.Fatalf("presences = %v", got)
	}
	// The group message started no turn and stays unread.
	stop = e.a.Typing(t.Context(), event.Origin{Conversation: group.String()})
	stop()
	if n := len(f.readsDone()); n != 2 {
		t.Fatalf("reads = %v", f.readsDone())
	}
}

func TestTypingFailureStopsQuietly(t *testing.T) {
	e, f := connected(t)
	eventually(t, "available on connect", func() bool { return len(f.presences()) == 1 })
	f.mu.Lock()
	f.chatPresenceErr = errors.New("offline")
	f.mu.Unlock()
	stop := e.a.Typing(t.Context(), event.Origin{Conversation: peerConv})
	eventually(t, "composing attempt", func() bool { return len(f.presences()) == 2 })
	stop()
	if got := f.presences(); len(got) != 2 {
		t.Fatalf("presences = %v, want no paused after a failed composing", got)
	}
	if !strings.Contains(e.logs.String(), "typing indicator failed") {
		t.Fatal("the failure was not logged")
	}
	// No client: a no-op.
	New(Options{}).Typing(t.Context(), event.Origin{Conversation: peerConv})()
}

func TestPresenceAfterPushName(t *testing.T) {
	_, f := connected(t)
	eventually(t, "available on connect", func() bool { return len(f.presences()) == 1 })
	f.emit(&events.PushNameSetting{})
	eventually(t, "second available", func() bool { return len(f.presences()) == 2 })
}
