package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"holaclaw/internal/api"
	"holaclaw/internal/app"
	"holaclaw/internal/config"
	"holaclaw/internal/provider"
)

// lister is a provider that lists ids, or fails to.
type lister struct {
	ids   []string
	err   error
	calls atomic.Int32
}

func (l *lister) Name() string { return "lister" }
func (l *lister) Chat(context.Context, provider.Request) (provider.Stream, error) {
	return nil, errors.New("no chat")
}
func (l *lister) Models(context.Context) ([]string, error) {
	l.calls.Add(1)
	return l.ids, l.err
}

// mute is a provider that cannot list its models.
type mute struct{}

func (mute) Name() string { return "mute" }
func (mute) Chat(context.Context, provider.Request) (provider.Stream, error) {
	return nil, errors.New("no chat")
}

// syncBuffer is a log sink a daemon goroutine and the test share.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestCheckModels(t *testing.T) {
	live := &lister{ids: []string{"gemma4:e2b", "qwen3:latest"}}
	down := &lister{err: errors.New("connection refused")}
	logs := &syncBuffer{}
	d := &Daemon{
		App: &app.App{
			Config: &config.Config{Models: map[string]config.Model{
				"served":  {Provider: "live", ID: "gemma4:e2b"},
				"latest":  {Provider: "live", ID: "qwen3"},
				"retired": {Provider: "live", ID: "gemma4:e4b"},
				"own-url": {Provider: "live", ID: "elsewhere", URL: "https://example.test/v1/decide"},
				"unknown": {Provider: "down", ID: "anything"},
				"silent":  {Provider: "mute", ID: "anything"},
			}},
			Providers: map[string]provider.Provider{"live": live, "down": down, "mute": mute{}, "unused": &lister{}},
		},
		log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
	d.checkModels(context.Background())

	out := logs.String()
	var warns []string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "level=WARN") {
			warns = append(warns, line)
		}
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "model=retired") ||
		!strings.Contains(warns[0], "id=gemma4:e4b") || !strings.Contains(warns[0], "provider=live") {
		t.Errorf("warnings = %q, want one, for retired on live", warns)
	}
	// A list that cannot be read says nothing about its models.
	if !strings.Contains(out, `level=INFO msg="model list not read" provider=down`) {
		t.Errorf("no info line for the provider that did not answer:\n%s", out)
	}
	if live.calls.Load() != 1 || down.calls.Load() != 1 {
		t.Errorf("requests = %d and %d, want one per provider", live.calls.Load(), down.calls.Load())
	}
	if n := d.App.Providers["unused"].(*lister).calls.Load(); n != 0 {
		t.Errorf("a provider with no model was asked %d times", n)
	}
}

// modelChecks asks at start, and ends with its context.
func TestModelChecksAtStart(t *testing.T) {
	live := &lister{ids: []string{"gemma4:e2b"}}
	d := &Daemon{
		App: &app.App{
			Config:    &config.Config{Models: map[string]config.Model{"gemma": {Provider: "live", ID: "gemma4:e2b"}}},
			Providers: map[string]provider.Provider{"live": live},
		},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.modelChecks(ctx)
	}()
	deadline := time.Now().Add(wait)
	for live.calls.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the provider was not asked at start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(wait):
		t.Fatal("modelChecks did not end with its context")
	}
	if n := live.calls.Load(); n != 1 {
		t.Errorf("requests = %d, want 1 until the next day", n)
	}
}

// The running daemon warns at start about an id its provider does not
// serve, starts all the same, and serves the list on the API.
func TestDaemonModelLists(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"gemma4:e2b"},{"id":"qwen3:8b"}]}`)
	}))
	t.Cleanup(srv.Close)

	logs := &syncBuffer{}
	dir := t.TempDir()
	e := &env{
		t: t, dir: dir, dataDir: filepath.Join(dir, "data"), socket: shortSocket(t), ctx: context.Background(),
		logger: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	writeConfig(t, e, opts{}, srv.URL+"/v1") // its model id is "m"
	e.launch()

	deadline := time.Now().Add(wait)
	for !strings.Contains(logs.String(), "does not serve a configured model id") {
		if time.Now().After(deadline) {
			t.Fatalf("no warning at start:\n%s", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out := logs.String(); !strings.Contains(out, "model=m") || !strings.Contains(out, "provider=test") {
		t.Errorf("the warning does not name the model and its provider:\n%s", out)
	}

	ids, err := e.client.ProviderModels(e.ctx, "test")
	if err != nil || len(ids) != 2 || ids[0] != "gemma4:e2b" || ids[1] != "qwen3:8b" {
		t.Errorf("ProviderModels = %v, %v", ids, err)
	}
	var ce *api.ClientError
	if _, err := e.client.ProviderModels(e.ctx, "nope"); !errors.As(err, &ce) || ce.Status != http.StatusNotFound {
		t.Errorf("unknown provider: err = %v, want a 404", err)
	}
}
