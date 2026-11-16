package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"

	"holaclaw/internal/provider"
)

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Usage *wireUsage `json:"usage"`
}

// Embed returns one vector per input, in input order. A reply that does not
// match the inputs is an error that still carries the reply's usage: the
// call was made and paid for.
func (c *Client) Embed(ctx context.Context, model provider.Model, inputs []string) ([][]float32, provider.Usage, error) {
	if len(inputs) == 0 {
		return nil, provider.Usage{}, nil
	}
	body, err := json.Marshal(embedRequest{Model: model.ID, Input: inputs})
	if err != nil {
		return nil, provider.Usage{}, &provider.Error{Kind: provider.ErrBadRequest, Provider: c.name, Message: "encoding request", Err: err}
	}
	var out embedResponse
	if err := c.doJSON(ctx, model, http.MethodPost, "/embeddings", "application/json", bytes.NewReader(body), &out); err != nil {
		return nil, provider.Usage{}, err
	}
	usage := out.Usage.usage()
	if len(out.Data) != len(inputs) {
		return nil, usage, &provider.Error{
			Kind: provider.ErrServer, Provider: c.name,
			Message: "embeddings: got a different number of vectors than inputs",
		}
	}
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })
	vecs := make([][]float32, 0, len(out.Data))
	for _, d := range out.Data {
		vecs = append(vecs, d.Embedding)
	}
	return vecs, usage, nil
}

// doJSON performs a non-streaming request under the concurrency limit and
// decodes a JSON reply.
func (c *Client) doJSON(ctx context.Context, model provider.Model, method, path, contentType string, body io.Reader, out any) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	rctx, cancel := context.WithTimeout(ctx, c.requestTimeout(model))
	defer cancel()

	req, err := c.newRequest(rctx, method, path, contentType, body)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return c.transportError(ctx, rctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return c.httpError(resp, errBody)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return c.transportError(ctx, rctx, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return &provider.Error{
			Kind: provider.ErrServer, Provider: c.name, Status: resp.StatusCode,
			Message: "decoding response: " + err.Error(), Err: err,
		}
	}
	return nil
}
