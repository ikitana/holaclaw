package slack

import (
	"testing"

	"holaclaw/internal/event"
)

func TestConversationKindFollowsSlackType(t *testing.T) {
	for name, tc := range map[string]struct {
		channel, typ string
		want         event.ConversationKind
	}{
		"im":                {"D1", "im", event.ConversationDM},
		"mpim is a room":    {"G1", "mpim", event.ConversationRoom},
		"mpim with D id":    {"D9", "mpim", event.ConversationRoom},
		"unknown is a room": {"D1", "future", event.ConversationRoom},
		"missing, D id":     {"D1", "", event.ConversationDM},
		"missing, other id": {"C1", "", event.ConversationRoom},
		"channel":           {"C1", "channel", event.ConversationRoom},
		"private group":     {"G1", "group", event.ConversationRoom},
	} {
		t.Run(name, func(t *testing.T) {
			if got := conversationKind(slackEvent{Channel: tc.channel, ChannelType: tc.typ}); got != tc.want {
				t.Fatalf("kind = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestMpimEnvelopeIsRoom(t *testing.T) {
	a := mappingAdapter(t, newFakeSlack(t), nil)
	ev := channelEvent("hi all", map[string]any{"channel": "D7", "channel_type": "mpim"})
	got := deliver(t, a, eventsPayloadFor("Ev1", ev))
	if len(got) != 1 || got[0].Origin.ConversationKind != event.ConversationRoom {
		t.Fatalf("got %+v", got)
	}
}
