// Package tokens is the single token estimator used for budgets: tool schema
// caps, the context builder, compaction and `holaclaw prompt-size`. It is a
// deliberate, model-independent heuristic so the numbers are reproducible in
// CI; it errs on the high side.
package tokens

import "unicode"

// Estimate returns the estimated token count of s.
//
// Heuristic: a run of letters/digits costs one token per 4 runes (rounded
// up); every other non-space rune costs one token; whitespace is free except
// newlines.
func Estimate(s string) int {
	n, run := 0, 0
	flush := func() {
		if run > 0 {
			n += (run + 3) / 4
			run = 0
		}
	}
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			run++
		case r == '\n':
			flush()
			n++
		case unicode.IsSpace(r):
			flush()
		default:
			flush()
			n++
		}
	}
	flush()
	return n
}

// PerMessageOverhead is added for every chat message (role markers, framing).
const PerMessageOverhead = 4
