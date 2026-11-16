// Package images implements provider.ImageGenerator for endpoints that speak
// the OpenRouter image wire format: POST <base_url>/images with a model and a
// prompt, back a base64 image with usage and cost (docs/models.md, "Image
// models").
package images

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"holaclaw/internal/provider"
	"holaclaw/internal/provider/wire"
)

// Defaults and hard caps.
const (
	// DefaultMaxConcurrent caps in-flight image calls per client.
	DefaultMaxConcurrent = 2
	// DefaultRequestTimeout bounds a call when Model.Timeout is 0. Measured
	// ≈7 s, but these models reason before they render.
	DefaultRequestTimeout = 120 * time.Second
	// MaxPromptBytes is the largest prompt accepted; longer is refused, never
	// silently truncated.
	MaxPromptBytes = 4000
	// MaxResponseBytes caps how much of a reply is read (base64 is ~4/3 of the
	// image it carries).
	MaxResponseBytes = 32 << 20
	// MaxImageBytes caps the decoded image.
	MaxImageBytes = 20 << 20
	// MaxMessageBytes caps an upstream error message.
	MaxMessageBytes = 300

	maxErrorBody = 64 << 10
)

// Options configures a Client. Only BaseURL is required, and only for models
// that do not carry an absolute URL of their own.
type Options struct {
	Name    string // provider instance name, used in errors
	BaseURL string // e.g. "https://openrouter.ai/api/v1"
	// APIKey is called once per request so rotated secrets are picked up.
	// Nil or an empty result means no Authorization header. Never logged.
	APIKey func() string
	// MaxConcurrent caps in-flight requests (default DefaultMaxConcurrent).
	MaxConcurrent int
	// HTTPClient overrides the default client (which has no global timeout:
	// deadlines are per request via context).
	HTTPClient *http.Client
	// DefaultTimeout bounds a whole request when Model.Timeout is 0.
	DefaultTimeout time.Duration
}

// Client is a configured connection to one image endpoint. It implements
// provider.ImageGenerator.
type Client struct {
	name    string
	baseURL string // no trailing slash
	apiKey  func() string
	hc      *http.Client
	timeout time.Duration
	sem     chan struct{}
}

var _ provider.ImageGenerator = (*Client)(nil)

// New builds a Client from opts.
func New(opts Options) *Client {
	c := &Client{
		name:    opts.Name,
		baseURL: strings.TrimRight(opts.BaseURL, "/"),
		apiKey:  opts.APIKey,
		hc:      opts.HTTPClient,
		timeout: opts.DefaultTimeout,
	}
	if c.name == "" {
		c.name = "images"
	}
	if c.hc == nil {
		// Never follow a redirect: the target is chosen by the upstream, and the
		// request carries a bearer key and content that must not travel there.
		c.hc = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if c.timeout <= 0 {
		c.timeout = DefaultRequestTimeout
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
		return c.ctxError(ctx.Err())
	}
}

func (c *Client) release() { <-c.sem }

// endpoint is the URL to post to: the model's own, else BaseURL + "/images".
func (c *Client) endpoint(m provider.Model) string {
	if m.URL != "" {
		return m.URL
	}
	if c.baseURL == "" {
		return ""
	}
	return c.baseURL + "/images"
}

// requestTimeout is the whole-request budget for m.
func (c *Client) requestTimeout(m provider.Model) time.Duration {
	if m.Timeout > 0 {
		return m.Timeout
	}
	return c.timeout
}

// badRequest is a local rejection: the call never reaches the network.
func (c *Client) badRequest(msg string) *provider.Error {
	return &provider.Error{Kind: provider.ErrBadRequest, Provider: c.name, Message: msg}
}

// serverError reports an unusable reply, with one of our own fixed messages.
func (c *Client) serverError(msg string) *provider.Error {
	return &provider.Error{Kind: provider.ErrServer, Provider: c.name, Message: msg}
}

// httpError maps a non-2xx response onto a typed provider error. Both error
// shapes are handled: the OpenAI-style {"error":{"message"}} and the validation
// {"success":false,"error":{"name":"ZodError","message"}}.
func (c *Client) httpError(resp *http.Response, body []byte) *provider.Error {
	msg := c.redact(wire.Truncate(wire.ErrorMessage(body), MaxMessageBytes))
	e := &provider.Error{Provider: c.name, Status: resp.StatusCode, Message: msg}
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		e.Kind = provider.ErrAuth
	case resp.StatusCode == http.StatusPaymentRequired:
		e.Kind = provider.ErrUsageLimit
		e.RetryAfter = wire.ParseRetryAfter(resp.Header.Get("Retry-After"))
	case resp.StatusCode == http.StatusTooManyRequests:
		e.Kind = provider.ErrRateLimit
		e.RetryAfter = wire.ParseRetryAfter(resp.Header.Get("Retry-After"))
	case resp.StatusCode >= 500:
		e.Kind = provider.ErrServer
		e.RetryAfter = wire.ParseRetryAfter(resp.Header.Get("Retry-After"))
	default:
		e.Kind = provider.ErrBadRequest
	}
	return e
}

// ctxError maps a transport failure: deadlines become timeouts, everything else
// is a network error.
func (c *Client) ctxError(err error) *provider.Error {
	kind := provider.ErrNetwork
	if errors.Is(err, context.DeadlineExceeded) || wire.IsTimeout(err) {
		kind = provider.ErrTimeout
	}
	return &provider.Error{
		Kind:     kind,
		Provider: c.name,
		Message:  c.redact(wire.Truncate(err.Error(), MaxMessageBytes)),
		Err:      err,
	}
}

// transportError distinguishes a caller cancellation from our own deadline.
func (c *Client) transportError(parent, rctx context.Context, err error) *provider.Error {
	if parent.Err() != nil {
		return c.ctxError(parent.Err())
	}
	if rctx.Err() != nil {
		return c.ctxError(rctx.Err())
	}
	return c.ctxError(err)
}

// redact removes the API key from anything that may be shown or logged.
func (c *Client) redact(s string) string {
	if s == "" {
		return s
	}
	if c.apiKey != nil {
		if key := c.apiKey(); len(key) >= 6 {
			s = strings.ReplaceAll(s, key, "[redacted]")
		}
	}
	if i := strings.Index(strings.ToLower(s), "authorization"); i >= 0 {
		// Never echo an Authorization header back, whatever follows it.
		s = s[:i] + "[redacted]"
	}
	return s
}
