package platform

import (
	"container/list"
	"sync"
	"time"
)

// LRUCache is a tenant-aware LRU cache with TTL-based expiry and a hard cap
// on entry count. Keys are free-form strings; the recommended convention is
// "<tenant_id>:<resource_key>" so that entries for inactive tenants age out
// naturally (zero-idle-cost: they consume no memory once evicted).
//
// The cache is safe for concurrent use. The implementation is a doubly-linked
// list + hash map — O(1) get/set, O(1) LRU eviction.
type LRUCache struct {
	mu         sync.Mutex
	maxEntries int
	ttl        time.Duration
	now        func() time.Time
	order      *list.List
	index      map[string]*list.Element
}

type cacheEntry struct {
	key       string
	value     any
	expiresAt time.Time
}

// NewLRUCache constructs an LRUCache bounded at maxEntries items with the
// supplied TTL. Both must be positive; zero or negative values are clamped to
// sensible defaults.
func NewLRUCache(maxEntries int, ttl time.Duration) *LRUCache {
	if maxEntries <= 0 {
		maxEntries = 1024
	}
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &LRUCache{
		maxEntries: maxEntries,
		ttl:        ttl,
		now:        time.Now,
		order:      list.New(),
		index:      make(map[string]*list.Element, maxEntries),
	}
}

// Get returns the cached value for key, or (nil, false) if the key is absent
// or expired. Access promotes the entry to most-recently-used.
func (c *LRUCache) Get(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.index[key]
	if !ok {
		return nil, false
	}
	entry := elem.Value.(*cacheEntry)
	if c.now().After(entry.expiresAt) {
		c.removeElement(elem)
		return nil, false
	}
	c.order.MoveToFront(elem)
	return entry.value, true
}

// Set stores value under key with the cache's TTL, evicting the oldest entry
// if the cache is at capacity.
func (c *LRUCache) Set(key string, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.index[key]; ok {
		entry := elem.Value.(*cacheEntry)
		entry.value = value
		entry.expiresAt = c.now().Add(c.ttl)
		c.order.MoveToFront(elem)
		return
	}
	entry := &cacheEntry{
		key:       key,
		value:     value,
		expiresAt: c.now().Add(c.ttl),
	}
	elem := c.order.PushFront(entry)
	c.index[key] = elem
	if c.order.Len() > c.maxEntries {
		oldest := c.order.Back()
		if oldest != nil {
			c.removeElement(oldest)
		}
	}
}

// Delete removes the entry for key, if any.
func (c *LRUCache) Delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.index[key]; ok {
		c.removeElement(elem)
	}
}

// Len returns the current number of live entries (including expired ones
// that have not yet been reaped). It is primarily useful for tests and
// metrics.
func (c *LRUCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// Purge empties the cache.
func (c *LRUCache) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order.Init()
	c.index = make(map[string]*list.Element, c.maxEntries)
}

func (c *LRUCache) removeElement(elem *list.Element) {
	entry := elem.Value.(*cacheEntry)
	c.order.Remove(elem)
	delete(c.index, entry.key)
}
