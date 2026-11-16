package mcp

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxURI caps the server-controlled uri in an omitted-resource line; a
// longer one is left out.
const maxURI = 1024

func convertTool(t *sdk.Tool) Tool {
	schema, err := json.Marshal(t.InputSchema)
	if err != nil || t.InputSchema == nil {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	out := Tool{Name: t.Name, Description: t.Description, InputSchema: schema}
	if a := t.Annotations; a != nil {
		out.ReadOnlyHint = a.ReadOnlyHint
		if a.DestructiveHint != nil {
			d := *a.DestructiveHint
			out.DestructiveHint = &d
		}
	}
	return out
}

// flatten joins text parts; every other part becomes one line saying so.
func flatten(r *sdk.CallToolResult) Result {
	parts := make([]string, 0, len(r.Content))
	for _, ct := range r.Content {
		switch v := ct.(type) {
		case *sdk.TextContent:
			parts = append(parts, v.Text)
		case *sdk.ImageContent:
			parts = append(parts, "[image omitted]")
		case *sdk.AudioContent:
			parts = append(parts, "[audio omitted]")
		case *sdk.ResourceLink:
			parts = append(parts, resourceLine(v.URI))
		case *sdk.EmbeddedResource:
			uri := ""
			if v.Resource != nil {
				uri = v.Resource.URI
			}
			parts = append(parts, resourceLine(uri))
		default:
			parts = append(parts, "[content omitted]")
		}
	}
	return Result{Text: strings.Join(parts, "\n"), IsError: r.IsError}
}

// resourceLine names an omitted resource by its uri when the uri parses
// with a scheme and fits maxURI once encoded, else without it.
func resourceLine(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return "[resource omitted]"
	}
	s := escapeUnsafe(u.String())
	if len(s) > maxURI {
		return "[resource omitted]"
	}
	return "[resource omitted: " + s + "]"
}

// escapeUnsafe percent-encodes what url.URL.String leaves raw in opaque
// parts and queries: controls, space, brackets and non-ASCII. The line
// stays one line and cannot close itself early.
func escapeUnsafe(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch <= ' ' || ch >= 0x7f || ch == '[' || ch == ']' {
			fmt.Fprintf(&b, "%%%02X", ch)
			continue
		}
		b.WriteByte(ch)
	}
	return b.String()
}
