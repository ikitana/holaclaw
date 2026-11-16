package tool

import (
	"context"
	"fmt"

	"holaclaw/internal/provider"
)

// Merge joins several executors into one: the registry's selection for an
// agent and one executor per granted connector (docs/connectors.md §5), so the
// policy gate wraps a single Executor as before. Each tool name has exactly
// one owner. Two executors offering the same name is an error, never a silent
// shadow: connector tools are prefixed `<id>.` and an id that collides with a
// built-in group is refused by config validation, so a duplicate is a bug.
//
// Every executor must implement Lookuper, and Execute dispatches only a name
// its owner can look up: the gate answers a name it cannot look up as unknown
// without running it, so a tool without metadata never runs, with or without
// policy.
//
// The specs are read once, here, and the returned Executor, which also
// implements Lookuper, keeps that snapshot. A caller that must see a refreshed
// connector list merges again (docs/connectors.md §7).
func Merge(execs ...Executor) (Executor, error) {
	m := &multi{owner: map[string]Lookuper{}, exec: map[string]Executor{}}
	for i, e := range execs {
		lk, ok := e.(Lookuper)
		if !ok {
			return nil, fmt.Errorf("tool: merge: executor %d has no tool metadata", i)
		}
		for _, s := range e.Specs() {
			if _, dup := m.exec[s.Name]; dup {
				return nil, fmt.Errorf("tool: merge: %s is offered twice", s.Name)
			}
			m.owner[s.Name], m.exec[s.Name] = lk, e
			m.specs = append(m.specs, s)
		}
	}
	return m, nil
}

type multi struct {
	owner map[string]Lookuper
	exec  map[string]Executor
	specs []provider.ToolSpec // in argument order, each executor's own order kept
}

func (m *multi) Specs() []provider.ToolSpec { return m.specs }

func (m *multi) Lookup(name string) (Tool, bool) {
	lk, ok := m.owner[name]
	if !ok {
		return nil, false
	}
	return lk.Lookup(name)
}

func (m *multi) Execute(ctx context.Context, c Call) (Result, error) {
	if _, ok := m.Lookup(c.Name); !ok {
		return Result{Text: fmt.Sprintf("unknown tool %q", c.Name), IsError: true}, nil
	}
	return m.exec[c.Name].Execute(ctx, c)
}
