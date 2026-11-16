package telegram

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Kind classifies a Bot API failure so the daemon can react without parsing
// strings.
type Kind string

const (
	KindAuth       Kind = "auth"        // 401: token revoked, instance is logged out
	KindForbidden  Kind = "forbidden"   // 403: blocked by the user, kicked from the chat
	KindRateLimit  Kind = "rate_limit"  // 429, carries RetryAfter
	KindConflict   Kind = "conflict"    // 409: another poller holds getUpdates
	KindBadRequest Kind = "bad_request" // 400 and anything else in the 4xx range
	KindServer     Kind = "server"      // 5xx
	KindNetwork    Kind = "network"     // transport failure, never a status code
)

// Sentinels for errors.Is. An *Error unwraps to the sentinel of its kind.
var (
	ErrAuth       = errors.New("telegram: unauthorized")
	ErrForbidden  = errors.New("telegram: forbidden")
	ErrRateLimit  = errors.New("telegram: rate limited")
	ErrConflict   = errors.New("telegram: conflict")
	ErrBadRequest = errors.New("telegram: bad request")
	ErrServer     = errors.New("telegram: server error")
	ErrNetwork    = errors.New("telegram: network error")
)

var sentinels = map[Kind]error{
	KindAuth: ErrAuth, KindForbidden: ErrForbidden, KindRateLimit: ErrRateLimit,
	KindConflict: ErrConflict, KindBadRequest: ErrBadRequest,
	KindServer: ErrServer, KindNetwork: ErrNetwork,
}

// Error is a Bot API failure. Description is the API's own text, already
// scrubbed of the bot token — Go's http errors quote the request URL, and the
// URL carries the token.
type Error struct {
	Method      string
	Kind        Kind
	Status      int
	Description string
	RetryAfter  time.Duration
	cause       error // scrubbed: never an error that quotes the URL
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("telegram ")
	b.WriteString(e.Method)
	b.WriteString(": ")
	b.WriteString(string(e.Kind))
	if e.Status != 0 {
		fmt.Fprintf(&b, " (%d)", e.Status)
	}
	if e.Description != "" {
		b.WriteString(": ")
		b.WriteString(e.Description)
	}
	return b.String()
}

// Unwrap yields the sentinel for the kind so errors.Is works, and keeps the
// transport cause reachable for context errors.
func (e *Error) Unwrap() []error {
	out := make([]error, 0, 2)
	if s, ok := sentinels[e.Kind]; ok {
		out = append(out, s)
	}
	if e.cause != nil {
		out = append(out, e.cause)
	}
	return out
}

// kindOf maps a Bot API status code onto a Kind.
func kindOf(status int) Kind {
	switch {
	case status == 401:
		return KindAuth
	case status == 403:
		return KindForbidden
	case status == 409:
		return KindConflict
	case status == 429:
		return KindRateLimit
	case status >= 500:
		return KindServer
	default:
		return KindBadRequest
	}
}

// parseErrorText reports whether a 400 is Telegram rejecting our HTML.
func parseErrorText(e *Error) bool {
	if e == nil || e.Status != 400 {
		return false
	}
	d := strings.ToLower(e.Description)
	return strings.Contains(d, "parse entities") || strings.Contains(d, "parse_mode") ||
		strings.Contains(d, "unsupported start tag") || strings.Contains(d, "can't parse")
}

// replyGone reports whether a 400 is Telegram refusing a reply because the
// message it answers no longer exists.
func replyGone(e *Error) bool {
	if e == nil || e.Status != 400 {
		return false
	}
	d := strings.ToLower(e.Description)
	return strings.Contains(d, "message to be replied not found") || strings.Contains(d, "replied message not found")
}

// scrub removes the bot token from anything that may be logged or returned.
// It is applied to every description, transport error and log line.
func scrub(s, token string) string {
	if s == "" || token == "" {
		return s
	}
	s = strings.ReplaceAll(s, token, "[redacted]")
	// A token is "<id>:<secret>"; some errors quote only the secret half.
	if i := strings.IndexByte(token, ':'); i >= 0 && len(token)-i > 8 {
		s = strings.ReplaceAll(s, token[i+1:], "[redacted]")
	}
	return s
}
