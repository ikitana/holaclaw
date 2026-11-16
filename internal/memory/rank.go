package memory

import (
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Ranking is deterministic and model-free (docs/memory.md): hybrid FTS +
// vector scores, normalised to 0..1, multiplied by a recency decay and
// diversified with MMR. Everything in this file is a pure function so the
// backend stays testable without a database.

// MMRLambda weights relevance against diversity in MMR.
const MMRLambda = 0.7

// NoteDecaySlowdown is how much slower a curated note decays than a recall
// entry: notes are long-term, recall is short-term.
const NoteDecaySlowdown = 4

// NormaliseFloor is the weight the weakest candidate keeps. Plain min-max
// would pin it to zero, and a zero relevance cannot be reordered by recency —
// a stale exact match would beat a fresh one for ever.
const NormaliseFloor = 0.2

// Normalise maps scores into NormaliseFloor..1 by min-max over the slice. An
// empty slice or a slice with no spread yields all-ones, so a single
// candidate is not silently ranked low.
func Normalise(scores []float64) []float64 {
	out := make([]float64, len(scores))
	if len(scores) == 0 {
		return out
	}
	lo, hi := scores[0], scores[0]
	for _, s := range scores[1:] {
		lo, hi = math.Min(lo, s), math.Max(hi, s)
	}
	if hi-lo <= 0 {
		for i := range out {
			out[i] = 1
		}
		return out
	}
	for i, s := range scores {
		out[i] = NormaliseFloor + (1-NormaliseFloor)*(s-lo)/(hi-lo)
	}
	return out
}

// Decay is the recency factor 0.5^(age/halfLife). A non-positive half-life or
// a negative age (clock skew) means no decay.
func Decay(age, halfLife time.Duration) float64 {
	if halfLife <= 0 || age <= 0 {
		return 1
	}
	return math.Pow(0.5, age.Seconds()/halfLife.Seconds())
}

// Combine merges the two normalised channels: half each when vectors are
// available, FTS alone otherwise, times the recency factor.
func Combine(fts, vec float64, hasVectors bool, decay float64) float64 {
	base := fts
	if hasVectors {
		base = 0.5*fts + 0.5*vec
	}
	return base * decay
}

// Cosine is the cosine similarity of two vectors, 0 when either is empty or
// zero-length, and 0 on a dimension mismatch (a model swap must never panic).
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Words splits text into lowercased letter/digit tokens: the fallback
// similarity basis when an item has no vector.
func Words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// Jaccard is the set overlap of two token slices.
func Jaccard(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	sa := map[string]bool{}
	for _, w := range a {
		sa[w] = true
	}
	sb := map[string]bool{}
	for _, w := range b {
		sb[w] = true
	}
	inter := 0
	for w := range sa {
		if sb[w] {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// Candidate is one scored item as MMR sees it: a relevance score plus what is
// needed to measure similarity against the others.
type Candidate struct {
	ID    string // tie-breaker, so equal scores still order deterministically
	Score float64
	Vec   []float32
	Words []string
}

// Similarity is the similarity of two candidates: cosine when both carry a
// vector, Jaccard over their token sets otherwise.
func Similarity(a, b Candidate) float64 {
	if len(a.Vec) > 0 && len(b.Vec) > 0 {
		return Cosine(a.Vec, b.Vec)
	}
	return Jaccard(a.Words, b.Words)
}

// MMR selects at most limit candidates by maximal marginal relevance, so a
// pile of near-duplicates cannot fill the injection cap. The input is not
// modified and the result is deterministic: ties go to the lower id.
func MMR(cands []Candidate, lambda float64, limit int) []Candidate {
	if limit <= 0 || len(cands) == 0 {
		return nil
	}
	pool := make([]Candidate, len(cands))
	copy(pool, cands)
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].Score != pool[j].Score {
			return pool[i].Score > pool[j].Score
		}
		return pool[i].ID < pool[j].ID
	})
	if limit > len(pool) {
		limit = len(pool)
	}
	picked := make([]Candidate, 0, limit)
	taken := make([]bool, len(pool))
	for len(picked) < limit {
		best, bestScore := -1, math.Inf(-1)
		for i, c := range pool {
			if taken[i] {
				continue
			}
			maxSim := 0.0
			for _, p := range picked {
				if s := Similarity(c, p); s > maxSim {
					maxSim = s
				}
			}
			score := lambda*c.Score - (1-lambda)*maxSim
			// pool is already in score/id order, so ">" keeps the first
			// (highest-ranked) candidate on a tie.
			if score > bestScore {
				best, bestScore = i, score
			}
		}
		if best < 0 {
			break
		}
		taken[best] = true
		picked = append(picked, pool[best])
	}
	return picked
}
