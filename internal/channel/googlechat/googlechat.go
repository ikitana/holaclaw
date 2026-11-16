// Package googlechat implements channel.Channel for a Google Chat app that
// uses a Cloud Pub/Sub connection (docs/channels/googlechat.md): events are pulled from projects/<key project>/subscriptions/holaclaw
// and replies are posted with the Chat REST API, both authenticated with one
// service-account key. JWT signing is standard-library RSA, so this package
// needs no dependency beyond the standard library.
package googlechat

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

// SubscriptionID is the Pub/Sub subscription holaclaw always pulls from, in the
// project the key belongs to (docs/channels/googlechat.md §3).
const SubscriptionID = "holaclaw"

// Defaults and Google's own limits.
const (
	DefaultVerifyTimeout = 15 * time.Second
	DefaultMaxDownload   = 20 << 20 // 20 MiB, like Telegram and Slack

	defaultTokenURL  = "https://oauth2.googleapis.com/token"
	defaultPubSubURL = "https://pubsub.googleapis.com"
	defaultChatURL   = "https://chat.googleapis.com"

	maxTextChars = 4000 // docs/channels/googlechat.md §5
	maxPullBatch = 50

	// approvalFunction is the card button action holaclaw owns.
	approvalFunction = "hb_approval"
)

// Tuning that tests override; not configuration.
var (
	backoffBase     = 1 * time.Second
	backoffMax      = 60 * time.Second
	loggedOutWait   = 60 * time.Second // between retries while the key is refused or missing
	pullTimeout     = 90 * time.Second // one long poll
	callTimeout     = 2 * time.Minute  // any other request, downloads included
	ackTimeout      = 10 * time.Second
	maxSendAttempts = 3
	dedupeTTL       = 30 * time.Minute
)

// Options configures an Adapter. Every field but Name and Key has a default.
type Options struct {
	Name string // channel instance name, e.g. "googlechat"
	// Key returns the service-account key JSON. It is read per request so a
	// rotated key is picked up; it is never stored, logged or put in an error.
	Key func() string
	// BaseURL is for tests: when set, the token endpoint (BaseURL+"/token"),
	// Pub/Sub and Chat all live under it.
	BaseURL string
	State   channel.StateStore
	Blobs   channel.BlobOpener
	// PutBlob stores an inbound file and returns its content address.
	PutBlob func(ctx context.Context, r io.Reader, mime string) (blobID string, size int64, err error)
	// OnApproval is called when an approval button is pressed. fromAliases
	// come from the event's authenticated user, never from the button. It
	// reports whether the press resolved the approval; only then is the card
	// rewritten without its buttons.
	OnApproval func(ctx context.Context, approvalID event.ID, choice string, fromAliases []string) (accepted bool)
	// OnJoined is called when the app is added to a space (not a DM).
	OnJoined    func(ctx context.Context, space string, byAliases []string)
	HTTPClient  *http.Client
	Logger      *slog.Logger
	MaxDownload int64 // inbound download cap, default DefaultMaxDownload
}

// Adapter is one Google Chat app instance.
type Adapter struct {
	opt      Options
	tokenURL string
	pubsub   string
	chat     string
	hc       *http.Client
	log      *slog.Logger
	seen     *ttlSet
	tokens   tokenCache
}

var (
	_ channel.Channel          = (*Adapter)(nil)
	_ channel.ApprovalPrompter = (*Adapter)(nil)
)

// New builds an Adapter. It performs no I/O; Run connects.
func New(o Options) *Adapter {
	if o.Name == "" {
		o.Name = "googlechat"
	}
	if o.Key == nil {
		o.Key = func() string { return "" }
	}
	if o.MaxDownload <= 0 {
		o.MaxDownload = DefaultMaxDownload
	}
	a := &Adapter{
		opt:      o,
		tokenURL: defaultTokenURL,
		pubsub:   defaultPubSubURL,
		chat:     defaultChatURL,
		hc:       o.HTTPClient,
		log:      o.Logger,
		seen:     newTTLSet(dedupeTTL),
	}
	if base := strings.TrimRight(o.BaseURL, "/"); base != "" {
		a.tokenURL, a.pubsub, a.chat = base+"/token", base, base
	}
	if a.hc == nil {
		// No client timeout: each request carries its own deadline, and a
		// long poll must outlast any fixed one.
		a.hc = &http.Client{}
	}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	a.log = a.log.With("channel", o.Name, "channel_type", "googlechat")
	return a
}

// Name reports the channel instance name.
func (a *Adapter) Name() string { return a.opt.Name }

// Type reports the channel type.
func (a *Adapter) Type() string { return "googlechat" }

// Limits reports what this adapter can deliver. A Chat app cannot upload
// files (media.upload takes user credentials only), so Kinds is empty and
// every attachment is degraded.
func (a *Adapter) Limits() channel.Limits {
	return channel.Limits{TextChars: maxTextChars}
}

// ttlSet is a short-TTL dedupe set over platform ids.
type ttlSet struct {
	ttl time.Duration
	mu  sync.Mutex
	at  map[string]time.Time
}

func newTTLSet(ttl time.Duration) *ttlSet { return &ttlSet{ttl: ttl, at: map[string]time.Time{}} }

// has reports whether key was recorded within the TTL.
func (s *ttlSet) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.at[key]
	return ok && time.Since(t) <= s.ttl
}

// add records key, pruning expired entries.
func (s *ttlSet) add(key string) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.at {
		if now.Sub(t) > s.ttl {
			delete(s.at, k)
		}
	}
	s.at[key] = now
}

// sleepCtx sleeps for d, or returns false when ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
