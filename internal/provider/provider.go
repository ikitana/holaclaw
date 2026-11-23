// Package provider defines the model-provider contract. A Provider owns the
// connection: wire format, auth and concurrency. A Model owns identity and
// capabilities (docs/models.md). Implementations live in subpackages.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/shopspring/decimal"
)

// Model is the capability profile of one configured model.
type Model struct {
	Name          string        // config name, e.g. "gemma"
	Provider      string        // provider instance name
	ID            string        // id on the wire, e.g. "gemma4:e2b"
	Context       int           // context window in tokens
	Tools         bool          // supports native tool calling
	Vision        bool          // accepts image parts
	Strict        bool          // promote text-leaked tool calls (off until measured)
	ThinkingTags  []string      // extra reasoning-tag names to scrub
	SchemaProfile string        // "" | "llamacpp"
	Reasoning     Reasoning     // effort for a call that sets none; "" = the model's default
	Timeout       time.Duration // whole-request timeout; 0 = provider default
	API           API           // wire API the model speaks; "" means APIChat
	URL           string        // absolute endpoint override; required for APIDecisions
}

// API is the wire API a model speaks. A role only accepts the APIs it can use.
type API string

const (
	APIChat      API = "chat"      // Provider
	APIDecisions API = "decisions" // Decider
	APIImages    API = "images"    // ImageGenerator
)

// Role of a chat message on the wire.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Part is one piece of message content. Exactly one of Text or Image is set.
type Part struct {
	Text  string
	Image *Image
}

// Image is inline image content for vision-capable models.
type Image struct {
	MIME string
	Data []byte
}

// ToolCall is a tool invocation requested by the model.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string // JSON object text
}

// Message is one chat message sent to a model.
type Message struct {
	Role       Role
	Parts      []Part
	ToolCalls  []ToolCall // assistant only
	ToolCallID string     // tool only
	ToolName   string     // tool only
}

// Text returns the concatenated text parts.
func (m Message) Text() string {
	var s string
	for _, p := range m.Parts {
		s += p.Text
	}
	return s
}

// TextMessage builds a single-part text message.
func TextMessage(r Role, text string) Message {
	return Message{Role: r, Parts: []Part{{Text: text}}}
}

// ToolSpec describes a tool to the model: schema only, described once.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage // JSON Schema object
}

// Request is one model call. System is kept apart from Messages because some
// transports carry it separately (Responses API `instructions`).
type Request struct {
	Model     Model
	System    string
	Messages  []Message
	Tools     []ToolSpec
	MaxTokens int    // reply reserve; 0 = unset (some transports forbid it)
	SessionID string // opaque, for transports that want a session header
	// Reasoning bounds how much a reasoning model may think before it
	// answers; "" leaves the model's default. Transports that cannot express
	// it ignore it, and so do models without the feature.
	Reasoning Reasoning
}

// Effort is the reasoning effort of the call: its own, else its model's.
func (r Request) Effort() Reasoning {
	if r.Reasoning != ReasoningDefault {
		return r.Reasoning
	}
	return r.Model.Reasoning
}

// Reasoning is the effort a call may spend thinking (Request.Reasoning).
type Reasoning string

const (
	ReasoningDefault Reasoning = ""
	ReasoningNone    Reasoning = "none" // answer directly: extraction, classification
	ReasoningLow     Reasoning = "low"  // a little: short structured plans
	// A model may be configured with any other effort its server knows
	// ("medium", "high", …); it is passed through as is.
)

// FinishReason says why a response ended.
type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishToolCalls FinishReason = "tool_calls"
	FinishLength    FinishReason = "length"  // truncated by the token limit
	FinishDropped   FinishReason = "dropped" // stream ended without a finish reason
)

// Usage is what a model call cost, as the upstream reported it. Tokens are
// zero when not reported; the pointers are nil when not reported, so a call
// to a local engine or a subscription (no price) never reads as free, and a
// reported zero (a free model) stays a zero. Sums of calls are event.Usage.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	// Cost is what the upstream charged for the call.
	Cost *NanoUSD
	// CachedTokens is the part of PromptTokens served from a prompt cache.
	CachedTokens *int
	// ReasoningTokens is the part of CompletionTokens spent reasoning.
	ReasoningTokens *int
}

// NanoUSD is an amount of USD in units of 1e-9. It is exact: sums are
// integer adds, never float ones. ParseUSD reads an upstream's price,
// Decimal formats one.
type NanoUSD int64

// ParseUSD reads a decimal USD amount as an upstream writes it ("0.000123",
// "1.2e-05") exactly, rounded half away from zero to the nano-USD. A
// negative amount is an error, as is one beyond int64 or not a number: an
// upstream never charges less than nothing, so its wire parser drops such a
// cost and the call counts as unpriced.
func ParseUSD(s string) (NanoUSD, error) {
	if len(s) > 64 {
		return 0, errors.New("provider: usd amount too long")
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return 0, fmt.Errorf("provider: usd amount: %w", err)
	}
	// The exponent bounds keep the rescaling below cheap; 64 digits times
	// 1e-100 is far below half a nano-USD.
	switch {
	case d.IsNegative():
		return 0, fmt.Errorf("provider: negative usd amount %s", s)
	case d.Exponent() < -100:
		return 0, nil
	case d.Exponent() > 100:
		return 0, fmt.Errorf("provider: usd amount %s out of range", s)
	}
	n := d.Shift(9).Round(0)
	if n.Cmp(decimal.New(math.MaxInt64, 0)) > 0 {
		return 0, fmt.Errorf("provider: usd amount %s out of range", s)
	}
	return NanoUSD(n.IntPart()), nil
}

// WireCost is a reported cost from the raw JSON value of an upstream's cost
// field, read as text so no float is ever made. Absent, null, a string or
// any value ParseUSD refuses is nil: the call is unpriced, not failed.
func WireCost(raw json.RawMessage) *NanoUSD {
	if len(raw) == 0 {
		return nil
	}
	n, err := ParseUSD(string(raw))
	if err != nil {
		return nil
	}
	return &n
}

// Decimal is n in USD.
func (n NanoUSD) Decimal() decimal.Decimal { return decimal.New(int64(n), -9) }

// Response is the fully assembled result of a stream.
type Response struct {
	Text      string // reasoning tags already scrubbed
	ToolCalls []ToolCall
	Finish    FinishReason
	Usage     Usage
	// Repairs lists tool-call ids whose arguments needed repair, with the
	// repair stage that succeeded ("lenient", "trailing-comma", "auto-close",
	// "empty").
	Repairs map[string]string
}

// ChunkKind discriminates stream chunks.
type ChunkKind int

const (
	ChunkText     ChunkKind = iota // Text holds a visible text delta
	ChunkProgress                  // keep-alive while a slow local model works
	ChunkDone                      // Response holds the final result; last chunk
)

// Chunk is one item of a response stream.
type Chunk struct {
	Kind     ChunkKind
	Text     string
	Response *Response
}

// Stream yields chunks until a ChunkDone or an error. Callers must Close it.
type Stream interface {
	// Next blocks for the next chunk. After a ChunkDone chunk it returns io.EOF.
	Next() (Chunk, error)
	Close() error
}

// Provider is a configured connection to a model backend.
type Provider interface {
	Name() string
	// Chat starts a streaming model call. Concurrency limits are enforced
	// inside; ctx cancellation aborts the request.
	Chat(ctx context.Context, req Request) (Stream, error)
}

// Embedder is implemented by providers that serve /v1/embeddings.
// It returns one vector per input and what the call cost; a reply that does
// not match the inputs is an error that still carries the reply's usage.
type Embedder interface {
	Embed(ctx context.Context, model Model, inputs []string) ([][]float32, Usage, error)
}

// Transcriber is implemented by providers with an OpenAI-compatible STT
// endpoint. It returns the text and what the call cost.
type Transcriber interface {
	Transcribe(ctx context.Context, model Model, mime string, audio []byte) (string, Usage, error)
}

// ModelLister is implemented by providers that can list the model ids they
// serve for their credentials. Models only reads the stored credentials, and
// never refreshes or rewrites them: any process may call it, not only the
// daemon that owns them.
type ModelLister interface {
	Models(ctx context.Context) ([]string, error)
}

// Serves reports whether id is among ids, the answer of a ModelLister. Ollama
// serves a name without a tag as its `latest`.
func Serves(ids []string, id string) bool {
	return slices.Contains(ids, id) || slices.Contains(ids, id+":latest")
}

// Collect drains a stream and returns the final response. onText, if not nil,
// receives each visible text delta.
func Collect(s Stream, onText func(string)) (*Response, error) {
	return CollectProgress(s, onText, nil)
}

// CollectProgress is Collect with a callback for keep-alive progress chunks.
func CollectProgress(s Stream, onText func(string), onProgress func()) (*Response, error) {
	defer func() { _ = s.Close() }()
	for {
		c, err := s.Next()
		if err != nil {
			return nil, err
		}
		switch c.Kind {
		case ChunkText:
			if onText != nil {
				onText(c.Text)
			}
		case ChunkProgress:
			if onProgress != nil {
				onProgress()
			}
		case ChunkDone:
			if c.Response == nil {
				return nil, errors.New("provider: done chunk without response")
			}
			return c.Response, nil
		}
	}
}

// ErrorKind classifies provider failures so callers can decide on fallback.
type ErrorKind string

const (
	ErrAuth       ErrorKind = "auth"        // credentials missing/invalid: never retry
	ErrRateLimit  ErrorKind = "rate_limit"  // 429
	ErrUsageLimit ErrorKind = "usage_limit" // subscription quota; ResetAt set
	ErrTimeout    ErrorKind = "timeout"
	ErrContext    ErrorKind = "context_overflow"
	ErrBadRequest ErrorKind = "bad_request"
	ErrServer     ErrorKind = "server"
	ErrNetwork    ErrorKind = "network"
)

// Error is the typed error every provider returns. Message never holds secrets.
type Error struct {
	Kind       ErrorKind
	Provider   string
	Status     int
	Message    string
	RetryAfter time.Duration
	ResetAt    time.Time
	Err        error
}

func (e *Error) Error() string {
	s := "provider " + e.Provider + ": " + string(e.Kind)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf returns the ErrorKind of err, or "" if it is not a provider error.
func KindOf(err error) ErrorKind {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Kind
	}
	return ""
}
