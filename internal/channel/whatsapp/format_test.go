package whatsapp

import (
	"strings"
	"testing"

	"holaclaw/internal/channel/textutil"
)

func TestMarkdownToWhatsApp(t *testing.T) {
	for in, want := range map[string]string{
		"**bold** and __bold__":         "*bold* and *bold*",
		"*italic* and _italic_":         "_italic_ and _italic_",
		"~~gone~~":                      "~gone~",
		"`**code**` stays":              "`**code**` stays",
		"# Title":                       "*Title*",
		"## **Already** bold":           "*Already bold*",
		"> quoted **text**":             "> quoted *text*",
		"* one\n- two\n1. three":        "- one\n- two\n1. three",
		"[docs](https://x.io)":          "docs (https://x.io)",
		"[https://x.io](https://x.io)":  "https://x.io",
		"![chart](https://x.io/c.png)":  "chart (https://x.io/c.png)",
		"```go\nx := **y**\n```":        "```\nx := **y**\n```",
		"2 * 3 * 4":                     "2 * 3 * 4",
		"unclosed `**bold** tick":       "unclosed `**bold** tick",
		"a **b *c* d** e":               "a *b _c_ d* e",
		"snake_case_name stays as text": "snake_case_name stays as text",
	} {
		if got := mdToWA(in); got != want {
			t.Errorf("mdToWA(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitText(t *testing.T) {
	if got := splitText("short", 4000); len(got) != 1 || got[0] != "short" {
		t.Fatalf("short = %q", got)
	}
	if got := splitText("  \n ", 4000); len(got) != 0 {
		t.Fatalf("blank = %q", got)
	}
	// Paragraphs are preferred over lines and words.
	p, q := strings.Repeat("a", 80), strings.Repeat("b", 30)
	got := splitText(p+"\n\n"+q+"\n"+p, 150)
	if len(got) != 2 || got[0] != p || got[1] != q+"\n"+p {
		t.Fatalf("paragraph split = %q", got)
	}
	// No break at all: a hard cut that keeps every rune whole.
	long := strings.Repeat("ñ", 250)
	got = splitText(long, 100)
	if strings.Join(got, "") != long {
		t.Fatal("hard cuts lost text")
	}
	for _, part := range got {
		if textutil.UTF16Len(part) > 100 {
			t.Fatalf("part of %d units", textutil.UTF16Len(part))
		}
	}
	// Emoji count twice, as UTF-16.
	for _, part := range splitText(strings.Repeat("😀", 120), 100) {
		if textutil.UTF16Len(part) > 100 {
			t.Fatalf("emoji part of %d units", textutil.UTF16Len(part))
		}
	}
	// A code block cut in two is closed and reopened.
	code := "```\n" + strings.Repeat("line of code\n", 30) + "```"
	for i, part := range splitText(code, 120) {
		if strings.Count(part, "```")%2 != 0 {
			t.Fatalf("part %d leaves a block open: %q", i, part)
		}
		if textutil.UTF16Len(part) > 120 {
			t.Fatalf("part %d has %d units", i, textutil.UTF16Len(part))
		}
	}
}
