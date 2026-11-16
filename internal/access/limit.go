package access

import (
	"sync"
	"time"
)

// limiter is an in-memory sliding-window counter per key. Every limit in this
// package is a flood guard, not a security boundary: a restart forgives, the
// way auth's redeem limiter does.
type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

// maxKeys caps the map so a flood from many forged senders cannot grow it
// without bound; the sweep drops keys whose window has passed.
const maxKeys = 4096

func newLimiter() *limiter { return &limiter{hits: map[string][]time.Time{}} }

// allow reports whether every key is still under limit within window and, when
// it is, records one hit on each. All keys must pass: a sender is throttled as
// soon as one of its aliases is.
func (l *limiter) allow(keys []string, now time.Time, limit int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) > maxKeys {
		l.sweepLocked(now, window)
	}
	for _, k := range keys {
		if len(l.pruneLocked(k, now, window)) >= limit {
			return false
		}
	}
	for _, k := range keys {
		l.hits[k] = append(l.hits[k], now)
	}
	return true
}

// pruneLocked drops hits older than window; mu must be held.
func (l *limiter) pruneLocked(key string, now time.Time, window time.Duration) []time.Time {
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.hits, key)
		return nil
	}
	l.hits[key] = kept
	return kept
}

func (l *limiter) sweepLocked(now time.Time, window time.Duration) {
	for k := range l.hits {
		l.pruneLocked(k, now, window)
	}
}
