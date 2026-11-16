package whatsapp

import (
	"strconv"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"holaclaw/internal/event"
)

var botPN = types.NewJID(number, types.DefaultUserServer)

// groupReact is a group reaction whose key names participant as the author.
func groupReact(target, emoji string, participant types.JID) *waE2E.Message {
	m := reactMsg(target, emoji)
	m.ReactionMessage.Key.Participant = ptr(participant.String())
	m.ReactionMessage.Key.RemoteJID = ptr(groupConv)
	return m
}

func fromMeKey(m *waE2E.Message) *waE2E.Message {
	m.ReactionMessage.Key.FromMe = ptr(true)
	return m
}

func TestReactionOnAnothersMessageIsDropped(t *testing.T) {
	e, f := connected(t)
	noPart := groupReact("D2", "👍", botPN)
	noPart.ReactionMessage.Key.Participant = nil
	elsewhere := groupReact("D3", "👍", botPN)
	elsewhere.ReactionMessage.Key.RemoteJID = ptr("120363999999999999@g.us")
	dmGroupKey := reactMsg("D4", "👍")
	dmGroupKey.ReactionMessage.Key.RemoteJID = ptr(groupConv)
	for _, m := range []*events.Message{
		inGroup("D1", groupReact("D1", "👍", peerLID)), // a third party's decoy
		inGroup("D2", noPart),
		inGroup("D3", elsewhere),
		dm("D4", dmGroupKey),
		dm("D5", fromMeKey(reactMsg("X", "👍"))), // the reactor's own message
	} {
		if !f.emit(m) {
			t.Fatalf("%s: dropped reaction not acked", m.Info.ID)
		}
	}
	if n := len(e.stored()); n != 0 {
		t.Fatalf("stored %d reactions on another's message", n)
	}
	if !strings.Contains(e.logs.String(), "reaction on another's message") {
		t.Fatalf("drop not logged:\n%s", e.logs.String())
	}

	// The bot's own id, even with a device, is the bot.
	devPN := botPN
	devPN.Device = 7
	f.emit(inGroup("A1", groupReact("P1", "👍", devPN)))
	if n := len(e.stored()); n != 1 {
		t.Fatalf("reaction on the bot's message stored %d times", n)
	}
	// The account's other device reacting: only on its own message counts.
	own := dm("A2", reactMsg("P1", "👍"))
	own.Info.IsFromMe = true
	f.emit(own)
	ownOK := dm("A3", fromMeKey(reactMsg("P1", "👍")))
	ownOK.Info.IsFromMe = true
	f.emit(ownOK)
	if got := e.stored(); len(got) != 2 || !got[1].Sender.FromSelf {
		t.Fatalf("stored = %d", len(got))
	}
}

func TestReactionBadEmojiIsDropped(t *testing.T) {
	e, f := connected(t)
	for i, em := range []string{"abc", "👍\n", strings.Repeat("👍", 17), "hi 👍", "٣"} {
		if !f.emit(dm("B"+strconv.Itoa(i), reactMsg("M1", em))) {
			t.Fatalf("%q not acked", em)
		}
	}
	if n := len(e.stored()); n != 0 {
		t.Fatalf("stored %d reactions with a bad emoji", n)
	}
	for _, ok := range []string{"👍", "❤️", "1️⃣", "#️⃣", "👨‍👩‍👧", "🇪🇸", "👍🏽"} {
		if !validEmoji(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	if validEmoji("") || validEmoji(strings.Repeat("👍", 17)) {
		t.Error("empty or oversized accepted")
	}
}

func TestNoteAuthorFirstWriterWins(t *testing.T) {
	a := newEnv(t, number, nil).a
	a.noteAuthor("c/1", peerLID)
	a.noteAuthor("c/1", peerPN) // a later message reusing the id
	if a.authors["c/1"] != peerLID {
		t.Fatalf("author replaced: %v", a.authors["c/1"])
	}
	a.noteAuthor("c/2", types.EmptyJID)
	a.noteAuthor("c/2", peerPN) // a member reusing the bot's sent id
	if !a.authors["c/2"].IsEmpty() {
		t.Fatalf("own entry replaced: %v", a.authors["c/2"])
	}
	a.noteAuthor("c/1", types.EmptyJID) // the bot's own send wins over a stranger's
	if !a.authors["c/1"].IsEmpty() {
		t.Fatal("own send did not replace a stranger's entry")
	}
}

func TestAuthorsAreBounded(t *testing.T) {
	a := newEnv(t, number, nil).a
	for i := range maxAuthors + 10 {
		a.noteAuthor("c/"+strconv.Itoa(i), peerLID)
	}
	if len(a.authors) > maxAuthors {
		t.Fatalf("authors hold %d, cap %d", len(a.authors), maxAuthors)
	}
	if _, ok := a.authors["c/0"]; ok {
		t.Fatal("the oldest author was kept")
	}
	if _, ok := a.authors["c/"+strconv.Itoa(maxAuthors+9)]; !ok {
		t.Fatal("the newest author was evicted")
	}
}

func TestReactToMessageFromOwnOtherDevice(t *testing.T) {
	e, f := connected(t)
	m := inGroup("S1", text("sent from the phone"))
	m.Info.IsFromMe = true
	f.emit(m)
	if err := e.a.React(t.Context(), event.Origin{Conversation: groupConv, PlatformMsgID: groupConv + "/S1"}, "👍"); err != nil {
		t.Fatal(err)
	}
	if rm := f.sentMsgs()[0].msg.GetReactionMessage(); !rm.GetKey().GetFromMe() || rm.GetKey().GetParticipant() != "" {
		t.Fatalf("reaction = %+v", rm)
	}
}
