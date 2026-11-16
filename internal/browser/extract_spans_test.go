package browser

import (
	"os"
	"strings"
	"testing"
)

// TestSpansOfRealPageText feeds spansOf the text readSnapshot actually
// produces — the golden that internal/browser/page writes and checks — rather
// than hand-delimited text. The old test used "a\tb|c\nd. e." and passed
// while the real pipeline handed over one space-joined line, so a product
// page became a single span capped at 120 bytes and a request for the price
// of every capacity came back with whatever fitted in those bytes.
func TestSpansOfRealPageText(t *testing.T) {
	b, err := os.ReadFile("testdata/product-page-text.txt")
	if err != nil {
		t.Fatalf("golden: %v", err)
	}
	spans := spansOf(string(b))

	for _, want := range []string{
		"128 GB 1.219,00 €",
		"256 GB 1.349,00 €",
		"512 GB 1.599,00 €",
		"1 TB 1.849,00 €",
	} {
		var found bool
		for _, s := range spans {
			if s == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is not a span of its own; spans = %q", want, spans)
		}
	}
	for _, s := range spans {
		if strings.HasSuffix(s, "…") {
			t.Errorf("a span of an ordinary page was capped: %q", s)
		}
	}
}
