package openaicompat

import (
	"context"
	"net/http"

	"holaclaw/internal/provider"
)

var _ provider.ModelLister = (*Client)(nil)

// Models lists the model ids the endpoint serves, from GET <base_url>/models
// (provider.ModelLister). An answer without a single id is an error: it says
// nothing about what the endpoint serves.
func (c *Client) Models(ctx context.Context) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.doJSON(ctx, provider.Model{}, http.MethodGet, "/models", "", nil, &out); err != nil {
		return nil, err
	}
	var ids []string
	for _, d := range out.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	if len(ids) == 0 {
		return nil, &provider.Error{Kind: provider.ErrServer, Provider: c.name, Message: "the model list is empty"}
	}
	return ids, nil
}
