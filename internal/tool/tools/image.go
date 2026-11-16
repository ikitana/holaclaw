package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

// image.generate is the second way a file leaves an agent (docs/sessions.md):
// the bytes are stored as a blob and attached to the reply, never written to
// the workspace. The class is `external` — the prompt leaves the machine and
// every call costs money — and a daily cap bounds the spend (docs/security.md).

const (
	// maxImagePromptBytes matches what the images transport accepts. A longer
	// prompt is refused, never silently truncated.
	maxImagePromptBytes = 4000
	// defaultImageStem names the attachment when the model gives no name.
	defaultImageStem = "image"
)

// ErrNoImageModel is what an ImageBackend returns when the agent has no
// `image` role. The tool turns it into a Result the model can act on.
var ErrNoImageModel = errors.New("tools: no image model configured")

// ImageBackend is wired by the daemon: it resolves the agent's `image` role
// and owns the daily counter. The narrow shape keeps this package free of the
// model registry and the store.
type ImageBackend interface {
	// GenerateImage runs the agent's image model, returning the image and the
	// model name it was made with. ErrNoImageModel when the agent has no
	// `image` role; upstream failures are *provider.Error values, with a
	// response holding only the usage when the model answered.
	GenerateImage(ctx context.Context, agent, prompt string) (*provider.ImageResponse, string, error)
	// TakeImage counts one generation against the agent's daily cap. ok is
	// false when the cap is reached; limit is the cap, for the message.
	TakeImage(ctx context.Context, agent string) (ok bool, limit int, err error)
	// ReturnImage gives a counted use back after a generation that failed.
	ReturnImage(ctx context.Context, agent string)
}

// RegisterImage adds image.generate. It is registered only when the agent set
// has an `image` role. sink may be nil; it receives the image.generated trace
// event, which never carries the prompt.
func RegisterImage(r *tool.Registry, b ImageBackend, blobs BlobPutter, sink event.Sink) error {
	if b == nil {
		return errors.New("tools: RegisterImage needs an image backend")
	}
	if blobs == nil {
		return errors.New("tools: RegisterImage needs a blob store")
	}
	if sink == nil {
		sink = event.Discard
	}
	return r.Register(&imageGenerate{backend: b, blobs: blobs, sink: sink})
}

// imageGenerate holds only read-only wiring, so calls run concurrently.
type imageGenerate struct {
	backend ImageBackend
	blobs   BlobPutter
	sink    event.Sink
}

func (*imageGenerate) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name: "image.generate",
		Description: "Create an image from a text description and attach it to your reply. " +
			"Describe subject, style and composition in one prompt.",
		Parameters: object{
			Properties: map[string]prop{"prompt": {Type: tString}, "name": {Type: tString}},
			Required:   []string{"prompt"},
		}.raw(),
	}
}

func (*imageGenerate) Risk() tool.Risk { return tool.RiskExternal }

// Run never returns a Go error: bad input and upstream failures are Results
// (docs/context.md). The Result is not Tainted — nothing readable came back, only pixels
// the agent itself asked for.
func (t *imageGenerate) Run(ctx context.Context, c tool.Call) (tool.Result, error) {
	var a struct {
		Prompt string `json:"prompt"`
		Name   string `json:"name"`
	}
	if err := decodeArgs(c.Args, &a); err != nil {
		return errf("image.generate: arguments must be a JSON object: %v", err), nil
	}
	prompt := strings.TrimSpace(a.Prompt)
	switch {
	case prompt == "":
		return errf("image.generate: prompt is required"), nil
	case len(prompt) > maxImagePromptBytes:
		return errf("image.generate: prompt is %d bytes, cap is %d; shorten it",
			len(prompt), maxImagePromptBytes), nil
	}

	// The cap is taken before anything is spent, so a refusal costs nothing.
	ok, limit, err := t.backend.TakeImage(ctx, c.Agent)
	if err != nil {
		return errf("image.generate: the daily image limit could not be checked"), nil
	}
	if !ok {
		return errf("image.generate: daily limit of %d images reached; try again tomorrow", limit), nil
	}

	start := time.Now()
	resp, model, err := t.backend.GenerateImage(ctx, c.Agent, prompt)
	took := time.Since(start)
	var usage event.Usage // zero without a reply; a reply is paid for, usable or not
	if resp != nil {
		usage = event.Call(resp.Usage)
	}
	if err != nil {
		if refundable(err) {
			t.backend.ReturnImage(ctx, c.Agent)
		}
		return t.failed(ctx, c, model, len(prompt), took, usage, err), nil
	}

	att, size, err := t.store(ctx, resp, a.Name)
	if err != nil {
		// The use stays counted: the money was spent upstream whatever the
		// blob store did. Returning it here would let a broken store fund
		// unbounded generations.
		return t.failed(ctx, c, model, len(prompt), took, usage,
			fmt.Errorf("storing the image failed: %w", err)), nil
	}

	attrs := map[string]any{
		"model":        model,
		"mime":         att.MIME,
		"bytes":        size,
		"prompt_bytes": len(prompt),
	}
	// image.generated counts towards a trace's total (event.UsageTypes).
	event.PutUsage(attrs, usage)
	t.emit(ctx, c, took, attrs)
	return tool.Result{
		Text: fmt.Sprintf("generated %s (%s, %d bytes); it is attached to your reply",
			att.Name, att.MIME, size),
		Attachments: []event.Attachment{att},
	}, nil
}

// store puts the bytes in the blob store and builds the reply attachment.
func (t *imageGenerate) store(ctx context.Context, resp *provider.ImageResponse, name string) (event.Attachment, int64, error) {
	if resp == nil || len(resp.Data) == 0 {
		return event.Attachment{}, 0, errors.New("empty image")
	}
	mt := strings.TrimSpace(resp.Mime)
	if mt == "" {
		mt = "application/octet-stream"
	}
	id, size, err := t.blobs.PutBlob(ctx, bytes.NewReader(resp.Data), mt)
	if err != nil {
		return event.Attachment{}, 0, err
	}
	return event.Attachment{
		ID:     event.NewID(),
		BlobID: id,
		SHA256: id, // blob ids are content addresses: hex sha256
		MIME:   mt,
		Name:   imageName(name, mt),
		Size:   size,
		Kind:   event.KindFromMIME(mt),
	}, size, nil
}

// failed records the failure and renders it for the model. The message is a
// fixed sentence plus the error *kind*: an upstream message may echo the
// prompt or internals and never reaches the model.
func (t *imageGenerate) failed(ctx context.Context, c tool.Call, model string, promptBytes int, took time.Duration, usage event.Usage, err error) tool.Result {
	if errors.Is(err, ErrNoImageModel) {
		t.emit(ctx, c, took, map[string]any{"error": "no_model", "prompt_bytes": promptBytes})
		return errf("image.generate: no image model is configured")
	}
	kind := string(provider.KindOf(err))
	if kind == "" {
		kind = "failed"
	}
	attrs := map[string]any{"error": kind, "prompt_bytes": promptBytes}
	if model != "" {
		attrs["model"] = model
	}
	// When the model answered, the call was paid for even though nothing
	// usable came of it.
	event.PutUsage(attrs, usage)
	t.emit(ctx, c, took, attrs)
	return errf("image.generate: the image service failed (%s)", kind)
}

// emit sends one image.generated event, stamped with the generation time.
func (t *imageGenerate) emit(ctx context.Context, c tool.Call, took time.Duration, attrs map[string]any) {
	e := event.New(event.ImageGenerated, c.TraceID, c.SessionID, "", attrs)
	e.Duration = took
	t.sink.Emit(ctx, e)
}

// imageName builds the attachment name: a sanitised stem from the model plus
// the extension the mime type implies. A stem that carries an image extension
// already loses it, so "cat.png" with a WebP answer is "cat.webp". A name made
// only of separators, dots or control characters falls back to the default.
func imageName(raw, mt string) string {
	ext := imageExt(mt)
	if !usableStem(raw) {
		return defaultImageStem + ext
	}
	// Windows separators are separators here too: safeName only drops them.
	stem := safeName(strings.ReplaceAll(raw, `\`, "/"))
	if e := filepath.Ext(stem); e != "" && isImageExt(e) {
		stem = strings.TrimSuffix(stem, e)
	}
	stem = strings.Trim(stem, ". ")
	if stem == "" {
		stem = defaultImageStem
	}
	return stem + ext
}

// usableStem reports whether raw holds anything that can become a file name.
func usableStem(raw string) bool {
	for _, r := range raw {
		switch {
		case unicode.IsControl(r), unicode.IsSpace(r):
		case r == '.' || r == '/' || r == '\\':
		default:
			return true
		}
	}
	return false
}

// imageExt is the extension for one of the three mime types the transport
// produces; anything else keeps a neutral one.
func imageExt(mt string) string {
	base, _, err := mime.ParseMediaType(mt)
	if err != nil {
		base = strings.ToLower(strings.TrimSpace(mt))
	}
	switch base {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	}
	return ".img"
}

func isImageExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".img":
		return true
	}
	return false
}

// refundable reports whether a failed generation provably cost nothing, so
// its use may go back to the daily cap. Anything that can happen after the
// upstream rendered — a timeout, a dropped connection, an answer we refuse
// (unsupported type, too large, malformed) — stays counted: otherwise a model
// that always answers AVIF would be billed on every retry while the cap never
// moved.
func refundable(err error) bool {
	if errors.Is(err, ErrNoImageModel) {
		return true
	}
	switch provider.KindOf(err) {
	case provider.ErrAuth, provider.ErrRateLimit, provider.ErrUsageLimit, provider.ErrBadRequest:
		return true
	}
	return false
}
