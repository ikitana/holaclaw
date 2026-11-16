// Package sqlite is the v1 memory backend (docs/memory.md): notes and
// recall in the shared SQLite database, FTS5 plus brute-force cosine over
// vectors kept in the same file, no CGO.
//
// Two rules are enforced here rather than by callers: provenance is written
// from trusted state only (Write refuses an item without agent and
// partition), and every read is partition-scoped by default-deny — an item is
// visible from its own partition, and additionally from any partition when it
// is a note marked shared. There is no option, method or query that lifts it.
package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/memory"
	"holaclaw/internal/provider"
	"holaclaw/internal/tokens"
)

// Defaults for Options.
const (
	DefaultNoteBudgetTokens = 2000
	DefaultInjectionCap     = 5
	DefaultHalfLife         = 30 * 24 * time.Hour
)

// embedQueueSize bounds the off-path embedding queue. A full queue drops the
// enqueue, never the item: the item simply stays pending until Reindex.
const embedQueueSize = 256

// reindexBatch is how many texts one Reindex embedding call carries.
const reindexBatch = 32

// ErrProvenance is returned when a write or query arrives without the
// mandatory provenance fields.
var ErrProvenance = memory.ErrProvenance

// ProvenanceError names the missing field. It matches ErrProvenance under
// errors.Is.
type ProvenanceError struct{ Field string }

func (e *ProvenanceError) Error() string { return "memory: missing " + e.Field }
func (e *ProvenanceError) Is(target error) bool {
	return target == ErrProvenance
}

// ErrTaskPartition refuses recall ingestion for a task run: task sessions are
// excluded from recall (docs/memory.md).
var ErrTaskPartition = memory.ErrTaskPartition

// Options configures the backend. An absent Embedder or EmbedModel means
// FTS-only retrieval; everything else falls back to the documented default.
type Options struct {
	// Embedder and EmbedModel are optional; both must be set for vectors.
	Embedder   provider.Embedder
	EmbedModel provider.Model
	// Events, optional, receives memory.embedded for every embeddings call
	// that was answered, so what embedding costs reaches the traces.
	Events event.Sink
	// NoteBudgetTokens caps the notes of one agent across all partitions.
	NoteBudgetTokens int
	// InjectionCap is the default Query.Limit.
	InjectionCap int
	// HalfLife is the recency decay half-life; notes decay slower.
	HalfLife time.Duration
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Store is the SQLite memory backend. Safe for concurrent use.
type Store struct {
	db   *sql.DB
	opts Options

	writeMu sync.Mutex // one writer per process, as in the main store

	queue    chan event.ID
	inflight sync.WaitGroup
	startOne sync.Once

	mu         sync.Mutex
	embedErr   string
	lastDims   int
	queueDrops int
}

var (
	_ memory.Store           = (*Store)(nil)
	_ memory.PartitionLister = (*Store)(nil)
)

// New builds a store over an already-migrated database handle.
func New(db *sql.DB, opts Options) *Store {
	if opts.NoteBudgetTokens <= 0 {
		opts.NoteBudgetTokens = DefaultNoteBudgetTokens
	}
	if opts.InjectionCap <= 0 {
		opts.InjectionCap = DefaultInjectionCap
	}
	if opts.HalfLife <= 0 {
		opts.HalfLife = DefaultHalfLife
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Store{db: db, opts: opts, queue: make(chan event.ID, embedQueueSize)}
}

// Start launches the bounded embedding worker. It is optional: without it
// embeddings are computed by Flush or Reindex. Calling it twice is a no-op.
func (s *Store) Start(ctx context.Context) {
	if !s.embeds() {
		return
	}
	s.startOne.Do(func() {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-s.queue:
					s.embedItem(ctx, id)
					s.inflight.Done()
				}
			}
		}()
	})
}

// Flush embeds everything enqueued so far and waits for the worker to finish
// what it already took. Tests use it instead of sleeping.
func (s *Store) Flush(ctx context.Context) error {
	if !s.embeds() {
		return nil
	}
	for {
		select {
		case id := <-s.queue:
			s.embedItem(ctx, id)
			s.inflight.Done()
			continue
		default:
		}
		break
	}
	done := make(chan struct{})
	go func() { s.inflight.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Store) embeds() bool { return s.opts.Embedder != nil && s.opts.EmbedModel.ID != "" }

func (s *Store) now() time.Time { return s.opts.Now().UTC() }

// Write stores an item and enqueues its embedding. Provenance comes from the
// caller's trusted state; nothing here is model-settable.
func (s *Store) Write(ctx context.Context, it memory.Item) (event.ID, error) {
	if err := checkProv(it.Prov); err != nil {
		return "", err
	}
	if strings.TrimSpace(it.Text) == "" {
		return "", errors.New("memory: empty text")
	}
	switch it.Layer {
	case memory.LayerNote, memory.LayerRecall:
	case "":
		it.Layer = memory.LayerNote
	default:
		return "", fmt.Errorf("memory: unknown layer %q", it.Layer)
	}
	vis := it.Visibility
	if it.Layer == memory.LayerRecall {
		vis = memory.VisOrigin // recall never crosses a partition
	} else if vis != memory.VisShared {
		vis = memory.VisOrigin
	}
	when := it.Prov.Time
	if when.IsZero() {
		when = s.now()
	}
	nTokens := tokens.Estimate(it.Text)
	hash := textHash(it.Text)

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if it.Layer == memory.LayerRecall {
		var existing string
		err := s.db.QueryRowContext(ctx,
			`SELECT id FROM memory_items WHERE agent = ? AND "partition" = ? AND layer = 'recall' AND source_hash = ?`,
			it.Prov.Agent, it.Prov.Partition, hash).Scan(&existing)
		switch {
		case err == nil:
			return event.ID(existing), nil // already ingested
		case errors.Is(err, sql.ErrNoRows):
		default:
			return "", err
		}
	} else if err := s.checkBudget(ctx, it.Prov.Agent, nTokens); err != nil {
		return "", err
	}

	id := event.NewIDAt(when)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_items
		   (id, agent, layer, text, about, visibility, "partition", user_id, role, session_id, trace_id, created_at, tokens, source_hash, tainted)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		string(id), it.Prov.Agent, string(it.Layer), it.Text, it.About, string(vis),
		it.Prov.Partition, string(it.Prov.UserID), it.Prov.Role, string(it.Prov.SessionID),
		string(it.Prov.TraceID), when.UnixMilli(), nTokens, hash, boolInt(it.Tainted))
	if err != nil {
		return "", fmt.Errorf("memory: write: %w", err)
	}
	s.enqueue(id)
	return id, nil
}

// checkBudget fails with memory.ErrBudget when one more note would push the
// agent over its budget, so the caller consolidates instead of growing the
// prompt.
func (s *Store) checkBudget(ctx context.Context, agent string, add int) error {
	var used int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(tokens),0) FROM memory_items WHERE agent = ? AND layer = 'note'`,
		agent).Scan(&used); err != nil {
		return err
	}
	if used+add > s.opts.NoteBudgetTokens {
		return fmt.Errorf("%w (%d + %d > %d tokens)", memory.ErrBudget, used, add, s.opts.NoteBudgetTokens)
	}
	return nil
}

// IngestRecall writes one recall entry, deduplicated by the hash of its text.
// The daemon calls it at session end and at compaction. Task sessions are
// excluded by the caller; the guard here is the second line of defence.
// tainted marks a summary of untrusted content: the flag travels with the
// entry so injecting it wraps it as untrusted again (docs/security.md).
func (s *Store) IngestRecall(ctx context.Context, prov memory.Provenance, summary string, tainted bool) (event.ID, error) {
	if err := checkProv(prov); err != nil {
		return "", err
	}
	if strings.HasPrefix(prov.Partition, "task/") {
		return "", ErrTaskPartition
	}
	return s.Write(ctx, memory.Item{
		Layer:      memory.LayerRecall,
		Text:       summary,
		Visibility: memory.VisOrigin,
		Tainted:    tainted,
		Prov:       prov,
	})
}

// Get returns one item, subject to the same partition rule as Search.
func (s *Store) Get(ctx context.Context, agent, partition string, id event.ID) (memory.Item, error) {
	if err := checkScope(agent, partition); err != nil {
		return memory.Item{}, err
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+itemCols+` FROM memory_items WHERE id = ? AND agent = ? AND (`+visibleSQL+`)`,
		string(id), agent, partition)
	it, _, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Item{}, memory.ErrNotFound
	}
	return it, err
}

// Forget deletes one item with its vector (cascade) and its FTS row
// (trigger). An item outside the caller's partition is simply not found.
func (s *Store) Forget(ctx context.Context, agent, partition string, id event.ID) error {
	if err := checkScope(agent, partition); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM memory_items WHERE id = ? AND agent = ? AND (`+visibleSQL+`)`,
		string(id), agent, partition)
	if err != nil {
		return fmt.Errorf("memory: forget: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return memory.ErrNotFound
	}
	return nil
}

// Status reports index health for one agent, including a configured embedder
// that is down: retrieval degrades to FTS, but never silently.
func (s *Store) Status(ctx context.Context, agent string) (memory.Status, error) {
	st := memory.Status{NoteBudget: s.opts.NoteBudgetTokens}
	rows, err := s.db.QueryContext(ctx,
		`SELECT layer, COUNT(*), COALESCE(SUM(tokens),0) FROM memory_items WHERE agent = ? GROUP BY layer`, agent)
	if err != nil {
		return st, err
	}
	defer rows.Close() //nolint:errcheck // read-only
	for rows.Next() {
		var layer string
		var n, tok int
		if err := rows.Scan(&layer, &n, &tok); err != nil {
			return st, err
		}
		if memory.Layer(layer) == memory.LayerNote {
			st.Notes, st.NoteTokens = n, tok
		} else {
			st.Recall = n
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	if s.embeds() {
		st.EmbedModel = s.opts.EmbedModel.ID
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memory_items i
			   LEFT JOIN memory_vectors v ON v.item_id = i.id AND v.model = ?
			  WHERE i.agent = ? AND v.item_id IS NULL`,
			s.opts.EmbedModel.ID, agent).Scan(&st.PendingEmbeddings); err != nil {
			return st, err
		}
	}
	s.mu.Lock()
	st.EmbedError, st.EmbedDims = s.embedErr, s.lastDims
	s.mu.Unlock()
	if st.EmbedDims == 0 && s.embeds() {
		// Fall back to what is stored for the current model.
		_ = s.db.QueryRowContext(ctx,
			`SELECT dims FROM memory_vectors WHERE model = ? LIMIT 1`, s.opts.EmbedModel.ID).Scan(&st.EmbedDims)
	}
	return st, nil
}

// itemCols is the column list every item scan uses.
const itemCols = `id, layer, text, about, visibility, agent, "partition", user_id, role, session_id, trace_id, created_at, tainted`

// visibleSQL is the one partition rule: an item is readable from its own
// partition, and a shared note is readable from every partition of the agent.
// Every read path interpolates exactly this string with the partition bound
// as a parameter.
const visibleSQL = `"partition" = ? OR (layer = 'note' AND visibility = 'shared')`

type scanner interface{ Scan(dest ...any) error }

func scanItem(sc scanner) (memory.Item, time.Time, error) {
	var (
		it                    memory.Item
		id, layer, vis        string
		agent, part, user     string
		role, session, traceI string
		created               int64
		tainted               int64
	)
	if err := sc.Scan(&id, &layer, &it.Text, &it.About, &vis, &agent, &part, &user, &role, &session, &traceI, &created, &tainted); err != nil {
		return memory.Item{}, time.Time{}, err
	}
	t := time.UnixMilli(created).UTC()
	it.ID, it.Layer, it.Visibility = event.ID(id), memory.Layer(layer), memory.Visibility(vis)
	it.Tainted = tainted != 0
	it.Prov = memory.Provenance{
		Agent: agent, Partition: part, UserID: event.ID(user), Role: role,
		SessionID: event.ID(session), TraceID: event.ID(traceI), Time: t,
	}
	return it, t, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func checkProv(p memory.Provenance) error {
	return checkScope(p.Agent, p.Partition)
}

func checkScope(agent, partition string) error {
	if strings.TrimSpace(agent) == "" {
		return &ProvenanceError{Field: "agent"}
	}
	if strings.TrimSpace(partition) == "" {
		return &ProvenanceError{Field: "partition"}
	}
	return nil
}

func textHash(s string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(s)))
	return hex.EncodeToString(sum[:])
}

// ForgetSession removes everything derived from one session: deleting a
// conversation must not leave a searchable summary of it behind
// (docs/traceability.md). Curated notes are kept: they were saved on purpose and
// are managed with memory.forget.
func (s *Store) ForgetSession(ctx context.Context, sessionID event.ID) (int64, error) {
	if sessionID == "" {
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM memory_items WHERE session_id = ? AND layer = 'recall'`, string(sessionID))
	if err != nil {
		return 0, fmt.Errorf("memory: forget session: %w", err)
	}
	return res.RowsAffected()
}

// ForgetPartition removes a person's own memory: every recall row and every
// origin note of the partition, in every agent. Shared notes stay: they were
// written for everyone the agent serves, and their provenance keeps the
// writer. Deleting an identity uses it (docs/identity.md §2).
func (s *Store) ForgetPartition(ctx context.Context, partition string) (int64, error) {
	if strings.TrimSpace(partition) == "" {
		return 0, &ProvenanceError{Field: "partition"}
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM memory_items WHERE "partition" = ? AND NOT (layer = 'note' AND visibility = 'shared')`,
		partition)
	if err != nil {
		return 0, fmt.Errorf("memory: forget partition: %w", err)
	}
	return res.RowsAffected()
}

// Partitions enumerates the memory spaces one agent holds, newest written
// first. It is the only read here that is not partition-scoped, and it is
// allowed to be because it carries no text: a name and two counts say what
// exists, never what is in it. The agent is still mandatory — partitions of
// two agents are never mixed.
func (s *Store) Partitions(ctx context.Context, agent string) ([]memory.Partition, error) {
	if strings.TrimSpace(agent) == "" {
		return nil, &ProvenanceError{Field: "agent"}
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT "partition",
		        COUNT(*) FILTER (WHERE layer = 'note')   AS notes,
		        COUNT(*) FILTER (WHERE layer = 'recall') AS recall,
		        MAX(created_at)                          AS last_written
		   FROM memory_items
		  WHERE agent = ?
		  GROUP BY "partition"
		  ORDER BY last_written DESC, "partition"`, agent)
	if err != nil {
		return nil, fmt.Errorf("memory: partitions: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only
	// Never nil: an agent nobody has written for has no partitions, which is
	// an empty list and not an absent one.
	out := []memory.Partition{}
	for rows.Next() {
		var p memory.Partition
		var last sql.NullInt64
		if err := rows.Scan(&p.Name, &p.Notes, &p.Recall, &last); err != nil {
			return nil, err
		}
		if last.Valid {
			p.LastWritten = time.UnixMilli(last.Int64).UTC()
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
