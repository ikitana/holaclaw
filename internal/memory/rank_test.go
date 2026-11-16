package memory

import (
	"math"
	"testing"
	"time"
)

func TestNormalise(t *testing.T) {
	cases := []struct {
		name string
		in   []float64
		want []float64
	}{
		{"empty", nil, []float64{}},
		{"flat", []float64{2, 2, 2}, []float64{1, 1, 1}},
		{"single", []float64{-4}, []float64{1}},
		{"spread", []float64{0, 5, 10}, []float64{NormaliseFloor, 0.6, 1}},
		{"negative bm25", []float64{-3, -1, -2}, []float64{NormaliseFloor, 1, 0.6}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Normalise(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("len %d, want %d", len(got), len(c.want))
			}
			for i := range got {
				if math.Abs(got[i]-c.want[i]) > 1e-9 {
					t.Fatalf("got %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestDecay(t *testing.T) {
	half := 30 * 24 * time.Hour
	cases := []struct {
		age  time.Duration
		want float64
	}{
		{0, 1},
		{-time.Hour, 1},
		{half, 0.5},
		{2 * half, 0.25},
		{4 * half, 0.0625},
	}
	for _, c := range cases {
		if got := Decay(c.age, half); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("Decay(%v) = %v, want %v", c.age, got, c.want)
		}
	}
	if got := Decay(time.Hour, 0); got != 1 {
		t.Errorf("Decay with no half-life = %v, want 1", got)
	}
	// Notes decay four times slower: one note half-life is four recall ones.
	if got := Decay(half*NoteDecaySlowdown, half*NoteDecaySlowdown); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("note decay = %v, want 0.5", got)
	}
}

func TestCombine(t *testing.T) {
	if got := Combine(1, 0, false, 1); got != 1 {
		t.Errorf("fts only = %v, want 1", got)
	}
	if got := Combine(1, 0, true, 1); got != 0.5 {
		t.Errorf("half weights = %v, want 0.5", got)
	}
	if got := Combine(1, 1, true, 0.5); got != 0.5 {
		t.Errorf("with decay = %v, want 0.5", got)
	}
}

func TestCosine(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 0}, []float32{1, 0}, 1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"opposite", []float32{1, 0}, []float32{-1, 0}, -1},
		{"dimension mismatch", []float32{1, 0}, []float32{1, 0, 0}, 0},
		{"empty", nil, nil, 0},
		{"zero vector", []float32{0, 0}, []float32{1, 1}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Cosine(c.a, c.b); math.Abs(got-c.want) > 1e-6 {
				t.Fatalf("Cosine = %v, want %v", got, c.want)
			}
		})
	}
}

func TestJaccardAndWords(t *testing.T) {
	if got := Words("Angel's  cat: Mila, 3!"); len(got) != 5 {
		t.Fatalf("Words = %v, want 5 tokens", got)
	}
	cases := []struct {
		name string
		a, b string
		want float64
	}{
		{"same", "the cat sleeps", "the cat sleeps", 1},
		{"disjoint", "cat", "dog", 0},
		{"half", "cat dog", "cat bird", 1.0 / 3.0},
		{"empty", "", "cat", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Jaccard(Words(c.a), Words(c.b)); math.Abs(got-c.want) > 1e-9 {
				t.Fatalf("Jaccard = %v, want %v", got, c.want)
			}
		})
	}
}

func TestMMRDiversity(t *testing.T) {
	// Three near-duplicates with the best scores, one different item below
	// them: MMR must still surface the different one.
	cands := []Candidate{
		{ID: "a", Score: 1.0, Words: Words("the cat sleeps on the sofa")},
		{ID: "b", Score: 0.9, Words: Words("the cat sleeps on the sofa")},
		{ID: "c", Score: 0.85, Words: Words("the cat sleeps on the sofa")},
		{ID: "d", Score: 0.6, Words: Words("angel deploys holaclaw on friday")},
	}
	got := MMR(cands, MMRLambda, 2)
	if len(got) != 2 {
		t.Fatalf("got %d hits, want 2", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "d" {
		t.Fatalf("got %s,%s, want a,d", got[0].ID, got[1].ID)
	}
}

func TestMMRDeterministicAndBounded(t *testing.T) {
	cands := []Candidate{
		{ID: "z", Score: 0.5, Words: Words("one")},
		{ID: "a", Score: 0.5, Words: Words("two")},
		{ID: "m", Score: 0.5, Words: Words("three")},
	}
	first := MMR(cands, MMRLambda, 3)
	for i := 0; i < 5; i++ {
		again := MMR(cands, MMRLambda, 3)
		for j := range first {
			if first[j].ID != again[j].ID {
				t.Fatalf("run %d differs: %v vs %v", i, ids(first), ids(again))
			}
		}
	}
	if got := ids(first); got[0] != "a" {
		t.Fatalf("ties not broken by id: %v", got)
	}
	if got := MMR(cands, MMRLambda, 0); got != nil {
		t.Fatalf("limit 0 returned %v", got)
	}
	if got := MMR(cands, MMRLambda, 10); len(got) != 3 {
		t.Fatalf("limit over the pool returned %d", len(got))
	}
	if got := MMR(nil, MMRLambda, 3); got != nil {
		t.Fatalf("empty pool returned %v", got)
	}
}

func TestMMRPrefersVectorsWhenBothHave(t *testing.T) {
	// Same words, orthogonal vectors: the vector similarity wins, so both are
	// kept even though the token sets are identical.
	cands := []Candidate{
		{ID: "a", Score: 1, Vec: []float32{1, 0}, Words: Words("same text")},
		{ID: "b", Score: 0.9, Vec: []float32{0, 1}, Words: Words("same text")},
	}
	got := MMR(cands, MMRLambda, 2)
	if len(got) != 2 || got[1].ID != "b" {
		t.Fatalf("got %v, want a,b", ids(got))
	}
}

func ids(cs []Candidate) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}
