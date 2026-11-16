package tools

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"unicode"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

// maxAttachBytes is the cap on one attached file.
const maxAttachBytes = 20 << 20

// builtinMIME overrides the OS mime database for extensions whose system
// registration is missing or inconsistent across platforms (a bare Linux
// image, for instance, has no entry for ".md" at all).
var builtinMIME = map[string]string{
	".md":       "text/markdown",
	".markdown": "text/markdown",
	".csv":      "text/csv",
}

// attachTool sends a workspace file to the user: the one mechanism by which
// files leave an agent (docs/sessions.md).
type attachTool struct {
	blobs BlobPutter
}

func (*attachTool) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "attach",
		Description: "Attach a workspace file to your reply: images, audio, documents. Paths are relative. This is the only way to send a file.",
		Parameters: object{
			Properties: map[string]prop{"path": {Type: tString}},
			Required:   []string{"path"},
		}.raw(),
	}
}

func (*attachTool) Risk() tool.Risk { return tool.RiskRead }

func (t *attachTool) Run(ctx context.Context, c tool.Call) (tool.Result, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := decodeArgs(c.Args, &a); err != nil {
		return errf("bad arguments: %v", err), nil
	}
	p, err := relPath(a.Path)
	if err != nil {
		return errf("%v", err), nil
	}
	if t.blobs == nil {
		return errf("attach: no blob store configured"), nil
	}
	root, err := openWorkspace(c)
	if err != nil {
		return errf("%v", err), nil
	}
	defer root.Close() //nolint:errcheck // read-only

	f, err := root.Open(p)
	if err != nil {
		return jailErr("attach", a.Path, err), nil
	}
	defer f.Close() //nolint:errcheck // read-only
	st, err := f.Stat()
	if err != nil {
		return errf("attach: %s: %v", a.Path, err), nil
	}
	if st.IsDir() {
		return errf("attach: %s is a directory", a.Path), nil
	}
	if st.Size() > maxAttachBytes {
		return errf("attach: %s is %d bytes, cap is %d", a.Path, st.Size(), maxAttachBytes), nil
	}

	head := make([]byte, 512)
	n, _ := f.Read(head) //nolint:errcheck // a short or failed read just means no sniff
	if _, err := f.Seek(0, 0); err != nil {
		return errf("attach: %s: %v", a.Path, err), nil
	}
	mt := detectMIME(p, head[:n])

	id, size, err := t.blobs.PutBlob(ctx, f, mt)
	if err != nil {
		return errf("attach: storing %s failed: %v", a.Path, err), nil
	}
	name := safeName(p)
	att := event.Attachment{
		ID:     event.NewID(),
		BlobID: id,
		SHA256: id, // blob ids are content addresses: hex sha256
		MIME:   mt,
		Name:   name,
		Size:   size,
		Kind:   event.KindFromMIME(mt),
	}
	return tool.Result{
		Text:        fmt.Sprintf("attached %s (%s, %d bytes)", name, mt, size),
		Attachments: []event.Attachment{att},
	}, nil
}

// detectMIME prefers the extension (it knows text/markdown, text/csv) and
// falls back to content sniffing.
func detectMIME(p string, head []byte) string {
	if ext := filepath.Ext(p); ext != "" {
		if mt, ok := builtinMIME[strings.ToLower(ext)]; ok {
			return mt
		}
		if mt := mime.TypeByExtension(ext); mt != "" {
			if parsed, _, err := mime.ParseMediaType(mt); err == nil {
				// Keep audio parameters (codecs=opus decides "voice note").
				if strings.HasPrefix(parsed, "audio/") {
					return mt
				}
				return parsed
			}
			return mt
		}
	}
	if len(head) == 0 {
		return "application/octet-stream"
	}
	mt, _, err := mime.ParseMediaType(http.DetectContentType(head))
	if err != nil {
		return "application/octet-stream"
	}
	return mt
}

// safeName reduces a path to a plain base file name: no directories, no
// control characters, bounded length.
func safeName(p string) string {
	base := filepath.Base(p)
	base = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return -1
		}
		return r
	}, base)
	base = strings.TrimSpace(strings.TrimLeft(base, "."))
	if len(base) > 120 {
		base = base[:120]
	}
	if base == "" {
		return "file"
	}
	return base
}
