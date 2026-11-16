package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
	"holaclaw/internal/store"
)

// Compaction constants. Like the budget constants in builder.go they are part
// of the prompt contract: documented here, reported in the context.compacted
// event and asserted by tests (docs/context.md).
const (
	// CompressibleFloorTokens and CompressibleFloorPct define the floor below
	// which compaction cannot help: if `window − system − schemas − min tail −
	// reply reserve` is smaller than the larger of the two, the window is too
	// small for this agent and we fail loudly instead of thrashing.
	CompressibleFloorTokens = 1024
	CompressibleFloorPct    = 15
	// MinReclaimPct is the share of the history a rewrite must remove to be
	// worth committing. Below it the summary is ineffective: no commit.
	MinReclaimPct = 25
	// RearmPct is how much the history must grow, as a share of the available
	// history budget, after a commit before compaction is attempted again.
	RearmPct = 20
	// MaxCompactFails is how many ineffective or failed compactions a session
	// tolerates before compaction is switched off for it. A broken summarizer
	// must not burn tokens every turn.
	MaxCompactFails = 3
)

// CompactStore is the narrow persistence the compactor needs; store.Store
// satisfies it.
type CompactStore interface {
	ListMessages(ctx context.Context, sessionID, after event.ID, limit int) ([]event.Message, error)
	AppendMessage(ctx context.Context, m event.Message, pending bool) (bool, error)
	GetSession(ctx context.Context, id event.ID) (store.Session, error)
	UpdateSessionContext(ctx context.Context, id event.ID, prefixHash string, summaryMsgID event.ID, compactFails int) error
}

// CompactError is the typed failure of a compaction. Kind is stable and is
// carried by the context.compacted event as the reason.
type CompactError struct {
	Kind string // "window_too_small", "no_cut_point", "summarize", "empty_summary", "ineffective", "store"
	Msg  string
	Err  error
}

func (e *CompactError) Error() string {
	s := "agent: compaction: " + e.Kind
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	return s
}

func (e *CompactError) Unwrap() error { return e.Err }

// Compactor rewrites the prefix of a session transcript: one mechanical stage,
// one `summarize` call, one commit path (docs/context.md).
//
// It is stateless between calls; everything it needs is the session row and
// the transcript after the session's compaction point.
type Compactor struct {
	Store     CompactStore
	Summarize ModelRef // the `summarize` role (caller passes chat when absent)
	Events    event.Sink
	// Blobs spills an over-long tool result and returns its blob id; nil means
	// stage 1 truncates without a blob pointer.
	Blobs func(ctx context.Context, text string) (blobID string, err error)
	Now   func() time.Time
	// OnSummary lets the daemon feed the summary to recall memory; may be nil.
	// tainted says the summary covers untrusted content, so the caller must
	// keep it marked as such instead of laundering it (docs/security.md).
	OnSummary func(ctx context.Context, sess store.Session, summary string, tainted bool)
}

// CompactInput is one compaction attempt.
type CompactInput struct {
	SessionID, TraceID event.ID
	Def                Definition
	Model              provider.Model // the *chat* model: its window is the budget
	Tools              []provider.ToolSpec
	// Force runs the compaction even below the trigger and the re-arm
	// threshold. It never bypasses the compressible floor.
	Force bool
}

// CompactResult reports what one attempt did. Before and After are history
// tokens, the same number the builder puts in Breakdown.History.
type CompactResult struct {
	Compacted     bool
	Reason        string
	Before, After int      // history tokens before and after the rewrite
	SummaryMsgID  event.ID // id of the appended summary message
}

func (c *Compactor) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Compactor) emit(ctx context.Context, in CompactInput, attrs map[string]any) {
	if c.Events == nil {
		return
	}
	c.Events.Emit(ctx, event.New(event.ContextCompacted, in.TraceID, in.SessionID, "", attrs))
}

// budget is the arithmetic of one compaction: the same numbers the builder
// uses, computed with an empty history.
type budget struct {
	window       int
	system       int
	schemas      int // tool schemas plus Breakdown.Connectors
	reserve      int
	available    int // window − system − schemas − reply reserve
	compressible int // available − min tail
	floor        int
}

// budgetOf asks the builder for the fixed cost of this agent (system prompt
// plus tool schemas) by building a prompt with no history at all.
func (c *Compactor) budgetOf(ctx context.Context, in CompactInput) (budget, error) {
	p, err := NewBuilder().Build(ctx, BuildInput{Def: in.Def, Model: in.Model, Tools: in.Tools})
	if err != nil {
		return budget{}, err
	}
	b := p.Breakdown
	bg := budget{
		window:  b.Window,
		system:  b.System,
		schemas: b.ToolSchemas + b.Connectors,
		reserve: b.ReplyReserve,
		floor:   CompressibleFloorTokens,
	}
	if pct := b.Window * CompressibleFloorPct / 100; pct > bg.floor {
		bg.floor = pct
	}
	bg.available = bg.window - bg.system - bg.schemas - bg.reserve
	bg.compressible = bg.available - MinTailTokens
	return bg, nil
}

// Compact rewrites the session prefix if it is worth it.
//
// The order is fixed: budget arithmetic first (a window that cannot hold a
// compacted prompt is an error, not a retry), then the ineffectiveness
// counter, then the trigger and re-arm gates, then at most one summarize call,
// then the single commit path.
func (c *Compactor) Compact(ctx context.Context, in CompactInput) (CompactResult, error) {
	sess, err := c.Store.GetSession(ctx, in.SessionID)
	if err != nil {
		return CompactResult{}, &CompactError{Kind: "store", Msg: "get session", Err: err}
	}

	// 1. Budget arithmetic. Force does not bypass the floor.
	bg, err := c.budgetOf(ctx, in)
	if err != nil {
		var be *BudgetError
		if errors.As(err, &be) {
			return c.tooSmall(ctx, in, fmt.Sprintf("%s: %d tokens, cap %d", be.What, be.Tokens, be.Cap))
		}
		return CompactResult{}, &CompactError{Kind: "budget", Msg: err.Error(), Err: err}
	}
	if bg.compressible < bg.floor {
		return c.tooSmall(ctx, in, fmt.Sprintf("compressible %d tokens, floor %d", bg.compressible, bg.floor))
	}

	// 2. A session whose summarizer keeps failing is left alone.
	if !in.Force && sess.CompactFails >= MaxCompactFails {
		const reason = "disabled after repeated failures"
		c.emit(ctx, in, map[string]any{"outcome": "disabled", "reason": reason, "compact_fails": sess.CompactFails})
		return CompactResult{Reason: reason}, nil
	}

	msgs, err := c.Store.ListMessages(ctx, in.SessionID, sess.SummaryMsgID, 0)
	if err != nil {
		return CompactResult{}, &CompactError{Kind: "store", Msg: "list messages", Err: err}
	}
	items := c.convertAll(ctx, in, msgs)
	history := sumTokens(items)

	// 3. Trigger and re-arm gates.
	if !in.Force {
		if history*100 <= bg.available*CompactionPct {
			return CompactResult{Reason: "below trigger", Before: history, After: history}, nil
		}
		if post, armed := rearmed(msgs, items, bg.available); !armed {
			return CompactResult{
				Reason: fmt.Sprintf("re-arm threshold not reached (post %d, history %d)", post, history),
				Before: history, After: history,
			}, nil
		}
	}

	// 4. Choose the cut, summarise everything before it, commit once.
	return c.rewrite(ctx, in, sess, msgs, items, history)
}

func (c *Compactor) tooSmall(ctx context.Context, in CompactInput, msg string) (CompactResult, error) {
	err := &CompactError{Kind: "window_too_small", Msg: msg}
	c.emit(ctx, in, map[string]any{"outcome": "failed", "reason": err.Kind, "detail": msg})
	return CompactResult{Reason: err.Kind}, err
}

// convertAll budgets the transcript exactly as the builder does. Images are
// counted by their stub and derived text: the compactor has no image loader,
// so a vision prompt is charged slightly less here than at build time.
func (c *Compactor) convertAll(ctx context.Context, in CompactInput, msgs []event.Message) []built {
	bin := BuildInput{Def: in.Def, Model: in.Model, Tools: in.Tools}
	items := make([]built, 0, len(msgs))
	for _, m := range msgs {
		items = append(items, convert(ctx, bin, m))
	}
	return items
}

func sumTokens(items []built) int {
	n := 0
	for _, it := range items {
		n += it.tokens
	}
	return n
}

// rearmed answers "has the transcript grown enough since the last commit?"
// without a new DB column.
//
// Derivation: a committed summary is a RoleSystem message at the head of the
// transcript window (ListMessages(after=SummaryMsgID) returns it first,
// because its id is the last summarised position + 1). Its *id* carries the
// timestamp of the message it follows — that is what makes it sort there —
// but its CreatedAt is the commit instant. So the post-compaction size is the
// summary plus every message after it that already existed at that instant:
// those with CreatedAt not after the summary's CreatedAt. Everything with a
// later CreatedAt is growth since the commit.
//
// With no summary at the head (never compacted) the session is armed.
func rearmed(msgs []event.Message, items []built, available int) (post int, armed bool) {
	if len(msgs) == 0 || msgs[0].Role != event.RoleSystem {
		return 0, true
	}
	commit := msgs[0].CreatedAt
	for i, m := range msgs {
		if i > 0 && m.CreatedAt.After(commit) {
			continue
		}
		post += items[i].tokens
	}
	return post, sumTokens(items) >= post+available*RearmPct/100
}

// rewrite is stage 1, stage 2 and the commit path for one attempt.
func (c *Compactor) rewrite(ctx context.Context, in CompactInput, sess store.Session,
	msgs []event.Message, items []built, history int) (CompactResult, error) {

	cut, summaryID, ok := chooseCut(msgs, items)
	if !ok {
		err := &CompactError{Kind: "no_cut_point", Msg: "no boundary before a user message leaves a summarisable prefix"}
		c.emit(ctx, in, map[string]any{"outcome": "failed", "reason": err.Kind, "before": history})
		return CompactResult{Reason: err.Kind, Before: history, After: history}, err
	}

	// Stage 1 is applied to the prefix that is about to be summarised. Stored
	// rows are immutable: this output is what the model reads, it never
	// rewrites the transcript. (The same function runs at ingest, where it
	// does shape what gets stored.)
	prefix := Mechanical(ctx, msgs[:cut], MechanicalOpts{Spill: c.Blobs})

	summary, usage, err := c.summarize(ctx, in, prefix)
	if err != nil {
		return c.failed(ctx, in, sess, history, usage, err)
	}
	anchors := harvestAnchors(prefix)
	// The skill note reads the stored rows, not stage 1: a deduplicated or
	// capped result must still name the skill it loaded.
	body := summary + anchorBlock(anchors) + skillNote(msgs[:cut]) + recallNote

	now := c.now()
	sm := event.Message{
		Schema:    event.SchemaVersion,
		ID:        summaryID,
		Pos:       summaryID, // between the summarised prefix and the kept tail
		SessionID: in.SessionID,
		TraceID:   in.TraceID,
		// RoleSystem: the builder replays it as a user message prefixed with
		// SummaryPrefix, so the body must not carry that prefix itself.
		Role: event.RoleSystem,
		Text: body,
		// A summary of untrusted content is untrusted: the builder wraps it in
		// <untrusted> (the "[summary] " prefix stays outside the span), so
		// compaction cannot launder it into trusted context (docs/security.md).
		Tainted: anyTainted(msgs[:cut]),
		// The highest score of what it covers, so an injected page still arms
		// the taint after compaction and a browsed price still does not.
		TaintScore: maxTaintScore(msgs[:cut]),
		CreatedAt:  now.UTC(),
	}

	// Minimum reclaim, measured on what the next prompt will actually cost:
	// the summary plus the kept rows (stage 1 did not shrink those).
	after := convert(ctx, BuildInput{Def: in.Def, Model: in.Model}, sm).tokens + sumTokens(items[cut:])
	if reclaim := history - after; reclaim*100 < history*MinReclaimPct {
		err := &CompactError{Kind: "ineffective", Msg: fmt.Sprintf("reclaimed %d of %d tokens", reclaim, history)}
		return c.failed(ctx, in, sess, history, usage, err)
	}
	return c.commit(ctx, in, sess, sm, msgs, cut, history, after, len(anchors), usage)
}

// commit is the single path that rewrites a prefix: append the summary, move
// the session's compaction point, clear the ineffectiveness counter, emit the
// trace event.
func (c *Compactor) commit(ctx context.Context, in CompactInput, sess store.Session, sm event.Message,
	msgs []event.Message, cut, before, after, anchors int, usage event.Usage) (CompactResult, error) {

	if _, err := c.Store.AppendMessage(ctx, sm, false); err != nil {
		return c.storeFailed(ctx, in, before, usage, &CompactError{Kind: "store", Msg: "append summary", Err: err})
	}
	// The session's compaction point is the id of the LAST SUMMARISED message,
	// not the summary's own id: ListMessages(after=…) returns what is placed
	// strictly after the cursor's position, and the summary's position is
	// exactly one above it. So the next build sees the summary first, then
	// the kept tail (docs/context.md).
	cursor := msgs[cut-1].ID
	if err := c.Store.UpdateSessionContext(ctx, in.SessionID, sess.PrefixHash, cursor, 0); err != nil {
		return c.storeFailed(ctx, in, before, usage, &CompactError{Kind: "store", Msg: "update session", Err: err})
	}
	attrs := map[string]any{
		"outcome":             "committed",
		"before":              before,
		"after":               after,
		"reclaimed":           before - after,
		"summarised_messages": cut,
		"summary_msg_id":      string(sm.ID),
		"compaction_point":    string(cursor),
		"anchors":             anchors,
	}
	event.PutUsage(attrs, usage)
	c.emit(ctx, in, attrs)
	if c.OnSummary != nil {
		c.OnSummary(ctx, sess, sm.Text, sm.Tainted)
	}
	return CompactResult{Compacted: true, Reason: "committed", Before: before, After: after, SummaryMsgID: sm.ID}, nil
}

// failed persists the ineffectiveness counter and reports the failure. Nothing
// is appended and the compaction point does not move. It only ever follows the
// summarize call, so its event carries that call's usage like a commit's
// does — none when the call never got an answer — because an ineffective
// summary was paid for all the same.
func (c *Compactor) failed(ctx context.Context, in CompactInput, sess store.Session, history int, usage event.Usage, ce *CompactError) (CompactResult, error) {
	outcome := "failed"
	if ce.Kind == "ineffective" {
		outcome = "ineffective"
	}
	fails := sess.CompactFails + 1
	if err := c.Store.UpdateSessionContext(ctx, in.SessionID, sess.PrefixHash, sess.SummaryMsgID, fails); err != nil {
		ce = &CompactError{Kind: ce.Kind, Msg: ce.Msg + " (and persisting the counter failed: " + err.Error() + ")", Err: ce.Err}
	}
	attrs := map[string]any{
		"outcome": outcome, "reason": ce.Kind, "detail": ce.Msg,
		"before": history, "compact_fails": fails,
	}
	event.PutUsage(attrs, usage)
	c.emit(ctx, in, attrs)
	return CompactResult{Reason: ce.Kind, Before: history, After: history}, ce
}

// storeFailed reports a commit the store refused. The counter is left alone
// (the summary was good), but the summarize call was paid for, so the event
// still carries its usage.
func (c *Compactor) storeFailed(ctx context.Context, in CompactInput, before int, usage event.Usage, ce *CompactError) (CompactResult, error) {
	attrs := map[string]any{"outcome": "failed", "reason": ce.Kind, "detail": ce.Msg, "before": before}
	event.PutUsage(attrs, usage)
	c.emit(ctx, in, attrs)
	return CompactResult{Before: before, After: before}, ce
}
