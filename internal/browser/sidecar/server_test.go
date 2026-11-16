package sidecar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"

	"holaclaw/internal/browser/proto"
)

// --- a fake engine -----------------------------------------------------------

// fakeEngine answers with whatever the test put in it. err, when set, is
// returned by every call that can fail.
type fakeEngine struct {
	health   proto.Health
	sessions []proto.Session
	session  proto.Session
	obs      proto.Observation
	act      proto.ActResult
	shot     []byte
	download proto.Download
	body     string
	err      error

	mu       sync.Mutex
	opened   []proto.OpenRequest
	closed   []string
	acted    []proto.Action
	observed []proto.ObserveRequest
	agents   []string
	deleted  bool
	shutdown bool

	// stream is the live view the test drives; in receives what the viewer
	// sent, and stream returns when the func returns.
	stream func(ctx context.Context, in <-chan proto.StreamMessage, v Viewer) error
}

var _ Engine = (*fakeEngine)(nil)

func (f *fakeEngine) note(agent string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents = append(f.agents, agent)
}

func (f *fakeEngine) Health(context.Context) proto.Health      { return f.health }
func (f *fakeEngine) Sessions(context.Context) []proto.Session { return f.sessions }

func (f *fakeEngine) Open(_ context.Context, agent string, req proto.OpenRequest) (proto.Session, error) {
	f.note(agent)
	f.mu.Lock()
	f.opened = append(f.opened, req)
	f.mu.Unlock()
	if f.err != nil {
		return proto.Session{}, f.err
	}
	return f.session, nil
}

func (f *fakeEngine) Close(_ context.Context, agent string) error {
	f.note(agent)
	f.mu.Lock()
	f.closed = append(f.closed, agent)
	f.mu.Unlock()
	return f.err
}

func (f *fakeEngine) Observe(_ context.Context, agent string, req proto.ObserveRequest) (proto.Observation, error) {
	f.note(agent)
	f.mu.Lock()
	f.observed = append(f.observed, req)
	f.mu.Unlock()
	if f.err != nil {
		return proto.Observation{}, f.err
	}
	return f.obs, nil
}

func (f *fakeEngine) Act(_ context.Context, agent string, a proto.Action) (proto.ActResult, error) {
	f.note(agent)
	f.mu.Lock()
	f.acted = append(f.acted, a)
	f.mu.Unlock()
	if f.err != nil {
		return proto.ActResult{}, f.err
	}
	return f.act, nil
}

func (f *fakeEngine) Screenshot(_ context.Context, agent string) ([]byte, error) {
	f.note(agent)
	if f.err != nil {
		return nil, f.err
	}
	return f.shot, nil
}

func (f *fakeEngine) Download(_ context.Context, agent, _ string) (io.ReadCloser, proto.Download, error) {
	f.note(agent)
	if f.err != nil {
		return nil, proto.Download{}, f.err
	}
	return &markCloser{Reader: strings.NewReader(f.body), f: f}, f.download, nil
}

func (f *fakeEngine) Stream(ctx context.Context, agent string, in <-chan proto.StreamMessage, v Viewer) error {
	f.note(agent)
	if f.stream == nil {
		return f.err
	}
	return f.stream(ctx, in, v)
}

func (f *fakeEngine) Shutdown(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shutdown = true
	return nil
}

// markCloser records that the quarantined file was released.
type markCloser struct {
	io.Reader
	f *fakeEngine
}

func (c *markCloser) Close() error {
	c.f.mu.Lock()
	defer c.f.mu.Unlock()
	c.f.deleted = true
	return nil
}

// --- helpers -----------------------------------------------------------------

func newTestServer(t *testing.T, f *fakeEngine, opts Options) (*Server, *httptest.Server) {
	t.Helper()
	s := New(f, opts)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func do(t *testing.T, ts *httptest.Server, method, path, body string) *http.Response {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, ts.URL+path, r)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeInto(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

// --- happy paths --------------------------------------------------------------

func TestHealth(t *testing.T) {
	f := &fakeEngine{health: proto.Health{Version: "test", Binary: "/usr/bin/chromium", Browser: "153.0"}}
	_, ts := newTestServer(t, f, Options{})
	resp := do(t, ts, http.MethodGet, proto.PathHealth, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var h proto.Health
	decodeInto(t, resp, &h)
	if h.Binary != "/usr/bin/chromium" || h.Version != "test" {
		t.Errorf("health = %+v", h)
	}
}

func TestSessionsIsAlwaysAList(t *testing.T) {
	_, ts := newTestServer(t, &fakeEngine{}, Options{})
	resp := do(t, ts, http.MethodGet, proto.PathSessions, "")
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "[]" {
		t.Errorf("empty sessions = %q, want []", body)
	}
}

func TestOpenCloseObserve(t *testing.T) {
	f := &fakeEngine{
		session: proto.Session{Agent: "alice", Tabs: []proto.Tab{{ID: "t1", URL: "about:blank", Active: true}}},
		obs:     proto.Observation{URL: "https://example.test", Title: "t", Total: 3},
	}
	_, ts := newTestServer(t, f, Options{})

	resp := do(t, ts, http.MethodPost, "/sessions/alice/open", `{"allow":["example.test"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open status = %d", resp.StatusCode)
	}
	var sess proto.Session
	decodeInto(t, resp, &sess)
	if sess.Agent != "alice" || len(sess.Tabs) != 1 {
		t.Errorf("session = %+v", sess)
	}
	if len(f.opened) != 1 || len(f.opened[0].Allow) != 1 || f.opened[0].Allow[0] != "example.test" {
		t.Errorf("allow list did not reach the engine: %+v", f.opened)
	}

	resp = do(t, ts, http.MethodPost, "/sessions/alice/observe", `{"hints":["search"],"text_bytes":100}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("observe status = %d", resp.StatusCode)
	}
	var obs proto.Observation
	decodeInto(t, resp, &obs)
	if obs.Total != 3 {
		t.Errorf("observation = %+v", obs)
	}
	if len(f.observed) != 1 || f.observed[0].TextBytes != 100 {
		t.Errorf("observe request = %+v", f.observed)
	}

	resp = do(t, ts, http.MethodPost, "/sessions/alice/close", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("close status = %d", resp.StatusCode)
	}
	if len(f.closed) != 1 {
		t.Errorf("close did not reach the engine")
	}
}

// A refused action is a 200 with ok=false: the executor reads the code.
func TestActFailureIsStill200(t *testing.T) {
	f := &fakeEngine{act: proto.ActResult{OK: false, Code: proto.ErrStaleRef, URL: "https://example.test"}}
	_, ts := newTestServer(t, f, Options{})
	resp := do(t, ts, http.MethodPost, "/sessions/alice/act", `{"kind":"click","ref":"r1"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var res proto.ActResult
	decodeInto(t, resp, &res)
	if res.OK || res.Code != proto.ErrStaleRef {
		t.Errorf("result = %+v", res)
	}
	if len(f.acted) != 1 || f.acted[0].Kind != proto.ActClick {
		t.Errorf("action = %+v", f.acted)
	}
}

func TestScreenshot(t *testing.T) {
	f := &fakeEngine{shot: []byte{0xff, 0xd8, 0xff, 0xe0, 0x00}}
	_, ts := newTestServer(t, f, Options{})
	resp := do(t, ts, http.MethodPost, "/sessions/alice/screenshot", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("content type = %q", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, f.shot) {
		t.Errorf("body = %x", body)
	}
}

func TestDownloadStreamsAndDeletes(t *testing.T) {
	f := &fakeEngine{
		body:     "hello quarantine",
		download: proto.Download{ID: "d1", Name: `../../étrange "report".pdf`, Size: 16},
	}
	_, ts := newTestServer(t, f, Options{})
	resp := do(t, ts, http.MethodGet, "/sessions/alice/downloads/d1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("content type = %q", got)
	}
	if got := resp.Header.Get("Content-Length"); got != "16" {
		t.Errorf("content length = %q", got)
	}
	cd := resp.Header.Get("Content-Disposition")
	if strings.ContainsAny(cd, "/\\") || strings.Contains(cd, "..") {
		t.Errorf("content disposition was not sanitised: %q", cd)
	}
	if !strings.Contains(cd, ".pdf") {
		t.Errorf("content disposition lost the extension: %q", cd)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello quarantine" {
		t.Errorf("body = %q", body)
	}
	if !f.deleted {
		t.Error("the quarantined file was not released")
	}
}

func TestDisposition(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"report.pdf", `attachment; filename="report.pdf"`},
		{"", `attachment; filename="download"`},
		{"../../etc/passwd", `attachment; filename="passwd"`},
		{`a"b;c.txt`, `attachment; filename="a_b_c.txt"`},
		{"...", `attachment; filename="download"`},
	} {
		if got := disposition(tc.in); got != tc.want {
			t.Errorf("disposition(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	long := disposition(strings.Repeat("x", 500))
	if len(long) > maxDispositionName+40 {
		t.Errorf("long name was not capped: %d bytes", len(long))
	}
}

// --- failure paths -------------------------------------------------------------

func TestErrorCodesMapToStatus(t *testing.T) {
	cases := []struct {
		code   proto.ErrorCode
		status int
	}{
		{proto.ErrNoSession, http.StatusNotFound},
		{proto.ErrBadAction, http.StatusBadRequest},
		{proto.ErrNotAllowed, http.StatusForbidden},
		{proto.ErrStaleRef, http.StatusConflict},
		{proto.ErrHeld, http.StatusConflict},
		{proto.ErrTimeout, http.StatusGatewayTimeout},
		{proto.ErrBrowser, http.StatusBadGateway},
	}
	for _, tc := range cases {
		f := &fakeEngine{err: Errorf(tc.code, "detail")}
		_, ts := newTestServer(t, f, Options{})
		resp := do(t, ts, http.MethodPost, "/sessions/alice/observe", `{}`)
		if resp.StatusCode != tc.status {
			t.Errorf("%s: status = %d, want %d", tc.code, resp.StatusCode, tc.status)
		}
		var e proto.Error
		decodeInto(t, resp, &e)
		if e.Code != tc.code || e.Detail != "detail" {
			t.Errorf("%s: body = %+v", tc.code, e)
		}
	}
}

// A plain Go error may quote the page: it must never reach the wire.
func TestPlainErrorIsAFixed500(t *testing.T) {
	f := &fakeEngine{err: errors.New("cdp: <script>alert('secret page text')</script>")}
	_, ts := newTestServer(t, f, Options{})
	resp := do(t, ts, http.MethodPost, "/sessions/alice/observe", "{}")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "secret page text") {
		t.Fatalf("the Go error text reached the wire: %s", body)
	}
	var e proto.Error
	if err := json.Unmarshal(body, &e); err != nil || e.Code != proto.ErrBrowser {
		t.Errorf("body = %s", body)
	}
}

func TestBadAgentNameIsRejectedBeforeTheEngine(t *testing.T) {
	for _, name := range []string{"Alice", "alice!", "_alice", "-alice", strings.Repeat("a", 200)} {
		f := &fakeEngine{}
		_, ts := newTestServer(t, f, Options{})
		resp := do(t, ts, http.MethodPost, "/sessions/"+name+"/act", `{"kind":"click"}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q: status = %d, want 400", name, resp.StatusCode)
		}
		var e proto.Error
		decodeInto(t, resp, &e)
		if e.Code != proto.ErrBadAction {
			t.Errorf("%q: code = %s", name, e.Code)
		}
		if len(f.agents) != 0 {
			t.Errorf("%q reached the engine", name)
		}
	}
}

func TestBodyCap(t *testing.T) {
	f := &fakeEngine{}
	_, ts := newTestServer(t, f, Options{})
	big := `{"hints":["` + strings.Repeat("x", MaxJSONBody+1024) + `"]}`
	resp := do(t, ts, http.MethodPost, "/sessions/alice/observe", big)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if len(f.observed) != 0 {
		t.Error("an oversized body reached the engine")
	}
}

func TestMalformedBody(t *testing.T) {
	f := &fakeEngine{}
	_, ts := newTestServer(t, f, Options{})
	resp := do(t, ts, http.MethodPost, "/sessions/alice/act", `{"kind":`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if len(f.acted) != 0 {
		t.Error("a malformed body reached the engine")
	}
}

func TestEmptyBodyIsTheZeroValue(t *testing.T) {
	f := &fakeEngine{}
	_, ts := newTestServer(t, f, Options{})
	if resp := do(t, ts, http.MethodPost, "/sessions/alice/open", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(f.opened) != 1 || f.opened[0].Allow != nil {
		t.Errorf("open request = %+v", f.opened)
	}
}

func TestUnknownRouteAndMethod(t *testing.T) {
	_, ts := newTestServer(t, &fakeEngine{}, Options{})
	if resp := do(t, ts, http.MethodGet, "/nope", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown path: status = %d", resp.StatusCode)
	}
	if resp := do(t, ts, http.MethodGet, "/sessions/alice/act", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("wrong method: status = %d", resp.StatusCode)
	}
}

// --- the live view ---------------------------------------------------------------

// dialStream opens the live view of agent over ts.
func dialStream(t *testing.T, ts *httptest.Server, agent string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/sessions/" + agent + "/stream"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPClient: ts.Client()})
	if err != nil {
		t.Fatalf("dial stream: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func ack(t *testing.T, c *websocket.Conn) {
	t.Helper()
	body, err := json.Marshal(proto.StreamMessage{Type: proto.StreamAck})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, body); err != nil {
		t.Fatalf("write ack: %v", err)
	}
}

// The whole round trip: meta out, frames out, acks in, input in.
func TestStreamFramesMetaAndInput(t *testing.T) {
	framed := make(chan error, 2)
	gotInput := make(chan proto.StreamMessage, 4)
	done := make(chan struct{})
	f := &fakeEngine{stream: func(ctx context.Context, in <-chan proto.StreamMessage, v Viewer) error {
		defer close(done)
		if err := v.Meta(ctx, proto.StreamMessage{Type: proto.StreamMeta, Width: 1280, Height: 800}); err != nil {
			return err
		}
		framed <- v.Frame(ctx, []byte("jpeg-1"), 1280, 800)
		framed <- v.Frame(ctx, []byte("jpeg-2"), 1280, 800)
		select {
		case m := <-in:
			gotInput <- m
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	}}
	_, ts := newTestServer(t, f, Options{AckTimeout: 2 * time.Second})
	c := dialStream(t, ts, "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	typ, data, err := c.Read(ctx)
	if err != nil || typ != websocket.MessageText {
		t.Fatalf("meta: type %v err %v", typ, err)
	}
	var meta proto.StreamMessage
	if err := json.Unmarshal(data, &meta); err != nil || meta.Width != 1280 {
		t.Fatalf("meta = %s (%v)", data, err)
	}

	for i, want := range []string{"jpeg-1", "jpeg-2"} {
		typ, data, err := c.Read(ctx)
		if err != nil || typ != websocket.MessageBinary {
			t.Fatalf("frame %d: type %v err %v", i, typ, err)
		}
		if string(data) != want {
			t.Fatalf("frame %d = %q", i, data)
		}
		ack(t, c)
		if err := <-framed; err != nil {
			t.Fatalf("Frame %d returned %v", i, err)
		}
	}

	// A typed key reaches the engine's input channel.
	body, err := json.Marshal(proto.StreamMessage{Type: proto.StreamKey, Event: "down", Key: "a", Code: "KeyA"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageText, body); err != nil {
		t.Fatalf("write input: %v", err)
	}
	select {
	case m := <-gotInput:
		if m.Type != proto.StreamKey || m.Key != "a" {
			t.Errorf("input = %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("input never reached the engine")
	}
	<-done
}

// Frame is the backpressure: it must not return before the ack.
func TestFrameBlocksUntilAck(t *testing.T) {
	returned := make(chan struct{})
	f := &fakeEngine{stream: func(ctx context.Context, _ <-chan proto.StreamMessage, v Viewer) error {
		if err := v.Frame(ctx, []byte("jpeg"), 100, 100); err != nil {
			return err
		}
		close(returned)
		<-ctx.Done()
		return nil
	}}
	_, ts := newTestServer(t, f, Options{AckTimeout: 2 * time.Second})
	c := dialStream(t, ts, "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	select {
	case <-returned:
		t.Fatal("Frame returned before the ack")
	case <-time.After(150 * time.Millisecond):
	}
	ack(t, c)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("Frame never returned after the ack")
	}
}

// A viewer that never acknowledges loses the stream.
func TestAckTimeoutClosesTheStream(t *testing.T) {
	failed := make(chan error, 1)
	f := &fakeEngine{stream: func(ctx context.Context, _ <-chan proto.StreamMessage, v Viewer) error {
		err := v.Frame(ctx, []byte("jpeg"), 100, 100)
		failed <- err
		return err
	}}
	_, ts := newTestServer(t, f, Options{AckTimeout: 100 * time.Millisecond})
	c := dialStream(t, ts, "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	select {
	case err := <-failed:
		var pe *proto.Error
		if !errors.As(err, &pe) || pe.Code != proto.ErrTimeout {
			t.Fatalf("Frame error = %v, want a timeout", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Frame never timed out")
	}
	// The connection is gone from the viewer's side too.
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("the stream stayed open after the ack timeout")
	}
}

func TestStreamMalformedMessageClosesWith1003(t *testing.T) {
	f := &fakeEngine{stream: func(ctx context.Context, _ <-chan proto.StreamMessage, _ Viewer) error {
		<-ctx.Done()
		return nil
	}}
	_, ts := newTestServer(t, f, Options{})
	c := dialStream(t, ts, "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte("{not json")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err := c.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusUnsupportedData {
		t.Fatalf("close status = %v (err %v), want 1003", websocket.CloseStatus(err), err)
	}
}

// An unknown type is ignored, so a newer viewer cannot break this sidecar.
func TestStreamIgnoresUnknownTypes(t *testing.T) {
	seen := make(chan proto.StreamMessage, 4)
	f := &fakeEngine{stream: func(ctx context.Context, in <-chan proto.StreamMessage, _ Viewer) error {
		for {
			select {
			case m := <-in:
				seen <- m
			case <-ctx.Done():
				return nil
			}
		}
	}}
	_, ts := newTestServer(t, f, Options{})
	c := dialStream(t, ts, "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, body := range []string{`{"type":"telepathy"}`, `{"type":"meta","width":9}`, `{"type":"tab","tab":"t2"}`} {
		if err := c.Write(ctx, websocket.MessageText, []byte(body)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	select {
	case m := <-seen:
		if m.Type != proto.StreamTab || m.Tab != "t2" {
			t.Fatalf("first message through = %+v, want only the tab", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing reached the engine")
	}
	select {
	case m := <-seen:
		t.Fatalf("an ignored type reached the engine: %+v", m)
	case <-time.After(100 * time.Millisecond):
	}
}

// Input is never logged, in whole or in part, and the stream's access line
// carries no agent.
func TestStreamIsNeverLogged(t *testing.T) {
	const marker = "hunter2-correct-horse"
	var buf lockedBuffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	got := make(chan proto.StreamMessage, 1)
	f := &fakeEngine{stream: func(ctx context.Context, in <-chan proto.StreamMessage, _ Viewer) error {
		select {
		case m := <-in:
			got <- m
		case <-ctx.Done():
		}
		return nil
	}}
	_, ts := newTestServer(t, f, Options{Logger: log})
	c := dialStream(t, ts, "alice")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	body, err := json.Marshal(proto.StreamMessage{Type: proto.StreamText, Text: marker})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageText, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case m := <-got:
		if m.Text != marker {
			t.Fatalf("text = %q", m.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("input never arrived")
	}
	_ = c.Close(websocket.StatusNormalClosure, "")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(buf.String(), "/stream") {
		time.Sleep(10 * time.Millisecond)
	}
	out := buf.String()
	if !strings.Contains(out, "/stream") {
		t.Fatalf("the stream request was not logged at all:\n%s", out)
	}
	if strings.Contains(out, marker) {
		t.Fatalf("typed text reached the log:\n%s", out)
	}
	if strings.Contains(out, "agent=") {
		t.Fatalf("the stream line named the agent:\n%s", out)
	}
}

// Everything but the stream logs the agent, and no body ever appears.
func TestAccessLogHasAgentAndNoBody(t *testing.T) {
	var buf lockedBuffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	f := &fakeEngine{}
	_, ts := newTestServer(t, f, Options{Logger: log})
	do(t, ts, http.MethodPost, "/sessions/alice/observe", `{"hints":["secret-hint-marker"]}`)
	out := buf.String()
	if !strings.Contains(out, "agent=alice") || !strings.Contains(out, "status=200") {
		t.Errorf("access line = %q", out)
	}
	if strings.Contains(out, "secret-hint-marker") {
		t.Errorf("a body reached the log: %q", out)
	}
}

// lockedBuffer is a log sink two goroutines may touch.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Off the socket the live view is refused: it is not an internet endpoint.
func TestStreamRefusedOffTheSocket(t *testing.T) {
	s := New(&fakeEngine{}, Options{})
	s.requireSocket = true
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp := do(t, ts, http.MethodGet, "/sessions/alice/stream", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	var e proto.Error
	decodeInto(t, resp, &e)
	if e.Code != proto.ErrNotAllowed {
		t.Errorf("code = %s", e.Code)
	}
}

// --- the socket ----------------------------------------------------------------

// socketDir is a short temporary directory for a socket: the path is bounded
// by the kernel (108 bytes), so it cannot live under the test's name.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "hb")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestServeSocketPermissionsAndShutdown(t *testing.T) {
	dir := socketDir(t)
	sock := filepath.Join(dir, "run", "browser.sock")
	f := &fakeEngine{health: proto.Health{Version: "test"}}
	bound := make(chan struct{})
	s := New(f, Options{OnListen: func(net.Addr) { close(bound) }})

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ctx, sock) }()

	select {
	case <-bound:
	case err := <-errc:
		t.Fatalf("Serve returned early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("never bound")
	}

	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o660 {
		t.Errorf("socket mode = %#o, want 0660", perm)
	}

	// It answers over the socket.
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://sidecar"+proto.PathHealth, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("health over the socket: %v", err)
	}
	var h proto.Health
	decodeInto(t, resp, &h)
	_ = resp.Body.Close()
	if h.Version != "test" {
		t.Errorf("health = %+v", h)
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not stop")
	}
	if !f.shutdown {
		t.Error("the engine was not shut down")
	}
}

func TestServeReplacesAStaleSocket(t *testing.T) {
	dir := socketDir(t)
	sock := filepath.Join(dir, "browser.sock")
	// A socket file nobody listens on: exactly what a crash leaves behind.
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := os.WriteFile(sock, nil, 0o600); err == nil {
		// Some platforms let the close remove it; recreate the stale file.
		t.Log("socket file recreated as a plain file")
	}
	// Recreate it as a real socket left behind.
	_ = os.Remove(sock)
	ln2, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	if uc, ok := ln2.(*net.UnixListener); ok {
		uc.SetUnlinkOnClose(false)
	}
	_ = ln2.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket did not survive: %v", err)
	}

	bound := make(chan struct{})
	s := New(&fakeEngine{}, Options{OnListen: func(net.Addr) { close(bound) }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- s.Serve(ctx, sock) }()
	select {
	case <-bound:
	case err := <-errc:
		t.Fatalf("Serve refused a stale socket: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("never bound")
	}
}

func TestListenSocketRefusesALiveSocket(t *testing.T) {
	dir := socketDir(t)
	sock := filepath.Join(dir, "browser.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if _, err := listenSocket(sock); err == nil {
		t.Fatal("listenSocket stole a live socket")
	}
}

// The socket is never reachable by anyone but the owner and the group, not
// even for the instant between bind and chmod: a permissive umask must not
// change what the kernel creates. A watcher stats the path throughout, so a
// socket born 0755 would be caught even if chmod tidied it afterwards.
func TestListenSocketIsNeverWorldReachable(t *testing.T) {
	dir := socketDir(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "browser.sock")

	stop := make(chan struct{})
	seen := make(chan os.FileMode, 1)
	go func() {
		for {
			select {
			case <-stop:
				close(seen)
				return
			default:
			}
			if info, err := os.Lstat(sock); err == nil && info.Mode().Perm()&0o007 != 0 {
				select {
				case seen <- info.Mode().Perm():
				default:
				}
			}
		}
	}()

	umaskMu.Lock()
	restore := syscall.Umask(0) // the worst case: nothing is masked off
	umaskMu.Unlock()
	ln, err := listenSocket(sock)
	close(stop)
	// The process umask is the process's: listenSocket puts back what it
	// found, so this still reads the 0 set above.
	umaskMu.Lock()
	after := syscall.Umask(restore)
	umaskMu.Unlock()
	if after != 0 {
		t.Errorf("umask after listenSocket = %#o, want the caller's 0", after)
	}
	if err != nil {
		t.Fatalf("listenSocket: %v", err)
	}
	defer func() { _ = ln.Close() }()

	if perm, ok := <-seen; ok {
		t.Errorf("the socket was reachable by others: mode %#o", perm)
	}
	info, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o660 {
		t.Errorf("socket mode = %#o, want 0660", perm)
	}
}

func TestListenSocketRefusesANonSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "not-a-socket")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listenSocket(path); err == nil {
		t.Fatal("listenSocket bound over a regular file")
	}
}

// An action that failed for a protocol reason is an answer, not an error
// status: the executor acts on the code.
func TestActCodedFailureIs200(t *testing.T) {
	f := &fakeEngine{err: Errorf(proto.ErrStaleRef, "detail")}
	_, ts := newTestServer(t, f, Options{})
	resp := do(t, ts, http.MethodPost, "/sessions/alice/act", `{"kind":"click"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var res proto.ActResult
	decodeInto(t, resp, &res)
	if res.OK || res.Code != proto.ErrStaleRef {
		t.Fatalf("result = %+v, want OK=false code stale_ref", res)
	}
}
