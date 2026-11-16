// Package slack is the Slack channel adapter: Socket Mode inbound, Web API
// outbound, plain mrkdwn, Block Kit only for approval buttons
// (docs/sessions.md, docs/channels).
package slack

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

const (
	// defaultAPIBase is the Slack Web API root; injectable for tests.
	defaultAPIBase = "https://slack.com/api"
	// defaultMaxDownload caps one inbound file download.
	defaultMaxDownload = 20 << 20
	// maxUploadBytes is our own outbound cap; beyond it an attachment is
	// degraded even though Slack itself accepts up to MaxFileBytes.
	maxUploadBytes = 100 << 20
	// chunkChars is the outbound text chunk size (Slack's limit is ~4000).
	chunkChars = 4000
	// approvalActionID is the Block Kit action id of our approval buttons.
	approvalActionID = "hb_approval"
)

// Options configure one Slack instance. Tokens are read per call so a rotated
// secret takes effect without a restart; they are never logged.
type Options struct {
	Name       string        // instance name, e.g. "slack-main"
	BotToken   func() string // xoxb-…, Web API and file downloads
	AppToken   func() string // xapp-…, apps.connections.open only
	APIBaseURL string        // default https://slack.com/api
	State      channel.StateStore
	Blobs      channel.BlobOpener
	// PutBlob stores an inbound file and returns its content address and size.
	PutBlob func(ctx context.Context, r io.Reader, mime string) (string, int64, error)
	// OnApproval reports a button press back to core.
	OnApproval  func(ctx context.Context, approvalID event.ID, choice string, fromAliases []string)
	HTTPClient  *http.Client
	Logger      *slog.Logger
	MaxDownload int64 // default 20 MiB
}

// Adapter is one Slack channel instance.
type Adapter struct {
	opt Options
	hc  *http.Client
	log *slog.Logger

	mu      sync.Mutex
	botUser string // our own bot user id, from auth.test
	team    string

	users *nameCache
	seen  *ttlSet
}

var (
	_ channel.Channel          = (*Adapter)(nil)
	_ channel.ApprovalPrompter = (*Adapter)(nil)
)

// New builds an adapter. It performs no I/O; Run connects.
func New(o Options) *Adapter {
	if o.APIBaseURL == "" {
		o.APIBaseURL = defaultAPIBase
	}
	o.APIBaseURL = strings.TrimRight(o.APIBaseURL, "/")
	if o.MaxDownload <= 0 {
		o.MaxDownload = defaultMaxDownload
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Adapter{
		opt:   o,
		hc:    hc,
		log:   log.With("channel", o.Name, "channel_type", "slack"),
		users: newNameCache(time.Hour),
		seen:  newTTLSet(10 * time.Minute),
	}
}

func (a *Adapter) Name() string { return a.opt.Name }
func (a *Adapter) Type() string { return "slack" }

// Limits declares what this adapter can deliver. MaxFileBytes is Slack's own
// 1 GiB limit; anything above maxUploadBytes is degraded by the adapter.
func (a *Adapter) Limits() channel.Limits {
	return channel.Limits{
		TextChars:    chunkChars,
		CaptionChars: 0,
		Kinds:        []event.Kind{event.KindImage, event.KindAudio, event.KindVideo, event.KindDocument},
		MaxFileBytes: 1 << 30,
	}
}

func (a *Adapter) botToken() string {
	if a.opt.BotToken == nil {
		return ""
	}
	return a.opt.BotToken()
}

func (a *Adapter) appToken() string {
	if a.opt.AppToken == nil {
		return ""
	}
	return a.opt.AppToken()
}

func (a *Adapter) identity() (botUser, team string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.botUser, a.team
}

func (a *Adapter) setIdentity(botUser, team string) {
	a.mu.Lock()
	a.botUser, a.team = botUser, team
	a.mu.Unlock()
}

// redact removes both tokens from any string that may reach a log or an error,
// and strips the credential-bearing parts of any URL it carries.
func (a *Adapter) redact(s string) string {
	for _, t := range []string{a.botToken(), a.appToken()} {
		if len(t) >= 8 {
			s = strings.ReplaceAll(s, t, "[redacted]")
		}
	}
	return stripURLSecrets(s)
}

// urlish finds an absolute URL inside free-form text. Quotes, angle brackets
// and whitespace end it: transport errors quote the URL they were about.
var urlish = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s"'<>\\]+`)

// stripURLSecrets removes the query string, the fragment and any userinfo from
// every URL in s. Slack hands out one-shot credentials in exactly those
// places — the Socket Mode `ticket` on the wss URL and the signed
// `files.getUploadURLExternal` upload URL — and a dial or upload failure quotes
// the whole URL in its error text, which then reaches a returned error, a
// channel status detail and the event log (ARCHITECTURE.md §4: secrets never
// reach a trace).
func stripURLSecrets(s string) string {
	return urlish.ReplaceAllStringFunc(s, func(u string) string {
		// Sentence punctuation that followed the URL is not part of it.
		tail := ""
		for len(u) > 0 && strings.ContainsRune(`.,;:!?)]}`, rune(u[len(u)-1])) {
			tail = u[len(u)-1:] + tail
			u = u[:len(u)-1]
		}
		scheme, rest, ok := strings.Cut(u, "://")
		if !ok {
			return u + tail
		}
		// Userinfo lives before the first "@" of the authority.
		authority, path, hasPath := strings.Cut(rest, "/")
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			authority = "[redacted]@" + authority[at+1:]
		}
		rest = authority
		if hasPath {
			rest += "/" + path
		}
		if i := strings.IndexAny(rest, "?#"); i >= 0 {
			rest = rest[:i] + "?[redacted]"
		}
		return scheme + "://" + rest + tail
	})
}

// ttlSet is a short-TTL dedupe set (platform message ids, event ids).
type ttlSet struct {
	ttl time.Duration
	mu  sync.Mutex
	at  map[string]time.Time
}

func newTTLSet(ttl time.Duration) *ttlSet { return &ttlSet{ttl: ttl, at: map[string]time.Time{}} }

// add records key and reports whether it was already present (a duplicate).
func (s *ttlSet) add(key string) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, t := range s.at {
		if now.Sub(t) > s.ttl {
			delete(s.at, k)
		}
	}
	_, dup := s.at[key]
	s.at[key] = now
	return dup
}

// nameCache caches users.info display names for a TTL.
type nameCache struct {
	ttl time.Duration
	mu  sync.Mutex
	m   map[string]nameEntry
}

type nameEntry struct {
	name string
	at   time.Time
}

func newNameCache(ttl time.Duration) *nameCache {
	return &nameCache{ttl: ttl, m: map[string]nameEntry{}}
}

func (c *nameCache) get(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[id]
	if !ok || time.Since(e.at) > c.ttl {
		return "", false
	}
	return e.name, true
}

func (c *nameCache) put(id, name string) {
	c.mu.Lock()
	c.m[id] = nameEntry{name: name, at: time.Now()}
	c.mu.Unlock()
}
