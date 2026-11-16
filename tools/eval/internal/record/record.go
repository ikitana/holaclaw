// Package record gives the eval harness deterministic model calls: a
// provider.Provider wrapper that either records every call of a live model or
// replays a recorded run. Recordings hold requests digests and responses only
// — never headers, keys or URLs (tools/eval/README.md).
package record

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"holaclaw/internal/provider"
)

// Mode selects where model responses come from.
type Mode string

const (
	ModeLive   Mode = "live"   // call the model, record nothing
	ModeRecord Mode = "record" // call the model and write a recording
	ModeReplay Mode = "replay" // serve a recording, never touch the network
)

// ParseMode validates a --mode flag value.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeLive, ModeRecord, ModeReplay:
		return Mode(s), nil
	default:
		return "", fmt.Errorf("eval: unknown mode %q (live|record|replay)", s)
	}
}

// Run is the recording of one (model × scenario × run) triple.
type Run struct {
	Model    string `json:"model"`
	Scenario string `json:"scenario"`
	Run      int    `json:"run"`
	Steps    []Step `json:"steps"`
}

// Step is one model call: the request digest plus what came back.
type Step struct {
	Step     int       `json:"step"`
	Digest   string    `json:"digest"`
	Shape    Shape     `json:"shape"` // human hint for digest mismatches
	Response *Response `json:"response,omitempty"`
	// ErrorKind is set instead of Response when the call failed.
	ErrorKind string `json:"error_kind,omitempty"`
}

// Shape is the coarse form of a request, enough to point at what changed.
type Shape struct {
	Messages    int      `json:"messages"`
	Tools       []string `json:"tools"`
	SystemChars int      `json:"system_chars"`
	LastMessage string   `json:"last_message"` // tail of the last message, truncated
}

// Response is the recorded final provider response.
type Response struct {
	Text      string                `json:"text,omitempty"`
	ToolCalls []provider.ToolCall   `json:"tool_calls,omitempty"`
	Finish    provider.FinishReason `json:"finish"`
	Usage     provider.Usage        `json:"usage"`
}

// maxShapeText caps the request tail kept for diff hints.
const maxShapeText = 160

// Provider records or replays the model calls of one run.
type Provider struct {
	mode  Mode
	inner provider.Provider
	path  string
	run   Run
	n     int // calls served so far
}

var _ provider.Provider = (*Provider)(nil)

// Open builds the provider for one run. In replay mode inner may be nil and
// path must name an existing recording; in record mode path is where the
// recording is written by Save.
func Open(mode Mode, inner provider.Provider, path string, meta Run) (*Provider, error) {
	p := &Provider{mode: mode, inner: inner, path: path, run: meta}
	switch mode {
	case ModeReplay:
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("eval: no recording (run with --mode record): %w", err)
		}
		var r Run
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, fmt.Errorf("eval: parse recording %s: %w", path, err)
		}
		p.run.Steps = r.Steps
	default:
		if inner == nil {
			return nil, fmt.Errorf("eval: %s mode needs a live provider", mode)
		}
		p.run.Steps = nil
	}
	return p, nil
}

// Name reports the provider instance name.
func (p *Provider) Name() string {
	if p.inner != nil {
		return p.inner.Name()
	}
	return "replay"
}

// Chat serves one model call from the recording, or makes it and records it.
func (p *Provider) Chat(ctx context.Context, req provider.Request) (provider.Stream, error) {
	digest, shape := Digest(req)
	p.n++
	if p.mode == ModeReplay {
		return p.replay(digest, shape)
	}
	stream, err := p.inner.Chat(ctx, req)
	if err != nil {
		p.record(Step{Step: p.n, Digest: digest, Shape: shape, ErrorKind: string(provider.KindOf(err))})
		return nil, err
	}
	if p.mode == ModeLive {
		return stream, nil
	}
	return &recStream{p: p, inner: stream, step: Step{Step: p.n, Digest: digest, Shape: shape}}, nil
}

// replay serves the step-th recorded response, refusing a changed request.
func (p *Provider) replay(digest string, shape Shape) (provider.Stream, error) {
	if p.n > len(p.run.Steps) {
		return nil, fmt.Errorf("eval: recording %s has %d steps, model asked for step %d; re-record",
			p.path, len(p.run.Steps), p.n)
	}
	st := p.run.Steps[p.n-1]
	if st.Digest != digest {
		return nil, &MismatchError{Path: p.path, Step: p.n, Want: st.Digest, Got: digest, WantShape: st.Shape, GotShape: shape}
	}
	if st.ErrorKind != "" {
		return nil, &provider.Error{Kind: provider.ErrorKind(st.ErrorKind), Provider: "replay", Message: "recorded failure"}
	}
	if st.Response == nil {
		return nil, fmt.Errorf("eval: recording %s step %d has neither response nor error", p.path, p.n)
	}
	return &replayStream{resp: &provider.Response{
		Text:      st.Response.Text,
		ToolCalls: st.Response.ToolCalls,
		Finish:    st.Response.Finish,
		Usage:     st.Response.Usage,
	}}, nil
}

func (p *Provider) record(s Step) { p.run.Steps = append(p.run.Steps, s) }

// Save writes the recording (record mode only; a no-op otherwise).
func (p *Provider) Save() error {
	if p.mode != ModeRecord {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p.path), 0o755); err != nil {
		return fmt.Errorf("eval: recording dir: %w", err)
	}
	b, err := json.MarshalIndent(p.run, "", "  ")
	if err != nil {
		return fmt.Errorf("eval: encode recording: %w", err)
	}
	if err := os.WriteFile(p.path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("eval: write recording: %w", err)
	}
	return nil
}

// MismatchError says the replayed request is no longer the recorded one.
type MismatchError struct {
	Path      string
	Step      int
	Want, Got string
	WantShape Shape
	GotShape  Shape
}

func (e *MismatchError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "eval: recording %s step %d does not match this prompt (re-record)\n", e.Path, e.Step)
	fmt.Fprintf(&b, "  recorded %s  %d messages, %d system chars, tools [%s]\n",
		short(e.Want), e.WantShape.Messages, e.WantShape.SystemChars, strings.Join(e.WantShape.Tools, " "))
	fmt.Fprintf(&b, "  now      %s  %d messages, %d system chars, tools [%s]",
		short(e.Got), e.GotShape.Messages, e.GotShape.SystemChars, strings.Join(e.GotShape.Tools, " "))
	if e.WantShape.LastMessage != e.GotShape.LastMessage {
		fmt.Fprintf(&b, "\n  last message recorded: %q\n  last message now:      %q",
			e.WantShape.LastMessage, e.GotShape.LastMessage)
	}
	return b.String()
}

func short(d string) string {
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// Digest hashes the part of a request that must not drift between record and
// replay: the system prompt, the messages and the tool names.
func Digest(req provider.Request) (string, Shape) {
	h := sha256.New()
	write := func(parts ...string) {
		for _, s := range parts {
			_, _ = io.WriteString(h, s)
			_, _ = h.Write([]byte{0})
		}
	}
	write("system", req.System)
	last := ""
	for _, m := range req.Messages {
		write("msg", string(m.Role), m.Text(), m.ToolCallID, m.ToolName)
		for _, tc := range m.ToolCalls {
			write("call", tc.ID, tc.Name, tc.Arguments)
		}
		if t := m.Text(); t != "" {
			last = t
		}
	}
	names := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		names = append(names, t.Name)
		write("tool", t.Name)
	}
	return hex.EncodeToString(h.Sum(nil)), Shape{
		Messages:    len(req.Messages),
		Tools:       names,
		SystemChars: len(req.System),
		LastMessage: tail(last, maxShapeText),
	}
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

// Path is where the recording of one run lives.
func Path(dir, model, scenario string, run int) string {
	return filepath.Join(dir, Slug(model), Slug(scenario), fmt.Sprintf("%d.json", run))
}

// Slug makes a model or scenario name safe for a path segment.
func Slug(s string) string {
	var b strings.Builder
	prev := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
			prev = false
		default:
			if !prev && b.Len() > 0 {
				b.WriteByte('-')
				prev = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// recStream forwards a live stream and keeps the final response.
type recStream struct {
	p     *Provider
	inner provider.Stream
	step  Step
	saved bool
}

func (s *recStream) Next() (provider.Chunk, error) {
	c, err := s.inner.Next()
	if err != nil && !s.saved && err != io.EOF {
		s.saved = true
		s.step.ErrorKind = string(provider.KindOf(err))
		s.p.record(s.step)
		return c, err
	}
	if c.Kind == provider.ChunkDone && c.Response != nil && !s.saved {
		s.saved = true
		s.step.Response = &Response{
			Text:      c.Response.Text,
			ToolCalls: c.Response.ToolCalls,
			Finish:    c.Response.Finish,
			Usage:     c.Response.Usage,
		}
		s.p.record(s.step)
	}
	return c, err
}

func (s *recStream) Close() error { return s.inner.Close() }

// replayStream serves a recorded response as a minimal stream: the whole text
// as one delta, then the done chunk.
type replayStream struct {
	resp *provider.Response
	i    int
}

func (s *replayStream) Next() (provider.Chunk, error) {
	switch s.i {
	case 0:
		s.i++
		if s.resp.Text != "" {
			return provider.Chunk{Kind: provider.ChunkText, Text: s.resp.Text}, nil
		}
		fallthrough
	case 1:
		s.i = 2
		return provider.Chunk{Kind: provider.ChunkDone, Response: s.resp}, nil
	default:
		return provider.Chunk{}, io.EOF
	}
}

func (s *replayStream) Close() error { return nil }
