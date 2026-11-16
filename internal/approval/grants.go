package approval

import (
	"context"
	"fmt"

	"holaclaw/internal/event"
	"holaclaw/internal/tool"
)

const grantColumns = `id, agent, tool, pattern, scope, session_id, task_id, created_by, created_at`

// AddGrant persists g, filling ID and CreatedAt when empty.
func (s *Service) AddGrant(ctx context.Context, g tool.Grant) (tool.Grant, error) {
	if g.Agent == "" || g.Tool == "" {
		return tool.Grant{}, fmt.Errorf("approval: grant needs an agent and a tool")
	}
	switch g.Scope {
	case tool.GrantSession, tool.GrantAlways, tool.GrantTask:
	default:
		return tool.Grant{}, fmt.Errorf("approval: grant scope %q is not persistable", g.Scope)
	}
	if g.ID == "" {
		g.ID = event.NewID()
	}
	if g.CreatedAt.IsZero() {
		g.CreatedAt = s.now()
	}
	_, err := s.exec(ctx, `INSERT INTO grants (`+grantColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(g.ID), g.Agent, g.Tool, g.Pattern, string(g.Scope),
		string(g.SessionID), string(g.TaskID), string(g.CreatedBy), ms(g.CreatedAt))
	if err != nil {
		return tool.Grant{}, fmt.Errorf("approval: add grant: %w", err)
	}
	return g, nil
}

// ListGrants returns every grant of one agent, newest first.
func (s *Service) ListGrants(ctx context.Context, agent string) ([]tool.Grant, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+grantColumns+` FROM grants WHERE agent = ? ORDER BY created_at DESC, id DESC`, agent)
	if err != nil {
		return nil, fmt.Errorf("approval: list grants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []tool.Grant
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("approval: list grants: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("approval: list grants: %w", err)
	}
	return out, nil
}

// RevokeGrant deletes one grant. Revoking an unknown grant is not an error.
func (s *Service) RevokeGrant(ctx context.Context, id event.ID) error {
	if _, err := s.exec(ctx, `DELETE FROM grants WHERE id = ?`, string(id)); err != nil {
		return fmt.Errorf("approval: revoke grant: %w", err)
	}
	return nil
}

// MatchGrant reports whether a persisted grant covers this call. A grant
// matches when the tool name is equal, the scope reaches this call (always: any
// session of the agent; session: the same session; task: the same task) and the
// pattern covers the call's command. A query failure reports no match: grants
// only ever relax a decision, so failing closed means asking the human.
func (s *Service) MatchGrant(ctx context.Context, agent, toolName, sessionID, taskID, pattern string) (tool.Grant, bool) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+grantColumns+` FROM grants WHERE agent = ? AND tool = ? ORDER BY created_at DESC, id DESC`,
		agent, toolName)
	if err != nil {
		return tool.Grant{}, false
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return tool.Grant{}, false
		}
		if !scopeCovers(g, sessionID, taskID) {
			continue
		}
		if g.Pattern != "" && !tool.MatchCommand(g.Pattern, pattern) {
			continue
		}
		return g, true
	}
	return tool.Grant{}, false
}

func scopeCovers(g tool.Grant, sessionID, taskID string) bool {
	switch g.Scope {
	case tool.GrantAlways:
		return true
	case tool.GrantSession:
		return sessionID != "" && string(g.SessionID) == sessionID
	case tool.GrantTask:
		return taskID != "" && string(g.TaskID) == taskID
	}
	return false
}

func scanGrant(sc interface{ Scan(...any) error }) (tool.Grant, error) {
	var g tool.Grant
	var id, scope, session, task, by string
	var created int64
	if err := sc.Scan(&id, &g.Agent, &g.Tool, &g.Pattern, &scope, &session, &task, &by, &created); err != nil {
		return tool.Grant{}, err
	}
	g.ID, g.Scope = event.ID(id), tool.GrantScope(scope)
	g.SessionID, g.TaskID, g.CreatedBy = event.ID(session), event.ID(task), event.ID(by)
	g.CreatedAt = fromMS(created)
	return g, nil
}
