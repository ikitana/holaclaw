package sqlite

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"
	"unicode"

	"holaclaw/internal/event"
	"holaclaw/internal/memory"
)

// Retrieval is deterministic and calls no chat model (docs/memory.md): FTS5
// BM25 candidates ∪ vector candidates, each normalised to 0..1, combined,
// multiplied by the recency decay and diversified with MMR down to the
// injection cap. The same query twice yields the same order.

// candidateK is how many candidates each channel contributes before merging.
const candidateK = 50

// maxFTSTerms bounds a sanitised query, so a pathological prompt cannot build
// a pathological MATCH expression.
const maxFTSTerms = 16

// ftsQuery turns arbitrary user text into a safe FTS5 MATCH expression: every
// non-word rune is a separator, so quotes, `*`, `^`, `:`, `-` and parentheses
// cannot reach the parser; the FTS5 keywords (AND, OR, NOT, NEAR) are dropped
// rather than honoured; each remaining term is double-quoted as a literal and
// the terms are OR-joined. It returns "" when nothing survives, which the
// caller turns into a recency-only listing.
func ftsQuery(text string) string {
	terms := make([]string, 0, maxFTSTerms)
	for _, w := range strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		switch strings.ToUpper(w) {
		case "AND", "OR", "NOT", "NEAR":
			continue
		}
		if r := []rune(w); len(r) > 64 {
			w = string(r[:64])
		}
		terms = append(terms, `"`+w+`"`)
		if len(terms) == maxFTSTerms {
			break
		}
	}
	return strings.Join(terms, " OR ")
}

// candidate is one row in flight through scoring.
type candidate struct {
	item memory.Item
	fts  float64 // -bm25: higher is better
	vec  float64 // cosine
	hasF bool
	hasV bool
}

// Search returns the ranked, diversified hits visible from q.Partition.
func (s *Store) Search(ctx context.Context, q memory.Query) ([]memory.Hit, error) {
	if err := checkScope(q.Agent, q.Partition); err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = s.opts.InjectionCap
	}
	cands := map[string]*candidate{}
	if err := s.ftsCandidates(ctx, q, cands); err != nil {
		return nil, err
	}
	// The vector channel embeds the query; a failure here is recorded for
	// Status and leaves an FTS-only result rather than an error.
	var qvec []float32
	if s.embeds() && strings.TrimSpace(q.Text) != "" {
		f := embedFor{trace: q.TraceID, session: q.SessionID, purpose: purposeQuery}
		if vs, err := s.embedTexts(ctx, []string{q.Text}, f); err == nil && len(vs) == 1 {
			qvec = vs[0]
		}
	}
	if len(qvec) > 0 {
		if err := s.vecCandidates(ctx, q, qvec, cands); err != nil {
			return nil, err
		}
	}
	if len(cands) == 0 {
		return nil, nil
	}
	return s.rank(ctx, cands, limit), nil
}

// scope builds the agent + partition + layer + about filter for a query,
// qualified with the table alias. The partition rule is not optional and
// there is no code path that omits it.
func scope(q memory.Query, alias string) (string, []any) {
	a := alias
	where := a + `agent = ? AND (` + a + `"partition" = ? OR (` + a + `layer = 'note' AND ` + a + `visibility = 'shared'))`
	args := []any{q.Agent, q.Partition}
	if len(q.Layers) > 0 {
		marks := make([]string, 0, len(q.Layers))
		for _, l := range q.Layers {
			marks = append(marks, "?")
			args = append(args, string(l))
		}
		where += ` AND ` + a + `layer IN (` + strings.Join(marks, ",") + `)`
	}
	if about := strings.TrimSpace(q.About); about != "" {
		where += ` AND lower(` + a + `about) = lower(?)`
		args = append(args, about)
	}
	return where, args
}

// ftsCandidates fills cands from the BM25 top-K, or from the most recent
// items when the query is empty after sanitising.
func (s *Store) ftsCandidates(ctx context.Context, q memory.Query, cands map[string]*candidate) error {
	match := ftsQuery(q.Text)
	var rows *sql.Rows
	var err error
	if match == "" {
		where, args := scope(q, "")
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+itemCols+`, 0 FROM memory_items WHERE `+where+
				` ORDER BY created_at DESC, id DESC LIMIT ?`,
			append(args, candidateK)...)
	} else {
		where, args := scope(q, "i.")
		rows, err = s.db.QueryContext(ctx,
			`SELECT `+prefixCols("i.")+`, bm25(memory_fts) AS score
			   FROM memory_fts JOIN memory_items i ON i.rowid = memory_fts.rowid
			  WHERE memory_fts MATCH ? AND `+where+
				` ORDER BY score LIMIT ?`,
			append(append([]any{match}, args...), candidateK)...)
	}
	if err != nil {
		return err
	}
	defer rows.Close() //nolint:errcheck // read-only
	for rows.Next() {
		it, score, err := scanScored(rows)
		if err != nil {
			return err
		}
		c := &candidate{item: it, hasF: true}
		if match != "" {
			c.fts = -score // bm25 is negative; smaller is better
		}
		cands[string(it.ID)] = c
	}
	return rows.Err()
}

// vecCandidates scores every visible vector of the current model in Go
// (brute-force cosine, no CGO) and keeps the best.
func (s *Store) vecCandidates(ctx context.Context, q memory.Query, qvec []float32, cands map[string]*candidate) error {
	where, args := scope(q, "i.")
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+prefixCols("i.")+`, 0 FROM memory_items i
		   JOIN memory_vectors v ON v.item_id = i.id
		  WHERE v.model = ? AND `+where,
		append([]any{s.opts.EmbedModel.ID}, args...)...)
	if err != nil {
		return err
	}
	defer rows.Close() //nolint:errcheck // read-only
	items := map[string]memory.Item{}
	var ids []string
	for rows.Next() {
		it, _, err := scanScored(rows)
		if err != nil {
			return err
		}
		ids = append(ids, string(it.ID))
		items[string(it.ID)] = it
	}
	if err := rows.Err(); err != nil {
		return err
	}
	vecs := s.loadVectors(ctx, ids)
	type scored struct {
		id  string
		sim float64
	}
	all := make([]scored, 0, len(vecs))
	for _, id := range ids {
		if v, ok := vecs[id]; ok {
			all = append(all, scored{id, memory.Cosine(qvec, v)})
		}
	}
	// Deterministic top-K: score first, id as the tie-breaker.
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].sim != all[j].sim {
			return all[i].sim > all[j].sim
		}
		return all[i].id < all[j].id
	})
	if len(all) > candidateK {
		all = all[:candidateK]
	}
	for _, sc := range all {
		c, ok := cands[sc.id]
		if !ok {
			c = &candidate{item: items[sc.id]}
			cands[sc.id] = c
		}
		c.vec, c.hasV = sc.sim, true
	}
	return nil
}

// rank normalises both channels, applies the recency decay and runs MMR.
func (s *Store) rank(ctx context.Context, cands map[string]*candidate, limit int) []memory.Hit {
	ids := make([]string, 0, len(cands))
	for id := range cands {
		ids = append(ids, id)
	}
	sort.Strings(ids) // stable input order: the same query twice ranks the same

	ftsRaw := make([]float64, len(ids))
	vecRaw := make([]float64, len(ids))
	hasVectors := false
	for i, id := range ids {
		c := cands[id]
		ftsRaw[i], vecRaw[i] = c.fts, c.vec
		if c.hasV {
			hasVectors = true
		}
	}
	ftsN, vecN := memory.Normalise(ftsRaw), memory.Normalise(vecRaw)

	vecs := s.loadVectors(ctx, ids)
	now := s.now()
	scored := make([]memory.Candidate, 0, len(ids))
	for i, id := range ids {
		c := cands[id]
		f, v := 0.0, 0.0
		if c.hasF {
			f = ftsN[i]
		}
		if c.hasV {
			v = vecN[i]
		}
		half := s.opts.HalfLife
		if c.item.Layer == memory.LayerNote {
			half *= memory.NoteDecaySlowdown
		}
		scored = append(scored, memory.Candidate{
			ID:    id,
			Score: memory.Combine(f, v, hasVectors, memory.Decay(now.Sub(c.item.Prov.Time), half)),
			Vec:   vecs[id],
			Words: memory.Words(c.item.Text),
		})
	}
	picked := memory.MMR(scored, memory.MMRLambda, limit)
	hits := make([]memory.Hit, 0, len(picked))
	for _, p := range picked {
		hits = append(hits, memory.Hit{Item: cands[p.ID].item, Score: p.Score})
	}
	return hits
}

// scanScored reads an item row followed by one score column.
func scanScored(rows *sql.Rows) (memory.Item, float64, error) {
	var (
		it                     memory.Item
		id, layer, vis         string
		agent, part, user      string
		role, session, traceID string
		created                int64
		tainted                int64
		score                  float64
	)
	if err := rows.Scan(&id, &layer, &it.Text, &it.About, &vis, &agent, &part,
		&user, &role, &session, &traceID, &created, &tainted, &score); err != nil {
		return memory.Item{}, 0, err
	}
	it.ID, it.Layer, it.Visibility = event.ID(id), memory.Layer(layer), memory.Visibility(vis)
	it.Tainted = tainted != 0
	it.Prov = memory.Provenance{
		Agent: agent, Partition: part, UserID: event.ID(user), Role: role,
		SessionID: event.ID(session), TraceID: event.ID(traceID),
		Time: time.UnixMilli(created).UTC(),
	}
	return it, score, nil
}

// prefixCols qualifies itemCols with a table alias.
func prefixCols(alias string) string {
	parts := strings.Split(itemCols, ", ")
	for i, p := range parts {
		parts[i] = alias + p
	}
	return strings.Join(parts, ", ")
}
