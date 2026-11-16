package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"

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
		opts.Name = "local"
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

func testModel() provider.Model {
	return provider.Model{Name: "m", ID: "gemma", Tools: true, Timeout: 5 * time.Second}
}

// --- streaming -------------------------------------------------------------

func TestChatHappyTextStream(t *testing.T) {
	var got wireRequest
	var auth, extra string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		auth = r.Header.Get("Authorization")
		extra = r.Header.Get("X-Title")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		sseHandler(t, fixture(t, "happy_text.sse"))(w, r)
	}), Options{
		APIKey:  func() string { return "sk-secret-value" },
		Headers: map[string]string{"X-Title": "holaclaw"},
	})

	req := provider.Request{Model: testModel(), System: "be brief", MaxTokens: 128,
		Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "hola")}}
	resp, deltas, err := chatOnce(t, c, req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Text != "Hola, Angel" {
		t.Errorf("text = %q", resp.Text)
	}
	if strings.Join(deltas, "") != "Hola, Angel" {
		t.Errorf("deltas = %q", deltas)
	}
	if resp.Finish != provider.FinishStop {
		t.Errorf("finish = %q", resp.Finish)
	}
	if resp.Usage.PromptTokens != 11 || resp.Usage.CompletionTokens != 3 || resp.Usage.Cost != nil {
		// No `cost` in the usage object (a plain OpenAI-compatible server): unpriced.
		t.Errorf("usage = %+v", resp.Usage)
	}
	if auth != "Bearer sk-secret-value" || extra != "holaclaw" {
		t.Errorf("headers: auth=%q extra=%q", auth, extra)
	}
	if !got.Stream || got.StreamOptions == nil || !got.StreamOptions.IncludeUsage {
		t.Errorf("stream options = %+v", got.StreamOptions)
	}
	if got.MaxTokens != 128 || got.Model != "gemma" {
		t.Errorf("request = %+v", got)
	}
	if len(got.Messages) != 2 || got.Messages[0].Role != "system" || got.Messages[0].Content != "be brief" {
		t.Errorf("messages = %+v", got.Messages)
	}
	if len(got.Tools) != 0 {
		t.Errorf("tools sent without any declared: %+v", got.Tools)
	}
}

func TestChatToolCallFragments(t *testing.T) {
	c := newTestClient(t, sseHandler(t, fixture(t, "tool_calls.sse")), Options{})
	resp, _, err := chatOnce(t, c, provider.Request{Model: testModel()})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Finish != provider.FinishToolCalls {
		t.Fatalf("finish = %q", resp.Finish)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("calls = %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].ID != "call_a" || resp.ToolCalls[0].Name != "weather" ||
		resp.ToolCalls[0].Arguments != `{"city":"Madrid"}` {
		t.Errorf("call 0 = %+v", resp.ToolCalls[0])
	}
	if resp.ToolCalls[1].ID != "call_b" || resp.ToolCalls[1].Arguments != "{}" {
		t.Errorf("call 1 = %+v", resp.ToolCalls[1])
	}
	if len(resp.Repairs) != 0 {
		t.Errorf("unexpected repairs: %v", resp.Repairs)
	}
}

func TestChatStopWithToolCallsBecomesToolCalls(t *testing.T) {
	body := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"t","arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	c := newTestClient(t, sseHandler(t, body), Options{})
	resp, _, err := chatOnce(t, c, provider.Request{Model: testModel()})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Finish != provider.FinishToolCalls {
		t.Errorf("finish = %q", resp.Finish)
	}
}

func TestChatTruncation(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"half a sen"},"finish_reason":null}]}

data: {"choices":[{"delta":{},"finish_reason":"length"}]}

data: [DONE]

`
	c := newTestClient(t, sseHandler(t, body), Options{})
	resp, _, err := chatOnce(t, c, provider.Request{Model: testModel()})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Finish != provider.FinishLength || resp.Text != "half a sen" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestChatDroppedStreamKeepsPartialText(t *testing.T) {
	// No finish_reason, no [DONE]: the connection just ends.
	body := `data: {"choices":[{"delta":{"content":"partial"}}]}

`
	c := newTestClient(t, sseHandler(t, body), Options{})
	resp, _, err := chatOnce(t, c, provider.Request{Model: testModel()})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Finish != provider.FinishDropped {
		t.Errorf("finish = %q", resp.Finish)
	}
	if resp.Text != "partial" {
		t.Errorf("text = %q", resp.Text)
	}
}

func TestChatDroppedStreamDropsIncompleteToolCall(t *testing.T) {
	body := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"ok","function":{"name":"a","arguments":"{\"x\":1}"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"bad","function":{"name":"b","arguments":"{\"y\":"}}]}}]}

`
	c := newTestClient(t, sseHandler(t, body), Options{})
	resp, _, err := chatOnce(t, c, provider.Request{Model: testModel()})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Finish != provider.FinishDropped {
		t.Errorf("finish = %q", resp.Finish)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "ok" {
		t.Fatalf("calls = %+v", resp.ToolCalls)
	}
	if len(resp.Repairs) != 0 {
		t.Errorf("incomplete call was repaired: %v", resp.Repairs)
	}
}

func TestChatRepairStagesRecorded(t *testing.T) {
	cases := []struct {
		name  string
		args  string
		stage string
		want  string
	}{
		{"strict", `{\"a\":1}`, "", `{"a":1}`},
		{"lenient", "```json\\n{\\\"a\\\":1}\\n```", wire.StageLenient, `{"a":1}`},
		{"trailing-comma", `{\"a\":1,}`, wire.StageTrailingComma, `{"a":1}`},
		{"auto-close", `{\"a\":\"b`, wire.StageAutoClose, `{"a":"b"}`},
		{"empty", `not json at all`, wire.StageEmpty, "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"t","arguments":"` + tc.args + `"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
			c := newTestClient(t, sseHandler(t, body), Options{})
			resp, _, err := chatOnce(t, c, provider.Request{Model: testModel()})
			if err != nil {
				t.Fatalf("chat: %v", err)
			}
			if len(resp.ToolCalls) != 1 {
				t.Fatalf("calls = %+v", resp.ToolCalls)
			}
			if got := resp.ToolCalls[0].Arguments; got != tc.want {
				t.Errorf("args = %q want %q", got, tc.want)
			}
			if tc.stage == "" {
				if len(resp.Repairs) != 0 {
					t.Errorf("repairs recorded for a strict parse: %v", resp.Repairs)
				}
				return
			}
			if resp.Repairs["c1"] != tc.stage {
				t.Errorf("repairs = %v want %s", resp.Repairs, tc.stage)
			}
		})
	}
}

func TestChatScrubsReasoningTags(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"a<thi"}}]}

data: {"choices":[{"delta":{"content":"nk>hidden</thi"}}]}

data: {"choices":[{"delta":{"content":"nk>b"}}]}

data: {"choices":[{"delta":{"content":"<ponder>c</ponder>"}}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	c := newTestClient(t, sseHandler(t, body), Options{})
	m := testModel()
	m.ThinkingTags = []string{"ponder"}
	resp, deltas, err := chatOnce(t, c, provider.Request{Model: m})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Text != "ab" {
		t.Errorf("text = %q", resp.Text)
	}
	for _, d := range deltas {
		if strings.Contains(d, "hidden") || strings.Contains(d, "<") {
			t.Errorf("leaked tag text in delta %q", d)
		}
	}
}

func TestChatInStreamError(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"x"}}]}

data: {"error":{"message":"rate limited upstream","code":429}}

`
	c := newTestClient(t, sseHandler(t, body), Options{})
	_, _, err := chatOnce(t, c, provider.Request{Model: testModel()})
	if provider.KindOf(err) != provider.ErrRateLimit {
		t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
	}
	if !strings.Contains(err.Error(), "rate limited upstream") {
		t.Errorf("message = %q", err.Error())
	}
}

func TestChatProgressChunks(t *testing.T) {
	release := make(chan struct{})
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}), Options{ProgressInterval: 5 * time.Millisecond})

	s, err := c.Chat(context.Background(), provider.Request{Model: testModel()})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	defer func() { _ = s.Close() }()
	for i := 0; i < 2; i++ {
		ch, err := s.Next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if ch.Kind != provider.ChunkProgress {
			t.Fatalf("chunk %d kind = %v", i, ch.Kind)
		}
	}
	close(release)
	resp, err := provider.Collect(s, nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if resp.Text != "hi" {
		t.Errorf("text = %q", resp.Text)
	}
}

func TestChatContextCancelAbortsAndReleasesSlot(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}), Options{MaxConcurrent: 1, ProgressInterval: time.Hour})

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		s, err := c.Chat(ctx, provider.Request{Model: testModel()})
		if err != nil {
			t.Fatalf("chat %d: %v", i, err)
		}
		cancel()
		if _, err := s.Next(); err == nil {
			t.Fatalf("call %d: expected an error after cancel", i)
		} else if k := provider.KindOf(err); k != provider.ErrNetwork && k != provider.ErrTimeout {
			t.Fatalf("call %d: kind = %q", i, k)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		_ = s.Close() // Close must be idempotent: a double release would panic later
	}
	// The single slot must still be free.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.acquire(ctx); err != nil {
		t.Fatalf("slot leaked: %v", err)
	}
	c.release()
}

func TestChatConcurrencyLimit(t *testing.T) {
	started := make(chan struct{}, 8)
	hold := make(chan struct{})
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		started <- struct{}{}
		select {
		case <-hold:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}), Options{MaxConcurrent: 2})

	var streams []provider.Stream
	for i := 0; i < 2; i++ {
		s, err := c.Chat(context.Background(), provider.Request{Model: testModel()})
		if err != nil {
			t.Fatalf("chat %d: %v", i, err)
		}
		streams = append(streams, s)
		<-started
	}

	// The third call must block on the semaphore until a slot frees.
	third := make(chan error, 1)
	go func() {
		s, err := c.Chat(context.Background(), provider.Request{Model: testModel()})
		if s != nil {
			_ = s.Close()
		}
		third <- err
	}()
	select {
	case err := <-third:
		t.Fatalf("third call did not block: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(hold)
	_ = streams[0].Close()
	select {
	case err := <-third:
		if err != nil {
			t.Fatalf("third call: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("third call never got a slot")
	}
	_ = streams[1].Close()
}

func TestChatBlockedAcquireHonoursContext(t *testing.T) {
	c := New(Options{BaseURL: "http://127.0.0.1:1/v1", MaxConcurrent: 1})
	if err := c.acquire(context.Background()); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := c.Chat(ctx, provider.Request{Model: testModel()})
	if provider.KindOf(err) != provider.ErrTimeout {
		t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
	}
}

// --- request mapping -------------------------------------------------------

func TestChatImagesAndToolsWireFormat(t *testing.T) {
	var raw map[string]any
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		sseHandler(t, "data: [DONE]\n\n")(w, r)
	}), Options{})

	m := testModel()
	m.Vision = true
	req := provider.Request{
		Model: m,
		Messages: []provider.Message{
			{Role: provider.RoleUser, Parts: []provider.Part{
				{Text: "what is this?"},
				{Image: &provider.Image{MIME: "image/png", Data: []byte{1, 2, 3}}},
			}},
			{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "look", Arguments: `{"a":1}`}}},
			{Role: provider.RoleTool, ToolCallID: "c1", ToolName: "look", Parts: []provider.Part{{Text: "a cat"}}},
		},
		Tools: []provider.ToolSpec{{Name: "look", Description: "look", Parameters: json.RawMessage(`{"type":"object"}`)}},
	}
	s, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	_, _ = provider.Collect(s, nil)
	_ = s.Close()

	msgs, _ := raw["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages = %v", msgs)
	}
	user := msgs[0].(map[string]any)
	parts, ok := user["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("user content = %#v", user["content"])
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Errorf("part type = %v", img["type"])
	}
	url := img["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Errorf("image url = %q", url)
	}
	asst := msgs[1].(map[string]any)
	calls := asst["tool_calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["id"] != "c1" {
		t.Errorf("assistant = %#v", asst)
	}
	tool := msgs[2].(map[string]any)
	if tool["tool_call_id"] != "c1" || tool["content"] != "a cat" || tool["name"] != "look" {
		t.Errorf("tool message = %#v", tool)
	}
	if tools, _ := raw["tools"].([]any); len(tools) != 1 {
		t.Errorf("tools = %#v", raw["tools"])
	}
}

func TestChatToolsOmittedWhenModelHasNoTools(t *testing.T) {
	var raw map[string]any
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		sseHandler(t, "data: [DONE]\n\n")(w, r)
	}), Options{})
	m := testModel()
	m.Tools = false
	s, err := c.Chat(context.Background(), provider.Request{Model: m,
		Tools: []provider.ToolSpec{{Name: "x", Parameters: json.RawMessage(`{"type":"object"}`)}}})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	_, _ = provider.Collect(s, nil)
	_ = s.Close()
	if _, ok := raw["tools"]; ok {
		t.Errorf("tools sent to a non-tool model: %#v", raw["tools"])
	}
	if _, ok := raw["max_tokens"]; ok {
		t.Errorf("max_tokens sent when unset: %#v", raw["max_tokens"])
	}
}

// --- HTTP errors -----------------------------------------------------------

func TestChatHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   provider.ErrorKind
	}{
		{http.StatusUnauthorized, `{"error":{"message":"bad key"}}`, provider.ErrAuth},
		{http.StatusForbidden, `{"error":{"message":"nope"}}`, provider.ErrAuth},
		{http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, provider.ErrRateLimit},
		{http.StatusBadRequest, `{"error":{"message":"this model's maximum context length is 4096 tokens"}}`, provider.ErrContext},
		{http.StatusBadRequest, `{"error":{"message":"unknown field"}}`, provider.ErrBadRequest},
		{http.StatusNotFound, `{"error":{"message":"no such model"}}`, provider.ErrBadRequest},
		{http.StatusInternalServerError, `{"error":{"message":"boom"}}`, provider.ErrServer},
		{http.StatusBadGateway, `upstream exploded`, provider.ErrServer},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}), Options{APIKey: func() string { return "sk-super-secret" }})
			_, _, err := chatOnce(t, c, provider.Request{Model: testModel()})
			if provider.KindOf(err) != tc.want {
				t.Fatalf("kind = %q want %q (%v)", provider.KindOf(err), tc.want, err)
			}
			var pe *provider.Error
			if !errors.As(err, &pe) || pe.Status != tc.status {
				t.Fatalf("error = %#v", err)
			}
			if strings.Contains(err.Error(), "sk-super-secret") {
				t.Fatal("error leaked the api key")
			}
		})
	}
}

func TestChatRetryAfter(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}), Options{})
	_, _, err := chatOnce(t, c, provider.Request{Model: testModel()})
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v", err)
	}
	if pe.RetryAfter != 42*time.Second {
		t.Errorf("retry-after = %v", pe.RetryAfter)
	}
}

func TestChatErrorNeverLeaksAuthorization(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid Authorization: Bearer sk-super-secret"}}`)
	}), Options{APIKey: func() string { return "sk-super-secret" }})
	_, _, err := chatOnce(t, c, provider.Request{Model: testModel()})
	if strings.Contains(err.Error(), "sk-super-secret") || strings.Contains(err.Error(), "Bearer") {
		t.Fatalf("leaked credentials: %v", err)
	}
}

func TestChatNetworkErrorKind(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	c := New(Options{Name: "local", BaseURL: url + "/v1"})
	_, _, err := chatOnce(t, c, provider.Request{Model: testModel()})
	if provider.KindOf(err) != provider.ErrNetwork {
		t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
	}
}

func TestChatRequestTimeout(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second): // never reached: the client gives up first
		}
	}), Options{DefaultTimeout: 30 * time.Millisecond})
	m := testModel()
	m.Timeout = 0
	_, _, err := chatOnce(t, c, provider.Request{Model: m})
	if provider.KindOf(err) != provider.ErrTimeout {
		t.Fatalf("kind = %q (%v)", provider.KindOf(err), err)
	}
}

// The final usage chunk: OpenRouter adds the call's price as `usage.cost`
// (USD), and servers that cache or reason add the details. Each lands on
// Response.Usage when sent and stays nil when not — a reported 0 is a free
// call, an absent cost an unpriced one.
func TestChatUsageCost(t *testing.T) {
	cases := []struct {
		name, usage string
		want        provider.Usage
	}{
		{"priced", `{"prompt_tokens":120,"completion_tokens":7,"cost":0.00042,"is_byok":false}`,
			provider.Usage{PromptTokens: 120, CompletionTokens: 7, Cost: nano(420000)}},
		{"exponent", `{"prompt_tokens":120,"completion_tokens":7,"cost":1.2e-05}`,
			provider.Usage{PromptTokens: 120, CompletionTokens: 7, Cost: nano(12000)}},
		{"free", `{"prompt_tokens":120,"completion_tokens":7,"cost":0}`,
			provider.Usage{PromptTokens: 120, CompletionTokens: 7, Cost: nano(0)}},
		{"malformed is unpriced", `{"prompt_tokens":120,"completion_tokens":7,"cost":"0.1"}`,
			provider.Usage{PromptTokens: 120, CompletionTokens: 7}},
		{"negative is unpriced", `{"prompt_tokens":120,"completion_tokens":7,"cost":-0.1}`,
			provider.Usage{PromptTokens: 120, CompletionTokens: 7}},
		{"unpriced", `{"prompt_tokens":120,"completion_tokens":7}`,
			provider.Usage{PromptTokens: 120, CompletionTokens: 7}},
		{"details", `{"prompt_tokens":120,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":100},"completion_tokens_details":{"reasoning_tokens":0}}`,
			provider.Usage{PromptTokens: 120, CompletionTokens: 7, CachedTokens: ip(100), ReasoningTokens: ip(0)}},
		{"empty details", `{"prompt_tokens":120,"completion_tokens":7,"prompt_tokens_details":{},"completion_tokens_details":null}`,
			provider.Usage{PromptTokens: 120, CompletionTokens: 7}},
	}
	for _, tc := range cases {
		body := `data: {"id":"1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}

data: {"id":"1","choices":[],"usage":` + tc.usage + `}

data: [DONE]

`
		c := newTestClient(t, sseHandler(t, body), Options{})
		resp, _, err := chatOnce(t, c, provider.Request{Model: testModel(),
			Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "hola")}})
		if err != nil {
			t.Fatalf("%s: chat: %v", tc.name, err)
		}
		if !reflect.DeepEqual(resp.Usage, tc.want) {
			t.Errorf("%s: usage = %s, want %s", tc.name, usageText(resp.Usage), usageText(tc.want))
		}
	}
}

// A stream that ends without any usage chunk reports zero tokens and
// nothing else.
func TestChatUsageAbsent(t *testing.T) {
	body := `data: {"id":"1","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}

data: [DONE]

`
	c := newTestClient(t, sseHandler(t, body), Options{})
	resp, _, err := chatOnce(t, c, provider.Request{Model: testModel(),
		Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "hola")}})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if !reflect.DeepEqual(resp.Usage, provider.Usage{}) {
		t.Errorf("usage = %s, want zero", usageText(resp.Usage))
	}
}

func nano(v provider.NanoUSD) *provider.NanoUSD { return &v }
func ip(v int) *int                             { return &v }

func usageText(u provider.Usage) string {
	b, _ := json.Marshal(u)
	return string(b)
}
