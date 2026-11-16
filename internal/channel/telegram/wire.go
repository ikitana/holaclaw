package telegram

import "encoding/json"

// Bot API wire types. Only the fields holaclaw uses are declared; the Bot API
// is additive, so unknown fields are ignored (ARCHITECTURE.md §7).

// apiResponse is the envelope every Bot API method answers with.
type apiResponse struct {
	OK          bool             `json:"ok"`
	Result      json.RawMessage  `json:"result"`
	Description string           `json:"description"`
	ErrorCode   int              `json:"error_code"`
	Parameters  *apiResponseParm `json:"parameters"`
}

type apiResponseParm struct {
	RetryAfter int `json:"retry_after"`
}

type wireUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

type wireChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"` // private, group, supergroup, channel
}

type wireEntity struct {
	Type   string    `json:"type"`
	Offset int       `json:"offset"` // UTF-16 code units
	Length int       `json:"length"`
	URL    string    `json:"url"`
	User   *wireUser `json:"user"`
}

// wireFile is the common shape of every media field: a file id plus optional
// name, mime and size.
type wireFile struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MIMEType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
	FilePath string `json:"file_path"` // only on getFile results
}

type wirePhoto struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

type wireMessage struct {
	MessageID       int64        `json:"message_id"`
	MessageThreadID int64        `json:"message_thread_id"`
	IsTopicMessage  bool         `json:"is_topic_message"`
	From            *wireUser    `json:"from"`
	Chat            wireChat     `json:"chat"`
	Date            int64        `json:"date"`
	Text            string       `json:"text"`
	Caption         string       `json:"caption"`
	Entities        []wireEntity `json:"entities"`
	CaptionEntities []wireEntity `json:"caption_entities"`
	MediaGroupID    string       `json:"media_group_id"`
	ReplyToMessage  *wireMessage `json:"reply_to_message"`

	Photo     []wirePhoto `json:"photo"`
	Document  *wireFile   `json:"document"`
	Audio     *wireFile   `json:"audio"`
	Voice     *wireFile   `json:"voice"`
	Video     *wireFile   `json:"video"`
	VideoNote *wireFile   `json:"video_note"`
	Animation *wireFile   `json:"animation"`
	Sticker   *wireFile   `json:"sticker"`
}

type wireCallbackQuery struct {
	ID      string       `json:"id"`
	From    *wireUser    `json:"from"`
	Message *wireMessage `json:"message"`
	Data    string       `json:"data"`
}

type wireChatMemberUpdated struct {
	Chat          wireChat  `json:"chat"`
	From          *wireUser `json:"from"`
	OldChatMember struct {
		User   *wireUser `json:"user"`
		Status string    `json:"status"`
	} `json:"old_chat_member"`
	NewChatMember struct {
		User   *wireUser `json:"user"`
		Status string    `json:"status"`
	} `json:"new_chat_member"`
}

// wireReactionUpdate is a message_reaction update: the reactor's old and new
// reactions on one message.
type wireReactionUpdate struct {
	Chat        wireChat       `json:"chat"`
	MessageID   int64          `json:"message_id"`
	User        *wireUser      `json:"user"`
	ActorChat   *wireChat      `json:"actor_chat"` // set instead of user for an anonymous reactor
	Date        int64          `json:"date"`
	OldReaction []wireReaction `json:"old_reaction"`
	NewReaction []wireReaction `json:"new_reaction"`
}

type wireReaction struct {
	Type  string `json:"type"` // emoji, custom_emoji, paid
	Emoji string `json:"emoji"`
}

type wireUpdate struct {
	UpdateID        int64                  `json:"update_id"`
	MessageReaction *wireReactionUpdate    `json:"message_reaction"`
	Message         *wireMessage           `json:"message"`
	CallbackQuery   *wireCallbackQuery     `json:"callback_query"`
	MyChatMember    *wireChatMemberUpdated `json:"my_chat_member"`
}

// inlineButton and inlineKeyboard render approval prompts.
type inlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

type inlineKeyboard struct {
	InlineKeyboard [][]inlineButton `json:"inline_keyboard"`
}
