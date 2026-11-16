package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"rsc.io/qr"
)

// The code is square: two module rows per line, every line as wide as the
// code plus its quiet zone, and the quiet zone drawn light all round.
func TestWriteQR(t *testing.T) {
	const link = "https://t.me/holaclaw_bot?start=7KQ2M9XA"
	c, err := qr.Encode(link, qr.L)
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	writeQR(&sb, link)
	lines := strings.Split(strings.TrimSuffix(sb.String(), "\n"), "\n")
	side := c.Size + 2*qrQuiet
	if want := (side + 1) / 2; len(lines) != want {
		t.Fatalf("lines = %d, want %d", len(lines), want)
	}
	for i, l := range lines {
		l = strings.TrimPrefix(l, "  ")
		if n := utf8.RuneCountInString(l); n != side {
			t.Fatalf("line %d is %d wide, want %d", i, n, side)
		}
		if !strings.HasPrefix(l, "██") || !strings.HasSuffix(l, "██") {
			t.Fatalf("line %d = %q: the quiet zone is not light", i, l)
		}
	}
	if strings.TrimPrefix(lines[0], "  ") != strings.Repeat("█", side) {
		t.Fatalf("first line = %q, want all light", lines[0])
	}
	// The top-left finder's dark ring starts right after the quiet zone.
	if r := []rune(strings.TrimPrefix(lines[1], "  ")); r[qrQuiet] != ' ' {
		t.Fatalf("line 1 = %q: no dark finder at the corner", lines[1])
	}
}
