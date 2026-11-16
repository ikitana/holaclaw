package agent

import (
	"encoding/json"
	"regexp"
	"strings"
)

// leakedCall is a tool call a model wrote into its visible text instead of
// using the tool interface. Such calls are never executed blindly
// (docs/models.md).
type leakedCall struct {
	Name string
	Args string // JSON object text, "{}" when the model gave none
}

// Shapes local models leak tool calls in.
var (
	fenceRe     = regexp.MustCompile("(?s)```[A-Za-z0-9_+-]*[ \t]*\r?\n?(.*?)```")
	toolCallRe  = regexp.MustCompile(`(?s)<tool_call>(.*?)</tool_call>`)
	functionRe  = regexp.MustCompile(`(?s)<function=([A-Za-z0-9_.\-]+)>(.*?)(?:</function>|$)`)
	toolMarkRe  = regexp.MustCompile(`(?s)\[TOOL_CALLS\][ \t]*(.*)$`)
	blankLineRe = regexp.MustCompile(`\n{3,}`)
)

// nameKeys and argKeys are the field names the leaked shapes use.
var (
	nameKeys = []string{"name", "tool", "function", "tool_name"}
	argKeys  = []string{"arguments", "args", "parameters", "input"}
)

// detectLeakedCalls looks for tool calls written as text. It only fires when
// a call names a tool this agent actually has, so ordinary JSON the user
// asked for is left alone. It returns the calls found, the text with the
// leaked blocks removed, and whether anything was found.
func detectLeakedCalls(text string, known func(string) bool) ([]leakedCall, string, bool) {
	if known == nil || strings.TrimSpace(text) == "" {
		return nil, text, false
	}
	var calls []leakedCall
	rest := text

	// Delimited shapes first, so their wrappers go away with the payload.
	rest = cutMatches(rest, fenceRe, 1, func(m []string) []leakedCall {
		return callsFromJSON(m[1], known)
	}, &calls)
	rest = cutMatches(rest, toolCallRe, 1, func(m []string) []leakedCall {
		return callsFromJSON(m[1], known)
	}, &calls)
	rest = cutMatches(rest, functionRe, 2, func(m []string) []leakedCall {
		if !known(m[1]) {
			return nil
		}
		return []leakedCall{{Name: m[1], Args: objectOr(m[2])}}
	}, &calls)
	rest = cutMatches(rest, toolMarkRe, 1, func(m []string) []leakedCall {
		return callsFromJSON(m[1], known)
	}, &calls)

	// Then bare JSON objects left in the prose.
	bare, cut := scanBareCalls(rest, known)
	calls = append(calls, bare...)
	rest = cut

	if len(calls) == 0 {
		return nil, text, false
	}
	return calls, tidy(rest), true
}

// cutMatches removes every match whose payload yields at least one known
// call, appending those calls.
func cutMatches(s string, re *regexp.Regexp, groups int, extract func([]string) []leakedCall, out *[]leakedCall) string {
	idx := re.FindAllStringSubmatchIndex(s, -1)
	if idx == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range idx {
		m := make([]string, groups+1)
		for g := 0; g <= groups; g++ {
			if loc[2*g] >= 0 {
				m[g] = s[loc[2*g]:loc[2*g+1]]
			}
		}
		found := extract(m)
		if len(found) == 0 {
			continue
		}
		*out = append(*out, found...)
		b.WriteString(s[last:loc[0]])
		last = loc[1]
	}
	b.WriteString(s[last:])
	return b.String()
}

// scanBareCalls finds top-level JSON values in prose that describe a call to
// a known tool and removes them.
func scanBareCalls(s string, known func(string) bool) ([]leakedCall, string) {
	var calls []leakedCall
	var b strings.Builder
	i, last := 0, 0
	for i < len(s) {
		c := s[i]
		if c != '{' && c != '[' {
			i++
			continue
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var v any
		if err := dec.Decode(&v); err == nil {
			if found := callsFromValue(v, known); len(found) > 0 {
				end := i + int(dec.InputOffset())
				calls = append(calls, found...)
				b.WriteString(s[last:i])
				last, i = end, end
				continue
			}
		}
		i++
	}
	b.WriteString(s[last:])
	return calls, b.String()
}

// callsFromJSON decodes the first JSON value in s and reads calls out of it.
func callsFromJSON(s string, known func(string) bool) []leakedCall {
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(s)))
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil
	}
	return callsFromValue(v, known)
}

// callsFromValue reads {"name": ..., "arguments": ...} objects, or an array
// of them, out of a decoded JSON value.
func callsFromValue(v any, known func(string) bool) []leakedCall {
	switch t := v.(type) {
	case []any:
		var out []leakedCall
		for _, e := range t {
			out = append(out, callsFromValue(e, known)...)
		}
		return out
	case map[string]any:
		// Some templates nest the call under "function".
		if inner, ok := t["function"].(map[string]any); ok {
			return callsFromValue(inner, known)
		}
		name := ""
		for _, k := range nameKeys {
			if s, ok := t[k].(string); ok && s != "" {
				name = s
				break
			}
		}
		if name == "" || !known(name) {
			return nil
		}
		args := "{}"
		for _, k := range argKeys {
			raw, ok := t[k]
			if !ok {
				continue
			}
			switch a := raw.(type) {
			case map[string]any:
				if b, err := json.Marshal(a); err == nil {
					args = string(b)
				}
			case string:
				args = objectOr(a)
			}
			break
		}
		return []leakedCall{{Name: name, Args: args}}
	default:
		return nil
	}
}

// objectOr returns s when it is a JSON object, else "{}".
func objectOr(s string) string {
	s = strings.TrimSpace(s)
	var obj map[string]any
	if s == "" || json.Unmarshal([]byte(s), &obj) != nil {
		return "{}"
	}
	return s
}

// tidy cleans up the holes left by stripping.
func tidy(s string) string {
	s = blankLineRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
