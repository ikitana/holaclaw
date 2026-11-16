package googlechat

import (
	"regexp"
	"strings"
)

// --- outbound: markdown -> Chat text --------------------------------------

var (
	reCode     = regexp.MustCompile("(?s)```.*?```|`[^`\n]*`")
	reHeading  = regexp.MustCompile(`(?m)^[ \t]{0,3}#{1,6}[ \t]+(.*?)[ \t]*#*[ \t]*$`)
	reLink     = regexp.MustCompile(`\[([^\]\n]*)\]\(([^)\s]+)\)`)
	reBoldStar = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	reBoldUnd  = regexp.MustCompile(`__([^_\n]+)__`)
	reStrike   = regexp.MustCompile(`~~([^~\n]+)~~`)
	reItalic   = regexp.MustCompile(`\*([^*\n]+)\*`)
	reAngleTag = regexp.MustCompile(`<(users/|https?:|mailto:)`)
)

// boldMark stands in for a finished bold marker so the italic pass cannot
// reopen it.
const boldMark = "\x00"

// markdownToChat converts canonical markdown to Chat's text subset: *bold*,
// _italic_, ~strike~, `code`, fenced code and <url|text> links. Code spans and
// fenced blocks keep their content.
func markdownToChat(s string) string {
	var b strings.Builder
	last := 0
	for _, loc := range reCode.FindAllStringIndex(s, -1) {
		b.WriteString(convertProse(s[last:loc[0]]))
		b.WriteString(s[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(convertProse(s[last:]))
	return b.String()
}

func convertProse(s string) string {
	if s == "" {
		return ""
	}
	s = defuse(s)
	s = reHeading.ReplaceAllString(s, boldMark+"$1"+boldMark)
	s = reLink.ReplaceAllString(s, "<$2|$1>")
	s = reBoldStar.ReplaceAllString(s, boldMark+"$1"+boldMark)
	s = reBoldUnd.ReplaceAllString(s, boldMark+"$1"+boldMark)
	s = reStrike.ReplaceAllString(s, "~$1~")
	s = reItalic.ReplaceAllString(s, "_${1}_")
	return strings.ReplaceAll(s, boldMark, "*")
}

// defuse breaks Chat's angle-bracket syntax in text that holaclaw did not
// build: "<users/all>" would notify the whole space and "<url|label>" would
// disguise a link. A zero-width space after "<" leaves the text readable.
func defuse(s string) string {
	return reAngleTag.ReplaceAllString(s, "<"+zwsp+"$1")
}

// zwsp is U+200B ZERO WIDTH SPACE.
var zwsp = string(rune(0x200B))

// --- chunking ------------------------------------------------------------

// maxReopener caps the fence line that reopens a code block in the next
// chunk: "```" and the first word of the info string, never the whole line.
const maxReopener = 16

// fenceReserve leaves room for a reopened opener line and a closing fence.
const fenceReserve = maxReopener + len("\n") + len("\n```")

// chunkText splits text into chunks of at most max bytes (so at most max
// characters), preferring paragraph then line boundaries, and never leaves a
// code fence unclosed.
func chunkText(s string, max int) []string {
	s = strings.TrimRight(s, "\n")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	budget := max - fenceReserve
	if budget < 1 {
		budget = max
	}
	var chunks []string
	var cur string
	add := func(part, sep string) {
		if cur == "" {
			cur = part
			return
		}
		cur += sep + part
	}
	flush := func() {
		if cur != "" {
			chunks = append(chunks, cur)
			cur = ""
		}
	}
	for _, para := range splitParagraphs(s) {
		if cur != "" && len(cur)+2+len(para) > budget {
			flush()
		}
		if len(para) <= budget {
			add(para, "\n\n")
			continue
		}
		flush()
		for _, line := range splitLines(para, budget) {
			if cur != "" && len(cur)+1+len(line) > budget {
				flush()
			}
			add(line, "\n")
		}
	}
	flush()
	return balanceFences(chunks)
}

// splitParagraphs cuts on blank lines outside fenced code blocks.
func splitParagraphs(s string) []string {
	var out []string
	var cur []string
	fenced := false
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
		}
		if !fenced && strings.TrimSpace(line) == "" {
			if len(cur) > 0 {
				out = append(out, strings.Join(cur, "\n"))
				cur = nil
			}
			continue
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 {
		out = append(out, strings.Join(cur, "\n"))
	}
	return out
}

// splitLines returns the paragraph's lines, hard-cutting any line over max.
func splitLines(para string, max int) []string {
	var out []string
	for _, line := range strings.Split(para, "\n") {
		for len(line) > max {
			cut := max
			for cut > 0 && line[cut] != ' ' && line[cut] != '\t' {
				cut--
			}
			if cut < max/2 {
				cut = max
			}
			for cut > 1 && cut < len(line) && line[cut]&0xC0 == 0x80 {
				cut--
			}
			out = append(out, strings.TrimRight(line[:cut], " "))
			line = strings.TrimLeft(line[cut:], " ")
		}
		out = append(out, line)
	}
	return out
}

// balanceFences closes a fence left open at the end of a chunk and reopens it
// at the start of the next one, with the language word of its info string.
func balanceFences(chunks []string) []string {
	open := ""
	for i, c := range chunks {
		if open != "" {
			c = open + "\n" + c
		}
		state := ""
		for _, line := range strings.Split(c, "\n") {
			if l := strings.TrimSpace(line); strings.HasPrefix(l, "```") {
				if state == "" {
					state = reopener(l)
				} else {
					state = ""
				}
			}
		}
		if state != "" {
			c += "\n```"
		}
		chunks[i] = c
		open = state
	}
	return chunks
}

// reopener reduces a fence opener line to "```" and the first word of its
// info string, at most maxReopener bytes, so a reopened chunk stays within
// fenceReserve.
func reopener(line string) string {
	info := strings.Fields(strings.TrimLeft(line, "`"))
	if len(info) == 0 {
		return "```"
	}
	return "```" + truncateBytes(info[0], maxReopener-len("```"))
}

// truncateBytes cuts s to at most n bytes on a UTF-8 boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
