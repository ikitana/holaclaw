package oauth

import "strings"

// challenge is one auth challenge of a WWW-Authenticate header (RFC 9110
// §11.6.1). Scheme and parameter names are lower case.
type challenge struct {
	scheme  string
	token68 string
	params  map[string]string
}

// parseChallenges reads every challenge of a header value: several schemes,
// quoted strings with escapes, token68. Parsing stops at the first thing it
// cannot read and keeps what came before; a repeated parameter keeps its
// first value.
func parseChallenges(h string) []challenge {
	l := lexer{s: h}
	var out []challenge
	for {
		l.skip(" \t,")
		scheme := l.token()
		if scheme == "" {
			return out
		}
		c := challenge{scheme: strings.ToLower(scheme), params: map[string]string{}}
		if l.skip(" \t") && !l.done() && l.peek() != ',' {
			if t, ok := l.token68(); ok {
				c.token68 = t
			} else {
				l.params(c.params)
			}
		}
		out = append(out, c)
	}
}

// bearerParam is a parameter of the first Bearer challenge.
func bearerParam(h, name string) string {
	for _, c := range parseChallenges(h) {
		if c.scheme == "bearer" {
			return c.params[name]
		}
	}
	return ""
}

type lexer struct {
	s string
	i int
}

func (l *lexer) done() bool { return l.i >= len(l.s) }
func (l *lexer) peek() byte { return l.s[l.i] }

// skip advances over any of chars and reports whether it moved.
func (l *lexer) skip(chars string) bool {
	start := l.i
	for !l.done() && strings.IndexByte(chars, l.peek()) >= 0 {
		l.i++
	}
	return l.i > start
}

func (l *lexer) token() string {
	start := l.i
	for !l.done() && isTchar(l.peek()) {
		l.i++
	}
	return l.s[start:l.i]
}

// token68 reads a token68 when it is the whole credential: followed only by
// space, a comma or the end. Otherwise nothing is consumed.
func (l *lexer) token68() (string, bool) {
	start := l.i
	j := l.i
	for j < len(l.s) && isToken68(l.s[j]) {
		j++
	}
	if j == start {
		return "", false
	}
	for j < len(l.s) && l.s[j] == '=' {
		j++
	}
	k := j
	for k < len(l.s) && (l.s[k] == ' ' || l.s[k] == '\t') {
		k++
	}
	if k < len(l.s) && l.s[k] != ',' {
		return "", false
	}
	l.i = j
	return l.s[start:j], true
}

// params reads name=value pairs separated by commas until something that is
// not one, such as the next scheme, and leaves the lexer before it.
func (l *lexer) params(into map[string]string) {
	for {
		start := l.i
		l.skip(" \t")
		name := l.token()
		l.skip(" \t")
		if name == "" || l.done() || l.peek() != '=' {
			l.i = start
			return
		}
		l.i++
		l.skip(" \t")
		var value string
		if !l.done() && l.peek() == '"' {
			value = l.quoted()
		} else {
			value = l.token()
		}
		key := strings.ToLower(name)
		if _, seen := into[key]; !seen {
			into[key] = value
		}
		l.skip(" \t")
		if l.done() || l.peek() != ',' {
			return
		}
		l.i++
	}
}

// quoted reads a quoted-string; an unterminated one runs to the end.
func (l *lexer) quoted() string {
	l.i++ // opening quote
	var b strings.Builder
	for !l.done() {
		ch := l.peek()
		l.i++
		switch {
		case ch == '"':
			return b.String()
		case ch == '\\' && !l.done():
			b.WriteByte(l.peek())
			l.i++
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

func isTchar(c byte) bool {
	return isAlnum(c) || strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

func isToken68(c byte) bool {
	return isAlnum(c) || strings.IndexByte("-._~+/", c) >= 0
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}
