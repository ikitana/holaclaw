package wire

import "strings"

// DefaultThinkingTags is the one list of reasoning-tag names (docs/models.md). Per-model extras from Model.ThinkingTags are appended to it.
var DefaultThinkingTags = []string{"think", "thinking", "reasoning", "thought", "reflection"}

// Scrubber removes <think>…</think>-style reasoning blocks from a token
// stream. It is stateful: tags may be split across any number of chunk
// boundaries, and it never emits text that might still turn out to be part of
// a tag.
type Scrubber struct {
	tags   []string // lower-case tag names
	pend   string   // withheld tail: a possible (partial) tag
	inside bool     // inside a reasoning block
	open   string   // name of the block we are inside
}

// NewScrubber builds a Scrubber recognising DefaultThinkingTags plus extra.
func NewScrubber(extra []string) *Scrubber {
	s := &Scrubber{tags: make([]string, 0, len(DefaultThinkingTags)+len(extra))}
	seen := map[string]bool{}
	add := func(t string) {
		t = strings.ToLower(strings.TrimSpace(strings.Trim(t, "<>/")))
		if t == "" || seen[t] {
			return
		}
		seen[t] = true
		s.tags = append(s.tags, t)
	}
	for _, t := range DefaultThinkingTags {
		add(t)
	}
	for _, t := range extra {
		add(t)
	}
	return s
}

// Write feeds a delta and returns the text that is safe to emit now.
func (s *Scrubber) Write(delta string) string {
	s.pend += delta
	var out strings.Builder
	for {
		if s.inside {
			// Everything up to the matching close tag is dropped.
			idx, n := s.findClose()
			if idx < 0 {
				return out.String()
			}
			s.pend = s.pend[idx+n:]
			s.inside, s.open = false, ""
			continue
		}
		lt := strings.IndexByte(s.pend, '<')
		if lt < 0 {
			out.WriteString(s.pend)
			s.pend = ""
			return out.String()
		}
		out.WriteString(s.pend[:lt])
		s.pend = s.pend[lt:]
		gt := strings.IndexByte(s.pend, '>')
		if gt < 0 {
			if s.couldOpen(s.pend) {
				return out.String() // wait for more bytes
			}
			out.WriteByte('<')
			s.pend = s.pend[1:]
			continue
		}
		name, closing := parseTag(s.pend[:gt+1])
		if !s.known(name) {
			// Not ours: emit the '<' and rescan, so a real tag that starts
			// inside what looked like a token is still found.
			out.WriteByte('<')
			s.pend = s.pend[1:]
			continue
		}
		s.pend = s.pend[gt+1:]
		if !closing {
			s.inside, s.open = true, name
		}
		// A stray close tag outside a block is simply swallowed.
	}
}

// Flush returns whatever is left once the stream ends. Text withheld inside
// an unterminated reasoning block is dropped; anything else was ordinary text
// that never became a tag.
func (s *Scrubber) Flush() string {
	if s.inside {
		s.pend = ""
		return ""
	}
	out := s.pend
	s.pend = ""
	return out
}

// findClose locates the close tag for the open block in pend. It returns the
// offset and length of the tag, or -1 when it has not arrived yet.
func (s *Scrubber) findClose() (int, int) {
	from := 0
	for {
		i := strings.IndexByte(s.pend[from:], '<')
		if i < 0 {
			s.pend = "" // all dropped; nothing here can start a tag
			return -1, 0
		}
		i += from
		gt := strings.IndexByte(s.pend[i:], '>')
		if gt < 0 {
			s.pend = s.pend[i:] // a tag is still arriving; keep it
			return -1, 0
		}
		name, closing := parseTag(s.pend[i : i+gt+1])
		if closing && name == s.open {
			return i, gt + 1
		}
		from = i + 1
	}
}

func (s *Scrubber) known(name string) bool {
	for _, t := range s.tags {
		if t == name {
			return true
		}
	}
	return false
}

// couldOpen reports whether frag (which starts with '<' and holds no '>') may
// still become one of our tags once more bytes arrive.
func (s *Scrubber) couldOpen(frag string) bool {
	low := strings.ToLower(frag)
	for _, t := range s.tags {
		for _, full := range [2]string{"<" + t, "</" + t} {
			if len(low) <= len(full) {
				if strings.HasPrefix(full, low) {
					return true
				}
				continue
			}
			// Longer than the name: only attributes/whitespace may follow.
			if strings.HasPrefix(low, full) && isTagTail(low[len(full):]) {
				return true
			}
		}
	}
	return false
}

// isTagTail reports whether rest can be the inside of a tag before its '>'.
func isTagTail(rest string) bool {
	return rest[0] == ' ' || rest[0] == '\t' || rest[0] == '/' || rest[0] == '\n' || rest[0] == '\r'
}

// parseTag extracts the lower-case name of a complete "<...>" token and says
// whether it is a closing tag.
func parseTag(tok string) (name string, closing bool) {
	body := strings.TrimSuffix(strings.TrimPrefix(tok, "<"), ">")
	body = strings.TrimSuffix(body, "/")
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "/") {
		closing = true
		body = strings.TrimSpace(body[1:])
	}
	if i := strings.IndexAny(body, " \t\r\n"); i >= 0 {
		body = body[:i]
	}
	return strings.ToLower(body), closing
}
