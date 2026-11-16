package event

import (
	"context"
	"slices"
	"sync"
	"testing"
)

func TestOrderedIDsFollowEmissionOrder(t *testing.T) {
	var got []ID
	sink := Ordered(SinkFunc(func(_ context.Context, e Event) { got = append(got, e.ID) }))
	// Create events first, emit them concurrently and in reverse.
	evs := make([]Event, 200)
	for i := range evs {
		evs[i] = New(RunStarted, NewID(), "", "", nil)
	}
	var wg sync.WaitGroup
	for i := len(evs) - 1; i >= 0; i-- {
		wg.Add(1)
		go func(e Event) { defer wg.Done(); sink.Emit(context.Background(), e) }(evs[i])
	}
	wg.Wait()
	if len(got) != len(evs) {
		t.Fatalf("got %d events", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("ids out of emission order at %d: %s then %s", i, got[i-1], got[i])
		}
	}
}

// TestTypesSorted keeps Types in name order and free of duplicates, so a new
// type goes where a reader expects it.
func TestTypesSorted(t *testing.T) {
	if !slices.IsSorted(Types) {
		t.Fatal("event.Types is not sorted by name")
	}
	if len(slices.Compact(slices.Clone(Types))) != len(Types) {
		t.Fatal("event.Types has a duplicate")
	}
}
