package responses

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"holaclaw/internal/provider"
	"holaclaw/internal/provider/wire"
)

// Error codes the Responses API uses that map onto a contract kind.
const (
	codeRateLimit      = "rate_limit_exceeded"
	codeContextLength  = "context_length_exceeded"
	codeUsageLimit     = "usage_limit_reached"
	maxErrorBody       = 64 << 10
	usageLimitFallback = "usage limit reached"
)

// httpError maps a non-2xx response onto a typed provider error. A 429 whose
// body carries usage_limit_reached is a subscription quota with a reset time,
// not a plain rate limit and never a re-login (docs/models.md).
func (c *Client) httpError(resp *http.Response, body []byte, secrets []string) *provider.Error {
	code, resetAt := errorCode(body)
	msg := c.redact(wire.ErrorMessage(body), secrets)
	e := &provider.Error{Provider: c.name, Status: resp.StatusCode, Message: msg}
	switch {
	case code == codeUsageLimit:
		e.Kind = provider.ErrUsageLimit
		e.ResetAt = resetAt
		e.RetryAfter = wire.ParseRetryAfter(resp.Header.Get("Retry-After"))
		if e.Message == "" {
			e.Message = usageLimitFallback
		}
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		e.Kind = provider.ErrAuth
	case resp.StatusCode == http.StatusTooManyRequests:
		e.Kind = provider.ErrRateLimit
		e.RetryAfter = wire.ParseRetryAfter(resp.Header.Get("Retry-After"))
	case resp.StatusCode == http.StatusBadRequest:
		if code == codeContextLength || wire.MentionsContext(msg) {
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

// streamError maps a `response.failed` or `error` event onto a typed error.
func (c *Client) streamError(ev *wireEvent, data string, secrets []string) *provider.Error {
	we := ev.Error
	if we == nil && ev.Response != nil {
		we = ev.Response.Error
	}
	if we == nil {
		// A flat error frame: the fields sit on the event itself.
		we = &wireError{
			Type: ev.Type, Code: ev.Code, Message: ev.Message,
			ResetsAt: ev.ResetsAt, ResetsInSeconds: ev.ResetsInSeconds,
		}
	}
	code := we.Code
	if code == "" {
		code = we.Type
	}
	msg := c.redact(we.Message, secrets)
	if msg == "" {
		msg = c.redact(wire.ErrorMessage([]byte(data)), secrets)
	}
	if msg == "" {
		msg = "stream error"
	}
	e := &provider.Error{Provider: c.name, Message: msg}
	switch code {
	case codeUsageLimit:
		e.Kind = provider.ErrUsageLimit
		e.ResetAt = resetTime(we.ResetsAt, we.ResetsInSeconds)
	case codeRateLimit:
		e.Kind = provider.ErrRateLimit
	case codeContextLength:
		e.Kind = provider.ErrContext
	default:
		if wire.MentionsContext(msg) {
			e.Kind = provider.ErrContext
		} else {
			e.Kind = provider.ErrServer
		}
	}
	return e
}

// ctxError maps a transport failure: deadlines become timeouts, everything
// else is a network error.
func (c *Client) ctxError(err error, secrets []string) *provider.Error {
	kind := provider.ErrNetwork
	if isDeadline(err) {
		kind = provider.ErrTimeout
	}
	return &provider.Error{
		Kind:     kind,
		Provider: c.name,
		Message:  c.redact(err.Error(), secrets),
		Err:      err,
	}
}

// errorCode digs the error code and any reset time out of an error body,
// whatever nesting the endpoint used.
func errorCode(body []byte) (string, time.Time) {
	var env struct {
		Error  *wireError `json:"error"`
		Detail *wireError `json:"detail"`
		wireError
	}
	if err := json.Unmarshal(body, &env); err != nil {
		// Not JSON: fall back to a substring so a plain-text quota page is
		// still recognised.
		if strings.Contains(string(body), codeUsageLimit) {
			return codeUsageLimit, time.Time{}
		}
		return "", time.Time{}
	}
	for _, e := range []*wireError{env.Error, env.Detail, &env.wireError} {
		if e == nil {
			continue
		}
		code := e.Code
		if code == "" {
			code = e.Type
		}
		if code != "" {
			return code, resetTime(e.ResetsAt, e.ResetsInSeconds)
		}
	}
	return "", time.Time{}
}

// resetTime turns a `resets_at` unix timestamp or a `resets_in_seconds` offset
// into an absolute time.
func resetTime(at, in *json.Number) time.Time {
	if at != nil {
		if secs, err := strconv.ParseInt(strings.TrimSpace(at.String()), 10, 64); err == nil && secs > 0 {
			return time.Unix(secs, 0).UTC()
		}
	}
	if in != nil {
		if secs, err := strconv.ParseFloat(strings.TrimSpace(in.String()), 64); err == nil && secs > 0 {
			return time.Now().Add(time.Duration(secs * float64(time.Second))).UTC()
		}
	}
	return time.Time{}
}

// isDeadline reports whether err is a context deadline or a network timeout.
func isDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || wire.IsTimeout(err)
}

// redact removes every secret — the API key and the dynamic header values —
// from anything that may be shown or logged.
func (c *Client) redact(s string, secrets []string) string {
	if s == "" {
		return s
	}
	for _, sec := range secrets {
		if len(sec) >= 6 {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	if i := strings.Index(strings.ToLower(s), "authorization"); i >= 0 {
		// Never echo an Authorization header back, whatever follows it.
		s = s[:i] + "[redacted]"
	}
	return s
}
