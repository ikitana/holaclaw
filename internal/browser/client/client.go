// Package client is the daemon's side of the sidecar socket (docs/browser.md): an HTTP/JSON client over the unix socket the sidecar listens on, plus
// the one WebSocket that carries the live view. It implements
// browser.Sidecar, so the executor never learns there is a socket at all.
//
// Two rules shape the code. A non-2xx answer is decoded into a *proto.Error
// and returned as is: the sidecar's codes are a closed set the executor acts
// on. And nothing that crosses this client is ever logged — request and
// response bodies carry page text, and the stream carries what a person types.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"holaclaw/internal/browser"
	"holaclaw/internal/browser/proto"
)

const (
	// DefaultTimeout bounds every call but Act, Download and Stream.
	DefaultTimeout = 30 * time.Second
	// actGrace is added to an action's own TimeoutMS: the sidecar bounds the
	// action, we only bound the transport around it.
	actGrace = 5 * time.Second
	// maxErrorBody caps the body read from a failed call before it is decoded.
	maxErrorBody = 8 << 10
	// frameBuffer is how many messages the reader may queue for the relay.
	// One in flight plus a little slack: the screencast is acked, not queued.
	frameBuffer = 4
	// maxStreamMessage caps one message read from the sidecar. Frames are
	// ~10 KiB at the measured quality; 8 MiB is a ceiling, not a target.
	maxStreamMessage = 8 << 20
)

// Options are the client's knobs; every one has a working default.
type Options struct {
	// Timeout overrides DefaultTimeout; tests shorten it.
	Timeout time.Duration
}

// Client talks to one sidecar. It is safe for concurrent use.
type Client struct {
	socket string
	hc     *http.Client
	// timeout bounds a single call; Act adds the action's own budget.
	timeout time.Duration
}

// New returns a client for the sidecar listening on socketPath. Nothing is
// dialled until the first call.
func New(socketPath string, opts Options) *Client {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	dialer := &net.Dialer{}
	return &Client{
		socket:  socketPath,
		timeout: timeout,
		// No Timeout on the http.Client: the deadline is the call's context,
		// which the WebSocket dial needs to be free of.
		hc: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socketPath)
			},
			DisableCompression: true,
		}},
	}
}

// Health reports what the sidecar found: binary, version, and the fixed
// problem sentence when no browser can run.
func (c *Client) Health(ctx context.Context) (proto.Health, error) {
	var out proto.Health
	err := c.call(ctx, http.MethodGet, proto.PathHealth, nil, nil, &out)
	return out, err
}

// Sessions lists the running browsers.
func (c *Client) Sessions(ctx context.Context) ([]proto.Session, error) {
	var out []proto.Session
	if err := c.call(ctx, http.MethodGet, proto.PathSessions, nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Open starts the agent's browser or returns the running one.
func (c *Client) Open(ctx context.Context, agent string, req proto.OpenRequest) (proto.Session, error) {
	var out proto.Session
	err := c.call(ctx, http.MethodPost, proto.PathOpen, map[string]string{"agent": agent}, req, &out)
	return out, err
}

// Close stops the agent's browser.
func (c *Client) Close(ctx context.Context, agent string) error {
	return c.call(ctx, http.MethodPost, proto.PathClose, map[string]string{"agent": agent}, nil, nil)
}

// Observe asks for a compact view of the active tab.
func (c *Client) Observe(ctx context.Context, agent string, req proto.ObserveRequest) (proto.Observation, error) {
	var out proto.Observation
	err := c.call(ctx, http.MethodPost, proto.PathObserve, map[string]string{"agent": agent}, req, &out)
	return out, err
}

// Act runs one action. Its budget is the action's own TimeoutMS plus a grace
// margin, so the transport never gives up before the sidecar does.
func (c *Client) Act(ctx context.Context, agent string, a proto.Action) (proto.ActResult, error) {
	budget := c.timeout
	if a.TimeoutMS > 0 {
		budget = time.Duration(a.TimeoutMS)*time.Millisecond + actGrace
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var out proto.ActResult
	err := c.do(ctx, http.MethodPost, proto.PathAct, map[string]string{"agent": agent}, a, &out)
	// A coded failure (no_session, held, …) is an answer the executor acts
	// on, not a transport error: browser.Sidecar promises OK=false + Code.
	var pe *proto.Error
	if errors.As(err, &pe) {
		return proto.ActResult{Code: pe.Code, Detail: pe.Detail}, nil
	}
	return out, err
}

// Screenshot is one JPEG of the active tab.
func (c *Client) Screenshot(ctx context.Context, agent string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	resp, err := c.send(ctx, http.MethodPost, proto.PathScreenshot, map[string]string{"agent": agent}, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := statusError(resp); err != nil {
		return nil, err
	}
	jpeg, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("browser client: screenshot: %w", err)
	}
	return jpeg, nil
}

// Download opens one quarantined file. The sidecar deletes it once the body
// has been read, so the caller must close the reader. The call is bounded by
// ctx alone: a large file may take longer than an ordinary call.
func (c *Client) Download(ctx context.Context, agent, id string) (io.ReadCloser, error) {
	resp, err := c.send(ctx, http.MethodGet, proto.PathDownload,
		map[string]string{"agent": agent, "id": id}, nil)
	if err != nil {
		return nil, err
	}
	if err := statusError(resp); err != nil {
		_ = resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

// The executor sees exactly this much of the sidecar.
var _ browser.Sidecar = (*Client)(nil)

// --- calls -------------------------------------------------------------------

// call runs a JSON request under the default timeout.
func (c *Client) call(ctx context.Context, method, path string, args map[string]string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.do(ctx, method, path, args, body, out)
}

// do runs a JSON request under ctx as it stands.
func (c *Client) do(ctx context.Context, method, path string, args map[string]string, body, out any) error {
	resp, err := c.send(ctx, method, path, args, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := statusError(resp); err != nil {
		return err
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		// The message names the call, never the body: a body may quote a page.
		return fmt.Errorf("browser client: %s: malformed response", path)
	}
	return nil
}

func (c *Client) send(ctx context.Context, method, path string, args map[string]string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("browser client: %s: encode request: %w", path, err)
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://sidecar"+expand(path, args), rdr)
	if err != nil {
		return nil, fmt.Errorf("browser client: %s: %w", path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("browser client: %s: %w", path, err)
	}
	return resp, nil
}

// statusError turns a non-2xx answer into the sidecar's own error. A body that
// is not a proto.Error still becomes one: the executor acts on codes.
func statusError(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	var pe proto.Error
	if err := json.Unmarshal(raw, &pe); err != nil || pe.Code == "" {
		return &proto.Error{Code: proto.ErrBrowser, Detail: http.StatusText(resp.StatusCode)}
	}
	return &pe
}

// expand fills a proto path template. Every value is one path segment.
func expand(path string, args map[string]string) string {
	for k, v := range args {
		path = strings.ReplaceAll(path, "{"+k+"}", url.PathEscape(v))
	}
	return path
}

// --- live view ---------------------------------------------------------------

// Frame is one message from the sidecar's live view: either a JPEG frame of
// the active tab or a control message (size, tabs). Exactly one field is set,
// and the order they arrive in is kept.
type Frame struct {
	JPEG []byte
	Meta *proto.StreamMessage
}

// Stream is one live view of an agent's browser. Frames arrive on the channel
// Frames returns, which is closed when the stream ends; Err then says why.
// Nothing read from or written to a stream is ever logged.
type Stream struct {
	conn   *websocket.Conn
	frames chan Frame
	cancel context.CancelFunc

	mu  sync.Mutex // one writer at a time on the connection
	err error
}

// Stream opens the live view of one agent. It is not bounded by a timeout:
// the stream lives as long as ctx, or until Close.
func (c *Client) Stream(ctx context.Context, agent string) (*Stream, error) {
	path := expand(proto.PathStream, map[string]string{"agent": agent})
	conn, resp, err := websocket.Dial(ctx, "ws://sidecar"+path, &websocket.DialOptions{
		HTTPClient:      c.hc,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			if serr := statusError(resp); serr != nil {
				return nil, serr
			}
		}
		return nil, fmt.Errorf("browser client: stream: %w", err)
	}
	conn.SetReadLimit(maxStreamMessage)
	runCtx, cancel := context.WithCancel(ctx)
	s := &Stream{conn: conn, frames: make(chan Frame, frameBuffer), cancel: cancel}
	go s.read(runCtx)
	return s, nil
}

// Frames is the stream of frames and control messages. It is closed when the
// stream ends.
func (s *Stream) Frames() <-chan Frame { return s.frames }

// Send writes one message to the sidecar. It is never logged.
func (s *Stream) Send(ctx context.Context, m proto.StreamMessage) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("browser client: stream: encode %s", m.Type)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.conn.Write(ctx, websocket.MessageText, raw); err != nil {
		return fmt.Errorf("browser client: stream: write %s", m.Type)
	}
	return nil
}

// Ack tells the sidecar the last frame has been dealt with. That ack is the
// screencast's backpressure: without it the sidecar stops sending.
func (s *Stream) Ack(ctx context.Context) error {
	return s.Send(ctx, proto.StreamMessage{Type: proto.StreamAck})
}

// Close ends the stream. It is safe to call more than once.
func (s *Stream) Close() error {
	s.cancel()
	_ = s.conn.Close(websocket.StatusNormalClosure, "")
	return s.conn.CloseNow()
}

// Err is why the stream ended, or nil when it ended cleanly.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// read pumps the connection until it ends. A malformed control message is
// dropped rather than reported: its content is not ours to quote.
func (s *Stream) read(ctx context.Context) {
	defer close(s.frames)
	for {
		typ, data, err := s.conn.Read(ctx)
		if err != nil {
			s.mu.Lock()
			if s.err == nil && !errors.Is(err, context.Canceled) {
				s.err = fmt.Errorf("browser client: stream: %w", err)
			}
			s.mu.Unlock()
			return
		}
		f := Frame{}
		switch typ {
		case websocket.MessageBinary:
			f.JPEG = data
		case websocket.MessageText:
			var m proto.StreamMessage
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			f.Meta = &m
		}
		select {
		case s.frames <- f:
		case <-ctx.Done():
			return
		}
	}
}
