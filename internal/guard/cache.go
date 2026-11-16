package guard

import (
	"container/list"
	"sync"
	"time"

	"holaclaw/internal/tool"
)

// Cache size and lifetime. A tool loop that retries the same call must not
// re-send the excerpt on every step; an entry that is old enough to have been
// re-approved by a human must not answer for the new call either.
const (
	cacheEntries = 256
	cacheTTL     = 10 * time.Minute
)

// cache is a small LRU of decided calls. Only verdicts are cached: an error
// is never an answer, so a failed classification is retried next time.
type cache struct {
	mu      sync.Mutex
	max     int
	ttl     time.Duration
	now     func() time.Time
	order   *list.List // front = most recently used; values are *entry
	entries map[string]*list.Element
}

type entry struct {
	key     string
	verdict tool.Verdict
	expires time.Time
}

func newCache(max int, ttl time.Duration, now func() time.Time) *cache {
	return &cache{max: max, ttl: ttl, now: now, order: list.New(), entries: map[string]*list.Element{}}
}

// get returns a live entry and marks it used. An expired entry is dropped.
// The usage is left out: a replayed verdict was not a call and was not paid
// for again.
func (c *cache) get(key string) (tool.Verdict, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return tool.Verdict{}, false
	}
	e, _ := el.Value.(*entry)
	if !c.now().Before(e.expires) {
		c.remove(el)
		return tool.Verdict{}, false
	}
	c.order.MoveToFront(el)
	v := e.verdict
	v.Usage = nil
	return v, true
}

// put stores a verdict, evicting the least recently used entry when full.
func (c *cache) put(key string, v tool.Verdict) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		e, _ := el.Value.(*entry)
		e.verdict, e.expires = v, c.now().Add(c.ttl)
		c.order.MoveToFront(el)
		return
	}
	for c.order.Len() >= c.max {
		if back := c.order.Back(); back != nil {
			c.remove(back)
		}
	}
	c.entries[key] = c.order.PushFront(&entry{key: key, verdict: v, expires: c.now().Add(c.ttl)})
}

// remove drops one element; the caller holds the lock.
func (c *cache) remove(el *list.Element) {
	e, _ := el.Value.(*entry)
	c.order.Remove(el)
	delete(c.entries, e.key)
}
