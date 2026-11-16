package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"holaclaw/internal/browser"
	"holaclaw/internal/browser/proto"
	"holaclaw/internal/config"
	"holaclaw/internal/provider"
	"holaclaw/internal/tool/tools"
)

// These tests exercise the wiring alone: which roles an executor is built
// from, what a call gets when there is no browser or no daemon, and that the
// sidecar client is one per app. The executor itself is tested in
// internal/browser.

// --- fakes -----------------------------------------------------------------------

type fakeProvider struct{ name string }

func (f fakeProvider) Name() string { return f.name }
func (fakeProvider) Chat(context.Context, provider.Request) (provider.Stream, error) {
	return nil, errors.New("not called")
}

type fakeDecider struct{}

func (fakeDecider) Decide(context.Context, provider.DecisionRequest) (*provider.DecisionResponse, error) {
	return nil, errors.New("not called")
}

// noHumans is a browser.Humans that is never actually asked anything: the
// executor only has to be built.
type noHumans struct{}

func (noHumans) Approve(context.Context, browser.Task, browser.RiskKind, string) (bool, error) {
	return false, nil
}
func (noHumans) Takeover(context.Context, browser.Task, browser.Blocker) (bool, error) {
	return false, nil
}

// testApp is an App with one provider, one chat model and one agent, plus
// whatever the caller adds.
func testApp(t *testing.T, socket string) *App {
	t.Helper()
	cfg := &config.Config{
		Version:   config.CurrentVersion,
		Providers: map[string]config.Provider{"p": {Name: "p", Type: "openai-compatible", BaseURL: "http://127.0.0.1:1/v1"}},
		Models: map[string]config.Model{
			"chat": {Name: "chat", Provider: "p", ID: "m", Context: 8192},
		},
		Agents: map[string]config.Agent{
			"main": {Name: "main", Models: config.Roles{Chat: "chat"}},
		},
	}
	if socket != "" {
		cfg.Browser = config.Browser{Enabled: true, Socket: socket}
	}
	return &App{
		Config:    cfg,
		Providers: map[string]provider.Provider{"p": fakeProvider{"p"}},
		Deciders:  map[string]provider.Decider{},
		Imagers:   map[string]provider.ImageGenerator{},
	}
}

// sidecarSocket serves h on a unix socket short enough to bind.
func sidecarSocket(t *testing.T, h http.Handler) string {
	t.Helper()
	d, err := os.MkdirTemp("", "hb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	path := filepath.Join(d, "browser.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(h)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return path
}

// --- tests -----------------------------------------------------------------------

func TestBrowserBackendNoBrowser(t *testing.T) {
	a := testApp(t, "")
	b := browserBackend{a}
	if _, err := b.Run(context.Background(), browser.Task{Agent: "main"}); !errors.Is(err, tools.ErrNoBrowser) {
		t.Fatalf("Run without a [browser] table: %v, want ErrNoBrowser", err)
	}
	if _, err := b.Download(context.Background(), "main", proto.Download{ID: "x"}); !errors.Is(err, tools.ErrNoBrowser) {
		t.Fatalf("Download without a [browser] table: %v, want ErrNoBrowser", err)
	}
	if a.BrowserClient() != nil {
		t.Fatal("a client was built with no [browser] table")
	}
}

func TestBrowserBackendUnknownAgent(t *testing.T) {
	a := testApp(t, "/nonexistent/browser.sock")
	a.SetBrowserHumans(noHumans{})
	b := browserBackend{a}
	if _, err := b.Run(context.Background(), browser.Task{Agent: "ghost"}); !errors.Is(err, tools.ErrNoBrowser) {
		t.Fatalf("Run for an undefined agent: %v, want ErrNoBrowser", err)
	}
}

func TestBrowserNeedsTheDaemon(t *testing.T) {
	a := testApp(t, "/nonexistent/browser.sock")
	_, err := (browserBackend{a}).Run(context.Background(), browser.Task{Agent: "main"})
	if !errors.Is(err, errBrowserNeedsDaemon) {
		t.Fatalf("Run with no Humans: %v, want errBrowserNeedsDaemon", err)
	}
	if !strings.Contains(err.Error(), "holaclaw run") {
		t.Fatalf("the error should say what to do: %q", err)
	}
}

func TestBrowserAllow(t *testing.T) {
	a := testApp(t, "/nonexistent/browser.sock")
	b := browserBackend{a}
	if got := b.Allow("main"); len(got) != 0 {
		t.Fatalf("Allow with no list = %v, want empty (any site)", got)
	}
	ac := a.Config.Agents["main"]
	ac.Browser = config.AgentBrowser{Allow: []string{"example.com"}}
	a.Config.Agents["main"] = ac
	if got := b.Allow("main"); len(got) != 1 || got[0] != "example.com" {
		t.Fatalf("Allow = %v", got)
	}
	if got := b.Allow("ghost"); got != nil {
		t.Fatalf("Allow for an undefined agent = %v", got)
	}
}

func TestBrowserExecutorRoles(t *testing.T) {
	newApp := func(t *testing.T) *App {
		a := testApp(t, "/nonexistent/browser.sock")
		a.SetBrowserHumans(noHumans{})
		return a
	}

	t.Run("chat only", func(t *testing.T) {
		a := newApp(t)
		e, err := a.browserExecutor("main")
		if err != nil || e == nil {
			t.Fatalf("browserExecutor: %v", err)
		}
	})
	t.Run("built once", func(t *testing.T) {
		a := newApp(t)
		first, err := a.browserExecutor("main")
		if err != nil {
			t.Fatal(err)
		}
		second, err := a.browserExecutor("main")
		if err != nil {
			t.Fatal(err)
		}
		if first != second {
			t.Fatal("a second call built a second executor")
		}
	})
	t.Run("decide role", func(t *testing.T) {
		a := newApp(t)
		a.Config.Models["jev"] = config.Model{
			Name: "jev", Provider: "p", ID: "jev", API: string(provider.APIDecisions),
			URL: "https://example.test/decisions",
		}
		ac := a.Config.Agents["main"]
		ac.Models.Decide = "jev"
		a.Config.Agents["main"] = ac

		// No decisions transport for that provider: a mistake worth naming.
		if _, err := a.browserExecutor("main"); err == nil {
			t.Fatal("a decide role with no decisions transport built an executor")
		} else if !strings.Contains(err.Error(), "decision models") {
			t.Fatalf("err = %v", err)
		}
		a.Deciders["p"] = fakeDecider{}
		if _, err := a.browserExecutor("main"); err != nil {
			t.Fatalf("browserExecutor with a decide role: %v", err)
		}
	})
	t.Run("vision role", func(t *testing.T) {
		a := newApp(t)
		a.Config.Models["eye"] = config.Model{Name: "eye", Provider: "p", ID: "eye", Context: 8192, Vision: true}
		ac := a.Config.Agents["main"]
		ac.Models.Vision = "eye"
		a.Config.Agents["main"] = ac
		if _, err := a.browserExecutor("main"); err != nil {
			t.Fatalf("browserExecutor with a vision role: %v", err)
		}
	})
	t.Run("plan role", func(t *testing.T) {
		a := newApp(t)
		a.Config.Models["fast"] = config.Model{Name: "fast", Provider: "p", ID: "fast", Context: 8192}
		ac := a.Config.Agents["main"]
		ac.Models.Plan = "fast"
		a.Config.Agents["main"] = ac
		if _, err := a.browserExecutor("main"); err != nil {
			t.Fatalf("browserExecutor with a plan role: %v", err)
		}
	})
	t.Run("dangling plan role", func(t *testing.T) {
		a := newApp(t)
		ac := a.Config.Agents["main"]
		ac.Models.Plan = "gone"
		a.Config.Agents["main"] = ac
		if _, err := a.browserExecutor("main"); err == nil {
			t.Fatal("a dangling plan role built an executor")
		}
	})
	t.Run("dangling chat role", func(t *testing.T) {
		a := newApp(t)
		ac := a.Config.Agents["main"]
		ac.Models.Chat = "gone"
		a.Config.Agents["main"] = ac
		if _, err := a.browserExecutor("main"); err == nil {
			t.Fatal("a dangling chat role built an executor")
		}
	})
}

func TestBrowserClientIsOne(t *testing.T) {
	a := testApp(t, "/nonexistent/browser.sock")
	first := a.BrowserClient()
	if first == nil {
		t.Fatal("no client with a [browser] table")
	}
	if a.BrowserClient() != first {
		t.Fatal("a second call built a second client")
	}
}

// TestBrowserDownloadAndClose drives the two calls that go straight to the
// sidecar against a fake one, and proves Close stops the browsers this app
// opened.
func TestBrowserDownloadAndClose(t *testing.T) {
	var (
		mu     sync.Mutex
		closed []string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions/{agent}/close", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		closed = append(closed, r.PathValue("agent"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /sessions/{agent}/downloads/{id}", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("file body"))
	})
	a := testApp(t, sidecarSocket(t, mux))
	a.SetBrowserHumans(noHumans{})

	rc, err := (browserBackend{a}).Download(context.Background(), "main", proto.Download{ID: "d1", Name: "x.txt"})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	body, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(body) != "file body" {
		t.Fatalf("body = %q, %v", body, err)
	}

	// An agent with no executor was never opened, so nothing is closed.
	a.closeBrowser()
	mu.Lock()
	n := len(closed)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("closed %v with no executor built", closed)
	}

	a.SetBrowserHumans(noHumans{})
	if _, err := a.browserExecutor("main"); err != nil {
		t.Fatal(err)
	}
	a.closeBrowser()
	mu.Lock()
	defer mu.Unlock()
	if len(closed) != 1 || closed[0] != "main" {
		t.Fatalf("closed = %v, want [main]", closed)
	}
}

func (noHumans) WaitReleased(context.Context, browser.Task) error { return nil }

// TestBrowserExecutorBuildsWithoutCalibration: M11 removed browser.calibrate,
// so the executor is built from the roles alone.
func TestBrowserExecutorBuilds(t *testing.T) {
	a := testApp(t, "/nonexistent/browser.sock")
	a.SetBrowserHumans(noHumans{})
	if _, err := a.browserExecutor("main"); err != nil {
		t.Fatalf("browserExecutor: %v", err)
	}
}
