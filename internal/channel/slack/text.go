package slack

import (
	"regexp"
	"strings"
)

// --- inbound: Slack text -> canonical markdown ---------------------------

// slackToMarkdown rewrites Slack's angle-bracket tokens into markdown and
// unescapes the three entities Slack escapes. resolve maps a user id to a
// display name; it may return the id itself.
func slackToMarkdown(s string, resolve func(userID string) string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			b.WriteString(unescapeEntities(s))
			return b.String()
		}
		j := strings.IndexByte(s[i:], '>')
		if j < 0 {
			b.WriteString(unescapeEntities(s))
			return b.String()
		}
		b.WriteString(unescapeEntities(s[:i]))
		b.WriteString(convertToken(s[i+1:i+j], resolve))
		s = s[i+j+1:]
	}
}

// convertToken renders one Slack token (the text between < and >).
func convertToken(tok string, resolve func(string) string) string {
	body, label, hasLabel := strings.Cut(tok, "|")
	label = unescapeEntities(label)
	switch {
	case strings.HasPrefix(body, "@"):
		id := body[1:]
		if hasLabel && label != "" {
			return "@" + label
		}
		if resolve != nil {
			return "@" + resolve(id)
		}
		return "@" + id
	case strings.HasPrefix(body, "#"):
		if hasLabel && label != "" {
			return "#" + label
		}
		return "#" + body[1:]
	case strings.HasPrefix(body, "!"):
		if hasLabel && label != "" {
			return "@" + strings.TrimPrefix(label, "@")
		}
		cmd, _, _ := strings.Cut(body[1:], "^")
		return "@" + cmd
	case hasLabel && label != "":
		return "[" + label + "](" + unescapeEntities(body) + ")"
	default:
		return unescapeEntities(body)
	}
}

func unescapeEntities(s string) string {
	if !strings.ContainsRune(s, '&') {
		return s
	}
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return strings.ReplaceAll(s, "&amp;", "&")
}

func escapeEntities(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, ">", "&gt;")
}

// stripMention removes our own mention token and tidies the spacing left
// behind. Slack always writes it as <@BOTID> (optionally with a label).
func stripMention(s, botUserID string) string {
	if botUserID == "" {
		return s
	}
	out := s
	for _, tok := range []string{"<@" + botUserID + ">", "<@" + botUserID + "|"} {
		for {
			i := strings.Index(out, tok)
			if i < 0 {
				break
			}
			end := i + len(tok)
			if strings.HasSuffix(tok, "|") {
				j := strings.IndexByte(out[i:], '>')
				if j < 0 {
					break
				}
				end = i + j + 1
			}
			out = out[:i] + out[end:]
		}
	}
	return strings.TrimSpace(collapseSpaces(out))
}

var manySpaces = regexp.MustCompile(`[ \t]{2,}`)

func collapseSpaces(s string) string { return manySpaces.ReplaceAllString(s, " ") }

// mentions reports whether the text addresses the given bot user id.
func mentions(text, botUserID string) bool {
	if botUserID == "" {
		return false
	}
	return strings.Contains(text, "<@"+botUserID+">") || strings.Contains(text, "<@"+botUserID+"|")
}

// --- outbound: markdown -> Slack mrkdwn ----------------------------------

var (
	reCode     = regexp.MustCompile("(?s)```.*?```|`[^`\n]*`")
	reHeading  = regexp.MustCompile(`(?m)^[ \t]{0,3}#{1,6}[ \t]+(.*?)[ \t]*#*[ \t]*$`)
	reLink     = regexp.MustCompile(`\[([^\]\n]*)\]\(([^)\s]+)\)`)
	reBoldStar = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	reBoldUnd  = regexp.MustCompile(`__([^_\n]+)__`)
	reStrike   = regexp.MustCompile(`~~([^~\n]+)~~`)
	reItalic   = regexp.MustCompile(`\*([^*\n]+)\*`)
)

// boldMark stands in for a finished bold marker so the italic pass cannot
// reopen it.
const boldMark = "\x00"

// markdownToMrkdwn converts canonical markdown to Slack mrkdwn. Code spans
// and fenced blocks keep their content; everything else is escaped.
func markdownToMrkdwn(s string) string {
	var b strings.Builder
	last := 0
	for _, loc := range reCode.FindAllStringIndex(s, -1) {
		b.WriteString(convertProse(s[last:loc[0]]))
		b.WriteString(escapeEntities(s[loc[0]:loc[1]]))
		last = loc[1]
	}
	b.WriteString(convertProse(s[last:]))
	return b.String()
}

func convertProse(s string) string {
	if s == "" {
		return ""
	}
	s = escapeEntities(s)
	s = reHeading.ReplaceAllString(s, boldMark+"$1"+boldMark)
	s = reLink.ReplaceAllString(s, "<$2|$1>")
	s = reBoldStar.ReplaceAllString(s, boldMark+"$1"+boldMark)
	s = reBoldUnd.ReplaceAllString(s, boldMark+"$1"+boldMark)
	s = reStrike.ReplaceAllString(s, "~$1~")
	s = reItalic.ReplaceAllString(s, "_${1}_")
	return strings.ReplaceAll(s, boldMark, "*")
}

// --- chunking ------------------------------------------------------------

// fenceReserve leaves room for a closing fence and a reopened opener line.
const fenceReserve = 16

// chunkText splits text into chunks of at most max characters, preferring
// paragraph then line boundaries, and never leaves a code fence unclosed.
func chunkText(s string, max int) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	budget := max - fenceReserve
	if budget < 1 {
		budget = max
	}
	var chunks []string
	var cur string
	add := func(part string) {
		if cur == "" {
			cur = part
			return
		}
		cur += "\n\n" + part
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
			add(para)
			continue
		}
		flush()
		for _, line := range splitLines(para, budget) {
			if cur != "" && len(cur)+1+len(line) > budget {
				flush()
			}
			if cur == "" {
				cur = line
			} else {
				cur += "\n" + line
			}
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
			for cut > 0 && !isBoundary(line[cut]) {
				cut--
			}
			if cut < max/2 {
				cut = max
			}
			for cut > 1 && cut < len(line) && !isRuneStart(line[cut]) {
				cut--
			}
			out = append(out, strings.TrimRight(line[:cut], " "))
			line = strings.TrimLeft(line[cut:], " ")
		}
		out = append(out, line)
	}
	return out
}

func isBoundary(b byte) bool { return b == ' ' || b == '\t' }

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// balanceFences closes a fence left open at the end of a chunk and reopens it
// with the same info string at the start of the next one.
func balanceFences(chunks []string) []string {
	open := ""
	for i, c := range chunks {
		if open != "" {
			c = open + "\n" + c
		}
		state := ""
		for _, line := range strings.Split(c, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				if state == "" {
					state = strings.TrimSpace(line)
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
