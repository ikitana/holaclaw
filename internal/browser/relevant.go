package browser

import (
	"sort"
	"strings"
	"unicode"
)

// Finding the part of a page a request is about.
//
// Both halves of the extraction rung used to read a page from the top: the
// selection rung was offered the first maxSpans spans in page order, and the
// chat rung the first extractTextBytes of the text. On the pages people
// actually ask about — a shop's search results, a listing, a timetable — the
// top of the page is the cookie banner, the account menu, the department nav
// and the filter rail, and what was asked for is a long way below all of it.
// Both rungs then answer honestly about the wrong part of the page.
//
// So the page is matched against the request first. Nothing here judges
// anything: it is a word match, with the accents folded, and its only job is
// to make sure that what the request talks about is in front of the model.
// The models still decide.

// Bounds of the matching.
const (
	// minTermLen keeps a term worth matching on. Two, not three: "18" in
	// "iPhone 18 Pro" is the most distinguishing word of that request.
	minTermLen = 2
	// spanBefore and spanAfter are the spans kept around one the request's
	// words appear in, when the page has more spans than can be offered: a
	// list follows its heading, so more are kept after a hit than before it.
	spanBefore = 2
	spanAfter  = 8
)

// requestTerms are the words of the request worth matching a page against:
// folded, deduplicated, in order, without the ones every sentence has.
func requestTerms(request string) []string {
	out := make([]string, 0, 12)
	seen := map[string]bool{}
	for _, w := range strings.Fields(foldText(request)) {
		if len(w) < minTermLen || commonWords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// commonWords are the words that match everything and so distinguish nothing.
// Both languages this is most used in are here; a word that slips through
// costs a little ranking noise, not an answer.
var commonWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "into": true,
	"that": true, "this": true, "what": true, "whats": true, "which": true, "give": true,
	"show": true, "find": true, "get": true, "tell": true, "please": true, "much": true,
	"how": true, "does": true, "cost": true, "are": true, "you": true, "can": true,
	"de": true, "del": true, "la": true, "el": true, "los": true, "las": true, "un": true,
	"una": true, "en": true, "por": true, "para": true, "que": true, "con": true,
	"cuanto": true, "cual": true, "dame": true, "busca": true, "es": true, "al": true,
}

// foldText lowercases, strips the accents this is likely to meet, and turns
// everything that is not a letter or a digit into a space, so "Inglés" and
// "ingles" are one word and "1.469,00€" is matched by "1", "469" and "00" as
// well as whole.
func foldText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if f, ok := folded[r]; ok {
			b.WriteRune(f)
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			continue
		}
		b.WriteByte(' ')
	}
	return b.String()
}

// folded maps the accented letters of the western European languages onto
// their plain forms. A table rather than a normaliser: it is the whole of what
// is needed here, and it costs no dependency.
var folded = map[rune]rune{
	'á': 'a', 'à': 'a', 'ä': 'a', 'â': 'a', 'ã': 'a', 'å': 'a',
	'é': 'e', 'è': 'e', 'ë': 'e', 'ê': 'e',
	'í': 'i', 'ì': 'i', 'ï': 'i', 'î': 'i',
	'ó': 'o', 'ò': 'o', 'ö': 'o', 'ô': 'o', 'õ': 'o',
	'ú': 'u', 'ù': 'u', 'ü': 'u', 'û': 'u',
	'ñ': 'n', 'ç': 'c', 'ý': 'y', 'ÿ': 'y', 'ø': 'o', 'æ': 'a', 'ß': 's',
}

// matches counts how many of the request's terms a piece of page text carries.
// A term is matched as a whole word, because "pro" inside "producto" says
// nothing about an iPhone Pro.
func matches(text string, terms []string) int {
	if len(terms) == 0 {
		return 0
	}
	words := map[string]bool{}
	for _, w := range strings.Fields(foldText(text)) {
		words[w] = true
	}
	n := 0
	for _, t := range terms {
		if words[t] {
			n++
		}
	}
	return n
}

// rankSpans cuts a shortlist of spans out of a longer list: the ones the
// request's own words appear in first, and page order within that, so the
// model is offered the part of the page it was asked about rather than the
// first screenful of it. Everything is kept when there is room for it, which
// is every page small enough for this not to matter.
func rankSpans(spans []string, terms []string, max int) []string {
	if len(spans) <= max || len(terms) == 0 {
		if len(spans) > max {
			return spans[:max]
		}
		return spans
	}
	type scored struct {
		idx, hits int
	}
	all := make([]scored, 0, len(spans))
	for i, s := range spans {
		all = append(all, scored{i, matches(s, terms)})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].hits > all[j].hits })

	// The spans the words appear in first — but a list is the one shape where
	// the answer is *next to* the match rather than in it: "top models today"
	// matches the heading, and the rows under it name models and numbers and
	// nothing the request said. So the spans that follow a hit, and a couple
	// before it, are kept ahead of weaker hits further down the page, as far
	// as the cap allows.
	keep := make([]bool, len(spans))
	kept := 0
	mark := func(i int) {
		if i >= 0 && i < len(spans) && !keep[i] && kept < max {
			keep[i] = true
			kept++
		}
	}
	for _, s := range all {
		if s.hits == 0 || kept >= max {
			break
		}
		mark(s.idx)
		for d := 1; d <= spanBefore; d++ {
			mark(s.idx - d)
		}
		for d := 1; d <= spanAfter; d++ {
			mark(s.idx + d)
		}
	}
	for _, s := range all {
		if kept >= max {
			break
		}
		mark(s.idx)
	}
	out := make([]string, 0, max)
	for i, s := range spans {
		if keep[i] {
			out = append(out, s)
		}
	}
	return out
}
