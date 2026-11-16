// Package textutil holds the UTF-16 measuring and slicing that Telegram and
// WhatsApp both need: their length limits and entity offsets are counted in
// UTF-16 code units, not bytes or runes, because that is how their own
// clients count.
package textutil

import "unicode/utf16"

// UTF16Len is the length of s in UTF-16 code units.
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		n += utf16Width(r)
	}
	return n
}

// UTF16Prefix returns the byte length of the longest prefix of s that fits
// limit UTF-16 code units, on a rune boundary.
func UTF16Prefix(s string, limit int) int {
	n := 0
	for i, r := range s {
		n += utf16Width(r)
		if n > limit {
			return i
		}
	}
	return len(s)
}

// UTF16Sub returns the substring of s at the given UTF-16 offset and length.
func UTF16Sub(s string, off, length int) string {
	if off < 0 || length <= 0 {
		return ""
	}
	start, end, at := -1, len(s), 0
	for i, r := range s {
		if at == off && start < 0 {
			start = i
		}
		if at == off+length {
			end = i
			break
		}
		at += utf16Width(r)
	}
	if at == off && start < 0 {
		start = len(s)
	}
	if start < 0 || start > end {
		return ""
	}
	return s[start:end]
}

// utf16Width is a rune's width in UTF-16 code units: 2 for a surrogate pair,
// 1 otherwise. RuneLen returns -1 only for a value invalid to encode, which
// ranging over a string never yields, but 1 is the safe fallback.
func utf16Width(r rune) int {
	if n := utf16.RuneLen(r); n > 0 {
		return n
	}
	return 1
}
