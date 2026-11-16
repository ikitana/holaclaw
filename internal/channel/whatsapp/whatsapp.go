// Package whatsapp implements channel.Channel as a linked device of a WhatsApp
// account (docs/channels/whatsapp.md). It speaks the multi-device protocol
// in-process through whatsmeow, which sits behind the narrow client interface
// in client.go so tests can fake it. whatsmeow keeps its own session database,
// one file per instance; the pairing-code login runs here, in the daemon that
// owns that file (§5.4).
package whatsapp

import (
	"context"
	"io"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

// Defaults for Options left at their zero value.
const (
	DefaultMaxDownload = 20 << 20 // 20 MiB, like Telegram

	// WhatsApp limits (§5.1).
	maxTextChars    = 4000
	maxCaptionChars = 1024
	maxUploadBytes  = 100 << 20 // Upload takes the whole file in memory
)

// Tuning that tests override; not configuration (§5.1, §5.4).
var (
	backoffBase = 2 * time.Second
	backoffMax  = 5 * time.Minute
	// qrWait bounds the wait for the first QR event, which means the linking
	// socket is up.
	qrWait = 30 * time.Second
	// codeTTL is how long a pairing code lives: WhatsApp closes the linking
	// socket when the QR codes it rotates run out.
	codeTTL = 160 * time.Second
	// partGap spaces the parts of a split reply, the way a person types.
	partGap         = time.Second
	retryWait       = time.Second
	maxSendAttempts = 3
	typingInterval  = 8 * time.Second
)

// Options configures an Adapter. Every field but Name and SessionPath has a
// default.
type Options struct {
	Name string // channel instance name, e.g. "whatsapp"
	// SessionPath is whatsmeow's database, <data_dir>/whatsapp/<name>.db. It
	// is the credential: the directory is made 0700 and the file 0600.
	SessionPath string
	// State keeps each conversation's disappearing timer across restarts.
	State channel.StateStore
	Blobs channel.BlobOpener
	// PutBlob stores an inbound file and returns its content address.
	PutBlob func(ctx context.Context, r io.Reader, mime string) (blobID string, size int64, err error)
	// OnJoined is called when the account is added to a group.
	OnJoined    func(ctx context.Context, chatID string, byAliases []string)
	Logger      *slog.Logger
	MaxDownload int64 // inbound download cap, default DefaultMaxDownload
}

// opener opens a client over the session database at path, on the stored
// device or on a fresh one when the store has none.
type opener func(ctx context.Context, path string, log *slog.Logger) (waClient, error)

// Adapter is one WhatsApp linked device.
type Adapter struct {
	name        string
	path        string
	state       channel.StateStore
	blobs       channel.BlobOpener
	putBlob     func(context.Context, io.Reader, string) (string, int64, error)
	onJoined    func(context.Context, string, []string)
	log         *slog.Logger
	maxDownload int64

	// Seams for tests.
	open   opener
	now    func() time.Time
	after  func(time.Duration) <-chan time.Time
	jitter func(n int64) int64 // uniform in [0, n)
	sleep  func(ctx context.Context, d time.Duration) bool

	// sessionMu serialises the operations that connect, replace or wipe the
	// client.
	sessionMu sync.Mutex
	// sendMu keeps one send at a time per instance, the way a person replies.
	sendMu sync.Mutex

	stateMu   sync.Mutex
	client    waClient // nil while Run is not running
	clientGen int      // bumped each time client is replaced; stale events are ignored
	runCtx    context.Context
	inbound   channel.Inbound
	status    func(channel.State, string)
	link      channel.LinkState
	firstQR   chan struct{} // closed on the first QR event of a link
	queue     []item        // lifecycle events for the Run loop, never blocks the handler
	queueWake chan struct{}

	unread      map[string][]readRef // per conversation, messages that may start a turn
	unreadOrder []string
	expiry      map[string]uint32 // per conversation, disappearing timer in seconds
	expiryOrder []string
	// authors maps "<chat>/<id>" of a message to its sender, empty for the
	// account's own: BuildReaction needs it to react in a group (§5.1).
	authors      map[string]types.JID
	authorsOrder []string
	// linkEnd is closed when a pending link ends, so a Link waiting for the
	// socket gives up at once.
	linkEnd chan struct{}
}

// item is one queued lifecycle event of client generation clientGen.
type item struct {
	clientGen int
	event     any
}

var (
	_ channel.Channel         = (*Adapter)(nil)
	_ channel.Linker          = (*Adapter)(nil)
	_ channel.AliasNamespacer = (*Adapter)(nil)
	_ channel.Typer           = (*Adapter)(nil)
	_ channel.Reactor         = (*Adapter)(nil)
)

// New builds an Adapter from opts.
func New(opts Options) *Adapter {
	a := &Adapter{
		name:        opts.Name,
		path:        opts.SessionPath,
		state:       opts.State,
		blobs:       opts.Blobs,
		putBlob:     opts.PutBlob,
		onJoined:    opts.OnJoined,
		log:         opts.Logger,
		maxDownload: opts.MaxDownload,
		open:        openSession,
		now:         time.Now,
		after:       time.After,
		jitter:      rand.Int64N,
		sleep:       sleepCtx,
		link:        channel.LinkState{Status: channel.LinkNone},
		queueWake:   make(chan struct{}, 1),
		unread:      map[string][]readRef{},
		expiry:      map[string]uint32{},
		authors:     map[string]types.JID{},
	}
	if a.name == "" {
		a.name = "whatsapp"
	}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	if a.maxDownload <= 0 {
		a.maxDownload = DefaultMaxDownload
	}
	return a
}

// Name reports the channel instance name.
func (a *Adapter) Name() string { return a.name }

// Type reports the channel type.
func (a *Adapter) Type() string { return "whatsapp" }

// Limits reports what this adapter can deliver (§5.1).
func (a *Adapter) Limits() channel.Limits {
	return channel.Limits{
		TextChars:    maxTextChars,
		CaptionChars: maxCaptionChars,
		Kinds:        []event.Kind{event.KindImage, event.KindVideo, event.KindAudio, event.KindDocument},
		MaxFileBytes: maxUploadBytes,
	}
}

// AliasNamespaces declares the phone number and LID namespaces: one of each
// per account (§4).
func (a *Adapter) AliasNamespaces() []string { return []string{"wa:", "wa:lid:"} }
