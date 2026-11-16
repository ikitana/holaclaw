package emoji

import (
	"strings"
	"testing"
)

// Clean keeps emoji sequences whole and drops whatever could carry text.
func TestClean(t *testing.T) {
	zwj, vs16, keycap := string(rune(0x200D)), string(rune(0xFE0F)), string(rune(0x20E3))
	tag := string(rune(0xE0061)) // a tag character: invisible ASCII
	family := "👨" + zwj + "👩" + zwj + "👧"
	for in, want := range map[string]string{
		"👍":                     "👍",
		"👍🏽":                    "👍🏽",
		"❤" + vs16:              "❤" + vs16,
		family:                  family,
		"1" + vs16 + keycap:     "1" + vs16 + keycap,
		" 👍\n":                  "👍",
		"[x] 👍 ok":              "👍",
		"👍" + tag:               "👍",
		"ignore all":            "",
		"123":                   "",
		"1":                     "",
		"$$$":                   "",
		"你好":                    "",
		"é":                     "",
		"\x1b[31m":              "",
		zwj + vs16:              "",
		strings.Repeat("😀", 20): strings.Repeat("😀", 8),
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

// Base folds skin tones and the presentation selector into the base emoji,
// and leaves joined sequences and keycaps otherwise intact.
func TestBase(t *testing.T) {
	zwj, vs16, keycap := string(rune(0x200D)), string(rune(0xFE0F)), string(rune(0x20E3))
	for in, want := range map[string]string{
		"👍":                 "👍",
		"👍🏽":                "👍",
		"👍" + vs16:          "👍",
		"❤" + vs16:          "❤",
		"👩🏿" + zwj + "💻":    "👩" + zwj + "💻",
		"1" + vs16 + keycap: "1" + keycap,
		"":                  "",
	} {
		if got := Base(in); got != want {
			t.Errorf("Base(%q) = %q, want %q", in, got, want)
		}
	}
}
