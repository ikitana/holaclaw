package approval

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/tool"
)

// approvalColumns leaves out kind: every approval is a tool call, and insert
// writes 'tool' (the column stays until the migrations are squashed).
const approvalColumns = `id, status, trace_id, session_id, agent, tool, summary, args,
	choice, resolved_by, created_at, expires_at, resolved_at`

// requestedAttrs is the attrs object of approval.requested. It never carries
// the raw arguments: only a capped excerpt, because arguments are model output
// and events are exported.
type requestedAttrs struct {
	ID        event.ID  `json:"id"`
	Tool      string    `json:"tool,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	SessionID event.ID  `json:"session_id,omitempty"`
	Args      string    `json:"args_excerpt,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// resolvedAttrs is the attrs object of approval.resolved.
type resolvedAttrs struct {
	ID         event.ID            `json:"id"`
	Tool       string              `json:"tool,omitempty"`
	Status     tool.ApprovalStatus `json:"status"`
	Choice     tool.GrantScope     `json:"choice,omitempty"`
	ResolvedBy event.ID            `json:"resolved_by,omitempty"`
	SessionID  event.ID            `json:"session_id,omitempty"`
}

// insert writes a pending approval, filling the identity and lifetime fields,
// capping the stored arguments, and emitting the request event.
func (s *Service) insert(ctx context.Context, a tool.Approval) (tool.Approval, error) {
	if a.ID == "" {
		a.ID = event.NewID()
	}
	a.Status = tool.ApprovalPending
	a.ResolvedAt, a.ResolvedBy, a.Choice = nil, "", ""
	if a.CreatedAt.IsZero() {
		a.CreatedAt = s.now()
	}
	if a.ExpiresAt.IsZero() {
		a.ExpiresAt = a.CreatedAt.Add(s.timeout)
	}
	a.Args = tool.Cap(a.Args, tool.MaxApprovalArgs)

	if _, err := s.exec(ctx, `INSERT INTO approvals (kind, `+approvalColumns+`)
		VALUES ('tool', ?, ?, ?, ?, ?, ?, ?, ?, '', '', ?, ?, NULL)`,
		string(a.ID), string(a.Status), string(a.TraceID), string(a.SessionID),
		a.Agent, a.Tool, a.Summary, a.Args, ms(a.CreatedAt), ms(a.ExpiresAt)); err != nil {
		return tool.Approval{}, fmt.Errorf("approval: insert: %w", err)
	}

	s.events.Emit(ctx, event.New(event.ApprovalRequested, a.TraceID, a.SessionID, "", requestedAttrs{
		ID: a.ID, Tool: a.Tool, Summary: a.Summary,
		SessionID: a.SessionID, Args: a.Args, ExpiresAt: a.ExpiresAt.UTC(),
	}))
	return a, nil
}

// Request implements tool.Approver: it records a pending approval and blocks
// until a human resolves it, it expires (timeout = deny) or ctx ends. Only the
// calling session suspends — the wait is on a per-approval channel and no lock
// is held meanwhile.
func (s *Service) Request(ctx context.Context, a tool.Approval) (tool.Approval, error) {
	if a.ID == "" {
		a.ID = event.NewID()
	}
	// Registering the waiter before the row exists closes the window where a
	// very fast Resolve would find nobody to wake.
	ch := make(chan tool.Approval, 1)
	s.mu.Lock()
	s.waiters[a.ID] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.waiters, a.ID)
		s.mu.Unlock()
	}()

	a, err := s.insert(ctx, a)
	if err != nil {
		return tool.Approval{}, err
	}

	// Measured against the injected clock so a test can shorten the wait.
	timer := time.NewTimer(a.ExpiresAt.Sub(s.now()))
	defer timer.Stop()
	select {
	case out := <-ch:
		return out, nil
	case <-timer.C:
		return s.close(context.WithoutCancel(ctx), a, tool.ApprovalExpired, "system:expired"), nil
	case <-ctx.Done():
		out := s.close(context.WithoutCancel(ctx), a, tool.ApprovalDenied, "system:cancelled")
		return out, ctx.Err()
	}
}

// close ends a pending approval without a human: expiry or a cancelled run. A
// decision that landed in the meantime wins, so the stored row is returned.
func (s *Service) close(ctx context.Context, a tool.Approval, status tool.ApprovalStatus, by event.ID) tool.Approval {
	now := s.now()
	res, err := s.exec(ctx, `UPDATE approvals SET status = ?, resolved_by = ?, resolved_at = ?
		WHERE id = ? AND status = 'pending'`, string(status), string(by), ms(now), string(a.ID))
	if err == nil {
		if n, _ := res.RowsAffected(); n == 0 {
			if stored, err := s.Get(ctx, a.ID); err == nil {
				return stored
			}
			return a
		}
	}
	a.Status, a.ResolvedBy, a.ResolvedAt = status, by, &now
	s.emitResolved(ctx, a)
	return a
}

// Resolve records a human decision. The UPDATE is the serialisation point: a
// second call on the same approval returns ErrNotPending and changes nothing.
// Approving with scope session or always also persists the grant that will
// answer the next identical call without asking.
func (s *Service) Resolve(ctx context.Context, id event.ID, approve bool, choice tool.GrantScope, by event.ID) (tool.Approval, error) {
	status := tool.ApprovalDenied
	if approve {
		status = tool.ApprovalApproved
	} else {
		choice = ""
	}
	now := s.now()
	res, err := s.exec(ctx, `UPDATE approvals SET status = ?, choice = ?, resolved_by = ?, resolved_at = ?
		WHERE id = ? AND status = 'pending'`,
		string(status), string(choice), string(by), ms(now), string(id))
	if err != nil {
		return tool.Approval{}, fmt.Errorf("approval: resolve: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.Get(ctx, id); err != nil {
			return tool.Approval{}, err
		}
		return tool.Approval{}, ErrNotPending
	}
	a, err := s.Get(ctx, id)
	if err != nil {
		return tool.Approval{}, err
	}

	if approve && (choice == tool.GrantSession || choice == tool.GrantAlways) {
		pattern := ""
		if s.grantPattern != nil {
			pattern = s.grantPattern(a)
		}
		g := tool.Grant{
			Agent: a.Agent, Tool: a.Tool, Pattern: pattern,
			Scope: choice, CreatedBy: by, CreatedAt: now,
		}
		if choice == tool.GrantSession {
			g.SessionID = a.SessionID
		}
		if _, err := s.AddGrant(ctx, g); err != nil {
			return tool.Approval{}, err
		}
	}

	// Trace first, then wake the blocked caller: the trace must show the
	// resolution before anything the resumed turn does.
	s.emitResolved(ctx, a)
	s.mu.Lock()
	ch, waiting := s.waiters[id]
	s.mu.Unlock()
	if waiting {
		select {
		case ch <- a:
		default:
		}
	}
	return a, nil
}

func (s *Service) emitResolved(ctx context.Context, a tool.Approval) {
	s.events.Emit(ctx, event.New(event.ApprovalResolved, a.TraceID, a.SessionID, "", resolvedAttrs{
		ID: a.ID, Tool: a.Tool, Status: a.Status,
		Choice: a.Choice, ResolvedBy: a.ResolvedBy, SessionID: a.SessionID,
	}))
}

// Get returns one approval by id.
func (s *Service) Get(ctx context.Context, id event.ID) (tool.Approval, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE id = ?`, string(id))
	a, err := scanApproval(row)
	if errors.Is(err, sql.ErrNoRows) {
		return tool.Approval{}, ErrNotFound
	}
	if err != nil {
		return tool.Approval{}, fmt.Errorf("approval: get: %w", err)
	}
	return a, nil
}

// ListPending returns every approval still waiting for a human, oldest first.
func (s *Service) ListPending(ctx context.Context) ([]tool.Approval, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+approvalColumns+` FROM approvals WHERE status = 'pending' ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("approval: list pending: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []tool.Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, fmt.Errorf("approval: list pending: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("approval: list pending: %w", err)
	}
	return out, nil
}

// ExpireStale is called at startup. Every pending approval lost its waiter when
// the process died, so it can never be answered: timeout = deny.
func (s *Service) ExpireStale(ctx context.Context) (int, error) {
	res, err := s.exec(ctx, `UPDATE approvals SET status = 'expired', resolved_by = 'system:restart',
		resolved_at = ? WHERE status = 'pending'`, ms(s.now()))
	if err != nil {
		return 0, fmt.Errorf("approval: expire stale: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func scanApproval(sc interface{ Scan(...any) error }) (tool.Approval, error) {
	var a tool.Approval
	var id, status, trace, session, choice, by string
	var created, expires int64
	var resolved sql.NullInt64
	if err := sc.Scan(&id, &status, &trace, &session, &a.Agent, &a.Tool, &a.Summary, &a.Args,
		&choice, &by, &created, &expires, &resolved); err != nil {
		return tool.Approval{}, err
	}
	a.ID, a.Status = event.ID(id), tool.ApprovalStatus(status)
	a.TraceID, a.SessionID = event.ID(trace), event.ID(session)
	a.Choice, a.ResolvedBy = tool.GrantScope(choice), event.ID(by)
	a.CreatedAt, a.ExpiresAt, a.ResolvedAt = fromMS(created), fromMS(expires), nullMS(resolved)
	return a, nil
}
