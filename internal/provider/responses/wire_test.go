package responses

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"holaclaw/internal/provider"
)

// field walks a decoded JSON body.
func field(t *testing.T, m map[string]any, key string) any {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("body has no %q: %v", key, m)
	}
	return v
}

func TestBuildRequestShape(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, captureBody(t, fixture(t, "text.sse"), &got, nil), Options{})

	req := testRequest()
	req.MaxTokens = 256
	req.Tools = []provider.ToolSpec{{
		Name:        "weather",
		Description: "current weather",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}}
	req.Messages = []provider.Message{
		provider.TextMessage(provider.RoleUser, "hola"),
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "call_a", Name: "weather", Arguments: `{"city":"Madrid"}`}}},
		{Role: provider.RoleTool, ToolCallID: "call_a", ToolName: "weather", Parts: []provider.Part{{Text: "18C"}}},
		{Role: provider.RoleAssistant, Parts: []provider.Part{{Text: "Hace 18 grados."}}},
	}
	if _, _, err := chatOnce(t, c, req); err != nil {
		t.Fatalf("chat: %v", err)
	}

	if got["stream"] != true {
		t.Errorf("stream = %v", got["stream"])
	}
	if got["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls = %v", got["parallel_tool_calls"])
	}
	if got["instructions"] != "You are holaclaw." {
		t.Errorf("instructions = %v", got["instructions"])
	}
	if got["max_output_tokens"] != float64(256) {
		t.Errorf("max_output_tokens = %v", got["max_output_tokens"])
	}

	tools, _ := field(t, got, "tools").([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", tools)
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "weather" || tool["strict"] != false {
		t.Errorf("tool = %v", tool)
	}
	if _, nested := tool["function"]; nested {
		t.Errorf("tool must be flat, got %v", tool)
	}
	if _, ok := tool["parameters"].(map[string]any); !ok {
		t.Errorf("parameters = %v", tool["parameters"])
	}

	input, _ := field(t, got, "input").([]any)
	if len(input) != 4 {
		t.Fatalf("input = %v", input)
	}
	user, _ := input[0].(map[string]any)
	if user["type"] != "message" || user["role"] != "user" {
		t.Errorf("user item = %v", user)
	}
	uc, _ := user["content"].([]any)
	if p, _ := uc[0].(map[string]any); p["type"] != "input_text" || p["text"] != "hola" {
		t.Errorf("user content = %v", uc)
	}
	call, _ := input[1].(map[string]any)
	if call["type"] != "function_call" || call["call_id"] != "call_a" ||
		call["name"] != "weather" || call["arguments"] != `{"city":"Madrid"}` {
		t.Errorf("call item = %v", call)
	}
	out, _ := input[2].(map[string]any)
	if out["type"] != "function_call_output" || out["call_id"] != "call_a" || out["output"] != "18C" {
		t.Errorf("output item = %v", out)
	}
	asst, _ := input[3].(map[string]any)
	ac, _ := asst["content"].([]any)
	if p, _ := ac[0].(map[string]any); p["type"] != "output_text" {
		t.Errorf("assistant content = %v", ac)
	}
}

func TestInstructionsNeverEmpty(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, captureBody(t, fixture(t, "text.sse"), &got, nil), Options{})
	req := testRequest()
	req.System = ""
	if _, _, err := chatOnce(t, c, req); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if got["instructions"] != DefaultInstructions {
		t.Errorf("instructions = %v", got["instructions"])
	}
	if strings.TrimSpace(DefaultInstructions) == "" {
		t.Error("default instructions must not be blank")
	}
}

func TestStoreAlwaysPresent(t *testing.T) {
	for _, tc := range []struct {
		name string
		opt  *bool
		want bool
	}{
		{"default", nil, false},
		{"explicit false", new(bool), false},
		{"explicit true", func() *bool { b := true; return &b }(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			c := newTestClient(t, captureBody(t, fixture(t, "text.sse"), &got, nil), Options{Store: tc.opt})
			if _, _, err := chatOnce(t, c, testRequest()); err != nil {
				t.Fatalf("chat: %v", err)
			}
			v, ok := got["store"]
			if !ok {
				t.Fatalf("store missing: %v", got)
			}
			if v != tc.want {
				t.Errorf("store = %v, want %v", v, tc.want)
			}
		})
	}
}

func TestOmitMaxOutputTokens(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, captureBody(t, fixture(t, "text.sse"), &got, nil), Options{OmitMaxOutputTokens: true})
	req := testRequest()
	req.MaxTokens = 1024
	if _, _, err := chatOnce(t, c, req); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if _, ok := got["max_output_tokens"]; ok {
		t.Errorf("max_output_tokens must be absent: %v", got)
	}
}

func TestImageInputWireShape(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, captureBody(t, fixture(t, "text.sse"), &got, nil), Options{})
	req := testRequest()
	req.Messages = []provider.Message{{Role: provider.RoleUser, Parts: []provider.Part{
		{Text: "what is this?"},
		{Image: &provider.Image{MIME: "image/jpeg", Data: []byte{0xff, 0xd8, 0xff}}},
	}}}
	if _, _, err := chatOnce(t, c, req); err != nil {
		t.Fatalf("chat: %v", err)
	}
	input, _ := field(t, got, "input").([]any)
	msg, _ := input[0].(map[string]any)
	content, _ := msg["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %v", content)
	}
	img, _ := content[1].(map[string]any)
	if img["type"] != "input_image" {
		t.Errorf("image part type = %v", img["type"])
	}
	url, _ := img["image_url"].(string)
	if !strings.HasPrefix(url, "data:image/jpeg;base64,") {
		t.Errorf("image_url = %q", url)
	}
	if _, nested := img["text"]; nested {
		t.Errorf("image part carries text: %v", img)
	}
}

func TestToolsOnlyWhenModelSupportsThem(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, captureBody(t, fixture(t, "text.sse"), &got, nil), Options{})
	req := testRequest()
	req.Model.Tools = false
	req.Tools = []provider.ToolSpec{{Name: "weather"}}
	if _, _, err := chatOnce(t, c, req); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if _, ok := got["tools"]; ok {
		t.Errorf("tools advertised to a non-tool model: %v", got)
	}
}

func TestDynamicHeadersPerRequest(t *testing.T) {
	var hdr http.Header
	var n int
	c := newTestClient(t, captureBody(t, fixture(t, "text.sse"), nil, &hdr), Options{
		APIKey: func() string { return "sk-test-key-123456" },
		Headers: func(req provider.Request) map[string]string {
			n++
			return map[string]string{
				"chatgpt-account-id": "acct-" + req.SessionID,
				"originator":         "holaclaw",
				"session_id":         req.SessionID,
			}
		},
	})
	req := testRequest()
	req.SessionID = "sess-one"
	if _, _, err := chatOnce(t, c, req); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if got := hdr.Get("chatgpt-account-id"); got != "acct-sess-one" {
		t.Errorf("account id = %q", got)
	}
	if got := hdr.Get("Authorization"); got != "Bearer sk-test-key-123456" {
		t.Errorf("authorization = %q", got)
	}
	if got := hdr.Get("Originator"); got != "holaclaw" {
		t.Errorf("originator = %q", got)
	}

	req.SessionID = "sess-two"
	if _, _, err := chatOnce(t, c, req); err != nil {
		t.Fatalf("chat 2: %v", err)
	}
	if got := hdr.Get("session_id"); got != "sess-two" {
		t.Errorf("second session_id = %q", got)
	}
	if n < 2 {
		t.Errorf("Headers called %d times, want one per request", n)
	}
}

func TestBaseURLAndPathDefaults(t *testing.T) {
	if got := New(Options{}).url; got != DefaultBaseURL+DefaultPath {
		t.Errorf("default url = %q", got)
	}
	// Codex points BaseURL+Path at its own backend.
	got := New(Options{BaseURL: "https://chatgpt.com/backend-api/codex", Path: "/responses"}).url
	if got != "https://chatgpt.com/backend-api/codex/responses" {
		t.Errorf("codex url = %q", got)
	}
}
