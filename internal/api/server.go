package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"holaclaw/internal/store"
)

const (
	// ReadHeaderTimeout bounds how long a client may take to send its
	// headers. There is deliberately no WriteTimeout: /v1/events is a stream
	// that lives as long as the client window.
	ReadHeaderTimeout = 10 * time.Second
	// DefaultShutdownTimeout bounds the graceful stop.
	DefaultShutdownTimeout = 5 * time.Second
	// socketProbeTimeout is how long the stale-socket probe waits for an
	// answer before deciding nobody is listening.
	socketProbeTimeout = 250 * time.Millisecond
)

// ServeOptions is where the API listens.
type ServeOptions struct {
	// Socket is the unix socket path; it is admin by file permission.
	Socket string
	// Listen is an optional TCP address. Binding a non-loopback address is
	// refused without TLS and at least one token (docs/security.md).
	Listen  string
	TLSCert string
	TLSKey  string
	// AllowInsecureTransport waives the TLS half of that rule; the token half
	// still applies.
	AllowInsecureTransport bool
	// ShutdownTimeout defaults to DefaultShutdownTimeout.
	ShutdownTimeout time.Duration
	// OnListen is called once per bound listener ("socket" or "tcp"), for the
	// daemon's log and for tests that need the chosen port.
	OnListen func(kind string, addr net.Addr)
}

// InsecureBindError refuses a non-loopback bind that is not protected. It
// names what is missing, because the operator has to fix exactly that.
type InsecureBindError struct {
	Addr   string
	Reason string
}

func (e *InsecureBindError) Error() string {
	return fmt.Sprintf("api: refusing to bind %s: %s (bind a loopback address, or configure server.tls_cert / server.tls_key or set server.allow_insecure_transport, and run `holaclaw init`)", e.Addr, e.Reason)
}

// Serve runs the Control API until ctx ends, then shuts down gracefully.
func Serve(ctx context.Context, b *Backend, opts ServeOptions) error {
	if opts.Socket == "" && opts.Listen == "" {
		return errors.New("api: no socket and no listen address configured")
	}
	handler := NewHandler(b)

	type bound struct {
		srv *http.Server
		ln  net.Listener
		tls bool
	}
	var servers []bound
	closeAll := func() {
		for _, s := range servers {
			_ = s.ln.Close()
		}
	}

	if opts.Socket != "" {
		ln, err := listenSocket(opts.Socket)
		if err != nil {
			return err
		}
		servers = append(servers, bound{srv: newServer(handler), ln: SocketListener{ln}})
		if opts.OnListen != nil {
			opts.OnListen("socket", ln.Addr())
		}
	}

	if opts.Listen != "" {
		if err := b.checkBind(ctx, opts); err != nil {
			closeAll()
			return err
		}
		ln, err := net.Listen("tcp", opts.Listen)
		if err != nil {
			closeAll()
			return fmt.Errorf("api: listen %s: %w", opts.Listen, err)
		}
		servers = append(servers, bound{srv: newServer(handler), ln: ln, tls: opts.TLSCert != ""})
		if opts.OnListen != nil {
			opts.OnListen("tcp", ln.Addr())
		}
	}

	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func(s bound) {
			var err error
			if s.tls {
				err = s.srv.ServeTLS(s.ln, opts.TLSCert, opts.TLSKey)
			} else {
				err = s.srv.Serve(s.ln)
			}
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			errc <- err
		}(s)
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}

	timeout := opts.ShutdownTimeout
	if timeout <= 0 {
		timeout = DefaultShutdownTimeout
	}
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	// Event streams are long-lived requests that Shutdown can never drain: end
	// them first, or every stop with a client attached costs the full grace
	// period and reports an error.
	if b.Hub != nil {
		b.Hub.Close()
	}
	for _, s := range servers {
		if err := s.srv.Shutdown(stopCtx); err != nil && runErr == nil {
			runErr = err
		}
	}
	if b.Hub != nil {
		b.Hub.Close()
	}
	return runErr
}

func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: ReadHeaderTimeout,
		IdleTimeout:       2 * time.Minute,
		ConnContext:       ConnContext,
	}
}

// listenSocket binds the unix socket: the directory is 0700, the socket 0600,
// and a socket file left behind by a crash is removed — but only after a probe
// proves nobody is listening on it, so a second daemon cannot steal the first
// one's socket.
func listenSocket(path string) (net.Listener, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("api: socket path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("api: socket directory: %w", err)
	}
	if info, err := os.Lstat(abs); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("api: %s exists and is not a socket", abs)
		}
		conn, derr := net.DialTimeout("unix", abs, socketProbeTimeout)
		if derr == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("api: %s is already in use by another holaclaw", abs)
		}
		if err := os.Remove(abs); err != nil {
			return nil, fmt.Errorf("api: remove stale socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("api: socket path: %w", err)
	}
	ln, err := net.Listen("unix", abs)
	if err != nil {
		return nil, fmt.Errorf("api: listen %s: %w", abs, err)
	}
	if err := os.Chmod(abs, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("api: socket permissions: %w", err)
	}
	return ln, nil
}

// checkBind applies the rule of docs/security.md: a loopback bind is fine,
// anything wider needs TLS *and* at least one live token, so the port is never
// open with nothing in front of it. AllowInsecureTransport waives only TLS.
func (b *Backend) checkBind(ctx context.Context, opts ServeOptions) error {
	loopback, err := isLoopback(opts.Listen)
	if err != nil {
		return err
	}
	if loopback {
		return nil
	}
	if !opts.AllowInsecureTransport && (opts.TLSCert == "" || opts.TLSKey == "") {
		return &InsecureBindError{Addr: opts.Listen, Reason: "it is not a loopback address and TLS is not configured"}
	}
	if b.Auth == nil {
		return &InsecureBindError{Addr: opts.Listen, Reason: "no token store is available"}
	}
	// An owner, not a token: after `holaclaw init` devices get their first
	// token over this very port, by pairing.
	_, err = b.Auth.Owner(ctx)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return &InsecureBindError{Addr: opts.Listen, Reason: "holaclaw is not initialised yet: run `holaclaw init`"}
	case err != nil:
		return fmt.Errorf("api: check owner: %w", err)
	}
	return nil
}

// isLoopback reports whether addr binds only the loopback interface. An empty
// or wildcard host binds every interface, so it is not loopback.
func isLoopback(addr string) (bool, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Errorf("api: listen address %q must be host:port: %w", addr, err)
	}
	switch host {
	case "":
		return false, nil
	case "localhost":
		return true, nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// A name we cannot resolve here is treated as public: refusing is the
		// safe side of the mistake.
		return false, nil
	}
	return ip.IsLoopback(), nil
}
