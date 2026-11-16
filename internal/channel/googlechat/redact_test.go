package googlechat

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"holaclaw/internal/channel"
)

// The key, the signed assertion and every access token stay out of errors,
// status details and log lines, even when Google (or a transport) echoes them.

// assertClean fails when any credential fragment appears in texts.
func assertClean(t *testing.T, texts ...string) {
	t.Helper()
	var frags []string
	for _, line := range strings.Split(pemPKCS8(testRSA), "\n") {
		if len(line) >= 16 && !strings.HasPrefix(line, "-----") {
			frags = append(frags, line)
		}
	}
	frags = append(frags, testKeyID, "PRIVATE KEY", "ya29.", "eyJ")
	for _, s := range texts {
		for _, f := range frags {
			if strings.Contains(s, f) {
				t.Fatalf("credential fragment %q leaked into:\n%s", f, s)
			}
		}
	}
}

// assertCleanErr is assertClean over every error reachable from err through
// Unwrap, each formatted with %v, %+v and %#v.
func assertCleanErr(t *testing.T, err error) {
	t.Helper()
	queue := []error{err}
	for len(queue) > 0 {
		e := queue[0]
		queue = queue[1:]
		if e == nil {
			continue
		}
		assertClean(t, fmt.Sprintf("%v", e), fmt.Sprintf("%+v", e), fmt.Sprintf("%#v", e))
		switch u := e.(type) {
		case interface{ Unwrap() error }:
			queue = append(queue, u.Unwrap())
		case interface{ Unwrap() []error }:
			queue = append(queue, u.Unwrap()...)
		}
	}
}

func TestRedactBadKey(t *testing.T) {
	// A key of the wrong type, and one whose PEM is cut short: neither error
	// may quote the input.
	for _, key := range []string{
		keyJSON(func(m map[string]any) { m["type"] = "user" }),
		keyJSON(func(m map[string]any) { m["private_key"] = pemPKCS8(testRSA)[:400] }),
		"{" + pemPKCS8(testRSA), // not JSON at all
	} {
		_, err := Verify(context.Background(), VerifyOptions{Key: key, BaseURL: "http://127.0.0.1:1"})
		if !errors.Is(err, ErrBadKey) {
			t.Fatalf("err = %v", err)
		}
		assertClean(t, err.Error())
	}
}

func TestRedactTokenEndpointEcho(t *testing.T) {
	f := newFake(t)
	f.handle("token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		// A hostile or buggy endpoint echoing what it was sent, and the key.
		writeJSON(w, 400, map[string]any{
			"error":             "invalid_grant",
			"error_description": "bad assertion " + r.Form.Get("assertion") + " for key " + pemPKCS8(testRSA) + " id " + testKeyID,
		})
	})
	a, logs := newAdapter(t, f, nil)
	_, err := a.credentials(context.Background())
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("the useful part was lost: %v", err)
	}

	r := run(t, a, nil)
	st := r.waitStatus(t, channel.StateLoggedOut)
	_ = r.stop()
	_, verr := Verify(context.Background(), VerifyOptions{Key: keyJSON(nil), BaseURL: f.srv.URL})
	assertClean(t, err.Error(), st.detail, logs.String(), verr.Error())
}

func TestRedactAPIEchoOfAccessToken(t *testing.T) {
	f := newFake(t)
	f.handle("pull", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 403, googleErr(403, "caller "+r.Header.Get("Authorization")+" lacks pubsub.subscriptions.consume"))
	})
	a, logs := newAdapter(t, f, nil)
	r := run(t, a, nil)
	st := r.waitStatus(t, channel.StateLoggedOut)
	_ = r.stop()
	if !strings.Contains(st.detail, "pubsub.subscriptions.consume") {
		t.Fatalf("detail lost its useful part: %q", st.detail)
	}
	assertClean(t, st.detail, logs.String())
}

// leakyTransport fails with an error that quotes the request's credentials.
type leakyTransport struct{}

func (leakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := ""
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}
	if form, err := url.ParseQuery(body); err == nil && form.Get("assertion") != "" {
		body = form.Get("assertion")
	}
	return nil, fmt.Errorf("connection reset while sending %q with %q", body, r.Header.Get("Authorization"))
}

func TestRedactTransportErrors(t *testing.T) {
	f := newFake(t)
	// The token request fails on the wire, quoting the assertion.
	a, logs := newAdapter(t, f, func(o *Options) { o.HTTPClient = &http.Client{Transport: leakyTransport{}} })
	r := run(t, a, nil)
	st := r.waitStatus(t, channel.StateReconnecting)
	_ = r.stop()
	if !strings.Contains(st.detail, "network") {
		t.Fatalf("detail = %q", st.detail)
	}
	assertClean(t, st.detail, logs.String())

	// With a token cached, an API call fails on the wire quoting the bearer.
	a2, logs2 := newAdapter(t, f, nil)
	if _, err := a2.credentials(context.Background()); err != nil {
		t.Fatal(err)
	}
	a2.hc = &http.Client{Transport: leakyTransport{}}
	_, err := a2.Send(context.Background(), out("spaces/S1", "", "hi"))
	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("err = %v", err)
	}
	assertClean(t, err.Error(), logs2.String())
	assertCleanErr(t, err)

	// The token request itself fails on the wire: neither the message nor
	// anything behind Unwrap may quote the assertion.
	a3, _ := newAdapter(t, f, func(o *Options) { o.HTTPClient = &http.Client{Transport: leakyTransport{}} })
	_, err = a3.credentials(context.Background())
	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("err = %v", err)
	}
	assertCleanErr(t, err)
}

// A transport error keeps only a bare context error in its chain, so
// errors.Is still sees a deadline.
func TestTransportErrorKeepsOnlyContextCause(t *testing.T) {
	scrub := func(s string) string { return s }
	leak := errors.New("reset quoting Bearer ya29.secret")
	err := transportError(context.Background(), "pull", "p", &url.Error{Op: "Post", URL: "u", Err: leak}, scrub)
	if errors.Is(err, leak) {
		t.Fatal("the raw transport error is reachable through Unwrap")
	}
	wrapped := fmt.Errorf("dial: %w (Bearer ya29.secret)", context.DeadlineExceeded)
	err = transportError(context.Background(), "pull", "p", &url.Error{Op: "Post", URL: "u", Err: wrapped}, scrub)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, wrapped) {
		t.Fatalf("err = %v: want the bare deadline only", err)
	}
	assertCleanErr(t, err)
}

// A failing request is scrubbed with the token it sent, even when the cache
// has already moved on to another one.
func TestScrubWithUsedToken(t *testing.T) {
	a, _ := newAdapter(t, newFake(t), nil)
	used := "opaque-token-not-matching-any-pattern"
	if got := a.scrubWith(used)("sent Bearer " + used); strings.Contains(got, used) {
		t.Fatalf("used token not scrubbed: %q", got)
	}
}
