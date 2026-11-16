// Package testmodel is a scripted OpenAI-compatible endpoint for tests: one
// scripted Step answers one POST /chat/completions as a streamed SSE response,
// and POST /embeddings answers with deterministic vectors. It exists so the
// daemon and the CLI can be exercised end to end without a model — the same
// wire format internal/provider/openaicompat speaks, nothing more.
//
// When the script runs out the server answers a fixed text instead of hanging:
// a test then fails on its own assertion, which names what went wrong, rather
// than on a timeout.
package testmodel

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/provider"
)

// Exhausted is the reply sent once the script has no Step left.
const Exhausted = "testmodel: the script ran out of steps"

// EmbedDims is the length of every vector /embeddings returns. Small on
// purpose: recall ranking only needs cosine distances to be stable.
const EmbedDims = 8

// Step is one scripted answer.
type Step struct {
	// Text is the visible reply, streamed in a few deltas.
	Text string
	// ToolCalls, when set, are streamed as tool-call deltas and the finish
	// reason defaults to "tool_calls".
	ToolCalls []provider.ToolCall
	// Finish overrides the finish reason ("stop", "length", …).
	Finish string
	// Status, when >= 400, answers with that HTTP status and an error body
	// instead of a stream.
	Status int
	// Delay is how long the server waits before answering. It honours the
	// request context, so a cancelled turn does not hold the test up.
	Delay time.Duration
}

// Message is one decoded request message.
type Message struct {
	Role       string
	Content    string
	ToolCallID string
	ToolName   string
	ToolCalls  []provider.ToolCall
}

// Request is one decoded /chat/completions request.
type Request struct {
	Model    string
	Messages []Message
	// Tools is the advertised tool names, in the order they were sent.
	Tools []string
	// Raw is the request body as received.
	Raw []byte
}

// System returns the system message, empty when there is none.
func (r Request) System() string {
	for _, m := range r.Messages {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

// Text is every message's content joined, for a "does the prompt contain …"
// assertion.
func (r Request) Text() string {
	var b strings.Builder
	for _, m := range r.Messages {
		b.WriteString(m.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// Last returns the final message of the request.
func (r Request) Last() Message {
	if len(r.Messages) == 0 {
		return Message{}
	}
	return r.Messages[len(r.Messages)-1]
}

// Server is one running scripted endpoint.
type Server struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	script   []Step
	step     int
	requests []Request
	embeds   int
}

// New starts a scripted server and stops it when the test ends.
func New(t *testing.T, script ...Step) *Server {
	t.Helper()
	s := &Server{t: t, script: script}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("POST /v1/embeddings", s.embed)
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the base URL to configure as a provider's base_url.
func (s *Server) URL() string { return s.srv.URL + "/v1" }

// Requests returns every decoded chat request so far, in order.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Calls is how many chat requests have been answered.
func (s *Server) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

// Embeds is how many /embeddings requests were served.
func (s *Server) Embeds() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.embeds
}

// next takes the next scripted step, or reports that the script is spent.
func (s *Server) next(req Request) (Step, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, req)
	if s.step >= len(s.script) {
		return Step{Text: Exhausted}, false
	}
	st := s.script[s.step]
	s.step++
	return st, true
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	req, err := decode(raw)
	if err != nil {
		s.t.Errorf("testmodel: bad request body: %v", err)
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	step, _ := s.next(req)

	if step.Delay > 0 {
		timer := time.NewTimer(step.Delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.Context().Done():
			return
		}
	}
	if step.Status >= 400 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(step.Status)
		_, _ = fmt.Fprintf(w, `{"error":{"message":%q,"type":"testmodel"}}`, "scripted failure")
		return
	}
	s.stream(w, r, step)
}

// stream writes one scripted answer as chat-completions SSE frames.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, step Step) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	send := func(obj any) bool {
		b, err := json.Marshal(obj)
		if err != nil {
			s.t.Errorf("testmodel: encode chunk: %v", err)
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return r.Context().Err() == nil
	}

	for _, part := range splitText(step.Text) {
		if !send(chunk{Choices: []choice{{Delta: delta{Content: part}}}}) {
			return
		}
	}
	for i, tc := range step.ToolCalls {
		idx := i
		args := tc.Arguments
		if args == "" {
			args = "{}"
		}
		d := delta{ToolCalls: []deltaCall{{
			Index: &idx, ID: tc.ID, Type: "function",
			Function: deltaFunc{Name: tc.Name, Arguments: args},
		}}}
		if !send(chunk{Choices: []choice{{Delta: d}}}) {
			return
		}
	}

	finish := step.Finish
	if finish == "" {
		finish = "stop"
		if len(step.ToolCalls) > 0 {
			finish = "tool_calls"
		}
	}
	if !send(chunk{Choices: []choice{{FinishReason: finish}}}) {
		return
	}
	if !send(chunk{Usage: &usage{PromptTokens: 11, CompletionTokens: 3}}) {
		return
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// splitText cuts a reply into two deltas, so callers exercise the streaming
// path rather than a single frame. Empty text streams nothing.
func splitText(s string) []string {
	if s == "" {
		return nil
	}
	if len(s) < 4 {
		return []string{s}
	}
	half := len(s) / 2
	return []string{s[:half], s[half:]}
}

// embed answers with one deterministic vector per input: same text, same
// vector, on every run and every process.
func (s *Server) embed(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&in); err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.embeds++
	s.mu.Unlock()

	type item struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	out := struct {
		Data []item `json:"data"`
	}{Data: make([]item, 0, len(in.Input))}
	for i, text := range in.Input {
		out.Data = append(out.Data, item{Index: i, Embedding: Vector(text)})
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		s.t.Errorf("testmodel: encode embeddings: %v", err)
	}
}

// Vector is the embedding of one text: a unit-length vector derived from the
// text's words, so similar texts land near each other.
func Vector(text string) []float32 {
	v := make([]float32, EmbedDims)
	for _, word := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = io.WriteString(h, word)
		v[h.Sum32()%EmbedDims]++
	}
	var norm float32
	for _, x := range v {
		norm += x * x
	}
	if norm == 0 {
		v[0] = 1
		return v
	}
	inv := 1 / sqrt32(norm)
	for i := range v {
		v[i] *= inv
	}
	return v
}

// sqrt32 is Newton's method: math.Sqrt would need a float64 round trip for a
// value this small.
func sqrt32(x float32) float32 {
	z := x
	for range 12 {
		z = (z + x/z) / 2
	}
	return z
}

// --- request decoding ----------------------------------------------------------

func decode(raw []byte) (Request, error) {
	var w wireRequest
	if err := json.Unmarshal(raw, &w); err != nil {
		return Request{}, err
	}
	out := Request{Model: w.Model, Raw: raw}
	for _, m := range w.Messages {
		msg := Message{Role: m.Role, Content: content(m.Content), ToolCallID: m.ToolCallID, ToolName: m.Name}
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, provider.ToolCall{
				ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
			})
		}
		out.Messages = append(out.Messages, msg)
	}
	for _, t := range w.Tools {
		out.Tools = append(out.Tools, t.Function.Name)
	}
	return out, nil
}

// content flattens a string or a content-part array into plain text.
func content(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := m["text"].(string); ok {
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		return ""
	}
}

type wireRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role      string `json:"role"`
		Content   any    `json:"content"`
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
		ToolCallID string `json:"tool_call_id"`
		Name       string `json:"name"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

type chunk struct {
	Choices []choice `json:"choices"`
	Usage   *usage   `json:"usage,omitempty"`
}

type choice struct {
	Delta        delta  `json:"delta"`
	FinishReason string `json:"finish_reason,omitempty"`
}

type delta struct {
	Content   string      `json:"content,omitempty"`
	ToolCalls []deltaCall `json:"tool_calls,omitempty"`
}

type deltaCall struct {
	Index    *int      `json:"index,omitempty"`
	ID       string    `json:"id,omitempty"`
	Type     string    `json:"type,omitempty"`
	Function deltaFunc `json:"function"`
}

type deltaFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}
