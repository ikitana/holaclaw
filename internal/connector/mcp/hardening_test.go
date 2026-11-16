package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"holaclaw/internal/connector/mcp/mcptest"
)

// rpcMethod peeks at a POST's JSON-RPC method and restores the body.
func rpcMethod(r *http.Request) string {
	if r.Method != http.MethodPost {
		return ""
	}
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	var m struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Method
}

func TestErrorStreamBodyIsBounded(t *testing.T) {
	var broken atomic.Bool
	var written atomic.Int64
	done := make(chan struct{})
	srv := mcptest.New(t, mcptest.Options{Wrap: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !broken.Load() || r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}
			defer close(done)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusBadRequest)
			chunk := bytes.Repeat([]byte("x"), 32<<10)
			for written.Load() < 64<<20 {
				n, err := w.Write(chunk)
				written.Add(int64(n))
				if err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
		})
	}})
	c := dial(t, srv, Options{})
	broken.Store(true)
	if err := c.Ping(context.Background()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("server still streaming")
	}
	if n := written.Load(); n >= 32<<20 {
		t.Fatalf("server wrote %d bytes; the client kept reading", n)
	}
}

func TestToolsChangedIsCoalescedAndStopsAtClose(t *testing.T) {
	srv := mcptest.New(t, mcptest.Options{})
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	c := dial(t, srv, Options{OnToolsChanged: func() {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
	}})
	c.mu.Lock()
	req := &sdk.ToolListChangedRequest{Session: c.live.cs}
	c.mu.Unlock()
	ctx := context.Background()
	c.toolsChanged(ctx, req)
	<-started
	for range 10000 {
		c.toolsChanged(ctx, req)
	}
	close(release)
	waitFor(t, "second call", func() bool { return calls.Load() == 2 })
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Fatalf("calls = %d, want 2 (coalesced)", n)
	}
	c.toolsChanged(ctx, &sdk.ToolListChangedRequest{}) // another session
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Fatalf("stale session notification ran the callback (%d)", n)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c.toolsChanged(ctx, req)
	time.Sleep(50 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Fatalf("callback ran after Close (%d)", n)
	}
}

func TestListToolsCaps(t *testing.T) {
	bigSchema := json.RawMessage(fmt.Sprintf(`{"type":"object","description":%q}`, strings.Repeat("s", maxSchemaBytes)))
	srv := mcptest.New(t, mcptest.Options{Tools: []mcptest.Tool{
		{Name: "ok"},
		{Name: "longdesc", Description: strings.Repeat("d", maxDescBytes+1)},
		{Name: "bigschema", InputSchema: bigSchema},
	}})
	c := dial(t, srv, Options{})
	got, err := c.ListTools(context.Background())
	if err != nil || len(got) != 1 || got[0].Name != "ok" {
		t.Fatalf("ListTools = %+v, %v; want only ok", got, err)
	}

	var many []mcptest.Tool
	schema := json.RawMessage(fmt.Sprintf(`{"type":"object","description":%q}`, strings.Repeat("s", 60<<10)))
	for i := range MaxListBytes/(60<<10) + 2 {
		many = append(many, mcptest.Tool{Name: fmt.Sprintf("t%d", i), InputSchema: schema})
	}
	srv2 := mcptest.New(t, mcptest.Options{Tools: many, PageSize: 10})
	c2 := dial(t, srv2, Options{})
	if _, err := c2.ListTools(context.Background()); !errors.Is(err, ErrListTooLarge) || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("over budget: err = %v", err)
	}
}

func TestIsLoopback(t *testing.T) {
	for h, want := range map[string]bool{
		"localhost": true, "LOCALHOST": true, "127.0.0.1": true, "127.9.9.9": true, "::1": true,
		"example.com": false, "10.0.0.1": false, "localhost.example.com": false, "": false,
	} {
		if got := isLoopback(h); got != want {
			t.Errorf("isLoopback(%q) = %v", h, got)
		}
	}
}

func TestRedirectStaysOnOrigin(t *testing.T) {
	origin, _ := url.Parse("https://mcp.example.com/mcp")
	hc := newHTTPClient(nil, origin, nil, false)
	for target, ok := range map[string]bool{
		"https://mcp.example.com/other": true,
		"https://evil.example.com/mcp":  false,
		"http://mcp.example.com/mcp":    false,
		"https://mcp.example.com:8443/": false,
	} {
		req, _ := http.NewRequest(http.MethodPost, target, nil)
		if err := hc.CheckRedirect(req, nil); (err == nil) != ok {
			t.Errorf("redirect to %s: err = %v", target, err)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTokenGetsDeadline(t *testing.T) {
	origin, _ := url.Parse("https://mcp.example.com/")
	var hadDeadline bool
	tr := &transport{
		origin: origin,
		token: func(ctx context.Context) (string, error) {
			_, hadDeadline = ctx.Deadline()
			return "tok", nil
		},
		base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("Authorization") != "Bearer tok" {
				t.Error("no token sent")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
		}),
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://mcp.example.com/", nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if !hadDeadline {
		t.Fatal("token callback got no deadline")
	}
}

func TestHTTPClientCopyDropsJarAndTimeout(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	base := &http.Client{Jar: jar, Timeout: time.Second}
	origin, _ := url.Parse("https://mcp.example.com/")
	hc := newHTTPClient(base, origin, nil, false)
	if hc.Jar != nil || hc.Timeout != 0 {
		t.Fatalf("copy kept Jar or Timeout: %+v", hc)
	}
	if base.Jar == nil || base.Timeout != time.Second || base.CheckRedirect != nil {
		t.Fatal("base client modified")
	}
}

func TestCloseInterruptsCalls(t *testing.T) {
	srv := mcptest.New(t, mcptest.Options{Tools: []mcptest.Tool{mcptest.BlockingTool("slow", nil)}})
	c := dial(t, srv, Options{})
	errc := make(chan error, 1)
	go func() {
		_, err := c.CallTool(context.Background(), "slow", nil)
		errc <- err
	}()
	waitFor(t, "call in flight", func() bool { return srv.MaxConcurrentCalls() == 1 })
	start := time.Now()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("Close took %v", d)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("interrupted call err = %v, want ErrClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("call not interrupted by Close")
	}
	for _, r := range srv.Requests() {
		if r.HTTPMethod == http.MethodDelete {
			t.Fatal("DELETE sent while a call was in flight")
		}
	}
}

func TestDeadConnectionRecoversWithoutSending(t *testing.T) {
	srv := mcptest.New(t, mcptest.Options{Legacy: true, Tools: []mcptest.Tool{mcptest.TextTool("echo", "hi")}})
	c := dial(t, srv, Options{})
	c.mu.Lock()
	old := c.live
	c.mu.Unlock()
	// The GET stream reconnects, gets 404 and the SDK fails the connection.
	srv.ExpireSessions()
	srv.CloseClientConnections()
	select {
	case <-old.dead:
	case <-time.After(15 * time.Second):
		t.Fatal("SDK connection did not fail")
	}
	r, err := c.CallTool(context.Background(), "echo", nil)
	if err != nil || r.Text != "hi" {
		t.Fatalf("CallTool = %+v, %v", r, err)
	}
	if n := srv.Initializes(); n != 2 {
		t.Fatalf("initializes = %d, want 2", n)
	}
	if n := count(srv.Requests(), "tools/call"); n != 1 {
		t.Fatalf("tools/call seen %d times, want 1 (the dead attempt is never sent)", n)
	}
}

func TestInitializeWaitersHonourContextAndClose(t *testing.T) {
	var stall atomic.Bool
	stalled := make(chan struct{}, 4)
	srv := mcptest.New(t, mcptest.Options{Legacy: true, Wrap: func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if stall.Load() && rpcMethod(r) == "initialize" {
				stalled <- struct{}{}
				<-r.Context().Done()
				return
			}
			next.ServeHTTP(w, r)
		})
	}})
	c := dial(t, srv, Options{})
	stall.Store(true)
	srv.ExpireSessions()
	errA := make(chan error, 1)
	go func() { errA <- c.Ping(context.Background()) }()
	<-stalled

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Ping(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter err = %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("waiter ignored its context for %v", d)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errA:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("dialing call err = %v, want ErrClosed", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not interrupt initialize")
	}
}

func TestTimeoutIncludesSlotWait(t *testing.T) {
	srv := mcptest.New(t, mcptest.Options{Tools: []mcptest.Tool{mcptest.BlockingTool("slow", nil)}})
	c := dial(t, srv, Options{})
	c.timeout = time.Second
	for range MaxInFlight {
		go func() { _, _ = c.CallTool(context.Background(), "slow", nil) }()
	}
	waitFor(t, "slots taken", func() bool { return srv.MaxConcurrentCalls() == MaxInFlight })
	start := time.Now()
	_, err := c.CallTool(context.Background(), "slow", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 1600*time.Millisecond {
		t.Fatalf("timeout took %v; the slot wait was not counted", d)
	}
}

func TestListToolsRestartsWhenSessionChanges(t *testing.T) {
	var srv *mcptest.Server
	var lists atomic.Int32
	srv = mcptest.New(t, mcptest.Options{Legacy: true,
		Tools:    []mcptest.Tool{{Name: "a"}, {Name: "b"}, {Name: "c"}},
		PageSize: 1,
		Wrap: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if rpcMethod(r) == "tools/list" && lists.Add(1) == 2 {
					srv.ExpireSessions() // the second page hits a lost session
				}
				next.ServeHTTP(w, r)
			})
		},
	})
	c := dial(t, srv, Options{})
	got, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range got {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "a,b,c" {
		t.Fatalf("names = %v", names)
	}
	// page 1, page 2 (404), page 2 retried on the new session (discarded),
	// then pages 1-3 again.
	if n := count(srv.Requests(), "tools/list"); n != 6 {
		t.Fatalf("tools/list requests = %d, want 6", n)
	}
}
