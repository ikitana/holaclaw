package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"holaclaw/internal/connector/mcp/mcptest"
)

// floodStream answers the long-lived stream (legacy GET, modern
// subscriptions/listen) with one data line that never ends, counting what
// it manages to write.
func floodStream(legacy bool, written *atomic.Int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			isStream := legacy && r.Method == http.MethodGet || !legacy && rpcMethod(r) == "subscriptions/listen"
			if !isStream {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			chunk := []byte("data: " + strings.Repeat("x", 64<<10))
			for r.Context().Err() == nil {
				n, err := w.Write(chunk)
				written.Add(int64(n))
				if err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
		})
	}
}

func TestOversizedStreamEventStopsReading(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		t.Run(map[bool]string{true: "legacy", false: "modern"}[legacy], func(t *testing.T) {
			var written atomic.Int64
			srv := mcptest.New(t, mcptest.Options{Legacy: legacy, Wrap: floodStream(legacy, &written)})
			c := dial(t, srv, Options{OnToolsChanged: func() {}})
			c.mu.Lock()
			cn := c.live
			c.mu.Unlock()
			select {
			case <-cn.dead:
			case <-time.After(10 * time.Second):
				t.Fatal("SDK did not give up on the oversized event")
			}
			// No further call: the dead connection must stop reading by itself.
			time.Sleep(500 * time.Millisecond)
			before := written.Load()
			time.Sleep(time.Second)
			if d := written.Load() - before; d > MaxResponseBytes {
				t.Fatalf("client kept reading %d MiB/s after the SDK gave up", d>>20)
			}
			if total := written.Load(); total > 8*MaxResponseBytes {
				t.Fatalf("client read %d MiB in all", total>>20)
			}
		})
	}
}

func TestModernPingReopensListenStream(t *testing.T) {
	var listens atomic.Int32
	srv := mcptest.New(t, mcptest.Options{
		Tools: []mcptest.Tool{mcptest.TextTool("one", "1")},
		Wrap: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if rpcMethod(r) == "subscriptions/listen" && listens.Add(1) == 1 {
					// The first stream ends at once, silently.
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					return
				}
				next.ServeHTTP(w, r)
			})
		},
	})
	changed := make(chan struct{}, 16)
	c := dial(t, srv, Options{OnToolsChanged: func() { changed <- struct{}{} }})
	waitFor(t, "first listen", func() bool { return listens.Load() == 1 })
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "listen reopened", func() bool { return count(srv.Requests(), "subscriptions/listen") == 1 })
	if n := handshakes(srv); n != 2 {
		t.Fatalf("handshakes = %d, want 2 (Ping reconnected)", n)
	}
	two := []mcptest.Tool{mcptest.TextTool("one", "1"), mcptest.TextTool("two", "2")}
	deadline := time.After(5 * time.Second)
	for i := 0; ; i++ {
		srv.SetTools(two[:1+i%2]...)
		select {
		case <-changed:
			return
		case <-time.After(50 * time.Millisecond):
		case <-deadline:
			t.Fatal("no list_changed after Ping reopened the stream")
		}
	}
}

func TestCloseDoesNotWaitForListenCancel(t *testing.T) {
	srv := mcptest.New(t, mcptest.Options{Wrap: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rpcMethod(r) == "notifications/cancelled" {
				<-r.Context().Done() // never answers
				return
			}
			next.ServeHTTP(w, r)
		})
	}})
	c := dial(t, srv, Options{OnToolsChanged: func() {}})
	waitFor(t, "listen", func() bool { return count(srv.Requests(), "subscriptions/listen") == 1 })
	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("Close took %v", d)
	}
}

func TestToolsCacheByteBound(t *testing.T) {
	schema := json.RawMessage(fmt.Sprintf(`{"type":"object","description":%q}`, strings.Repeat("s", 60<<10)))
	var tools []mcptest.Tool
	for i := range 30 {
		tools = append(tools, mcptest.Tool{Name: fmt.Sprintf("t%d", i), InputSchema: schema})
	}
	srv := mcptest.New(t, mcptest.Options{Tools: tools, PageSize: 30})
	c := dial(t, srv, Options{})
	// One page of ~1.8 MiB per listing: 5 listings stay under 8 MiB of
	// fetched pages only from the 6th check on.
	for i := range 5 {
		if _, err := c.ListTools(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := handshakes(srv); n != 1 {
			t.Fatalf("listing %d: replaced early (%d handshakes)", i+1, n)
		}
	}
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := handshakes(srv); n != 2 {
		t.Fatalf("handshakes = %d after %d MiB cached, want 2", n, 6*30*60>>10)
	}
}

func TestModernSessionHeaderNeverRetries(t *testing.T) {
	var failed atomic.Bool
	srv := mcptest.New(t, mcptest.Options{
		Tools: []mcptest.Tool{mcptest.TextTool("echo", "hi")},
		Wrap: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Mcp-Session-Id", "bogus") // a modern server should not
				if rpcMethod(r) == "tools/call" && failed.CompareAndSwap(false, true) {
					http.Error(w, "gone", http.StatusNotFound)
					return
				}
				next.ServeHTTP(w, r)
			})
		},
	})
	c := dial(t, srv, Options{})
	if c.SessionID() != "" {
		t.Fatalf("SessionID on a modern server = %q", c.SessionID())
	}
	if _, err := c.CallTool(context.Background(), "echo", nil); err == nil {
		t.Fatal("404 succeeded")
	}
	if n := handshakes(srv); n != 1 {
		t.Fatalf("handshakes = %d: a modern 404 must not reconnect and resend", n)
	}
	if n := count(srv.Requests(), "tools/call"); n != 0 {
		t.Fatalf("tools/call resent %d times", n)
	}
}

func TestModernUnauthorizedMidSession(t *testing.T) {
	srv := mcptest.New(t, mcptest.Options{Token: "a", Tools: []mcptest.Tool{mcptest.TextTool("echo", "hi")}})
	var tok atomic.Value
	tok.Store("a")
	c := dial(t, srv, Options{Token: func(context.Context) (string, error) { return tok.Load().(string), nil }})
	srv.SetToken("b")
	_, err := c.CallTool(context.Background(), "echo", nil)
	var ue *UnauthorizedError
	if !errors.As(err, &ue) || !strings.Contains(ue.WWWAuthenticate, "resource_metadata=") {
		t.Fatalf("err = %v", err)
	}
	tok.Store("b")
	if r, err := c.CallTool(context.Background(), "echo", nil); err != nil || r.Text != "hi" {
		t.Fatalf("after refresh = %+v, %v", r, err)
	}
	if n := handshakes(srv); n != 1 {
		t.Fatalf("handshakes = %d: a 401 must not cost the connection", n)
	}
}
