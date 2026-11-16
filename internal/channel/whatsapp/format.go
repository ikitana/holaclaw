package whatsapp

import (
	"regexp"
	"strings"

	"holaclaw/internal/channel/textutil"
)

// WhatsApp renders a small markup of its own: *bold*, _italic_, ~strike~,
// `code`, ``` blocks, "> " quotes and lists. Malformed markup shows
// literally, so conversion never loses a reply (§5.1 Text out).
var (
	reLink   = regexp.MustCompile(`!?\[([^\]\n]*)\]\(([^)\s]+)\)`)
	reBold   = regexp.MustCompile(`\*\*([^\n]+?)\*\*|__([^_\n]+?)__`)
	reItalic = regexp.MustCompile(`(^|[^*\w])\*([^*\s][^*\n]*?)\*`)
	reStrike = regexp.MustCompile(`~~([^~\n]+?)~~`)
)

// boldMark stands in for "*" while italics are converted, so a bold span is
// not read as an italic one.
const boldMark = "\x00"

// mdToWA converts holaclaw's canonical markdown to WhatsApp's markup.
// Headings become bold lines and links become "text (url)", since WhatsApp
// links bare URLs itself.
func mdToWA(md string) string {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			out = append(out, "```")
			continue
		}
		if inFence {
			out = append(out, line)
			continue
		}
		indent := line[:len(line)-len(trimmed)]
		switch {
		case headingLevel(trimmed) > 0:
			n := headingLevel(trimmed)
			text := strings.ReplaceAll(inline(strings.TrimSpace(trimmed[n:])), "*", "")
			out = append(out, indent+"*"+text+"*")
		case strings.HasPrefix(trimmed, ">"):
			out = append(out, "> "+inline(strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))))
		case len(trimmed) > 1 && (trimmed[0] == '-' || trimmed[0] == '*' || trimmed[0] == '+') && trimmed[1] == ' ':
			out = append(out, indent+"- "+inline(strings.TrimSpace(trimmed[2:])))
		default:
			out = append(out, inline(line))
		}
	}
	return strings.Join(out, "\n")
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

// inline converts the spans of one line, leaving code spans untouched.
func inline(s string) string {
	parts := strings.Split(s, "`")
	for i := 0; i < len(parts); i += 2 {
		if i == len(parts)-1 && len(parts)%2 == 0 {
			break // an unclosed backtick: the rest is literal
		}
		p := reLink.ReplaceAllStringFunc(parts[i], func(m string) string {
			sm := reLink.FindStringSubmatch(m)
			text, url := strings.TrimSpace(sm[1]), sm[2]
			if text == "" || text == url {
				return url
			}
			return text + " (" + url + ")"
		})
		p = reBold.ReplaceAllString(p, boldMark+"$1$2"+boldMark)
		p = reItalic.ReplaceAllString(p, "${1}_${2}_")
		p = reStrike.ReplaceAllString(p, "~$1~")
		parts[i] = strings.ReplaceAll(p, boldMark, "*")
	}
	return strings.Join(parts, "`")
}

// fence is the code block marker; a block cut by a split is closed at the end
// of one part and reopened at the start of the next.
const fence = "```"

// splitText cuts s into parts of at most limit UTF-16 code units, preferring a
// paragraph, then a line, a sentence and a word break in the second half of
// a part, and keeping code blocks balanced in every part.
func splitText(s string, limit int) []string {
	var out []string
	s = strings.TrimSpace(s)
	for s != "" {
		if textutil.UTF16Len(s) <= limit {
			out = append(out, s)
			break
		}
		room := limit - len("\n"+fence) // space to close a cut block
		end := textutil.UTF16Prefix(s, room)
		head := s[:end]
		cut := end
		for _, sep := range []string{"\n\n", "\n", ". ", " "} {
			if i := strings.LastIndex(head, sep); i >= 0 && textutil.UTF16Len(head[:i])*2 >= room {
				cut = i + len(sep)
				break
			}
		}
		if cut == 0 {
			cut = len(string([]rune(s)[0]))
		}
		part, rest := strings.TrimRight(s[:cut], " \n"), strings.TrimLeft(s[cut:], " \n")
		if strings.Count(part, fence)%2 == 1 {
			part += "\n" + fence
			rest = fence + "\n" + rest
		}
		if part != "" {
			out = append(out, part)
		}
		s = rest
	}
	return out
}
