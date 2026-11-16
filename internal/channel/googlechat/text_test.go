package googlechat

import (
	"strings"
	"testing"
)

func TestMarkdownToChat(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"**bold** and __also__", "*bold* and *also*"},
		{"*italic* and _italic_", "_italic_ and _italic_"},
		{"~~gone~~", "~gone~"},
		{"# Title", "*Title*"},
		{"see [docs](https://example.com/x)", "see <https://example.com/x|docs>"},
		{"`**code**` stays", "`**code**` stays"},
		{"```\n**raw**\n```", "```\n**raw**\n```"},
		{"ping <users/all> now", "ping <" + zwsp + "users/all> now"},
		{"<https://evil.example|bank>", "<" + zwsp + "https://evil.example|bank>"},
		{"a < b > c", "a < b > c"},
	} {
		if got := markdownToChat(tc.in); got != tc.want {
			t.Errorf("markdownToChat(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestChunkText(t *testing.T) {
	if chunkText("  \n", 100) != nil {
		t.Fatal("blank text is no chunk")
	}
	long := strings.Repeat("x", 250)
	chunks := chunkText(long, 100)
	for _, c := range chunks {
		if len(c) > 100 {
			t.Fatalf("chunk of %d", len(c))
		}
	}
	if strings.Join(chunks, "") != long {
		t.Fatal("hard cut lost text")
	}
	fenced := "```go\n" + strings.Repeat("line\n", 40) + "```"
	for _, c := range chunkText(fenced, 64) {
		if strings.Count(c, "```")%2 != 0 {
			t.Fatalf("unbalanced fence in %q", c)
		}
	}
	multi := strings.Repeat("é", 300) // 600 bytes
	for _, c := range chunkText(multi, 101) {
		if !strings.HasPrefix(c, "é") || len(c)%2 != 0 {
			t.Fatalf("cut inside a rune: %q", c)
		}
	}
}

// A long info string on the opener is not copied into the reopened fence, so
// no chunk passes the limit.
func TestChunkTextLongInfoString(t *testing.T) {
	info := "python " + strings.Repeat("title=\"x\" ", 300)
	body := strings.Repeat("print(1)\n", 900)
	for _, max := range []int{maxTextChars, 200} {
		chunks := chunkText("```"+info+"\n"+body+"```", max)
		if len(chunks) < 2 {
			t.Fatalf("max %d: chunks = %d, want a split", max, len(chunks))
		}
		for i, c := range chunks {
			if len(c) > max {
				t.Fatalf("max %d: chunk %d is %d bytes", max, i, len(c))
			}
			if strings.Count(c, "```")%2 != 0 {
				t.Fatalf("max %d: unbalanced fence in chunk %d", max, i)
			}
			if i > 0 && !strings.HasPrefix(c, "```python\n") {
				t.Fatalf("max %d: chunk %d reopens as %q", max, i, c[:min(len(c), 30)])
			}
		}
	}
	if got := reopener("```" + strings.Repeat("a", 40)); len(got) != maxReopener {
		t.Fatalf("reopener = %q, want %d bytes", got, maxReopener)
	}
}
