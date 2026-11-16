package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"holaclaw/internal/event"
	"holaclaw/internal/memory"
	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

// The memory tools are the safe tier (docs/security.md). Everything the store
// records as provenance — agent, partition, user, role, session, trace, time —
// is taken from the Call, which the daemon fills from trusted state: the model
// supplies text only, and extra JSON fields in the arguments are ignored. The
// partition rule itself lives in the store, not here.

// Caps on what one search result may spend of the prompt.
const (
	maxMemoryLineRunes = 240
	maxMemoryResult    = 1500
)

// recallBanner marks the block as memory that is already stored, so the model
// does not save it back (docs/memory.md: never re-ingest recalled content).
const recallBanner = "(recalled memory — do not save again)"

// budgetHint is what the model is told when the notes budget is exhausted.
const budgetHint = "memory is full: merge or forget notes first (memory.search, memory.forget)"

// RegisterMemory adds memory.save, memory.search and memory.forget. sink may
// be nil; it receives the memory.saved and memory.forgotten trace events.
func RegisterMemory(r *tool.Registry, s memory.Store, sink event.Sink) error {
	if s == nil {
		return errors.New("tools: RegisterMemory needs a memory store")
	}
	if sink == nil {
		sink = event.Discard
	}
	for _, t := range []tool.Tool{
		&memorySave{mem: s, sink: sink},
		&memorySearch{mem: s},
		&memoryForget{mem: s, sink: sink},
	} {
		if err := r.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// provenanceOf builds the provenance from the trusted call envelope only.
func provenanceOf(c tool.Call) memory.Provenance {
	return memory.Provenance{
		Agent:     c.Agent,
		Partition: c.Partition,
		UserID:    c.UserID,
		Role:      c.Role,
		SessionID: c.SessionID,
		TraceID:   c.TraceID,
	}
}

// noTaskMemory is the error every memory tool returns in a task run: tasks
// have no memory, so the loop never offers these tools there (docs/memory.md §7).
const noTaskMemory = "tasks have no memory"

// privileged reports whether the sender may create shared notes.
func privileged(role string) bool { return role == "owner" || role == "admin" }

// memorySave stores one note.
type memorySave struct {
	mem  memory.Store
	sink event.Sink
}

func (*memorySave) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "memory.save",
		Description: "Remember one short fact for later conversations. One fact per note.",
		Parameters: object{
			Properties: map[string]prop{
				"text":       {Type: tString},
				"about":      {Type: tString},
				"visibility": {Type: tString, Enum: []string{"shared", "origin"}},
			},
			Required: []string{"text"},
		}.raw(),
	}
}

func (*memorySave) Risk() tool.Risk { return tool.RiskWrite }

func (t *memorySave) Run(ctx context.Context, c tool.Call) (tool.Result, error) {
	var a struct {
		Text       string `json:"text"`
		About      string `json:"about"`
		Visibility string `json:"visibility"`
	}
	if err := decodeArgs(c.Args, &a); err != nil {
		return errf("bad arguments: %v", err), nil
	}
	if strings.TrimSpace(a.Text) == "" {
		return errf("text is required"), nil
	}
	if c.RunKind == tool.RunTask {
		return errf("%s", noTaskMemory), nil
	}
	if c.Role == "guest" {
		return errf("guests cannot save notes"), nil
	}
	vis, ok := visibilityFor(c, a.Visibility)
	if !ok {
		return errf("visibility must be \"shared\" or \"origin\""), nil
	}
	id, err := t.mem.Write(ctx, memory.Item{
		Layer:      memory.LayerNote,
		Text:       strings.TrimSpace(a.Text),
		About:      strings.TrimSpace(a.About),
		Visibility: vis,
		Prov:       provenanceOf(c),
	})
	switch {
	case errors.Is(err, memory.ErrBudget):
		return errf("%s", budgetHint), nil
	case err != nil:
		return errf("memory.save failed: %v", err), nil
	}
	t.sink.Emit(ctx, event.New(event.MemorySaved, c.TraceID, c.SessionID, "", map[string]string{
		"memory_id":  string(id),
		"agent":      c.Agent,
		"layer":      string(memory.LayerNote),
		"visibility": string(vis),
	}))
	return tool.Result{Text: fmt.Sprintf("saved (%s)", vis)}, nil
}

// visibilityFor applies the docs/memory.md defaults: owner and admin writing in a DM
// share by default, everyone else keeps the note in its own partition, and a
// model-supplied "shared" is honoured only for owner and admin — silently
// downgraded otherwise, because the model must not widen its own reach.
func visibilityFor(c tool.Call, requested string) (memory.Visibility, bool) {
	switch strings.TrimSpace(strings.ToLower(requested)) {
	case "shared":
		if privileged(c.Role) {
			return memory.VisShared, true
		}
		return memory.VisOrigin, true
	case "origin":
		return memory.VisOrigin, true
	case "":
		if privileged(c.Role) && c.Origin.ConversationKind == event.ConversationDM {
			return memory.VisShared, true
		}
		return memory.VisOrigin, true
	default:
		return "", false
	}
}

// memorySearch reads back what is visible from this conversation.
type memorySearch struct{ mem memory.Store }

func (*memorySearch) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "memory.search",
		Description: "Look up what you remember. Returns a few short lines with their ids.",
		Parameters: object{
			Properties: map[string]prop{"query": {Type: tString}, "about": {Type: tString}},
			Required:   []string{"query"},
		}.raw(),
	}
}

func (*memorySearch) Risk() tool.Risk { return tool.RiskRead }

func (t *memorySearch) Run(ctx context.Context, c tool.Call) (tool.Result, error) {
	var a struct {
		Query string `json:"query"`
		About string `json:"about"`
	}
	if err := decodeArgs(c.Args, &a); err != nil {
		return errf("bad arguments: %v", err), nil
	}
	if c.RunKind == tool.RunTask {
		return errf("%s", noTaskMemory), nil
	}
	hits, err := t.mem.Search(ctx, memory.Query{
		Agent:     c.Agent,
		Partition: c.Partition,
		Text:      a.Query,
		About:     strings.TrimSpace(a.About),
		TraceID:   c.TraceID,
		SessionID: c.SessionID,
	})
	if err != nil {
		return errf("memory.search failed: %v", err), nil
	}
	if len(hits) == 0 {
		return tool.Result{Text: "no memories found"}, nil
	}
	var b strings.Builder
	b.WriteString(recallBanner)
	for _, h := range hits {
		b.WriteString("\n")
		b.WriteString(memoryLine(h.Item))
	}
	return tool.Result{Text: capText(b.String(), maxMemoryResult)}, nil
}

// memoryLine renders one hit: id, date, visibility, text.
func memoryLine(it memory.Item) string {
	text := strings.Join(strings.Fields(it.Text), " ")
	if r := []rune(text); len(r) > maxMemoryLineRunes {
		text = string(r[:maxMemoryLineRunes]) + "…"
	}
	return fmt.Sprintf("[%s] (%s, %s) %s", it.ID,
		it.Prov.Time.UTC().Format("2006-01-02"), it.Visibility, text)
}

// memoryForget deletes one item, if it is visible from this conversation.
type memoryForget struct {
	mem  memory.Store
	sink event.Sink
}

func (*memoryForget) Spec() provider.ToolSpec {
	return provider.ToolSpec{
		Name:        "memory.forget",
		Description: "Delete one remembered item by the id memory.search returned.",
		Parameters: object{
			Properties: map[string]prop{"id": {Type: tString}},
			Required:   []string{"id"},
		}.raw(),
	}
}

func (*memoryForget) Risk() tool.Risk { return tool.RiskWrite }

func (t *memoryForget) Run(ctx context.Context, c tool.Call) (tool.Result, error) {
	var a struct {
		ID string `json:"id"`
	}
	if err := decodeArgs(c.Args, &a); err != nil {
		return errf("bad arguments: %v", err), nil
	}
	id := strings.TrimSpace(a.ID)
	if id == "" {
		return errf("id is required"), nil
	}
	if c.RunKind == tool.RunTask {
		return errf("%s", noTaskMemory), nil
	}
	if c.Role == "guest" {
		return errf("guests cannot change memory"), nil
	}
	switch err := t.mem.Forget(ctx, c.Agent, c.Partition, event.ID(id)); {
	case errors.Is(err, memory.ErrNotFound):
		return errf("no memory %s here", id), nil
	case err != nil:
		return errf("memory.forget failed: %v", err), nil
	}
	t.sink.Emit(ctx, event.New(event.MemoryForgotten, c.TraceID, c.SessionID, "", map[string]string{
		"memory_id": id,
		"agent":     c.Agent,
		"partition": c.Partition,
	}))
	return tool.Result{Text: "forgotten " + id}, nil
}
