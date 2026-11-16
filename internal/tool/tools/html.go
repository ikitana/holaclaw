package tools

import (
	"html"
	"strings"
	"unicode"
)

// dropped elements whose content is never readable text.
var droppedTags = map[string]bool{"script": true, "style": true, "head": true, "noscript": true, "svg": true}

// breakTags end a line, so the stripped text keeps some structure.
var breakTags = map[string]bool{
	"p": true, "br": true, "div": true, "li": true, "tr": true, "section": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
}

// htmlToText reduces an HTML document to readable text without a parser: it
// drops script/style/head, turns tags into spaces (block tags into newlines),
// decodes entities and collapses whitespace. It is deliberately approximate —
// the output is untrusted content shown to the model, not a DOM.
func htmlToText(s string) string {
	var b strings.Builder
	b.Grow(len(s) / 2)
	for i := 0; i < len(s); {
		if s[i] != '<' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if strings.HasPrefix(s[i:], "<!--") {
			if end := strings.Index(s[i+4:], "-->"); end >= 0 {
				i += 4 + end + 3
			} else {
				i = len(s)
			}
			continue
		}
		end := strings.IndexByte(s[i:], '>')
		if end < 0 {
			break // unterminated tag: drop the rest
		}
		tag := s[i+1 : i+end]
		i += end + 1
		name, closing := tagName(tag), strings.HasPrefix(strings.TrimSpace(tag), "/")
		if droppedTags[name] && !closing {
			if skip := skipElement(s[i:], name); skip >= 0 {
				i += skip
			} else {
				i = len(s)
			}
			continue
		}
		if breakTags[name] && !closing {
			b.WriteByte('\n')
			continue
		}
		b.WriteByte(' ')
	}
	return collapse(html.UnescapeString(b.String()))
}

// tagName extracts the lower-case element name of a tag body ("/p class=x").
func tagName(tag string) string {
	tag = strings.TrimPrefix(strings.TrimSpace(tag), "/")
	i := strings.IndexAny(tag, " \t\r\n/>")
	if i >= 0 {
		tag = tag[:i]
	}
	return strings.ToLower(tag)
}

// skipElement returns how many bytes to skip to get past the closing tag of
// name, or -1 when it never closes.
func skipElement(s, name string) int {
	closing := "</" + name
	for i := 0; i+1 < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		if len(s[i:]) >= len(closing) && strings.EqualFold(s[i:i+len(closing)], closing) {
			if end := strings.IndexByte(s[i:], '>'); end >= 0 {
				return i + end + 1
			}
			return -1
		}
	}
	return -1
}

// collapse squeezes runs of spaces and blank lines.
func collapse(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space, newlines := false, 0
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r':
			newlines++
			space = false
		case unicode.IsSpace(r) || r == ' ':
			space = true
		default:
			if newlines > 0 && b.Len() > 0 {
				if newlines > 2 {
					newlines = 2
				}
				b.WriteString(strings.Repeat("\n", newlines))
			} else if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			newlines, space = 0, false
			b.WriteRune(r)
		}
	}
	return b.String()
}
