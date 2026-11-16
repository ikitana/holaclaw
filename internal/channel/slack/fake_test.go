package slack

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

// Distinctive fake credentials: no test may ever find these in an error or a
// log line.
const (
	fakeBotToken = "xoxb-BOTSECRET-9f3a2b1c"
	fakeAppToken = "xapp-APPSECRET-4d7e8a6b"
)

// apiCall is one recorded Web API request.
type apiCall struct {
	Method string
	Auth   string
	Body   string
	Form   url.Values
	JSON   map[string]any
}

// fakeSlack is an in-process Slack: an httptest Web API plus a websocket
// endpoint whose envelopes and acks the test drives.
type fakeSlack struct {
	t   *testing.T
	api *httptest.Server
	ws  *httptest.Server

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	calls    []apiCall
	handlers map[string]http.HandlerFunc

	conns chan *fakeConn
}

// fakeConn is one accepted Socket Mode connection.
type fakeConn struct {
	c    *websocket.Conn
	acks chan string
	done chan struct{}
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeSlack{
		t:        t,
		ctx:      ctx,
		cancel:   cancel,
		handlers: map[string]http.HandlerFunc{},
		conns:    make(chan *fakeConn, 8),
	}
	f.api = httptest.NewServer(http.HandlerFunc(f.serveAPI))
	f.ws = httptest.NewServer(http.HandlerFunc(f.serveWS))
	t.Cleanup(func() {
		cancel()
		f.api.Close()
		f.ws.Close()
	})
	return f
}

// set installs a handler for one Web API method (or upload path).
func (f *fakeSlack) set(method string, h http.HandlerFunc) {
	f.mu.Lock()
	f.handlers[method] = h
	f.mu.Unlock()
}

func (f *fakeSlack) serveAPI(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/")
	body, _ := io.ReadAll(r.Body)
	call := apiCall{Method: method, Auth: r.Header.Get("Authorization"), Body: string(body)}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		call.Form, _ = url.ParseQuery(string(body))
	} else {
		_ = json.Unmarshal(body, &call.JSON)
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	h := f.handlers[method]
	f.mu.Unlock()

	if h != nil {
		h(w, r)
		return
	}
	switch method {
	case "auth.test":
		f.ok(w, `{"user_id":"UBOT","team_id":"T1","bot_id":"B1"}`)
	case "apps.connections.open":
		f.ok(w, `{"url":`+jsonString("ws"+strings.TrimPrefix(f.ws.URL, "http")+"/link")+`}`)
	case "users.info":
		f.ok(w, `{"user":{"name":"alice","profile":{"display_name":"Alice"}}}`)
	case "chat.postMessage":
		f.ok(w, `{"channel":"C1","ts":"111.222"}`)
	case "chat.update":
		f.ok(w, `{"channel":"C1","ts":"111.222"}`)
	case "files.getUploadURLExternal":
		f.ok(w, `{"upload_url":`+jsonString(f.api.URL+"/upload")+`,"file_id":"F1"}`)
	case "files.completeUploadExternal":
		f.ok(w, `{"files":[{"id":"F1","shares":{"public":{"C1":[{"ts":"333.444"}]}}}]}`)
	case "upload":
		w.WriteHeader(http.StatusOK)
	default:
		f.ok(w, `{}`)
	}
}

// ok writes a successful Slack response with the given extra fields.
func (f *fakeSlack) ok(w http.ResponseWriter, extra string) {
	w.Header().Set("Content-Type", "application/json")
	body := `{"ok":true`
	if inner := strings.TrimSpace(extra); len(inner) > 2 {
		body += "," + strings.TrimSuffix(strings.TrimPrefix(inner, "{"), "}")
	}
	_, _ = io.WriteString(w, body+"}")
}

// fail writes an ok:false response.
func fail(code string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":false,"error":`+jsonString(code)+`}`)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (f *fakeSlack) serveWS(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	fc := &fakeConn{c: c, acks: make(chan string, 32), done: make(chan struct{})}
	select {
	case f.conns <- fc:
	default:
	}
	defer close(fc.done)
	for {
		_, data, err := c.Read(f.ctx)
		if err != nil {
			return
		}
		var m struct {
			EnvelopeID string `json:"envelope_id"`
		}
		_ = json.Unmarshal(data, &m)
		select {
		case fc.acks <- m.EnvelopeID:
		default:
		}
	}
}

// conn waits for the next accepted connection.
func (f *fakeSlack) conn(t *testing.T) *fakeConn {
	t.Helper()
	select {
	case c := <-f.conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no socket connection")
		return nil
	}
}

// push sends one envelope to the adapter.
func (c *fakeConn) push(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("push: %v", err)
	}
}

// ack waits for the adapter's ack of one envelope.
func (c *fakeConn) ack(t *testing.T, within time.Duration) string {
	t.Helper()
	select {
	case id := <-c.acks:
		return id
	case <-time.After(within):
		t.Fatalf("no ack within %s", within)
		return ""
	}
}

func (c *fakeConn) drop() { _ = c.c.CloseNow() }

// callsFor returns every recorded call to one method.
func (f *fakeSlack) callsFor(method string) []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []apiCall
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeSlack) waitCalls(t *testing.T, method string, n int) []apiCall {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c := f.callsFor(method); len(c) >= n {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d calls to %s, want %d", len(f.callsFor(method)), method, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// memState is an in-memory StateStore.
type memState struct {
	mu sync.Mutex
	m  map[string]string
}

func newMemState() *memState { return &memState{m: map[string]string{}} }

func (s *memState) Get(_ context.Context, k string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	return v, ok, nil
}

func (s *memState) Set(_ context.Context, k, v string) error {
	s.mu.Lock()
	s.m[k] = v
	s.mu.Unlock()
	return nil
}

// memBlobs serves attachment bytes for outbound tests.
type memBlobs struct {
	data map[string][]byte
	err  error
}

func (b memBlobs) OpenAttachment(_ context.Context, a event.Attachment) ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.data[a.BlobID], nil
}

// newAdapter builds an adapter wired to the fake, with room for per-test
// tweaks.
func newAdapter(t *testing.T, f *fakeSlack, tweak func(*Options)) (*Adapter, *strings.Builder) {
	t.Helper()
	logs := &strings.Builder{}
	o := Options{
		Name:       "slack-test",
		BotToken:   func() string { return fakeBotToken },
		AppToken:   func() string { return fakeAppToken },
		APIBaseURL: f.api.URL,
		State:      newMemState(),
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		PutBlob: func(_ context.Context, r io.Reader, _ string) (string, int64, error) {
			b, err := io.ReadAll(r)
			if err != nil {
				return "", 0, err
			}
			return "blob-" + jsonString(string(b[:min(len(b), 4)])), int64(len(b)), nil
		},
	}
	if tweak != nil {
		tweak(&o)
	}
	return New(o), logs
}

// run starts Run and returns the collected status changes plus a stop func.
func run(t *testing.T, a *Adapter, in channel.Inbound) (statuses chan statusChange, stop func() error) {
	t.Helper()
	if in == nil {
		in = func(context.Context, event.Envelope) error { return nil }
	}
	ctx, cancel := context.WithCancel(context.Background())
	st := make(chan statusChange, 32)
	errc := make(chan error, 1)
	go func() {
		errc <- a.Run(ctx, in, func(s channel.State, msg string) {
			select {
			case st <- statusChange{s, msg}:
			default:
			}
		})
	}()
	return st, func() error {
		cancel()
		select {
		case err := <-errc:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after cancel")
			return nil
		}
	}
}

type statusChange struct {
	state channel.State
	msg   string
}

// waitStatus waits for a specific lifecycle state.
func waitStatus(t *testing.T, st chan statusChange, want channel.State) statusChange {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case s := <-st:
			if s.state == want {
				return s
			}
		case <-deadline:
			t.Fatalf("status %q never seen", want)
			return statusChange{}
		}
	}
}
