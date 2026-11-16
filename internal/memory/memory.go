// Package memory defines the agent memory contract: long-term notes and
// short-term recall behind one swappable Store (docs/memory.md). Semantics
// live here; the v1 backend is SQLite.
package memory

import (
	"context"
	"errors"
	"time"

	"holaclaw/internal/event"
)

var ErrNotFound = errors.New("memory: not found")

// Layer separates curated notes from the derived recall index.
type Layer string

const (
	LayerNote   Layer = "note"
	LayerRecall Layer = "recall"
)

// Visibility of a note.
type Visibility string

const (
	VisShared Visibility = "shared" // readable from every partition of the agent
	VisOrigin Visibility = "origin" // readable only from the partition it came from
)

// Provenance is written by code from trusted state, never by the model.
type Provenance struct {
	Agent     string
	Partition string   // mandatory: one per DM user, one per room
	UserID    event.ID // sender
	Role      string   // sender role at write time
	SessionID event.ID
	TraceID   event.ID
	Time      time.Time
}

// Item is one note or recall entry.
type Item struct {
	ID         event.ID
	Layer      Layer
	Text       string
	About      string // optional person this is about
	Visibility Visibility
	// Tainted marks an item whose text came from, or summarises, untrusted
	// content. Injecting it wraps it as untrusted again, so recall cannot
	// launder it into trusted context (docs/security.md).
	Tainted bool
	Prov    Provenance
}

// Query is a deterministic retrieval request; no model call on this path
// except the embedding of Text when an embedder is configured.
type Query struct {
	Agent     string
	Partition string // mandatory; cross-partition reads are default-deny
	Text      string
	About     string
	Layers    []Layer // empty = both
	Limit     int     // 0 = store default injection cap
	// TraceID and SessionID say whose trace the embedding call is recorded
	// in (memory.embedded); they never filter. Empty: a trace of its own.
	TraceID   event.ID
	SessionID event.ID
}

// Hit is a ranked result.
type Hit struct {
	Item  Item
	Score float64
}

// Status reports index health.
type Status struct {
	Notes, Recall     int
	NoteTokens        int
	NoteBudget        int
	EmbedModel        string
	EmbedDims         int
	PendingEmbeddings int
	EmbedError        string // non-empty when a configured embedder is down
}

// Store is the memory backend contract.
type Store interface {
	// Write stores an item. Prov.Partition and Prov.Agent are mandatory.
	// Notes over the budget fail with ErrBudget so the caller consolidates.
	Write(ctx context.Context, it Item) (event.ID, error)
	Search(ctx context.Context, q Query) ([]Hit, error)
	// Get and Forget enforce the same partition rule as Search.
	Get(ctx context.Context, agent, partition string, id event.ID) (Item, error)
	Forget(ctx context.Context, agent, partition string, id event.ID) error
	// Reindex rebuilds recall and re-embeds when the embed model changed.
	Reindex(ctx context.Context, agent string) error
	Status(ctx context.Context, agent string) (Status, error)
}

// ErrBudget is returned by Write when the notes budget is exhausted.
var ErrBudget = errors.New("memory: notes budget exhausted, consolidate first")

// ErrProvenance is returned when a write or query arrives without the
// mandatory agent and partition.
var ErrProvenance = errors.New("memory: agent and partition are mandatory")

// ErrTaskPartition refuses recall ingestion for a task run: task sessions are
// excluded from recall.
var ErrTaskPartition = errors.New("memory: task partitions are excluded from recall")

// RecallIngester is implemented by backends that build the recall layer from
// session summaries (at session end or compaction).
type RecallIngester interface {
	// tainted carries the untrusted flag of the summarised content.
	IngestRecall(ctx context.Context, prov Provenance, summary string, tainted bool) (event.ID, error)
}
