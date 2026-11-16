package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"holaclaw/internal/provider"
	"holaclaw/internal/provider/wire"
)

const doneSentinel = "[DONE]"

// maxErrorBody caps how much of an error response is read.
const maxErrorBody = 64 << 10

// Chat starts a streaming chat-completions call. A concurrency slot is held
// until the returned stream is closed.
func (c *Client) Chat(ctx context.Context, req provider.Request) (provider.Stream, error) {
	w := buildRequest(req)
	if c.sort != "" {
		w.Provider = &wireProvider{Sort: c.sort}
	}
	body, err := json.Marshal(w)
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrBadRequest, Provider: c.name, Message: "encoding request: " + err.Error(), Err: err}
	}
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, c.requestTimeout(req.Model))
	release := sync.OnceFunc(func() { cancel(); c.release() })

	httpReq, err := c.newRequest(rctx, http.MethodPost, "/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		release()
		return nil, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		release()
		return nil, c.transportError(ctx, rctx, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
		release()
		return nil, c.httpError(resp, errBody)
	}

	s := &stream{
		c:        c,
		ctx:      rctx,
		parent:   ctx,
		body:     resp.Body,
		items:    make(chan item, 8),
		progress: c.progress,
		release:  release,
	}
	go s.read(req)
	return s, nil
}

// transportError distinguishes a caller cancellation from our own deadline.
func (c *Client) transportError(parent, rctx context.Context, err error) *provider.Error {
	if parent.Err() != nil {
		return c.ctxError(parent.Err())
	}
	if rctx.Err() != nil {
		return c.ctxError(rctx.Err())
	}
	return c.ctxError(err)
}

type item struct {
	chunk provider.Chunk
	err   error
}

type stream struct {
	c        *Client
	ctx      context.Context
	parent   context.Context
	body     io.ReadCloser
	items    chan item
	progress time.Duration
	release  func()

	closeOnce sync.Once
	done      bool
}

// Next returns the next chunk, emitting a progress keep-alive whenever the
// endpoint has been quiet for ProgressInterval.
func (s *stream) Next() (provider.Chunk, error) {
	if s.done {
		return provider.Chunk{}, io.EOF
	}
	t := time.NewTimer(s.progress)
	defer t.Stop()
	select {
	case it, ok := <-s.items:
		if !ok {
			s.done = true
			return provider.Chunk{}, io.EOF
		}
		if it.err != nil {
			s.done = true
			return provider.Chunk{}, it.err
		}
		if it.chunk.Kind == provider.ChunkDone {
			s.done = true
		}
		return it.chunk, nil
	case <-t.C:
		return provider.Chunk{Kind: provider.ChunkProgress}, nil
	}
}

// Close aborts the request and releases the concurrency slot exactly once.
func (s *stream) Close() error {
	s.closeOnce.Do(func() {
		s.release() // cancels the request context, unblocking the reader
		_ = s.body.Close()
	})
	return nil
}

// send delivers an item unless the stream has been closed. The buffered
// channel is tried first so a cancelled request still reports its error rather
// than looking like a clean end of stream.
func (s *stream) send(it item) bool {
	select {
	case s.items <- it:
		return true
	default:
	}
	select {
	case s.items <- it:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// read consumes the SSE body and assembles the final response.
func (s *stream) read(req provider.Request) {
	defer close(s.items)

	scrub := wire.NewScrubber(req.Model.ThinkingTags)
	acc := newCallAccumulator()
	var text []byte
	var usage provider.Usage
	var finish string
	sse := wire.NewSSEReader(s.body)

	for {
		_, data, err := sse.Next()
		if err != nil {
			if ctxErr := s.contextError(); ctxErr != nil {
				s.send(item{err: ctxErr})
				return
			}
			// EOF (or a broken connection) without a finish reason is a
			// dropped stream, not an error: the caller decides what to do.
			break
		}
		if data == doneSentinel {
			break
		}
		var ch wireChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue // ignore frames we cannot parse (keep-alives, banners)
		}
		if len(ch.Error) > 0 && string(ch.Error) != "null" {
			s.send(item{err: s.c.streamError(ch.Error)})
			return
		}
		if ch.Usage != nil {
			usage = ch.Usage.usage()
		}
		for _, choice := range ch.Choices {
			if choice.FinishReason != "" {
				finish = choice.FinishReason
			}
			acc.add(choice.Delta.ToolCalls)
			if choice.Delta.Content == "" {
				continue
			}
			if out := scrub.Write(choice.Delta.Content); out != "" {
				text = append(text, out...)
				if !s.send(item{chunk: provider.Chunk{Kind: provider.ChunkText, Text: out}}) {
					return
				}
			}
		}
	}

	if out := scrub.Flush(); out != "" {
		text = append(text, out...)
		if !s.send(item{chunk: provider.Chunk{Kind: provider.ChunkText, Text: out}}) {
			return
		}
	}

	resp := &provider.Response{
		Text:    string(text),
		Usage:   usage,
		Repairs: map[string]string{},
	}
	dropped := finish == ""
	resp.ToolCalls = s.finishCalls(acc, dropped, resp.Repairs)
	resp.Finish = finishReason(finish, len(resp.ToolCalls) > 0)
	if len(resp.Repairs) == 0 {
		resp.Repairs = nil
	}
	s.send(item{chunk: provider.Chunk{Kind: provider.ChunkDone, Response: resp}})
}

// contextError reports a typed error when the read stopped because the request
// context ended rather than because the body ended.
func (s *stream) contextError() *provider.Error {
	if s.parent.Err() != nil {
		return s.c.ctxError(s.parent.Err())
	}
	if s.ctx.Err() != nil {
		return s.c.ctxError(s.ctx.Err())
	}
	return nil
}

// finishCalls repairs the accumulated tool calls. On a dropped stream a call
// whose arguments did not arrive complete is discarded rather than repaired:
// half a call must never be executed (docs/models.md).
func (s *stream) finishCalls(acc *callAccumulator, dropped bool, repairs map[string]string) []provider.ToolCall {
	calls := acc.calls()
	out := make([]provider.ToolCall, 0, len(calls))
	logger := slog.Default()
	for _, tc := range calls {
		if tc.Name == "" {
			continue
		}
		if dropped && !wire.IsJSONObject(tc.Arguments) {
			logger.Warn("dropping incomplete tool call from a dropped stream",
				"provider", s.c.name, "tool", tc.Name, "call_id", tc.ID)
			continue
		}
		wire.RepairCall(logger, s.c.name, &tc, repairs)
		out = append(out, tc)
	}
	return out
}

// finishReason maps the wire finish_reason onto the contract.
func finishReason(raw string, hasCalls bool) provider.FinishReason {
	switch raw {
	case "":
		return provider.FinishDropped
	case "length", "max_tokens":
		return provider.FinishLength
	case "tool_calls", "function_call":
		return provider.FinishToolCalls
	default:
		if hasCalls {
			// Several local runtimes report "stop" alongside tool calls.
			return provider.FinishToolCalls
		}
		return provider.FinishStop
	}
}

// callAccumulator assembles tool calls from deltas keyed by index: the id and
// name arrive once, the arguments in fragments.
type callAccumulator struct {
	order []int
	byIdx map[int]*provider.ToolCall
	args  map[int]*bytes.Buffer
}

func newCallAccumulator() *callAccumulator {
	return &callAccumulator{byIdx: map[int]*provider.ToolCall{}, args: map[int]*bytes.Buffer{}}
}

func (a *callAccumulator) add(deltas []wireDeltaCall) {
	for _, d := range deltas {
		// Endpoints that omit the index send one call at a time: a fragment
		// without an id continues the call already in flight.
		var idx int
		switch {
		case d.Index != nil:
			idx = *d.Index
		case d.ID != "" || len(a.order) == 0:
			idx = len(a.order)
		default:
			idx = a.order[len(a.order)-1]
		}
		tc, ok := a.byIdx[idx]
		if !ok {
			tc = &provider.ToolCall{}
			a.byIdx[idx] = tc
			a.args[idx] = &bytes.Buffer{}
			a.order = append(a.order, idx)
		}
		if d.ID != "" {
			tc.ID = d.ID
		}
		if d.Function.Name != "" {
			tc.Name = d.Function.Name
		}
		if d.Function.Arguments != "" {
			a.args[idx].WriteString(d.Function.Arguments)
		}
	}
}

func (a *callAccumulator) calls() []provider.ToolCall {
	idx := append([]int(nil), a.order...)
	sort.Ints(idx)
	out := make([]provider.ToolCall, 0, len(idx))
	for i, n := range idx {
		tc := *a.byIdx[n]
		tc.Arguments = a.args[n].String()
		if tc.ID == "" {
			tc.ID = "call_" + strconv.Itoa(i)
		}
		out = append(out, tc)
	}
	return out
}
