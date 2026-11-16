package page

import (
	"os"
	"strings"
	"testing"

	"holaclaw/internal/browser/cdp"
)

// goldenPageText is the file internal/browser's span test reads. The two
// packages meet there: readSnapshot writes the page text and spansOf cuts it
// up, and for a long time they disagreed about what separates one piece of
// text from the next — the writer joined everything with a space, the cutter
// split on line breaks, and every page arrived as a single span capped at
// 120 bytes. The golden is what stops that drifting apart again unseen.
const goldenPageText = "../testdata/product-page-text.txt"

// productSnapshot lays out a product page the way a real one lays out: rows
// of a capacity table, each label and price side by side, under a header and
// a title.
func productSnapshot() *cdp.Snapshot {
	type node struct {
		text       string
		x, y, w, h float64
	}
	nodes := []node{
		{"El Corte Inglés", 0, 0, 200, 20},
		{"Cesta", 900, 0, 60, 20},
		{"iPhone 18 Pro", 0, 100, 400, 40},
		{"Apple", 0, 150, 100, 20},
		{"Capacidad", 0, 200, 200, 24},
		{"128 GB", 0, 240, 100, 20},
		{"1.219,00 €", 150, 240, 120, 20},
		{"256 GB", 0, 280, 100, 20},
		{"1.349,00 €", 150, 280, 120, 20},
		{"512 GB", 0, 320, 100, 20},
		{"1.599,00 €", 150, 320, 120, 20},
		{"1 TB", 0, 360, 100, 20},
		{"1.849,00 €", 150, 360, 120, 20},
		{"Añadir a la cesta", 0, 420, 200, 40},
	}
	snap := &cdp.Snapshot{Strings: []string{"", "DIV"}}
	doc := cdp.DocumentSnapshot{}
	for i, n := range nodes {
		snap.Strings = append(snap.Strings, n.text)
		doc.Nodes.ParentIndex = append(doc.Nodes.ParentIndex, -1)
		doc.Nodes.NodeName = append(doc.Nodes.NodeName, 1)
		doc.Nodes.BackendNodeID = append(doc.Nodes.BackendNodeID, int64(i+1))
		doc.Layout.NodeIndex = append(doc.Layout.NodeIndex, i)
		doc.Layout.Bounds = append(doc.Layout.Bounds, []float64{n.x, n.y, n.w, n.h})
		doc.Layout.Text = append(doc.Layout.Text, cdp.StrIdx(i+2))
	}
	snap.Documents = []cdp.DocumentSnapshot{doc}
	return snap
}

// TestReadSnapshotBreaksVisualLines: text that shares a line is joined by a
// space, text that starts a new one by a newline. A page whose every word is
// space-joined is one span to everything downstream, which is how a request
// for the price of each capacity came back with two of them.
func TestReadSnapshotBreaksVisualLines(t *testing.T) {
	_, _, got := readSnapshot(productSnapshot(), textBudget(1<<10), 0)

	want, err := os.ReadFile(goldenPageText)
	if err != nil {
		t.Fatalf("golden: %v", err)
	}
	if got.text != strings.TrimRight(string(want), "\n") {
		t.Fatalf("page text =\n%s\n\nwant\n%s", got.text, want)
	}
	// The property the golden stands for, asserted outright.
	lines := strings.Split(got.text, "\n")
	if len(lines) != 9 {
		t.Fatalf("%d lines, want one per visual row", len(lines))
	}
	for _, price := range []string{"1.219,00 €", "1.349,00 €", "1.599,00 €", "1.849,00 €"} {
		var found bool
		for _, l := range lines {
			if strings.Contains(l, price) && strings.Contains(l, "GB") || strings.Contains(l, price) && strings.Contains(l, "TB") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is not on a line with its capacity", price)
		}
	}
}

// A run of text on one line stays on it: side-by-side boxes are one line, a
// box that starts back at the margin begins the next.
func TestSameLine(t *testing.T) {
	label := rect{x: 0, y: 240, w: 100, h: 20}
	price := rect{x: 150, y: 240, w: 120, h: 20}
	below := rect{x: 0, y: 280, w: 100, h: 20}

	if !sameLine(label, price) {
		t.Error("a price beside its label was read as a new line")
	}
	if sameLine(price, below) {
		t.Error("the next row was read as a continuation")
	}
	if sameLine(label, rect{}) || sameLine(rect{}, label) {
		t.Error("an empty box was read as sharing a line")
	}
	// Glyph boxes of one run are not exactly aligned; a pixel back is not a wrap.
	if !sameLine(price, rect{x: price.x - 1, y: 240, w: 40, h: 20}) {
		t.Error("a pixel of slack was read as a wrap")
	}
}
