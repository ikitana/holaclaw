package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/event"
)

// fakeToken is distinctive so tests can assert it never reaches a log or an
// error message.
const fakeToken = "424242:AAHsUpErSeCrEtToKeNvAlUe-do-not-log"

// call is one recorded request to the fake Bot API.
type call struct {
	Method   string
	Body     map[string]any    // JSON calls
	Fields   map[string]string // multipart calls
	FileName string
	File     []byte
}

// str reads a string field from either encoding.
func (c call) str(key string) string {
	if v, ok := c.Fields[key]; ok {
		return v
	}
	if v, ok := c.Body[key].(string); ok {
		return v
	}
	return ""
}

// fakeErr is a scripted Bot API failure.
type fakeErr struct {
	Status      int
	Description string
	RetryAfter  int
}

// reply is what a handler returns: a result, or an error, or both nil for
// `{"ok":true,"result":true}`.
type reply struct {
	Result any
	Err    *fakeErr
}

// fakeAPI is a scriptable Bot API over httptest. Handlers are per method; a
// queued script is popped one entry per call and the last entry repeats.
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	calls   []call
	scripts map[string][]reply
	handler map[string]func(c call) reply
	files   map[string][]byte
}

func newFake(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{
		t:       t,
		scripts: map[string][]reply{},
		handler: map[string]func(call) reply{},
		files:   map[string][]byte{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	// Every bot needs an identity.
	f.on("getMe", wireUser{ID: 99, IsBot: true, Username: "holaclaw"})
	return f
}

// on scripts one or more replies for a method. Values that are not *fakeErr or
// reply are sent as the result.
func (f *fakeAPI) on(method string, replies ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var rs []reply
	for _, r := range replies {
		switch v := r.(type) {
		case reply:
			rs = append(rs, v)
		case *fakeErr:
			rs = append(rs, reply{Err: v})
		default:
			rs = append(rs, reply{Result: v})
		}
	}
	f.scripts[method] = rs
}

// onFunc installs a dynamic handler, which wins over a script.
func (f *fakeAPI) onFunc(method string, fn func(c call) reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler[method] = fn
}

// file registers downloadable content at a Bot API file path.
func (f *fakeAPI) file(path string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = data
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if strings.HasPrefix(path, "file/bot") {
		rest := strings.TrimPrefix(path, "file/bot")
		i := strings.IndexByte(rest, '/')
		if i < 0 || rest[:i] != fakeToken {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		data, ok := f.files[rest[i+1:]]
		f.mu.Unlock()
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		_, _ = w.Write(data)
		return
	}
	if !strings.HasPrefix(path, "bot"+fakeToken+"/") {
		http.Error(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	c := call{Method: strings.TrimPrefix(path, "bot"+fakeToken+"/")}
	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/") {
		c.Fields = map[string]string{}
		if err := r.ParseMultipartForm(64 << 20); err == nil {
			for k, v := range r.MultipartForm.Value {
				c.Fields[k] = v[0]
			}
			for _, fhs := range r.MultipartForm.File {
				fh := fhs[0]
				c.FileName = fh.Filename
				if fd, err := fh.Open(); err == nil {
					c.File, _ = io.ReadAll(fd)
					_ = fd.Close()
				}
			}
		}
	} else {
		body, _ := io.ReadAll(r.Body)
		if len(body) > 0 {
			_ = json.Unmarshal(body, &c.Body)
		}
	}

	f.mu.Lock()
	f.calls = append(f.calls, c)
	fn := f.handler[c.Method]
	var rep reply
	if fn == nil {
		if rs := f.scripts[c.Method]; len(rs) > 0 {
			rep = rs[0]
			if len(rs) > 1 {
				f.scripts[c.Method] = rs[1:]
			}
		}
	}
	f.mu.Unlock()
	if fn != nil {
		rep = fn(c)
	}

	if rep.Err != nil {
		e := rep.Err
		out := map[string]any{"ok": false, "error_code": e.Status, "description": e.Description}
		if e.RetryAfter > 0 {
			out["parameters"] = map[string]any{"retry_after": e.RetryAfter}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(e.Status)
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	res := rep.Result
	if res == nil {
		res = true
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": res})
}

// seen returns every recorded call to a method.
func (f *fakeAPI) seen(method string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeAPI) count(method string) int { return len(f.seen(method)) }

// waitFor polls until cond holds or the deadline passes.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// memState is an in-memory channel.StateStore.
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
	defer s.mu.Unlock()
	s.m[k] = v
	return nil
}

func (s *memState) get(k string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[k]
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

// newAdapter builds an Adapter pointed at the fake, with a logger whose output
// tests can inspect for leaked tokens.
func newAdapter(t *testing.T, f *fakeAPI, tweak func(*Options)) (*Adapter, *memState, *logSink) {
	t.Helper()
	st := newMemState()
	sink := newLogSink()
	opts := Options{
		Name:        "tg-test",
		Token:       func() string { return fakeToken },
		BaseURL:     f.srv.URL,
		State:       st,
		Logger:      sink.logger(),
		PollTimeout: 10 * time.Millisecond,
		AlbumWait:   30 * time.Millisecond,
		PutBlob: func(_ context.Context, r io.Reader, _ string) (string, int64, error) {
			b, err := io.ReadAll(r)
			return "blob-" + string(rune('a'+len(b)%26)), int64(len(b)), err
		},
	}
	if tweak != nil {
		tweak(&opts)
	}
	return New(opts), st, sink
}

// logSink captures slog output so tests can assert the token never appears.
type logSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func newLogSink() *logSink { return &logSink{} }

func (s *logSink) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(s, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (s *logSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *logSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// update helpers keep the test tables short.
func msgUpdate(id int64, m *wireMessage) wireUpdate {
	return wireUpdate{UpdateID: id, Message: m}
}

func dmMessage(id int64, text string) *wireMessage {
	return &wireMessage{
		MessageID: id,
		From:      &wireUser{ID: 7, Username: "angel", FirstName: "Angel"},
		Chat:      wireChat{ID: 1234, Type: "private"},
		Date:      1700000000,
		Text:      text,
	}
}
