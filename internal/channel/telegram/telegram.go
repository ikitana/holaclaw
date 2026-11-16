// Package telegram implements channel.Channel over the Telegram Bot API
// (docs/channels): long polling with a persisted update offset, HTML parse
// mode, 4096/1024 limits, forum topics, album debouncing and inline keyboards.
// Reactions go both ways; in a group Telegram delivers message_reaction
// updates only when the bot is an administrator.
// The Bot API is plain HTTPS+JSON, so this package needs no dependency beyond
// the standard library.
package telegram

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

// Defaults for Options left at their zero value.
const (
	DefaultBaseURL     = "https://api.telegram.org"
	DefaultPollTimeout = 50 * time.Second
	DefaultMaxDownload = 20 << 20 // 20 MiB
	DefaultAlbumWait   = 800 * time.Millisecond

	// Telegram's own limits (docs/channels).
	maxTextChars    = 4096
	maxCaptionChars = 1024
	maxUploadBytes  = 50 << 20 // bot upload ceiling
	maxPhotoBytes   = 10 << 20 // above this a photo must go as a document
)

// offsetKey is where the next update offset lives in the instance state store.
const offsetKey = "offset"

// Tuning that tests override; not configuration (docs/channels fixes them).
var (
	backoffBase     = 1 * time.Second
	backoffMax      = 60 * time.Second
	typingInterval  = 4 * time.Second
	typingCooldown  = 60 * time.Second
	approvalTTL     = time.Hour
	maxApprovals    = 4096
	maxSendAttempts = 3
)

// Options configures an Adapter. Every field but Name and Token has a default.
type Options struct {
	Name string // channel instance name, e.g. "tg-main"
	// Token is read once per request so a rotated token is picked up. It is
	// never logged and is scrubbed out of every error.
	Token   func() string
	BaseURL string // default DefaultBaseURL
	State   channel.StateStore
	Blobs   channel.BlobOpener
	// PutBlob stores an inbound file and returns its content address.
	PutBlob func(ctx context.Context, r io.Reader, mime string) (blobID string, size int64, err error)
	// OnApproval is called when an approval button is pressed.
	OnApproval func(ctx context.Context, approvalID event.ID, choice string, fromAliases []string)
	// OnJoined is called when the bot is added to a group; such updates carry
	// no message and so produce no envelope.
	OnJoined    func(ctx context.Context, chatID string, byAliases []string)
	HTTPClient  *http.Client
	Logger      *slog.Logger
	PollTimeout time.Duration // long-poll timeout, default DefaultPollTimeout
	MaxDownload int64         // inbound download cap, default DefaultMaxDownload
	AlbumWait   time.Duration // media-group debounce, default DefaultAlbumWait
}

// Adapter is one Telegram bot instance.
type Adapter struct {
	name        string
	base        string
	token       func() string
	state       channel.StateStore
	blobs       channel.BlobOpener
	putBlob     func(context.Context, io.Reader, string) (string, int64, error)
	onApproval  func(context.Context, event.ID, string, []string)
	onJoined    func(context.Context, string, []string)
	hc          *http.Client
	log         *slog.Logger
	pollTimeout time.Duration
	maxDownload int64
	albumWait   time.Duration

	mu       sync.Mutex
	botID    int64
	botUser  string // lower-case, without "@"
	coolDown time.Time
	pending  map[string]pendingApproval
}

type pendingApproval struct {
	id      event.ID
	expires time.Time
}

var (
	_ channel.Channel          = (*Adapter)(nil)
	_ channel.Typer            = (*Adapter)(nil)
	_ channel.ApprovalPrompter = (*Adapter)(nil)
)

// New builds an Adapter from opts.
func New(opts Options) *Adapter {
	a := &Adapter{
		name:        opts.Name,
		base:        strings.TrimRight(opts.BaseURL, "/"),
		token:       opts.Token,
		state:       opts.State,
		blobs:       opts.Blobs,
		putBlob:     opts.PutBlob,
		onApproval:  opts.OnApproval,
		onJoined:    opts.OnJoined,
		hc:          opts.HTTPClient,
		log:         opts.Logger,
		pollTimeout: opts.PollTimeout,
		maxDownload: opts.MaxDownload,
		albumWait:   opts.AlbumWait,
		pending:     map[string]pendingApproval{},
	}
	if a.name == "" {
		a.name = "telegram"
	}
	if a.base == "" {
		a.base = DefaultBaseURL
	}
	if a.token == nil {
		a.token = func() string { return "" }
	}
	if a.hc == nil {
		a.hc = &http.Client{}
	}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	if a.pollTimeout <= 0 {
		a.pollTimeout = DefaultPollTimeout
	}
	if a.maxDownload <= 0 {
		a.maxDownload = DefaultMaxDownload
	}
	if a.albumWait <= 0 {
		a.albumWait = DefaultAlbumWait
	}
	return a
}

// Name reports the channel instance name.
func (a *Adapter) Name() string { return a.name }

// Type reports the channel type.
func (a *Adapter) Type() string { return "telegram" }

// Limits reports what this adapter can deliver (docs/sessions.md, docs/channels).
func (a *Adapter) Limits() channel.Limits {
	return channel.Limits{
		TextChars:    maxTextChars,
		CaptionChars: maxCaptionChars,
		Kinds:        []event.Kind{event.KindImage, event.KindAudio, event.KindVideo, event.KindDocument},
		MaxFileBytes: maxUploadBytes,
		Reactions:    reactionEmoji,
	}
}

// bot returns the identity learned from getMe.
func (a *Adapter) bot() (int64, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.botID, a.botUser
}

func (a *Adapter) setBot(id int64, user string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.botID, a.botUser = id, strings.ToLower(user)
}
