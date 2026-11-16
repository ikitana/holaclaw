package whatsapp

import (
	"errors"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// TestCallFromStrangerIsRejectedSilently covers the ban-risk rule of §7: a
// stranger the gate never saw gets nothing beyond the reject, not even the
// callNotice text.
func TestCallFromStrangerIsRejectedSilently(t *testing.T) {
	e, f := connected(t)
	e.a.state = &memState{}
	f.emit(&events.CallOffer{BasicCallMeta: types.BasicCallMeta{From: peerPN, CallCreator: peerPN, CallID: "C-stranger"}})
	eventually(t, "the call to be rejected", func() bool { return len(f.rejectedCalls()) > 0 })
	rej := f.rejectedCalls()
	if len(rej) != 1 || rej[0].id != "C-stranger" || rej[0].from != peerPN.ToNonAD() {
		t.Fatalf("rejected = %+v", rej)
	}
	if msgs := f.sentMsgs(); len(msgs) != 0 {
		t.Fatalf("a stranger's call got a message: %+v", msgs)
	}
}

// TestCallFromKnownPeerGetsNotice covers the other half of §7: a caller who
// already had a turn with the bot — recorded when Typing marked their
// messages read — gets the reject and the callNotice text in the DM.
func TestCallFromKnownPeerGetsNotice(t *testing.T) {
	e, f := connected(t)
	st := &memState{}
	e.a.state = st
	e.a.rememberPeer(t.Context(), peerPN, types.JID{})

	f.emit(&events.CallOffer{BasicCallMeta: types.BasicCallMeta{From: peerPN, CallCreator: peerPN, CallID: "C-known"}})
	eventually(t, "the call to be rejected", func() bool { return len(f.rejectedCalls()) > 0 })
	eventually(t, "the notice to be sent", func() bool { return len(f.sentMsgs()) > 0 })

	msgs := f.sentMsgs()
	if len(msgs) != 1 || msgs[0].to != peerPN.ToNonAD() || msgs[0].msg.GetConversation() != callNotice {
		t.Fatalf("sent = %+v", msgs)
	}
}

// TestCallFromKnownPeerByAltID covers a caller addressed by LID whose
// phone-number alias is the one the bot remembers (or the reverse): either
// id recorded by rememberPeer is enough to recognise the caller.
func TestCallFromKnownPeerByAltID(t *testing.T) {
	e, f := connected(t)
	e.a.state = &memState{}
	// Only the phone-number id was ever recorded...
	e.a.rememberPeer(t.Context(), peerPN, types.JID{})
	// ...but the call names the peer by LID, with the phone number as its alt.
	f.emit(&events.CallOffer{BasicCallMeta: types.BasicCallMeta{
		From: peerLID, CallCreator: peerLID, CallCreatorAlt: peerPN, CallID: "C-alt",
	}})
	eventually(t, "the notice to be sent", func() bool { return len(f.sentMsgs()) > 0 })
	if msgs := f.sentMsgs(); len(msgs) != 1 || msgs[0].to != peerPN.ToNonAD() {
		t.Fatalf("sent = %+v", msgs)
	}
}

// TestGroupCallRejectedOnly covers the group half of §5.1 Calls: even a
// known peer's group call gets no message, only the reject.
func TestGroupCallRejectedOnly(t *testing.T) {
	e, f := connected(t)
	e.a.state = &memState{}
	e.a.rememberPeer(t.Context(), peerPN, types.JID{})
	f.emit(&events.CallOfferNotice{BasicCallMeta: types.BasicCallMeta{
		From: peerPN, CallCreator: peerPN, CallID: "C-group", GroupJID: group,
	}})
	eventually(t, "the call to be rejected", func() bool { return len(f.rejectedCalls()) > 0 })
	if msgs := f.sentMsgs(); len(msgs) != 0 {
		t.Fatalf("a group call got a message: %+v", msgs)
	}
}

// TestCallRejectErrorLoggedNoPanic covers a RejectCall failure: it is logged
// at debug with the call id and nothing else, and the goroutine does not
// panic — a known peer still gets the notice.
func TestCallRejectErrorLoggedNoPanic(t *testing.T) {
	e, f := connected(t)
	e.a.state = &memState{}
	e.a.rememberPeer(t.Context(), peerPN, types.JID{})
	f.mu.Lock()
	f.rejectErr = errors.New("network down")
	f.mu.Unlock()
	f.emit(&events.CallOffer{BasicCallMeta: types.BasicCallMeta{From: peerPN, CallCreator: peerPN, CallID: "C-err"}})
	eventually(t, "the reject to be logged", func() bool { return strings.Contains(e.logs.String(), "id=C-err") })
	eventually(t, "the notice to be sent", func() bool { return len(f.sentMsgs()) > 0 })
	if strings.Contains(e.logs.String(), "network down") {
		t.Fatalf("the reject error leaked into the log: %s", e.logs.String())
	}
}

// TestCallOfferNoticeFromKnownPeer covers the CallOfferNotice variant for a
// direct call (no GroupJID): it gets the same treatment as CallOffer.
func TestCallOfferNoticeFromKnownPeer(t *testing.T) {
	e, f := connected(t)
	e.a.state = &memState{}
	e.a.rememberPeer(t.Context(), peerPN, types.JID{})
	f.emit(&events.CallOfferNotice{BasicCallMeta: types.BasicCallMeta{From: peerPN, CallCreator: peerPN, CallID: "C-notice"}})
	eventually(t, "the notice to be sent", func() bool { return len(f.sentMsgs()) > 0 })
	if msgs := f.sentMsgs(); len(msgs) != 1 || msgs[0].msg.GetConversation() != callNotice {
		t.Fatalf("sent = %+v", msgs)
	}
}
