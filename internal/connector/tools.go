package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"holaclaw/internal/connector/mcp"
	"holaclaw/internal/provider"
	"holaclaw/internal/tool"
)

// maxResultBytes bounds the text of one connector result: mcp's own cap on
// a response, so a result mcp accepts is cut only by the notes flattening
// adds. The transcript cap and the spill to a blob come later, from
// compaction, as for every tool result.
const maxResultBytes = mcp.MaxResponseBytes

// emptySchema stands in for a missing or unusable input schema: the loop's
// validator and every provider need a JSON object.
var emptySchema = func() json.RawMessage {
	b, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{}})
	return b
}()

// Executor is the tool set of the connectors ids grants an agent
// (docs/connectors.md §5), also a tool.Lookuper. It is a live view of the
// cached tool lists: Specs and Lookup read them on every call, so the next
// turn and a new session see a refreshed list. A connector the manager does
// not know, or one without tools, contributes nothing.
func (m *Manager) Executor(ids []string) tool.Executor {
	ids = slices.Clone(ids)
	slices.Sort(ids)
	return &executor{manager: m, ids: slices.Compact(ids)}
}

type executor struct {
	manager *Manager
	ids     []string // granted, sorted
}

// Specs lists every granted connector's tools: ids sorted, each
// connector's tools sorted by name, so the prompt prefix stays byte-stable
// while nothing changes. CachedSpecs reads the same lists from cache rows.
func (e *executor) Specs() []provider.ToolSpec {
	var out []provider.ToolSpec
	for _, id := range e.ids {
		if listed, err := e.manager.Tools(id); err == nil {
			out = append(out, specsOf(id, listed)...)
		}
	}
	return out
}

// Lookup resolves "<id>.<tool>" for a granted id.
func (e *executor) Lookup(name string) (tool.Tool, bool) {
	id, server, ok := strings.Cut(name, ".")
	if !ok {
		return nil, false
	}
	if _, granted := slices.BinarySearch(e.ids, id); !granted {
		return nil, false
	}
	listed, err := e.manager.Tools(id)
	if err != nil {
		return nil, false
	}
	for _, t := range listed {
		if t.Name == server {
			return &connTool{manager: e.manager, id: id, tool: t}, true
		}
	}
	return nil, false
}

func (e *executor) Execute(ctx context.Context, c tool.Call) (tool.Result, error) {
	t, ok := e.Lookup(c.Name)
	if !ok {
		return tool.Result{Text: fmt.Sprintf("unknown tool %q", c.Name), IsError: true}, nil
	}
	return t.Run(ctx, c)
}

// connTool is one cached MCP tool as a tool.Tool named "<id>.<tool>". Its
// description and schema are the server's: it is never registered, so the
// registry's schema cap does not apply (docs/connectors.md §5).
type connTool struct {
	manager *Manager
	id      string
	tool    mcp.Tool
}

func (t *connTool) Spec() provider.ToolSpec { return toolSpec(t.id, t.tool) }

func (t *connTool) Risk() tool.Risk { return Risk(t.tool) }

// Risk is read for a tool the server marks readOnlyHint, else external
// (docs/connectors.md §6). destructiveHint does not change the class: it
// makes the call always ask, in AlwaysAsk.
func Risk(t mcp.Tool) tool.Risk {
	if t.ReadOnlyHint {
		return tool.RiskRead
	}
	return tool.RiskExternal
}

// AlwaysAsk is true only when the server set destructiveHint = true; an
// unset hint is plain external.
func AlwaysAsk(t mcp.Tool) bool { return t.DestructiveHint != nil && *t.DestructiveHint }

func (t *connTool) AlwaysAsk() bool { return AlwaysAsk(t.tool) }

// Trusted is the connector's `trusted`, read at every call.
func (t *connTool) Trusted() bool { return t.manager.trusted(t.id) }

func (t *connTool) Connector() string { return t.id }

// Run calls the tool. Every result is tainted, errors included. A server
// isError, a connector that refuses the call (expired, not connected) and a
// failed request are error results the model can read; only a cancelled
// ctx is an error.
func (t *connTool) Run(ctx context.Context, c tool.Call) (tool.Result, error) {
	res, err := t.manager.CallTool(ctx, t.id, t.tool.Name, c.Args)
	if err != nil {
		if ctx.Err() != nil {
			return tool.Result{}, ctx.Err()
		}
		return tool.Result{Text: callText(t.id, err), IsError: true, Tainted: true}, nil
	}
	return tool.Result{Text: capResult(res.Text), IsError: res.IsError, Tainted: true}, nil
}

// callText is the model's text for a failed call. StateError and CallError
// texts are the daemon's own words; anything else gets a generic one.
func callText(id string, err error) string {
	var se *StateError
	var ce *CallError
	switch {
	case errors.As(err, &se):
		return se.Error()
	case errors.As(err, &ce):
		return ce.Error()
	case errors.Is(err, ErrNotFound):
		return "connector " + id + " does not exist"
	}
	return "connector " + id + ": " + cause(err)
}

// capResult cuts text at maxResultBytes on a UTF-8 boundary, saying so.
func capResult(s string) string {
	if len(s) <= maxResultBytes {
		return s
	}
	cut := maxResultBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[result cut at %d bytes]", maxResultBytes)
}

// trusted is the connector's `trusted` key; false for an unknown id.
func (m *Manager) trusted(id string) bool {
	c, err := m.conn(id)
	if err != nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.Trusted && !c.deleted
}
