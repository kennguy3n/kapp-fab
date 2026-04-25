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
// An optional OnEvict callback is invoked outside the lock whenever a value
// leaves the cache (capacity-driven eviction, TTL expiry, explicit Delete,
// or Purge) so callers that hold expensive resources — e.g. an S3 client's
// idle connection pool — can tear them down deterministically.
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
	onEvict    func(key string, value any)
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

// SetOnEvict registers an eviction callback. Calling SetOnEvict more than
// once replaces the prior callback. The callback runs synchronously after
// the entry has been removed and the lock has been released, so callbacks
// may safely call back into the cache (Get/Set/Delete) without deadlocking.
func (c *LRUCache) SetOnEvict(fn func(key string, value any)) {
	c.mu.Lock()
	c.onEvict = fn
	c.mu.Unlock()
}

// SetClock overrides the time source used to compute and check entry
// expiry. Intended for tests that need deterministic TTL-driven
// eviction without sleeping. A nil argument restores time.Now.
func (c *LRUCache) SetClock(now func() time.Time) {
	c.mu.Lock()
	if now == nil {
		c.now = time.Now
	} else {
		c.now = now
	}
	c.mu.Unlock()
}

// Get returns the cached value for key, or (nil, false) if the key is absent
// or expired. Access promotes the entry to most-recently-used.
func (c *LRUCache) Get(key string) (any, bool) {
	c.mu.Lock()
	var evicted *cacheEntry
	elem, ok := c.index[key]
	if !ok {
		c.mu.Unlock()
		return nil, false
	}
	entry := elem.Value.(*cacheEntry)
	if c.now().After(entry.expiresAt) {
		evicted = entry
		c.removeElement(elem)
		c.mu.Unlock()
		c.fireEvict(evicted)
		return nil, false
	}
	c.order.MoveToFront(elem)
	value := entry.value
	c.mu.Unlock()
	return value, true
}

// Set stores value under key with the cache's TTL, evicting the oldest entry
// if the cache is at capacity.
func (c *LRUCache) Set(key string, value any) {
	c.mu.Lock()
	var evicted *cacheEntry
	if elem, ok := c.index[key]; ok {
		entry := elem.Value.(*cacheEntry)
		// If the value pointer is being replaced (not just refreshed
		// with the same instance), surface the displaced value through
		// OnEvict so callers like PerTenantS3Store can close idle
		// transports rather than leak them under concurrent miss-then-Set.
		if entry.value != value {
			evicted = &cacheEntry{key: entry.key, value: entry.value}
		}
		entry.value = value
		entry.expiresAt = c.now().Add(c.ttl)
		c.order.MoveToFront(elem)
		c.mu.Unlock()
		c.fireEvict(evicted)
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
			evicted = oldest.Value.(*cacheEntry)
			c.removeElement(oldest)
		}
	}
	c.mu.Unlock()
	c.fireEvict(evicted)
}

// Delete removes the entry for key, if any.
func (c *LRUCache) Delete(key string) {
	c.mu.Lock()
	var evicted *cacheEntry
	if elem, ok := c.index[key]; ok {
		evicted = elem.Value.(*cacheEntry)
		c.removeElement(elem)
	}
	c.mu.Unlock()
	c.fireEvict(evicted)
}

// Len returns the current number of live entries (including expired ones
// that have not yet been reaped). It is primarily useful for tests and
// metrics.
func (c *LRUCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// Purge empties the cache. The OnEvict callback fires once per entry that
// was present at the time of the call.
func (c *LRUCache) Purge() {
	c.mu.Lock()
	var evicted []*cacheEntry
	if c.onEvict != nil {
		evicted = make([]*cacheEntry, 0, c.order.Len())
		for e := c.order.Front(); e != nil; e = e.Next() {
			evicted = append(evicted, e.Value.(*cacheEntry))
		}
	}
	c.order.Init()
	c.index = make(map[string]*list.Element, c.maxEntries)
	c.mu.Unlock()
	for _, ent := range evicted {
		c.fireEvict(ent)
	}
}

// fireEvict invokes the eviction callback (if any) on the supplied entry.
// Safe to call with a nil entry. Callers must release the lock before
// invoking so the callback can safely call back into the cache.
func (c *LRUCache) fireEvict(entry *cacheEntry) {
	if entry == nil {
		return
	}
	c.mu.Lock()
	fn := c.onEvict
	c.mu.Unlock()
	if fn != nil {
		fn(entry.key, entry.value)
	}
}

func (c *LRUCache) removeElement(elem *list.Element) {
	entry := elem.Value.(*cacheEntry)
	c.order.Remove(elem)
	delete(c.index, entry.key)
}
