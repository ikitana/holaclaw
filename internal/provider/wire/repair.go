package wire

import (
	"encoding/json"
	"log/slog"
	"strings"

	"holaclaw/internal/provider"
)

// Repair stages recorded in Response.Repairs (a strict parse records nothing).
const (
	StageLenient       = "lenient"
	StageTrailingComma = "trailing-comma"
	StageAutoClose     = "auto-close"
	StageEmpty         = "empty"
)

// RepairArguments turns model-emitted tool arguments into a JSON object,
// layering ever more forgiving repairs (docs/models.md). It returns the
// object text and the stage that succeeded, or "" when the strict parse was
// enough.
func RepairArguments(args string) (string, string) {
	// A model that takes no arguments often sends nothing at all; that is not
	// damage, so it is not reported as a repair.
	if strings.TrimSpace(args) == "" {
		return "{}", ""
	}
	if IsJSONObject(args) {
		return args, ""
	}
	s := unwrapJunk(args)
	if IsJSONObject(s) {
		return s, StageLenient
	}
	s = dropTrailingCommas(s)
	if IsJSONObject(s) {
		return s, StageTrailingComma
	}
	s = autoClose(s)
	if IsJSONObject(s) {
		return s, StageAutoClose
	}
	return "{}", StageEmpty
}

// IsJSONObject reports whether s parses as a JSON object.
func IsJSONObject(s string) bool {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "{") {
		return false
	}
	var v map[string]any
	return json.Unmarshal([]byte(t), &v) == nil && v != nil
}

// unwrapJunk strips markdown code fences and any text around the outermost
// braces.
func unwrapJunk(s string) string {
	t := strings.TrimSpace(s)
	if i := strings.Index(t, "```"); i >= 0 {
		t = t[i+3:]
		if nl := strings.IndexByte(t, '\n'); nl >= 0 && !strings.Contains(t[:nl], "{") {
			t = t[nl+1:] // drop the language tag line
		}
		if j := strings.Index(t, "```"); j >= 0 {
			t = t[:j]
		}
	}
	start := strings.IndexByte(t, '{')
	if start < 0 {
		return strings.TrimSpace(t)
	}
	end := strings.LastIndexByte(t, '}')
	if end > start {
		return strings.TrimSpace(t[start : end+1])
	}
	return strings.TrimSpace(t[start:])
}

// dropTrailingCommas removes commas that sit just before a closing bracket.
func dropTrailingCommas(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			b.WriteByte(c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(s) && (s[j] == ' ' || s[j] == '\t' || s[j] == '\n' || s[j] == '\r') {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue // swallow the comma, keep the whitespace
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// autoClose terminates an unbalanced string and closes open brackets/braces in
// the order they were opened.
func autoClose(s string) string {
	var stack []byte
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			stack = append(stack, c)
		case '}', ']':
			if n := len(stack); n > 0 {
				stack = stack[:n-1]
			}
		}
	}
	var b strings.Builder
	b.WriteString(s)
	if esc {
		b.WriteByte('\\') // an escape with nothing after it
	}
	if inStr {
		b.WriteByte('"')
	}
	// A dangling key or comma cannot be closed, so trim it first.
	out := strings.TrimRight(b.String(), " \t\r\n")
	out = strings.TrimRight(out, ",")
	if strings.HasSuffix(out, ":") {
		out += "null"
	}
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == '{' {
			out += "}"
		} else {
			out += "]"
		}
	}
	return out
}

// RepairCall repairs one finished tool call in place and records the stage.
func RepairCall(logger *slog.Logger, name string, tc *provider.ToolCall, repairs map[string]string) {
	fixed, stage := RepairArguments(tc.Arguments)
	tc.Arguments = fixed
	if stage == "" {
		return
	}
	repairs[tc.ID] = stage
	if stage == StageEmpty {
		logger.Warn("tool-call arguments unrepairable, falling back to {}",
			"provider", name, "tool", tc.Name, "call_id", tc.ID)
	}
}
