package store

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTakeDaily(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	day := time.Date(2026, 9, 19, 23, 30, 0, 0, time.UTC)

	if ok, err := s.TakeDaily(ctx, "main", "images", day, 0); err != nil || ok {
		t.Fatalf("limit 0: ok=%v err=%v", ok, err)
	}
	var taken atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.TakeDaily(ctx, "main", "images", day, 3)
			if err != nil {
				t.Error(err)
			}
			if ok {
				taken.Add(1)
			}
		}()
	}
	wg.Wait()
	if taken.Load() != 3 {
		t.Fatalf("taken = %d, want 3", taken.Load())
	}
	// Another agent, another key and the next day each have their own count.
	for _, c := range []struct {
		agent, key string
		at         time.Time
	}{{"other", "images", day}, {"main", "other", day}, {"main", "images", day.Add(time.Hour)}} {
		if ok, err := s.TakeDaily(ctx, c.agent, c.key, c.at, 3); err != nil || !ok {
			t.Fatalf("%+v: ok=%v err=%v", c, ok, err)
		}
	}
	if err := s.ReturnDaily(ctx, "main", "images", day); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.TakeDaily(ctx, "main", "images", day, 3); !ok {
		t.Fatal("a returned use must be available again")
	}
	if ok, _ := s.TakeDaily(ctx, "main", "images", day, 3); ok {
		t.Fatal("over the limit")
	}
}
