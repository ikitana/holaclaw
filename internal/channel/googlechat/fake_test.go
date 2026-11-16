package googlechat

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/channel"
	"holaclaw/internal/event"
)

const (
	testProject = "proj-1"
	testEmail   = "holaclaw@proj-1.iam.gserviceaccount.com"
	testKeyID   = "kid-5f0c9a7e1d2b4c3a-do-not-log"
)

var testRSA *rsa.PrivateKey

func TestMain(m *testing.M) {
	var err error
	testRSA, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	backoffBase = time.Millisecond
	backoffMax = 5 * time.Millisecond
	loggedOutWait = 10 * time.Millisecond
	os.Exit(m.Run())
}

// pemPKCS8 is the private key as Google issues it.
func pemPKCS8(k *rsa.PrivateKey) string {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// keyJSON builds a service-account key file. token_uri points elsewhere on
// purpose: the adapter must ignore it.
func keyJSON(mod func(m map[string]any)) string {
	m := map[string]any{
		"type":           "service_account",
		"project_id":     testProject,
		"private_key_id": testKeyID,
		"private_key":    pemPKCS8(testRSA),
		"client_email":   testEmail,
		"token_uri":      "https://token.invalid/never",
	}
	if mod != nil {
		mod(m)
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// rec is one request the fake saw.
type rec struct {
	Route string
	Path  string
	Query url.Values
	Body  []byte
	Auth  string
}

func (r rec) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("%s body is not JSON: %v (%s)", r.Route, err, r.Body)
	}
	return m
}

// fakeGoogle is the token endpoint, Pub/Sub and Chat on one httptest server,
// routed by path.
type fakeGoogle struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	recs     []rec
	handlers map[string]http.HandlerFunc
	pulls    []any // queued pull answers: []string (event data) or int (status)
	media    map[string][]byte
	tokens   int
}

func newFake(t *testing.T) *fakeGoogle {
	t.Helper()
	f := &fakeGoogle{t: t, handlers: map[string]http.HandlerFunc{}, media: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// route names a request.
func route(r *http.Request) string {
	p := r.URL.Path
	sub := "/v1/projects/" + testProject + "/subscriptions/holaclaw"
	switch {
	case p == "/token":
		return "token"
	case p == sub+":pull":
		return "pull"
	case p == sub+":acknowledge":
		return "ack"
	case p == sub+":modifyAckDeadline":
		return "modack"
	case p == sub+":testIamPermissions" && r.Method == http.MethodPost:
		return "subcheck"
	case strings.HasPrefix(p, "/v1/media/"):
		return "media"
	case p == "/v1/spaces" && r.Method == http.MethodGet:
		return "spaces.list"
	case strings.HasSuffix(p, "/messages") && r.Method == http.MethodPost:
		return "create"
	case strings.Contains(p, "/messages/") && r.Method == http.MethodPatch:
		return "patch"
	case strings.Contains(p, "/messages/") && r.Method == http.MethodGet:
		return "get"
	}
	return "unknown"
}

func (f *fakeGoogle) handle(name string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[name] = h
}

// queuePull queues one pull answer: event data strings, or an HTTP status.
func (f *fakeGoogle) queuePull(v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls = append(f.pulls, v)
}

func (f *fakeGoogle) records(name string) []rec {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []rec
	for _, r := range f.recs {
		if name == "" || r.Route == name {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeGoogle) tokenCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokens
}

func (f *fakeGoogle) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	name := route(r)
	f.mu.Lock()
	f.recs = append(f.recs, rec{Route: name, Path: r.URL.Path, Query: r.URL.Query(), Body: body, Auth: r.Header.Get("Authorization")})
	h := f.handlers[name]
	f.mu.Unlock()
	if h != nil {
		h(w, r)
		return
	}
	switch name {
	case "token":
		f.mu.Lock()
		f.tokens++
		n := f.tokens
		f.mu.Unlock()
		writeJSON(w, 200, map[string]any{"access_token": fmt.Sprintf("ya29.fake-access-token-%d", n), "expires_in": 3600, "token_type": "Bearer"})
	case "pull":
		f.mu.Lock()
		var next any
		if len(f.pulls) > 0 {
			next, f.pulls = f.pulls[0], f.pulls[1:]
		}
		f.mu.Unlock()
		switch v := next.(type) {
		case nil:
			time.Sleep(5 * time.Millisecond)
			writeJSON(w, 200, map[string]any{})
		case int:
			writeJSON(w, v, googleErr(v, "scripted"))
		case []string:
			var msgs []any
			for i, d := range v {
				msgs = append(msgs, map[string]any{
					"ackId":   fmt.Sprintf("ack-%s-%d", shortHash(d), i),
					"message": map[string]any{"data": d, "messageId": fmt.Sprint(i)},
				})
			}
			writeJSON(w, 200, map[string]any{"receivedMessages": msgs})
		}
	case "ack", "modack", "patch", "spaces.list":
		writeJSON(w, 200, map[string]any{})
	case "subcheck":
		writeJSON(w, 200, map[string]any{"permissions": []string{"pubsub.subscriptions.consume"}})
	case "create":
		space := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/"), "/messages")
		writeJSON(w, 200, map[string]any{"name": space + "/messages/" + r.URL.Query().Get("messageId")})
	case "get":
		writeJSON(w, 200, map[string]any{"name": strings.TrimPrefix(r.URL.Path, "/v1/"), "createTime": "2026-10-19T10:00:00.123456Z"})
	case "media":
		f.mu.Lock()
		data, ok := f.media[strings.TrimPrefix(r.URL.Path, "/v1/media/")]
		f.mu.Unlock()
		if !ok {
			writeJSON(w, 404, googleErr(404, "no media"))
			return
		}
		_, _ = w.Write(data)
	default:
		writeJSON(w, 404, googleErr(404, "unknown route "+r.URL.Path))
	}
}

func shortHash(s string) string {
	if len(s) > 12 {
		return s[len(s)-12:]
	}
	return s
}

func googleErr(code int, msg string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": msg, "status": http.StatusText(code)}}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newAdapter builds an adapter against f with a captured debug log.
func newAdapter(t *testing.T, f *fakeGoogle, mod func(o *Options)) (*Adapter, *syncBuf) {
	t.Helper()
	logs := &syncBuf{}
	key := keyJSON(nil)
	o := Options{
		Name:    "gchat",
		Key:     func() string { return key },
		BaseURL: f.srv.URL,
		Logger:  slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		PutBlob: func(_ context.Context, r io.Reader, _ string) (string, int64, error) {
			b, err := io.ReadAll(r)
			return fmt.Sprintf("blob-%d", len(b)), int64(len(b)), err
		},
	}
	if mod != nil {
		mod(&o)
	}
	return New(o), logs
}

// --- Chat events -------------------------------------------------------------

// encode wraps an event the way Pub/Sub carries it.
func encode(ev map[string]any) string {
	b, _ := json.Marshal(ev)
	return base64.StdEncoding.EncodeToString(b)
}

func human(id, name, email string) map[string]any {
	return map[string]any{"name": "users/" + id, "displayName": name, "email": email, "type": "HUMAN"}
}

var alice = human("111", "Alice", "Alice@Example.com")

// msgEvent is a MESSAGE event in a space; mod adjusts the message.
func msgEvent(space, spaceType, msgID string, created time.Time, text string, mod func(m map[string]any)) map[string]any {
	m := map[string]any{
		"name":       space + "/messages/" + msgID,
		"sender":     alice,
		"createTime": created.UTC().Format(time.RFC3339Nano),
		"text":       text,
		"thread":     map[string]any{"name": space + "/threads/t" + msgID},
	}
	if mod != nil {
		mod(m)
	}
	return map[string]any{
		"type":      "MESSAGE",
		"eventTime": created.UTC().Format(time.RFC3339Nano),
		"space":     map[string]any{"name": space, "spaceType": spaceType},
		"message":   m,
		"user":      alice,
	}
}

// --- Run harness -------------------------------------------------------------

type statusRec struct {
	state  channel.State
	detail string
}

type runner struct {
	statuses chan statusRec
	envs     chan event.Envelope
	stop     func() error
}

// run starts Run with in (nil: accept everything) and returns its channels.
func run(t *testing.T, a *Adapter, in channel.Inbound) *runner {
	t.Helper()
	r := &runner{statuses: make(chan statusRec, 256), envs: make(chan event.Envelope, 256)}
	if in == nil {
		in = func(_ context.Context, env event.Envelope) error {
			r.envs <- env
			return nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- a.Run(ctx, in, func(s channel.State, d string) { r.statuses <- statusRec{s, d} })
	}()
	var once sync.Once
	var err error
	r.stop = func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return")
			}
		})
		return err
	}
	t.Cleanup(func() { _ = r.stop() })
	return r
}

func (r *runner) waitStatus(t *testing.T, s channel.State) statusRec {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-r.statuses:
			if got.state == s {
				return got
			}
		case <-deadline:
			t.Fatalf("no %s status", s)
		}
	}
}

func (r *runner) nextEnv(t *testing.T) event.Envelope {
	t.Helper()
	select {
	case env := <-r.envs:
		return env
	case <-time.After(5 * time.Second):
		t.Fatal("no envelope")
	}
	return event.Envelope{}
}

// eventually polls cond for up to 5 s.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// deliver feeds events through handleBatch directly and returns the envelopes.
func deliver(t *testing.T, a *Adapter, evs ...map[string]any) []event.Envelope {
	t.Helper()
	var out []event.Envelope
	var msgs []pulledMessage
	for i, ev := range evs {
		var m pulledMessage
		m.AckID = fmt.Sprintf("ack-%d", i)
		m.Message.Data = encode(ev)
		msgs = append(msgs, m)
	}
	a.handleBatch(context.Background(), func(_ context.Context, env event.Envelope) error {
		out = append(out, env)
		return nil
	}, msgs)
	return out
}
