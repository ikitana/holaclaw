// Package responses implements provider.Provider for the OpenAI Responses API
// over SSE (docs/models.md). One transport serves two auth strategies: an
// OpenAI API key, and — built on top of this package — the Codex
// ChatGPT-subscription flow, which supplies its own base URL, its own
// per-request headers (chatgpt-account-id, originator, session_id), a token
// refresh hook for 401s, non-empty `instructions`, `store:false` and no
// `max_output_tokens`.
package responses

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"holaclaw/internal/provider"
)

// Defaults for Options left at their zero value.
const (
	DefaultMaxConcurrent  = 2
	DefaultRequestTimeout = 10 * time.Minute
	DefaultProgressPeriod = 10 * time.Second
	DefaultBaseURL        = "https://api.openai.com/v1"
	DefaultPath           = "/responses"
	// DefaultInstructions is sent when Request.System is empty: the Responses
	// API (and Codex in particular) rejects empty instructions.
	DefaultInstructions = "You are a helpful assistant."
)

// Options configures a Client. Every field has a usable default.
type Options struct {
	Name    string // provider instance name, used in errors
	BaseURL string // default DefaultBaseURL
	Path    string // default DefaultPath; joined onto BaseURL
	// APIKey is called once per request so rotated secrets are picked up.
	// Nil or an empty result means no Authorization header. Never logged.
	APIKey func() string
	// Headers returns extra headers for one request. It is called on every
	// attempt, so a refreshed account id or session id is picked up. Values are
	// treated as secrets: they are redacted out of error messages.
	Headers func(req provider.Request) map[string]string
	// Store sets the `store` field. Nil means false, which is also what is sent
	// when it is false: the field is always present on the wire.
	Store *bool
	// OmitMaxOutputTokens drops max_output_tokens entirely (Codex forbids it).
	OmitMaxOutputTokens bool
	// MaxConcurrent caps in-flight requests (default DefaultMaxConcurrent).
	MaxConcurrent int
	// HTTPClient overrides the default client (which has no global timeout:
	// deadlines are per request via context).
	HTTPClient *http.Client
	// DefaultTimeout bounds a whole request when Model.Timeout is 0.
	DefaultTimeout time.Duration
	// ProgressInterval is how often ChunkProgress is emitted while the endpoint
	// is quiet.
	ProgressInterval time.Duration
	// OnUnauthorized, when set, is called at most once per request on a 401 —
	// a token refresh. The request is then retried once with fresh headers and
	// key; a second 401 is a provider.Error of kind auth.
	OnUnauthorized func(ctx context.Context) error
}

// Client is a configured connection to one Responses endpoint. It implements
// provider.Provider.
type Client struct {
	name       string
	url        string // full endpoint URL
	apiKey     func() string
	headers    func(provider.Request) map[string]string
	store      bool
	omitMaxOut bool
	hc         *http.Client
	timeout    time.Duration
	progress   time.Duration
	onUnauth   func(context.Context) error
	sem        chan struct{}
}

var _ provider.Provider = (*Client)(nil)

// New builds a Client from opts.
func New(opts Options) *Client {
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	path := opts.Path
	if path == "" {
		path = DefaultPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	c := &Client{
		name:       opts.Name,
		url:        base + path,
		apiKey:     opts.APIKey,
		headers:    opts.Headers,
		omitMaxOut: opts.OmitMaxOutputTokens,
		hc:         opts.HTTPClient,
		timeout:    opts.DefaultTimeout,
		progress:   opts.ProgressInterval,
		onUnauth:   opts.OnUnauthorized,
	}
	if opts.Store != nil {
		c.store = *opts.Store
	}
	if c.name == "" {
		c.name = "responses"
	}
	if c.hc == nil {
		c.hc = &http.Client{}
	}
	if c.timeout <= 0 {
		c.timeout = DefaultRequestTimeout
	}
	if c.progress <= 0 {
		c.progress = DefaultProgressPeriod
	}
	n := opts.MaxConcurrent
	if n <= 0 {
		n = DefaultMaxConcurrent
	}
	c.sem = make(chan struct{}, n)
	return c
}

// Name reports the provider instance name.
func (c *Client) Name() string { return c.name }

// acquire takes a concurrency slot, honouring ctx while waiting.
func (c *Client) acquire(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return c.ctxError(ctx.Err(), nil)
	}
}

func (c *Client) release() { <-c.sem }

// newRequest builds the POST with auth and the per-request headers applied.
func (c *Client) newRequest(ctx context.Context, req provider.Request, body io.Reader) (*http.Request, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, body)
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrBadRequest, Provider: c.name, Message: "bad request url", Err: err}
	}
	for k, v := range c.dynamicHeaders(req) {
		httpReq.Header.Set(k, v)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.apiKey != nil {
		if key := c.apiKey(); key != "" {
			httpReq.Header.Set("Authorization", "Bearer "+key)
		}
	}
	return httpReq, nil
}

func (c *Client) dynamicHeaders(req provider.Request) map[string]string {
	if c.headers == nil {
		return nil
	}
	return c.headers(req)
}

// secrets lists the values that must never appear in an error message: the API
// key and every dynamic header value (account id, session id).
func (c *Client) secrets(req provider.Request) []string {
	var out []string
	if c.apiKey != nil {
		if key := c.apiKey(); key != "" {
			out = append(out, key)
		}
	}
	for _, v := range c.dynamicHeaders(req) {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// requestTimeout is the whole-request budget for m.
func (c *Client) requestTimeout(m provider.Model) time.Duration {
	if m.Timeout > 0 {
		return m.Timeout
	}
	return c.timeout
}
