package wire

import (
	"strings"
	"testing"
)

// runScrubber feeds in at the given split points and returns the emitted text.
func runScrubber(extra []string, pieces []string) string {
	s := NewScrubber(extra)
	var out strings.Builder
	for _, p := range pieces {
		out.WriteString(s.Write(p))
	}
	out.WriteString(s.Flush())
	return out.String()
}

func TestScrubberWholeInput(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain text", "plain text"},
		{"<think>hidden</think>visible", "visible"},
		{"a<thinking>x</thinking>b", "ab"},
		{"a<reasoning>x</reasoning>b<thought>y</thought>c", "abc"},
		{"<reflection>\nmulti\nline\n</reflection>done", "done"},
		{"keep <b>bold</b> markup", "keep <b>bold</b> markup"},
		{"math 3 < 4 and 5 > 2", "math 3 < 4 and 5 > 2"},
		{"<THINK>caps</THINK>ok", "ok"},
		{"<think attr=\"1\">x</think>ok", "ok"},
		{"unterminated <think>forever", "unterminated "},
		{"trailing <thi", "trailing <thi"},
		{"stray </think>text", "stray text"},
		{"<think>a</think><think>b</think>c", "c"},
		{"<think>inner <tag> junk</think>after", "after"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := runScrubber(nil, []string{tc.in}); got != tc.want {
			t.Errorf("scrub(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

// TestScrubberSplitsAreIdentical splits each input at every position (and at
// every pair of positions) and asserts the output never changes.
func TestScrubberSplitsAreIdentical(t *testing.T) {
	inputs := []string{
		"before<think>hidden</think>after",
		"a<thinking>x</thinking>b<reasoning>y</reasoning>c",
		"no tags at all, just <angle brackets> and text",
		"<think>only reasoning</think>",
		"tail <think>cut off mid",
		"<reflection>a</reflection><thought>b</thought>visible",
		"3 < 4 <think>x</think> 5 > 2",
		"<think attr=\"x\" >spaced</think>end",
		"<ponder>custom</ponder>kept",
	}
	extra := []string{"ponder"}
	for _, in := range inputs {
		want := runScrubber(extra, []string{in})
		for i := 0; i <= len(in); i++ {
			got := runScrubber(extra, []string{in[:i], in[i:]})
			if got != want {
				t.Fatalf("split %q at %d = %q want %q", in, i, got, want)
			}
		}
		for i := 0; i <= len(in); i++ {
			for j := i; j <= len(in); j++ {
				got := runScrubber(extra, []string{in[:i], in[i:j], in[j:]})
				if got != want {
					t.Fatalf("split %q at %d,%d = %q want %q", in, i, j, got, want)
				}
			}
		}
		// Byte at a time.
		var pieces []string
		for i := 0; i < len(in); i++ {
			pieces = append(pieces, in[i:i+1])
		}
		if got := runScrubber(extra, pieces); got != want {
			t.Fatalf("byte-wise %q = %q want %q", in, got, want)
		}
	}
}

func TestScrubberNeverEmitsPartialTagText(t *testing.T) {
	in := "ok<think>secret</think>done"
	s := NewScrubber(nil)
	for i := 0; i < len(in); i++ {
		out := s.Write(in[i : i+1])
		if strings.ContainsAny(out, "<>") || strings.Contains(out, "secret") {
			t.Fatalf("emitted %q at byte %d", out, i)
		}
	}
	if rest := s.Flush(); rest != "" {
		t.Fatalf("flush = %q", rest)
	}
}

func TestScrubberCustomTagsDeduped(t *testing.T) {
	s := NewScrubber([]string{"think", "<scratch>", "SCRATCH"})
	if len(s.tags) != len(DefaultThinkingTags)+1 {
		t.Fatalf("tags = %v", s.tags)
	}
	if got := runScrubber([]string{"scratch"}, []string{"a<scratch>b</scratch>c"}); got != "ac" {
		t.Errorf("got %q", got)
	}
}
