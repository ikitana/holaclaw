package openaicompat

import (
	"encoding/base64"
	"encoding/json"

	"holaclaw/internal/provider"
)

// Request wire types. Only the fields holaclaw sends are modelled.

type wireRequest struct {
	Model         string         `json:"model"`
	Messages      []wireMessage  `json:"messages"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
	Tools         []wireTool     `json:"tools,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	// Reasoning is OpenRouter's unified reasoning control; OpenAI-compatible
	// servers without it ignore the field. Ollama's /v1 reads its effort and,
	// when the object is present, ignores ReasoningEffort: an effort set here
	// always carries Effort, or Ollama falls back to the model's default.
	Reasoning *wireReasoning `json:"reasoning,omitempty"`
	// ReasoningEffort is OpenAI's control; it is sent next to Reasoning, with
	// the same effort.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// Provider is OpenRouter's routing block.
	Provider *wireProvider `json:"provider,omitempty"`
}

type wireProvider struct {
	Sort string `json:"sort,omitempty"`
}

type wireReasoning struct {
	Effort  string `json:"effort,omitempty"`
	Enabled *bool  `json:"enabled,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"` // string or []contentPart
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireToolFunc `json:"function"`
}

type wireToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// buildRequest maps a provider.Request onto the chat-completions wire format.
func buildRequest(req provider.Request) wireRequest {
	w := wireRequest{
		Model:         req.Model.ID,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	if req.MaxTokens > 0 {
		w.MaxTokens = req.MaxTokens
	}
	switch e := req.Effort(); e {
	case provider.ReasoningDefault:
	case provider.ReasoningNone:
		off := false
		w.Reasoning = &wireReasoning{Effort: string(e), Enabled: &off}
		w.ReasoningEffort = string(e)
	default:
		w.Reasoning = &wireReasoning{Effort: string(e)}
		w.ReasoningEffort = string(e)
	}
	if req.System != "" {
		w.Messages = append(w.Messages, wireMessage{Role: string(provider.RoleSystem), Content: req.System})
	}
	for _, m := range req.Messages {
		w.Messages = append(w.Messages, buildMessage(m))
	}
	// Tools are only advertised to models that claim native tool calling.
	if req.Model.Tools && len(req.Tools) > 0 {
		for _, t := range req.Tools {
			w.Tools = append(w.Tools, wireTool{
				Type: "function",
				Function: wireToolFunc{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  sanitizeSchema(t.Parameters, req.Model.SchemaProfile),
				},
			})
		}
	}
	return w
}

func buildMessage(m provider.Message) wireMessage {
	w := wireMessage{Role: string(m.Role)}
	switch m.Role {
	case provider.RoleTool:
		w.Content = m.Text()
		w.ToolCallID = m.ToolCallID
		w.Name = m.ToolName
		return w
	case provider.RoleAssistant:
		for _, tc := range m.ToolCalls {
			args := tc.Arguments
			if args == "" {
				args = "{}"
			}
			w.ToolCalls = append(w.ToolCalls, wireToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: wireFunction{Name: tc.Name, Arguments: args},
			})
		}
	case provider.RoleSystem, provider.RoleUser:
	}
	if txt, parts, hasImage := splitParts(m.Parts); hasImage {
		w.Content = parts
	} else if txt != "" || len(w.ToolCalls) == 0 {
		w.Content = txt
	}
	return w
}

// splitParts flattens parts to a plain string, or to a content-part array when
// any image is present (images travel as data: URIs).
func splitParts(parts []provider.Part) (text string, out []contentPart, hasImage bool) {
	for _, p := range parts {
		if p.Image != nil {
			hasImage = true
		}
	}
	for _, p := range parts {
		switch {
		case p.Image != nil:
			mime := p.Image.MIME
			if mime == "" {
				mime = "image/png"
			}
			out = append(out, contentPart{
				Type:     "image_url",
				ImageURL: &imageURL{URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(p.Image.Data)},
			})
		case p.Text != "":
			text += p.Text
			if hasImage {
				out = append(out, contentPart{Type: "text", Text: p.Text})
			}
		}
	}
	return text, out, hasImage
}

// Streaming response wire types.

type wireChunk struct {
	Choices []wireChoice    `json:"choices"`
	Usage   *wireUsage      `json:"usage"`
	Error   json.RawMessage `json:"error"`
}

type wireChoice struct {
	Delta        wireDelta `json:"delta"`
	FinishReason string    `json:"finish_reason"`
}

type wireDelta struct {
	Content   string          `json:"content"`
	ToolCalls []wireDeltaCall `json:"tool_calls"`
	// Reasoning fields are decoded only so they are never mistaken for
	// visible content; their value is dropped.
	ReasoningContent json.RawMessage `json:"reasoning_content"`
	Reasoning        json.RawMessage `json:"reasoning"`
}

type wireDeltaCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// wireUsage is the usage object of the final chunk (and of an embeddings
// reply). Cost is OpenRouter's extension (USD for the call), kept as text
// for provider.WireCost; other servers leave it out and it stays nil, as do
// the details a server does not send.
type wireUsage struct {
	PromptTokens        int             `json:"prompt_tokens"`
	CompletionTokens    int             `json:"completion_tokens"`
	Cost                json.RawMessage `json:"cost"`
	PromptTokensDetails *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// usage is the provider's view of u.
func (u *wireUsage) usage() provider.Usage {
	if u == nil {
		return provider.Usage{}
	}
	out := provider.Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, Cost: provider.WireCost(u.Cost)}
	if u.PromptTokensDetails != nil {
		out.CachedTokens = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		out.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	return out
}
