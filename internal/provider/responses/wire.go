package responses

import (
	"encoding/base64"
	"encoding/json"

	"holaclaw/internal/provider"
)

// Request wire types. Only the fields holaclaw sends are modelled.

type wireRequest struct {
	Model string `json:"model"`
	// Instructions carries the system prompt and is never empty.
	Instructions string `json:"instructions"`
	// Input is always present, even when empty.
	Input  []inputItem `json:"input"`
	Stream bool        `json:"stream"`
	// Store is always present on the wire, false by default: no server-side
	// retention of a conversation the daemon already owns.
	Store             bool       `json:"store"`
	Tools             []wireTool `json:"tools,omitempty"`
	MaxOutputTokens   int        `json:"max_output_tokens,omitempty"`
	ParallelToolCalls bool       `json:"parallel_tool_calls"`
}

// inputItem is one element of `input`: a message, a function call the model
// made earlier, or the output of one.
type inputItem struct {
	Type      string        `json:"type"`
	Role      string        `json:"role,omitempty"`
	Content   []contentPart `json:"content,omitempty"`
	CallID    string        `json:"call_id,omitempty"`
	Name      string        `json:"name,omitempty"`
	Arguments string        `json:"arguments,omitempty"`
	// Output is a pointer so an empty tool result is still sent.
	Output *string `json:"output,omitempty"`
}

type contentPart struct {
	Type     string `json:"type"` // input_text | output_text | input_image
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"` // a data: URI
}

// wireTool is flat: name/description/parameters sit on the tool itself, not
// nested under "function" as in chat completions.
type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      bool            `json:"strict"`
}

// buildRequest maps a provider.Request onto the Responses wire format.
func (c *Client) buildRequest(req provider.Request) wireRequest {
	names := toolNames(req)
	w := wireRequest{
		Model:             req.Model.ID,
		Instructions:      req.System,
		Stream:            true,
		Input:             []inputItem{},
		Store:             c.store,
		ParallelToolCalls: false,
	}
	if w.Instructions == "" {
		w.Instructions = DefaultInstructions
	}
	if req.MaxTokens > 0 && !c.omitMaxOut {
		w.MaxOutputTokens = req.MaxTokens
	}
	for _, m := range req.Messages {
		items := buildItems(m)
		for i := range items {
			if items[i].Type == "function_call" {
				items[i].Name = names[items[i].Name]
			}
		}
		w.Input = append(w.Input, items...)
	}
	// Tools are only advertised to models that claim native tool calling.
	if req.Model.Tools && len(req.Tools) > 0 {
		for _, t := range req.Tools {
			w.Tools = append(w.Tools, wireTool{
				Type:        "function",
				Name:        names[t.Name],
				Description: t.Description,
				Parameters:  t.Parameters,
				Strict:      false,
			})
		}
	}
	return w
}

// buildItems turns one message into input items. An assistant turn with both
// text and tool calls becomes a message item followed by one function_call item
// per call, in order.
func buildItems(m provider.Message) []inputItem {
	if m.Role == provider.RoleTool {
		out := m.Text()
		return []inputItem{{Type: "function_call_output", CallID: m.ToolCallID, Output: &out}}
	}
	var items []inputItem
	if parts := buildParts(m); len(parts) > 0 {
		items = append(items, inputItem{Type: "message", Role: string(m.Role), Content: parts})
	}
	for _, tc := range m.ToolCalls {
		args := tc.Arguments
		if args == "" {
			args = "{}"
		}
		items = append(items, inputItem{
			Type:      "function_call",
			CallID:    tc.ID,
			Name:      tc.Name,
			Arguments: args,
		})
	}
	return items
}

// buildParts maps message parts onto content parts. Text written by the model
// is output_text; everything else is input. Images travel as data: URIs.
func buildParts(m provider.Message) []contentPart {
	textType := "input_text"
	if m.Role == provider.RoleAssistant {
		textType = "output_text"
	}
	var out []contentPart
	for _, p := range m.Parts {
		switch {
		case p.Image != nil:
			mime := p.Image.MIME
			if mime == "" {
				mime = "image/png"
			}
			out = append(out, contentPart{
				Type:     "input_image",
				ImageURL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(p.Image.Data),
			})
		case p.Text != "":
			out = append(out, contentPart{Type: textType, Text: p.Text})
		}
	}
	return out
}

// Streaming event types. The Responses API names every frame with `event:` and
// repeats the name in the payload's "type".

type wireEvent struct {
	Type string `json:"type"`
	// Text deltas.
	Delta string `json:"delta"`
	// Item identity; output_index is a pointer so index 0 is distinguishable
	// from an absent field.
	ItemID      string     `json:"item_id"`
	OutputIndex *int       `json:"output_index"`
	Item        *wireItem  `json:"item"`
	Arguments   string     `json:"arguments"`
	Response    *wireResp  `json:"response"`
	Error       *wireError `json:"error"`
	// Some error frames are flat: the fields sit on the event itself.
	Code            string       `json:"code"`
	Message         string       `json:"message"`
	ResetsAt        *json.Number `json:"resets_at"`
	ResetsInSeconds *json.Number `json:"resets_in_seconds"`
}

type wireItem struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireResp struct {
	Status            string      `json:"status"`
	Usage             *wireUsage  `json:"usage"`
	IncompleteDetails *wireReason `json:"incomplete_details"`
	Error             *wireError  `json:"error"`
}

type wireReason struct {
	Reason string `json:"reason"`
}

// wireUsage is a finished response's usage. The details are pointers so a
// field the server leaves out stays nil, not zero. There is no cost: a
// subscription call is not priced per call.
type wireUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens *int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type wireError struct {
	Type            string       `json:"type"`
	Code            string       `json:"code"`
	Message         string       `json:"message"`
	ResetsAt        *json.Number `json:"resets_at"`
	ResetsInSeconds *json.Number `json:"resets_in_seconds"`
}
