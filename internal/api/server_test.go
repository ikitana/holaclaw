package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/auth"
	"holaclaw/internal/event"
	"holaclaw/internal/store"
)

// shortDir is a temp directory with a short name: a unix socket path is capped
// at ~107 bytes, and the go test temp directory already eats most of that.
func shortDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hb")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// serve runs the API on a fresh unix socket and returns its path.
func (h *harness) serve(t testing.TB, opts ServeOptions) (ServeOptions, func()) {
	t.Helper()
	if opts.Socket == "" {
		opts.Socket = filepath.Join(shortDir(t), "s.sock")
	}
	type bound struct {
		kind string
		addr net.Addr
	}
	// Every listener reports through the channel, and serve waits for all of
	// them: handing an address over any other way races with the goroutine
	// that bound it.
	want := 1
	if opts.Listen != "" {
		want++
	}
	ready := make(chan bound, want)
	opts.OnListen = func(kind string, addr net.Addr) { ready <- bound{kind, addr} }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, h.b, opts) }()
	addrs := map[string]net.Addr{}
	for range want {
		select {
		case b := <-ready:
			addrs[b.kind] = b.addr
		case err := <-done:
			t.Fatalf("serve returned early: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("the server did not start")
		}
	}
	h.addrs = addrs
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("the server did not stop")
		}
	}
	t.Cleanup(stop)
	return opts, stop
}

// TestServeRefusesNonLoopbackBind is the docs/security.md rule: a port open to the network
// needs TLS and a token before anything listens on it.
func TestServeRefusesNonLoopbackBind(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ctx := context.Background()

	var insecure *InsecureBindError
	err := Serve(ctx, h.b, ServeOptions{Listen: "0.0.0.0:0"})
	if !errors.As(err, &insecure) {
		t.Fatalf("bind without TLS: err = %v, want InsecureBindError", err)
	}
	if insecure.Addr != "0.0.0.0:0" {
		t.Fatalf("error = %v", insecure)
	}

	// A host name that is not loopback is refused too.
	if err := Serve(ctx, h.b, ServeOptions{Listen: "example.test:8080"}); !errors.As(err, &insecure) {
		t.Fatalf("named host: err = %v", err)
	}
	// A malformed address is a plain error, not a bind.
	if err := Serve(ctx, h.b, ServeOptions{Listen: "nonsense"}); err == nil || errors.As(err, &insecure) {
		t.Fatalf("malformed address: err = %v", err)
	}
	// Nothing at all configured is refused.
	if err := Serve(ctx, h.b, ServeOptions{}); err == nil {
		t.Fatal("serve with no socket and no listen address succeeded")
	}
}

// TestCheckBindAllowInsecureTransport: the acknowledgement waives TLS and
// nothing else, so the daemon must still be initialised.
func TestCheckBindAllowInsecureTransport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := &Backend{Store: st, Auth: auth.New(st)}

	var insecure *InsecureBindError
	opts := ServeOptions{Listen: "0.0.0.0:0", AllowInsecureTransport: true}
	if err := b.checkBind(ctx, opts); !errors.As(err, &insecure) || !strings.Contains(insecure.Reason, "init") {
		t.Fatalf("plain http before init: err = %v, want an init refusal", err)
	}
	if _, err := b.Auth.CreateOwner(ctx, false, nil); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if err := b.checkBind(ctx, opts); err != nil {
		t.Fatalf("plain http acknowledged after init: %v", err)
	}
	// Without the acknowledgement the same bind is refused for TLS.
	opts.AllowInsecureTransport = false
	if err := b.checkBind(ctx, opts); !errors.As(err, &insecure) || !strings.Contains(insecure.Reason, "TLS") {
		t.Fatalf("plain http not acknowledged: err = %v, want a TLS refusal", err)
	}
}

// TestServeRefusesNonLoopbackBeforeInit covers the second half of the rule:
// TLS alone is not enough while there is no owner. An owner with no token at
// all is enough: its devices get their first token by pairing over this port.
func TestServeRefusesNonLoopbackBeforeInit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "data"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b := &Backend{Store: st, Auth: auth.New(st)}

	opts := ServeOptions{Listen: "0.0.0.0:0", TLSCert: filepath.Join(dir, "cert.pem"), TLSKey: filepath.Join(dir, "key.pem")}
	var insecure *InsecureBindError
	if err := Serve(ctx, b, opts); !errors.As(err, &insecure) {
		t.Fatalf("err = %v, want InsecureBindError", err)
	}

	if _, err := b.Auth.CreateOwner(ctx, false, nil); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if toks, err := b.Auth.ListTokens(ctx, auth.TokenFilter{}); err != nil || len(toks) != 0 {
		t.Fatalf("tokens after an init without one = %v, %v", toks, err)
	}
	if err := b.checkBind(ctx, opts); err != nil {
		t.Fatalf("checkBind with an owner and no token: %v", err)
	}
}

// TestServeLoopbackTCP: plain loopback needs no TLS.
func TestServeLoopbackTCP(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.serve(t, ServeOptions{Listen: "127.0.0.1:0"})
	tcp := h.addrs["tcp"]
	if tcp == nil {
		t.Fatal("no TCP listener")
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+tcp.String()+BaseURL+"/status", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+h.ownerToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	st := decode[gen.Status](t, resp, http.StatusOK)
	if st.Version != "test" {
		t.Fatalf("status = %+v", st)
	}
}

func TestListenSocket(t *testing.T) {
	t.Parallel()

	t.Run("fresh socket is 0600 in a 0700 directory", func(t *testing.T) {
		path := filepath.Join(shortDir(t), "sub", "s.sock")
		ln, err := listenSocket(path)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer func() { _ = ln.Close() }()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("socket mode = %o, want 600", perm)
		}
		dir, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatalf("stat dir: %v", err)
		}
		if perm := dir.Mode().Perm(); perm != 0o700 {
			t.Fatalf("directory mode = %o, want 700", perm)
		}
	})

	t.Run("a stale socket is removed", func(t *testing.T) {
		path := filepath.Join(shortDir(t), "s.sock")
		stale, err := net.Listen("unix", path)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		// Leave the file behind, the way a crash does.
		stale.(*net.UnixListener).SetUnlinkOnClose(false)
		if err := stale.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the stale socket is gone before the test: %v", err)
		}
		ln, err := listenSocket(path)
		if err != nil {
			t.Fatalf("listen over a stale socket: %v", err)
		}
		_ = ln.Close()
	})

	t.Run("a live socket is not stolen", func(t *testing.T) {
		path := filepath.Join(shortDir(t), "s.sock")
		live, err := listenSocket(path)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer func() { _ = live.Close() }()
		// Something must answer the probe.
		go func() {
			for {
				c, err := live.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
		if _, err := listenSocket(path); err == nil {
			t.Fatal("a second listen took over a live socket")
		}
	})

	t.Run("a regular file is refused", func(t *testing.T) {
		path := filepath.Join(shortDir(t), "s.sock")
		if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := listenSocket(path); err == nil {
			t.Fatal("a regular file was replaced by a socket")
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("the file was removed anyway: %v", err)
		}
	})
}

// TestClientRoundTripOverUnixSocket drives the CLI client against a real
// server on a real socket: the socket is admin, so no token is involved.
func TestClientRoundTripOverUnixSocket(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	opts, _ := h.serve(t, ServeOptions{})
	ctx := context.Background()

	c, err := Dial(opts.Socket, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.Version != "test" {
		t.Fatalf("status = %+v", st)
	}

	users, err := c.ListIdentities(ctx, false)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("users = %d, want 3", len(users))
	}

	sess, err := c.CreateSession(ctx, gen.SessionCreate{})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	stream, err := c.Events(ctx, sess.Id, "")
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	defer func() { _ = stream.Close() }()

	acc, err := c.SendMessage(ctx, sess.Id, gen.MessageSend{Id: string(event.NewID()), Text: ptr("hola")})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if acc.TraceId == "" {
		t.Fatalf("accepted = %+v", acc)
	}

	reply := readUntil(t, stream, string(event.Reply), 10*time.Second)
	if reply.TraceId != acc.TraceId {
		t.Fatalf("reply trace = %s, want %s", reply.TraceId, acc.TraceId)
	}
	if stream.LastID() != reply.Id {
		t.Fatalf("LastID = %q, want %q", stream.LastID(), reply.Id)
	}

	trace, err := c.GetTrace(ctx, acc.TraceId)
	if err != nil {
		t.Fatalf("trace: %v", err)
	}
	if len(trace.Messages) == 0 {
		t.Fatal("the trace carries no messages")
	}

	if _, err := c.ListApprovals(ctx); err != nil {
		t.Fatalf("approvals: %v", err)
	}
	created, err := c.CreateToken(ctx, gen.TokenCreate{Name: "gui", Scope: ptr(gen.TokenCreateScopeClient), UserId: ptr(string(h.other.ID))})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if created.Secret == "" {
		t.Fatal("no secret returned")
	}
	if _, err := c.CreateInvite(ctx, gen.InviteCreate{Channel: "holaclaw", UserId: ptr(string(h.other.ID))}); err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if err := c.PutCredentials(ctx, "ollama", map[string]string{"api_key": "written-once"}); err != nil {
		t.Fatalf("put credentials: %v", err)
	}

	// The error model survives the round trip.
	_, err = c.GetTrace(ctx, string(event.NewID()))
	var cerr *ClientError
	if !errors.As(err, &cerr) || cerr.Code != CodeNotFound {
		t.Fatalf("err = %v, want a not_found ClientError", err)
	}
}

// readUntil pulls events off the client stream until one of the wanted type
// arrives.
func readUntil(t *testing.T, s *EventStream, typ string, timeout time.Duration) gen.Event {
	t.Helper()
	type result struct {
		e   gen.Event
		err error
	}
	ch := make(chan result)
	go func() {
		for {
			e, err := s.Next()
			ch <- result{e, err}
			if err != nil {
				return
			}
		}
	}()
	deadline := time.After(timeout)
	for {
		select {
		case r := <-ch:
			if r.err != nil {
				if errors.Is(r.err, io.EOF) {
					t.Fatalf("the stream ended before a %s event", typ)
				}
				t.Fatalf("next: %v", r.err)
			}
			if r.e.Type == typ {
				return r.e
			}
		case <-deadline:
			t.Fatalf("no %s event within %s", typ, timeout)
		}
	}
}

func TestDialRejectsAnEmptyAddress(t *testing.T) {
	t.Parallel()
	if _, err := Dial("", ""); err == nil {
		t.Fatal("an empty address was accepted")
	}
	c, err := Dial("http://127.0.0.1:1", "tok")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if c.base != "http://127.0.0.1:1"+BaseURL {
		t.Fatalf("base = %q", c.base)
	}
}
