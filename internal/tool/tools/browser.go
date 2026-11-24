package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"holaclaw/internal/browser"
	"holaclaw/internal/browser/proto"
	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

// browser.do is the whole browser as one tool call (docs/browser.md): the
// agent states an intention once and gets a compact result back — never a
// snapshot. The class is `external` and the result is tainted, because
// everything in it was read off a web page (docs/security.md), and guests never reach it:
// every user of an agent shares that agent's logins.

const (
	// maxIntentBytes bounds the intention. A longer one is refused, never
	// silently cut: half an intention is a different task.
	maxIntentBytes = 2000
	// maxBrowserDownloads caps how many files one task may attach.
	maxBrowserDownloads = 10
	// maxBrowserResultBytes caps the rendered result.
	maxBrowserResultBytes = 8000
	// browserShotName and browserShotMIME are what a returned screenshot is
	// attached as: the sidecar takes JPEGs.
	browserShotName = "screenshot.jpg"
	browserShotMIME = "image/jpeg"
)

// ErrNoBrowser is what a BrowserBackend returns when no browser is configured
// for this agent. The tool turns it into a Result the model can act on.
var ErrNoBrowser = errors.New("tools: no browser configured")

// BrowserBackend is wired by the daemon: it owns the executor, the sidecar
// client and the agent's configuration. The narrow shape keeps this package
// free of both.
type BrowserBackend interface {
	// Run carries out one task. ErrNoBrowser when the agent has no browser.
	Run(ctx context.Context, t browser.Task) (browser.Result, error)
	// Download fetches one quarantined file from the sidecar; the caller
	// closes the reader.
	Download(ctx context.Context, agent string, d proto.Download) (io.ReadCloser, error)
	// Allow is the agent's navigation allow-list; empty means any site.
	Allow(agent string) []string
}

// RegisterBrowser adds browser.do. It is registered only when a browser is
// configured. sink may be nil; it receives one browser.download event per
// attached file, which never carries page text.
func RegisterBrowser(r *tool.Registry, b BrowserBackend, blobs BlobPutter, sink event.Sink) error {
	if b == nil {
		return errors.New("tools: RegisterBrowser needs a browser backend")
	}
	if blobs == nil {
		return errors.New("tools: RegisterBrowser needs a blob store")
	}
	if sink == nil {
		sink = event.Discard
	}
	return r.Register(&browserDo{backend: b, blobs: blobs, sink: sink})
}

// browserDo holds only read-only wiring, so calls run concurrently; the
// executor serialises the runs of one agent itself.
type browserDo struct {
	backend BrowserBackend
	blobs   BlobPutter
	sink    event.Sink
}

func (*browserDo) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name: "browser.do",
		Description: "Do one thing in a real browser: say the whole intention " +
			"(find X on Y and read the price). Returns what it read, images and charts included, " +
			"not the page.",
		Parameters: object{
			Properties: map[string]prop{
				"intent": {Type: tString},
				"url": {Type: tString, Description: "optional start: an entry page or image URL, " +
					"not an invented deep link (they render empty)"},
			},
			Required: []string{"intent"},
		}.raw(),
	}
}

func (*browserDo) Risk() tool.Risk { return tool.RiskExternal }

// Run never returns a Go error for anything the model can act on; a failure
// of the executor itself is an internal error and is returned as one.
func (t *browserDo) Run(ctx context.Context, c tool.Call) (tool.Result, error) {
	if c.Role == "guest" {
		return errf("browser.do: guests cannot use the browser"), nil
	}
	var a struct {
		Intent string `json:"intent"`
		URL    string `json:"url"`
	}
	if err := decodeArgs(c.Args, &a); err != nil {
		return errf("browser.do: arguments must be a JSON object: %v", err), nil
	}
	intent := strings.TrimSpace(a.Intent)
	switch {
	case intent == "":
		return errf("browser.do: intent is required: say what should be done"), nil
	case len(intent) > maxIntentBytes:
		return errf("browser.do: intent is %d bytes, cap is %d; shorten it",
			len(intent), maxIntentBytes), nil
	}
	start := strings.TrimSpace(a.URL)
	if start != "" && !httpURL(start) {
		return errf("browser.do: url must be an http or https address"), nil
	}

	res, err := t.backend.Run(ctx, browser.Task{
		Agent:     c.Agent,
		Intent:    intent,
		URL:       start,
		Allow:     t.backend.Allow(c.Agent),
		TraceID:   c.TraceID,
		SessionID: c.SessionID,
		UserID:    c.UserID,
		Role:      c.Role,
		Detached:  c.RunKind == tool.RunTask,
	})
	if err != nil {
		if errors.Is(err, ErrNoBrowser) {
			return errf("browser.do: no browser is configured"), nil
		}
		return tool.Result{}, fmt.Errorf("browser.do: %w", err)
	}

	downloads := t.attach(ctx, c, res.Downloads)
	atts := downloads
	shot, shown := t.screenshot(ctx, res.Screenshot)
	if shown {
		atts = append(atts, shot)
	}
	// The last line is always there: whether a screenshot of the page is
	// attached.
	text := render(res, downloads) + fmt.Sprintf("\nscreenshot: %t", shown)
	return tool.Result{
		Text:        capText(text, maxBrowserResultBytes),
		Tainted:     true,
		Attachments: atts,
	}, nil
}

// render is the compact result: an outcome line, the answer, where it
// stopped, and the files it brought back.
func render(res browser.Result, atts []event.Attachment) string {
	var b strings.Builder
	b.WriteString("outcome: " + string(res.Outcome) + "\n")
	if res.Answer != "" {
		b.WriteString("answer: " + res.Answer + "\n")
	}
	if res.Notes != "" {
		b.WriteString("notes: " + res.Notes + "\n")
	}
	// Where the browser actually ended up, done or failed alike: the title
	// alongside the URL, in the one place a person or the chat model reading
	// the result is told about it (M11).
	if res.URL != "" {
		b.WriteString("url: " + res.URL + "\n")
		if res.Title != "" {
			b.WriteString("title: " + res.Title + "\n")
		}
	}
	if len(atts) > 0 {
		names := make([]string, 0, len(atts))
		for _, a := range atts {
			names = append(names, a.Name)
		}
		b.WriteString("downloads attached: " + strings.Join(names, ", ") + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// screenshot stores the picture a run returned as a blob, next to its
// downloads. One that is over the cap or cannot be stored is dropped, like a
// download: the task itself succeeded.
func (t *browserDo) screenshot(ctx context.Context, shot []byte) (event.Attachment, bool) {
	if len(shot) == 0 || len(shot) > maxAttachBytes {
		return event.Attachment{}, false
	}
	id, size, err := t.blobs.PutBlob(ctx, bytes.NewReader(shot), browserShotMIME)
	if err != nil {
		return event.Attachment{}, false
	}
	return event.Attachment{
		ID:     event.NewID(),
		BlobID: id,
		SHA256: id, // blob ids are content addresses: hex sha256
		MIME:   browserShotMIME,
		Name:   browserShotName,
		Size:   size,
		Kind:   event.KindFromMIME(browserShotMIME),
	}, true
}

// attach moves the quarantined downloads into blobs. A file that cannot be
// fetched or stored is dropped: the task itself succeeded, and a browser that
// saved something unreadable must not fail the turn.
func (t *browserDo) attach(ctx context.Context, c tool.Call, ds []proto.Download) []event.Attachment {
	var out []event.Attachment
	for i, d := range ds {
		if i >= maxBrowserDownloads || d.Size > maxAttachBytes {
			continue
		}
		att, err := t.store(ctx, c.Agent, d)
		if err != nil {
			continue
		}
		t.sink.Emit(ctx, event.New(event.BrowserDownload, c.TraceID, c.SessionID, "",
			map[string]any{"agent": c.Agent, "name": att.Name, "size": att.Size}))
		out = append(out, att)
	}
	return out
}

func (t *browserDo) store(ctx context.Context, agent string, d proto.Download) (event.Attachment, error) {
	rc, err := t.backend.Download(ctx, agent, d)
	if err != nil {
		return event.Attachment{}, err
	}
	defer rc.Close() //nolint:errcheck // read-only
	name := safeName(d.Name)
	mt := detectMIME(name, nil)
	id, size, err := t.blobs.PutBlob(ctx, io.LimitReader(rc, maxAttachBytes), mt)
	if err != nil {
		return event.Attachment{}, err
	}
	return event.Attachment{
		ID:     event.NewID(),
		BlobID: id,
		SHA256: id, // blob ids are content addresses: hex sha256
		MIME:   mt,
		Name:   name,
		Size:   size,
		Kind:   event.KindFromMIME(mt),
	}, nil
}

// httpURL is the only scheme check here; the allow-list is enforced in the
// sidecar, on every navigation and redirect.
func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
