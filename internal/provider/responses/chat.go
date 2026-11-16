package responses

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

// Event names this transport acts on. Everything else — reasoning summaries,
// item content parts, in-progress notices — is ignored.
const (
	evTextDelta      = "response.output_text.delta"
	evItemAdded      = "response.output_item.added"
	evItemDone       = "response.output_item.done"
	evArgsDelta      = "response.function_call_arguments.delta"
	evArgsDone       = "response.function_call_arguments.done"
	evCompleted      = "response.completed"
	evIncomplete     = "response.incomplete"
	evFailed         = "response.failed"
	evError          = "error"
	reasonMaxTokens  = "max_output_tokens"
	itemFunctionCall = "function_call"
)

// Chat starts a streaming Responses call. A concurrency slot is held until the
// returned stream is closed.
func (c *Client) Chat(ctx context.Context, req provider.Request) (provider.Stream, error) {
	body, err := json.Marshal(c.buildRequest(req))
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrBadRequest, Provider: c.name, Message: "encoding request: " + err.Error(), Err: err}
	}
	if err := c.acquire(ctx); err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, c.requestTimeout(req.Model))
	release := sync.OnceFunc(func() { cancel(); c.release() })

	resp, secrets, err := c.send(rctx, ctx, req, body)
	if err != nil {
		release()
		return nil, err
	}

	s := &stream{
		c:        c,
		ctx:      rctx,
		parent:   ctx,
		body:     resp.Body,
		items:    make(chan item, 8),
		progress: c.progress,
		secrets:  secrets,
		release:  release,
	}
	go s.read(req)
	return s, nil
}

// send performs the POST, retrying once through OnUnauthorized on a 401. It
// returns the streaming response and the secrets to keep out of error text.
func (c *Client) send(rctx, parent context.Context, req provider.Request, body []byte) (*http.Response, []string, error) {
	for attempt := 0; ; attempt++ {
		secrets := c.secrets(req)
		httpReq, err := c.newRequest(rctx, req, bytes.NewReader(body))
		if err != nil {
			return nil, secrets, err
		}
		resp, err := c.hc.Do(httpReq)
		if err != nil {
			return nil, secrets, c.transportError(parent, rctx, err, secrets)
		}
		if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
			return resp, secrets, nil
		}
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		_ = resp.Body.Close()
		// A 401 is worth exactly one refresh: the hook renews the token and
		// the request goes out again with fresh headers and key.
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && c.onUnauth != nil {
			if rerr := c.onUnauth(rctx); rerr != nil {
				return nil, secrets, &provider.Error{
					Kind:     provider.ErrAuth,
					Provider: c.name,
					Status:   resp.StatusCode,
					Message:  c.redact(rerr.Error(), secrets),
					Err:      rerr,
				}
			}
			continue
		}
		return nil, secrets, c.httpError(resp, errBody, secrets)
	}
}

// transportError distinguishes a caller cancellation from our own deadline.
func (c *Client) transportError(parent, rctx context.Context, err error, secrets []string) *provider.Error {
	if parent.Err() != nil {
		return c.ctxError(parent.Err(), secrets)
	}
	if rctx.Err() != nil {
		return c.ctxError(rctx.Err(), secrets)
	}
	return c.ctxError(err, secrets)
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
	secrets  []string
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

// send delivers an item unless the stream has been closed.
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
	names := map[string]string{}
	for original, alias := range toolNames(req) {
		names[alias] = original
	}
	var text []byte
	var usage provider.Usage
	var finish provider.FinishReason // empty until a terminal event arrives
	sse := wire.NewSSEReader(s.body)

	for {
		name, data, err := sse.Next()
		if err != nil {
			if ctxErr := s.contextError(); ctxErr != nil {
				s.send(item{err: ctxErr})
				return
			}
			// EOF without a terminal event is a dropped stream, not an error:
			// the caller decides what to do.
			break
		}
		var ev wireEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue // ignore frames we cannot parse (keep-alives, banners)
		}
		if name == "" {
			name = ev.Type // the payload repeats the event name
		}

		switch name {
		case evTextDelta:
			if ev.Delta == "" {
				continue
			}
			if out := scrub.Write(ev.Delta); out != "" {
				text = append(text, out...)
				if !s.send(item{chunk: provider.Chunk{Kind: provider.ChunkText, Text: out}}) {
					return
				}
			}
		case evItemAdded, evItemDone:
			if ev.Item != nil {
				if original, ok := names[ev.Item.Name]; ok {
					ev.Item.Name = original
				}
			}
			acc.item(ev.OutputIndex, ev.Item, name == evItemDone)
		case evArgsDelta:
			acc.argsDelta(ev.OutputIndex, ev.ItemID, ev.Delta)
		case evArgsDone:
			acc.argsDone(ev.OutputIndex, ev.ItemID, ev.Arguments)
		case evCompleted:
			usage = eventUsage(ev, usage)
			finish = provider.FinishStop
		case evIncomplete:
			usage = eventUsage(ev, usage)
			finish = provider.FinishStop
			if ev.Response != nil && ev.Response.IncompleteDetails != nil &&
				ev.Response.IncompleteDetails.Reason == reasonMaxTokens {
				finish = provider.FinishLength
			}
		case evFailed, evError:
			s.send(item{err: s.c.streamError(&ev, data, s.secrets)})
			return
		default:
			continue // reasoning summaries and everything else we do not model
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
	switch {
	case dropped:
		resp.Finish = provider.FinishDropped
	case finish == provider.FinishLength:
		resp.Finish = provider.FinishLength
	case len(resp.ToolCalls) > 0:
		resp.Finish = provider.FinishToolCalls
	default:
		resp.Finish = provider.FinishStop
	}
	if len(resp.Repairs) == 0 {
		resp.Repairs = nil
	}
	s.send(item{chunk: provider.Chunk{Kind: provider.ChunkDone, Response: resp}})
}

func eventUsage(ev wireEvent, cur provider.Usage) provider.Usage {
	if ev.Response == nil || ev.Response.Usage == nil {
		return cur
	}
	u := ev.Response.Usage
	out := provider.Usage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens}
	if u.InputTokensDetails != nil {
		out.CachedTokens = u.InputTokensDetails.CachedTokens
	}
	if u.OutputTokensDetails != nil {
		out.ReasoningTokens = u.OutputTokensDetails.ReasoningTokens
	}
	return out
}

// contextError reports a typed error when the read stopped because the request
// context ended rather than because the body ended.
func (s *stream) contextError() *provider.Error {
	if s.parent.Err() != nil {
		return s.c.ctxError(s.parent.Err(), s.secrets)
	}
	if s.ctx.Err() != nil {
		return s.c.ctxError(s.ctx.Err(), s.secrets)
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

// callAccumulator assembles function calls keyed by output_index. The name and
// call_id arrive on the item, the arguments in fragments that may identify the
// item by index or by item id.
type callAccumulator struct {
	order  []int
	byIdx  map[int]*provider.ToolCall
	args   map[int]*bytes.Buffer
	byItem map[string]int // item id -> output index
}

func newCallAccumulator() *callAccumulator {
	return &callAccumulator{
		byIdx:  map[int]*provider.ToolCall{},
		args:   map[int]*bytes.Buffer{},
		byItem: map[string]int{},
	}
}

// slot returns the accumulator entry for an output index, creating it if new.
func (a *callAccumulator) slot(idx int) *provider.ToolCall {
	tc, ok := a.byIdx[idx]
	if !ok {
		tc = &provider.ToolCall{}
		a.byIdx[idx] = tc
		a.args[idx] = &bytes.Buffer{}
		a.order = append(a.order, idx)
	}
	return tc
}

// resolve maps an event onto an output index: by output_index when the frame
// carries one, otherwise by the item id seen on output_item.added, and failing
// both by the call currently in flight.
func (a *callAccumulator) resolve(outputIndex *int, itemID string) (int, bool) {
	if outputIndex != nil {
		return *outputIndex, true
	}
	if idx, ok := a.byItem[itemID]; ok {
		return idx, true
	}
	if n := len(a.order); n > 0 {
		return a.order[n-1], true
	}
	return 0, false
}

// item records a function_call item; done marks its arguments complete.
func (a *callAccumulator) item(outputIndex *int, wi *wireItem, done bool) {
	if wi == nil || wi.Type != itemFunctionCall {
		return
	}
	// An item frame names its own slot: without an output_index a new item is
	// a new call, never a continuation of the last one.
	idx := len(a.order)
	switch {
	case outputIndex != nil:
		idx = *outputIndex
	case wi.ID != "":
		if known, ok := a.byItem[wi.ID]; ok {
			idx = known
		}
	}
	tc := a.slot(idx)
	if wi.ID != "" {
		a.byItem[wi.ID] = idx
	}
	if wi.CallID != "" {
		tc.ID = wi.CallID
	}
	if wi.Name != "" {
		tc.Name = wi.Name
	}
	// A done item carries the whole argument text and is authoritative.
	if done && wi.Arguments != "" {
		a.args[idx].Reset()
		a.args[idx].WriteString(wi.Arguments)
	}
}

func (a *callAccumulator) argsDelta(outputIndex *int, itemID, delta string) {
	if delta == "" {
		return
	}
	idx, ok := a.resolve(outputIndex, itemID)
	if !ok {
		return
	}
	a.slot(idx)
	if itemID != "" {
		a.byItem[itemID] = idx
	}
	a.args[idx].WriteString(delta)
}

// argsDone replaces the accumulated fragments with the authoritative text.
func (a *callAccumulator) argsDone(outputIndex *int, itemID, args string) {
	if args == "" {
		return
	}
	idx, ok := a.resolve(outputIndex, itemID)
	if !ok {
		return
	}
	a.slot(idx)
	a.args[idx].Reset()
	a.args[idx].WriteString(args)
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
