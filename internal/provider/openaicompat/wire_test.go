package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"holaclaw/internal/provider"
)

func buildRequestFixture(params json.RawMessage, profile string) wireRequest {
	m := provider.Model{ID: "m", Tools: true, SchemaProfile: profile}
	return buildRequest(provider.Request{
		Model: m,
		Tools: []provider.ToolSpec{{Name: "t", Description: "d", Parameters: params}},
	})
}

func TestBuildMessageTextOnly(t *testing.T) {
	w := buildMessage(provider.TextMessage(provider.RoleUser, "hola"))
	if w.Role != "user" || w.Content != "hola" {
		t.Errorf("message = %+v", w)
	}
}

func TestBuildMessageImagesBecomeContentParts(t *testing.T) {
	w := buildMessage(provider.Message{Role: provider.RoleUser, Parts: []provider.Part{
		{Text: "look"},
		{Image: &provider.Image{Data: []byte("png")}},
	}})
	parts, ok := w.Content.([]contentPart)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v", w.Content)
	}
	if parts[0].Type != "text" || parts[0].Text != "look" {
		t.Errorf("text part = %+v", parts[0])
	}
	// An image with no MIME falls back to PNG rather than an invalid URI.
	if !strings.HasPrefix(parts[1].ImageURL.URL, "data:image/png;base64,") {
		t.Errorf("image part = %+v", parts[1].ImageURL)
	}
}

func TestBuildMessageAssistantToolCalls(t *testing.T) {
	w := buildMessage(provider.Message{
		Role:      provider.RoleAssistant,
		ToolCalls: []provider.ToolCall{{ID: "c1", Name: "t", Arguments: ""}},
	})
	if len(w.ToolCalls) != 1 || w.ToolCalls[0].Function.Arguments != "{}" {
		t.Fatalf("tool calls = %+v", w.ToolCalls)
	}
	if w.Content != nil {
		t.Errorf("content should be omitted for a pure tool-call turn: %#v", w.Content)
	}
}

func TestBuildRequestSystemFirstAndMaxTokens(t *testing.T) {
	r := buildRequest(provider.Request{
		Model:    provider.Model{ID: "m"},
		System:   "rules",
		Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "hi")},
	})
	if len(r.Messages) != 2 || r.Messages[0].Role != "system" {
		t.Fatalf("messages = %+v", r.Messages)
	}
	if r.MaxTokens != 0 {
		t.Errorf("max_tokens set without a reserve: %d", r.MaxTokens)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "max_tokens") {
		t.Errorf("max_tokens on the wire: %s", b)
	}
}

func TestBuildRequestReasoning(t *testing.T) {
	for _, tc := range []struct {
		in   provider.Reasoning
		want string
	}{
		{provider.ReasoningDefault, ""},
		{provider.ReasoningNone, `"reasoning":{"effort":"none","enabled":false},"reasoning_effort":"none"`},
		{provider.ReasoningLow, `"reasoning":{"effort":"low"},"reasoning_effort":"low"`},
		{"medium", `"reasoning":{"effort":"medium"},"reasoning_effort":"medium"`},
	} {
		r := buildRequest(provider.Request{Model: provider.Model{ID: "m"}, Reasoning: tc.in,
			Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "hi")}})
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if tc.want == "" && strings.Contains(string(b), "reasoning") {
			t.Errorf("%q: reasoning sent when unset: %s", tc.in, b)
		}
		if tc.want != "" && !strings.Contains(string(b), tc.want) {
			t.Errorf("%q: want %s in %s", tc.in, tc.want, b)
		}
	}
}

// The model's effort applies to a call that sets none; a call's own wins.
func TestBuildRequestModelReasoning(t *testing.T) {
	for _, tc := range []struct {
		model, call provider.Reasoning
		want        string
	}{
		{provider.ReasoningNone, provider.ReasoningDefault, `"reasoning_effort":"none"`},
		{provider.ReasoningNone, provider.ReasoningLow, `"reasoning_effort":"low"`},
		{provider.ReasoningLow, provider.ReasoningNone, `"reasoning_effort":"none"`},
		{"high", provider.ReasoningDefault, `"reasoning_effort":"high"`},
	} {
		r := buildRequest(provider.Request{Model: provider.Model{ID: "m", Reasoning: tc.model}, Reasoning: tc.call,
			Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "hi")}})
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), tc.want) {
			t.Errorf("model %q, call %q: want %s in %s", tc.model, tc.call, tc.want, b)
		}
	}
}

func TestSortGoesOutAsProviderBlock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		p, _ := got["provider"].(map[string]any)
		if p["sort"] != "throughput" {
			t.Errorf("provider block = %v", got["provider"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	c := New(Options{Name: "t", BaseURL: srv.URL, Sort: "throughput"})
	s, err := c.Chat(context.Background(), provider.Request{Model: provider.Model{ID: "m"}, Messages: []provider.Message{provider.TextMessage(provider.RoleUser, "hi")}})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
}
