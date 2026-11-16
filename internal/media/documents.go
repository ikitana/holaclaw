package media

import (
	"bytes"
	"context"
	"io"
	"mime"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"holaclaw/internal/event"
)

// Document inlining caps (docs/sessions.md: "text-like documents inlined with a
// cap"). Anything bigger stays behind the `files` tool.
const (
	// InlineFileRunes is the cap for one document.
	InlineFileRunes = 6000
	// InlineTurnRunes is the cap for all documents of one turn together.
	InlineTurnRunes = 12000
	// SniffBytes is how much of a blob decides whether it is text.
	SniffBytes = 8 << 10
	// InlineTruncMarker closes content that had to be cut.
	InlineTruncMarker = "[…truncated, use files/attach tools for the rest]"
	// maxNameRunes caps the file name shown to the model: it is metadata.
	maxNameRunes = 64
)

// textMIMEs are the mime types we inline outright.
var textMIMEs = map[string]bool{
	"application/json":   true,
	"application/xml":    true,
	"application/x-yaml": true,
	"application/toml":   true,
	"application/csv":    true,
}

// textExts are the extensions that qualify a document when its mime type does
// not. The content still has to pass the sniff.
var textExts = map[string]bool{
	".md": true, ".txt": true, ".csv": true, ".json": true, ".yaml": true,
	".yml": true, ".toml": true, ".log": true, ".go": true, ".py": true,
	".js": true, ".ts": true, ".sh": true, ".html": true, ".css": true, ".sql": true,
}

// InlineDocuments returns one injection per text-like document attachment of
// the pending user messages: `file <name>:` followed by the content, capped
// and wrapped as untrusted. Documents that are not text-like yield nothing —
// the builder's stub line already names them and the `files` tool can read
// them. Reads are sequential: they are cheap next to a model call and the
// result must be stable.
func (n *Normaliser) InlineDocuments(ctx context.Context, msgs []event.Message) []string {
	if ctx.Err() != nil || n.d.Blobs == nil {
		return nil
	}
	var out []string
	budget := InlineTurnRunes
	for mi := range msgs {
		if msgs[mi].Role != event.RoleUser {
			continue
		}
		for _, a := range msgs[mi].Attachments {
			if budget <= 0 {
				return out
			}
			if ctx.Err() != nil {
				return out
			}
			if a.Kind != event.KindDocument || !textLike(a) {
				continue
			}
			text, ok := n.readText(ctx, a)
			if !ok {
				continue
			}
			text, used := capInline(text, budget)
			if used == 0 {
				continue
			}
			budget -= used
			out = append(out, "file "+safeName(a.Name)+":\n"+wrapUntrusted(text))
		}
	}
	return out
}

// textLike reports whether the mime type or the extension claims text. The
// claim is only a filter; the content still has to sniff as text.
func textLike(a event.Attachment) bool {
	mt, _, err := mime.ParseMediaType(a.MIME)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(a.MIME))
	}
	if strings.HasPrefix(mt, "text/") || textMIMEs[mt] {
		return true
	}
	return textExts[strings.ToLower(path.Ext(a.Name))]
}

// readText loads at most one file's worth of a blob and reports whether it
// sniffs as text: valid UTF-8 and no NUL in the first 8 KiB.
func (n *Normaliser) readText(ctx context.Context, a event.Attachment) (string, bool) {
	rc, _, err := n.d.Blobs(ctx, a.BlobID)
	if err != nil {
		return "", false
	}
	defer func() { _ = rc.Close() }()
	// 4 bytes per rune is the widest UTF-8 encoding, plus one byte to notice
	// that there is more.
	b, err := io.ReadAll(io.LimitReader(rc, InlineFileRunes*4+1))
	if err != nil || len(b) == 0 {
		return "", false
	}
	if !sniffText(b) {
		return "", false
	}
	return string(bytes.ToValidUTF8(b, nil)), true
}

// sniffText decides on the first 8 KiB: no NUL byte, valid UTF-8. A window cut
// mid-rune is not a failure, so up to three trailing bytes are dropped first.
func sniffText(b []byte) bool {
	truncated := len(b) > SniffBytes
	if truncated {
		b = b[:SniffBytes]
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	if truncated {
		for i := 0; i < 3 && len(b) > 0 && !utf8.Valid(b); i++ {
			b = b[:len(b)-1]
		}
	}
	return utf8.Valid(b)
}

// capInline cuts text to the per-file cap and to what is left of the turn
// budget, marking it when anything was dropped. It returns the text and the
// runes it consumed from the budget.
func capInline(text string, budget int) (string, int) {
	limit := InlineFileRunes
	if budget < limit {
		limit = budget
	}
	r := []rune(text)
	if len(r) <= limit {
		return text, len(r)
	}
	cut := strings.TrimRight(string(r[:limit]), " \t\n")
	return cut + "\n" + InlineTruncMarker, limit
}

// untrustedTag matches a literal <untrusted> or </untrusted> tag in any case.
// The wrapping convention is the builder's (internal/agent/sanitize.go); it is
// replicated here because that helper is unexported.
var untrustedTag = regexp.MustCompile(`(?i)</?untrusted\s*>`)

// neutralizeTaintTags escapes literal untrusted tags inside content so a file
// cannot close the delimiter that marks it as untrusted (docs/security.md).
func neutralizeTaintTags(s string) string {
	return untrustedTag.ReplaceAllStringFunc(s, func(m string) string {
		return "&lt;" + strings.Trim(m, "<>") + "&gt;"
	})
}

// wrapUntrusted delimits content that came from an untrusted source.
func wrapUntrusted(s string) string {
	return "<untrusted>\n" + neutralizeTaintTags(s) + "\n</untrusted>"
}

// safeName renders an attachment name as prompt metadata: no control runes, no
// brackets or backticks, whitespace collapsed, capped.
func safeName(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = true
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			continue
		case strings.ContainsRune("[]<>#`", r):
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := []rune(strings.TrimSpace(b.String()))
	if len(out) > maxNameRunes {
		out = out[:maxNameRunes]
	}
	if res := strings.TrimSpace(string(out)); res != "" {
		return res
	}
	return "file"
}
