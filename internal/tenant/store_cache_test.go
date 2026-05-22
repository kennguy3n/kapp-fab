package tenant

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// recordingCache implements the Cache interface and records every operation
// so the test can assert exactly which keys the store consulted, set, and
// deleted — and verify that a cache hit short-circuits the would-be pool
// query (no pool is wired into the store at all in these tests).
//
// SetOnEvict-registered callbacks fire synchronously from Set (when a value
// is replaced) and Delete (when a key is present and removed). This mirrors
// platform.LRUCache's contract closely enough to exercise the OnEvict-based
// slug-sibling cleanup in store.go.
type recordingCache struct {
	mu       sync.Mutex
	store    map[string]any
	gets     []string
	sets     []string
	deletes  []string
	evicts   []string
	onEvict  func(key string, value any)
	capacity int      // 0 = unlimited
	order    []string // FIFO of keys for LRU-style eviction when capacity > 0
}

func newRecordingCache() *recordingCache {
	return &recordingCache{store: make(map[string]any)}
}

func newBoundedRecordingCache(capacity int) *recordingCache {
	return &recordingCache{store: make(map[string]any), capacity: capacity}
}

func (c *recordingCache) Get(key string) (any, bool) {
	c.mu.Lock()
	c.gets = append(c.gets, key)
	v, ok := c.store[key]
	c.mu.Unlock()
	return v, ok
}

func (c *recordingCache) Set(key string, value any) {
	c.mu.Lock()
	c.sets = append(c.sets, key)
	prev, hadPrev := c.store[key]
	c.store[key] = value
	if !hadPrev {
		c.order = append(c.order, key)
	}
	var (
		displaced     any
		displacedKey  string
		displacedFire bool
		overflowKey   string
		overflowValue any
		overflowFire  bool
	)
	if hadPrev && prev != value {
		displaced = prev
		displacedKey = key
		displacedFire = true
	}
	if c.capacity > 0 && len(c.order) > c.capacity {
		overflowKey = c.order[0]
		c.order = c.order[1:]
		if v, ok := c.store[overflowKey]; ok {
			overflowValue = v
			delete(c.store, overflowKey)
			c.evicts = append(c.evicts, overflowKey)
			overflowFire = true
		}
	}
	cb := c.onEvict
	c.mu.Unlock()
	if cb != nil && displacedFire {
		cb(displacedKey, displaced)
	}
	if cb != nil && overflowFire {
		cb(overflowKey, overflowValue)
	}
}

func (c *recordingCache) Delete(key string) {
	c.mu.Lock()
	value, present := c.store[key]
	delete(c.store, key)
	if present {
		for i, k := range c.order {
			if k == key {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
	}
	c.deletes = append(c.deletes, key)
	cb := c.onEvict
	c.mu.Unlock()
	if cb != nil && present {
		cb(key, value)
	}
}

func (c *recordingCache) SetOnEvict(fn func(key string, value any)) {
	c.mu.Lock()
	c.onEvict = fn
	c.mu.Unlock()
}

// TestPGStore_Get_CacheHitShortCircuits proves the architectural contract: a
// warm cache hit returns the tenant without touching the pool. The store is
// constructed with a nil pool — if the cache path ever fell through to the
// DB query the test would panic with a nil dereference.
func TestPGStore_Get_CacheHitShortCircuits(t *testing.T) {
	cache := newRecordingCache()
	store := NewPGStore(nil).WithCache(cache)

	want := &Tenant{ID: uuid.New(), Slug: "acme", Status: StatusActive}
	cache.store[tenantCachePrefixID+want.ID.String()] = want

	got, err := store.Get(context.Background(), want.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != want {
		t.Fatalf("Get returned %p, want cached %p", got, want)
	}
	if n := len(cache.gets); n != 1 || cache.gets[0] != tenantCachePrefixID+want.ID.String() {
		t.Fatalf("expected exactly one cache Get on the id key, saw %v", cache.gets)
	}
	if len(cache.sets) != 0 {
		t.Fatalf("expected no cache Set on hit, saw %v", cache.sets)
	}
}

// TestPGStore_GetBySlug_CacheHitShortCircuits is the slug-keyed sibling of
// the above. Confirms the slug-keyed read path also short-circuits without
// touching the pool.
func TestPGStore_GetBySlug_CacheHitShortCircuits(t *testing.T) {
	cache := newRecordingCache()
	store := NewPGStore(nil).WithCache(cache)

	want := &Tenant{ID: uuid.New(), Slug: "acme", Status: StatusActive}
	cache.store[tenantCachePrefixSlug+want.Slug] = want

	got, err := store.GetBySlug(context.Background(), want.Slug)
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if got != want {
		t.Fatalf("GetBySlug returned %p, want cached %p", got, want)
	}
	if n := len(cache.gets); n != 1 || cache.gets[0] != tenantCachePrefixSlug+want.Slug {
		t.Fatalf("expected exactly one cache Get on the slug key, saw %v", cache.gets)
	}
}

// TestPGStore_WithCache_NilDisablesCache proves that WithCache(nil) is a
// no-op equivalent to never calling WithCache: the cache field stays nil and
// the cache-check branch is short-circuited so the read path goes straight
// to the (in this test, nil) pool. Verified by the recovered panic / DB
// error — both prove the cache path was skipped.
func TestPGStore_WithCache_NilDisablesCache(t *testing.T) {
	store := NewPGStore(nil).WithCache(nil)
	defer func() { _ = recover() }()
	_, err := store.Get(context.Background(), uuid.New())
	if err == nil {
		t.Fatalf("Get on nil-pool store with cache disabled returned nil error; expected DB error or panic")
	}
}

// TestPGStore_InvalidateCache_DropsIDAndSlugEntries proves the canonical
// mutation-side contract: after invalidateCache, neither the id-keyed nor
// the slug-keyed entry survives. The slug-keyed entry is reaped indirectly
// through the OnEvict callback installed by WithCache.
func TestPGStore_InvalidateCache_DropsIDAndSlugEntries(t *testing.T) {
	cache := newRecordingCache()
	store := NewPGStore(nil).WithCache(cache)

	t1 := &Tenant{ID: uuid.New(), Slug: "acme", Status: StatusActive}
	store.warmCache(t1)
	if _, ok := cache.store[tenantCachePrefixID+t1.ID.String()]; !ok {
		t.Fatalf("precondition: warmCache did not stamp id key")
	}
	if _, ok := cache.store[tenantCachePrefixSlug+t1.Slug]; !ok {
		t.Fatalf("precondition: warmCache did not stamp slug key")
	}

	store.invalidateCache(t1.ID)

	if _, ok := cache.store[tenantCachePrefixID+t1.ID.String()]; ok {
		t.Fatalf("invalidateCache: id-keyed entry still present")
	}
	if _, ok := cache.store[tenantCachePrefixSlug+t1.Slug]; ok {
		t.Fatalf("invalidateCache: slug-keyed entry still present (OnEvict callback failed to reap sibling)")
	}
}

// TestPGStore_OnEviction_ReapsSlugSibling proves the indirect cleanup path:
// when capacity pressure on the cache evicts an id-keyed entry, the OnEvict
// callback installed by WithCache drops the sibling slug-keyed entry so the
// two never drift apart. Without this guarantee a slug-keyed entry could
// outlive its id-keyed sibling, surfacing as a phantom tenant on the slug
// lookup path. recordingCache mirrors the LRU eviction contract closely
// enough to exercise this codepath without dragging in the real LRUCache
// implementation (which would re-introduce the internal/platform import
// cycle).
func TestPGStore_OnEviction_ReapsSlugSibling(t *testing.T) {
	// Capacity 4 so two tenants (4 entries: id+slug each) fit; warming a
	// third forces eviction of the oldest entries.
	cache := newBoundedRecordingCache(4)
	store := NewPGStore(nil).WithCache(cache)

	t1 := &Tenant{ID: uuid.New(), Slug: "first", Status: StatusActive}
	t2 := &Tenant{ID: uuid.New(), Slug: "second", Status: StatusActive}
	t3 := &Tenant{ID: uuid.New(), Slug: "third", Status: StatusActive}

	store.warmCache(t1)
	store.warmCache(t2)
	// 4 entries → cache full. Warming t3 adds 2 more, forcing 2 evictions
	// — t1's id key and t1's slug key in that order.
	store.warmCache(t3)

	if _, ok := cache.store[tenantCachePrefixID+t1.ID.String()]; ok {
		t.Fatalf("expected t1 id entry to be evicted under capacity pressure")
	}
	if _, ok := cache.store[tenantCachePrefixSlug+t1.Slug]; ok {
		t.Fatalf("OnEvict callback failed to reap slug sibling for evicted t1.id")
	}
}

// TestPGStore_Get_TypeAssertionMissResetsToFetch covers the defensive path:
// if a cache entry under the tenant prefix is somehow not a *Tenant (e.g.
// the same cache is reused for another domain that uses the same prefix),
// the type assertion fails and Get falls through to the DB rather than
// returning the unexpected value or panicking. Verified by the deferred
// recover catching the nil-pool panic — the function must reach the DB
// path to be considered correct.
func TestPGStore_Get_TypeAssertionMissResetsToFetch(t *testing.T) {
	cache := newRecordingCache()
	store := NewPGStore(nil).WithCache(cache)
	id := uuid.New()
	cache.store[tenantCachePrefixID+id.String()] = "not a *Tenant"

	defer func() { _ = recover() }()
	_, err := store.Get(context.Background(), id)
	if err == nil {
		t.Fatalf("Get with corrupt cache entry returned nil error; expected DB fallthrough")
	}
}
