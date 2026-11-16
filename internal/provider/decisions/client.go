// Package decisions implements provider.Decider for models that answer narrow,
// typed questions about a state instead of chatting (docs/models.md,
// "Decision models").
//
// EXPERIMENTAL: the only upstream is OpenRouter's alpha decisions API. There is
// no standard path to derive, so the endpoint is an explicit absolute Model.URL;
// the wire format may change under us and a Decider never retries, so every
// caller must have a defined behaviour when this fails.
package decisions

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"holaclaw/internal/provider"
	"holaclaw/internal/provider/wire"
)

// Defaults for Options left at their zero value.
const (
	// DefaultMaxConcurrent caps in-flight decision calls per client.
	DefaultMaxConcurrent = 2
	// DefaultRequestTimeout bounds a call when Model.Timeout is 0. Typed
	// answers are fast (≈0.4 s measured); a slow one is worth failing over.
	DefaultRequestTimeout = 2 * time.Second
	// MaxResponseBytes caps how much of a reply is read.
	MaxResponseBytes = 1 << 20
	// MaxMessageBytes caps an upstream error message: a schema error comes back
	// as a very long JSON string.
	MaxMessageBytes = 300
)

// Options configures a Client. All fields are optional; the endpoint comes from
// Model.URL, not from here.
type Options struct {
	Name string // provider instance name, used in errors
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

// Client is a configured connection to one decisions endpoint. It implements
// provider.Decider.
type Client struct {
	name    string
	apiKey  func() string
	hc      *http.Client
	timeout time.Duration
	sem     chan struct{}
}

var _ provider.Decider = (*Client)(nil)

// New builds a Client from opts.
func New(opts Options) *Client {
	c := &Client{
		name:    opts.Name,
		apiKey:  opts.APIKey,
		hc:      opts.HTTPClient,
		timeout: opts.DefaultTimeout,
	}
	if c.name == "" {
		c.name = "decisions"
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

// invalidResponse reports a reply that does not match what was asked. The
// message is always one of our own fixed strings: upstream answer text is never
// echoed, because the contract promises every string in an Answer comes from
// the closed set the caller sent.
func (c *Client) invalidResponse(msg string) *provider.Error {
	return &provider.Error{Kind: provider.ErrServer, Provider: c.name, Message: "invalid decision response: " + msg}
}

// httpError maps a non-2xx response onto a typed provider error.
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
	case resp.StatusCode == http.StatusBadRequest:
		e.Kind = provider.ErrBadRequest
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
	if isDeadline(err) {
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

// isDeadline reports whether err is a context deadline or a network timeout.
func isDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || wire.IsTimeout(err)
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
