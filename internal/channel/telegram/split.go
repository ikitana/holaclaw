package telegram

import (
	"strings"
	"unicode/utf8"
)

// visibleLen counts an HTML string the way Telegram counts the message it
// renders: tags are free, an entity is one character.
func visibleLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		switch s[i] {
		case '<':
			if j := strings.IndexByte(s[i:], '>'); j >= 0 {
				i += j + 1
				continue
			}
		case '&':
			if _, sz, ok := entityAt(s[i:]); ok {
				n++
				i += sz
				continue
			}
		}
		r, sz := utf8.DecodeRuneInString(s[i:])
		n++
		if r > 0xFFFF {
			n++
		}
		i += sz
	}
	return n
}

// openTag is an element left open at a split point.
type openTag struct {
	name string // lower-case element name
	raw  string // the opening tag verbatim, so attributes survive a reopen
}

// breakpoint is a position where a chunk may end.
type breakpoint struct {
	pos   int // byte offset in the chunk buffer
	seen  int // visible length up to pos
	pri   int // 3 paragraph, 2 line, 1 sentence, 0 word
	stack []openTag
}

// splitHTML cuts HTML into chunks of at most limit visible UTF-16 code units,
// never inside a tag or an entity, reopening every element that spans a cut
// (docs/channels).
func splitHTML(s string, limit int) []string {
	if limit <= 0 || visibleLen(s) <= limit {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []string{s}
	}
	var (
		out   []string
		buf   strings.Builder
		stack []openTag
		bps   []breakpoint
		seen  int
	)
	cut := func(at breakpoint) {
		text := buf.String()
		head := text[:at.pos] + closeTags(at.stack)
		if strings.TrimSpace(htmlToPlain(head)) != "" {
			out = append(out, head)
		}
		tail := strings.TrimLeft(text[at.pos:], " \t\n")
		seen -= at.seen + (len(text) - at.pos - len(tail))
		buf.Reset()
		buf.WriteString(openTags(at.stack))
		buf.WriteString(tail)
		bps = bps[:0]
	}
	for i := 0; i < len(s); {
		tok, width, kind := nextToken(s[i:])
		if seen+width > limit && seen > 0 {
			cut(pickBreak(bps, buf.Len(), seen, stack, limit))
		}
		switch kind {
		case tokOpen:
			stack = append(stack, parseOpen(tok))
		case tokClose:
			stack = popTag(stack, tagName(tok))
		case tokText:
			if p := breakPri(s, i, tok); p >= 0 {
				bps = append(bps, breakpoint{
					pos: buf.Len() + len(tok), seen: seen + width, pri: p,
					stack: append([]openTag(nil), stack...),
				})
			}
		}
		buf.WriteString(tok)
		seen += width
		i += len(tok)
	}
	if rest := buf.String(); strings.TrimSpace(htmlToPlain(rest)) != "" {
		out = append(out, rest+closeTags(stack))
	}
	return out
}

// pickBreak chooses the latest breakpoint of the best available class, keeping
// the ladder paragraph → line → sentence → word, and falls back to a hard cut
// at the current position.
func pickBreak(bps []breakpoint, pos, seen int, stack []openTag, limit int) breakpoint {
	for _, pri := range []int{3, 2, 1, 0} {
		for k := len(bps) - 1; k >= 0; k-- {
			// A break in the first half would waste most of the chunk.
			if bps[k].pri >= pri && bps[k].seen*2 >= limit {
				return bps[k]
			}
		}
	}
	return breakpoint{pos: pos, seen: seen, stack: append([]openTag(nil), stack...)}
}

// breakPri classifies a text token as a break opportunity, or -1.
func breakPri(s string, i int, tok string) int {
	if tok != " " && tok != "\n" && tok != "\t" {
		return -1
	}
	if tok == "\n" {
		if strings.HasPrefix(s[i+1:], "\n") || (i > 0 && s[i-1] == '\n') {
			return 3
		}
		return 2
	}
	if i > 0 && (s[i-1] == '.' || s[i-1] == '!' || s[i-1] == '?' || s[i-1] == ';') {
		return 1
	}
	return 0
}

// Token kinds of the HTML scanner.
const (
	tokText = iota
	tokOpen
	tokClose
)

// nextToken returns the next atomic unit of s: a whole tag, a whole entity, or
// one rune. width is its visible length in UTF-16 code units.
func nextToken(s string) (tok string, width, kind int) {
	switch s[0] {
	case '<':
		if j := strings.IndexByte(s, '>'); j >= 0 {
			k := tokOpen
			if strings.HasPrefix(s, "</") {
				k = tokClose
			}
			return s[:j+1], 0, k
		}
	case '&':
		if _, n, ok := entityAt(s); ok {
			return s[:n], 1, tokText
		}
	}
	r, sz := utf8.DecodeRuneInString(s)
	w := 1
	if r > 0xFFFF {
		w = 2
	}
	return s[:sz], w, tokText
}

// tagName extracts the lower-case element name of a tag token.
func tagName(tok string) string {
	body := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(tok, "<"), "/"), ">")
	if i := strings.IndexAny(body, " \t\n/"); i >= 0 {
		body = body[:i]
	}
	return strings.ToLower(body)
}

func parseOpen(tok string) openTag { return openTag{name: tagName(tok), raw: tok} }

// popTag removes the innermost open element with the given name.
func popTag(stack []openTag, name string) []openTag {
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].name == name {
			return append(stack[:i:i], stack[i+1:]...)
		}
	}
	return stack
}

func closeTags(stack []openTag) string {
	var b strings.Builder
	for i := len(stack) - 1; i >= 0; i-- {
		b.WriteString("</" + stack[i].name + ">")
	}
	return b.String()
}

func openTags(stack []openTag) string {
	var b strings.Builder
	for _, t := range stack {
		b.WriteString(t.raw)
	}
	return b.String()
}
