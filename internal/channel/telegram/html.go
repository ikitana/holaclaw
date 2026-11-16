package telegram

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxInlineDepth bounds emphasis nesting. Past it everything is escaped
// literally, so adversarial input can never make the converter emit an
// unbalanced tag.
const maxInlineDepth = 8

// mdToHTML converts holaclaw's canonical markdown to the subset of HTML
// Telegram's HTML parse mode accepts (docs/channels). Anything it cannot
// convert stays literal text; the output is always balanced.
func mdToHTML(md string) string {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var out []string
	for i := 0; i < len(lines); {
		line := lines[i]
		if fence, lang, ok := openFence(line); ok {
			if end := closeFence(lines, i+1, fence); end >= 0 {
				out = append(out, codeBlock(lines[i+1:end], lang))
				i = end + 1
				continue
			}
		}
		if strings.HasPrefix(strings.TrimLeft(line, " "), ">") {
			j := i
			var quoted []string
			for j < len(lines) && strings.HasPrefix(strings.TrimLeft(lines[j], " "), ">") {
				q := strings.TrimLeft(lines[j], " ")
				quoted = append(quoted, inlineHTML(strings.TrimPrefix(strings.TrimPrefix(q, ">"), " "), 0))
				j++
			}
			out = append(out, "<blockquote>"+strings.Join(quoted, "\n")+"</blockquote>")
			i = j
			continue
		}
		out = append(out, convertLine(line))
		i++
	}
	return strings.Join(out, "\n")
}

// convertLine handles the block prefixes that survive as plain text: headings
// become a bold line, list items keep their text with a bullet.
func convertLine(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	indent := line[:len(line)-len(trimmed)]
	if n := headingLevel(trimmed); n > 0 {
		return indent + "<b>" + inlineHTML(strings.TrimSpace(trimmed[n:]), 0) + "</b>"
	}
	if rest, ok := listItem(trimmed); ok {
		return indent + "• " + inlineHTML(rest, 0)
	}
	return inlineHTML(line, 0)
}

// headingLevel returns the number of leading '#' of an ATX heading, or 0.
func headingLevel(s string) int {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(s) || s[n] != ' ' {
		return 0
	}
	return n
}

// listItem strips a bullet or ordered-list marker.
func listItem(s string) (string, bool) {
	if len(s) > 1 && (s[0] == '-' || s[0] == '*' || s[0] == '+') && s[1] == ' ' {
		return strings.TrimSpace(s[2:]), true
	}
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	if n > 0 && n+1 < len(s) && (s[n] == '.' || s[n] == ')') && s[n+1] == ' ' {
		return strings.TrimSpace(s[n+2:]), true
	}
	return "", false
}

// openFence reports whether line opens a fenced code block.
func openFence(line string) (fence, lang string, ok bool) {
	t := strings.TrimLeft(line, " ")
	for _, f := range []string{"```", "~~~"} {
		if strings.HasPrefix(t, f) {
			return f, sanitiseLang(strings.TrimSpace(strings.TrimLeft(t[len(f):], string(f[0])))), true
		}
	}
	return "", "", false
}

// closeFence finds the line index that closes an open fence, or -1.
func closeFence(lines []string, from int, fence string) int {
	for j := from; j < len(lines); j++ {
		if strings.HasPrefix(strings.TrimLeft(lines[j], " "), fence) {
			return j
		}
	}
	return -1
}

func codeBlock(body []string, lang string) string {
	var b strings.Builder
	b.WriteString("<pre><code")
	if lang != "" {
		b.WriteString(` class="language-` + lang + `"`)
	}
	b.WriteString(">")
	b.WriteString(escapeHTML(strings.Join(body, "\n")))
	b.WriteString("</code></pre>")
	return b.String()
}

// sanitiseLang keeps only characters that are safe inside a class attribute.
func sanitiseLang(s string) string {
	out := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '+' || r == '-' || r == '.' || r == '_' || r == '#' {
			return r
		}
		return -1
	}, s)
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

// inlineHTML converts the inline markdown of one line.
func inlineHTML(s string, depth int) string {
	if depth > maxInlineDepth {
		return escapeHTML(s)
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		switch c := s[i]; {
		case c == '`':
			n := runLen(s[i:], '`')
			if j := strings.Index(s[i+n:], strings.Repeat("`", n)); j > 0 {
				b.WriteString("<code>" + escapeHTML(s[i+n:i+n+j]) + "</code>")
				i += n + j + n
				continue
			}
		case c == '[':
			if txt, href, n, ok := parseLink(s[i:]); ok {
				if isHTTPURL(href) {
					b.WriteString(`<a href="` + escapeAttr(href) + `">` + inlineHTML(txt, depth+1) + "</a>")
				} else {
					b.WriteString(escapeHTML(s[i : i+n]))
				}
				i += n
				continue
			}
		case strings.HasPrefix(s[i:], "**"), strings.HasPrefix(s[i:], "__"):
			if inner, n, ok := delim(s[i:], s[i:i+2]); ok {
				b.WriteString("<b>" + inlineHTML(inner, depth+1) + "</b>")
				i += n
				continue
			}
		case strings.HasPrefix(s[i:], "~~"):
			if inner, n, ok := delim(s[i:], "~~"); ok {
				b.WriteString("<s>" + inlineHTML(inner, depth+1) + "</s>")
				i += n
				continue
			}
		case c == '*' || c == '_':
			if c == '_' && i > 0 && isWordByte(s[i-1]) {
				break // snake_case is not emphasis
			}
			if inner, n, ok := delim(s[i:], s[i:i+1]); ok {
				if c != '_' || i+n >= len(s) || !isWordByte(s[i+n]) {
					b.WriteString("<i>" + inlineHTML(inner, depth+1) + "</i>")
					i += n
					continue
				}
			}
		}
		r, sz := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
		i += sz
	}
	return b.String()
}

// runLen counts the leading run of c in s.
func runLen(s string, c byte) int {
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	return n
}

// delim finds the closing delimiter d in s, which starts with d. It returns the
// inner text and the total bytes consumed, or ok=false when nothing closes it —
// unbalanced markup stays literal.
func delim(s, d string) (string, int, bool) {
	n := len(d)
	rest := s[n:]
	if rest == "" || rest[0] == ' ' || rest[0] == '\t' {
		return "", 0, false
	}
	for j := 1; j+n <= len(rest); j++ {
		if !strings.HasPrefix(rest[j:], d) {
			continue
		}
		if n == 1 && j+1 < len(rest) && rest[j+1] == d[0] {
			continue // part of a longer run: not this delimiter
		}
		if rest[j-1] == ' ' || rest[j-1] == '\t' {
			continue
		}
		return rest[:j], n + j + n, true
	}
	return "", 0, false
}

// parseLink parses "[text](url)" at the start of s.
func parseLink(s string) (text, href string, n int, ok bool) {
	end := strings.IndexByte(s, ']')
	if end < 0 || end+1 >= len(s) || s[end+1] != '(' {
		return "", "", 0, false
	}
	close := strings.IndexByte(s[end+2:], ')')
	if close < 0 {
		return "", "", 0, false
	}
	return s[1:end], strings.TrimSpace(s[end+2 : end+2+close]), end + 2 + close + 1, true
}

// isHTTPURL keeps link targets to schemes Telegram will open.
func isHTTPURL(u string) bool {
	l := strings.ToLower(u)
	if !strings.HasPrefix(l, "http://") && !strings.HasPrefix(l, "https://") {
		return false
	}
	return !strings.ContainsFunc(u, func(r rune) bool {
		return r < 0x20 || r == 0x7f || unicode.IsSpace(r)
	})
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}

func escapeHTML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func escapeAttr(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// htmlToPlain undoes the conversion for the plain-text resend that follows a
// "can't parse entities" rejection.
func htmlToPlain(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch s[i] {
		case '<':
			if j := strings.IndexByte(s[i:], '>'); j >= 0 {
				i += j + 1
				continue
			}
		case '&':
			if name, n, ok := entityAt(s[i:]); ok {
				b.WriteString(name)
				i += n
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// entityAt decodes the HTML entity at the start of s.
func entityAt(s string) (string, int, bool) {
	for ent, ch := range map[string]string{"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`} {
		if strings.HasPrefix(s, ent) {
			return ch, len(ent), true
		}
	}
	return "", 0, false
}
