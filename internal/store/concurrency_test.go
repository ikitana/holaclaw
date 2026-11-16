package store

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"holaclaw/internal/event"
)

func readerOf(s string) io.Reader { return strings.NewReader(s) }

var errDuplicateInsert = errors.New("store: duplicate id reported as inserted")

// TestConcurrentWrites hammers the writer lock from many goroutines: every
// message and event must land exactly once.
func TestConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sess := mustSession(t, s, "main/tg-main/concurrent")

	const workers, each = 8, 25
	var wg sync.WaitGroup
	var inserted atomic.Int64
	errs := make(chan error, workers*each)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				m := msg(sess.ID, event.NewID(), event.RoleUser, "x")
				ok, err := s.AppendMessage(ctx, m, i%2 == 0)
				if err != nil {
					errs <- err
					continue
				}
				if ok {
					inserted.Add(1)
				}
				// The same id twice must never insert again.
				if ok, err := s.AppendMessage(ctx, m, false); err != nil {
					errs <- err
				} else if ok {
					errs <- errDuplicateInsert
				}
				s.Emit(ctx, event.New(event.ModelRequest, m.TraceID, sess.ID, "", nil))
				s.Emit(ctx, event.New(event.ModelToken, m.TraceID, sess.ID, "", nil)) // dropped
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := inserted.Load(); got != workers*each {
		t.Fatalf("inserted = %d, want %d", got, workers*each)
	}
	var messages, events int
	if err := s.DB().QueryRow(`SELECT count(*) FROM messages`).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if messages != workers*each || events != workers*each {
		t.Fatalf("messages = %d, events = %d, want %d each", messages, events, workers*each)
	}

	// Pending entries are taken exactly once across parallel consumers.
	var taken atomic.Int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ms, err := s.TakePending(ctx, sess.ID, false)
			if err != nil {
				errs <- err
				return
			}
			taken.Add(int64(len(ms)))
		}()
	}
	wg.Wait()
	if got, want := taken.Load(), int64(workers*((each+1)/2)); got != want {
		t.Fatalf("taken = %d, want %d", got, want)
	}
}

// TestConcurrentEnsureSession is the registry-lookup invariant: one creator,
// one row, whatever the race (docs/sessions.md).
func TestConcurrentEnsureSession(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	const workers = 16
	var wg sync.WaitGroup
	var created atomic.Int64
	ids := make([]event.ID, workers)
	errs := make(chan error, workers)
	start := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			sess, isNew, err := s.EnsureSession(ctx, Session{Key: "main/tg-main/race", Agent: "main", Kind: SessionChat})
			if err != nil {
				errs <- err
				return
			}
			if isNew {
				created.Add(1)
			}
			ids[i] = sess.ID
		}(w)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := created.Load(); got != 1 {
		t.Fatalf("created = %d, want exactly 1", got)
	}
	for i, id := range ids {
		if id == "" || id != ids[0] {
			t.Fatalf("worker %d saw session %q, want %q", i, id, ids[0])
		}
	}
	list, err := s.ListSessions(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("sessions = %v, err %v", list, err)
	}
}

// TestConcurrentBeginTurn: exactly one goroutine claims the turn.
func TestConcurrentBeginTurn(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	sess := mustSession(t, s, "main/tg-main/claim")

	const workers = 12
	var wg sync.WaitGroup
	var claimed atomic.Int64
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := s.BeginTurn(ctx, sess.ID); err == nil {
				claimed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := claimed.Load(); got != 1 {
		t.Fatalf("claimed = %d, want 1", got)
	}
	got, err := s.GetSession(ctx, sess.ID)
	if err != nil || got.RunGeneration != 1 {
		t.Fatalf("generation = %d, err %v", got.RunGeneration, err)
	}
}

// TestConcurrentPutBlob: identical content from many goroutines dedupes to one
// row and one file.
func TestConcurrentPutBlob(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	const workers = 10
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.PutBlob(ctx, readerOf("same bytes"), "text/plain"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var n int
	if err := s.DB().QueryRow(`SELECT count(*) FROM blobs`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("blob rows = %d, err %v", n, err)
	}
}
