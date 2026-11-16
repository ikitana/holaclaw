package cdp

import "context"

// Fetch is where the navigation allow-list is enforced (docs/browser.md): every request
// pauses, the sidecar decides, and a refused one is failed. In a prompt it
// would be advice; here it is the network.

// EventRequestPaused is Fetch.requestPaused.
const EventRequestPaused = "Fetch.requestPaused"

// RequestPattern selects which requests pause. An empty pattern set pauses
// every request at the request stage.
type RequestPattern struct {
	URLPattern   string `json:"urlPattern,omitempty"`
	ResourceType string `json:"resourceType,omitempty"` // "Document", "XHR", …
	RequestStage string `json:"requestStage,omitempty"` // "Request" or "Response"
}

// Request is the paused request itself. Everything in it came from the page.
type Request struct {
	URL         string            `json:"url"`
	Method      string            `json:"method"`
	Headers     map[string]string `json:"headers,omitempty"`
	HasPostData bool              `json:"hasPostData,omitempty"`
}

// RequestPausedEvent is Fetch.requestPaused. A redirect pauses again, which is
// why the allow-list cannot be checked at navigation time only.
type RequestPausedEvent struct {
	RequestID           string  `json:"requestId"`
	Request             Request `json:"request"`
	FrameID             string  `json:"frameId"`
	ResourceType        string  `json:"resourceType"`
	NetworkID           string  `json:"networkId,omitempty"`
	ResponseStatusCode  int     `json:"responseStatusCode,omitempty"`
	ResponseErrorReason string  `json:"responseErrorReason,omitempty"`
}

// FetchEnable starts pausing requests that match patterns.
func (s *Session) FetchEnable(ctx context.Context, patterns []RequestPattern) error {
	params := map[string]any{}
	if len(patterns) > 0 {
		params["patterns"] = patterns
	}
	return s.Call(ctx, "Fetch.enable", params, nil)
}

// FetchDisable stops pausing and releases anything still held.
func (s *Session) FetchDisable(ctx context.Context) error {
	return s.Call(ctx, "Fetch.disable", nil, nil)
}

// ContinueRequest lets a paused request through unchanged.
func (s *Session) ContinueRequest(ctx context.Context, requestID string) error {
	return s.Call(ctx, "Fetch.continueRequest", map[string]any{"requestId": requestID}, nil)
}

// FailRequest blocks a paused request. errorReason is a network error name;
// "BlockedByClient" is what a disallowed navigation gets.
func (s *Session) FailRequest(ctx context.Context, requestID, errorReason string) error {
	return s.Call(ctx, "Fetch.failRequest", map[string]any{
		"requestId":   requestID,
		"errorReason": errorReason,
	}, nil)
}
