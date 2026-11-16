package images

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"holaclaw/internal/provider"
)

// wireRequest is the image request body.
type wireRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

// wireResponse is the 200 reply. media_type is read but never trusted: the
// contract says the mime comes from the bytes.
type wireResponse struct {
	Data []struct {
		B64JSON string `json:"b64_json"`
	} `json:"data"`
	Usage *wireUsage `json:"usage"`
}

// wireUsage is the reply's usage under either naming: OpenAI's images API
// says input/output tokens (with details), OpenRouter's says prompt and
// completion tokens and adds the cost (as text, for provider.WireCost).
// Absent fields stay nil, not zero.
type wireUsage struct {
	InputTokens        *int            `json:"input_tokens"`
	OutputTokens       *int            `json:"output_tokens"`
	PromptTokens       int             `json:"prompt_tokens"`
	CompletionTokens   int             `json:"completion_tokens"`
	Cost               json.RawMessage `json:"cost"`
	InputTokensDetails *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// usage is the provider's view of the reply's usage.
func (r wireResponse) usage() provider.Usage {
	w := r.Usage
	if w == nil {
		return provider.Usage{}
	}
	u := provider.Usage{PromptTokens: w.PromptTokens, CompletionTokens: w.CompletionTokens, Cost: provider.WireCost(w.Cost)}
	if w.InputTokens != nil {
		u.PromptTokens = *w.InputTokens
	}
	if w.OutputTokens != nil {
		u.CompletionTokens = *w.OutputTokens
	}
	if w.InputTokensDetails != nil {
		u.CachedTokens = w.InputTokensDetails.CachedTokens
	}
	return u
}

// GenerateImage asks the model for one image. There are no retries. A reply
// that parsed but holds no usable image returns the error with a response
// carrying only the usage.
func (c *Client) GenerateImage(ctx context.Context, req provider.ImageRequest) (*provider.ImageResponse, error) {
	if req.Prompt == "" {
		return nil, c.badRequest("empty prompt")
	}
	if len(req.Prompt) > MaxPromptBytes {
		return nil, c.badRequest("prompt too long")
	}
	url := c.endpoint(req.Model)
	if url == "" {
		return nil, c.badRequest("no image endpoint: set a base url or a model url")
	}
	body, err := json.Marshal(wireRequest{Model: req.Model.ID, Prompt: req.Prompt})
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrBadRequest, Provider: c.name, Message: "encoding request", Err: err}
	}

	raw, err := c.post(ctx, req.Model, url, body)
	if err != nil {
		return nil, err
	}
	var out wireResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, c.serverError("malformed JSON in response")
	}
	usage := out.usage()
	if len(out.Data) == 0 || out.Data[0].B64JSON == "" {
		return &provider.ImageResponse{Usage: usage}, c.serverError("response carried no image")
	}
	data, err := c.decodeImage(out.Data[0].B64JSON)
	if err != nil {
		return &provider.ImageResponse{Usage: usage}, err
	}
	mime := sniffMime(data)
	if mime == "" {
		return &provider.ImageResponse{Usage: usage}, c.serverError("unsupported image type")
	}
	return &provider.ImageResponse{Data: data, Mime: mime, Usage: usage}, nil
}

// post sends one request under the concurrency limit and returns the body.
func (c *Client) post(ctx context.Context, model provider.Model, url string, body []byte) ([]byte, error) {
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	defer c.release()
	rctx, cancel := context.WithTimeout(ctx, c.requestTimeout(model))
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrBadRequest, Provider: c.name, Message: "bad request url", Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.apiKey != nil {
		if key := c.apiKey(); key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, c.transportError(ctx, rctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, c.httpError(resp, errBody)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, c.transportError(ctx, rctx, err)
	}
	if len(raw) > MaxResponseBytes {
		return nil, c.serverError("response too large")
	}
	return raw, nil
}

// decodeImage decodes the base64 payload, tolerating a data: URI prefix, the
// raw (unpadded) alphabet and the URL-safe one, and refusing anything that
// would decode past MaxImageBytes.
func (c *Client) decodeImage(s string) ([]byte, error) {
	if i := strings.Index(s, "base64,"); i >= 0 && strings.HasPrefix(s, "data:") {
		s = s[i+len("base64,"):]
	}
	s = strings.TrimSpace(s)
	// 3 bytes out per 4 in: refuse before allocating.
	if len(s)/4*3 > MaxImageBytes {
		return nil, c.serverError("image too large")
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		if data, err := enc.DecodeString(s); err == nil {
			if len(data) > MaxImageBytes {
				return nil, c.serverError("image too large")
			}
			if len(data) == 0 {
				return nil, c.serverError("response carried no image")
			}
			return data, nil
		}
	}
	return nil, c.serverError("image is not valid base64")
}

// sniffMime derives the mime type from the bytes themselves. It returns "" for
// anything that is not one of the three types the contract allows.
func sniffMime(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}
