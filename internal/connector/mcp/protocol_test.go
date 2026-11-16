package mcp

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"holaclaw/internal/connector/mcp/mcptest"
)

func negotiated(c *Client) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live.cs.InitializeResult().ProtocolVersion
}

func TestNegotiatesModernProtocol(t *testing.T) {
	srv := mcptest.New(t, mcptest.Options{Tools: []mcptest.Tool{mcptest.TextTool("echo", "hi")}})
	c := dial(t, srv, Options{})
	if v := negotiated(c); v != protocolVersion {
		t.Fatalf("negotiated %q, want %q", v, protocolVersion)
	}
	if c.SessionID() != "" {
		t.Fatalf("modern server gave session %q", c.SessionID())
	}
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r, err := c.CallTool(context.Background(), "echo", nil); err != nil || r.Text != "hi" {
		t.Fatalf("CallTool = %+v, %v", r, err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	reqs := srv.Requests()
	// One discover to connect, one for Ping (2026-07-28 has no ping).
	if count(reqs, "server/discover") != 2 || count(reqs, "initialize") != 0 || count(reqs, "ping") != 0 {
		t.Fatalf("handshake: %d discover, %d initialize", count(reqs, "server/discover"), count(reqs, "initialize"))
	}
	for _, r := range reqs {
		if r.SessionID != "" || r.HTTPMethod == http.MethodGet {
			t.Fatalf("stateful request on a modern server: %+v", r)
		}
	}
}

func TestFallsBackToLegacyProtocol(t *testing.T) {
	srv := mcptest.New(t, mcptest.Options{Legacy: true, Tools: []mcptest.Tool{mcptest.TextTool("echo", "hi")}})
	c := dial(t, srv, Options{})
	if v := negotiated(c); v != mcptest.LegacyVersion {
		t.Fatalf("negotiated %q, want %q", v, mcptest.LegacyVersion)
	}
	if c.SessionID() == "" {
		t.Fatal("legacy server gave no session id")
	}
	reqs := srv.Requests()
	if count(reqs, "server/discover") != 1 || srv.Initializes() != 1 {
		t.Fatalf("handshake: %d discover, %d initialize", count(reqs, "server/discover"), srv.Initializes())
	}
	if r, err := c.CallTool(context.Background(), "echo", nil); err != nil || r.Text != "hi" {
		t.Fatalf("CallTool = %+v, %v", r, err)
	}
}

func TestModernNotFoundIsNotRetried(t *testing.T) {
	var mu sync.Mutex
	fail := 1
	srv := mcptest.New(t, mcptest.Options{
		Tools: []mcptest.Tool{mcptest.TextTool("echo", "hi")},
		Wrap: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				hit := fail > 0 && rpcMethod(r) == "tools/call"
				if hit {
					fail--
				}
				mu.Unlock()
				if hit {
					http.Error(w, "not found", http.StatusNotFound)
					return
				}
				next.ServeHTTP(w, r)
			})
		},
	})
	c := dial(t, srv, Options{})
	if _, err := c.CallTool(context.Background(), "echo", nil); err == nil {
		t.Fatal("404 without a session succeeded")
	}
	if n := handshakes(srv); n != 1 {
		t.Fatalf("handshakes = %d: a stateless 404 must not reconnect and retry", n)
	}
	// The fake records requests after Wrap, so the 404 itself is not seen.
	if n := count(srv.Requests(), "tools/call"); n != 0 {
		t.Fatalf("tools/call retried %d times", n)
	}
	if r, err := c.CallTool(context.Background(), "echo", nil); err != nil || r.Text != "hi" {
		t.Fatalf("next call = %+v, %v", r, err)
	}
}

func TestToolsCacheBoundReplacesConnection(t *testing.T) {
	var tools []mcptest.Tool
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		tools = append(tools, mcptest.Tool{Name: n})
	}
	for _, legacy := range []bool{false, true} {
		srv := mcptest.New(t, mcptest.Options{Legacy: legacy, Tools: tools, PageSize: 1})
		c := dial(t, srv, Options{})
		listings := maxCachedPages / len(tools)
		for range listings {
			if got, err := c.ListTools(context.Background()); err != nil || len(got) != len(tools) {
				t.Fatalf("ListTools = %d tools, %v", len(got), err)
			}
		}
		if n := handshakes(srv); n != 1 {
			t.Fatalf("legacy=%v: replaced before the cap (%d handshakes)", legacy, n)
		}
		if _, err := c.ListTools(context.Background()); err != nil {
			t.Fatal(err)
		}
		want := 2
		if legacy {
			want = 1 // nothing cached on a legacy connection
		}
		if n := handshakes(srv); n != want {
			t.Fatalf("legacy=%v: handshakes = %d after the cap, want %d", legacy, n, want)
		}
	}
}

func TestListenStreamIsNotAnInFlightCall(t *testing.T) {
	release := make(chan struct{})
	srv := mcptest.New(t, mcptest.Options{Tools: []mcptest.Tool{mcptest.BlockingTool("wait", release)}})
	c := dial(t, srv, Options{OnToolsChanged: func() {}})
	waitFor(t, "listen stream", func() bool { return count(srv.Requests(), "subscriptions/listen") == 1 })
	var wg sync.WaitGroup
	errs := make(chan error, MaxInFlight)
	for range MaxInFlight {
		wg.Go(func() {
			_, err := c.CallTool(context.Background(), "wait", nil)
			errs <- err
		})
	}
	waitFor(t, "all slots used", func() bool { return srv.MaxConcurrentCalls() == MaxInFlight })
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestListenRejectedDoesNotFailDial(t *testing.T) {
	srv := mcptest.New(t, mcptest.Options{
		Tools: []mcptest.Tool{mcptest.TextTool("echo", "hi")},
		Wrap: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if rpcMethod(r) == "subscriptions/listen" {
					w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope"`)
					http.Error(w, "no", http.StatusUnauthorized)
					return
				}
				next.ServeHTTP(w, r)
			})
		},
	})
	c := dial(t, srv, Options{OnToolsChanged: func() {}})
	if r, err := c.CallTool(context.Background(), "echo", nil); err != nil || r.Text != "hi" {
		t.Fatalf("CallTool = %+v, %v", r, err)
	}
}
