// Package openaicompat implements provider.Provider for any endpoint that
// speaks the OpenAI Chat Completions wire format: Ollama, llama.cpp, LM Studio,
// vLLM, OpenRouter (docs/models.md). It is written for local runtimes first —
// long but finite timeouts, progress events, a reasoning-tag scrubber, layered
// argument repair and dropped-stream detection.
package openaicompat

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
	DefaultMaxConcurrent   = 2
	DefaultRequestTimeout  = 10 * time.Minute
	DefaultProgressPeriod  = 10 * time.Second
	defaultTranscribeMIME  = "audio/mpeg"
	defaultTranscribeFname = "audio.mp3"
)

// Options configures a Client. Only BaseURL is required.
type Options struct {
	Name    string // provider instance name, used in errors
	BaseURL string // e.g. "http://localhost:11434/v1"
	// APIKey is called once per request so rotated secrets are picked up.
	// Nil or an empty result means no Authorization header. Never logged.
	APIKey func() string
	// Headers are extra static headers sent with every request.
	Headers map[string]string
	// MaxConcurrent caps in-flight requests (default DefaultMaxConcurrent).
	MaxConcurrent int
	// HTTPClient overrides the default client (which has no global timeout:
	// deadlines are per request via context).
	HTTPClient *http.Client
	// Sort is OpenRouter's provider.sort routing preference ("throughput" |
	// "latency" | "price"); "" sends nothing. Other servers ignore the field.
	Sort string
	// DefaultTimeout bounds a whole request when Model.Timeout is 0.
	DefaultTimeout time.Duration
	// ProgressInterval is how often ChunkProgress is emitted while the model
	// is quiet (slow local prompt processing).
	ProgressInterval time.Duration
}

// Client is a configured connection to one OpenAI-compatible endpoint. It
// implements provider.Provider, provider.Embedder and provider.Transcriber.
type Client struct {
	name     string
	baseURL  string // no trailing slash
	apiKey   func() string
	headers  map[string]string
	hc       *http.Client
	timeout  time.Duration
	progress time.Duration
	sem      chan struct{}
	sort     string
}

var (
	_ provider.Provider    = (*Client)(nil)
	_ provider.Embedder    = (*Client)(nil)
	_ provider.Transcriber = (*Client)(nil)
)

// New builds a Client from opts.
func New(opts Options) *Client {
	c := &Client{
		name:     opts.Name,
		baseURL:  strings.TrimRight(opts.BaseURL, "/"),
		apiKey:   opts.APIKey,
		hc:       opts.HTTPClient,
		timeout:  opts.DefaultTimeout,
		progress: opts.ProgressInterval,
	}
	if c.name == "" {
		c.name = "openai-compatible"
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
	c.sort = opts.Sort
	if len(opts.Headers) > 0 {
		c.headers = make(map[string]string, len(opts.Headers))
		for k, v := range opts.Headers {
			c.headers[k] = v
		}
	}
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

// newRequest builds an HTTP request with auth and static headers applied.
func (c *Client) newRequest(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrBadRequest, Provider: c.name, Message: "bad request url", Err: err}
	}
	c.applyHeaders(req, contentType)
	return req, nil
}

func (c *Client) applyHeaders(req *http.Request, contentType string) {
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != nil {
		if key := c.apiKey(); key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
}

// requestTimeout is the whole-request budget for m.
func (c *Client) requestTimeout(m provider.Model) time.Duration {
	if m.Timeout > 0 {
		return m.Timeout
	}
	return c.timeout
}
