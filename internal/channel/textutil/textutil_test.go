package textutil

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func TestUTF16Len(t *testing.T) {
	cases := []struct {
		s    string
		want int
	}{
		{"", 0}, {"abc", 3}, {"héllo", 5},
		{"😀", 2},   // a surrogate pair
		{"a😀b", 4}, // emoji between plain runes
		{"😀😀", 4},  // two surrogate pairs back to back
	}
	for _, c := range cases {
		if got := UTF16Len(c.s); got != c.want {
			t.Errorf("UTF16Len(%q) = %d, want %d", c.s, got, c.want)
		}
		if got, want := UTF16Len(c.s), len(utf16.Encode([]rune(c.s))); got != want {
			t.Errorf("UTF16Len(%q) = %d, stdlib says %d", c.s, got, want)
		}
	}
}

func TestUTF16Prefix(t *testing.T) {
	cases := []struct {
		s     string
		limit int
		want  string // the prefix UTF16Prefix's byte length should slice out
	}{
		{"", 10, ""},
		{"abc", 2, "ab"},
		{"abc", 0, ""},
		{"abc", 10, "abc"},
		// An emoji (2 units) must not be split: at limit 1 it does not fit at
		// all, so the prefix stops before it.
		{"a😀b", 1, "a"},
		{"a😀b", 2, "a"},
		{"a😀b", 3, "a😀"},
		{"a😀b", 4, "a😀b"},
		{"😀😀", 3, "😀"}, // the second pair does not fit in the last unit
	}
	for _, c := range cases {
		n := UTF16Prefix(c.s, c.limit)
		if got := c.s[:n]; got != c.want {
			t.Errorf("UTF16Prefix(%q, %d) = %q, want %q", c.s, c.limit, got, c.want)
		}
		if UTF16Len(c.s[:n]) > c.limit {
			t.Errorf("UTF16Prefix(%q, %d): prefix %q exceeds the limit", c.s, c.limit, c.s[:n])
		}
	}
}

func TestUTF16Sub(t *testing.T) {
	if got := UTF16Sub("😀@holaclaw hi", 2, 9); got != "@holaclaw" {
		t.Fatalf("UTF16Sub = %q", got)
	}
	if got := UTF16Sub("abc", 1, 2); got != "bc" {
		t.Fatalf("UTF16Sub = %q", got)
	}
	if got := UTF16Sub("abc", -1, 2); got != "" {
		t.Fatalf("negative offset: UTF16Sub = %q", got)
	}
	if got := UTF16Sub("abc", 1, 0); got != "" {
		t.Fatalf("zero length: UTF16Sub = %q", got)
	}
	if got := UTF16Sub("abc", 10, 2); got != "" {
		t.Fatalf("offset past the end: UTF16Sub = %q", got)
	}
	if got := UTF16Sub("abc", 3, 1); got != "" {
		t.Fatalf("offset at the end: UTF16Sub = %q", got)
	}
	// A surrogate pair in range is kept whole.
	if got := UTF16Sub("a😀b", 1, 2); got != "😀" {
		t.Fatalf("UTF16Sub over a surrogate pair = %q", got)
	}
}

func TestUTF16LenLong(t *testing.T) {
	// A sanity check against a longer mixed string, byte by byte against the
	// standard library's own UTF-16 encoder.
	s := strings.Repeat("a😀b héllo ", 50)
	if got, want := UTF16Len(s), len(utf16.Encode([]rune(s))); got != want {
		t.Fatalf("UTF16Len(long) = %d, want %d", got, want)
	}
}
