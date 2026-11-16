package openaicompat

import (
	"encoding/json"
	"strings"
	"testing"

	"holaclaw/internal/provider"
)

func TestStreamErrorKinds(t *testing.T) {
	c := New(Options{Name: "or", BaseURL: "http://x/v1"})
	cases := []struct {
		raw  string
		want provider.ErrorKind
	}{
		{`{"message":"key invalid","code":401}`, provider.ErrAuth},
		{`{"message":"slow down","code":"429"}`, provider.ErrRateLimit},
		{`{"message":"maximum context length exceeded","code":400}`, provider.ErrContext},
		{`{"message":"bad tool schema","code":400}`, provider.ErrBadRequest},
		{`{"message":"upstream died","code":502}`, provider.ErrServer},
		{`{"message":"something odd"}`, provider.ErrServer},
		{`{"message":"the prompt is too long for this model"}`, provider.ErrContext},
	}
	for _, tc := range cases {
		e := c.streamError(json.RawMessage(tc.raw))
		if e.Kind != tc.want {
			t.Errorf("streamError(%s) kind = %q want %q", tc.raw, e.Kind, tc.want)
		}
	}
}

func TestRedactHidesKey(t *testing.T) {
	c := New(Options{Name: "p", BaseURL: "http://x/v1", APIKey: func() string { return "sk-abcdefgh1234" }})
	got := c.redact("bad token sk-abcdefgh1234 in Authorization: Bearer sk-abcdefgh1234")
	if strings.Contains(got, "sk-abcdefgh1234") || strings.Contains(got, "Bearer") {
		t.Fatalf("redact leaked: %q", got)
	}
}

func TestFinishReasonMapping(t *testing.T) {
	cases := []struct {
		raw      string
		hasCalls bool
		want     provider.FinishReason
	}{
		{"stop", false, provider.FinishStop},
		{"stop", true, provider.FinishToolCalls},
		{"length", false, provider.FinishLength},
		{"max_tokens", false, provider.FinishLength},
		{"tool_calls", true, provider.FinishToolCalls},
		{"function_call", true, provider.FinishToolCalls},
		{"", false, provider.FinishDropped},
		{"", true, provider.FinishDropped},
	}
	for _, tc := range cases {
		if got := finishReason(tc.raw, tc.hasCalls); got != tc.want {
			t.Errorf("finishReason(%q,%v) = %q want %q", tc.raw, tc.hasCalls, got, tc.want)
		}
	}
}
