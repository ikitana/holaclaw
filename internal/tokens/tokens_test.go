package tokens

import "testing"

func TestEstimate(t *testing.T) {
	cases := map[string]int{
		"":                 0,
		"hello":            2,
		"hello world":      4,
		"a, b":             3,
		"{\"k\": 1}":       7,
		"line\nline":       3,
		"supercalifragili": 4,
	}
	for in, want := range cases {
		if got := Estimate(in); got != want {
			t.Errorf("Estimate(%q) = %d, want %d", in, got, want)
		}
	}
}
