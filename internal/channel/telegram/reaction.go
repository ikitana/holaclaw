package telegram

import (
	"context"
	"slices"
	"strconv"
	"time"
	"unicode"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

// reactionEmoji is Telegram's list, not ours: a bot may react only with these
// emoji (ReactionTypeEmoji, https://core.telegram.org/bots/api#reactiontypeemoji),
// and setMessageReaction fails with REACTION_INVALID for anything else. It is
// copied verbatim, offensive ones included, so core can refuse early with the
// list for the model to pick from and send ❤ for a model's ❤️. Update it when
// Telegram does. People's reactions coming in are not checked against it.
var reactionEmoji = []string{
	"❤", "👍", "👎", "🔥", "🥰", "👏", "😁", "🤔", "🤯", "😱", "🤬", "😢", "🎉", "🤩", "🤮", "💩",
	"🙏", "👌", "🕊", "🤡", "🥱", "🥴", "😍", "🐳", "❤‍🔥", "🌚", "🌭", "💯", "🤣", "⚡", "🍌", "🏆",
	"💔", "🤨", "😐", "🍓", "🍾", "💋", "🖕", "😈", "😴", "😭", "🤓", "👻", "👨‍💻", "👀", "🎃", "🙈",
	"😇", "😨", "🤝", "✍", "🤗", "🫡", "🎅", "🎄", "☃", "💅", "🤪", "🗿", "🆒", "💘", "🙉", "🦄",
	"😘", "💊", "🙊", "😎", "👾", "🤷‍♂", "🤷", "🤷‍♀", "😡",
}

var _ channel.Reactor = (*Adapter)(nil)

// React sets the bot's reaction on target.PlatformMsgID ("<chat>:<message id>");
// an empty emoji clears it.
func (a *Adapter) React(ctx context.Context, target event.Origin, emoji string) error {
	chat, id := splitMsgID(target.PlatformMsgID)
	if chat == "" || id == 0 || chat != target.Conversation {
		return &Error{Method: "setMessageReaction", Kind: KindBadRequest, Description: "bad target message id"}
	}
	reaction := []map[string]string{}
	if emoji != "" {
		reaction = append(reaction, map[string]string{"type": "emoji", "emoji": emoji})
	}
	body := map[string]any{"chat_id": chat, "message_id": id, "reaction": reaction}
	return a.call(ctx, "setMessageReaction", body, nil)
}

// splitMsgID splits "<chat>:<message id>" at its last colon.
func splitMsgID(s string) (string, int64) {
	for i := len(s) - 1; i > 0; i-- {
		if s[i] == ':' {
			id, err := strconv.ParseInt(s[i+1:], 10, 64)
			if err != nil {
				return "", 0
			}
			return s[:i], id
		}
	}
	return "", 0
}

// emojisOf lists the emoji-type entries; custom and paid reactions are left out.
func emojisOf(rs []wireReaction) []string {
	var out []string
	for _, r := range rs {
		if r.Type == "emoji" && r.Emoji != "" {
			out = append(out, r.Emoji)
		}
	}
	return out
}

// maxEmojiBytes bounds the emoji of a reaction.
const maxEmojiBytes = 64

// validEmoji accepts what an emoji is made of: symbols, marks, format
// characters (ZWJ) and the digits, # and * of keycaps, in at most
// maxEmojiBytes.
func validEmoji(s string) bool {
	if s == "" || len(s) > maxEmojiBytes {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r == '#', r == '*':
		case unicode.IsSymbol(r), unicode.IsMark(r), unicode.Is(unicode.Cf, r):
		default:
			return false
		}
	}
	return true
}

// reactionEnvelope maps a message_reaction update onto an Envelope. It
// reports false for what core cannot use: anonymous reactors, the bot's own
// reactions, custom and paid reactions, and updates that change nothing.
func (a *Adapter) reactionEnvelope(updateID int64, r *wireReactionUpdate) (event.Envelope, bool) {
	botID, _ := a.bot()
	if r.User == nil || r.ActorChat != nil || r.User.ID == botID {
		return event.Envelope{}, false
	}
	// The update carries the whole old and new set: the change is what
	// differs. A new emoji is an add; else one gone from the old set is a removal.
	oldE, newE := emojisOf(r.OldReaction), emojisOf(r.NewReaction)
	emoji, removed := "", false
	for _, e := range newE {
		if !slices.Contains(oldE, e) {
			emoji = e
			break
		}
	}
	switch {
	case emoji != "":
		if !validEmoji(emoji) {
			return event.Envelope{}, false
		}
	case slices.ContainsFunc(oldE, func(e string) bool { return !slices.Contains(newE, e) }):
		removed = true
	default:
		return event.Envelope{}, false
	}
	kind := event.ConversationRoom
	if r.Chat.Type == "private" {
		kind = event.ConversationDM
	}
	chat := strconv.FormatInt(r.Chat.ID, 10)
	// message_reaction carries no thread id, so Origin.Thread stays empty.
	return event.Envelope{
		Schema: event.SchemaVersion,
		Origin: event.Origin{
			Channel:          a.name,
			ChannelType:      a.Type(),
			Conversation:     chat,
			ConversationKind: kind,
			PlatformMsgID:    chat + ":r" + strconv.FormatInt(updateID, 10),
		},
		Sender:   sender(r.User),
		Reaction: &event.Reaction{TargetMsgID: chat + ":" + strconv.FormatInt(r.MessageID, 10), Emoji: emoji, Removed: removed},
		Message: event.Message{
			Schema:    event.SchemaVersion,
			ID:        event.NewID(),
			Role:      event.RoleUser,
			CreatedAt: time.Unix(r.Date, 0).UTC(),
		},
		Received: time.Now().UTC(),
	}, true
}
