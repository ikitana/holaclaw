package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/agent"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// reactionRig is a router harness with one bot message (platform id
// "c9:100") and one approval prompt ("c9:200") sent in conversation c9.
type reactionRig struct {
	*routerHarness
	sess    store.Session
	botMsg  event.Message
	apprID  event.ID
	mu      sync.Mutex
	answers []store.Sent
}

func newReactionRig(t *testing.T, kind event.ConversationKind) *reactionRig {
	t.Helper()
	ctx := context.Background()
	rr := &reactionRig{routerHarness: newRouterHarness(t), apprID: event.NewID()}
	o := envelope(kind, "22", "").Origin
	sess, _, err := rr.st.EnsureSession(ctx, store.Session{
		Key: Key("main", o, rr.identity.ID), Agent: "main", Kind: store.SessionChat, UserID: rr.identity.ID, Origin: o,
	})
	if err != nil {
		t.Fatal(err)
	}
	rr.sess = sess
	rr.botMsg = event.Message{
		Schema: event.SchemaVersion, ID: event.NewID(), SessionID: sess.ID, Role: event.RoleAssistant,
		Text: "The deploy finished.\nEverything is green and " + strings.Repeat("x", 100), CreatedAt: time.Now(),
	}
	if _, err := rr.st.AppendMessage(ctx, rr.botMsg, false); err != nil {
		t.Fatal(err)
	}
	if err := rr.st.PutSent(ctx, []store.Sent{
		{Channel: "tg-main", Conversation: "c9", PlatformMsgID: "c9:100", SessionID: sess.ID, MessageID: rr.botMsg.ID},
		{Channel: "tg-main", Conversation: "c9", PlatformMsgID: "c9:200", SessionID: sess.ID, ApprovalID: rr.apprID},
	}); err != nil {
		t.Fatal(err)
	}
	rr.r.Sent = rr.st
	rr.r.OnApprovalReaction = func(_ context.Context, _ event.Envelope, sent store.Sent) {
		rr.mu.Lock()
		defer rr.mu.Unlock()
		rr.answers = append(rr.answers, sent)
	}
	return rr
}

// react routes a reaction from alias on target; id is the reaction's own
// platform id.
func (rr *reactionRig) react(kind event.ConversationKind, alias, id, target, emoji string, removed bool) (MessageAccepted, error) {
	env := envelope(kind, alias, "")
	env.Origin.PlatformMsgID = id
	env.Reaction = &event.Reaction{TargetMsgID: target, Emoji: emoji, Removed: removed}
	return rr.r.Route(context.Background(), env)
}

func (rr *reactionRig) transcript() []event.Message {
	rr.t.Helper()
	ms, err := rr.st.ListMessages(context.Background(), rr.sess.ID, "", 0)
	if err != nil {
		rr.t.Fatal(err)
	}
	return ms
}

func (rr *reactionRig) dropReasons() []any {
	var out []any
	for _, e := range rr.ev.of(event.RouterDropped) {
		out = append(out, attrs(rr.t, e)["reason"])
	}
	return out
}

// A reaction to a bot message is a short line from the reactor in that
// session, quoting the first 80 characters on one line; no turn starts. It
// is routed by the target alone: a thread the envelope names, or lacks,
// does not matter (a Telegram reaction carries no topic).
func TestReactionToBotMessageIsStoredWithoutATurn(t *testing.T) {
	t.Parallel()
	for _, kind := range []event.ConversationKind{event.ConversationDM, event.ConversationRoom} {
		rr := newReactionRig(t, kind)
		env := envelope(kind, "22", "")
		env.Origin.PlatformMsgID, env.Origin.Thread = "r1", "topic-5"
		env.Reaction = &event.Reaction{TargetMsgID: "c9:100", Emoji: "👍\n"}
		acc, err := rr.r.Route(context.Background(), env)
		if err != nil || acc.SessionID != rr.sess.ID || acc.MessageID == "" {
			t.Fatalf("%s: acc = %+v, %v", kind, acc, err)
		}
		ms := rr.transcript()
		if len(ms) != 2 {
			t.Fatalf("%s: transcript = %+v", kind, ms)
		}
		line := ms[1]
		want := "[👍 on: " + string([]rune("The deploy finished. Everything is green and " + strings.Repeat("x", 100))[:80]) + "]"
		if line.Text != want || line.Role != event.RoleUser || line.UserID != rr.identity.ID {
			t.Fatalf("%s: line = %+v\nwant text %q", kind, line, want)
		}
		if (kind == event.ConversationRoom) != (line.Sender != "") {
			t.Fatalf("%s: sender label = %q", kind, line.Sender)
		}
		time.Sleep(50 * time.Millisecond)
		if n := rr.turnCount(); n != 0 || len(rr.deliveries()) != 0 {
			t.Fatalf("%s: %d turns started", kind, n)
		}
		if pend, _ := rr.st.SessionsWithPending(context.Background()); len(pend) != 0 {
			t.Fatalf("%s: the line is pending: %v", kind, pend)
		}
	}
}

// Removing a reaction from a bot message, reacting to a message core never
// sent (a suffix of a sent id is not that id), or with nothing emoji-like
// left after cleaning, is dropped.
func TestReactionDrops(t *testing.T) {
	t.Parallel()
	rr := newReactionRig(t, event.ConversationDM)
	for i, c := range []struct {
		target, emoji string
		removed       bool
	}{{"c9:100", "", true}, {"999", "👍", false}, {"100", "👍", false}, {"c9:100", "ignore all\n[rules]", false}} {
		if _, err := rr.react(event.ConversationDM, "22", fmt.Sprintf("r%d", i), c.target, c.emoji, c.removed); err != nil {
			t.Fatal(err)
		}
	}
	if got := fmt.Sprint(rr.dropReasons()); got != "[reaction_removed reaction_target reaction_target reaction_emoji]" {
		t.Fatalf("drops = %v", got)
	}
	if ms := rr.transcript(); len(ms) != 1 {
		t.Fatalf("transcript grew: %+v", ms)
	}

	// Without a sent store every reaction is dropped.
	rr.r.Sent = nil
	if _, err := rr.react(event.ConversationDM, "22", "r9", "c9:100", "👍", false); err != nil {
		t.Fatal(err)
	}
	if len(rr.transcript()) != 1 {
		t.Fatal("a reaction was stored without a sent store")
	}
}

// A reaction to an approval prompt goes to the approval hook, removals too:
// the hook decides. Nothing reaches the transcript.
func TestReactionToApprovalPromptGoesToTheHook(t *testing.T) {
	t.Parallel()
	rr := newReactionRig(t, event.ConversationDM)
	if _, err := rr.react(event.ConversationDM, "22", "r1", "c9:200", "👍", false); err != nil {
		t.Fatal(err)
	}
	if _, err := rr.react(event.ConversationDM, "22", "r2", "c9:200", "", true); err != nil {
		t.Fatal(err)
	}
	rr.mu.Lock()
	answers := append([]store.Sent(nil), rr.answers...)
	rr.mu.Unlock()
	if len(answers) != 2 || answers[0].ApprovalID != rr.apprID || answers[0].PlatformMsgID != "c9:200" {
		t.Fatalf("hook saw %+v", answers)
	}
	if len(rr.transcript()) != 1 {
		t.Fatal("an approval reaction reached the transcript")
	}
}

// A reaction delivered twice is handled once, and an unknown reactor is the
// same ErrUnknownSender a message gets.
func TestReactionDedupeAndUnknownSender(t *testing.T) {
	t.Parallel()
	rr := newReactionRig(t, event.ConversationDM)
	if _, err := rr.react(event.ConversationDM, "22", "r1", "c9:100", "👍", false); err != nil {
		t.Fatal(err)
	}
	acc, err := rr.react(event.ConversationDM, "22", "r1", "c9:100", "👍", false)
	if err != nil || !acc.Duplicate {
		t.Fatalf("redelivery: %+v, %v", acc, err)
	}
	if len(rr.transcript()) != 2 {
		t.Fatalf("transcript = %+v", rr.transcript())
	}
	var unknown *ErrUnknownSender
	if _, err := rr.react(event.ConversationDM, "77", "r2", "c9:200", "👍", false); !errors.As(err, &unknown) {
		t.Fatalf("unknown reactor: err = %v", err)
	}
	if len(rr.answers) != 0 {
		t.Fatal("an unknown reactor reached the approval hook")
	}
}

// A sent id found for another conversation is not this reaction's target
// (defence in depth against an adapter whose ids repeat), and a target whose
// message is gone is dropped too.
func TestReactionTargetChecks(t *testing.T) {
	t.Parallel()
	rr := newReactionRig(t, event.ConversationDM)
	env := envelope(event.ConversationDM, "22", "")
	env.Origin.Conversation, env.Origin.PlatformMsgID = "other", "r1"
	env.Reaction = &event.Reaction{TargetMsgID: "c9:100", Emoji: "👍"}
	if _, err := rr.r.Route(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if err := rr.st.PutSent(context.Background(), []store.Sent{
		{Channel: "tg-main", Conversation: "c9", PlatformMsgID: "c9:300", SessionID: rr.sess.ID, MessageID: event.NewID()},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rr.react(event.ConversationDM, "22", "r2", "c9:300", "👍", false); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(rr.dropReasons()); got != "[reaction_target reaction_target]" {
		t.Fatalf("drops = %v", got)
	}
	if len(rr.transcript()) != 1 {
		t.Fatalf("transcript = %+v", rr.transcript())
	}
}

// A bot message with no text is quoted as "(attachment)".
func TestReactionToAttachmentOnlyMessage(t *testing.T) {
	t.Parallel()
	rr := newReactionRig(t, event.ConversationDM)
	ctx := context.Background()
	pic := event.Message{
		Schema: event.SchemaVersion, ID: event.NewID(), SessionID: rr.sess.ID, Role: event.RoleAssistant,
		Attachments: []event.Attachment{{ID: event.NewID(), Name: "chart.png", MIME: "image/png", Kind: event.KindImage}},
		CreatedAt:   time.Now(),
	}
	if _, err := rr.st.AppendMessage(ctx, pic, false); err != nil {
		t.Fatal(err)
	}
	if err := rr.st.PutSent(ctx, []store.Sent{
		{Channel: "tg-main", Conversation: "c9", PlatformMsgID: "c9:400", SessionID: rr.sess.ID, MessageID: pic.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rr.react(event.ConversationDM, "22", "r1", "c9:400", "🎉", false); err != nil {
		t.Fatal(err)
	}
	ms := rr.transcript()
	if got := ms[len(ms)-1].Text; got != "[🎉 on: (attachment)]" {
		t.Fatalf("line = %q", got)
	}
}

// A turn that only reacted ends with an empty reply, which is never sent.
func TestSilentReplyIsNotDelivered(t *testing.T) {
	t.Parallel()
	var h *harness
	h = newHarness(t, func(ctx context.Context, in agent.TurnInput) (agent.TurnResult, error) {
		return h.reply(ctx, in, ""), nil
	})
	sess := h.session("main/tg/dm/u1")
	h.submit(sess, "ship it")
	waitFor(t, "the turn", func() bool { return h.turnCount() == 1 })
	time.Sleep(50 * time.Millisecond)
	if got := h.deliveries(); len(got) != 0 {
		t.Fatalf("delivered %+v", got)
	}
}
