package whatsapp

import (
	"errors"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

const (
	dmConv    = "34611111111@s.whatsapp.net"
	groupConv = "120363000000000001@g.us"
)

func reactMsg(target, emoji string) *waE2E.Message {
	return &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key: &waCommon.MessageKey{ID: ptr(target)}, Text: ptr(emoji),
	}}
}

func TestInboundReactionEnvelopes(t *testing.T) {
	e, f := connected(t)
	if !f.emit(dm("R1", reactMsg("M1", "👍"))) || !f.emit(dm("R2", reactMsg("M1", ""))) ||
		!f.emit(inGroup("R3", groupReact("M2", "🔥", botPN))) {
		t.Fatal("a reaction was not acked")
	}
	got := e.stored()
	if len(got) != 3 {
		t.Fatalf("stored %d envelopes", len(got))
	}
	add, rm, grp := got[0], got[1], got[2]
	if add.Reaction == nil || *add.Reaction != (event.Reaction{TargetMsgID: dmConv + "/M1", Emoji: "👍"}) {
		t.Fatalf("add = %+v", add.Reaction)
	}
	if add.Origin.PlatformMsgID != dmConv+"/r:R1" || add.Origin.Conversation != dmConv ||
		add.Origin.ConversationKind != event.ConversationDM || add.Mentioned {
		t.Fatalf("origin = %+v", add.Origin)
	}
	if len(add.Sender.Aliases) != 1 || add.Sender.Aliases[0] != "wa:+34611111111" || add.Sender.DisplayName != "Ana" {
		t.Fatalf("sender = %+v", add.Sender)
	}
	if a := grp.Sender.Aliases; len(a) != 2 || a[0] != "wa:lid:123456789" || a[1] != "wa:+34611111111" {
		t.Fatalf("group sender = %+v", grp.Sender)
	}
	if add.Message.Text != "" || len(add.Message.Attachments) != 0 {
		t.Fatalf("a reaction carries content: %+v", add.Message)
	}
	if rm.Reaction == nil || *rm.Reaction != (event.Reaction{TargetMsgID: dmConv + "/M1", Removed: true}) {
		t.Fatalf("remove = %+v", rm.Reaction)
	}
	if grp.Reaction == nil || *grp.Reaction != (event.Reaction{TargetMsgID: groupConv + "/M2", Emoji: "🔥"}) ||
		grp.Origin.ConversationKind != event.ConversationRoom || grp.Origin.PlatformMsgID != groupConv+"/r:R3" {
		t.Fatalf("group = %+v / %+v", grp.Reaction, grp.Origin)
	}
	if len(e.a.takeUnread(dmConv)) != 0 {
		t.Fatal("a reaction queued a read receipt")
	}
}

func TestInboundReactionDedupeAndStoreFailure(t *testing.T) {
	e, f := connected(t)
	e.inErr = []error{errors.New("store down")}
	if f.emit(dm("R1", reactMsg("M1", "👍"))) {
		t.Fatal("a reaction that was not stored was acked")
	}
	for range 2 {
		if !f.emit(dm("R1", reactMsg("M1", "👍"))) {
			t.Fatal("redelivery not acked")
		}
	}
	if n := len(e.stored()); n != 1 {
		t.Fatalf("stored %d envelopes, want 1", n)
	}
}

func TestUnroutableReactionsAreDropped(t *testing.T) {
	e, f := connected(t)
	enc := &waE2E.Message{EncReactionMessage: &waE2E.EncReactionMessage{}}
	noKey := &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Text: ptr("👍")}}
	for _, ev := range []string{"enc", "nokey"} {
		m := enc
		if ev == "nokey" {
			m = noKey
		}
		if !f.emit(dm(ev, m)) {
			t.Fatalf("%s: dropped reaction not acked", ev)
		}
	}
	if n := len(e.stored()); n != 0 {
		t.Fatalf("stored %d envelopes", n)
	}
}

func TestReactDM(t *testing.T) {
	e, f := connected(t)
	f.emit(dm("M1", text("hi")))
	err := e.a.React(t.Context(), event.Origin{Conversation: dmConv, PlatformMsgID: dmConv + "/M1"}, "👍")
	if err != nil {
		t.Fatal(err)
	}
	sends := f.sentMsgs()
	if len(sends) != 1 || sends[0].to.String() != dmConv {
		t.Fatalf("sends = %+v", sends)
	}
	rm := sends[0].msg.GetReactionMessage()
	if rm.GetText() != "👍" || rm.GetKey().GetID() != "M1" || rm.GetKey().GetFromMe() || rm.GetKey().GetParticipant() != "" {
		t.Fatalf("reaction = %+v", rm)
	}
}

func TestReactGroupUsesRememberedSender(t *testing.T) {
	e, f := connected(t)
	f.emit(inGroup("G1", text("hi")))
	if err := e.a.React(t.Context(), event.Origin{Conversation: groupConv, PlatformMsgID: groupConv + "/G1"}, ""); err != nil {
		t.Fatal(err)
	}
	rm := f.sentMsgs()[0].msg.GetReactionMessage()
	if rm.GetText() != "" || rm.GetKey().GetFromMe() || rm.GetKey().GetParticipant() != peerLID.String() {
		t.Fatalf("reaction = %+v", rm)
	}
}

func TestReactToOwnMessage(t *testing.T) {
	e, f := connected(t)
	rec, err := e.a.Send(t.Context(), channel.Outbound{Target: event.Origin{Channel: "wa", Conversation: groupConv}, Message: event.Message{Text: "hello"}})
	if err != nil || len(rec.PlatformMsgIDs) != 1 {
		t.Fatalf("send: %v %+v", err, rec)
	}
	target := event.Origin{Conversation: groupConv, PlatformMsgID: rec.PlatformMsgIDs[0]}
	if err := e.a.React(t.Context(), target, "✅"); err != nil {
		t.Fatal(err)
	}
	sends := f.sentMsgs()
	rm := sends[len(sends)-1].msg.GetReactionMessage()
	if !rm.GetKey().GetFromMe() || rm.GetKey().GetParticipant() != "" || rm.GetText() != "✅" ||
		!strings.HasSuffix(target.PlatformMsgID, "/"+rm.GetKey().GetID()) {
		t.Fatalf("reaction = %+v", rm)
	}
}

func TestReactErrors(t *testing.T) {
	e, f := connected(t)
	ctx := t.Context()
	// An unknown message in a group has no known sender.
	if err := e.a.React(ctx, event.Origin{Conversation: groupConv, PlatformMsgID: groupConv + "/nope"}, "👍"); err == nil {
		t.Fatal("reacted to a group message with an unknown sender")
	}
	for _, id := range []string{"", "M1", dmConv + "/", "/M1", groupConv + "/M1"} {
		if err := e.a.React(ctx, event.Origin{Conversation: dmConv, PlatformMsgID: id}, "👍"); err == nil {
			t.Fatalf("target %q accepted", id)
		}
	}
	if err := e.a.React(ctx, event.Origin{Conversation: "bad@", PlatformMsgID: "bad@/M1"}, "👍"); err == nil {
		t.Fatal("bad conversation accepted")
	}
	if n := len(f.sentMsgs()); n != 0 {
		t.Fatalf("a refused reaction sent %d messages", n)
	}
	f.sendErr = []error{errors.New("boom")}
	if err := e.a.React(ctx, event.Origin{Conversation: dmConv, PlatformMsgID: dmConv + "/M1"}, "👍"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("platform error = %v", err)
	}
}

func TestReactNotRunning(t *testing.T) {
	e := newEnv(t, number, nil)
	if err := e.a.React(t.Context(), event.Origin{Conversation: dmConv, PlatformMsgID: dmConv + "/M1"}, "👍"); err == nil {
		t.Fatal("reacted while not running")
	}
}

func TestLimitsReactionsAnyEmoji(t *testing.T) {
	e := newEnv(t, number, nil)
	if e.a.Limits().Reactions != nil {
		t.Fatal("WhatsApp accepts any emoji")
	}
}
