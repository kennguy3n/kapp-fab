package bundle

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/runtime"
)

// DefaultResolverCacheSize bounds the in-process LRU when callers
// do not specify their own. Each entry holds a fully-extracted
// *runtime.ResolvedBundle whose backing []byte buffers are bounded
// by the per-file (2 MiB) and per-bundle (10 MiB) caps the
// resolver already enforces, so 256 entries put the upper bound
// at roughly 2.5 GiB of headroom — well above any realistic
// working set of in-use extension versions per replica.
const DefaultResolverCacheSize = 256

// CachingResolver wraps another Resolver with a bounded in-process
// LRU keyed by version.ID. Bundles are immutable post-publish (the
// version row is write-once via the BEFORE UPDATE trigger added in
// migration 000068) so cache invalidation reduces to LRU eviction —
// there is no semantic "stale" case to worry about.
//
// The hot path this addresses is the B6 settings PATCH endpoint:
// every PATCH would otherwise re-fetch the full bundle from the
// CDN just to extract its SettingsSchemaJSON. With caching, the
// install fetch primes the cache and every subsequent PATCH on
// the same replica hits memory.
//
// CachingResolver is safe for concurrent use. The cache is per-
// process; replicated API replicas each warm their own cache. This
// is the same per-process semantic as eventrouter.RateLimiter and
// is acceptable here because the cache is a performance optimisation
// rather than a coherence guarantee — the underlying resolver still
// verifies the bundle hash on every fetch into the cache.
type CachingResolver struct {
	inner    Resolver
	capacity int

	mu      sync.Mutex
	entries map[string]*list.Element // version.ID → list element
	order   *list.List               // front = MRU, back = LRU
}

type cacheEntry struct {
	key    string
	bundle *runtime.ResolvedBundle
}

// NewCachingResolver wraps inner with an LRU of the given capacity.
// A capacity ≤ 0 falls back to DefaultResolverCacheSize. Panics if
// inner is nil — a caching wrapper around a missing resolver is
// always a programming error.
func NewCachingResolver(inner Resolver, capacity int) *CachingResolver {
	if inner == nil {
		panic("bundle: NewCachingResolver: nil inner resolver")
	}
	if capacity <= 0 {
		capacity = DefaultResolverCacheSize
	}
	return &CachingResolver{
		inner:    inner,
		capacity: capacity,
		entries:  make(map[string]*list.Element, capacity),
		order:    list.New(),
	}
}

// Resolve returns the bundle for version, fetching via the wrapped
// resolver on cache miss. Errors from the inner resolver are NOT
// cached — a transient CDN 5xx must not pin a sentinel error in
// memory until eviction. Only successful fetches enter the cache.
//
// The cache key is version.ID (a UUID). The version.BundleHash is
// not part of the key because the version row is immutable once
// inserted — the hash cannot change for a given ID. If a future
// migration ever relaxes this immutability, the cache key should
// be widened to (ID, BundleHash) so a hash drift evicts stale
// entries automatically.
func (c *CachingResolver) Resolve(ctx context.Context, version *marketplace.ExtensionVersion) (*runtime.ResolvedBundle, error) {
	if version == nil {
		return nil, errors.New("bundle: nil version")
	}
	key := version.ID.String()
	if key == "" || key == "00000000-0000-0000-0000-000000000000" {
		// Defence-in-depth: a zero UUID would collapse all
		// uncached bundles into a single cache slot. Skip the
		// cache entirely and surface the inner resolver's
		// validation error.
		return c.inner.Resolve(ctx, version)
	}
	if hit, ok := c.get(key); ok {
		return hit, nil
	}
	rb, err := c.inner.Resolve(ctx, version)
	if err != nil {
		return nil, err
	}
	c.put(key, rb)
	return rb, nil
}

// Invalidate evicts the cache entry for version. Exposed primarily
// for tests; production has no need to invalidate because version
// rows are immutable. A B7 admin operation that re-uploads a
// bundle for an existing version (which the trigger blocks) would
// be the only theoretical caller.
func (c *CachingResolver) Invalidate(version *marketplace.ExtensionVersion) {
	if version == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := version.ID.String()
	if el, ok := c.entries[key]; ok {
		c.order.Remove(el)
		delete(c.entries, key)
	}
}

// Len returns the current cache occupancy. Exposed primarily for
// tests + future operational metrics (cache-hit ratio).
func (c *CachingResolver) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func (c *CachingResolver) get(key string) (*runtime.ResolvedBundle, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	// The list is private to this struct; every push writes a
	// *cacheEntry. A failed type-assert here would mean memory
	// corruption — panic loudly rather than silently returning a
	// false-cache-miss.
	ce, ok := el.Value.(*cacheEntry)
	if !ok {
		panic(fmt.Sprintf("bundle: caching resolver list element is %T, want *cacheEntry", el.Value))
	}
	return ce.bundle, true
}

func (c *CachingResolver) put(key string, rb *runtime.ResolvedBundle) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		// Concurrent racer beat us to it — update value and
		// promote to MRU so we don't fragment the cache.
		el.Value = &cacheEntry{key: key, bundle: rb}
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&cacheEntry{key: key, bundle: rb})
	c.entries[key] = el
	for len(c.entries) > c.capacity {
		oldest := c.order.Back()
		if oldest == nil {
			// capacity ≤ 0 was guarded at construction; reaching
			// this branch means a memory-corruption-class bug.
			panic(fmt.Sprintf("bundle: caching resolver LRU empty but len=%d > capacity=%d",
				len(c.entries), c.capacity))
		}
		c.order.Remove(oldest)
		// Type-assertion safety: see get() — list elements are
		// always *cacheEntry by construction.
		ce, ok := oldest.Value.(*cacheEntry)
		if !ok {
			panic(fmt.Sprintf("bundle: caching resolver list element is %T, want *cacheEntry", oldest.Value))
		}
		delete(c.entries, ce.key)
	}
}
