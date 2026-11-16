package agent

import (
	"regexp"
	"strings"

	"holaclaw/internal/event"
	"holaclaw/internal/tokens"
)

// Anchor index caps. The index is computed in Go, never by the model: a model
// paraphrases ids and paths, and a paraphrased id is worse than none
// (docs/context.md).
const (
	MaxAnchors       = 40
	MaxAnchorTokens  = 400
	maxErrorAnchors  = 10
	maxAnchorRunes   = 120
	minPathSeparator = 1
)

var (
	anchorULID   = regexp.MustCompile(`\b[0-7][0-9ABCDEFGHJKMNPQRSTVWXYZ]{25}\b`)
	anchorTicket = regexp.MustCompile(`\b[A-Z]{2,}-\d+\b`)
	anchorURL    = regexp.MustCompile(`\bhttps?://[^\s<>"'` + "`" + `)\]]+`)
	anchorPath   = regexp.MustCompile(`(?:^|[\s"'` + "`" + `(\[<])((?:~|\.{1,2})?/[\w.@+-]+(?:/[\w.@+-]+)*|[\w.@+-]+(?:/[\w.@+-]+)+)`)
	anchorError  = regexp.MustCompile(`(?i)error|exception|failed|panic`)
)

// harvestAnchors extracts the literal strings a summary must not paraphrase:
// ids (ULIDs and ticket-like keys), file paths, URLs and error lines. The
// result is deduplicated, in that order, and capped by both MaxAnchors and
// MaxAnchorTokens.
func harvestAnchors(msgs []event.Message) []string {
	text := renderTranscript(msgs)

	var out []string
	seen := map[string]bool{}
	add := func(s string) bool {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return true
		}
		if len([]rune(s)) > maxAnchorRunes {
			s = string([]rune(s)[:maxAnchorRunes]) + "…"
			if seen[s] {
				return true
			}
		}
		seen[s] = true
		out = append(out, s)
		return len(out) < MaxAnchors && tokens.Estimate(strings.Join(out, "\n")) < MaxAnchorTokens
	}

	for _, m := range anchorULID.FindAllString(text, -1) {
		if !add(m) {
			return out
		}
	}
	for _, m := range anchorTicket.FindAllString(text, -1) {
		if !add(m) {
			return out
		}
	}
	urls := anchorURL.FindAllString(text, -1)
	// Paths are harvested from the text with URLs removed, so a URL's own
	// path never shows up twice.
	pathText := text
	for _, u := range urls {
		pathText = strings.ReplaceAll(pathText, u, " ")
	}
	for _, m := range anchorPath.FindAllStringSubmatch(pathText, -1) {
		if strings.Count(m[1], "/") < minPathSeparator {
			continue
		}
		if !add(strings.Trim(m[1], ".,;:)")) {
			return out
		}
	}
	for _, u := range urls {
		if !add(strings.Trim(u, ".,;:")) {
			return out
		}
	}
	errs := 0
	for _, l := range strings.Split(text, "\n") {
		if errs >= maxErrorAnchors {
			break
		}
		if !anchorError.MatchString(l) {
			continue
		}
		errs++
		if !add(l) {
			return out
		}
	}
	return out
}
