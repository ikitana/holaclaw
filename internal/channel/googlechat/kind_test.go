package googlechat

import (
	"testing"

	"holaclaw/internal/event"
)

func TestSpaceTypeClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		spaceType string
		legacy    map[string]any
		want      event.ConversationKind
	}{
		"GROUP_CHAT is a room":     {"GROUP_CHAT", nil, event.ConversationRoom},
		"SPACE is a room":          {"SPACE", nil, event.ConversationRoom},
		"unknown type is a room":   {"FUTURE_KIND", nil, event.ConversationRoom},
		"unknown beats legacy DM":  {"FUTURE_KIND", map[string]any{"type": "DM", "singleUserBotDm": true}, event.ConversationRoom},
		"DIRECT_MESSAGE is a DM":   {"DIRECT_MESSAGE", nil, event.ConversationDM},
		"legacy ROOM is a room":    {"", map[string]any{"type": "ROOM"}, event.ConversationRoom},
		"nothing at all is a room": {"", nil, event.ConversationRoom},
	} {
		t.Run(name, func(t *testing.T) {
			a, _ := newAdapter(t, newFake(t), nil)
			ev := msgEvent("spaces/X1", tc.spaceType, "m1", t0, "hi", nil)
			sp := ev["space"].(map[string]any)
			for k, v := range tc.legacy {
				sp[k] = v
			}
			env := one(t, deliver(t, a, ev))
			if env.Origin.ConversationKind != tc.want {
				t.Fatalf("kind = %s, want %s", env.Origin.ConversationKind, tc.want)
			}
		})
	}
}
