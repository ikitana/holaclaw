package guard

import (
	"strings"
	"unicode"

	"holaclaw/internal/tool"
)

// Parse turns a guard answer into a decision and a fixed reason label.
//
// The rule is deliberately narrow: the answer is scanned for the first run of
// letters — punctuation, quotes and markdown around it are skipped — and only
// that first word decides. "allow" and "safe" mean Allow; every recognised
// refusal ("ask", "deny", "block", "unsafe", "no") means Ask; anything else,
// including an empty answer and a sentence that merely happens to contain
// "safe" further along ("I think this is safe"), is unparseable and also
// means Ask. A verbose model is therefore never read as permission.
//
// Parse never returns Deny. The guard may only move a call between Ask and
// Allow (docs/security.md), and the gate turns a Deny answer into an Ask.
func Parse(answer string) (tool.Decision, string) {
	switch firstWord(answer) {
	case "allow", "safe":
		return tool.Allow, ReasonAllow
	case "ask", "deny", "denied", "block", "blocked", "unsafe", "no", "reject":
		return tool.Ask, ReasonAsk
	default:
		return tool.Ask, ReasonUnparseable
	}
}

// firstWord returns the first run of letters in s, lowercased.
func firstWord(s string) string {
	start := -1
	for i, r := range s {
		if unicode.IsLetter(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			return strings.ToLower(s[start:i])
		}
	}
	if start < 0 {
		return ""
	}
	return strings.ToLower(s[start:])
}
