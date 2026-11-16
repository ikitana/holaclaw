package page

import (
	"strings"
	"testing"
)

// The script is a constant that only looks: it activates nothing, types
// nothing and writes nothing back into the page.
func TestBotWallScriptOnlyFinds(t *testing.T) {
	for _, forbidden := range []string{"click(", "submit(", "innerHTML", "setAttribute", "eval(", ".value ="} {
		if strings.Contains(botWallJS, forbidden) {
			t.Errorf("the isolated-world script contains %q", forbidden)
		}
	}
	if !strings.Contains(botWallJS, "querySelector") {
		t.Error("the script does not read the page at all")
	}
}

// Nothing a page produced is ever evaluated: the expression is the constant
// script joined to this package's own table.
func TestBotWallExprIsConstant(t *testing.T) {
	expr, err := botWallExpr()
	if err != nil {
		t.Fatalf("botWallExpr: %v", err)
	}
	if !strings.HasPrefix(expr, botWallJS+"(") || !strings.HasSuffix(expr, ")") {
		t.Fatal("the expression is not the constant script applied to the table")
	}
}

// A page with no site is not worth a check.
func TestBotWallHost(t *testing.T) {
	for _, c := range []struct {
		raw  string
		host string
		ok   bool
	}{
		{"https://www.example.com/x", "www.example.com", true},
		{"http://127.0.0.1:8080/x", "127.0.0.1", true},
		{"about:blank", "", false},
		{"chrome-error://chromewebdata/", "", false},
		{"data:text/html,<p>", "", false},
		{"", "", false},
	} {
		host, ok := botWallHost(c.raw)
		if host != c.host || ok != c.ok {
			t.Errorf("botWallHost(%q) = %q, %v; want %q, %v", c.raw, host, ok, c.host, c.ok)
		}
	}
}

// An invisible reCAPTCHA guards a form without ever being shown; a page that
// merely carries one is not a wall, and the script says so by its address.
func TestBotWallSkipsInvisibleWidgets(t *testing.T) {
	if !strings.Contains(botWallJS, "size=invisible") {
		t.Error("the script does not skip invisible challenge frames")
	}
	if !strings.Contains(botWallJS, "getBoundingClientRect") {
		t.Error("the script does not check that the widget is laid out")
	}
}
