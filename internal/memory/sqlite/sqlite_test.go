package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"holaclaw/internal/event"
	"holaclaw/internal/memory"
	"holaclaw/internal/provider"
	"holaclaw/internal/store"
)

// fakeEmbedder produces deterministic low-dimensional vectors: each word is
// hashed into a bucket, so texts sharing words share direction. It counts
// calls and can be switched to failing.
type fakeEmbedder struct {
	mu       sync.Mutex
	calls    int // Embed invocations
	inputs   int // texts embedded
	dims     int
	err      error
	scramble bool // change the vectors, as another model would
	// cost, when set, is the price each call reports; its usage is one
	// prompt token per input.
	cost *provider.NanoUSD
}

func (f *fakeEmbedder) Embed(_ context.Context, _ provider.Model, inputs []string) ([][]float32, provider.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.inputs += len(inputs)
	if f.err != nil {
		return nil, provider.Usage{}, f.err
	}
	usage := provider.Usage{PromptTokens: len(inputs), Cost: f.cost}
	dims := f.dims
	if dims == 0 {
		dims = 8
	}
	out := make([][]float32, len(inputs))
	for i, in := range inputs {
		v := make([]float32, dims)
		for _, w := range memory.Words(in) {
			h := fnv.New32a()
			if f.scramble {
				_, _ = h.Write([]byte("v2:"))
			}
			_, _ = h.Write([]byte(w))
			v[int(h.Sum32())%dims] += 1
		}
		if allZero(v) {
			v[0] = 1
		}
		out[i] = v
	}
	return out, usage, nil
}

func (f *fakeEmbedder) stats() (calls, inputs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, f.inputs
}

func (f *fakeEmbedder) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func allZero(v []float32) bool {
	for _, x := range v {
		if x != 0 {
			return false
		}
	}
	return true
}

func model(id string) provider.Model {
	return provider.Model{Name: id, Provider: "local", ID: id}
}

// newStore opens a real, migrated SQLite database and a Store over it.
func newStore(t *testing.T, opts Options) (*Store, *sql.DB) {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st.DB(), opts), st.DB()
}

func prov(partition string) memory.Provenance {
	return memory.Provenance{
		Agent: "main", Partition: partition, UserID: event.ID("U1"), Role: "owner",
		SessionID: event.ID("S1"), TraceID: event.ID("T1"),
	}
}

func write(t *testing.T, s *Store, layer memory.Layer, partition, text string, vis memory.Visibility) event.ID {
	t.Helper()
	id, err := s.Write(context.Background(), memory.Item{
		Layer: layer, Text: text, Visibility: vis, Prov: prov(partition),
	})
	if err != nil {
		t.Fatalf("Write(%q): %v", text, err)
	}
	return id
}

func texts(hits []memory.Hit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Item.Text)
	}
	return out
}

func search(t *testing.T, s *Store, partition, query string) []memory.Hit {
	t.Helper()
	hits, err := s.Search(context.Background(), memory.Query{Agent: "main", Partition: partition, Text: query})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return hits
}

func TestWriteRefusesIncompleteProvenance(t *testing.T) {
	s, _ := newStore(t, Options{})
	ctx := context.Background()
	cases := []memory.Provenance{
		{Agent: "main"},     // no partition
		{Partition: "dm/a"}, // no agent
		{Agent: "main", Partition: "   "},
		{},
	}
	for i, p := range cases {
		_, err := s.Write(ctx, memory.Item{Layer: memory.LayerNote, Text: "x", Prov: p})
		if !errors.Is(err, ErrProvenance) {
			t.Errorf("case %d: err = %v, want ErrProvenance", i, err)
		}
	}
	// The same rule guards reads: an empty partition is an error, never "all".
	if _, err := s.Search(ctx, memory.Query{Agent: "main"}); !errors.Is(err, ErrProvenance) {
		t.Errorf("Search without partition: %v, want ErrProvenance", err)
	}
	if _, err := s.Get(ctx, "main", "", event.ID("x")); !errors.Is(err, ErrProvenance) {
		t.Errorf("Get without partition: %v, want ErrProvenance", err)
	}
	if err := s.Forget(ctx, "main", "", event.ID("x")); !errors.Is(err, ErrProvenance) {
		t.Errorf("Forget without partition: %v, want ErrProvenance", err)
	}
}

func TestPartitionIsolationMatrix(t *testing.T) {
	s, _ := newStore(t, Options{InjectionCap: 20})
	write(t, s, memory.LayerNote, "dm/alice", "alice likes espresso", memory.VisOrigin)
	write(t, s, memory.LayerNote, "dm/alice", "the office wifi password is stored in vault", memory.VisShared)
	write(t, s, memory.LayerNote, "dm/bob", "bob likes tea", memory.VisOrigin)
	write(t, s, memory.LayerNote, "room/g1", "the team ships on friday", memory.VisOrigin)
	write(t, s, memory.LayerRecall, "dm/alice", "alice asked about the espresso machine", "")
	write(t, s, memory.LayerRecall, "dm/bob", "bob asked about tea leaves", "")

	want := map[string][]string{
		"dm/alice": {"alice likes espresso", "the office wifi password is stored in vault", "alice asked about the espresso machine"},
		"dm/bob":   {"bob likes tea", "the office wifi password is stored in vault", "bob asked about tea leaves"},
		"room/g1":  {"the team ships on friday", "the office wifi password is stored in vault"},
	}
	for part, wantTexts := range want {
		got := texts(search(t, s, part, ""))
		if len(got) != len(wantTexts) {
			t.Fatalf("%s: got %v, want %v", part, got, wantTexts)
		}
		for _, w := range wantTexts {
			if !contains(got, w) {
				t.Fatalf("%s: got %v, missing %q", part, got, w)
			}
		}
	}
	// A shared *recall* entry cannot exist: the store forces origin.
	id, err := s.Write(context.Background(), memory.Item{
		Layer: memory.LayerRecall, Text: "sneaky recall", Visibility: memory.VisShared, Prov: prov("dm/alice"),
	})
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.Get(context.Background(), "main", "dm/alice", id)
	if err != nil {
		t.Fatal(err)
	}
	if it.Visibility != memory.VisOrigin {
		t.Fatalf("recall visibility = %s, want origin", it.Visibility)
	}
	if _, err := s.Get(context.Background(), "main", "dm/bob", id); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("recall leaked across partitions: %v", err)
	}
	// Another agent sees nothing of this one.
	hits, err := s.Search(context.Background(), memory.Query{Agent: "other", Partition: "dm/alice", Text: "espresso"})
	if err != nil || len(hits) != 0 {
		t.Fatalf("cross-agent read: %v %v", texts(hits), err)
	}
}

func TestAdversarialPartitionStrings(t *testing.T) {
	s, _ := newStore(t, Options{InjectionCap: 20})
	nasty := []string{
		`dm/' OR 1=1 --`,
		`dm/"; DROP TABLE memory_items; --`,
		`dm/%`,
		`dm/*`,
		`dm/ñoño 🙈`,
		`dm/a' UNION SELECT id FROM memory_items --`,
	}
	for i, p := range nasty {
		write(t, s, memory.LayerNote, p, fmt.Sprintf("secret number %d", i), memory.VisOrigin)
	}
	for i, p := range nasty {
		got := texts(search(t, s, p, ""))
		if len(got) != 1 || got[0] != fmt.Sprintf("secret number %d", i) {
			t.Fatalf("partition %q saw %v", p, got)
		}
	}
	// A wildcard-looking partition matches nothing but itself.
	if got := texts(search(t, s, "dm/%", "secret")); len(got) != 1 {
		t.Fatalf("wildcard partition saw %v", got)
	}
	// An adversarial agent name is a literal too.
	if _, err := s.Search(context.Background(), memory.Query{
		Agent: `main' OR '1'='1`, Partition: "dm/%", Text: "secret"}); err != nil {
		t.Fatalf("adversarial agent: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM memory_items`).Scan(&n); err != nil || n != len(nasty) {
		t.Fatalf("rows = %d (err %v), want %d", n, err, len(nasty))
	}
}

func TestNoteBudget(t *testing.T) {
	s, _ := newStore(t, Options{NoteBudgetTokens: 20})
	ctx := context.Background()
	write(t, s, memory.LayerNote, "dm/a", strings.Repeat("alpha ", 8), memory.VisOrigin)
	_, err := s.Write(ctx, memory.Item{
		Layer: memory.LayerNote, Text: strings.Repeat("beta ", 8), Prov: prov("dm/a"),
	})
	if !errors.Is(err, memory.ErrBudget) {
		t.Fatalf("err = %v, want ErrBudget", err)
	}
	// The budget spans partitions of the agent, and recall is not budgeted.
	_, err = s.Write(ctx, memory.Item{Layer: memory.LayerNote, Text: strings.Repeat("beta ", 8), Prov: prov("dm/b")})
	if !errors.Is(err, memory.ErrBudget) {
		t.Fatalf("other partition err = %v, want ErrBudget", err)
	}
	if _, err := s.Write(ctx, memory.Item{Layer: memory.LayerRecall, Text: strings.Repeat("gamma ", 40), Prov: prov("dm/a")}); err != nil {
		t.Fatalf("recall hit the budget: %v", err)
	}
	st, err := s.Status(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if st.Notes != 1 || st.Recall != 1 || st.NoteBudget != 20 || st.NoteTokens == 0 {
		t.Fatalf("status = %+v", st)
	}
}

func TestRecallDedupeAndTaskGuard(t *testing.T) {
	s, _ := newStore(t, Options{})
	ctx := context.Background()
	p := prov("dm/a")
	id1, err := s.IngestRecall(ctx, p, "  the user moved to Girona  ", false)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.IngestRecall(ctx, p, "the user moved to Girona", false)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("dedupe failed: %s vs %s", id1, id2)
	}
	// The same summary in another partition is a separate entry.
	id3, err := s.IngestRecall(ctx, prov("dm/b"), "the user moved to Girona", false)
	if err != nil || id3 == id1 {
		t.Fatalf("cross-partition dedupe: %s %v", id3, err)
	}
	if _, err := s.IngestRecall(ctx, prov("task/T1"), "task run summary", false); !errors.Is(err, ErrTaskPartition) {
		t.Fatalf("task partition err = %v, want ErrTaskPartition", err)
	}
	if _, err := s.IngestRecall(ctx, memory.Provenance{Agent: "main"}, "x", false); !errors.Is(err, ErrProvenance) {
		t.Fatalf("task guard skipped provenance check: %v", err)
	}
}

func TestFTSQuerySanitising(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{`"""`, ""},
		{"???", ""},
		{"cat AND dog", `"cat" OR "dog"`},
		{"cat OR dog NOT bird", `"cat" OR "dog" OR "bird"`},
		{"NEAR(a b)", `"a" OR "b"`},
		{`"exact phrase"`, `"exact" OR "phrase"`},
		{"prefix*", `"prefix"`},
		{"col:value", `"col" OR "value"`},
		{"ñoño Girona 2026", `"ñoño" OR "Girona" OR "2026"`},
		{"a-b^c", `"a" OR "b" OR "c"`},
	}
	for _, c := range cases {
		if got := ftsQuery(c.in); got != c.want {
			t.Errorf("ftsQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := ftsQuery(strings.Repeat("word ", 40)); strings.Count(got, " OR ") != maxFTSTerms-1 {
		t.Errorf("term cap not applied: %q", got)
	}
	if got := ftsQuery(strings.Repeat("x", 200)); len([]rune(got)) != 66 {
		t.Errorf("term length cap not applied: %d runes", len([]rune(got)))
	}
}

func TestSearchSurvivesHostileQueries(t *testing.T) {
	s, _ := newStore(t, Options{})
	write(t, s, memory.LayerNote, "dm/a", "the deploy key lives in vault", memory.VisOrigin)
	for _, q := range []string{
		`"`, `*`, `deploy AND key`, `NEAR(deploy key, 2)`, `deploy OR "`, `^deploy`, `ñ`, `(((`,
		`deploy" OR memory_fts MATCH "key`,
	} {
		if _, err := s.Search(context.Background(), memory.Query{Agent: "main", Partition: "dm/a", Text: q}); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
	}
	// An empty query after sanitising lists by recency instead of failing.
	if got := texts(search(t, s, "dm/a", "???")); len(got) != 1 {
		t.Fatalf("recency listing = %v", got)
	}
}

func TestRankingIsDeterministic(t *testing.T) {
	emb := &fakeEmbedder{}
	s, _ := newStore(t, Options{Embedder: emb, EmbedModel: model("embed-1"), InjectionCap: 3})
	for i := 0; i < 8; i++ {
		write(t, s, memory.LayerNote, "dm/a", fmt.Sprintf("note %d about deploys and keys", i), memory.VisOrigin)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := texts(search(t, s, "dm/a", "deploy keys"))
	if len(first) != 3 {
		t.Fatalf("cap not applied: %v", first)
	}
	for i := 0; i < 5; i++ {
		again := texts(search(t, s, "dm/a", "deploy keys"))
		if strings.Join(first, "|") != strings.Join(again, "|") {
			t.Fatalf("run %d: %v vs %v", i, first, again)
		}
	}
}

func TestRecencyDecay(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	s, _ := newStore(t, Options{HalfLife: 24 * time.Hour, InjectionCap: 5, Now: func() time.Time { return now }})
	ctx := context.Background()
	old := prov("dm/a")
	old.Time = now.Add(-100 * 24 * time.Hour)
	fresh := prov("dm/a")
	fresh.Time = now
	if _, err := s.Write(ctx, memory.Item{Layer: memory.LayerRecall, Text: "the deploy key rotates", Prov: old}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, memory.Item{Layer: memory.LayerRecall, Text: "the deploy key rotated today", Prov: fresh}); err != nil {
		t.Fatal(err)
	}
	got := texts(search(t, s, "dm/a", "deploy key"))
	if len(got) != 2 || got[0] != "the deploy key rotated today" {
		t.Fatalf("recency ignored: %v", got)
	}
	// A note of the same age decays four times slower than that recall entry.
	noteProv := old
	if _, err := s.Write(ctx, memory.Item{Layer: memory.LayerNote, Text: "the deploy key rotates", Prov: noteProv}); err != nil {
		t.Fatal(err)
	}
	hits := search(t, s, "dm/a", "deploy key")
	var noteScore, recallScore float64
	for _, h := range hits {
		if h.Item.Layer == memory.LayerNote {
			noteScore = h.Score
		} else if h.Item.Text == "the deploy key rotates" {
			recallScore = h.Score
		}
	}
	if !(noteScore > recallScore) {
		t.Fatalf("note %v did not outlive recall %v", noteScore, recallScore)
	}
}

func TestSearchMMRKeepsDiversity(t *testing.T) {
	s, _ := newStore(t, Options{InjectionCap: 2})
	for i := 0; i < 4; i++ {
		write(t, s, memory.LayerNote, "dm/a", "the deploy key lives in the vault", memory.VisOrigin)
	}
	write(t, s, memory.LayerNote, "dm/a", "angel prefers deploy on tuesday mornings", memory.VisOrigin)
	got := texts(search(t, s, "dm/a", "deploy"))
	if len(got) != 2 {
		t.Fatalf("cap = %v", got)
	}
	if got[0] == got[1] {
		t.Fatalf("near-duplicates filled the cap: %v", got)
	}
}

func TestAboutFilter(t *testing.T) {
	s, _ := newStore(t, Options{InjectionCap: 10})
	ctx := context.Background()
	for _, c := range []struct{ text, about string }{
		{"likes espresso", "Alice"},
		{"likes tea", "Bob"},
		{"likes nothing", ""},
	} {
		if _, err := s.Write(ctx, memory.Item{
			Layer: memory.LayerNote, Text: c.text, About: c.about, Prov: prov("dm/a"),
		}); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := s.Search(ctx, memory.Query{Agent: "main", Partition: "dm/a", Text: "likes", About: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Item.About != "Alice" {
		t.Fatalf("about filter = %v", texts(hits))
	}
	hits, err = s.Search(ctx, memory.Query{Agent: "main", Partition: "dm/a", Text: "likes", About: "Nobody"})
	if err != nil || len(hits) != 0 {
		t.Fatalf("unknown about = %v %v", texts(hits), err)
	}
}

func TestLayerFilter(t *testing.T) {
	s, _ := newStore(t, Options{InjectionCap: 10})
	write(t, s, memory.LayerNote, "dm/a", "a curated note about coffee", memory.VisOrigin)
	write(t, s, memory.LayerRecall, "dm/a", "a recall entry about coffee", "")
	hits, err := s.Search(context.Background(), memory.Query{
		Agent: "main", Partition: "dm/a", Text: "coffee", Layers: []memory.Layer{memory.LayerNote}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Item.Layer != memory.LayerNote {
		t.Fatalf("layer filter = %v", texts(hits))
	}
}

func TestEmbedderDownIsReportedAndSearchStillWorks(t *testing.T) {
	emb := &fakeEmbedder{}
	emb.fail(errors.New("connection refused"))
	s, _ := newStore(t, Options{Embedder: emb, EmbedModel: model("embed-1")})
	ctx := context.Background()
	write(t, s, memory.LayerNote, "dm/a", "the deploy key lives in vault", memory.VisOrigin)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if st.EmbedError == "" {
		t.Fatal("Status.EmbedError is empty after an embedder failure")
	}
	if st.PendingEmbeddings != 1 {
		t.Fatalf("PendingEmbeddings = %d, want 1", st.PendingEmbeddings)
	}
	// FTS still answers; the query embedding failure does not surface as an
	// error, it is reported through Status.
	if got := texts(search(t, s, "dm/a", "deploy key")); len(got) != 1 {
		t.Fatalf("FTS fallback = %v", got)
	}
	st, _ = s.Status(ctx, "main")
	if st.EmbedError == "" {
		t.Fatal("query embedding failure not reported")
	}
	// Recovery: Reindex fixes the pending item and clears the error.
	emb.fail(nil)
	if err := s.Reindex(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Status(ctx, "main")
	if st.PendingEmbeddings != 0 || st.EmbedError != "" || st.EmbedDims != 8 {
		t.Fatalf("after recovery: %+v", st)
	}
}

func TestReindexReportsAPersistentFailure(t *testing.T) {
	emb := &fakeEmbedder{}
	s, _ := newStore(t, Options{Embedder: emb, EmbedModel: model("embed-1")})
	write(t, s, memory.LayerNote, "dm/a", "something worth embedding", memory.VisOrigin)
	emb.fail(errors.New("boom"))
	if err := s.Reindex(context.Background(), "main"); err == nil {
		t.Fatal("Reindex succeeded with a failing embedder")
	}
	st, _ := s.Status(context.Background(), "main")
	if !strings.Contains(st.EmbedError, "boom") {
		t.Fatalf("EmbedError = %q", st.EmbedError)
	}
}

func TestEmbeddingCacheAvoidsASecondCall(t *testing.T) {
	emb := &fakeEmbedder{}
	s, _ := newStore(t, Options{Embedder: emb, EmbedModel: model("embed-1")})
	ctx := context.Background()
	write(t, s, memory.LayerNote, "dm/a", "cached text about deploys", memory.VisOrigin)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	_, inputs := emb.stats()
	if inputs != 1 {
		t.Fatalf("embedded %d texts, want 1", inputs)
	}
	// The same text in another partition, and the same text as a query, both
	// hit the cache.
	write(t, s, memory.LayerNote, "dm/b", "cached text about deploys", memory.VisOrigin)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	search(t, s, "dm/a", "cached text about deploys")
	if _, inputs = emb.stats(); inputs != 1 {
		t.Fatalf("embedded %d texts, want 1 (cache miss)", inputs)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM embedding_cache`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("cache rows = %d (err %v)", n, err)
	}
}

func TestModelSwapPendingThenReindex(t *testing.T) {
	emb := &fakeEmbedder{}
	ctx := context.Background()
	s1, db := newStore(t, Options{Embedder: emb, EmbedModel: model("embed-1")})
	for i := 0; i < 40; i++ { // more than one reindex batch
		write(t, s1, memory.LayerNote, "dm/a", fmt.Sprintf("fact number %d", i), memory.VisOrigin)
	}
	if err := s1.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if st, _ := s1.Status(ctx, "main"); st.PendingEmbeddings != 0 {
		t.Fatalf("pending after flush = %d", st.PendingEmbeddings)
	}
	// Swap the model: the old vectors no longer count.
	emb2 := &fakeEmbedder{dims: 16, scramble: true}
	s2 := New(db, Options{Embedder: emb2, EmbedModel: model("embed-2")})
	st, err := s2.Status(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingEmbeddings != 40 || st.EmbedModel != "embed-2" {
		t.Fatalf("after swap: %+v", st)
	}
	// A search with mixed dimensions must not panic and must still rank.
	if got := texts(search(t, s2, "dm/a", "fact number 3")); len(got) == 0 {
		t.Fatal("search returned nothing after the model swap")
	}
	if err := s2.Reindex(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	st, _ = s2.Status(ctx, "main")
	if st.PendingEmbeddings != 0 || st.EmbedDims != 16 {
		t.Fatalf("after reindex: %+v", st)
	}
	if calls, _ := emb2.stats(); calls > 3 {
		t.Fatalf("reindex made %d calls for 40 items, want batching", calls)
	}
	var dims int
	if err := s2.db.QueryRow(`SELECT dims FROM memory_vectors LIMIT 1`).Scan(&dims); err != nil || dims != 16 {
		t.Fatalf("stored dims = %d (err %v)", dims, err)
	}
}

func TestForgetRemovesEverything(t *testing.T) {
	emb := &fakeEmbedder{}
	s, _ := newStore(t, Options{Embedder: emb, EmbedModel: model("embed-1")})
	ctx := context.Background()
	id := write(t, s, memory.LayerNote, "dm/a", "forgettable fact about otters", memory.VisOrigin)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// Another partition cannot delete it.
	if err := s.Forget(ctx, "main", "dm/b", id); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("cross-partition forget: %v", err)
	}
	if err := s.Forget(ctx, "main", "dm/a", id); err != nil {
		t.Fatal(err)
	}
	if err := s.Forget(ctx, "main", "dm/a", id); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("second forget: %v", err)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM memory_items`,
		`SELECT COUNT(*) FROM memory_vectors`,
		`SELECT COUNT(*) FROM memory_fts WHERE memory_fts MATCH 'otters'`,
	} {
		var n int
		if err := s.db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != 0 {
			t.Fatalf("%s left %d rows", q, n)
		}
	}
	if _, err := s.Get(ctx, "main", "dm/a", id); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("Get after forget: %v", err)
	}
}

func TestGetIsPartitionScoped(t *testing.T) {
	s, _ := newStore(t, Options{})
	ctx := context.Background()
	own := write(t, s, memory.LayerNote, "dm/a", "private to alice", memory.VisOrigin)
	shared := write(t, s, memory.LayerNote, "dm/a", "shared with everyone", memory.VisShared)
	if _, err := s.Get(ctx, "main", "dm/b", own); !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("origin note leaked: %v", err)
	}
	it, err := s.Get(ctx, "main", "dm/b", shared)
	if err != nil {
		t.Fatalf("shared note unreachable: %v", err)
	}
	if it.Prov.Partition != "dm/a" || it.Prov.Role != "owner" || it.Prov.TraceID != event.ID("T1") {
		t.Fatalf("provenance not returned: %+v", it.Prov)
	}
}

func TestWriteRejectsBadInput(t *testing.T) {
	s, _ := newStore(t, Options{})
	ctx := context.Background()
	if _, err := s.Write(ctx, memory.Item{Layer: memory.LayerNote, Text: "  ", Prov: prov("dm/a")}); err == nil {
		t.Fatal("empty text accepted")
	}
	if _, err := s.Write(ctx, memory.Item{Layer: "gossip", Text: "x", Prov: prov("dm/a")}); err == nil {
		t.Fatal("unknown layer accepted")
	}
	// An unset layer defaults to a note, and an unset visibility to origin.
	id, err := s.Write(ctx, memory.Item{Text: "defaulted", Prov: prov("dm/a")})
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.Get(ctx, "main", "dm/a", id)
	if err != nil {
		t.Fatal(err)
	}
	if it.Layer != memory.LayerNote || it.Visibility != memory.VisOrigin {
		t.Fatalf("defaults = %s/%s", it.Layer, it.Visibility)
	}
}

func TestWorkerEmbedsOffThePath(t *testing.T) {
	emb := &fakeEmbedder{}
	s, _ := newStore(t, Options{Embedder: emb, EmbedModel: model("embed-1")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	s.Start(ctx) // idempotent
	for i := 0; i < 10; i++ {
		write(t, s, memory.LayerNote, "dm/a", fmt.Sprintf("worker fact %d", i), memory.VisOrigin)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if st.PendingEmbeddings != 0 {
		t.Fatalf("pending = %d after Flush", st.PendingEmbeddings)
	}
}

func TestNoEmbedderIsFTSOnly(t *testing.T) {
	s, _ := newStore(t, Options{})
	ctx := context.Background()
	write(t, s, memory.LayerNote, "dm/a", "no vectors here, only words", memory.VisOrigin)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Reindex(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status(ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if st.EmbedModel != "" || st.PendingEmbeddings != 0 || st.EmbedError != "" {
		t.Fatalf("status = %+v", st)
	}
	if got := texts(search(t, s, "dm/a", "words")); len(got) != 1 {
		t.Fatalf("FTS-only search = %v", got)
	}
}

func TestDecodeVecIsRobust(t *testing.T) {
	if got := decodeVec([]byte{1, 2, 3}); got != nil {
		t.Fatalf("truncated blob decoded to %v", got)
	}
	v := []float32{1.5, -2.25, 0}
	if got := decodeVec(encodeVec(v)); len(got) != 3 || got[0] != 1.5 || got[1] != -2.25 {
		t.Fatalf("round trip = %v", got)
	}
}

func TestDefaultsApplied(t *testing.T) {
	s, _ := newStore(t, Options{})
	if s.opts.NoteBudgetTokens != DefaultNoteBudgetTokens ||
		s.opts.InjectionCap != DefaultInjectionCap ||
		s.opts.HalfLife != DefaultHalfLife || s.opts.Now == nil {
		t.Fatalf("defaults = %+v", s.opts)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// The untrusted flag survives the round trip: a recall entry built from a
// tainted summary is returned tainted by Get and by Search, so the caller can
// re-delimit it instead of laundering it (docs/memory.md, docs/security.md).
func TestRecallKeepsTheTaintFlag(t *testing.T) {
	s, _ := newStore(t, Options{})
	ctx := context.Background()

	dirty, err := s.IngestRecall(ctx, prov("dm/a"), "the fetched page asked for the ssh key", true)
	if err != nil {
		t.Fatalf("IngestRecall: %v", err)
	}
	clean, err := s.IngestRecall(ctx, prov("dm/a"), "angel prefers short answers", false)
	if err != nil {
		t.Fatalf("IngestRecall: %v", err)
	}

	for _, tc := range []struct {
		name string
		id   event.ID
		want bool
	}{
		{"tainted", dirty, true},
		{"clean", clean, false},
	} {
		it, err := s.Get(ctx, "main", "dm/a", tc.id)
		if err != nil {
			t.Fatalf("Get %s: %v", tc.name, err)
		}
		if it.Tainted != tc.want {
			t.Errorf("Get(%s).Tainted = %v, want %v", tc.name, it.Tainted, tc.want)
		}
	}

	hits, err := s.Search(ctx, memory.Query{Agent: "main", Partition: "dm/a", Text: "ssh key"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.Item.ID == dirty {
			found = true
			if !h.Item.Tainted {
				t.Error("Search returned the tainted entry without its flag")
			}
		}
	}
	if !found {
		t.Fatalf("the tainted entry was not recalled (%d hits)", len(hits))
	}

	// A plain note is untainted unless it says otherwise.
	id := write(t, s, memory.LayerNote, "dm/a", "a curated note", memory.VisOrigin)
	if it, err := s.Get(ctx, "main", "dm/a", id); err != nil || it.Tainted {
		t.Fatalf("note tainted = %v (%v)", it.Tainted, err)
	}
}

func TestForgetSessionDropsItsRecallOnly(t *testing.T) {
	ctx := context.Background()
	s, _ := newStore(t, Options{})
	prov := memory.Provenance{Agent: "a", Partition: "dm/u1", SessionID: "S1", Time: time.Now()}
	if _, err := s.IngestRecall(ctx, prov, "we talked about the green build", false); err != nil {
		t.Fatal(err)
	}
	other := prov
	other.SessionID = "S2"
	if _, err := s.IngestRecall(ctx, other, "another conversation about lunch", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(ctx, memory.Item{Layer: memory.LayerNote, Text: "likes tea", Visibility: memory.VisOrigin, Prov: prov}); err != nil {
		t.Fatal(err)
	}
	n, err := s.ForgetSession(ctx, "S1")
	if err != nil || n != 1 {
		t.Fatalf("forgot %d, err %v", n, err)
	}
	hits, err := s.Search(ctx, memory.Query{Agent: "a", Partition: "dm/u1", Text: "green build lunch tea", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Item.Prov.SessionID == "S1" && h.Item.Layer == memory.LayerRecall {
			t.Fatalf("recall of the deleted session is still searchable: %+v", h.Item)
		}
	}
	if len(hits) != 2 {
		t.Fatalf("want the other session's recall and the note, got %d hits", len(hits))
	}
}

func TestForgetPartitionKeepsSharedNotesAndOtherPartitions(t *testing.T) {
	ctx := context.Background()
	s, db := newStore(t, Options{})
	mine := memory.Provenance{Agent: "a", Partition: "dm/u1", UserID: "u1", SessionID: "S1", Time: time.Now()}
	otherAgent := mine
	otherAgent.Agent = "b"
	room := mine
	room.Partition = "room/tg/1"
	write := func(p memory.Provenance, text string, vis memory.Visibility) {
		t.Helper()
		if _, err := s.Write(ctx, memory.Item{Layer: memory.LayerNote, Text: text, Visibility: vis, Prov: p}); err != nil {
			t.Fatal(err)
		}
	}
	write(mine, "likes tea", memory.VisOrigin)
	write(otherAgent, "likes coffee", memory.VisOrigin)
	write(mine, "the office opens at nine", memory.VisShared)
	write(room, "the room meets on mondays", memory.VisOrigin)
	if _, err := s.IngestRecall(ctx, mine, "we talked about the green build", false); err != nil {
		t.Fatal(err)
	}
	n, err := s.ForgetPartition(ctx, "dm/u1")
	if err != nil || n != 3 {
		t.Fatalf("forgot %d, err %v; want 3", n, err)
	}
	rows, err := db.QueryContext(ctx, `SELECT text FROM memory_items ORDER BY text`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close() //nolint:errcheck // test
	var left []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			t.Fatal(err)
		}
		left = append(left, text)
	}
	if want := []string{"the office opens at nine", "the room meets on mondays"}; !slices.Equal(left, want) {
		t.Fatalf("left %q, want %q", left, want)
	}
	if _, err := s.ForgetPartition(ctx, " "); err == nil {
		t.Fatal("an empty partition must be refused")
	}
}
