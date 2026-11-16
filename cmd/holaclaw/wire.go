package main

import (
	"encoding/json"
	"io"

	"holaclaw/internal/api/gen"
	"holaclaw/internal/event"
)

// The offline `trace` path reads the store directly, so it has to render the
// same wire shapes the API would have returned. Only the fields the trace
// view prints are filled; anything else is left at its zero value.

func storeEvent(e event.Event) gen.Event {
	out := gen.Event{
		Schema:    e.Schema,
		Id:        string(e.ID),
		TraceId:   string(e.TraceID),
		SpanId:    string(e.SpanID),
		ParentId:  optString(string(e.ParentID)),
		SessionId: optString(string(e.SessionID)),
		MessageId: optString(string(e.MessageID)),
		Type:      string(e.Type),
		Time:      e.Time,
	}
	if e.Duration > 0 {
		ns := int64(e.Duration)
		out.DurationNs = &ns
	}
	if len(e.Attrs) > 0 {
		var attrs map[string]any
		if err := json.Unmarshal(e.Attrs, &attrs); err == nil {
			out.Attrs = &attrs
		}
	}
	return out
}

func storeMessage(m event.Message) gen.Message {
	return gen.Message{
		Schema:    m.Schema,
		Id:        string(m.ID),
		SessionId: string(m.SessionID),
		TraceId:   string(m.TraceID),
		Role:      gen.MessageRole(m.Role),
		Text:      optString(m.Text),
		ToolName:  optString(m.ToolName),
	}
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// writeJSON prints one value as indented JSON.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// compactJSON renders an attrs object on one line.
func compactJSON(attrs map[string]any) string {
	b, err := json.Marshal(attrs)
	if err != nil {
		return ""
	}
	return string(b)
}
