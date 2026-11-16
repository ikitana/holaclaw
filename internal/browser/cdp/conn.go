// Package cdp is holaclaw's own Chrome DevTools Protocol client, spoken over
// --remote-debugging-pipe: Chromium reads commands from fd 3 and writes
// replies and events to fd 4, every message a JSON object terminated by a NUL
// byte. No TCP port to find, no dependency (docs/browser.md).
//
// The package does not start a browser; internal/browser/chromium does, and
// hands the two pipe ends to New.
//
// THE RULE of this layer, enforced in code by Call and not by convention:
// Runtime is never enabled and nothing is evaluated in a page's main world —
// both are what bot detectors look for. Pages are read through DOM, DOMSnapshot
// and Accessibility; when JavaScript is unavoidable it runs through
// EvalIsolated, in an isolated world. See deny.go.
package cdp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
)

// Defaults of Options.
const (
	// DefaultMaxMessageBytes caps one framed message. A full-page screenshot
	// arrives base64-encoded inside a single reply, so the cap is generous.
	DefaultMaxMessageBytes = 64 << 20
	// DefaultEventQueue is the per-subscription queue depth; see Subscribe.
	DefaultEventQueue = 1024
)

// Errors of the connection itself. Protocol errors are *Error.
var (
	// ErrClosed is returned by Call, and by Err, after Close.
	ErrClosed = errors.New("cdp: connection closed")
	// ErrTooLarge ends the connection: a message exceeded MaxMessageBytes and
	// the stream can no longer be framed.
	ErrTooLarge = errors.New("cdp: message too large")
)

// Error is a protocol error: the browser answered a command with {"error":…}.
type Error struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if e.Data != "" {
		return fmt.Sprintf("cdp: %s (%d): %s", e.Message, e.Code, e.Data)
	}
	return fmt.Sprintf("cdp: %s (%d)", e.Message, e.Code)
}

// Options tune a Conn. The zero value is fine.
type Options struct {
	// Logger receives malformed frames and dropped events; nil discards.
	Logger *slog.Logger
	// MaxMessageBytes caps one message; 0 means DefaultMaxMessageBytes.
	MaxMessageBytes int
	// EventQueue is the per-subscription queue depth; 0 means
	// DefaultEventQueue.
	EventQueue int
}

// Conn is one connection to a browser. It is safe for concurrent use: many
// goroutines may Call at once, ids are handed out monotonically and writes are
// serialised.
type Conn struct {
	r   io.Reader
	w   io.Writer
	log *slog.Logger
	max int
	qn  int

	wmu sync.Mutex // serialises writes
	id  atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan *message
	subs    map[int64]*subscription
	subID   int64
	closed  bool

	done      chan struct{}
	closeOnce sync.Once
	errOnce   sync.Once
	err       atomic.Value // error
}

// message is one frame in either direction. A reply has ID set; an event has
// Method set and no ID.
type message struct {
	ID        *int64          `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *Error          `json:"error,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

// command is what we send. Params is omitted when nil, as the protocol expects.
type command struct {
	ID        int64           `json:"id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
}

// New wraps the two pipe ends: r is Chromium's fd 4 (replies and events), w is
// its fd 3 (commands). It starts the read loop; the connection lives until
// Close or until the browser dies.
func New(r io.Reader, w io.Writer, opts Options) *Conn {
	c := &Conn{
		r:       r,
		w:       w,
		log:     opts.Logger,
		max:     opts.MaxMessageBytes,
		qn:      opts.EventQueue,
		pending: make(map[int64]chan *message),
		subs:    make(map[int64]*subscription),
		done:    make(chan struct{}),
	}
	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}
	if c.max <= 0 {
		c.max = DefaultMaxMessageBytes
	}
	if c.qn <= 0 {
		c.qn = DefaultEventQueue
	}
	go c.read()
	return c
}

// Done is closed when the read loop ends: Close was called, or the browser
// died. Err says which.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is nil while the connection is up, then ErrClosed after Close or the
// error that ended the read loop.
func (c *Conn) Err() error {
	if v := c.err.Load(); v != nil {
		return v.(error)
	}
	return nil
}

// Close ends the connection. Pending calls fail, subscriptions stop, and the
// pipe ends are closed when they are io.Closers so the read loop unblocks.
// It is idempotent.
func (c *Conn) Close() error {
	c.shutdown(ErrClosed)
	if err := c.Err(); err != nil && !errors.Is(err, ErrClosed) {
		return err
	}
	return nil
}

func (c *Conn) shutdown(err error) {
	c.errOnce.Do(func() {
		if err == nil {
			err = ErrClosed
		}
		c.err.Store(err)
	})
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		subs := c.subs
		c.subs = nil
		c.pending = nil
		c.mu.Unlock()

		for _, s := range subs {
			s.stop()
		}
		close(c.done)

		if cl, ok := c.r.(io.Closer); ok {
			_ = cl.Close()
		}
		if cl, ok := c.w.(io.Closer); ok {
			_ = cl.Close()
		}
	})
}

// read is the single reader of the pipe. It never runs a handler inline: a
// handler that called back into Call would deadlock the connection.
func (c *Conn) read() {
	br := bufio.NewReaderSize(c.r, 64<<10)
	var buf bytes.Buffer
	for {
		chunk, err := br.ReadSlice(0)
		if errors.Is(err, bufio.ErrBufferFull) {
			if buf.Len()+len(chunk) > c.max {
				c.shutdown(fmt.Errorf("%w: over %d bytes", ErrTooLarge, c.max))
				return
			}
			buf.Write(chunk)
			continue
		}
		if err != nil {
			// Trailing bytes without a terminator are an unfinished message.
			c.shutdown(fmt.Errorf("cdp: read: %w", err))
			return
		}
		frame := chunk[:len(chunk)-1] // drop the NUL
		if buf.Len() > 0 {
			if buf.Len()+len(frame) > c.max {
				c.shutdown(fmt.Errorf("%w: over %d bytes", ErrTooLarge, c.max))
				return
			}
			buf.Write(frame)
			frame = buf.Bytes()
		} else if len(frame) > c.max {
			c.shutdown(fmt.Errorf("%w: over %d bytes", ErrTooLarge, c.max))
			return
		}
		c.dispatch(frame)
		buf.Reset()
	}
}

// dispatch routes one frame. Malformed JSON is logged and skipped: a hostile
// page must not be able to end the connection.
func (c *Conn) dispatch(frame []byte) {
	if len(bytes.TrimSpace(frame)) == 0 {
		return
	}
	var m message
	if err := json.Unmarshal(frame, &m); err != nil {
		c.log.Warn("cdp: malformed frame", "err", err, "bytes", len(frame))
		return
	}
	if m.ID != nil {
		c.mu.Lock()
		ch := c.pending[*m.ID]
		delete(c.pending, *m.ID)
		c.mu.Unlock()
		if ch == nil {
			// Cancelled or unknown: nothing to deliver to.
			return
		}
		ch <- &m
		return
	}
	if m.Method == "" {
		c.log.Warn("cdp: frame with neither id nor method", "bytes", len(frame))
		return
	}
	c.mu.Lock()
	var match []*subscription
	for _, s := range c.subs {
		if s.session == m.SessionID && (s.method == "" || s.method == m.Method) {
			match = append(match, s)
		}
	}
	c.mu.Unlock()
	for _, s := range match {
		s.deliver(m.Params)
	}
}

func (c *Conn) write(cmd *command) error {
	b, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("cdp: marshal %s: %w", cmd.Method, err)
	}
	b = append(b, 0)
	c.wmu.Lock()
	_, err = c.w.Write(b)
	c.wmu.Unlock()
	if err != nil {
		err = fmt.Errorf("cdp: write %s: %w", cmd.Method, err)
		c.shutdown(err)
		return err
	}
	return nil
}

// Call sends one command and waits for its reply. sessionID is "" for
// browser-level commands and a flat session id otherwise; result may be nil
// when the reply is not needed. A protocol error comes back as *Error; a
// method the stealth rule forbids is refused here and nothing is sent.
func (c *Conn) Call(ctx context.Context, sessionID, method string, params, result any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return fmt.Errorf("cdp: %s params: %w", method, err)
	}
	if err := checkDenied(method, raw); err != nil {
		return err
	}
	return c.call(ctx, sessionID, method, raw, result)
}

func (c *Conn) call(ctx context.Context, sessionID, method string, raw json.RawMessage, result any) error {
	id := c.id.Add(1)
	ch := make(chan *message, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return c.Err()
	}
	c.pending[id] = ch
	c.mu.Unlock()

	forget := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}

	if err := c.write(&command{ID: id, Method: method, Params: raw, SessionID: sessionID}); err != nil {
		forget()
		return err
	}

	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if result == nil || len(m.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(m.Result, result); err != nil {
			return fmt.Errorf("cdp: %s result: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		forget()
		return ctx.Err()
	case <-c.done:
		forget()
		return c.Err()
	}
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		return raw, nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if string(b) == "null" {
		return nil, nil
	}
	return b, nil
}

// subscription is one Subscribe registration with its own ordered queue and
// worker goroutine.
type subscription struct {
	c       *Conn
	id      int64
	session string
	method  string
	fn      func(json.RawMessage)

	mu      sync.Mutex
	q       []json.RawMessage
	dropped int
	stopped bool

	wake chan struct{}
}

// Subscribe registers fn for events. sessionID "" is the browser level;
// method "" matches every event of that session. The returned function cancels
// the subscription; it is idempotent and may be called from the handler.
//
// Handlers never run on the read loop: each subscription has a goroutine and a
// FIFO queue, so events arrive in order and a slow — or re-entrant — handler
// cannot stall the connection. The queue is bounded (Options.EventQueue,
// default 1024); on overflow the OLDEST queued event is dropped and a warning
// is logged with a running count. Blocking was rejected because a handler is
// allowed to Call back into the same Conn, which would deadlock the read loop;
// a bounded queue degrades instead, which is what a high-rate stream such as
// Page.screencastFrame wants anyway.
func (c *Conn) Subscribe(sessionID, method string, fn func(params json.RawMessage)) (cancel func()) {
	s := &subscription{c: c, session: sessionID, method: method, fn: fn, wake: make(chan struct{}, 1)}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return func() {}
	}
	c.subID++
	s.id = c.subID
	c.subs[s.id] = s
	c.mu.Unlock()

	go s.run()

	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			delete(c.subs, s.id)
			c.mu.Unlock()
			s.stop()
		})
	}
}

func (s *subscription) deliver(params json.RawMessage) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	if len(s.q) >= s.c.qn {
		s.q = s.q[1:]
		s.dropped++
		n := s.dropped
		s.mu.Unlock()
		s.c.log.Warn("cdp: event queue full, dropped oldest",
			"method", s.method, "session", s.session, "dropped", n)
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return
		}
	}
	s.q = append(s.q, params)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *subscription) stop() {
	s.mu.Lock()
	s.stopped = true
	s.q = nil
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *subscription) run() {
	for range s.wake {
		for {
			s.mu.Lock()
			if s.stopped || len(s.q) == 0 {
				done := s.stopped
				s.mu.Unlock()
				if done {
					return
				}
				break
			}
			p := s.q[0]
			s.q = s.q[1:]
			s.mu.Unlock()
			s.fn(p)
		}
	}
}

// Session is a Conn bound to one flat session id — the convenience form used
// by the typed helpers. The empty id is the browser level.
type Session struct {
	c  *Conn
	id string
}

// Session returns the handle for a flat session id ("" = browser level).
func (c *Conn) Session(id string) *Session { return &Session{c: c, id: id} }

// ID is the session id, "" at the browser level.
func (s *Session) ID() string { return s.id }

// Conn is the connection this session belongs to.
func (s *Session) Conn() *Conn { return s.c }

// Call sends one command on this session. See Conn.Call.
func (s *Session) Call(ctx context.Context, method string, params, result any) error {
	return s.c.Call(ctx, s.id, method, params, result)
}

// Subscribe registers fn for this session's events. See Conn.Subscribe.
func (s *Session) Subscribe(method string, fn func(params json.RawMessage)) (cancel func()) {
	return s.c.Subscribe(s.id, method, fn)
}

// On is Subscribe with the params decoded into T. A frame that does not decode
// is logged and skipped.
func On[T any](s *Session, method string, fn func(ev T)) (cancel func()) {
	return s.Subscribe(method, func(params json.RawMessage) {
		var ev T
		if len(params) > 0 {
			if err := json.Unmarshal(params, &ev); err != nil {
				s.c.log.Warn("cdp: malformed event params", "method", method, "err", err)
				return
			}
		}
		fn(ev)
	})
}
