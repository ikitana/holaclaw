package main

import (
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/viewport"
)

// A text panel moves on every spelling of every motion, and stops at its ends.
func TestScrollKeyMovesAPanel(t *testing.T) {
	vp := viewport.New(20, 10)
	vp.SetContent(strings.TrimSuffix(strings.Repeat("line\n", 100), "\n"))
	last := 90 // 100 lines, 10 on screen
	for _, tc := range []struct {
		from int
		keys []string
		want int
	}{
		{0, []string{"pgdown", "ctrl+f", "ctrl+v"}, 10},
		{50, []string{"pgup", "ctrl+b", "alt+v"}, 40},
		{0, []string{"ctrl+d"}, 5},
		{50, []string{"ctrl+u"}, 45},
		{0, []string{"j", "ctrl+n"}, 1},
		{50, []string{"up", "k", "ctrl+p"}, 49},
		{50, []string{"g", "home", "alt+<"}, 0},
		{50, []string{"G", "end", "alt+>"}, last},
		{0, []string{"pgup", "alt+v", "ctrl+u", "ctrl+p"}, 0},
		{last, []string{"pgdown", "ctrl+v", "ctrl+d", "ctrl+n"}, last},
	} {
		for _, k := range tc.keys {
			vp.SetYOffset(tc.from)
			if !scrollKey(&vp, memKeyMsg(k)) {
				t.Errorf("%s was not taken as a motion", k)
			}
			if vp.YOffset != tc.want {
				t.Errorf("%s from %d scrolled to %d, want %d", k, tc.from, vp.YOffset, tc.want)
			}
		}
	}

	vp.SetYOffset(30)
	if scrollKey(&vp, memKeyMsg("x")) || vp.YOffset != 30 {
		t.Errorf("a key that is no motion moved the panel to %d", vp.YOffset)
	}
}
