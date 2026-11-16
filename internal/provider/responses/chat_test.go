package responses

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/provider"
	"holaclaw/internal/provider/wire"
)

// --- helpers ---------------------------------------------------------------

// sseHandler streams raw SSE text, flushing between frames.
func sseHandler(t *testing.T, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, frame := range strings.SplitAfter(body, "\n\n") {
			if frame == "" {
				continue
			}
			_, _ = io.WriteString(w, frame)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	return string(b)
}

// newTestClient wires a Client to a test server.
func newTestClient(t *testing.T, h http.Handler, opts Options) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	opts.BaseURL = srv.URL + "/v1"
	if opts.Name == "" {
		opts.Name = "codexish"
	}
	return New(opts)
}

func chatOnce(t *testing.T, c *Client, req provider.Request) (*provider.Response, []string, error) {
	t.Helper()
	s, err := c.Chat(context.Background(), req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = s.Close() }()
	var deltas []string
	resp, err := provider.Collect(s, func(txt string) { deltas = append(deltas, txt) })
	return resp, deltas, err
}

// firstErr drains a stream that may have failed at Chat time already.
func firstErr(s provider.Stream, err error) error {
	if err != nil {
		return err
	}
	_, err = provider.Collect(s, nil)
	return err
}

func testModel() provider.Model {
	return provider.Model{Name: "m", ID: "gpt-5-codex", Tools: true, Timeout: 5 * time.Second}
}

func testRequest() provider.Request {
	return provider.Request{
		Model:    testModel(),
		System:   "You are holaclaw.",
		Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "hola")},
	}
}

// captureBody serves a fixture and records the decoded request body.
func captureBody(t *testing.T, sse string, got *map[string]any, hdr *http.Header) http.HandlerFunc {
	t.Helper()
	var mu sync.Mutex
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if hdr != nil {
			*hdr = r.Header.Clone()
		}
		if got != nil {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode: %v", err)
			}
			*got = body
		}
		mu.Unlock()
		sseHandler(t, sse)(w, r)
	}
}

// --- streaming -------------------------------------------------------------

func TestChatTextStream(t *testing.T) {
	var path string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		sseHandler(t, fixture(t, "text.sse"))(w, r)
	}), Options{})

	resp, deltas, err := chatOnce(t, c, testRequest())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if path != "/v1/responses" {
		t.Errorf("path = %q", path)
	}
	if resp.Text != "Hola, Angel" {
		t.Errorf("text = %q", resp.Text)
	}
	if strings.Join(deltas, "|") != "Hola|, |Angel" {
		t.Errorf("deltas = %v", deltas)
	}
	if resp.Finish != provider.FinishStop {
		t.Errorf("finish = %q", resp.Finish)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 3 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("tool calls = %+v", resp.ToolCalls)
	}
}

func TestChatFunctionCallFragmentedArguments(t *testing.T) {
	c := newTestClient(t, sseHandler(t, fixture(t, "tool_call.sse")), Options{})
	resp, _, err := chatOnce(t, c, testRequest())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Finish != provider.FinishToolCalls {
		t.Errorf("finish = %q", resp.Finish)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_a" || tc.Name != "weather" || tc.Arguments != `{"city":"Madrid"}` {
		t.Errorf("call = %+v", tc)
	}
	if len(resp.Repairs) != 0 {
		t.Errorf("repairs = %v", resp.Repairs)
	}
}

func TestChatTwoSequentialFunctionCalls(t *testing.T) {
	c := newTestClient(t, sseHandler(t, fixture(t, "two_calls.sse")), Options{})
	resp, _, err := chatOnce(t, c, testRequest())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].ID != "call_a" || resp.ToolCalls[0].Name != "weather" {
		t.Errorf("first = %+v", resp.ToolCalls[0])
	}
	if resp.ToolCalls[1].ID != "call_b" || resp.ToolCalls[1].Arguments != `{"tz":"CET"}` {
		t.Errorf("second = %+v", resp.ToolCalls[1])
	}
}

func TestChatScrubsReasoningTags(t *testing.T) {
	c := newTestClient(t, sseHandler(t, fixture(t, "thinking.sse")), Options{})
	resp, deltas, err := chatOnce(t, c, testRequest())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Text != "Hola" {
		t.Errorf("text = %q", resp.Text)
	}
	for _, d := range deltas {
		if strings.Contains(d, "secret plan") {
			t.Errorf("reasoning leaked in delta %q", d)
		}
	}
}

func TestChatDroppedStreamDiscardsIncompleteCall(t *testing.T) {
	c := newTestClient(t, sseHandler(t, fixture(t, "dropped.sse")), Options{})
	resp, _, err := chatOnce(t, c, testRequest())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Finish != provider.FinishDropped {
		t.Errorf("finish = %q", resp.Finish)
	}
	if len(resp.ToolCalls) != 0 {
		t.Errorf("incomplete call survived: %+v", resp.ToolCalls)
	}
	if resp.Text != "thinking about it" {
		t.Errorf("text = %q", resp.Text)
	}
}

func TestChatIncompleteIsLength(t *testing.T) {
	c := newTestClient(t, sseHandler(t, fixture(t, "incomplete.sse")), Options{})
	resp, _, err := chatOnce(t, c, testRequest())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Finish != provider.FinishLength {
		t.Errorf("finish = %q", resp.Finish)
	}
	if resp.Usage.CompletionTokens != 64 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestChatRecordsArgumentRepair(t *testing.T) {
	c := newTestClient(t, sseHandler(t, fixture(t, "repair.sse")), Options{})
	resp, _, err := chatOnce(t, c, testRequest())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Arguments != `{"city":"Madrid"}` {
		t.Fatalf("calls = %+v", resp.ToolCalls)
	}
	if got := resp.Repairs["call_a"]; got != wire.StageTrailingComma {
		t.Errorf("repair stage = %q, repairs = %v", got, resp.Repairs)
	}
}

// --- operational behaviour -------------------------------------------------

func TestChatProgressWhileIdle(t *testing.T) {
	release := make(chan struct{})
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}), Options{ProgressInterval: 10 * time.Millisecond})
	t.Cleanup(func() { close(release) })

	s, err := c.Chat(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer func() { _ = s.Close() }()
	chunk, err := s.Next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if chunk.Kind != provider.ChunkProgress {
		t.Errorf("kind = %v", chunk.Kind)
	}
}

func TestChatConcurrencyLimit(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	gate := make(chan struct{})
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		<-gate
		mu.Lock()
		inFlight--
		mu.Unlock()
		sseHandler(t, fixture(t, "text.sse"))(w, r)
	}), Options{MaxConcurrent: 2})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = chatOnce(t, c, testRequest())
		}()
	}
	// Let the first batch reach the handler, then let everything through.
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if peak > 2 {
		t.Errorf("peak in-flight = %d, want <= 2", peak)
	}
}

func TestChatContextCancelFreesSlot(t *testing.T) {
	gate := make(chan struct{})
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		sseHandler(t, fixture(t, "text.sse"))(w, r)
	}), Options{MaxConcurrent: 1})

	// The only slot is taken by a call the caller then cancels.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	err := firstErr(c.Chat(ctx, testRequest()))
	if err == nil {
		t.Fatal("want an error after cancel")
	}
	if k := provider.KindOf(err); k != provider.ErrNetwork && k != provider.ErrTimeout {
		t.Errorf("kind = %q", k)
	}
	<-ctx.Done()

	// If the slot had leaked this would block until the request timeout.
	close(gate)
	resp, _, err := chatOnce(t, c, testRequest())
	if err != nil {
		t.Fatalf("second chat: %v", err)
	}
	if resp.Text != "Hola, Angel" {
		t.Errorf("text = %q", resp.Text)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c := newTestClient(t, sseHandler(t, fixture(t, "text.sse")), Options{MaxConcurrent: 1})
	s, err := c.Chat(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
	// A double release would have panicked or blocked; a new call must work.
	if _, _, err := chatOnce(t, c, testRequest()); err != nil {
		t.Fatalf("after close: %v", err)
	}
}

// A Codex reply reports tokens and, when the server sends them, the cached
// and reasoning shares; it never reports a cost, so the call is unpriced.
func TestChatUsageDetails(t *testing.T) {
	cached, reasoning := 1024, 0
	cases := []struct {
		name, usage string
		want        provider.Usage
	}{
		{"plain", `{"input_tokens":5,"output_tokens":2}`, provider.Usage{PromptTokens: 5, CompletionTokens: 2}},
		{"details", `{"input_tokens":2000,"output_tokens":30,"input_tokens_details":{"cached_tokens":1024},"output_tokens_details":{"reasoning_tokens":0}}`,
			provider.Usage{PromptTokens: 2000, CompletionTokens: 30, CachedTokens: &cached, ReasoningTokens: &reasoning}},
		{"empty details", `{"input_tokens":5,"output_tokens":2,"input_tokens_details":{},"output_tokens_details":null}`,
			provider.Usage{PromptTokens: 5, CompletionTokens: 2}},
	}
	for _, tc := range cases {
		body := "event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"Hola"}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"r","status":"completed","usage":` + tc.usage + `}}` + "\n\n"
		c := newTestClient(t, sseHandler(t, body), Options{})
		resp, _, err := chatOnce(t, c, testRequest())
		if err != nil {
			t.Fatalf("%s: chat: %v", tc.name, err)
		}
		if !reflect.DeepEqual(resp.Usage, tc.want) {
			t.Errorf("%s: usage = %+v, want %+v", tc.name, resp.Usage, tc.want)
		}
	}
}
