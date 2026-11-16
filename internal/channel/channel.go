// Package channel defines the two-way channel adapter contract
// (ARCHITECTURE.md §2, §6, docs/channels). One Go package per channel type.
package channel

import (
	"context"
	"errors"
	"time"

	"holaclaw/internal/event"
)

// State of a channel instance lifecycle, published as channel.status events.
type State string

const (
	StateUnlinked     State = "unlinked"
	StateLinking      State = "linking"
	StateConnected    State = "connected"
	StateReconnecting State = "reconnecting"
	StateLoggedOut    State = "logged_out"
)

// Limits an adapter declares so core can degrade delivery predictably.
type Limits struct {
	TextChars    int          // max characters per text message (adapter splits)
	CaptionChars int          // 0 = no captions
	Kinds        []event.Kind // deliverable attachment kinds
	MaxFileBytes int64
	Reactions    []string // emoji a Reactor accepts; nil = any
}

// Outbound is a reply to deliver. Target is always the envelope origin of the
// conversation, set by core, never by the model.
type Outbound struct {
	TraceID event.ID
	Target  event.Origin
	Message event.Message
}

// Receipt reports what was delivered.
type Receipt struct {
	PlatformMsgIDs []string
	// Degraded lists attachments that could not be delivered, with reasons;
	// core turns these into delivery.degraded events and a text notice.
	Degraded []string
}

// Inbound is how an adapter hands envelopes to the router. It returns once
// the envelope is durably stored (or dropped), so the adapter can advance
// its platform offset. ErrDuplicate means the message was already handled:
// the adapter acks it like nil (advances its offset, acks the delivery) but
// does nothing else, no read receipt and no unread mark.
type Inbound func(ctx context.Context, env event.Envelope) error

// ErrDuplicate is what Inbound returns for a message core already handled
// (#101).
var ErrDuplicate = errors.New("channel: duplicate inbound message")

// BlobOpener lets adapters read attachment bytes for delivery.
type BlobOpener interface {
	OpenAttachment(ctx context.Context, a event.Attachment) (data []byte, err error)
}

// StateStore is small per-instance persistent state (update offsets, dedupe).
type StateStore interface {
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, value string) error
}

// Channel is one named adapter instance.
type Channel interface {
	Name() string // instance name, e.g. "tg-main"
	Type() string // "telegram", "slack", ...
	Limits() Limits
	// Run connects and blocks until ctx ends, calling in for every inbound
	// message and status for lifecycle changes. It reconnects on its own.
	Run(ctx context.Context, in Inbound, status func(State, string)) error
	Send(ctx context.Context, out Outbound) (Receipt, error)
}

// Optional capability interfaces.

// AliasNamespacer declares the prefixes of the sender aliases an adapter
// emits when an account has more than one id, each of which holds one id per
// account (WhatsApp: "wa:" and "wa:lid:"). Core refuses to give an identity a
// second id in one namespace (docs/identity.md §2).
type AliasNamespacer interface {
	AliasNamespaces() []string
}

// Typer shows a typing indicator until the returned stop func is called.
type Typer interface {
	Typing(ctx context.Context, target event.Origin) (stop func())
}

// Reactor reacts to a message with one emoji. target.PlatformMsgID is the
// message reacted to, set by core from the turn's inbound message.
type Reactor interface {
	React(ctx context.Context, target event.Origin, emoji string) error
}

// LinkStatus is where an interactive link stands.
type LinkStatus string

const (
	LinkNone    LinkStatus = "none"
	LinkPending LinkStatus = "pending"
	LinkDone    LinkStatus = "done"
	LinkFailed  LinkStatus = "failed"
)

// LinkState reports an interactive link. Code is set only while the link is
// pending; it goes back to the admin who asked and never into events, traces
// or logs.
type LinkState struct {
	Status  LinkStatus
	Code    string
	Expires time.Time
	Detail  string
}

// Linker links an adapter to an account interactively (WhatsApp pairing code).
type Linker interface {
	Link(ctx context.Context, phone string) (LinkState, error)
	LinkState() LinkState
	Unlink(ctx context.Context) error
}

// ErrLinkConflict is a Link or Unlink refusal rooted in the instance's
// current state, not in what was asked: already linked, a link already
// pending, not linked on Unlink, or the instance not running.
var ErrLinkConflict = errors.New("channel: link conflict")

// ErrBadPhone is a Link refusal because the phone number is malformed.
var ErrBadPhone = errors.New("channel: malformed phone number")

// ApprovalPrompter renders an approval request with buttons in the channel.
// The adapter reports the choice through the resolve callback given at
// construction.
type ApprovalPrompter interface {
	PromptApproval(ctx context.Context, target event.Origin, approvalID event.ID, text string, choices []string) error
}
