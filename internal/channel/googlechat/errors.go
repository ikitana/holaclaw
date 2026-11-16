package googlechat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Kind classifies a failure so the daemon and `auth set` can react without
// parsing strings.
type Kind string

const (
	KindBadKey        Kind = "bad_key"        // not a service-account key, unparseable, or bad PEM
	KindAuth          Kind = "auth"           // the token endpoint refused the key, or 401
	KindForbidden     Kind = "forbidden"      // 403: the service account lacks a permission
	KindNotFound      Kind = "not_found"      // 404
	KindNotConfigured Kind = "not_configured" // the Chat app is not configured (§4.5)
	KindRateLimited   Kind = "rate_limited"   // 429, carries RetryAfter
	KindConflict      Kind = "conflict"       // 409 ALREADY_EXISTS
	KindBadRequest    Kind = "bad_request"    // 400 and any other 4xx
	KindNetwork       Kind = "network"        // transport failure, never a status code
	KindServer        Kind = "server"         // 5xx or a malformed response
)

// Sentinels for errors.Is. An *Error unwraps to the sentinel of its kind.
var (
	ErrBadKey        = errors.New("googlechat: not a usable service-account key")
	ErrAuth          = errors.New("googlechat: key refused")
	ErrForbidden     = errors.New("googlechat: forbidden")
	ErrNotFound      = errors.New("googlechat: not found")
	ErrNotConfigured = errors.New("googlechat: chat app not configured")
	ErrRateLimited   = errors.New("googlechat: rate limited")
	ErrConflict      = errors.New("googlechat: already exists")
	ErrBadRequest    = errors.New("googlechat: bad request")
	ErrNetwork       = errors.New("googlechat: network error")
	ErrServer        = errors.New("googlechat: server error")
)

var sentinels = map[Kind]error{
	KindBadKey: ErrBadKey, KindAuth: ErrAuth, KindForbidden: ErrForbidden,
	KindNotFound: ErrNotFound, KindNotConfigured: ErrNotConfigured,
	KindRateLimited: ErrRateLimited, KindConflict: ErrConflict,
	KindBadRequest: ErrBadRequest, KindNetwork: ErrNetwork, KindServer: ErrServer,
}

// Error is a Google API failure. Message is Google's own text, already
// scrubbed of the key, the JWT assertion and any access token.
type Error struct {
	Kind       Kind
	Op         string // "token", "pull", OpSubscriptionCheck, "messages.create", ...
	Status     int    // HTTP status, 0 when the request never completed
	Project    string // the key's project, when known
	Message    string
	RetryAfter time.Duration // from Retry-After on 429
	cause      error         // a bare context error only, never a raw transport error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("googlechat ")
	b.WriteString(e.Op)
	b.WriteString(": ")
	b.WriteString(string(e.Kind))
	if e.Status != 0 {
		fmt.Fprintf(&b, " (%d)", e.Status)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

// Unwrap yields the sentinel for the kind so errors.Is works, and keeps the
// context error of a transport failure reachable.
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

// kindOf maps an HTTP status onto a Kind.
func kindOf(status int) Kind {
	switch {
	case status == 401:
		return KindAuth
	case status == 403:
		return KindForbidden
	case status == 404:
		return KindNotFound
	case status == 409:
		return KindConflict
	case status == 429:
		return KindRateLimited
	case status >= 500:
		return KindServer
	default:
		return KindBadRequest
	}
}

// googleError is the error body every Google REST API returns.
type googleError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

// apiError builds an *Error from a non-2xx response body. The body is never
// quoted whole: only Google's message field, scrubbed.
func apiError(op string, status int, raw []byte, scrub func(string) string) *Error {
	e := &Error{Op: op, Kind: kindOf(status), Status: status}
	var g googleError
	if json.Unmarshal(raw, &g) == nil && g.Error.Message != "" {
		// Scrub before truncating: a cut secret no longer matches.
		e.Message = truncate(scrub(g.Error.Message), 300)
		if g.Error.Status != "" {
			e.Message = g.Error.Status + ": " + e.Message
		}
	}
	return e
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

// describe renders an error for a status detail or a log line. Errors out of
// this package are already scrubbed.
func describe(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// kindOfErr reports the Kind of err, "" when it is not an *Error.
func kindOfErr(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return ""
}
