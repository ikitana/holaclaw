package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"holaclaw/internal/provider"
)

// ErrProbeUnsupported is returned when the endpoint exposes no way to report
// the context it actually allocated.
var ErrProbeUnsupported = errors.New("openaicompat: context probe unsupported by this endpoint")

// Probe sources reported by ProbeContext.
const (
	SourceOllamaNumCtx    = "ollama:num_ctx"
	SourceOllamaModelInfo = "ollama:model_info"
	SourceLlamaCppProps   = "llamacpp:props"
)

// ProbeContext reports the context window the runtime actually allocated for
// model, best effort, so `holaclaw check` can compare it with the configured
// value (docs/models.md). It tries Ollama's /api/show, then llama.cpp's
// /props, and returns ErrProbeUnsupported when neither answers.
func (c *Client) ProbeContext(ctx context.Context, model provider.Model) (int, string, error) {
	root := c.rootURL()
	if n, src, ok := c.probeOllama(ctx, root, model.ID); ok {
		return n, src, nil
	}
	if n, ok := c.probeLlamaCPP(ctx, root); ok {
		return n, SourceLlamaCppProps, nil
	}
	return 0, "", ErrProbeUnsupported
}

// rootURL is the base URL without the OpenAI-compatible /v1 suffix: the
// native admin endpoints live next to it.
func (c *Client) rootURL() string {
	return strings.TrimSuffix(c.baseURL, "/v1")
}

func (c *Client) probeOllama(ctx context.Context, root, id string) (int, string, bool) {
	body, err := json.Marshal(map[string]string{"model": id})
	if err != nil {
		return 0, "", false
	}
	var out struct {
		Parameters string         `json:"parameters"`
		ModelInfo  map[string]any `json:"model_info"`
	}
	if !c.probeJSON(ctx, http.MethodPost, root+"/api/show", bytes.NewReader(body), &out) {
		return 0, "", false
	}
	// "parameters" is a plain text block of "key value" lines.
	for _, line := range strings.Split(out.Parameters, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "num_ctx" {
			if n, err := strconv.Atoi(fields[1]); err == nil && n > 0 {
				return n, SourceOllamaNumCtx, true
			}
		}
	}
	for k, v := range out.ModelInfo {
		if !strings.HasSuffix(k, ".context_length") {
			continue
		}
		if n, ok := toInt(v); ok && n > 0 {
			return n, SourceOllamaModelInfo, true
		}
	}
	return 0, "", false
}

func (c *Client) probeLlamaCPP(ctx context.Context, root string) (int, bool) {
	var out struct {
		DefaultGenerationSettings struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if !c.probeJSON(ctx, http.MethodGet, root+"/props", nil, &out) {
		return 0, false
	}
	if n := out.DefaultGenerationSettings.NCtx; n > 0 {
		return n, true
	}
	return 0, false
}

// probeJSON performs a best-effort request: any failure just means "this
// endpoint is not that runtime".
func (c *Client) probeJSON(ctx context.Context, method, url string, body io.Reader, out any) bool {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return false
	}
	c.applyHeaders(req, "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, out) == nil
}

func toInt(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case json.Number:
		n, err := t.Int64()
		return int(n), err == nil
	case string:
		n, err := strconv.Atoi(t)
		return n, err == nil
	}
	return 0, false
}
