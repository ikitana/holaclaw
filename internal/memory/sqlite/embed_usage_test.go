package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"holaclaw/internal/event"
	"holaclaw/internal/memory"
	"holaclaw/internal/provider"
)

// sink records what the store emits.
type sink struct {
	mu  sync.Mutex
	evs []event.Event
}

func (s *sink) Emit(_ context.Context, e event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, e)
}

func (s *sink) embedded(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for _, e := range s.evs {
		if e.Type != event.MemoryEmbedded {
			continue
		}
		var a map[string]any
		if err := json.Unmarshal(e.Attrs, &a); err != nil {
			t.Fatal(err)
		}
		a["_trace"], a["_session"] = string(e.TraceID), string(e.SessionID)
		out = append(out, a)
	}
	return out
}

// Every answered embeddings call is a memory.embedded in the trace it served:
// an item's in the trace that wrote it, a query's in the search's trace. A
// cache hit is not a call and records nothing.
func TestEmbeddingsAreRecordedInTheirTrace(t *testing.T) {
	cost := provider.NanoUSD(1000)
	emb := &fakeEmbedder{cost: &cost}
	ev := &sink{}
	s, _ := newStore(t, Options{Embedder: emb, EmbedModel: model("embed-1"), Events: ev})
	ctx := context.Background()

	write(t, s, memory.LayerNote, "dm/a", "the deploy key lives in the vault", memory.VisOrigin)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Search(ctx, memory.Query{Agent: "main", Partition: "dm/a", Text: "deploy key",
		TraceID: "T2", SessionID: "S2"}); err != nil {
		t.Fatal(err)
	}
	// The same query again is served from the cache: no call, no event.
	if _, err := s.Search(ctx, memory.Query{Agent: "main", Partition: "dm/a", Text: "deploy key", TraceID: "T3"}); err != nil {
		t.Fatal(err)
	}

	got := ev.embedded(t)
	if len(got) != 2 {
		t.Fatalf("memory.embedded = %v, want the item and the first query", got)
	}
	item, query := got[0], got[1]
	if item["purpose"] != "item" || item["_trace"] != "T1" || item["_session"] != "S1" {
		t.Errorf("item embedding = %v", item)
	}
	if query["purpose"] != "query" || query["_trace"] != "T2" || query["_session"] != "S2" {
		t.Errorf("query embedding = %v", query)
	}
	for _, a := range got {
		if a["prompt_tokens"] != float64(1) || a["cost_nanousd"] != float64(cost) || a["inputs"] != float64(1) || a["model"] != "embed-1" {
			t.Errorf("usage attrs = %v", a)
		}
		if u := event.UsageOf(event.MemoryEmbedded, a); u.PromptTokens != 1 || u.Cost == nil || u.UnpricedCalls != 0 {
			t.Errorf("counted as %+v", u)
		}
	}
}

// A reindex serves no conversation: its calls share one trace of their own,
// and a search with no trace gets one too, so nothing is left out of a trace.
func TestEmbeddingsWithoutATraceGetOne(t *testing.T) {
	emb := &fakeEmbedder{}
	ev := &sink{}
	s, _ := newStore(t, Options{Embedder: emb, EmbedModel: model("embed-1"), Events: ev})
	ctx := context.Background()
	write(t, s, memory.LayerNote, "dm/a", "one note about deploys", memory.VisOrigin)
	write(t, s, memory.LayerNote, "dm/a", "another note about keys", memory.VisOrigin)
	if err := s.Reindex(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Search(ctx, memory.Query{Agent: "main", Partition: "dm/a", Text: "vault"}); err != nil {
		t.Fatal(err)
	}
	got := ev.embedded(t)
	if len(got) != 2 {
		t.Fatalf("memory.embedded = %v", got)
	}
	if got[0]["purpose"] != "reindex" || got[0]["inputs"] != float64(2) || got[0]["_trace"] == "" || got[0]["_session"] != "" {
		t.Errorf("reindex = %v", got[0])
	}
	if got[1]["purpose"] != "query" || got[1]["_trace"] == "" || got[1]["_trace"] == got[0]["_trace"] {
		t.Errorf("traceless query = %v", got[1])
	}
	// No price reported: an unpriced call, not a free one.
	if _, ok := got[0]["cost_nanousd"]; ok {
		t.Errorf("unpriced embedding wrote a cost: %v", got[0])
	}
	if u := event.UsageOf(event.MemoryEmbedded, got[0]); u.UnpricedCalls != 1 {
		t.Errorf("counted as %+v", u)
	}
}

// A call that failed before any reply has nothing to record.
func TestFailedEmbeddingRecordsNothing(t *testing.T) {
	emb := &fakeEmbedder{err: errors.New("down")}
	ev := &sink{}
	s, _ := newStore(t, Options{Embedder: emb, EmbedModel: model("embed-1"), Events: ev})
	if _, err := s.Search(context.Background(), memory.Query{Agent: "main", Partition: "dm/a", Text: "x", TraceID: "T"}); err != nil {
		t.Fatal(err)
	}
	if got := ev.embedded(t); len(got) != 0 {
		t.Fatalf("memory.embedded = %v", got)
	}
}
