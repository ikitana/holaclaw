// Package sidecar serves the sidecar half of the browser (docs/browser.md):
// HTTP and JSON over a unix socket, plus one WebSocket per live view. It owns
// no Chromium itself — every route is a thin front for an Engine, which the
// page layer implements.
//
// Two rules shape the code. Nothing an engine says about a page reaches the
// wire unless it is a *proto.Error: a plain Go error may carry page text, so
// it becomes a fixed 500. And no stream message is ever logged, in whole or in
// part: what a person types through takeover is not ours to record.
package sidecar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"holaclaw/internal/browser/proto"
	"holaclaw/internal/config"
)

const (
	// MaxJSONBody caps every request body. The largest thing the daemon sends
	// is an observe request with hints; 64 KiB is room to spare.
	MaxJSONBody = 64 << 10
	// MaxStreamMessage caps one inbound text frame of the live view: a pointer
	// or key event, or committed text from a paste.
	MaxStreamMessage = 16 << 10
	// DefaultAckTimeout is how long a frame waits for the viewer's ack before
	// the stream is closed. Backpressure must not become a leak.
	DefaultAckTimeout = 10 * time.Second
	// ReadHeaderTimeout bounds the headers. There is deliberately no write
	// timeout: the stream lives as long as the viewer's window.
	ReadHeaderTimeout = 10 * time.Second
	// DefaultShutdownTimeout bounds the graceful stop.
	DefaultShutdownTimeout = 5 * time.Second
	// socketProbeTimeout is how long the stale-socket probe waits before
	// deciding nobody is listening.
	socketProbeTimeout = 250 * time.Millisecond
	// streamInputBuffer is how many input messages may be in flight towards
	// the engine. Input is never dropped: a full buffer slows the read loop,
	// and a viewer that stalls that long loses its stream to the ack timeout.
	streamInputBuffer = 64
	// maxDispositionName caps the file name echoed in Content-Disposition.
	maxDispositionName = 100
)

// Shutdowner is the optional half of Engine: an engine that owns browser
// processes stops them here when the server shuts down. Engine itself has no
// such method (its Close closes one agent's session), so the server asks for
// this one by type assertion.
type Shutdowner interface {
	Shutdown(ctx context.Context) error
}

// Options are the server's knobs; every one has a working default.
type Options struct {
	Logger *slog.Logger
	// AckTimeout overrides DefaultAckTimeout; tests shorten it.
	AckTimeout time.Duration
	// ShutdownTimeout overrides DefaultShutdownTimeout.
	ShutdownTimeout time.Duration
	// OnListen is called once with the bound socket address, for the log and
	// for tests.
	OnListen func(addr net.Addr)
}

// Server is the sidecar's HTTP surface over one Engine.
type Server struct {
	engine     Engine
	log        *slog.Logger
	ackTimeout time.Duration
	shutdown   time.Duration
	onListen   func(net.Addr)
	// requireSocket refuses a live view that did not arrive on the unix
	// socket. Serve sets it; a bare Handler (tests) does not.
	requireSocket bool
}

// New returns a server for engine.
func New(engine Engine, opts Options) *Server {
	s := &Server{
		engine:     engine,
		log:        opts.Logger,
		ackTimeout: opts.AckTimeout,
		shutdown:   opts.ShutdownTimeout,
		onListen:   opts.OnListen,
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.ackTimeout <= 0 {
		s.ackTimeout = DefaultAckTimeout
	}
	if s.shutdown <= 0 {
		s.shutdown = DefaultShutdownTimeout
	}
	return s
}

// Handler is the routing table, exposed for tests. The patterns are the
// proto.Path* constants exactly.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET "+proto.PathHealth, s.logged(s.health, false))
	mux.Handle("GET "+proto.PathSessions, s.logged(s.sessions, false))
	mux.Handle("POST "+proto.PathOpen, s.logged(s.withAgent(s.open), true))
	mux.Handle("POST "+proto.PathClose, s.logged(s.withAgent(s.close), true))
	mux.Handle("POST "+proto.PathObserve, s.logged(s.withAgent(s.observe), true))
	mux.Handle("POST "+proto.PathAct, s.logged(s.withAgent(s.act), true))
	mux.Handle("POST "+proto.PathScreenshot, s.logged(s.withAgent(s.screenshot), true))
	mux.Handle("GET "+proto.PathDownload, s.logged(s.withAgent(s.download), true))
	// The stream route logs no agent and nothing else: its request carries the
	// live view, and the less of it is named in a log line the better.
	mux.Handle("GET "+proto.PathStream, s.logged(s.withAgent(s.stream), false))
	return mux
}

// Serve listens on the unix socket until ctx ends, then stops gracefully and
// closes the engine.
func (s *Server) Serve(ctx context.Context, socketPath string) error {
	ln, err := listenSocket(socketPath)
	if err != nil {
		return err
	}
	if s.onListen != nil {
		s.onListen(ln.Addr())
	}
	s.requireSocket = true
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: ReadHeaderTimeout,
		ConnContext:       connContext,
	}

	errc := make(chan error, 1)
	go func() {
		err := srv.Serve(socketListener{ln})
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errc <- err
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}

	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdown)
	defer cancel()
	if err := srv.Shutdown(stopCtx); err != nil && runErr == nil {
		runErr = err
	}
	// Live views are long-lived requests Shutdown can never drain, and the
	// engine owns the browsers behind them: stop it either way.
	if sd, ok := s.engine.(Shutdowner); ok {
		if err := sd.Shutdown(stopCtx); err != nil && runErr == nil {
			runErr = err
		}
	}
	return runErr
}

// --- routes ------------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.engine.Health(r.Context()))
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	list := s.engine.Sessions(r.Context())
	if list == nil {
		list = []proto.Session{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) open(w http.ResponseWriter, r *http.Request, agent string) {
	var req proto.OpenRequest
	if !decode(w, r, &req) {
		return
	}
	sess, err := s.engine.Open(r.Context(), agent, req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) close(w http.ResponseWriter, r *http.Request, agent string) {
	if err := s.engine.Close(r.Context(), agent); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) observe(w http.ResponseWriter, r *http.Request, agent string) {
	var req proto.ObserveRequest
	if !decode(w, r, &req) {
		return
	}
	obs, err := s.engine.Observe(r.Context(), agent, req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, obs)
}

// act answers 200 with the result even when it failed: a refused click is an
// answer, not a transport error. Only a broken session is a non-2xx.
func (s *Server) act(w http.ResponseWriter, r *http.Request, agent string) {
	var a proto.Action
	if !decode(w, r, &a) {
		return
	}
	res, err := s.engine.Act(r.Context(), agent, a)
	if err != nil {
		// An action that failed is still an answer: the executor acts on
		// res.Code (stale_ref → observe again, not_allowed → blocked), so a
		// coded failure is a 200 with OK=false, not an error status.
		var pe *proto.Error
		if !errors.As(err, &pe) {
			writeError(w, err)
			return
		}
		res.OK, res.Code, res.Detail = false, pe.Code, pe.Detail
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) screenshot(w http.ResponseWriter, r *http.Request, agent string) {
	jpeg, err := s.engine.Screenshot(r.Context(), agent)
	if err != nil {
		writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Content-Length", strconv.Itoa(len(jpeg)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(jpeg)
}

// download streams one quarantined file. Closing the reader is what deletes
// it, so the deferred Close is the whole point of the route.
func (s *Server) download(w http.ResponseWriter, r *http.Request, agent string) {
	id := r.PathValue("id")
	rc, meta, err := s.engine.Download(r.Context(), agent, id)
	if err != nil {
		writeError(w, err)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
	w.Header().Set("Content-Disposition", disposition(meta.Name))
	w.WriteHeader(http.StatusOK)
	// The body has started: a copy that fails now can only be dropped.
	_, _ = io.Copy(w, rc)
}

// --- plumbing ----------------------------------------------------------------

// agentHandler is a route whose agent name has already been validated.
type agentHandler func(w http.ResponseWriter, r *http.Request, agent string)

// withAgent rejects an agent name that could never name an agent before the
// engine sees it: the path segment is the one part of the request the daemon
// does not fully compose.
func (s *Server) withAgent(h agentHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agent := r.PathValue("agent")
		if !config.ValidAgentName(agent) {
			writeError(w, Errorf(proto.ErrBadAction, "invalid agent name"))
			return
		}
		h(w, r, agent)
	}
}

// logged writes one access line per request: method, path, status, duration,
// and the agent for everything but the stream. Never a body, never a header.
func (s *Server) logged(h http.HandlerFunc, withAgent bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		h(rec, r)
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.code(),
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if withAgent {
			attrs = append(attrs, "agent", r.PathValue("agent"))
		}
		s.log.Info("browser: request", attrs...)
	})
}

// recorder remembers the status for the access log and stays transparent to
// the WebSocket upgrade, which hijacks the connection through Unwrap.
type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(p)
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) code() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

// decode reads a capped JSON body. An absent body is the zero value: `open`
// and `observe` have nothing required in them.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil || r.Body == http.NoBody {
		return true
	}
	body := http.MaxBytesReader(w, r.Body, MaxJSONBody)
	if err := json.NewDecoder(body).Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return true // empty body
		}
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, Errorf(proto.ErrBadAction, "request body too large"))
			return false
		}
		writeError(w, Errorf(proto.ErrBadAction, "malformed request body"))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Marshalling our own types cannot fail on anything the caller sent.
		http.Error(w, `{"code":"browser"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError maps an engine error onto the wire. A *proto.Error is the
// engine's own, closed vocabulary and is passed through; anything else is a
// fixed 500, because a Go error's text may quote the page.
func writeError(w http.ResponseWriter, err error) {
	var pe *proto.Error
	if errors.As(err, &pe) {
		writeJSON(w, statusFor(pe.Code), pe)
		return
	}
	writeJSON(w, http.StatusInternalServerError, &proto.Error{
		Code:   proto.ErrBrowser,
		Detail: "browser failed",
	})
}

// statusFor is the code → status table of the sidecar protocol.
func statusFor(code proto.ErrorCode) int {
	switch code {
	case proto.ErrNoSession:
		return http.StatusNotFound
	case proto.ErrBadAction:
		return http.StatusBadRequest
	case proto.ErrNotAllowed:
		return http.StatusForbidden
	case proto.ErrStaleRef, proto.ErrHeld:
		return http.StatusConflict
	case proto.ErrTimeout:
		return http.StatusGatewayTimeout
	case proto.ErrBrowser:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// disposition builds a Content-Disposition the daemon can trust: the name came
// from a page, so everything but a conservative set of characters goes.
func disposition(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= maxDispositionName {
			break
		}
	}
	clean := strings.Trim(b.String(), "._")
	if clean == "" {
		clean = "download"
	}
	return fmt.Sprintf("attachment; filename=%q", clean)
}

// --- socket ------------------------------------------------------------------

// listenSocket binds the unix socket. The parent directory is created 0700 and
// the socket itself is 0660: the daemon reaches it through the shared group
// (see deploy/holaclaw-browser.service), nobody else does. A socket left behind
// by a crash is removed, but only after a probe proves nobody is listening, so
// a second sidecar cannot steal the first one's socket.
func listenSocket(path string) (net.Listener, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("sidecar: socket path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("sidecar: socket directory: %w", err)
	}
	if info, err := os.Lstat(abs); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("sidecar: %s exists and is not a socket", abs)
		}
		conn, derr := net.DialTimeout("unix", abs, socketProbeTimeout)
		if derr == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("sidecar: %s is already in use by another sidecar", abs)
		}
		if err := os.Remove(abs); err != nil {
			return nil, fmt.Errorf("sidecar: remove stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("sidecar: socket path: %w", err)
	}
	// The umask is what makes the socket 0660 the moment the kernel creates
	// it. Chmod afterwards would leave a window — however short — in which
	// anyone on the host could connect, and a window is all a race needs.
	ln, err := listenUmasked(abs)
	if err != nil {
		return nil, fmt.Errorf("sidecar: listen %s: %w", abs, err)
	}
	// Belt and braces: the umask can only take permissions away, so this
	// makes the group bits certain rather than assumed.
	if err := os.Chmod(abs, 0o660); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("sidecar: socket permissions: %w", err)
	}
	return ln, nil
}

// umaskMu serialises the process-wide umask: it is a property of the process,
// not of this call, so two listeners must not overlap in it.
var umaskMu sync.Mutex

// listenUmasked binds the socket under a umask that denies everyone but the
// owner and the group (0660), and puts the old one back either way.
func listenUmasked(abs string) (net.Listener, error) {
	umaskMu.Lock()
	old := syscall.Umask(0o117)
	ln, err := net.Listen("unix", abs)
	syscall.Umask(old)
	umaskMu.Unlock()
	return ln, err
}

// socketListener marks every connection it accepts, so the live view can tell
// a request that arrived on the socket from anything else.
type socketListener struct{ net.Listener }

func (l socketListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return socketConn{c}, nil
}

type socketConn struct{ net.Conn }

// socketKey marks a per-connection context. Only ConnContext can set it: a
// request can never claim it itself.
type socketKey struct{}

func connContext(ctx context.Context, c net.Conn) context.Context {
	if _, ok := c.(socketConn); ok {
		return context.WithValue(ctx, socketKey{}, true)
	}
	return ctx
}

func fromSocket(ctx context.Context) bool {
	v, _ := ctx.Value(socketKey{}).(bool)
	return v
}

// --- live view ----------------------------------------------------------------

// stream upgrades to a WebSocket and runs the engine's live view on it. There
// is no origin check — the socket's file permission is the authentication, and
// the daemon has already authorised the viewer — but a request that did not
// come in over the socket is refused outright.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, agent string) {
	if s.requireSocket && !fromSocket(r.Context()) {
		writeError(w, Errorf(proto.ErrNotAllowed, "the live view is served on the unix socket only"))
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return // Accept has already answered
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(MaxStreamMessage)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	in := make(chan proto.StreamMessage, streamInputBuffer)
	v := &wsViewer{conn: c, acks: make(chan struct{}, 1), timeout: s.ackTimeout}

	go func() {
		defer cancel()
		readStream(ctx, c, in, v.acks)
	}()

	err = s.engine.Stream(ctx, agent, in, v)
	switch {
	case err == nil, ctx.Err() != nil:
		_ = c.Close(websocket.StatusNormalClosure, "")
	default:
		// The reason is a code, never a detail: a detail may quote the page.
		var pe *proto.Error
		reason := string(proto.ErrBrowser)
		if errors.As(err, &pe) {
			reason = string(pe.Code)
		}
		_ = c.Close(websocket.StatusInternalError, reason)
	}
}

// readStream decodes viewer input until the connection ends. An ack releases
// the frame the viewer just showed; an unknown type is ignored, so a newer
// client cannot break an older sidecar; malformed JSON ends the stream with
// 1003. Nothing read here is logged.
func readStream(ctx context.Context, c *websocket.Conn, in chan<- proto.StreamMessage, acks chan<- struct{}) {
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue // the viewer sends no binary frames
		}
		var m proto.StreamMessage
		if err := json.Unmarshal(data, &m); err != nil {
			_ = c.Close(websocket.StatusUnsupportedData, "malformed message")
			return
		}
		switch m.Type {
		case proto.StreamAck:
			// Backpressure only: the ack never reaches the engine.
			select {
			case acks <- struct{}{}:
			default:
			}
		case proto.StreamPointer, proto.StreamWheel, proto.StreamKey,
			proto.StreamText, proto.StreamTab, proto.StreamQuality:
			select {
			case in <- m:
			case <-ctx.Done():
				return
			}
		default:
			// Unknown or sidecar-to-viewer type: ignored on purpose.
		}
	}
}

// wsViewer is the Viewer the engine writes to: one JPEG per binary frame, meta
// as JSON, and a Frame that returns only once the viewer acknowledged it.
type wsViewer struct {
	conn    *websocket.Conn
	acks    chan struct{}
	timeout time.Duration
	mu      sync.Mutex // one writer at a time on the connection
}

var _ Viewer = (*wsViewer)(nil)

// Frame writes the JPEG and waits for the viewer's ack. That wait is the
// screencast's backpressure: a slow link slows Chromium instead of queueing
// frames. A viewer that does not answer within the timeout loses the stream.
func (v *wsViewer) Frame(ctx context.Context, jpeg []byte, width, height int) error {
	v.mu.Lock()
	err := v.conn.Write(ctx, websocket.MessageBinary, jpeg)
	v.mu.Unlock()
	if err != nil {
		return err
	}
	t := time.NewTimer(v.timeout)
	defer t.Stop()
	select {
	case <-v.acks:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		// CloseNow, not Close: the close handshake waits for a peer that has
		// already proved it is not reading.
		_ = v.conn.CloseNow()
		return Errorf(proto.ErrTimeout, "viewer did not acknowledge a frame")
	}
}

func (v *wsViewer) Meta(ctx context.Context, m proto.StreamMessage) error {
	body, err := json.Marshal(m)
	if err != nil {
		return Errorf(proto.ErrBrowser, "meta could not be encoded")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.conn.Write(ctx, websocket.MessageText, body)
}
