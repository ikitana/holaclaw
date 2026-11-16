package telegram

import (
	"fmt"
	"strings"
	"testing"
)

func TestMarkdownToHTML(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"bold stars", "**bold**", "<b>bold</b>"},
		{"bold underscores", "__bold__", "<b>bold</b>"},
		{"italic stars", "*italic*", "<i>italic</i>"},
		{"italic underscores", "_italic_", "<i>italic</i>"},
		{"code span", "`x = 1`", "<code>x = 1</code>"},
		{"strike", "~~gone~~", "<s>gone</s>"},
		{"escapes", "a < b & c > d", "a &lt; b &amp; c &gt; d"},
		{"link", "[text](https://x.test/a)", `<a href="https://x.test/a">text</a>`},
		{"non http link stays plain", "[text](ftp://x.test)", "[text](ftp://x.test)"},
		{"heading", "## Title", "<b>Title</b>"},
		{"bullets", "- one\n- two", "• one\n• two"},
		{"ordered list", "1. one", "• one"},
		{"blockquote", "> quoted\n> more", "<blockquote>quoted\nmore</blockquote>"},
		{"fenced code", "```go\nif a < b {\n}\n```", "<pre><code class=\"language-go\">if a &lt; b {\n}</code></pre>"},
		{"fenced code no lang", "```\nplain\n```", "<pre><code>plain</code></pre>"},
		{"unterminated bold stays literal", "**unbalanced", "**unbalanced"},
		{"unterminated fence stays literal", "```go\nnope", "```go\nnope"},
		{"snake case is not italic", "snake_case_name", "snake_case_name"},
		{"spaced star is not italic", "a * b * c", "a * b * c"},
		{"code escapes html", "`<b>`", "<code>&lt;b&gt;</code>"},
		{"nested emphasis", "**bold with `code`**", "<b>bold with <code>code</code></b>"},
		{"link text formatted", "[**x**](https://a.test)", `<a href="https://a.test"><b>x</b></a>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mdToHTML(c.in)
			if got != c.want {
				t.Fatalf("mdToHTML(%q) = %q, want %q", c.in, got, c.want)
			}
			if err := validateTags(got); err != nil {
				t.Fatalf("output not balanced: %v", err)
			}
		})
	}
}

// TestHTMLBalanceProperty feeds the converter input designed to break it: the
// output must always be balanced, well-formed HTML.
func TestHTMLBalanceProperty(t *testing.T) {
	nasty := []string{
		"", " ", "\n\n\n",
		"<script>alert(1)</script>",
		"**a*b**c*",
		"`````",
		"~~~~~~",
		"***bold italic***",
		"_a_b_c_",
		"# <b>evil</b>",
		"&amp; &lt; &#x41;",
		strings.Repeat("*", 200),
		strings.Repeat("_", 200),
		strings.Repeat("`", 99),
		strings.Repeat("[a](", 40),
		"[a](https://x.test/\"onmouseover=\"y)",
		"[a](javascript:alert(1))",
		"```\nunterminated <b>",
		"> **quote** with `code` and <tags>",
		"- **a\n- b**",
		"**" + strings.Repeat("a*b_c~d`e", 50) + "**",
		"text with emoji 😀 and **bold 😀**",
		"a\x00b",
		"[](https://x.test)",
		"[x]()",
		"**[link](https://x.test)**",
		strings.Repeat("**x**", 500),
		"```go\n" + strings.Repeat("<&>", 100) + "\n```",
	}
	for i, in := range nasty {
		out := mdToHTML(in)
		if err := validateTags(out); err != nil {
			t.Errorf("case %d %q -> %q: %v", i, in, out, err)
		}
	}
}

func TestHTMLToPlain(t *testing.T) {
	in := `<b>a</b> &lt;x&gt; <a href="https://x.test">link</a> &amp; <code>c</code>`
	want := "a <x> link & c"
	if got := htmlToPlain(in); got != want {
		t.Fatalf("htmlToPlain = %q, want %q", got, want)
	}
}

func TestIsHTTPURL(t *testing.T) {
	ok := []string{"https://a.test", "http://a.test/b?c=d"}
	bad := []string{"ftp://a.test", "javascript:alert(1)", "https://a.test/ x", "", "//a.test"}
	for _, u := range ok {
		if !isHTTPURL(u) {
			t.Errorf("isHTTPURL(%q) = false", u)
		}
	}
	for _, u := range bad {
		if isHTTPURL(u) {
			t.Errorf("isHTTPURL(%q) = true", u)
		}
	}
}

// allowedTags is the Telegram HTML subset this adapter may emit.
var allowedTags = map[string]bool{
	"b": true, "i": true, "s": true, "u": true, "code": true, "pre": true,
	"a": true, "blockquote": true,
}

// validateTags is the tiny validator the balance property test asserts with:
// every tag is known, every tag is closed in order, and every '&' starts a
// well-formed entity.
func validateTags(s string) error {
	var stack []string
	for i := 0; i < len(s); {
		switch s[i] {
		case '<':
			j := strings.IndexByte(s[i:], '>')
			if j < 0 {
				return fmt.Errorf("unterminated tag at %d", i)
			}
			tok := s[i : i+j+1]
			name := tagName(tok)
			if !allowedTags[name] {
				return fmt.Errorf("disallowed tag %q", tok)
			}
			if strings.HasPrefix(tok, "</") {
				if len(stack) == 0 || stack[len(stack)-1] != name {
					return fmt.Errorf("unbalanced close %q", tok)
				}
				stack = stack[:len(stack)-1]
			} else {
				if err := validateAttrs(name, tok); err != nil {
					return err
				}
				stack = append(stack, name)
			}
			i += j + 1
			continue
		case '&':
			if _, n, ok := entityAt(s[i:]); ok {
				i += n
				continue
			}
			return fmt.Errorf("bare & at %d", i)
		}
		i++
	}
	if len(stack) != 0 {
		return fmt.Errorf("unclosed tags %v", stack)
	}
	return nil
}

// validateAttrs allows only href on <a> and class on <code>, quoted and clean.
func validateAttrs(name, tok string) error {
	body := strings.TrimSuffix(strings.TrimPrefix(tok, "<"), ">")
	rest := strings.TrimSpace(strings.TrimPrefix(body, name))
	if rest == "" {
		return nil
	}
	switch {
	case name == "a" && strings.HasPrefix(rest, `href="`) && strings.HasSuffix(rest, `"`):
		v := rest[len(`href="`) : len(rest)-1]
		if strings.ContainsAny(v, `"<>`) {
			return fmt.Errorf("unescaped attribute value %q", v)
		}
		return nil
	case name == "code" && strings.HasPrefix(rest, `class="language-`) && strings.HasSuffix(rest, `"`):
		v := rest[len(`class="`) : len(rest)-1]
		if strings.ContainsAny(v, `"<>&`) {
			return fmt.Errorf("unescaped class %q", v)
		}
		return nil
	}
	return fmt.Errorf("unexpected attributes on <%s>: %q", name, rest)
}
