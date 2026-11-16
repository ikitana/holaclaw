package memory

import "testing"

func TestTrivial(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"   ", true},
		{"???", true},
		{"👋", true},
		{"...!!", true},
		{"hi", true},
		{"Hey!", true},
		{"ok thanks", true},
		{"hola buenas", true},
		{"hm", true},
		{"42", true},
		{"yes please", false},
		{"what is my cat called?", false},
		{"deploy holaclaw", false},
		{"remind me about the invoice", false},
		{"gracias por todo", false},
	}
	for _, c := range cases {
		if got := Trivial(c.in); got != c.want {
			t.Errorf("Trivial(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
