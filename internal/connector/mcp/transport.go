package mcp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// MaxResponseBytes bounds one JSON response body or one SSE event.
	MaxResponseBytes = 4 << 20
	// maxErrorBody bounds the body of a non-2xx response.
	maxErrorBody = 64 << 10
	// maxHeaderValue caps the WWW-Authenticate value kept from a 401.
	maxHeaderValue = 4096
	// maxRedirects matches net/http's default.
	maxRedirects = 10
	// tokenTimeout bounds the token callback when the request has no
	// deadline (the SDK's standalone GET and DELETE).
	tokenTimeout = 10 * time.Second
)

// callState records what the transport saw for one logical request. It
// travels in the request context because the SDK flattens error chains.
type callState struct {
	mu sync.Mutex
	s  seen
}

// seen is a snapshot of a callState.
type seen struct {
	sent         bool // a POST reached the network
	unauthorized bool
	wwwAuth      string
	gone         bool
	tooLarge     bool
	tokenErr     error
}

type stateKey struct{}

func withState(ctx context.Context, st *callState) context.Context {
	return context.WithValue(ctx, stateKey{}, st)
}

func stateFrom(ctx context.Context) *callState {
	st, _ := ctx.Value(stateKey{}).(*callState)
	return st
}

func (st *callState) set(f func(*seen)) {
	if st == nil {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	f(&st.s)
}

func (st *callState) get() seen {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s
}

// transport adds the bearer token, turns a 401 or a session 404 on a POST
// into an error (so the SDK keeps the connection) and bounds every body
// except a 2xx SSE stream, whose events the SDK bounds. An oversized event
// fails that call with the SDK's error and ends the connection.
type transport struct {
	base   http.RoundTripper
	origin *url.URL
	token  func(context.Context) (string, error)
	// noDelete refuses the session DELETE (Close with calls in flight).
	noDelete atomic.Bool
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodDelete && t.noDelete.Load() {
		return nil, errors.New("mcp: session delete skipped")
	}
	ctx := req.Context()
	st := stateFrom(ctx)
	post := req.Method == http.MethodPost
	if t.token != nil && sameOrigin(req.URL, t.origin) {
		tok, err := t.callToken(ctx)
		if err != nil {
			if req.Body != nil {
				_ = req.Body.Close()
			}
			if post {
				st.set(func(s *seen) { s.tokenErr = err })
			}
			return nil, errors.New("mcp: token unavailable")
		}
		if tok != "" {
			req = req.Clone(ctx)
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	if post {
		st.set(func(s *seen) { s.sent = true })
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if post {
		switch {
		case resp.StatusCode == http.StatusUnauthorized:
			www := resp.Header.Get("WWW-Authenticate")
			if len(www) > maxHeaderValue {
				www = www[:maxHeaderValue]
			}
			discard(resp)
			st.set(func(s *seen) { s.unauthorized, s.wwwAuth = true, www })
			return nil, &UnauthorizedError{WWWAuthenticate: www}
		case resp.StatusCode == http.StatusNotFound && req.Header.Get("Mcp-Session-Id") != "":
			discard(resp)
			st.set(func(s *seen) { s.gone = true })
			return nil, errSessionGone
		}
	}
	// GET and DELETE run under the initialize context; their errors are
	// not that call's.
	bst := st
	if !post {
		bst = nil
	}
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch {
	case !ok:
		resp.Body = &limitedBody{rc: resp.Body, n: maxErrorBody, st: bst}
	case mt == "text/event-stream":
		// The SDK parses it and bounds each event (MaxEventSize).
	default:
		resp.Body = &limitedBody{rc: resp.Body, n: MaxResponseBytes, st: bst}
	}
	return resp, nil
}

func (t *transport) callToken(ctx context.Context) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, tokenTimeout)
		defer cancel()
	}
	return t.token(ctx)
}

func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host)
}

// skipVerify is rt, or the default transport when rt is not an
// *http.Transport, with certificate verification off.
func skipVerify(rt http.RoundTripper) http.RoundTripper {
	t, ok := rt.(*http.Transport)
	if !ok {
		t = http.DefaultTransport.(*http.Transport)
	}
	t = t.Clone()
	if t.TLSClientConfig == nil {
		t.TLSClientConfig = &tls.Config{}
	}
	t.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // allow_insecure_server, the operator's choice
	return t
}

// isLoopback reports whether host is localhost or a loopback IP.
func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func discard(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
}

// limitedBody fails with ErrTooLarge once more than n bytes are read.
type limitedBody struct {
	rc io.ReadCloser
	n  int64
	st *callState
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.n <= 0 {
		var one [1]byte
		if k, _ := b.rc.Read(one[:]); k > 0 {
			b.st.set(func(s *seen) { s.tooLarge = true })
			return 0, ErrTooLarge
		}
		return b.rc.Read(p)
	}
	if int64(len(p)) > b.n {
		p = p[:b.n]
	}
	k, err := b.rc.Read(p)
	b.n -= int64(k)
	return k, err
}

func (b *limitedBody) Close() error { return b.rc.Close() }

// newHTTPClient copies base (or a zero client), wraps its transport and
// follows redirects only within the server's origin over https. Jar is
// cleared so server cookies never ride along, and Timeout because it would
// cut the long-lived SSE stream; CallTimeout bounds calls instead.
func newHTTPClient(base *http.Client, origin *url.URL, token func(context.Context) (string, error), insecure bool) *http.Client {
	var hc http.Client
	if base != nil {
		hc = *base
	}
	hc.Jar, hc.Timeout = nil, 0
	rt := hc.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	if insecure {
		rt = skipVerify(rt)
	}
	hc.Transport = &transport{base: rt, origin: origin, token: token}
	next := hc.CheckRedirect
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !sameOrigin(req.URL, origin) || (req.URL.Scheme != "https" && !insecure) {
			return fmt.Errorf("mcp: redirect off the server origin refused")
		}
		if next != nil {
			return next(req, via)
		}
		if len(via) >= maxRedirects {
			return errors.New("mcp: too many redirects")
		}
		return nil
	}
	return &hc
}
