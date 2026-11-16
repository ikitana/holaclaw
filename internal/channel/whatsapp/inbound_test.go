package whatsapp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

var (
	peerPN  = types.JID{User: "34611111111", Server: types.DefaultUserServer, Device: 12}
	peerLID = types.NewJID("123456789", types.HiddenUserServer)
	botLID  = types.NewJID("987654321", types.HiddenUserServer)
	group   = types.NewJID("120363000000000001", types.GroupServer)
)

// dm is a direct message from the peer, carrying both of its ids.
func dm(id string, m *waE2E.Message) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: peerPN.ToNonAD(), Sender: peerPN, SenderAlt: peerLID},
			ID:            id, PushName: "Ana", Timestamp: time.Unix(1_800_000_000, 0),
		},
		Message: m,
	}
}

// inGroup is a group message from the peer.
func inGroup(id string, m *waE2E.Message) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: group, Sender: peerLID, SenderAlt: peerPN, IsGroup: true},
			ID:            id, PushName: "Ana",
		},
		Message: m,
	}
}

func text(s string) *waE2E.Message { return &waE2E.Message{Conversation: ptr(s)} }

func extText(s string, ci *waE2E.ContextInfo) *waE2E.Message {
	return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: ptr(s), ContextInfo: ci}}
}

// blobSink stands in for the blob store.
type blobSink struct {
	mu    sync.Mutex
	blobs map[string][]byte
	err   error
}

func (b *blobSink) put(_ context.Context, r io.Reader, _ string) (string, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return "", 0, b.err
	}
	data, _ := io.ReadAll(r)
	id := "blob" + string(rune('a'+len(b.blobs)))
	if b.blobs == nil {
		b.blobs = map[string][]byte{}
	}
	b.blobs[id] = data
	return id, int64(len(data)), nil
}

func (b *blobSink) OpenAttachment(_ context.Context, a event.Attachment) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.blobs[a.BlobID]
	if !ok {
		return nil, errors.New("no such blob")
	}
	return data, nil
}

// one emits evt and returns the single envelope it stored.
func one(t *testing.T, e *env, f *fakeClient, evt any) event.Envelope {
	t.Helper()
	before := len(e.stored())
	if !f.emit(evt) {
		t.Fatal("the handler withheld the ack")
	}
	got := e.stored()
	if len(got) != before+1 {
		t.Fatalf("stored %d envelopes, want 1", len(got)-before)
	}
	return got[len(got)-1]
}

func TestInboundDirectMessage(t *testing.T) {
	e, f := connected(t)
	env := one(t, e, f, dm("3A01", text("hola")))
	if env.Origin.Conversation != "34611111111@s.whatsapp.net" || env.Origin.ConversationKind != event.ConversationDM ||
		env.Origin.PlatformMsgID != "34611111111@s.whatsapp.net/3A01" || env.Origin.Thread != "" ||
		env.Origin.Channel != "wa" || env.Origin.ChannelType != "whatsapp" {
		t.Fatalf("origin = %+v", env.Origin)
	}
	// A phone-addressed sender: whatsmeow filled SenderAlt from its LID map,
	// not from the stanza, so only the number counts. Device suffix stripped.
	if want := []string{"wa:+34611111111"}; !reflect.DeepEqual(env.Sender.Aliases, want) {
		t.Fatalf("aliases = %v, want %v", env.Sender.Aliases, want)
	}
	// A LID-addressed sender: SenderAlt is the stanza's sender_pn, so both.
	ev := dm("3A01b", text("hola"))
	ev.Info.Chat, ev.Info.Sender, ev.Info.SenderAlt = peerLID, peerLID, peerPN
	if got := one(t, e, f, ev).Sender.Aliases; !reflect.DeepEqual(got, []string{"wa:lid:123456789", "wa:+34611111111"}) {
		t.Fatalf("LID-addressed aliases = %v", got)
	}
	if env.Sender.DisplayName != "Ana" || env.Sender.FromSelf || !env.Mentioned || env.Message.Text != "hola" {
		t.Fatalf("envelope = %+v", env)
	}
	if !env.Message.CreatedAt.Equal(time.Unix(1_800_000_000, 0)) {
		t.Fatalf("created at = %v", env.Message.CreatedAt)
	}
}

func TestInboundConversationPrefersThePhoneNumber(t *testing.T) {
	e, f := connected(t)
	// LID addressing with the number in SenderAlt: the DM keeps the number.
	ev := dm("3A02", text("x"))
	ev.Info.Chat, ev.Info.Sender, ev.Info.SenderAlt = peerLID, peerLID, peerPN
	if got := one(t, e, f, ev).Origin.Conversation; got != "34611111111@s.whatsapp.net" {
		t.Fatalf("conversation = %q", got)
	}
	// The number hidden: the LID alone, and only it as an alias.
	ev = dm("3A03", text("x"))
	ev.Info.Chat, ev.Info.Sender, ev.Info.SenderAlt = peerLID, peerLID, types.JID{}
	env := one(t, e, f, ev)
	if env.Origin.Conversation != "123456789@lid" || !reflect.DeepEqual(env.Sender.Aliases, []string{"wa:lid:123456789"}) {
		t.Fatalf("LID-only origin = %+v, aliases %v", env.Origin, env.Sender.Aliases)
	}
}

func TestInboundFromSelf(t *testing.T) {
	e, f := connected(t)
	ev := dm("3A04", text("typed on the phone"))
	ev.Info.IsFromMe = true
	ev.Info.Sender, ev.Info.SenderAlt = types.NewJID(number, types.DefaultUserServer), types.JID{}
	if env := one(t, e, f, ev); !env.Sender.FromSelf {
		t.Fatal("FromSelf not set on the account's own message")
	}
	if got := e.a.takeUnread("34611111111@s.whatsapp.net"); len(got) != 0 {
		t.Fatalf("own message queued for a read receipt: %v", got)
	}
}

// TestAckAfterStore: a failed Inbound withholds the ack, so WhatsApp
// redelivers; the redelivery is stored once, and a further one is acked
// without a second envelope.
func TestAckAfterStore(t *testing.T) {
	e, f := connected(t)
	e.inMu.Lock()
	e.inErr = []error{errors.New("disk full")}
	e.inMu.Unlock()
	ev := dm("3A05", text("hola"))
	if f.emit(ev) {
		t.Fatal("acked a message Inbound did not store")
	}
	if n := len(e.stored()); n != 0 {
		t.Fatalf("stored %d envelopes", n)
	}
	if !f.emit(ev) {
		t.Fatal("the redelivery was not acked")
	}
	if !f.emit(ev) {
		t.Fatal("a duplicate was not acked")
	}
	if n := len(e.stored()); n != 1 {
		t.Fatalf("stored %d envelopes, want 1", n)
	}
}

// TestDuplicateFromCoreIsAcked: a message core already handled is acked with
// no read receipt queued, and every copy reaches core. The envelope carries
// the adapter's dedupe keys.
func TestDuplicateFromCoreIsAcked(t *testing.T) {
	e, f := connected(t)
	keys := one(t, e, f, dm("3A06", text("hola"))).DedupeKeys
	if want := []string{"34611111111@s.whatsapp.net/3A06", "123456789@lid/3A06"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("dedupe keys = %v, want %v", keys, want)
	}
	_ = e.a.takeUnread("34611111111@s.whatsapp.net")

	e.inMu.Lock()
	e.inErr = []error{channel.ErrDuplicate}
	e.inMu.Unlock()
	ev := dm("3A07", text("hola"))
	if !f.emit(ev) {
		t.Fatal("a duplicate was not acked")
	}
	if q := e.a.takeUnread("34611111111@s.whatsapp.net"); len(q) != 0 {
		t.Fatalf("duplicate queued for a read receipt: %v", q)
	}
	if !f.emit(ev) {
		t.Fatal("the next copy was not acked")
	}
	if n := len(e.stored()); n != 2 {
		t.Fatalf("stored %d envelopes, want 2: core decides, not the adapter", n)
	}
}

func TestDedupeKeysWithoutSender(t *testing.T) {
	a := New(Options{})
	if keys := a.dedupeKeys(types.MessageInfo{ID: "X"}); keys != nil {
		t.Fatalf("keys = %v, want none so core uses PlatformMsgID", keys)
	}
}

// TestDedupeAcrossAddressing replays the incident: one message arrives
// phone-addressed (SenderAlt is the LID, from the stanza or whatsmeow's LID
// map), then again as a retry, LID-addressed and without sender_pn. The
// conversation id changes between the two; the copy is still dropped.
func TestDedupeAcrossAddressing(t *testing.T) {
	e, f := connected(t)
	first := dm("3BCD", text("invite-code"))
	first.Info.Sender, first.Info.SenderAlt = peerPN, types.JID{User: peerLID.User, Server: types.HiddenUserServer, Device: 30}
	if got := one(t, e, f, first).Origin.PlatformMsgID; got != "34611111111@s.whatsapp.net/3BCD" {
		t.Fatalf("first platform id = %q", got)
	}
	retry := dm("3BCD", text("invite-code"))
	retry.Info.Chat, retry.Info.Sender, retry.Info.SenderAlt = peerLID, types.JID{User: peerLID.User, Server: types.HiddenUserServer, Device: 30}, types.JID{}
	if !f.emit(retry) {
		t.Fatal("the duplicate was not acked")
	}
	// And the other way round: LID-addressed with sender_pn first, then
	// phone-addressed with nothing else.
	lidFirst := dm("3BCE", text("x"))
	lidFirst.Info.Chat, lidFirst.Info.Sender, lidFirst.Info.SenderAlt = peerLID, peerLID, peerPN
	one(t, e, f, lidFirst)
	pnOnly := dm("3BCE", text("x"))
	pnOnly.Info.SenderAlt = types.JID{}
	if !f.emit(pnOnly) {
		t.Fatal("the duplicate was not acked")
	}
	if n := len(e.stored()); n != 2 {
		t.Fatalf("stored %d envelopes, want 2", n)
	}
	// A third copy: phone-addressed with no known LID, so the dedupe key is
	// {P/X} only; a LID retry carrying sender_pn goes to core with {L/X, P/X},
	// so core drops it on {P/X} and records {L/X}; a further LID-only retry
	// (no sender_pn, so only {L/X}) is then dropped as well, not routed as
	// new. The gate test TestThirdCopyAfterRestartIsDropped does the same
	// over a restart.
	phoneOnly := dm("3BCF", text("y"))
	phoneOnly.Info.Sender, phoneOnly.Info.SenderAlt = peerPN, types.JID{}
	one(t, e, f, phoneOnly)
	lidWithPN := dm("3BCF", text("y"))
	lidWithPN.Info.Chat, lidWithPN.Info.Sender, lidWithPN.Info.SenderAlt = peerLID, peerLID, peerPN
	if !f.emit(lidWithPN) {
		t.Fatal("the duplicate was not acked")
	}
	lidOnly := dm("3BCF", text("y"))
	lidOnly.Info.Chat, lidOnly.Info.Sender, lidOnly.Info.SenderAlt = peerLID, peerLID, types.JID{}
	if !f.emit(lidOnly) {
		t.Fatal("the third copy was not acked")
	}
	if n := len(e.stored()); n != 3 {
		t.Fatalf("stored %d envelopes, want 3", n)
	}
	// Every copy reached core, so each duplicate is traced there.
	if n := e.duplicates(); n != 4 {
		t.Fatalf("core saw %d duplicates, want 4", n)
	}
}

// TestDedupeIsPerSender: message ids are unique per sender only, so two
// senders' messages with one id are both stored, in a DM and in a group,
// and one group id in two groups is two messages.
func TestDedupeIsPerSender(t *testing.T) {
	e, f := connected(t)
	other := types.NewJID("34622222222", types.DefaultUserServer)
	one(t, e, f, dm("3C01", text("a")))
	ev := dm("3C01", text("b"))
	ev.Info.Chat, ev.Info.Sender, ev.Info.SenderAlt = other, other, types.JID{}
	one(t, e, f, ev)

	one(t, e, f, inGroup("3C02", text("a")))
	ev = inGroup("3C02", text("b"))
	ev.Info.Sender, ev.Info.SenderAlt = types.NewJID("555", types.HiddenUserServer), types.JID{}
	one(t, e, f, ev)
	ev = inGroup("3C02", text("c"))
	ev.Info.Chat = types.NewJID("120363000000000002", types.GroupServer)
	one(t, e, f, ev)
	// The first group message again, now phone-addressed: a copy.
	ev = inGroup("3C02", text("a"))
	ev.Info.Sender, ev.Info.SenderAlt = peerPN, peerLID
	if !f.emit(ev) || len(e.stored()) != 5 {
		t.Fatalf("group copy: stored %d envelopes, want 5", len(e.stored()))
	}
}

// TestDedupeFromSelf: the account's own messages are keyed on its own ids,
// whichever one the stanza names.
func TestDedupeFromSelf(t *testing.T) {
	e, f := connected(t)
	f.mu.Lock()
	f.lid = botLID
	f.mu.Unlock()
	ev := dm("3D01", text("typed on the phone"))
	ev.Info.IsFromMe = true
	ev.Info.Sender, ev.Info.SenderAlt = types.NewJID(number, types.DefaultUserServer), types.JID{}
	one(t, e, f, ev)
	ev = dm("3D01", text("typed on the phone"))
	ev.Info.IsFromMe = true
	ev.Info.Chat, ev.Info.Sender, ev.Info.SenderAlt = peerLID, botLID, types.JID{}
	if !f.emit(ev) || len(e.stored()) != 1 {
		t.Fatalf("own copy: stored %d envelopes, want 1", len(e.stored()))
	}
}

func TestInboundWithoutRunWithholdsTheAck(t *testing.T) {
	a := New(Options{})
	if a.handler(0)(dm("3A06", text("x"))) {
		t.Fatal("a message was acked with no Run to store it")
	}
}

func TestGroupMentions(t *testing.T) {
	e, f := connected(t)
	f.mu.Lock()
	f.lid = botLID
	f.mu.Unlock()
	bot := types.NewJID(number, types.DefaultUserServer).String()

	plain := one(t, e, f, inGroup("G1", text("just chatting")))
	if plain.Mentioned || plain.Origin.Conversation != group.String() || plain.Origin.ConversationKind != event.ConversationRoom {
		t.Fatalf("plain group message = %+v", plain)
	}
	if !reflect.DeepEqual(plain.Sender.Aliases, []string{"wa:lid:123456789", "wa:+34611111111"}) {
		t.Fatalf("group aliases = %v", plain.Sender.Aliases)
	}

	byNumber := one(t, e, f, inGroup("G2", extText("@"+number+" K7Q2-ABCD", &waE2E.ContextInfo{MentionedJID: []string{bot}})))
	if !byNumber.Mentioned || byNumber.Message.Text != "K7Q2-ABCD" {
		t.Fatalf("mention by number = %v %q, want a bare code", byNumber.Mentioned, byNumber.Message.Text)
	}
	byLID := one(t, e, f, inGroup("G3", extText("hey @987654321 look", &waE2E.ContextInfo{MentionedJID: []string{botLID.String()}})))
	if !byLID.Mentioned || byLID.Message.Text != "hey look" {
		t.Fatalf("mention by LID = %v %q", byLID.Mentioned, byLID.Message.Text)
	}
	quote := one(t, e, f, inGroup("G4", extText("and this?", &waE2E.ContextInfo{StanzaID: ptr("3EB01"), Participant: ptr(number + ":3@s.whatsapp.net")})))
	if !quote.Mentioned {
		t.Fatal("a quote of the bot's message is not a mention")
	}
	other := one(t, e, f, inGroup("G5", extText("@34622222222 hi", &waE2E.ContextInfo{MentionedJID: []string{"34622222222@s.whatsapp.net"}})))
	if other.Mentioned || other.Message.Text != "@34622222222 hi" {
		t.Fatalf("someone else's mention = %v %q", other.Mentioned, other.Message.Text)
	}
	longer := stripSelfMention("@"+number+"9 hi", types.NewJID(number, types.DefaultUserServer), types.JID{})
	if longer != "@"+number+"9 hi" {
		t.Fatalf("a longer number was stripped: %q", longer)
	}
}

func TestInboundTextSourcesAndWrappers(t *testing.T) {
	e, f := connected(t)
	sink := &blobSink{}
	e.a.putBlob = sink.put
	f.mu.Lock()
	f.media = []byte("\x89PNG....")
	f.mu.Unlock()

	if got := one(t, e, f, dm("T1", extText("extended", nil))).Message.Text; got != "extended" {
		t.Fatalf("extended text = %q", got)
	}
	img := one(t, e, f, dm("T2", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: ptr("a caption"), Mimetype: ptr("image/png")}}))
	if img.Message.Text != "a caption" || len(img.Message.Attachments) != 1 || img.Message.Attachments[0].Kind != event.KindImage {
		t.Fatalf("image = %+v", img.Message)
	}
	// Disappearing-message wrapper, unwrapped the way whatsmeow does it.
	eph := dm("T3", nil)
	eph.RawMessage = &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{Message: extText("vanishing", &waE2E.ContextInfo{Expiration: ptr(uint32(604800))})}}
	if got := one(t, e, f, eph.UnwrapRaw()).Message.Text; got != "vanishing" {
		t.Fatalf("ephemeral text = %q", got)
	}
	// View-once media: never downloaded, a stub instead.
	before := f.count(&f.downloads)
	vo := dm("T4", nil)
	vo.RawMessage = &waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: ptr("look")}}}}
	env := one(t, e, f, vo.UnwrapRaw())
	if env.Message.Text != "look\n[view-once media, not shown]" || len(env.Message.Attachments) != 0 || f.count(&f.downloads) != before {
		t.Fatalf("view-once = %q, %d attachments, downloads %d→%d", env.Message.Text, len(env.Message.Attachments), before, f.count(&f.downloads))
	}
}

func TestInboundMedia(t *testing.T) {
	e, f := connected(t)
	sink := &blobSink{}
	e.a.putBlob = sink.put
	e.a.maxDownload = 16
	f.mu.Lock()
	f.media = []byte("OggS-voice")
	f.mu.Unlock()

	voice := one(t, e, f, dm("M1", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{PTT: ptr(true), Mimetype: ptr("audio/ogg; codecs=opus")}}))
	att := voice.Message.Attachments
	if len(att) != 1 || att[0].Name != "voice.ogg" || att[0].Kind != event.KindAudio || !bytes.Equal(sink.blobs[att[0].BlobID], []byte("OggS-voice")) {
		t.Fatalf("voice note = %+v", att)
	}
	doc := one(t, e, f, dm("M2", &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: ptr("../../etc/report.pdf"), Mimetype: ptr("application/pdf")}}))
	if a := doc.Message.Attachments; len(a) != 1 || a[0].Name != "report.pdf" || a[0].Kind != event.KindDocument {
		t.Fatalf("document = %+v", a)
	}
	for id, m := range map[string]*waE2E.Message{
		"M3": {VideoMessage: &waE2E.VideoMessage{Mimetype: ptr("video/mp4")}},
		"M4": {StickerMessage: &waE2E.StickerMessage{Mimetype: ptr("image/webp")}},
	} {
		if a := one(t, e, f, dm(id, m)).Message.Attachments; len(a) != 1 {
			t.Fatalf("%s: attachments = %+v", id, a)
		}
	}

	// Over the cap by the declared length: not downloaded at all.
	before := f.count(&f.downloads)
	big := one(t, e, f, dm("M5", &waE2E.Message{VideoMessage: &waE2E.VideoMessage{FileLength: ptr(uint64(1 << 30))}}))
	if big.Message.Text != "[file too large: video.mp4]" || f.count(&f.downloads) != before {
		t.Fatalf("declared too large = %q", big.Message.Text)
	}
	// Over the cap once downloaded.
	f.mu.Lock()
	f.media = bytes.Repeat([]byte("x"), 17)
	f.mu.Unlock()
	if got := one(t, e, f, dm("M6", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: ptr("pic")}})).Message.Text; got != "pic\n[file too large: photo.jpg]" {
		t.Fatalf("downloaded too large = %q", got)
	}
	// A failed download or blob store is a note, and the message is still stored.
	f.mu.Lock()
	f.media, f.downloadErr = nil, errors.New("media gone")
	f.mu.Unlock()
	if got := one(t, e, f, dm("M7", &waE2E.Message{AudioMessage: &waE2E.AudioMessage{}})).Message.Text; got != "[file unavailable: audio]" {
		t.Fatalf("failed download = %q", got)
	}
	f.mu.Lock()
	f.media, f.downloadErr = []byte("ok"), nil
	f.mu.Unlock()
	sink.err = errors.New("blob store down")
	if got := one(t, e, f, dm("M8", &waE2E.Message{ImageMessage: &waE2E.ImageMessage{}})).Message.Text; got != "[file unavailable: photo.jpg]" {
		t.Fatalf("failed blob store = %q", got)
	}
}

func TestInboundStubs(t *testing.T) {
	e, f := connected(t)
	loc := one(t, e, f, dm("S1", &waE2E.Message{LocationMessage: &waE2E.LocationMessage{DegreesLatitude: ptr(40.4168), DegreesLongitude: ptr(-3.7038), Name: ptr("Sol")}}))
	if loc.Message.Text != "[location: 40.41680, -3.70380 Sol]" {
		t.Fatalf("location = %q", loc.Message.Text)
	}
	card := one(t, e, f, dm("S2", &waE2E.Message{ContactMessage: &waE2E.ContactMessage{DisplayName: ptr("Luis\nGarcía"), Vcard: ptr("BEGIN:VCARD…")}}))
	if card.Message.Text != "[contact card: Luis García]" {
		t.Fatalf("contact = %q", card.Message.Text)
	}
}

func TestDroppedKinds(t *testing.T) {
	e, f := connected(t)
	const secret = "SECRET-CONTENT"
	edit := dm("D-edit", text(secret))
	edit.IsEdit = true
	status := dm("D-status", text(secret))
	status.Info.Chat = types.StatusBroadcastJID
	list := dm("D-list", text(secret))
	list.Info.Chat = types.NewJID("1234567", types.BroadcastServer)
	news := dm("D-news", text(secret))
	news.Info.Chat = types.NewJID("1203630000", types.NewsletterServer)
	for _, ev := range []*events.Message{
		edit, status, list, news,
		dm("D-revoke", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Type: waE2E.ProtocolMessage_REVOKE.Enum()}}),
		dm("D-reaction", &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: ptr("👍")}}),
		dm("D-poll", &waE2E.Message{PollCreationMessageV3: &waE2E.PollCreationMessage{Name: ptr(secret)}}),
		dm("D-vote", &waE2E.Message{PollUpdateMessage: &waE2E.PollUpdateMessage{}}),
		dm("D-unsupported", &waE2E.Message{}),
	} {
		if !f.emit(ev) {
			t.Fatalf("%s: a dropped message was not acked", ev.Info.ID)
		}
		if !strings.Contains(e.logs.String(), "id="+ev.Info.ID) {
			t.Fatalf("%s: drop not logged with its id:\n%s", ev.Info.ID, e.logs.String())
		}
	}
	f.emit(&events.HistorySync{})
	// The call is rejected off the handler goroutine (§5.1 Calls), so the log
	// line lands a moment after emit returns.
	f.emit(&events.CallOffer{BasicCallMeta: types.BasicCallMeta{CallID: "CALL1"}})
	eventually(t, "the call to be rejected", func() bool { return len(f.rejectedCalls()) > 0 })
	eventually(t, "the call rejection to be logged", func() bool { return strings.Contains(e.logs.String(), "id=CALL1") })
	if n := len(e.stored()); n != 0 {
		t.Fatalf("dropped kinds stored %d envelopes", n)
	}
	if strings.Contains(e.logs.String(), secret) {
		t.Fatalf("drop log = %s", e.logs.String())
	}
	if msgs := f.sentMsgs(); len(msgs) != 0 {
		t.Fatalf("a stranger's call got a message: %+v", msgs)
	}
	if got := e.a.takeUnread("34611111111@s.whatsapp.net"); len(got) != 0 {
		t.Fatalf("dropped messages queued for read receipts: %v", got)
	}
}

func TestJoinedGroup(t *testing.T) {
	e, f := connected(t)
	type join struct {
		chat string
		by   []string
	}
	var mu sync.Mutex
	var got []join
	e.a.onJoined = func(_ context.Context, chat string, by []string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, join{chat, by})
	}
	pn := peerPN.ToNonAD()
	f.emit(&events.JoinedGroup{Sender: &peerLID, SenderPN: &pn, GroupInfo: types.GroupInfo{JID: group}})
	f.emit(&events.JoinedGroup{GroupInfo: types.GroupInfo{JID: group}})
	mu.Lock()
	defer mu.Unlock()
	want := []join{{group.String(), []string{"wa:lid:123456789", "wa:+34611111111"}}, {group.String(), nil}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("joins = %+v, want %+v", got, want)
	}
}

// memState is a StateStore in memory.
type memState struct {
	mu sync.Mutex
	m  map[string]string
}

func (s *memState) Get(_ context.Context, k string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	return v, ok, nil
}

func (s *memState) Set(_ context.Context, k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]string{}
	}
	s.m[k] = v
	return nil
}

func TestExpirationIsRememberedPerConversation(t *testing.T) {
	e, f := connected(t)
	st := &memState{}
	e.a.state = st
	conv := "34611111111@s.whatsapp.net"
	one(t, e, f, dm("E1", extText("shh", &waE2E.ContextInfo{Expiration: ptr(uint32(86400))})))
	if v, _, _ := st.Get(t.Context(), "expiration:"+conv); v != "86400" {
		t.Fatalf("stored expiration = %q", v)
	}
	// A fresh adapter over the same state (a restart) still knows it.
	b := New(Options{State: st})
	if got := b.expiration(t.Context(), conv); got != 86400 {
		t.Fatalf("expiration after restart = %d", got)
	}
	// The chat turning the timer off clears it.
	f.emit(dm("E2", &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_EPHEMERAL_SETTING.Enum(), EphemeralExpiration: ptr(uint32(0)),
	}}))
	if got := e.a.expiration(t.Context(), conv); got != 0 {
		t.Fatalf("expiration after the setting changed = %d", got)
	}
}
