package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/backup"
	"holaclaw/internal/skill"
	"holaclaw/internal/store"
)

// BaseURL is the one versioned prefix every operation lives under.
const BaseURL = "/v1"

// MaxJSONBody caps a JSON request body. Blobs and multipart uploads are capped
// by the store's blob size instead.
const MaxJSONBody = 1 << 20

type ctxKey int

const (
	keyPrincipal ctxKey = iota
	keySocket
)

// markSocket tags a request context as arriving on the unix socket. The server
// sets it per connection (ConnContext), never per request: a header could
// otherwise claim it.
func markSocket(ctx context.Context) context.Context {
	return context.WithValue(ctx, keySocket, true)
}

// IsSocket reports whether the request arrived on the unix socket.
func IsSocket(ctx context.Context) bool {
	v, _ := ctx.Value(keySocket).(bool)
	return v
}

// PrincipalFrom returns the authenticated caller of a request.
func PrincipalFrom(ctx context.Context) (auth.Principal, bool) {
	p, ok := ctx.Value(keyPrincipal).(auth.Principal)
	return p, ok
}

func withPrincipal(ctx context.Context, p auth.Principal) context.Context {
	return context.WithValue(ctx, keyPrincipal, p)
}

// NewHandler mounts the generated router under /v1 behind the middleware
// chain: panic recovery, access log, authentication, then per-operation scope
// and body caps. It fills b.Hub when the daemon has not.
func NewHandler(b *Backend) http.Handler {
	if b.Hub == nil {
		b.Hub = NewHub(b.Store, b.logger())
	}
	h := &handlers{b: b, fresh: newFreshBlobs()}
	mux := http.NewServeMux()
	gen.HandlerWithOptions(h, gen.StdHTTPServerOptions{
		BaseURL:    BaseURL,
		BaseRouter: mux,
		// The generated wrapper applies these inside routing, so the pattern
		// (and with it the operation) is known: order is scope, then body cap.
		Middlewares:      []gen.MiddlewareFunc{b.capBody, b.checkScope},
		ErrorHandlerFunc: b.paramError,
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such endpoint")
	})
	return b.recover(b.accessLog(b.gate(b.authenticate(mux))))
}

// gate answers every request with 503 while the database has no owner. It
// runs before authentication: there is no token to check yet, and the answer
// has to name the way out.
func (b *Backend) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.Bootstrap == nil {
			next.ServeHTTP(w, r)
			return
		}
		ok, err := b.Bootstrap.Initialised(r.Context())
		if err != nil {
			b.logger().Error("api: check initialised", "error", err)
			writeError(w, http.StatusInternalServerError, CodeInternal, "internal error")
			return
		}
		if !ok {
			writeError(w, http.StatusServiceUnavailable, CodeNotInitialised,
				"holaclaw is not initialised: run `holaclaw init` on the host")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// operationOf resolves the matched pattern back to its spec operation. An
// unmatched request never reaches a middleware, so an empty result means the
// route table drifted from the generated router — the drift test guards it and
// the middleware denies at runtime.
func operationOf(r *http.Request) string {
	pattern := r.Pattern
	if pattern == "" {
		return ""
	}
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		return ""
	}
	path = strings.TrimPrefix(path, BaseURL)
	return routes[route{Method: method, Path: path}]
}

// authenticate resolves the caller. Socket requests arrive as holaclaw:cli,
// admin by file permission; everything else needs a bearer token. Both resolve
// through the binding on every request. Every token failure is the same
// answer: a client cannot tell a wrong token from a revoked or unknown one.
func (b *Backend) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsSocket(r.Context()) {
			b.authenticateSocket(w, r, next)
			return
		}
		if isBrowserStream(r) {
			// The one operation the spec declares with `security: []`: a
			// browser cannot set an Authorization header on a WebSocket, so
			// the ticket in the query is the whole authentication and the
			// handler redeems it. The request reaches it with no principal.
			next.ServeHTTP(w, r)
			return
		}
		if isPair(r) {
			// The other `security: []` operation: a device pairing has no
			// token yet, and the code in the body is its credential, which
			// the handler redeems.
			next.ServeHTTP(w, r)
			return
		}
		token, ok := bearer(r.Header.Get("Authorization"))
		if !ok || b.Auth == nil {
			unauthorized(w)
			return
		}
		p, err := b.Auth.Authenticate(r.Context(), token)
		if errors.Is(err, auth.ErrIdentityDisabled) {
			writeError(w, http.StatusForbidden, CodeIdentityDisabled, "the identity is disabled")
			return
		}
		if err != nil {
			if !errors.Is(err, auth.ErrTokenInvalid) {
				b.logger().Error("api: authenticate", "error", err)
			}
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
	})
}

// authenticateSocket resolves holaclaw:cli for a socket request. The gate has
// already answered a database with no owner; a binding missing after that is a
// broken database, not a caller to tell apart.
func (b *Backend) authenticateSocket(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if b.Auth == nil {
		unauthorized(w)
		return
	}
	p, err := b.Auth.SocketPrincipal(r.Context())
	if err != nil {
		b.logger().Error("api: resolve the unix socket's binding", "binding", "holaclaw:"+auth.SocketAlias, "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), p)))
}

// isBrowserStream matches the live-view WebSocket before routing has run, so
// the auth middleware can let the ticket answer for it. It is deliberately
// exact: GET, one agent segment, and a ticket in the query.
//
// It matches the escaped path, never the decoded one. A %2F inside a segment
// decodes to a slash, so a decoded match would hand the bypass to a path the
// router then sends somewhere else entirely; an agent segment carrying an
// encoded slash is refused here rather than decoded.
func isBrowserStream(r *http.Request) bool {
	if r.Method != http.MethodGet || !r.URL.Query().Has("ticket") {
		return false
	}
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), BaseURL+"/browser/sessions/")
	if !ok {
		return false
	}
	agent, ok := strings.CutSuffix(rest, "/stream")
	if !ok || agent == "" || strings.Contains(agent, "/") {
		return false
	}
	return !strings.Contains(strings.ToLower(agent), "%2f")
}

// bearer extracts the token from an Authorization header.
func bearer(h string) (string, bool) {
	scheme, rest, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	rest = strings.TrimSpace(rest)
	return rest, rest != ""
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid or missing token")
}

// checkScope enforces the `x-scope` the spec declares for the operation.
func (b *Backend) checkScope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		op := operationOf(r)
		if op == "streamBrowser" || op == "pairDevice" {
			// The ticket, or the pairing code, stands in for both the token
			// and the scope: each was issued by a caller the scope gate had
			// already cleared, and the handler refuses an unknown, used or
			// expired one.
			next.ServeHTTP(w, r)
			return
		}
		p, ok := PrincipalFrom(r.Context())
		if !ok {
			unauthorized(w)
			return
		}
		required, known := scopes[op]
		if !known {
			b.logger().Error("api: no scope for operation", "pattern", r.Pattern)
			writeError(w, http.StatusForbidden, CodeForbidden, "operation is not permitted")
			return
		}
		if !p.Scope.Allows(required) {
			writeError(w, http.StatusForbidden, CodeForbidden, "token scope does not allow this operation")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// capBody bounds the request body before any handler reads it: 1 MiB for JSON,
// the store's blob cap for blob and multipart uploads (the store enforces its
// own configured cap on top), the install limit for a skill upload and the
// archive limit for a backup upload.
func (b *Backend) capBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, bodyLimit(r))
		}
		next.ServeHTTP(w, r)
	})
}

func bodyLimit(r *http.Request) int64 {
	switch operationOf(r) {
	case "putBlob":
		return store.DefaultMaxBlobSize
	case "uploadSkill":
		return skill.MaxInstallTotal
	case "uploadBackup":
		return backup.MaxUpload
	case "sendMessage":
		if isMultipart(r) {
			return store.DefaultMaxBlobSize
		}
	}
	return MaxJSONBody
}

func isMultipart(r *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/")
}

// recover turns a panic into a plain 500: no stack, no internals on the wire.
func (b *Backend) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if errors.Is(errAbortHandler(v), http.ErrAbortHandler) {
					panic(v)
				}
				b.logger().Error("api: handler panicked", "method", r.Method, "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, CodeInternal, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// errAbortHandler recovers the sentinel the http package uses to drop a
// connection on purpose; anything else is a real panic.
func errAbortHandler(v any) error {
	err, _ := v.(error)
	return err
}

// accessLog records one line per request. It never logs headers (the bearer
// token lives there) or bodies (they carry credentials).
func (b *Backend) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		p, _ := PrincipalFrom(r.Context())
		b.logger().Info("api: request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.code(),
			"bytes", rec.written,
			"duration_ms", time.Since(start).Milliseconds(),
			"scope", string(p.Scope),
			"user", string(p.UserID),
			"binding", p.Binding,
			"socket", IsSocket(r.Context()))
	})
}

// recorder remembers the status for the access log while staying transparent
// to the SSE handler, which needs Flush.
type recorder struct {
	http.ResponseWriter
	status  int
	written int64
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
	n, err := r.ResponseWriter.Write(p)
	r.written += int64(n)
	return n, err
}

func (r *recorder) code() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// paramError answers a binding failure from the generated wrapper: a bad or
// missing parameter is a bad request, never an internal error.
func (b *Backend) paramError(w http.ResponseWriter, _ *http.Request, err error) {
	writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
}

// --- responses ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternal, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	body, _ := json.Marshal(gen.Error{Code: code, Message: message})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// fail is the handlers' error path: it maps the errors the services return
// onto the stable codes and never leaks an internal message.
func (b *Backend) fail(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, "not found")
	case errors.Is(err, store.ErrBlobTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, CodeTooLarge, "payload too large")
	case tooLarge(err):
		writeError(w, http.StatusRequestEntityTooLarge, CodeTooLarge, "payload too large")
	case errors.Is(err, ErrBadInput):
		writeError(w, http.StatusBadRequest, CodeBadRequest, err.Error())
	case errors.Is(err, context.Canceled):
		writeError(w, http.StatusBadRequest, CodeBadRequest, "request cancelled")
	default:
		b.logger().Error("api: "+what, "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternal, "internal error")
	}
}

func tooLarge(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

func badRequest(w http.ResponseWriter, message string) {
	writeError(w, http.StatusBadRequest, CodeBadRequest, message)
}

func notFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, CodeNotFound, "not found")
}

func notImplemented(w http.ResponseWriter, what string) {
	writeError(w, http.StatusNotImplemented, CodeNotImplemented, what+" is not available in this build")
}

// decodeJSON reads a JSON body under the body cap. A body over the cap is a
// too_large, anything else a bad_request.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if tooLarge(err) {
			writeError(w, http.StatusRequestEntityTooLarge, CodeTooLarge, "payload too large")
			return false
		}
		if errors.Is(err, io.EOF) {
			badRequest(w, "a JSON body is required")
			return false
		}
		badRequest(w, "invalid JSON body")
		return false
	}
	return true
}

// handlers implements the generated ServerInterface.
type handlers struct {
	b *Backend
	// fresh remembers uploads not yet attached to a message, so the principal
	// that uploaded one can read it back (see mayReadBlob).
	fresh *freshBlobs
}

var _ gen.ServerInterface = (*handlers)(nil)

// SocketListener marks every connection it accepts as a socket connection, so
// the auth middleware can trust the file permission instead of a token. Pair
// it with ConnContext on the http.Server.
type SocketListener struct{ net.Listener }

func (l SocketListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return socketConn{c}, nil
}

// socketConn is the marker ConnContext looks for.
type socketConn struct{ net.Conn }

// ConnContext marks the per-connection context of socket connections. It is
// the http.Server hook: a request can never set this itself.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	if _, ok := c.(socketConn); ok {
		return markSocket(ctx)
	}
	return ctx
}
