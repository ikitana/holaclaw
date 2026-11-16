package telegram

import (
	"strings"
	"testing"

	"holaclaw/internal/event"
)

func TestReactionDiff(t *testing.T) {
	a, _, _ := newAdapter(t, newFake(t), nil)
	e := func(s string) wireReaction { return wireReaction{Type: "emoji", Emoji: s} }
	custom := wireReaction{Type: "custom_emoji"}
	cases := []struct {
		name     string
		old, new []wireReaction
		want     *event.Reaction // nil = dropped
	}{
		{"second reaction added", []wireReaction{e("👍")}, []wireReaction{e("👍"), e("❤")}, &event.Reaction{TargetMsgID: "1234:9", Emoji: "❤"}},
		{"one of two removed", []wireReaction{e("👍"), e("❤")}, []wireReaction{e("❤")}, &event.Reaction{TargetMsgID: "1234:9", Removed: true}},
		{"emoji swapped for custom", []wireReaction{e("👍")}, []wireReaction{custom}, &event.Reaction{TargetMsgID: "1234:9", Removed: true}},
		{"changed", []wireReaction{e("👍")}, []wireReaction{e("❤")}, &event.Reaction{TargetMsgID: "1234:9", Emoji: "❤"}},
		{"unchanged", []wireReaction{e("👍")}, []wireReaction{e("👍")}, nil},
		{"custom only added", nil, []wireReaction{custom}, nil},
		{"too long", nil, []wireReaction{e(strings.Repeat("👍", 17))}, nil},
		{"letters", nil, []wireReaction{e("ok")}, nil},
		{"control", nil, []wireReaction{e("👍\n")}, nil},
		{"keycap", nil, []wireReaction{e("1️⃣")}, &event.Reaction{TargetMsgID: "1234:9", Emoji: "1️⃣"}},
	}
	for _, c := range cases {
		env, ok := a.reactionEnvelope(1, &wireReactionUpdate{
			Chat: wireChat{ID: 1234, Type: "private"}, MessageID: 9, User: &wireUser{ID: 7},
			OldReaction: c.old, NewReaction: c.new,
		})
		if c.want == nil {
			if ok {
				t.Errorf("%s: routed %+v", c.name, env.Reaction)
			}
			continue
		}
		if !ok || *env.Reaction != *c.want {
			t.Errorf("%s: got %v %+v, want %+v", c.name, ok, env.Reaction, c.want)
		}
	}
}
