package slack

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors for the Slack failures core reacts to. Every other
// `ok:false` code is carried in Error.Code.
var (
	ErrInvalidAuth       = errors.New("slack: invalid_auth")
	ErrChannelNotFound   = errors.New("slack: channel_not_found")
	ErrNotInChannel      = errors.New("slack: not_in_channel")
	ErrRateLimited       = errors.New("slack: ratelimited")
	ErrCrossHostRedirect = errors.New("slack: redirect to another host with the bot token")
	ErrTooLarge          = errors.New("slack: file exceeds the size cap")
)

// Error is a failed Web API call: either a transport/HTTP failure or an
// `ok:false` response. Token values never appear in Message.
type Error struct {
	Op         string        // API method, e.g. "chat.postMessage"
	Code       string        // Slack error code, empty for transport errors
	HTTP       int           // HTTP status, 0 when the request never completed
	RetryAfter time.Duration // from Retry-After on 429
	Message    string
	Err        error // wrapped sentinel or transport error
}

func (e *Error) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("slack %s: %s: %s", e.Op, e.Code, e.Message)
	case e.Code != "":
		return fmt.Sprintf("slack %s: %s", e.Op, e.Code)
	case e.HTTP != 0:
		return fmt.Sprintf("slack %s: http %d: %s", e.Op, e.HTTP, e.Message)
	default:
		return fmt.Sprintf("slack %s: %s", e.Op, e.Message)
	}
}

func (e *Error) Unwrap() error { return e.Err }

// sentinelFor maps a Slack error code to its sentinel, nil when unmapped.
func sentinelFor(code string) error {
	switch code {
	case "invalid_auth", "not_authed", "account_inactive", "token_revoked", "token_expired":
		return ErrInvalidAuth
	case "channel_not_found":
		return ErrChannelNotFound
	case "not_in_channel", "is_archived":
		return ErrNotInChannel
	case "ratelimited", "rate_limited":
		return ErrRateLimited
	}
	return nil
}
