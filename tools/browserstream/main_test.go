package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"holaclaw/internal/api"
	"holaclaw/internal/browser/client"
	"holaclaw/internal/browser/proto"
	"holaclaw/internal/event"
)

// --- a sidecar on a unix socket --------------------------------------------------

// fakeSidecar serves the two routes this tool uses: the session list and the
// stream. The stream sends a meta, then an activity, then waits to be closed.
type fakeSidecar struct {
	socket string
	// extra is written to every stream after the activity, for a test that
	// wants more.
	opened chan struct{}
}

func newFakeSidecar(t testing.TB) *fakeSidecar {
	t.Helper()
	// Not t.TempDir: a unix socket path is capped at ~100 bytes and the test
	// name goes into that path.
	dir, err := os.MkdirTemp("", "hb")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	f := &fakeSidecar{socket: filepath.Join(dir, "s"), opened: make(chan struct{}, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+proto.PathSessions, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []proto.Session{{Agent: "main", Tabs: []proto.Tab{{ID: "t1", Active: true}}}})
	})
	mux.HandleFunc("GET "+proto.PathStream, f.stream)

	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return f
}

func (f *fakeSidecar) stream(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	ctx := r.Context()
	select {
	case f.opened <- struct{}{}:
	default:
	}
	for _, m := range []proto.StreamMessage{
		{Type: proto.StreamMeta, Width: 1280, Height: 800, Tabs: []proto.Tab{{ID: "t1", Active: true}}},
		{Type: proto.StreamActivity, Activity: &proto.Activity{
			At: time.Now().UTC(), Op: "act", Kind: proto.ActClick, Ref: "g1r3",
			Target: `button "Search"`, OK: true, Navigated: true, TookMS: 812,
		}},
	} {
		raw, _ := json.Marshal(m)
		if conn.Write(ctx, websocket.MessageText, raw) != nil {
			return
		}
	}
	<-ctx.Done()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// testTool starts the tool's own HTTP server over a fake sidecar.
func testTool(t testing.TB, hub *eventHub) (*httptest.Server, *manager) {
	t.Helper()
	side := newFakeSidecar(t)
	mgr := newManager(client.New(side.socket, client.Options{}), slog.New(slog.DiscardHandler))
	mgr.events = hub
	srv := httptest.NewServer(newMux(mgr))
	t.Cleanup(srv.Close)
	return srv, mgr
}

// dialViewer connects to the tool the way the page does.
func dialViewer(t testing.TB, srv *httptest.Server) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	url := "ws" + srv.URL[len("http"):] + "/stream?agent=main"
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn, ctx
}

// readText reads text messages until one of the wanted type arrives.
func readText(t testing.TB, ctx context.Context, conn *websocket.Conn, want string) map[string]any {
	t.Helper()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read %s: %v", want, err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if m["type"] == want {
			return m
		}
	}
}

// The sidecar's own account of what it served reaches the viewer, unchanged.
func TestActivityReachesTheViewer(t *testing.T) {
	srv, _ := testTool(t, nil)
	conn, ctx := dialViewer(t, srv)

	if m := readText(t, ctx, conn, "meta"); m["width"] != float64(1280) {
		t.Fatalf("meta = %+v", m)
	}
	m := readText(t, ctx, conn, "activity")
	a, _ := m["activity"].(map[string]any)
	if a == nil || a["ref"] != "g1r3" || a["target"] != `button "Search"` || a["ok"] != true {
		t.Fatalf("activity = %+v", m)
	}
}

// A viewer that joins later is caught up with the meta and nothing else: the
// activity is news, and the sidecar replays its own ring to each new stream.
func TestLateViewerIsReplayedOnlyTheMeta(t *testing.T) {
	srv, _ := testTool(t, nil)
	first, ctx1 := dialViewer(t, srv)
	readText(t, ctx1, first, "activity") // the relay is up and has both messages

	second, ctx2 := dialViewer(t, srv)
	if m := readText(t, ctx2, second, "meta"); m["width"] != float64(1280) {
		t.Fatalf("replayed meta = %+v", m)
	}
	// Nothing else arrives on its own: the upstream is quiet now.
	short, cancel := context.WithTimeout(ctx2, 300*time.Millisecond)
	defer cancel()
	for {
		typ, data, err := second.Read(short)
		if err != nil {
			return // the timeout: nothing more was replayed
		}
		if typ != websocket.MessageText {
			continue
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if m["type"] == "activity" {
			t.Fatalf("a joining viewer was replayed an old activity: %s", data)
		}
	}
}

// --- the daemon's event feed ------------------------------------------------------

// fakeDaemon serves /v1/events as SSE: two browser events, one that is not,
// then the end of the stream. Later connections block, so a test sees each
// event once.
func fakeDaemon(t testing.TB) *httptest.Server {
	t.Helper()
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != api.BaseURL+"/events" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		n++
		if n > 1 {
			<-r.Context().Done()
			return
		}
		for _, e := range []event.Event{
			event.New(event.BrowserStep, "trace1", "", "", map[string]any{"n": 1, "action": "click", "ref": "r3"}),
			event.New(event.ModelRequest, "trace1", "", "", map[string]any{"model": "x"}),
			event.New(event.BrowserStep, "trace1", "", "", map[string]any{"n": 2, "action": "type", "ref": "r7"}),
			// An approval is the one non-browser event shown: a run waiting on
			// one looks exactly like a frozen browser otherwise.
			event.New(event.ApprovalRequested, "trace1", "", "", map[string]any{"kind": "tool", "summary": "browser: a move classified send"}),
		} {
			raw, _ := json.Marshal(e)
			_, _ = w.Write([]byte("id: " + string(e.ID) + "\ndata: " + string(raw) + "\n\n"))
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The tailer keeps the browser events, drops everything else, and hands them
// to the viewers as they arrive.
func TestEventTailerFiltersAndFansOut(t *testing.T) {
	daemon := fakeDaemon(t)
	c, err := api.Dial(daemon.URL, "")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	hub := newEventHub(c, slog.New(slog.DiscardHandler))
	hub.retry = 10 * time.Millisecond

	srv, _ := testTool(t, hub)
	conn, ctx := dialViewer(t, srv)

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.run(runCtx)

	for _, want := range []float64{1, 2} {
		m := readText(t, ctx, conn, "event")
		ev, _ := m["event"].(map[string]any)
		attrs, _ := ev["attrs"].(map[string]any)
		if ev["type"] != string(event.BrowserStep) {
			t.Fatalf("event = %+v, want only browser.*", ev)
		}
		if attrs["n"] != want {
			t.Fatalf("event n = %v, want %v (an event was dropped or reordered)", attrs["n"], want)
		}
	}
	if m := readText(t, ctx, conn, "event"); m["event"].(map[string]any)["type"] != string(event.ApprovalRequested) {
		t.Fatalf("event = %+v, want the approval request", m)
	}

	// All three are kept for whoever joins next, and the model request is not.
	waitFor(t, func() bool { return hub.size() == 3 })
	late, lateCtx := dialViewer(t, srv)
	for _, want := range []float64{1, 2} {
		m := readText(t, lateCtx, late, "event")
		ev, _ := m["event"].(map[string]any)
		attrs, _ := ev["attrs"].(map[string]any)
		if attrs["n"] != want {
			t.Fatalf("replayed event n = %v, want %v", attrs["n"], want)
		}
	}
}

// The ring keeps the last eventRing entries and no more.
func TestEventRingCaps(t *testing.T) {
	hub := newEventHub(nil, slog.New(slog.DiscardHandler))
	for i := 0; i < eventRing+10; i++ {
		hub.publish([]byte(`{"type":"event"}`))
	}
	if hub.size() != eventRing {
		t.Fatalf("ring holds %d, want %d", hub.size(), eventRing)
	}
}

// --- the command line -------------------------------------------------------

// An unknown flag is a usage error: exit 2, and nothing is served.
func TestUsageError(t *testing.T) {
	var out bytes.Buffer
	code := execute([]string{"--nope"}, &out, &out)
	if code != 2 {
		t.Fatalf("code = %d, want 2; output: %s", code, out.String())
	}
}

// --help exits 0 and prints the flags, without starting a server.
func TestCommandHelp(t *testing.T) {
	var out bytes.Buffer
	code := execute([]string{"--help"}, &out, &out)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "--addr") {
		t.Fatalf("help output = %q, want a mention of --addr", out.String())
	}
}

func waitFor(t testing.TB, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never became true")
		}
		time.Sleep(time.Millisecond)
	}
}
