package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"holaclaw/internal/event"
)

// Enumerating the traces of a session is not part of the Store contract: the
// agent path never needs it — a runner writes into the trace it already has —
// and a backend that cannot answer it still stores every event. It lives here
// as an optional interface instead, the way memory.PartitionLister does, so
// the one caller that needs it (a person browsing traces through the Control
// API) asks with a type assertion and is told `not implemented` when the
// backend cannot. Nothing is disclosed that getTrace would not already show
// the same principal: the summary counts a session's own events, and the
// preview is the user's own message.

// tracePreviewRunes is how much of the opening message a row carries. A list
// row is one line wide; the rest of it is one keystroke away in the trace.
const tracePreviewRunes = 120

// TraceSummary is one trace of a session as a list row.
type TraceSummary struct {
	TraceID   event.ID
	SessionID event.ID
	StartedAt time.Time // first event
	EndedAt   time.Time // last event
	Events    int
	ToolCalls int
	LastType  event.Type // type of the last event, which is how a running trace shows
	Preview   string     // first user message of the trace, clipped; may be empty
	// Usage is what the trace's model calls cost, summed under the one
	// accounting rule (event.UsageTypes).
	Usage event.Usage
}

// TraceLister is implemented by backends that can enumerate the traces of one
// session; the API answers 501 without it.
type TraceLister interface {
	ListTraces(ctx context.Context, sessionID event.ID) ([]TraceSummary, error)
}

// ListTraces summarises every trace of one session, newest first. It is one
// grouped pass over the session's events (the events_session index covers the
// lookup) plus one pass over its user messages for the previews, rather than
// a query per trace: a busy session holds thousands of events and a browser
// asks for this on every keystroke that moves the cursor.
func (s *SQLite) ListTraces(ctx context.Context, sessionID event.ID) ([]TraceSummary, error) {
	if sessionID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT trace_id, MIN(time), MAX(time), COUNT(*),
		       COUNT(*) FILTER (WHERE type = ?), `+usageSums+`,
		       (SELECT type FROM events AS last
		         WHERE last.session_id = e.session_id AND last.trace_id = e.trace_id
		         ORDER BY last.id DESC LIMIT 1)
		  FROM events AS e
		 WHERE e.session_id = ?
		 GROUP BY trace_id
		 ORDER BY MIN(time) DESC, trace_id DESC`,
		string(event.ToolCallEvent), string(sessionID))
	if err != nil {
		return nil, fmt.Errorf("store: list traces: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []TraceSummary
	for rows.Next() {
		var (
			traceID, lastType string
			start, end        int64
			events, tools     int
			u                 event.Usage
		)
		dest := append([]any{&traceID, &start, &end, &events, &tools}, usageDest(&u)...)
		if err := rows.Scan(append(dest, &lastType)...); err != nil {
			return nil, fmt.Errorf("store: list traces: %w", err)
		}
		out = append(out, TraceSummary{
			TraceID:   event.ID(traceID),
			SessionID: sessionID,
			StartedAt: fromMS(start),
			EndedAt:   fromMS(end),
			Events:    events,
			ToolCalls: tools,
			LastType:  event.Type(lastType),
			Usage:     u,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list traces: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	previews, err := s.tracePreviews(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Preview = previews[out[i].TraceID]
	}
	return out, nil
}

// tracePreviews reads the first user message of every trace of the session.
// Only `user` rows are read: a page a tool fetched is content the agent was
// handed, and it has no business being quoted in a list of what somebody
// asked for.
func (s *SQLite) tracePreviews(ctx context.Context, sessionID event.ID) (map[event.ID]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT trace_id, text FROM messages
		 WHERE session_id = ? AND role = 'user' AND text <> ''
		 ORDER BY id`, string(sessionID))
	if err != nil {
		return nil, fmt.Errorf("store: trace previews: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[event.ID]string{}
	for rows.Next() {
		var traceID, text string
		if err := rows.Scan(&traceID, &text); err != nil {
			return nil, fmt.Errorf("store: trace previews: %w", err)
		}
		if traceID == "" {
			continue
		}
		// Ordered by id, so the first row of a trace is the one that opened
		// it and every later one is left alone.
		if _, seen := out[event.ID(traceID)]; !seen {
			out[event.ID(traceID)] = clipRunes(oneLineText(text), tracePreviewRunes)
		}
	}
	return out, rows.Err()
}

// oneLineText flattens a message for a row: a preview is one line wide, and a
// newline in it would break whatever is drawing the list.
func oneLineText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// usageSums is event.UsageOf summed in SQL: six aggregate columns — prompt
// tokens, completion tokens, cost, cached tokens, reasoning tokens, unpriced
// calls — over the rows of the group whose type is in event.UsageTypes, in
// the order usageDest scans them. The tokens are TOTALs (zero when none);
// cost and the details are SUMs, NULL when no event reported one, so "not
// reported" survives the sum. A completion count missing from an event falls
// back to the browser's old output_tokens name, a missing cost_nanousd to the
// old float cost; a value that is not a JSON number, and an attrs object that
// is not JSON, count as not reported rather than failing the query, as they
// do in Go. The type list is built from the
// Go one, so the two cannot name different events; a test holds the sums to
// the Go helper on the same events.
var usageSums = func() string {
	names := make([]string, 0, len(event.UsageTypes))
	for _, t := range event.UsageTypes {
		// Our own constants, never input: quoted here rather than bound, so
		// the fragment can sit in any query without shifting its arguments.
		names = append(names, "'"+strings.ReplaceAll(string(t), "'", "''")+"'")
	}
	counted := "type IN (" + strings.Join(names, ", ") + ")"
	// typed is the attribute when its JSON type is one of types, NULL
	// otherwise; num is any number. The CASEs nest because json_type fails on
	// an attrs object that is not JSON.
	typed := func(name, types string) string {
		return "(CASE WHEN json_valid(attrs) THEN CASE WHEN json_type(attrs, '$." + name +
			"') IN (" + types + ") THEN json_extract(attrs, '$." + name + "') END END)"
	}
	num := func(name string) string { return typed(name, "'integer', 'real'") }
	// cost is in nano-USD and an integer, so its SUM is exact: cost_nanousd,
	// else the legacy USD float rounded to the nano-USD (event.UsageOf).
	legacy := num(event.AttrLegacyCost)
	cost := "COALESCE(" + typed(event.AttrCostNanoUSD, "'integer'") +
		", CASE WHEN " + legacy + " >= 0 THEN CAST(ROUND(" + legacy + " * 1e9) AS INTEGER) END)"
	prompt, unpriced := num(event.AttrPromptTokens), num(event.AttrUnpricedCalls)
	return fmt.Sprintf(`CAST(TOTAL(%s) FILTER (WHERE %s) AS INTEGER),
		       CAST(TOTAL(COALESCE(%s, %s)) FILTER (WHERE %s) AS INTEGER),
		       SUM(%s) FILTER (WHERE %s),
		       CAST(SUM(%s) FILTER (WHERE %s) AS INTEGER),
		       CAST(SUM(%s) FILTER (WHERE %s) AS INTEGER),
		       CAST(TOTAL(CASE WHEN %s IS NOT NULL THEN %s
		                       WHEN %s IS NOT NULL AND %s IS NULL THEN 1 ELSE 0 END) FILTER (WHERE %s) AS INTEGER)`,
		prompt, counted,
		num(event.AttrCompletionTokens), num(event.AttrOutputTokens), counted,
		cost, counted,
		num(event.AttrCachedTokens), counted,
		num(event.AttrReasoningTokens), counted,
		unpriced, unpriced, prompt, cost, counted)
}()

// usageDest is where a row scans the usageSums columns into u.
func usageDest(u *event.Usage) []any {
	return []any{&u.PromptTokens, &u.CompletionTokens, &u.Cost, &u.CachedTokens, &u.ReasoningTokens, &u.UnpricedCalls}
}

// UsageLister is implemented by backends that can total what a session's
// model calls cost. Like TraceLister it is optional: the API leaves the
// session totals out without it.
type UsageLister interface {
	// SessionUsage totals the given sessions — every session with events
	// when none is given — in one grouped pass. A session with no counted
	// events is absent from the map, which reads as zero.
	SessionUsage(ctx context.Context, ids ...event.ID) (map[event.ID]event.Usage, error)
}

// SessionUsage is one grouped pass over the events, keyed by session: the
// same rule, and the same SQL, as a trace summary's usage.
func (s *SQLite) SessionUsage(ctx context.Context, ids ...event.ID) (map[event.ID]event.Usage, error) {
	q := `SELECT session_id, ` + usageSums + ` FROM events WHERE session_id <> ''`
	args := make([]any, 0, len(ids))
	if len(ids) > 0 {
		q += ` AND session_id IN (?` + strings.Repeat(", ?", len(ids)-1) + `)`
		for _, id := range ids {
			args = append(args, string(id))
		}
	}
	rows, err := s.db.QueryContext(ctx, q+` GROUP BY session_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: session usage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[event.ID]event.Usage{}
	for rows.Next() {
		var (
			id string
			u  event.Usage
		)
		if err := rows.Scan(append([]any{&id}, usageDest(&u)...)...); err != nil {
			return nil, fmt.Errorf("store: session usage: %w", err)
		}
		if !u.IsZero() {
			out[event.ID(id)] = u
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: session usage: %w", err)
	}
	return out, nil
}
