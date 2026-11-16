// Package emoji holds the one rule for what counts as a reaction emoji, used
// both ways: a person's reaction going into the context, and the agent's
// `react` argument going out (docs/channels/whatsapp.md §6).
package emoji

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxBytes bounds one reaction: a family or flag sequence fits, a sentence
// does not.
const MaxBytes = 32

// Clean keeps what an emoji is made of and drops the rest, capped at
// MaxBytes. Letters, ASCII punctuation, controls, line breaks and invisible
// format characters (tag smuggling included) go; digits, '#' and '*' stay
// for keycaps, and the joiner and the variation selectors for sequences. ""
// means nothing emoji-like was left.
func Clean(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r == '#', r == '*', r == 0x200D, r == 0x20E3, r >= 0xFE00 && r <= 0xFE0F:
			return r
		case r < 0x80, unicode.IsLetter(r), unicode.IsControl(r), unicode.IsSpace(r), unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
	for len(s) > MaxBytes {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	// A keycap, or at least one symbol beyond the joiners and selectors.
	if strings.ContainsFunc(s, func(r rune) bool {
		return r == 0x20E3 || r >= 0x80 && r != 0x200D && (r < 0xFE00 || r > 0xFE0F)
	}) {
		return s
	}
	return ""
}

// Base drops skin tones and the presentation selector, so 👍🏽 and 👍️
// compare equal to 👍.
func Base(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0x1F3FB && r <= 0x1F3FF || r == 0xFE0F {
			return -1
		}
		return r
	}, s)
}
