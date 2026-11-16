package responses

import (
	"sort"
	"strconv"
	"strings"

	"holaclaw/internal/provider"
)

// toolNames assigns API-safe aliases without changing the tool registry. Names
// from history participate too, so replayed calls use the same request mapping.
func toolNames(req provider.Request) map[string]string {
	names := map[string]string{}
	for _, tool := range req.Tools {
		names[tool.Name] = tool.Name
	}
	for _, message := range req.Messages {
		for _, call := range message.ToolCalls {
			names[call.Name] = call.Name
		}
	}
	used := map[string]bool{}
	var invalid []string
	for name := range names {
		if validToolName(name) {
			used[name] = true
		} else {
			invalid = append(invalid, name)
		}
	}
	sort.Strings(invalid)
	for _, name := range invalid {
		base := strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
				return r
			}
			return '_'
		}, name)
		if base == "" {
			base = "tool"
		}
		if len(base) > 48 {
			base = base[:48]
		}
		alias := base
		for n := 1; used[alias]; n++ {
			alias = base + "_" + strconv.Itoa(n)
		}
		names[name] = alias
		used[alias] = true
	}
	return names
}

func validToolName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}
