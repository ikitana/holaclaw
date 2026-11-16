package slack

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

// maxBody caps how much of a Web API response is read.
const maxBody = 4 << 20

// maxRetries is how often a 429 is retried before giving up.
const maxRetries = 3

// apiEnvelope is the part of every Web API response we always read.
type apiEnvelope struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Warn  string `json:"warning"`
}

// callJSON posts a JSON body to a Web API method and decodes the result.
func (a *Adapter) callJSON(ctx context.Context, op, token string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return &Error{Op: op, Message: "encoding request: " + a.redact(err.Error()), Err: err}
	}
	return a.do(ctx, op, token, "application/json; charset=utf-8", b, out)
}

// callForm posts form values to a Web API method (methods that take no JSON).
func (a *Adapter) callForm(ctx context.Context, op, token string, form url.Values, out any) error {
	return a.do(ctx, op, token, "application/x-www-form-urlencoded", []byte(form.Encode()), out)
}

func (a *Adapter) do(ctx context.Context, op, token, contentType string, body []byte, out any) error {
	endpoint := a.opt.APIBaseURL + "/" + op
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return &Error{Op: op, Message: a.redact(err.Error()), Err: err}
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := a.hc.Do(req)
		if err != nil {
			return &Error{Op: op, Message: a.redact(err.Error()), Err: err}
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()

		if resp.StatusCode == http.StatusTooManyRequests {
			wait := parseRetryAfter(resp.Header.Get("Retry-After"))
			if attempt >= maxRetries {
				return &Error{Op: op, Code: "ratelimited", HTTP: resp.StatusCode, RetryAfter: wait,
					Message: "rate limited", Err: ErrRateLimited}
			}
			if err := sleepCtx(ctx, wait); err != nil {
				return &Error{Op: op, Code: "ratelimited", HTTP: resp.StatusCode, RetryAfter: wait,
					Message: a.redact(err.Error()), Err: err}
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return &Error{Op: op, HTTP: resp.StatusCode, Message: a.redact(snippet(raw))}
		}
		if readErr != nil {
			return &Error{Op: op, HTTP: resp.StatusCode, Message: a.redact(readErr.Error()), Err: readErr}
		}

		var env apiEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return &Error{Op: op, HTTP: resp.StatusCode, Message: "decoding response: " + a.redact(err.Error()), Err: err}
		}
		if !env.OK {
			return &Error{Op: op, Code: env.Error, HTTP: resp.StatusCode, Err: sentinelFor(env.Error)}
		}
		if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				return &Error{Op: op, HTTP: resp.StatusCode, Message: "decoding response: " + a.redact(err.Error()), Err: err}
			}
		}
		return nil
	}
}

// parseRetryAfter reads the header in its seconds form; Slack always uses it.
func parseRetryAfter(v string) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	return time.Second
}

// sleepCtx waits for d unless ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 256 {
		s = s[:256] + "…"
	}
	return s
}

// authTestResult is the identity of the bot behind the token.
type authTestResult struct {
	UserID string `json:"user_id"`
	TeamID string `json:"team_id"`
	BotID  string `json:"bot_id"`
}

func (a *Adapter) authTest(ctx context.Context) (authTestResult, error) {
	var r authTestResult
	err := a.callForm(ctx, "auth.test", a.botToken(), url.Values{}, &r)
	return r, err
}

// connectionsOpen asks for a fresh Socket Mode wss URL (app token).
func (a *Adapter) connectionsOpen(ctx context.Context) (string, error) {
	var r struct {
		URL string `json:"url"`
	}
	if err := a.callForm(ctx, "apps.connections.open", a.appToken(), url.Values{}, &r); err != nil {
		return "", err
	}
	if r.URL == "" {
		return "", &Error{Op: "apps.connections.open", Message: "no url in response"}
	}
	return r.URL, nil
}

// displayName resolves a user id through users.info, cached for an hour. A
// lookup failure degrades to the user id, never to an error.
func (a *Adapter) displayName(ctx context.Context, userID string) string {
	if userID == "" {
		return ""
	}
	if n, ok := a.users.get(userID); ok {
		return n
	}
	var r struct {
		User struct {
			Name     string `json:"name"`
			RealName string `json:"real_name"`
			Profile  struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := a.callForm(ctx, "users.info", a.botToken(), url.Values{"user": {userID}}, &r); err != nil {
		a.log.Debug("users.info failed", "user", userID, "err", a.redact(err.Error()))
		return userID
	}
	name := firstNonEmpty(r.User.Profile.DisplayName, r.User.Profile.RealName, r.User.RealName, r.User.Name, userID)
	a.users.put(userID, name)
	return name
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// isAuthError reports whether err is an unrecoverable credential failure.
func isAuthError(err error) bool { return errors.Is(err, ErrInvalidAuth) }
