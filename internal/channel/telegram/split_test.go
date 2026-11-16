package telegram

import (
	"strings"
	"testing"
)

// UTF16Len and UTF16Sub themselves are tested in internal/channel/textutil;
// this package only uses them (entity offsets are covered in inbound_test.go).

func TestVisibleLen(t *testing.T) {
	if got := visibleLen(`<b>ab</b>`); got != 2 {
		t.Fatalf("tags must not count: %d", got)
	}
	if got := visibleLen(`&amp;&lt;`); got != 2 {
		t.Fatalf("entities count as one each: %d", got)
	}
	if got := visibleLen("😀"); got != 2 {
		t.Fatalf("emoji is two code units: %d", got)
	}
}

// eachChunkValid asserts the invariants every produced chunk must hold.
func eachChunkValid(t *testing.T, chunks []string, limit int) {
	t.Helper()
	for i, c := range chunks {
		if n := visibleLen(c); n > limit {
			t.Fatalf("chunk %d is %d units, limit %d", i, n, limit)
		}
		if err := validateTags(c); err != nil {
			t.Fatalf("chunk %d not balanced: %v\n%q", i, err, c)
		}
	}
}

func TestSplitHTMLPlainText(t *testing.T) {
	body := strings.TrimSpace(strings.Repeat("Sentence number one. Another sentence here.\n\n", 400))
	chunks := splitHTML(mdToHTML(body), maxTextChars)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	eachChunkValid(t, chunks, maxTextChars)
	if joined := strings.Join(chunks, "\n\n"); len(htmlToPlain(joined)) < len(body)-len(chunks)*4 {
		t.Fatalf("content lost while splitting")
	}
}

// TestSplitHTMLEmoji checks the UTF-16 accounting: 2200 emoji are 4400 code
// units and must not fit in one 4096-unit message.
func TestSplitHTMLEmoji(t *testing.T) {
	body := strings.Repeat("😀 ", 2200)
	chunks := splitHTML(mdToHTML(body), maxTextChars)
	if len(chunks) < 2 {
		t.Fatalf("emoji counted as one unit each: got %d chunk(s)", len(chunks))
	}
	eachChunkValid(t, chunks, maxTextChars)
}

// TestSplitHTMLCodeBlock checks that a code block spanning a cut is closed and
// reopened with its attributes intact.
func TestSplitHTMLCodeBlock(t *testing.T) {
	var b strings.Builder
	b.WriteString("```go\n")
	for i := 0; i < 400; i++ {
		b.WriteString("fmt.Println(\"a line of code that is reasonably long\")\n")
	}
	b.WriteString("```\n")
	chunks := splitHTML(mdToHTML(b.String()), maxTextChars)
	if len(chunks) < 2 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	eachChunkValid(t, chunks, maxTextChars)
	for i, c := range chunks {
		if !strings.HasPrefix(c, `<pre><code class="language-go">`) {
			t.Fatalf("chunk %d does not reopen the code block: %.60q", i, c)
		}
		if !strings.HasSuffix(strings.TrimSpace(c), "</code></pre>") {
			t.Fatalf("chunk %d does not close the code block: %q", i, c[len(c)-30:])
		}
	}
}

// TestSplitHTMLNeverSplitsMarkup runs a mixed document and asserts no chunk ends
// inside a tag or an entity.
func TestSplitHTMLNeverSplitsMarkup(t *testing.T) {
	md := strings.Repeat("**bold** _em_ `c` [l](https://a.test) a & b < c 😀\n", 400)
	chunks := splitHTML(mdToHTML(md), maxTextChars)
	eachChunkValid(t, chunks, maxTextChars)
	for i, c := range chunks {
		if strings.Count(c, "<") != strings.Count(c, ">") {
			t.Fatalf("chunk %d has a truncated tag", i)
		}
		if strings.HasSuffix(c, "&") || strings.HasSuffix(c, "&am") {
			t.Fatalf("chunk %d has a truncated entity", i)
		}
	}
}

func TestSplitHTMLShortInput(t *testing.T) {
	if got := splitHTML("<b>hi</b>", maxTextChars); len(got) != 1 || got[0] != "<b>hi</b>" {
		t.Fatalf("short input must pass through: %q", got)
	}
	if got := splitHTML("   ", maxTextChars); got != nil {
		t.Fatalf("blank input must produce no message: %q", got)
	}
}

// TestSplitHTMLHardBoundary covers text with no whitespace at all.
func TestSplitHTMLHardBoundary(t *testing.T) {
	chunks := splitHTML(strings.Repeat("x", 10000), maxTextChars)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(chunks))
	}
	eachChunkValid(t, chunks, maxTextChars)
	if n := visibleLen(chunks[0]); n != maxTextChars {
		t.Fatalf("first chunk should fill the message: %d", n)
	}
}
