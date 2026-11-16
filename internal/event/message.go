package event

import (
	"mime"
	"strings"
	"time"
)

// SchemaVersion is stamped on every persisted envelope, message and event.
// Consumers ignore unknown fields (ARCHITECTURE.md §7).
const SchemaVersion = 1

// Role of a message inside a session transcript.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
	// RoleSystem is used for compaction summaries and notices, never for the
	// system prompt itself (that is built from fragments).
	RoleSystem Role = "system"
)

// Kind of an attachment. Always derived from the mime type by holaclaw, never
// set by the sender.
type Kind string

const (
	KindImage    Kind = "image"
	KindAudio    Kind = "audio"
	KindVideo    Kind = "video"
	KindDocument Kind = "document"
)

// KindFromMIME derives the attachment kind from a mime type.
func KindFromMIME(m string) Kind {
	mt, _, err := mime.ParseMediaType(m)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(m))
	}
	switch {
	case strings.HasPrefix(mt, "image/"):
		return KindImage
	case strings.HasPrefix(mt, "audio/"):
		return KindAudio
	case strings.HasPrefix(mt, "video/"):
		return KindVideo
	default:
		return KindDocument
	}
}

// DerivedBy names the model role that produced derived text.
type DerivedBy string

const (
	DerivedByTranscribe DerivedBy = "transcribe"
	DerivedByVision     DerivedBy = "vision"
)

// Derived is text obtained from an attachment through a model role.
type Derived struct {
	Text string    `json:"text"`
	By   DerivedBy `json:"by"`
}

// Attachment is the only way files and media travel.
type Attachment struct {
	ID      ID       `json:"id"`
	BlobID  string   `json:"blob_id"` // content address: hex sha256
	MIME    string   `json:"mime"`
	Name    string   `json:"name"`
	Size    int64    `json:"size"`
	SHA256  string   `json:"sha256"`
	Kind    Kind     `json:"kind"`
	Derived *Derived `json:"derived,omitempty"`
}

// ToolCall is a model's request to run a tool.
type ToolCall struct {
	ID        string `json:"id"`        // provider call id, echoed in the result
	Name      string `json:"name"`      // tool name, e.g. "memory.save"
	Arguments string `json:"arguments"` // raw JSON object as produced (after repair)
}

// Message is one entry of a session transcript. Model responses, tool calls
// and tool results are all messages; events reference them by id.
type Message struct {
	Schema      int          `json:"schema"`
	ID          ID           `json:"id"`
	SessionID   ID           `json:"session_id"`
	TraceID     ID           `json:"trace_id"`
	Role        Role         `json:"role"`
	Text        string       `json:"text,omitempty"` // canonical markdown
	Attachments []Attachment `json:"attachments,omitempty"`
	ReplyTo     ID           `json:"reply_to,omitempty"`
	// ToolCalls is set on assistant messages that request tools.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID and ToolName are set on RoleTool messages (the result).
	ToolCallID string `json:"tool_call_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	// Sender is the verified, sanitised sender label for room transcripts.
	Sender string `json:"sender,omitempty"`
	// UserID is the resolved holaclaw user that sent this message. Written by
	// the router from trusted state, never by a sender or the model: a turn
	// takes its policy role from it, not from the session row (docs/sessions.md).
	UserID ID `json:"user_id,omitempty"`
	// Mentioned copies Envelope.Mentioned onto a user message. In a room a
	// turn ends at the first mention, so each request gets its own reply.
	Mentioned bool `json:"mentioned,omitempty"`
	// Tainted marks content that came from an untrusted source (fetch, MCP).
	Tainted bool `json:"tainted,omitempty"`
	// TaintScore is how much that untrusted content looks like instructions
	// aimed at the agent, 0–1, judged once when it was ingested (docs/security.md). Tainted says where the text came from; TaintScore says whether it
	// tries to steer. Policy escalates on the score, never on Tainted alone.
	// Absent (0) on records written before the score existed.
	TaintScore float64 `json:"taint_score,omitempty"`
	// Components is reserved (design only).
	Components []any     `json:"components,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	// Pos is the message's place in the transcript, filled by the store and
	// never part of the record. It is the id, except for a mailbox entry: that
	// one takes its place when a turn consumes it, so a message that arrived
	// while a turn ran sorts after that turn's answer. Empty on append means
	// the id, or just past the transcript's end when the id sorts before it
	// (a clock behind); compaction sets it to slot its summary in.
	Pos ID `json:"-"`
}

// ConversationKind is what every platform normalises to.
type ConversationKind string

const (
	ConversationDM   ConversationKind = "dm"
	ConversationRoom ConversationKind = "room"
)

// ChannelHolaclaw is the built-in channel instance, and its type, of the
// daemon's own clients: the CLI, the apps and automations (docs/identity.md
// §3). Its bindings are holaclaw:<alias>, and tokens sign them in.
const ChannelHolaclaw = "holaclaw"

// Origin identifies where an envelope came from. Delivery targets are always
// taken from here, never from a model-settable parameter.
type Origin struct {
	Channel          string           `json:"channel"`      // channel instance name, e.g. "tg-main", "holaclaw"
	ChannelType      string           `json:"channel_type"` // "telegram", "slack", "holaclaw"
	Conversation     string           `json:"conversation"` // platform conversation id
	ConversationKind ConversationKind `json:"conversation_kind"`
	Thread           string           `json:"thread,omitempty"`
	// ThreadRoot is the PlatformMsgID of the message a thread hangs off, set
	// by adapters whose threads start on a message (Slack). A new thread
	// session is seeded with the turn that message started.
	ThreadRoot    string `json:"thread_root,omitempty"`
	PlatformMsgID string `json:"platform_msg_id,omitempty"` // idempotency key
}

// Sender is the platform identity that sent an envelope. Aliases are all the
// ids the platform knows this sender by (WhatsApp later: JID + LID + phone).
type Sender struct {
	Aliases     []string `json:"aliases"`
	DisplayName string   `json:"display_name,omitempty"` // untrusted; sanitise before prompt use
	FromSelf    bool     `json:"from_self,omitempty"`
	// Token is the API token that signed the holaclaw binding in. Only the
	// Control API sets it, so a trace tells apart the tokens of one binding.
	Token ID `json:"token,omitempty"`
}

// Envelope is a normalized inbound message as produced by a channel adapter
// or the Control API, before routing.
type Envelope struct {
	Schema    int     `json:"schema"`
	TraceID   ID      `json:"trace_id"` // stamped by the router if empty
	Origin    Origin  `json:"origin"`
	Sender    Sender  `json:"sender"`
	Mentioned bool    `json:"mentioned,omitempty"` // the bot was addressed (rooms)
	Message   Message `json:"message"`
	// Reaction is set when the envelope is a reaction rather than a message;
	// Message then carries no content. Core routes it by target.
	Reaction *Reaction `json:"reaction,omitempty"`
	Received time.Time `json:"received"`
	// DedupeKeys identify this message across resends and addressings, set
	// by the adapter. Empty means core uses Origin.PlatformMsgID; with
	// neither there is no durable dedupe.
	DedupeKeys []string `json:"dedupe_keys,omitempty"`
}

// Reaction is an emoji put on, or taken off, an earlier message.
type Reaction struct {
	TargetMsgID string `json:"target_msg_id"` // PlatformMsgID of the message reacted to
	Emoji       string `json:"emoji"`         // as the platform sent it; empty when removed
	Removed     bool   `json:"removed,omitempty"`
}
