package slack

import (
	"strings"
	"testing"
)

func resolveFake(id string) string {
	switch id {
	case "U1":
		return "alice"
	case "U2":
		return "bob"
	}
	return id
}

func TestSlackToMarkdown(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain text", "plain text"},
		{"hi <@U1>", "hi @alice"},
		{"hi <@U404>", "hi @U404"},
		{"see <#C123|general>", "see #general"},
		{"see <#C123>", "see #C123"},
		{"<https://example.com|the site>", "[the site](https://example.com)"},
		{"<https://example.com>", "https://example.com"},
		{"a &amp; b &lt;c&gt;", "a & b <c>"},
		{"<https://x.test/?a=1&amp;b=2|q>", "[q](https://x.test/?a=1&b=2)"},
		{"<!here> everyone", "@here everyone"},
		{"unclosed < bracket", "unclosed < bracket"},
	}
	for _, tc := range cases {
		if got := slackToMarkdown(tc.in, resolveFake); got != tc.want {
			t.Errorf("slackToMarkdown(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStripMentionAndDetect(t *testing.T) {
	if !mentions("hey <@UBOT> there", "UBOT") {
		t.Error("mention not detected")
	}
	if mentions("hey <@UOTHER>", "UBOT") {
		t.Error("false mention")
	}
	cases := []struct{ in, want string }{
		{"<@UBOT> hello", "hello"},
		{"hello <@UBOT>", "hello"},
		{"a <@UBOT|bot> b", "a b"},
		{"no mention", "no mention"},
	}
	for _, tc := range cases {
		if got := stripMention(tc.in, "UBOT"); got != tc.want {
			t.Errorf("stripMention(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMarkdownToMrkdwn(t *testing.T) {
	cases := []struct{ in, want string }{
		{"**bold**", "*bold*"},
		{"*italic*", "_italic_"},
		{"_italic_", "_italic_"},
		{"~~gone~~", "~gone~"},
		{"[label](https://x.test)", "<https://x.test|label>"},
		{"# Heading", "*Heading*"},
		{"### Deep heading", "*Deep heading*"},
		{"a & b < c > d", "a &amp; b &lt; c &gt; d"},
		{"use `a < b` here", "use `a &lt; b` here"},
		{"**bold** and *it* and ~~no~~", "*bold* and _it_ and ~no~"},
		{"```\nx < y\n```", "```\nx &lt; y\n```"},
		{"see [a & b](https://x.test/?q=1)", "see <https://x.test/?q=1|a &amp; b>"},
	}
	for _, tc := range cases {
		if got := markdownToMrkdwn(tc.in); got != tc.want {
			t.Errorf("markdownToMrkdwn(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestChunkTextParagraphAndLineBoundaries(t *testing.T) {
	para := strings.Repeat("word ", 100) // 500 chars
	text := strings.Join([]string{para, para, para, para, para}, "\n\n")
	got := chunkText(text, 1200)
	if len(got) < 2 {
		t.Fatalf("expected several chunks, got %d", len(got))
	}
	for i, c := range got {
		if len(c) > 1200 {
			t.Fatalf("chunk %d is %d chars", i, len(c))
		}
	}
	if strings.Contains(got[0], "\n\n\n") {
		t.Fatal("blank-line runs left behind")
	}
}

func TestChunkTextHardCutsLongLine(t *testing.T) {
	line := strings.Repeat("x", 5000)
	got := chunkText(line, 1000)
	if len(got) < 5 {
		t.Fatalf("chunks = %d", len(got))
	}
	var total int
	for _, c := range got {
		if len(c) > 1000 {
			t.Fatalf("chunk of %d chars", len(c))
		}
		total += len(c)
	}
	if total != 5000 {
		t.Fatalf("recombined length = %d, want 5000", total)
	}
}

func TestChunkTextClosesAndReopensFences(t *testing.T) {
	code := strings.Repeat("fmt.Println(\"hello world\")\n", 60)
	text := "before\n\n```go\n" + code + "```\n\nafter"
	got := chunkText(text, 600)
	if len(got) < 3 {
		t.Fatalf("chunks = %d", len(got))
	}
	for i, c := range got {
		if len(c) > 600 {
			t.Fatalf("chunk %d is %d chars", i, len(c))
		}
		if n := strings.Count(c, "```"); n%2 != 0 {
			t.Fatalf("chunk %d has an unbalanced fence:\n%s", i, c)
		}
	}
	// the continuation chunk reopens the fence with its info string
	if !strings.HasPrefix(got[2], "```go") {
		t.Fatalf("continuation chunk does not reopen the fence:\n%s", got[2])
	}
}

func TestChunkTextEmpty(t *testing.T) {
	if got := chunkText("", chunkChars); got != nil {
		t.Fatalf("chunks = %v, want none", got)
	}
}
