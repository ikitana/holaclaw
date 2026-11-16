package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"holaclaw/internal/provider"
	"holaclaw/internal/provider/wire"
)

// httpError maps a non-2xx response onto a typed provider error.
func (c *Client) httpError(resp *http.Response, body []byte) *provider.Error {
	msg := c.redact(wire.ErrorMessage(body))
	e := &provider.Error{Provider: c.name, Status: resp.StatusCode, Message: msg}
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		e.Kind = provider.ErrAuth
	case resp.StatusCode == http.StatusTooManyRequests:
		e.Kind = provider.ErrRateLimit
		e.RetryAfter = wire.ParseRetryAfter(resp.Header.Get("Retry-After"))
	case resp.StatusCode == http.StatusBadRequest:
		if wire.MentionsContext(msg) {
			e.Kind = provider.ErrContext
		} else {
			e.Kind = provider.ErrBadRequest
		}
	case resp.StatusCode >= 500:
		e.Kind = provider.ErrServer
	default:
		e.Kind = provider.ErrBadRequest
	}
	return e
}

// streamError maps an error delivered inside a 200 SSE stream (OpenRouter).
func (c *Client) streamError(raw json.RawMessage) *provider.Error {
	msg := c.redact(wire.MessageFrom(raw))
	if msg == "" {
		msg = "stream error"
	}
	e := &provider.Error{Provider: c.name, Message: msg}
	var b struct {
		Code any `json:"code"`
	}
	_ = json.Unmarshal(raw, &b)
	status := 0
	switch v := b.Code.(type) {
	case float64:
		status = int(v)
	case string:
		status, _ = strconv.Atoi(v)
	}
	e.Status = status
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Kind = provider.ErrAuth
	case status == http.StatusTooManyRequests:
		e.Kind = provider.ErrRateLimit
	case status == http.StatusBadRequest && wire.MentionsContext(msg):
		e.Kind = provider.ErrContext
	case status == http.StatusBadRequest:
		e.Kind = provider.ErrBadRequest
	case status >= 500:
		e.Kind = provider.ErrServer
	case wire.MentionsContext(msg):
		e.Kind = provider.ErrContext
	default:
		e.Kind = provider.ErrServer
	}
	return e
}

// ctxError maps a transport failure: deadlines become timeouts, everything
// else is a network error.
func (c *Client) ctxError(err error) *provider.Error {
	kind := provider.ErrNetwork
	if errors.Is(err, context.DeadlineExceeded) || wire.IsTimeout(err) {
		kind = provider.ErrTimeout
	}
	return &provider.Error{
		Kind:     kind,
		Provider: c.name,
		Message:  c.redact(err.Error()),
		Err:      err,
	}
}

// redact removes the API key from anything that may be shown or logged.
func (c *Client) redact(s string) string {
	if s == "" {
		return s
	}
	if c.apiKey != nil {
		if key := c.apiKey(); len(key) >= 8 {
			s = strings.ReplaceAll(s, key, "[redacted]")
		}
	}
	if i := strings.Index(strings.ToLower(s), "authorization"); i >= 0 {
		// Never echo an Authorization header back, whatever follows it.
		s = s[:i] + "[redacted]"
	}
	return s
}
