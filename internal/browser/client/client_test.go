package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"

	"holaclaw/internal/browser/proto"
)

// fakeSidecar is a sidecar on a unix socket: the proto routes, nothing else.
type fakeSidecar struct {
	t      testing.TB
	socket string
}

func newFakeSidecar(t testing.TB, h http.Handler) *fakeSidecar {
	t.Helper()
	// Not t.TempDir: a unix socket path is capped at ~100 bytes and the test
	// name goes into that path.
	dir, err := os.MkdirTemp("", "hb")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return &fakeSidecar{t: t, socket: socket}
}

// routes is the sidecar surface the client uses.
func (f *fakeSidecar) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+proto.PathHealth, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, proto.Health{Version: "test", Binary: "/usr/bin/chromium", Headless: true})
	})
	mux.HandleFunc("GET "+proto.PathSessions, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []proto.Session{{Agent: "main", Tabs: []proto.Tab{{ID: "t1", Active: true}}}})
	})
	mux.HandleFunc("POST "+proto.PathOpen, func(w http.ResponseWriter, r *http.Request) {
		var req proto.OpenRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		writeJSON(w, http.StatusOK, proto.Session{Agent: r.PathValue("agent"), Tabs: []proto.Tab{{ID: "t1", URL: req.Allow[0]}}})
	})
	mux.HandleFunc("POST "+proto.PathClose, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+proto.PathObserve, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, proto.Observation{URL: "https://example.test", Total: 3})
	})
	mux.HandleFunc("POST "+proto.PathAct, func(w http.ResponseWriter, r *http.Request) {
		var a proto.Action
		_ = json.NewDecoder(r.Body).Decode(&a)
		writeJSON(w, http.StatusOK, proto.ActResult{OK: true, URL: a.URL, Navigated: a.Kind == proto.ActNavigate})
	})
	mux.HandleFunc("POST "+proto.PathScreenshot, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte{0xff, 0xd8, 0xff})
	})
	mux.HandleFunc("GET "+proto.PathDownload, func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "d1" {
			writeJSON(w, http.StatusBadRequest, proto.Error{Code: proto.ErrBadAction, Detail: "unknown id"})
			return
		}
		_, _ = w.Write([]byte("file body"))
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func newClient(t testing.TB, h http.Handler) *Client {
	f := newFakeSidecar(t, h)
	return New(f.socket, Options{Timeout: 2 * time.Second})
}

func TestCalls(t *testing.T) {
	t.Parallel()
	f := &fakeSidecar{t: t}
	c := newClient(t, f.routes())
	ctx := context.Background()

	health, err := c.Health(ctx)
	if err != nil || health.Binary != "/usr/bin/chromium" {
		t.Fatalf("health = %+v, %v", health, err)
	}
	sessions, err := c.Sessions(ctx)
	if err != nil || len(sessions) != 1 || sessions[0].Agent != "main" {
		t.Fatalf("sessions = %+v, %v", sessions, err)
	}
	sess, err := c.Open(ctx, "main", proto.OpenRequest{Allow: []string{"example.test"}})
	if err != nil || sess.Agent != "main" || sess.Tabs[0].URL != "example.test" {
		t.Fatalf("open = %+v, %v", sess, err)
	}
	obs, err := c.Observe(ctx, "main", proto.ObserveRequest{Hints: []string{"search"}})
	if err != nil || obs.Total != 3 {
		t.Fatalf("observe = %+v, %v", obs, err)
	}
	res, err := c.Act(ctx, "main", proto.Action{Kind: proto.ActNavigate, URL: "https://example.test", TimeoutMS: 500})
	if err != nil || !res.OK || !res.Navigated {
		t.Fatalf("act = %+v, %v", res, err)
	}
	shot, err := c.Screenshot(ctx, "main")
	if err != nil || len(shot) != 3 {
		t.Fatalf("screenshot = %v, %v", shot, err)
	}
	if err := c.Close(ctx, "main"); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestDownload(t *testing.T) {
	t.Parallel()
	f := &fakeSidecar{t: t}
	c := newClient(t, f.routes())

	rc, err := c.Download(context.Background(), "main", "d1")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if string(body) != "file body" {
		t.Fatalf("body = %q", body)
	}

	if _, err := c.Download(context.Background(), "main", "nope"); !isCode(err, proto.ErrBadAction) {
		t.Fatalf("unknown id: err = %v", err)
	}
}

// TestErrors proves the closed error vocabulary survives the wire, and that a
// body that is not one still becomes a code the executor can act on.
func TestErrors(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+proto.PathAct, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, proto.Error{Code: proto.ErrHeld, Detail: "a person holds the browser"})
	})
	mux.HandleFunc("POST "+proto.PathObserve, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "<html>panic</html>", http.StatusBadGateway)
	})
	mux.HandleFunc("POST "+proto.PathOpen, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, "not a session")
	})
	c := newClient(t, mux)
	ctx := context.Background()

	// A coded failure of an action is an answer (browser.Sidecar): OK=false
	// with the code, and no error.
	if res, err := c.Act(ctx, "main", proto.Action{Kind: proto.ActClick}); err != nil || res.OK || res.Code != proto.ErrHeld {
		t.Fatalf("act: res = %+v, err = %v", res, err)
	}
	if _, err := c.Observe(ctx, "main", proto.ObserveRequest{}); !isCode(err, proto.ErrBrowser) {
		t.Fatalf("observe: err = %v", err)
	}
	_, err := c.Open(ctx, "main", proto.OpenRequest{})
	if err == nil {
		t.Fatal("open: want an error for a malformed body")
	}
	// The message names the call, not the body.
	if got := err.Error(); got != "browser client: /sessions/{agent}/open: malformed response" {
		t.Fatalf("open: err = %q", got)
	}
}

func TestNoSocket(t *testing.T) {
	t.Parallel()
	c := New(filepath.Join(t.TempDir(), "missing.sock"), Options{Timeout: time.Second})
	if _, err := c.Health(context.Background()); err == nil {
		t.Fatal("want an error when nothing is listening")
	}
	if _, err := c.Stream(context.Background(), "main"); err == nil {
		t.Fatal("stream: want an error when nothing is listening")
	}
}

// TestStream runs the live view end to end: meta and frames out, acks and
// input in.
func TestStream(t *testing.T) {
	t.Parallel()
	got := make(chan proto.StreamMessage, 8)
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+proto.PathStream, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		meta := proto.StreamMessage{Type: proto.StreamMeta, Width: 1280, Height: 800,
			Tabs: []proto.Tab{{ID: "t1", Active: true}}}
		raw, _ := json.Marshal(meta)
		if conn.Write(ctx, websocket.MessageText, raw) != nil {
			return
		}
		if conn.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3}) != nil {
			return
		}
		for {
			typ, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			var m proto.StreamMessage
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			got <- m
		}
	})
	c := newClient(t, mux)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := c.Stream(ctx, "main")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	first := <-s.Frames()
	if first.Meta == nil || first.Meta.Width != 1280 || len(first.Meta.Tabs) != 1 {
		t.Fatalf("first = %+v", first)
	}
	second := <-s.Frames()
	if len(second.JPEG) != 3 || second.Meta != nil {
		t.Fatalf("second = %+v", second)
	}
	if err := s.Ack(ctx); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if m := <-got; m.Type != proto.StreamAck {
		t.Fatalf("server got %+v", m)
	}
	if err := s.Send(ctx, proto.StreamMessage{Type: proto.StreamKey, Event: "down", Key: "a"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if m := <-got; m.Type != proto.StreamKey || m.Key != "a" {
		t.Fatalf("server got %+v", m)
	}
}

// TestStreamClosed proves the frame channel closes when the sidecar goes away.
func TestStreamClosed(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+proto.PathStream, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "")
	})
	c := newClient(t, mux)
	s, err := c.Stream(context.Background(), "main")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	select {
	case _, ok := <-s.Frames():
		if ok {
			t.Fatal("want a closed channel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("frames channel never closed")
	}
	_ = s.Close()
}

// TestStreamRefused decodes the sidecar's error from a refused upgrade.
func TestStreamRefused(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+proto.PathStream, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, proto.Error{Code: proto.ErrNoSession})
	})
	c := newClient(t, mux)
	if _, err := c.Stream(context.Background(), "main"); !isCode(err, proto.ErrNoSession) {
		t.Fatalf("err = %v", err)
	}
}

func isCode(err error, code proto.ErrorCode) bool {
	var pe *proto.Error
	return errors.As(err, &pe) && pe.Code == code
}
