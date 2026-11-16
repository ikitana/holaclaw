package telegram

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

	"holaclaw/internal/event"
)

func reactTarget() event.Origin {
	return event.Origin{Conversation: "1234", ConversationKind: event.ConversationDM, PlatformMsgID: "1234:55"}
}

func TestReactSetsAndClears(t *testing.T) {
	f := newFake(t)
	f.on("setMessageReaction", true)
	a, _, _ := newAdapter(t, f, nil)

	if err := a.React(t.Context(), reactTarget(), "👍"); err != nil {
		t.Fatal(err)
	}
	if err := a.React(t.Context(), reactTarget(), ""); err != nil {
		t.Fatal(err)
	}
	calls := f.seen("setMessageReaction")
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[0].str("chat_id") != "1234" || calls[0].Body["message_id"] != float64(55) {
		t.Fatalf("body = %v", calls[0].Body)
	}
	r, _ := calls[0].Body["reaction"].([]any)
	if len(r) != 1 || r[0].(map[string]any)["type"] != "emoji" || r[0].(map[string]any)["emoji"] != "👍" {
		t.Fatalf("reaction = %v", calls[0].Body["reaction"])
	}
	if r, _ := calls[1].Body["reaction"].([]any); r == nil || len(r) != 0 {
		t.Fatalf("clear reaction = %v", calls[1].Body["reaction"])
	}
}

func TestReactErrors(t *testing.T) {
	f := newFake(t)
	f.on("setMessageReaction", &fakeErr{Status: 400, Description: "Bad Request: REACTION_INVALID"})
	a, _, _ := newAdapter(t, f, nil)

	var e *Error
	if err := a.React(t.Context(), reactTarget(), "🦖"); !errors.As(err, &e) || e.Kind != KindBadRequest {
		t.Fatalf("err = %v", err)
	}
	for _, id := range []string{"", "1234", "1234:x", "9999:55", ":55", "1234:0"} {
		o := reactTarget()
		o.PlatformMsgID = id
		if err := a.React(t.Context(), o, "👍"); err == nil {
			t.Fatalf("id %q accepted", id)
		}
	}
	if n := f.count("setMessageReaction"); n != 1 {
		t.Fatalf("a bad target reached the API: %d calls", n)
	}
}

func TestLimitsReactions(t *testing.T) {
	a, _, _ := newAdapter(t, newFake(t), nil)
	got := a.Limits().Reactions
	for _, e := range []string{"👍", "❤", "🔥", "👀"} {
		if !slices.Contains(got, e) {
			t.Fatalf("%q missing from Limits.Reactions", e)
		}
	}
	if slices.Contains(got, "🦖") || len(got) != 73 {
		t.Fatalf("Limits.Reactions has %d entries", len(got))
	}
}

func TestAllowedUpdatesIncludeReactions(t *testing.T) {
	f := newFake(t)
	feed := &updateFeed{}
	feed.install(f)
	a, _, _ := newAdapter(t, f, nil)
	_, stop := start(t, a, (&collector{}).in)
	waitFor(t, func() bool { return f.count("getUpdates") > 0 })
	au, _ := f.seen("getUpdates")[0].Body["allowed_updates"].([]any)
	if !slices.Contains(au, any("message_reaction")) {
		t.Fatalf("allowed_updates = %v", au)
	}
	_ = stop()
}

func reactionUpdate(id int64, r *wireReactionUpdate) wireUpdate {
	return wireUpdate{UpdateID: id, MessageReaction: r}
}

func TestInboundReactions(t *testing.T) {
	user := &wireUser{ID: 7, Username: "angel", FirstName: "Angel"}
	emoji := func(e string) wireReaction { return wireReaction{Type: "emoji", Emoji: e} }
	dm := wireChat{ID: 1234, Type: "private"}
	grp := wireChat{ID: -100500, Type: "supergroup"}

	f := newFake(t)
	feed := &updateFeed{}
	feed.install(f)
	a, _, _ := newAdapter(t, f, nil)
	c := &collector{}
	_, stop := start(t, a, c.in)
	waitFor(t, func() bool { return f.count("getMe") > 0 })

	feed.push(
		reactionUpdate(1, &wireReactionUpdate{Chat: dm, MessageID: 55, User: user, Date: 1700000000, NewReaction: []wireReaction{emoji("👍")}}),
		reactionUpdate(2, &wireReactionUpdate{Chat: grp, MessageID: 56, User: user, Date: 1700000001,
			OldReaction: []wireReaction{emoji("👍")}}), // removed
		reactionUpdate(3, &wireReactionUpdate{Chat: dm, MessageID: 57, User: user, // dropped: anonymous
			ActorChat: &wireChat{ID: -1, Type: "channel"}, NewReaction: []wireReaction{emoji("👍")}}),
		reactionUpdate(4, &wireReactionUpdate{Chat: dm, MessageID: 58, User: user, // dropped: custom only
			NewReaction: []wireReaction{{Type: "custom_emoji"}, {Type: "paid"}}}),
		reactionUpdate(5, &wireReactionUpdate{Chat: dm, MessageID: 59, User: user}), // dropped: nothing changed
		reactionUpdate(6, &wireReactionUpdate{Chat: dm, MessageID: 60, User: user, // first emoji-type entry wins
			NewReaction: []wireReaction{{Type: "custom_emoji"}, emoji("🔥"), emoji("❤")}}),
		reactionUpdate(7, &wireReactionUpdate{Chat: dm, MessageID: 61, NewReaction: []wireReaction{emoji("👍")}}), // dropped: no user
	)
	waitFor(t, func() bool { return c.len() == 3 })
	_ = stop()
	if c.len() != 3 {
		t.Fatalf("envelopes = %d", c.len())
	}

	add := c.at(0)
	if add.Reaction == nil || *add.Reaction != (event.Reaction{TargetMsgID: "1234:55", Emoji: "👍"}) {
		t.Fatalf("add = %+v", add.Reaction)
	}
	if add.Origin.PlatformMsgID != "1234:r1" || add.Origin.Conversation != "1234" ||
		add.Origin.ConversationKind != event.ConversationDM || add.Origin.ChannelType != "telegram" {
		t.Fatalf("origin = %+v", add.Origin)
	}
	if len(add.Sender.Aliases) != 1 || add.Sender.Aliases[0] != "tg:7" || add.Sender.DisplayName != "Angel" {
		t.Fatalf("sender = %+v", add.Sender)
	}
	if add.Message.Text != "" || len(add.Message.Attachments) != 0 {
		t.Fatalf("a reaction carries content: %+v", add.Message)
	}

	rm := c.at(1)
	if rm.Reaction == nil || *rm.Reaction != (event.Reaction{TargetMsgID: "-100500:56", Removed: true}) ||
		rm.Origin.ConversationKind != event.ConversationRoom || rm.Origin.PlatformMsgID != "-100500:r2" {
		t.Fatalf("remove = %+v / %+v", rm.Reaction, rm.Origin)
	}
	if got := c.at(2).Reaction; got == nil || got.Emoji != "🔥" || got.TargetMsgID != "1234:60" {
		t.Fatalf("first emoji = %+v", got)
	}
}

func TestInboundReactionFromBotIsDropped(t *testing.T) {
	f := newFake(t)
	feed := &updateFeed{}
	feed.install(f)
	a, _, _ := newAdapter(t, f, nil)
	c := &collector{}
	_, stop := start(t, a, c.in)
	// getMe being called is not enough: wait until its answer is stored.
	waitFor(t, func() bool { id, _ := a.bot(); return id != 0 })
	botID, _ := a.bot()
	feed.push(reactionUpdate(1, &wireReactionUpdate{Chat: wireChat{ID: 1234, Type: "private"}, MessageID: 5,
		User: &wireUser{ID: botID, IsBot: true}, NewReaction: []wireReaction{{Type: "emoji", Emoji: "👍"}}}))
	waitFor(t, func() bool { return feed.lastOffset() > 1 }) // update 1 handled and acked
	_ = stop()
	if c.len() != 0 {
		t.Fatalf("the bot's own reaction was routed")
	}
}

func TestRejectedReactionIsRedelivered(t *testing.T) {
	f := newFake(t)
	feed := &updateFeed{}
	feed.install(f)
	a, _, _ := newAdapter(t, f, nil)
	var n atomic.Int32 // Run's goroutine counts, the test reads
	in := func(_ context.Context, _ event.Envelope) error {
		if n.Add(1) == 1 {
			return errors.New("store down")
		}
		return nil
	}
	_, stop := start(t, a, in)
	waitFor(t, func() bool { return f.count("getMe") > 0 })
	feed.push(reactionUpdate(1, &wireReactionUpdate{Chat: wireChat{ID: 1234, Type: "private"}, MessageID: 5,
		User: &wireUser{ID: 7}, NewReaction: []wireReaction{{Type: "emoji", Emoji: "👍"}}}))
	waitFor(t, func() bool { return n.Load() >= 2 })
	_ = stop()
}
