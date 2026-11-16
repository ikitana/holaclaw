package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"holaclaw/internal/browser/proto"
)

// Without a [browser] table there is nothing to serve, and saying so is worth
// more than a socket nobody can use.
func TestBrowserRefusesAConfigWithoutTheTable(t *testing.T) {
	dir := fixture(t, "minimal")
	code, _, errOut := runCLI(t, "", "browser", "--config", dir)
	if code != 2 {
		t.Fatalf("exit = %d, want 2\n%s", code, errOut)
	}
	if !strings.Contains(errOut, "the browser is off") {
		t.Errorf("stderr = %q", errOut)
	}
}

// The sidecar comes up on its socket, answers /health as JSON and stops when
// its context ends. The health answer is the stub's for now (see newEngine).
func TestBrowserServesHealthOverTheSocket(t *testing.T) {
	dir := fixture(t, "minimal")
	// The socket path is bounded by the kernel (108 bytes), so it lives in its
	// own short temporary directory, not under the test's name.
	runDir, err := os.MkdirTemp("", "hb")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })
	sock := filepath.Join(runDir, "b.sock")

	cfg := filepath.Join(dir, "config.toml")
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	body = append(body, []byte("\n[browser]\nsocket = \""+sock+"\"\nheadless = true\n")...)
	if err := os.WriteFile(cfg, body, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"browser", "--config", dir, "--state", filepath.Join(runDir, "state")},
			strings.NewReader(""), &nopWriter{}, &nopWriter{})
	}()

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}

	var h proto.Health
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("the sidecar never answered /health")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://sidecar"+proto.PathHealth, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		err = json.NewDecoder(resp.Body).Decode(&h)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("health is not JSON: %v", err)
		}
		break
	}
	if !h.Headless {
		t.Errorf("health did not carry the configured mode: %+v", h)
	}

	if _, err := os.Stat(filepath.Join(runDir, "state")); err != nil {
		t.Errorf("state directory was not created: %v", err)
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit = %d, want 0", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the sidecar did not stop")
	}
}

// nopWriter swallows the sidecar's log during the test.
type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
