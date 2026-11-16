package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/provider/openaicompat"
)

// The turn after a reaction-only turn: its empty answer is left out of the
// prompt, so the wire carries no assistant entry with "content": "" (a 400
// on Anthropic and Gemini through OpenRouter). The tool call and its result
// stay, so the model still knows it reacted.
func TestSilentReplyIsNotReplayed(t *testing.T) {
	in := budgetInput(8192,
		userMsg("01", "deployed?"),
		asstMsg("02", "", event.ToolCall{ID: "c1", Name: "react", Arguments: `{"emoji":"👍"}`}),
		toolMsg("03", "c1", "react", "reacted 👍"),
		asstMsg("04", ""),
		userMsg("05", "thanks"),
	)
	p := build(t, in)
	for _, id := range p.MessageIDs {
		if id == "04" {
			t.Fatalf("the silent reply is in the prompt: %v", p.MessageIDs)
		}
	}

	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	c := openaicompat.New(openaicompat.Options{Name: "t", BaseURL: srv.URL})
	s, err := c.Chat(context.Background(), provider.Request{
		Model: provider.Model{ID: "m", Tools: true}, System: p.System, Messages: p.Messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	calls := 0
	for _, m := range body.Messages {
		if m["role"] != "assistant" {
			continue
		}
		if c, ok := m["content"]; ok && c == "" {
			t.Fatalf("an assistant entry with empty content went out: %v", body.Messages)
		}
		if _, ok := m["tool_calls"]; ok {
			calls++
		}
	}
	if calls != 1 || body.Messages[len(body.Messages)-1]["content"] == nil {
		t.Fatalf("messages = %v", body.Messages)
	}
}
