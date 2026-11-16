package googlechat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxBody caps how much of a JSON response is read.
const maxBody = 4 << 20

// request is one authenticated Google API call.
type request struct {
	op      string
	method  string
	url     func(project string) string // built per call: the project comes from the key
	body    any                         // JSON-encoded when non-nil
	timeout time.Duration               // default callTimeout
}

// call runs r with the app token and decodes a JSON response into out. A 401
// drops the cached token and retries once, so an expired or revoked token
// costs one round trip, not an outage.
func (a *Adapter) call(ctx context.Context, r request, out any) error {
	for attempt := 0; ; attempt++ {
		resp, c, cancel, err := a.send(ctx, r)
		if err != nil {
			return err
		}
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
		cancel()
		scrub := a.scrubWith(c.token)
		if rerr != nil {
			return transportError(ctx, r.op, c.project, rerr, scrub)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			a.tokens.drop(c.token)
			continue
		}
		if resp.StatusCode/100 != 2 {
			e := apiError(r.op, resp.StatusCode, raw, scrub)
			e.Project = c.project
			if resp.StatusCode == http.StatusTooManyRequests {
				e.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
			}
			return e
		}
		if out != nil && len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, out); err != nil {
				return &Error{Op: r.op, Kind: KindServer, Status: resp.StatusCode, Project: c.project, Message: "malformed response"}
			}
		}
		return nil
	}
}

// send performs r and hands back the open response; the caller closes it and
// then calls cancel.
func (a *Adapter) send(ctx context.Context, r request) (*http.Response, creds, context.CancelFunc, error) {
	c, err := a.credentials(ctx)
	if err != nil {
		return nil, c, nil, err
	}
	var rdr io.Reader
	if r.body != nil {
		buf, err := json.Marshal(r.body)
		if err != nil {
			return nil, c, nil, &Error{Op: r.op, Kind: KindBadRequest, Project: c.project, Message: "encode request"}
		}
		rdr = bytes.NewReader(buf)
	}
	timeout := r.timeout
	if timeout <= 0 {
		timeout = callTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(rctx, r.method, r.url(c.project), rdr)
	if err != nil {
		cancel()
		return nil, c, nil, &Error{Op: r.op, Kind: KindBadRequest, Project: c.project, Message: a.scrubWith(c.token)(err.Error())}
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		cancel()
		return nil, c, nil, transportError(ctx, r.op, c.project, err, a.scrubWith(c.token))
	}
	return resp, c, cancel, nil
}

// transportError turns a transport failure into a scrubbed *Error. The message
// is unwrapped out of *url.Error first, which quotes the request URL. The raw
// error is never kept: a transport may quote the assertion or the bearer, and
// a kept cause would be reachable through Unwrap. Only a context error, which
// carries no request data, stays in the chain for errors.Is.
func transportError(ctx context.Context, op, project string, err error, scrub func(string) string) error {
	msg := err
	var ue *url.Error
	if errors.As(err, &ue) {
		msg = ue.Err
	}
	var cause error
	switch {
	case ctx.Err() != nil:
		cause = ctx.Err()
	case errors.Is(err, context.DeadlineExceeded):
		cause = context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		cause = context.Canceled
	}
	if cause != nil {
		msg = cause
	}
	return &Error{Op: op, Kind: KindNetwork, Project: project, Message: scrub(msg.Error()), cause: cause}
}

// parseRetryAfter reads the header in its seconds form, then as an HTTP date.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// subscriptionPath is the one subscription holaclaw pulls from.
func subscriptionPath(project string) string {
	return "projects/" + url.PathEscape(project) + "/subscriptions/" + SubscriptionID
}
