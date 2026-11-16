package sqlite

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"holaclaw/internal/event"
	"holaclaw/internal/provider"
)

// Embeddings are computed off the busy path: Write stores the item and
// enqueues it, a bounded worker embeds it, and a cache keyed by (provider,
// model, sha256(text)) makes a repeat free. A failing embedder never loses an
// item and never degrades silently — the item stays pending and Status
// reports the error (docs/memory.md).

// enqueue schedules an embedding. A full queue is not an error: the item
// keeps counting as pending and Reindex picks it up.
func (s *Store) enqueue(id event.ID) {
	if !s.embeds() {
		return
	}
	s.inflight.Add(1)
	select {
	case s.queue <- id:
	default:
		s.inflight.Done()
		s.mu.Lock()
		s.queueDrops++
		s.mu.Unlock()
	}
}

func (s *Store) setEmbedErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.embedErr = ""
		return
	}
	s.embedErr = err.Error()
}

func (s *Store) noteDims(n int) {
	s.mu.Lock()
	s.lastDims = n
	s.mu.Unlock()
}

// embedFor is where an embeddings call is recorded: the trace and session it
// was made for, and why ("item", "query", "reindex"). An empty trace gets one
// of its own, so a call made for no conversation is still in a trace.
type embedFor struct {
	trace, session event.ID
	purpose        string
}

// Purposes of an embeddings call, as memory.embedded reports them.
const (
	purposeItem    = "item"
	purposeQuery   = "query"
	purposeReindex = "reindex"
)

// embedItem embeds one stored item, through the cache. The call is recorded
// in the trace that wrote the item.
func (s *Store) embedItem(ctx context.Context, id event.ID) {
	var text, trace, session string
	err := s.db.QueryRowContext(ctx, `SELECT text, trace_id, session_id FROM memory_items WHERE id = ?`,
		string(id)).Scan(&text, &trace, &session)
	if err != nil {
		return // deleted meanwhile, or the database is gone; nothing to do
	}
	vecs, err := s.embedTexts(ctx, []string{text},
		embedFor{trace: event.ID(trace), session: event.ID(session), purpose: purposeItem})
	if err != nil || len(vecs) != 1 {
		return
	}
	s.storeVector(ctx, id, vecs[0])
}

// recordEmbed emits memory.embedded for one answered embeddings call, with
// the error kind when the answer was unusable.
func (s *Store) recordEmbed(ctx context.Context, f embedFor, inputs int, u provider.Usage, err error) {
	if s.opts.Events == nil {
		return
	}
	trace := f.trace
	if trace == "" {
		trace = event.NewID()
	}
	attrs := map[string]any{
		"model": s.opts.EmbedModel.Name, "provider": s.opts.EmbedModel.Provider,
		"purpose": f.purpose, "inputs": inputs,
	}
	if err != nil {
		attrs["error"] = errKind(err)
	}
	event.PutUsage(attrs, event.Call(u))
	s.opts.Events.Emit(ctx, event.New(event.MemoryEmbedded, trace, f.session, "", attrs))
}

// errKind is the provider error kind of err, never its text.
func errKind(err error) string {
	if k := provider.KindOf(err); k != "" {
		return string(k)
	}
	return "failed"
}

// embedTexts returns one vector per input, using the cache first and calling
// the embedder for the rest. An embedder failure is recorded for Status and
// returned; an answered call is recorded in f's trace.
func (s *Store) embedTexts(ctx context.Context, texts []string, f embedFor) ([][]float32, error) {
	if !s.embeds() {
		return nil, errors.New("memory: no embedder configured")
	}
	out := make([][]float32, len(texts))
	var missIdx []int
	var missText []string
	for i, t := range texts {
		if v, ok := s.cacheGet(ctx, t); ok {
			out[i] = v
			continue
		}
		missIdx = append(missIdx, i)
		missText = append(missText, t)
	}
	if len(missText) == 0 {
		s.setEmbedErr(nil)
		if len(out) > 0 {
			s.noteDims(len(out[0]))
		}
		return out, nil
	}
	vecs, usage, err := s.opts.Embedder.Embed(ctx, s.opts.EmbedModel, missText)
	// A failed call was answered only when its reply reported usage; one with
	// nothing reported never got a reply and is not recorded.
	answered := err == nil || usage.PromptTokens > 0 || usage.CompletionTokens > 0 || usage.Cost != nil
	if err == nil && len(vecs) != len(missText) {
		err = fmt.Errorf("memory: embedder returned %d vectors for %d inputs", len(vecs), len(missText))
	}
	if answered {
		s.recordEmbed(context.WithoutCancel(ctx), f, len(missText), usage, err)
	}
	if err != nil {
		s.setEmbedErr(err)
		return nil, err
	}
	s.setEmbedErr(nil)
	for k, i := range missIdx {
		out[i] = vecs[k]
		s.cachePut(ctx, missText[k], vecs[k])
	}
	if len(out) > 0 {
		s.noteDims(len(out[0]))
	}
	return out, nil
}

func (s *Store) cacheGet(ctx context.Context, text string) ([]float32, bool) {
	var blob []byte
	var dims int
	err := s.db.QueryRowContext(ctx,
		`SELECT dims, vec FROM embedding_cache WHERE provider = ? AND model = ? AND hash = ?`,
		s.opts.EmbedModel.Provider, s.opts.EmbedModel.ID, textHash(text)).Scan(&dims, &blob)
	if err != nil {
		return nil, false
	}
	v := decodeVec(blob)
	if len(v) != dims || len(v) == 0 {
		return nil, false
	}
	return v, true
}

func (s *Store) cachePut(ctx context.Context, text string, v []float32) {
	if len(v) == 0 {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO embedding_cache (provider, model, hash, dims, vec, created_at) VALUES (?,?,?,?,?,?)`,
		s.opts.EmbedModel.Provider, s.opts.EmbedModel.ID, textHash(text), len(v), encodeVec(v), s.now().UnixMilli())
}

func (s *Store) storeVector(ctx context.Context, id event.ID, v []float32) {
	if len(v) == 0 {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO memory_vectors (item_id, model, dims, vec) VALUES (?,?,?,?)`,
		string(id), s.opts.EmbedModel.ID, len(v), encodeVec(v))
}

// Reindex re-embeds every item of the agent that has no vector for the
// current embed model — which is what a model swap leaves behind — in
// batches.
func (s *Store) Reindex(ctx context.Context, agent string) error {
	if agent == "" {
		return &ProvenanceError{Field: "agent"}
	}
	if !s.embeds() {
		return nil
	}
	// A reindex serves no one conversation: its calls share one trace of
	// their own, so what it cost is in one place.
	f := embedFor{trace: event.NewID(), purpose: purposeReindex}
	for {
		rows, err := s.db.QueryContext(ctx,
			`SELECT i.id, i.text FROM memory_items i
			   LEFT JOIN memory_vectors v ON v.item_id = i.id AND v.model = ?
			  WHERE i.agent = ? AND v.item_id IS NULL
			  ORDER BY i.id LIMIT ?`,
			s.opts.EmbedModel.ID, agent, reindexBatch)
		if err != nil {
			return err
		}
		var ids []event.ID
		var texts []string
		for rows.Next() {
			var id, text string
			if err := rows.Scan(&id, &text); err != nil {
				rows.Close() //nolint:errcheck // read-only
				return err
			}
			ids = append(ids, event.ID(id))
			texts = append(texts, text)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		vecs, err := s.embedTexts(ctx, texts, f)
		if err != nil {
			return fmt.Errorf("memory: reindex: %w", err)
		}
		for i, id := range ids {
			s.storeVector(ctx, id, vecs[i])
		}
	}
}

// loadVectors reads the vectors of the current model for the given item ids.
func (s *Store) loadVectors(ctx context.Context, ids []string) map[string][]float32 {
	out := map[string][]float32{}
	if !s.embeds() || len(ids) == 0 {
		return out
	}
	q := `SELECT item_id, dims, vec FROM memory_vectors WHERE model = ? AND item_id IN (` +
		placeholders(len(ids)) + `)`
	args := make([]any, 0, len(ids)+1)
	args = append(args, s.opts.EmbedModel.ID)
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return out
	}
	defer rows.Close() //nolint:errcheck // read-only
	for rows.Next() {
		var id string
		var dims int
		var blob []byte
		if err := rows.Scan(&id, &dims, &blob); err != nil {
			return out
		}
		if v := decodeVec(blob); len(v) == dims && len(v) > 0 {
			out[id] = v
		}
	}
	return out
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, 0, n*2-1)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}

// encodeVec writes float32s little-endian, as the schema documents.
func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

// decodeVec is the inverse; a truncated blob yields nil rather than a panic.
func decodeVec(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}
