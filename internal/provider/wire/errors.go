package wire

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// contextPhrases are the wordings endpoints use when the prompt does not fit.
var contextPhrases = []string{
	"context length", "context window", "context_length", "maximum context",
	"too many tokens", "exceeds context", "reduce the length", "n_ctx",
	"prompt is too long", "input is too long", "exceed the context",
}

// errorEnvelope covers the OpenAI shape and the looser variants endpoints use.
type errorEnvelope struct {
	Error json.RawMessage `json:"error"`
}

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// ErrorMessage extracts a human message from an OpenAI-style error payload.
// It falls back to the raw body, trimmed to something loggable.
func ErrorMessage(body []byte) string {
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && len(env.Error) > 0 {
		if m := MessageFrom(env.Error); m != "" {
			return m
		}
	}
	if m := MessageFrom(body); m != "" {
		return m
	}
	return Truncate(strings.TrimSpace(string(body)), 400)
}

// MessageFrom extracts a message from a raw JSON fragment that is either a
// plain string or an object carrying a "message" field. It returns "" when
// neither shape matches, which callers use to decide on their own fallback
// text (unlike ErrorMessage, which always falls back to the truncated body).
func MessageFrom(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return Truncate(s, 400)
	}
	var b errorBody
	if err := json.Unmarshal(raw, &b); err == nil && b.Message != "" {
		return Truncate(b.Message, 400)
	}
	return ""
}

// Truncate shortens s to at most n bytes, marking the cut with an ellipsis.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// IsTimeout reports whether err is a network timeout.
func IsTimeout(err error) bool {
	var t interface{ Timeout() bool }
	return errors.As(err, &t) && t.Timeout()
}

// MentionsContext reports whether msg reads like a context-window overflow.
func MentionsContext(msg string) bool {
	low := strings.ToLower(msg)
	for _, p := range contextPhrases {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}

// ParseRetryAfter parses a Retry-After header value, either delta-seconds or
// an HTTP date, into a duration. It returns 0 when v is empty, unparsable, or
// names a time already in the past.
func ParseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}
