package mcp

import "errors"

var (
	// ErrUnauthorized matches every *UnauthorizedError.
	ErrUnauthorized = errors.New("mcp: unauthorized")
	// ErrSessionLost means the server dropped the session again right after
	// a fresh initialize; the call was not retried a second time.
	ErrSessionLost = errors.New("mcp: session lost")
	// ErrClosed is returned by every method after Close.
	ErrClosed = errors.New("mcp: client closed")
	// ErrTooLarge means a JSON or error response body exceeded its cap. An
	// oversized SSE event is the SDK's error instead.
	ErrTooLarge = errors.New("mcp: response too large")
	// ErrListTooLarge means a tools listing exceeded MaxListBytes.
	ErrListTooLarge = errors.New("mcp: tools/list: listing too large")
)

// UnauthorizedError is a 401 from the server. WWWAuthenticate is the
// response header as sent (capped), for the manager's discovery; it is
// server-controlled text and is never part of Error().
type UnauthorizedError struct {
	WWWAuthenticate string
}

func (e *UnauthorizedError) Error() string { return ErrUnauthorized.Error() }

func (e *UnauthorizedError) Is(target error) bool { return target == ErrUnauthorized }

// errSessionGone is a 404 for a known session id: re-initialize and retry.
var errSessionGone = errors.New("mcp: session not found")
