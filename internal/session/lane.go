package session

import (
	"context"

	"holaclaw/internal/store"
)

// Lane is a concurrency class, not a priority: interactive turns and task
// turns have separate caps per provider, so a saturated task lane can never
// starve a human (docs/sessions.md).
type Lane string

const (
	LaneInteractive Lane = "interactive"
	LaneTask        Lane = "task"
)

// Default caps per (provider, lane) when Deps.LaneCaps says nothing.
const (
	DefaultInteractiveCap = 4
	DefaultTaskCap        = 1
)

// LaneOf is the lane a session's turns run in.
func LaneOf(s store.Session) Lane {
	if s.Kind == store.SessionTask {
		return LaneTask
	}
	return LaneInteractive
}

func (m *Manager) cap(l Lane) int {
	if n, ok := m.d.LaneCaps[l]; ok && n > 0 {
		return n
	}
	if l == LaneTask {
		return DefaultTaskCap
	}
	return DefaultInteractiveCap
}

// sem returns the semaphore for one (provider, lane) pair, creating it once.
func (m *Manager) sem(prov string, l Lane) chan struct{} {
	key := prov + "\x00" + string(l)
	m.semMu.Lock()
	defer m.semMu.Unlock()
	s, ok := m.sems[key]
	if !ok {
		s = make(chan struct{}, m.cap(l))
		m.sems[key] = s
	}
	return s
}

// acquire waits for a slot in the session's lane. It honours ctx, so an abort
// releases a turn that is still queued.
func (m *Manager) acquire(ctx context.Context, prov string, l Lane) (func(), error) {
	s := m.sem(prov, l)
	select {
	case s <- struct{}{}:
		return func() { <-s }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (m *Manager) providerOf(s store.Session) string {
	if m.d.ProviderOf == nil {
		return ""
	}
	return m.d.ProviderOf(s)
}
