package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestClaimInbound(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	t0 := time.Date(2026, 10, 19, 14, 4, 36, 0, time.UTC)

	claim := func(ch string, at time.Time, keys ...string) bool {
		t.Helper()
		fresh, err := s.ClaimInbound(ctx, ch, keys, at)
		if err != nil {
			t.Fatalf("claim %v: %v", keys, err)
		}
		return fresh
	}
	if !claim("wa", t0, "phone|id1") {
		t.Fatal("first claim not fresh")
	}
	if claim("wa", t0.Add(time.Minute), "phone|id1") {
		t.Fatal("same key claimed twice")
	}
	if !claim("other", t0, "phone|id1") {
		t.Fatal("key on another channel instance is not fresh")
	}
	// A copy under another addressing overlaps on one key and records the other.
	if claim("wa", t0.Add(time.Minute), "lid|id1", "phone|id1") {
		t.Fatal("overlapping keys claimed as fresh")
	}
	if claim("wa", t0.Add(2*time.Minute), "lid|id1") {
		t.Fatal("the missing key of a duplicate was not recorded")
	}
	if !claim("wa", t0, "phone|id2") {
		t.Fatal("new id not fresh")
	}
	// No keys: always fresh, nothing recorded.
	for range 2 {
		if !claim("wa", t0) {
			t.Fatal("empty keys not fresh")
		}
	}
	var at int64
	if err := s.DB().QueryRow(`SELECT seen_at FROM inbound_seen WHERE channel = 'wa' AND key = ?`,
		inboundHash("phone|id1")).Scan(&at); err != nil {
		t.Fatal(err)
	}
	if at != t0.UnixMilli() {
		t.Fatalf("seen_at = %d, want the first sighting %d", at, t0.UnixMilli())
	}
	// Only hashes are stored, never a key in clear.
	var clear, odd int
	if err := s.DB().QueryRow(`SELECT count(*) FROM inbound_seen
		WHERE key IN (CAST('phone|id1' AS BLOB), CAST('lid|id1' AS BLOB), 'phone|id1', 'lid|id1')`).Scan(&clear); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT count(*) FROM inbound_seen WHERE length(key) != 32`).Scan(&odd); err != nil {
		t.Fatal(err)
	}
	if clear != 0 || odd != 0 {
		t.Fatalf("%d keys in clear, %d not 32 bytes", clear, odd)
	}
}

func TestClaimInboundConcurrent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// Two handles on one file, so the claim is atomic across connections and
	// not only under this process's writer lock.
	a, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	b, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()

	for round := range 20 {
		key := string(rune('a'+round)) + "|shared"
		var wg sync.WaitGroup
		results := make([]bool, 4)
		errs := make([]error, 4)
		for i := range 4 {
			s := a
			if i%2 == 1 {
				s = b
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				own := key + string(rune('0'+i))
				results[i], errs[i] = s.ClaimInbound(ctx, "wa", []string{own, key}, time.Now())
			}()
		}
		wg.Wait()
		fresh := 0
		for i := range 4 {
			if errs[i] != nil {
				t.Fatalf("round %d claim %d: %v", round, i, errs[i])
			}
			if results[i] {
				fresh++
			}
		}
		if fresh != 1 {
			t.Fatalf("round %d: %d fresh claims, want exactly 1", round, fresh)
		}
	}
}

func TestReleaseInbound(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now()
	keys := []string{"lid|id1", "phone|id1"}
	if fresh, err := s.ClaimInbound(ctx, "wa", keys, now); err != nil || !fresh {
		t.Fatalf("claim = %v, %v", fresh, err)
	}
	if err := s.ReleaseInbound(ctx, "wa", keys); err != nil {
		t.Fatal(err)
	}
	if fresh, err := s.ClaimInbound(ctx, "wa", keys[1:], now); err != nil || !fresh {
		t.Fatalf("claim after release = %v, %v", fresh, err)
	}
	if err := s.ReleaseInbound(ctx, "wa", nil); err != nil {
		t.Fatalf("release no keys: %v", err)
	}
}

func TestPruneInbound(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i, k := range []string{"old1", "old2", "new"} {
		at := t0
		if i == 2 {
			at = t0.Add(15 * 24 * time.Hour)
		}
		if _, err := s.ClaimInbound(ctx, "wa", []string{k}, at); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.PruneInbound(ctx, t0.Add(14*24*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("prune = %d, %v; want 2", n, err)
	}
	if fresh, _ := s.ClaimInbound(ctx, "wa", []string{"old1"}, t0); !fresh {
		t.Fatal("pruned key still recorded")
	}
	if fresh, _ := s.ClaimInbound(ctx, "wa", []string{"new"}, t0); fresh {
		t.Fatal("recent key pruned")
	}
}

func TestInboundClosedStore(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := s.ClaimInbound(ctx, "wa", []string{"k"}, time.Now()); err == nil {
		t.Fatal("claim on a closed store succeeded")
	}
	if err := s.ReleaseInbound(ctx, "wa", []string{"k"}); err == nil {
		t.Fatal("release on a closed store succeeded")
	}
	if _, err := s.PruneInbound(ctx, time.Now()); err == nil {
		t.Fatal("prune on a closed store succeeded")
	}
}
