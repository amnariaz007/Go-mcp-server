// Package cache implements a bounded, thread-safe, in-memory TTL cache with
// least-recently-used eviction. Only successful results should be stored;
// callers must not cache transient failures.
package cache

import (
	"container/list"
	"sync"
	"time"
)

// Cache is a generic TTL + LRU cache. The zero value is not usable; use New.
type Cache[V any] struct {
	mu         sync.Mutex
	ttl        time.Duration
	maxEntries int
	ll         *list.List
	items      map[string]*list.Element
	now        func() time.Time
}

type entry[V any] struct {
	key     string
	value   V
	expires time.Time
}

// New creates a cache whose entries expire after ttl and which holds at most
// maxEntries entries (the least recently used entry is evicted first).
func New[V any](ttl time.Duration, maxEntries int) *Cache[V] {
	if maxEntries < 1 {
		maxEntries = 1
	}
	return &Cache[V]{
		ttl:        ttl,
		maxEntries: maxEntries,
		ll:         list.New(),
		items:      make(map[string]*list.Element),
		now:        time.Now,
	}
}

// Get returns the value for key if present and not expired.
func (c *Cache[V]) Get(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	el, ok := c.items[key]
	if !ok {
		return zero, false
	}
	e := el.Value.(*entry[V])
	if !c.now().Before(e.expires) {
		c.removeElement(el)
		return zero, false
	}
	c.ll.MoveToFront(el)
	return e.value, true
}

// Set stores value under key, replacing any existing entry.
func (c *Cache[V]) Set(key string, value V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp := c.now().Add(c.ttl)
	if el, ok := c.items[key]; ok {
		e := el.Value.(*entry[V])
		e.value, e.expires = value, exp
		c.ll.MoveToFront(el)
		return
	}
	c.items[key] = c.ll.PushFront(&entry[V]{key: key, value: value, expires: exp})
	for c.ll.Len() > c.maxEntries {
		c.removeElement(c.ll.Back())
	}
}

// Len reports the number of stored entries, including not-yet-purged expired ones.
func (c *Cache[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

func (c *Cache[V]) removeElement(el *list.Element) {
	c.ll.Remove(el)
	delete(c.items, el.Value.(*entry[V]).key)
}
