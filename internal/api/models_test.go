package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"holaclaw/internal/provider"
)

// listingProvider answers provider.ModelLister with ids, or with err.
type listingProvider struct {
	chatOnly
	ids []string
	err error
}

func (p listingProvider) Models(context.Context) ([]string, error) { return p.ids, p.err }

// chatOnly is a provider that cannot list its models.
type chatOnly struct{}

func (chatOnly) Name() string { return "test" }
func (chatOnly) Chat(context.Context, provider.Request) (provider.Stream, error) {
	return nil, errors.New("no chat")
}

func TestListProviderModels(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.b.Providers = map[string]provider.Provider{
		"live": listingProvider{ids: []string{"gemma4:e2b", "qwen3:8b"}},
		"down": listingProvider{err: &provider.Error{Kind: provider.ErrServer, Provider: "down", Status: 500, Message: "model list unavailable"}},
		"mute": chatOnly{},
	}

	got := decode[struct {
		Models []string `json:"models"`
	}](t, h.do(http.MethodGet, "/providers/live/models", h.adminToken, nil), http.StatusOK)
	if len(got.Models) != 2 || got.Models[0] != "gemma4:e2b" || got.Models[1] != "qwen3:8b" {
		t.Fatalf("models = %v", got.Models)
	}

	errorBody(t, h.do(http.MethodGet, "/providers/nope/models", h.adminToken, nil), http.StatusNotFound, CodeNotFound)
	errorBody(t, h.do(http.MethodGet, "/providers/mute/models", h.adminToken, nil), http.StatusNotImplemented, CodeNotImplemented)
	errorBody(t, h.do(http.MethodGet, "/providers/down/models", h.adminToken, nil), http.StatusBadGateway, CodeInternal)
	// Admin scope: a client token learns nothing about the providers.
	errorBody(t, h.do(http.MethodGet, "/providers/live/models", h.ownerToken, nil), http.StatusForbidden, CodeForbidden)
}
