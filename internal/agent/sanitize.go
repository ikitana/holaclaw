package agent

import (
	"regexp"
	"strings"
	"unicode"
)

// maxLabelRunes caps a sanitised display label. A label is metadata, not
// content: 32 runes is plenty for "Angel M." and short enough that a long
// name cannot push the real turn out of the budget.
const maxLabelRunes = 32

// SanitizeSender returns a display name that is safe to place in a prompt as
// a "[name] " prefix. A display name is attacker-controlled on every channel,
// so it must not be able to forge a prompt section, a markdown heading, a
// code fence or an <untrusted> tag (docs/sessions.md, docs/security.md).
//
// It drops control and formatting runes (including bidi overrides), the
// bracket runes []<>, the markdown heading and backtick runes, collapses all
// whitespace to single spaces and caps the result at 32 runes. An empty
// result falls back to "user".
func SanitizeSender(s string) string { return sanitizeLabel(s, "user") }

// SanitizeDisplay is SanitizeSender without the fallback: a name that
// sanitizes to nothing stays empty. A binding stores its display label this
// way, so an absent name never reads as one called "user".
func SanitizeDisplay(s string) string { return sanitizeLabel(s, "") }

// sanitizeLabel is SanitizeSender with a caller-chosen fallback; attachment
// names go through it with "file".
func sanitizeLabel(s, fallback string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t' || unicode.IsSpace(r):
			space = true
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			// Control and format runes (RTL overrides, zero-width joiners).
			continue
		case strings.ContainsRune("[]<>#`", r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := []rune(strings.TrimSpace(b.String()))
	if len(out) > maxLabelRunes {
		out = out[:maxLabelRunes]
	}
	res := strings.TrimSpace(string(out))
	if res == "" {
		return fallback
	}
	return res
}

// untrustedTag matches a literal <untrusted> or </untrusted> tag in any case.
var untrustedTag = regexp.MustCompile(`(?i)</?untrusted\s*>`)

// neutralizeTaintTags escapes literal untrusted tags inside text so content
// cannot close the delimiter that marks it as untrusted (docs/security.md).
func neutralizeTaintTags(s string) string {
	return untrustedTag.ReplaceAllStringFunc(s, func(m string) string {
		return "&lt;" + strings.Trim(m, "<>") + "&gt;"
	})
}

// WrapUntrusted delimits text that came from an untrusted source, neutralising
// any literal tag inside it so the content cannot close its own delimiter. It
// is exported for callers that inject untrusted text themselves (the daemon
// injects tainted recall hits this way).
func WrapUntrusted(s string) string { return wrapUntrusted(s) }

// The delimiters of an untrusted span. Content inside one has its literal
// tags neutralised, so it cannot fake or close one.
const (
	untrustedOpen  = "<untrusted>"
	untrustedClose = "</untrusted>"
)

// wrapUntrusted delimits text that came from an untrusted source.
func wrapUntrusted(s string) string {
	return untrustedOpen + "\n" + neutralizeTaintTags(s) + "\n" + untrustedClose
}
