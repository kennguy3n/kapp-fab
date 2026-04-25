package files

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeResolver is a minimal TenantResolver that hands out per-tenant
// S3StoreConfig values without contacting the real ZK fabric. The
// underlying *S3Store is constructed against a no-network endpoint
// (the AWS SDK does not contact the bucket on construction), which
// is sufficient to exercise the LRU cache + OnEvict pipeline.
type fakeResolver struct {
	mu       sync.Mutex
	provided map[uuid.UUID]int
	endpoint string
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{
		provided: make(map[uuid.UUID]int),
		endpoint: "https://invalid.example.invalid", // never dialed
	}
}

func (f *fakeResolver) ResolveZKCredentials(_ context.Context, tenantID uuid.UUID) (S3StoreConfig, bool, error) {
	f.mu.Lock()
	f.provided[tenantID]++
	f.mu.Unlock()
	return S3StoreConfig{
		Endpoint:  f.endpoint,
		Region:    "us-east-1",
		Bucket:    "tenant-" + tenantID.String(),
		AccessKey: "ak",
		SecretKey: "sk",
	}, true, nil
}

// resolveCount returns how many times credentials for tenantID were
// re-resolved. A jump > 1 implies the LRU evicted (or never cached)
// the prior store.
func (f *fakeResolver) resolveCount(tenantID uuid.UUID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.provided[tenantID]
}

// TestPerTenantStoreCapacityEviction verifies the LRU eviction
// fires once MaxEntries is exceeded — the evicted *S3Store is
// surfaced through PerTenantConfig.OnEvict (which the production
// path uses for metrics) and the cache size is bounded.
func TestPerTenantStoreCapacityEviction(t *testing.T) {
	resolver := newFakeResolver()

	var (
		mu       sync.Mutex
		evicted  []uuid.UUID
		evicted2 []*S3Store
	)
	store, err := NewPerTenantS3Store(PerTenantConfig{
		Resolver:   resolver,
		Fallback:   NewMemoryStore(),
		MaxEntries: 2,
		IdleTTL:    time.Hour,
		OnEvict: func(id uuid.UUID, s *S3Store) {
			mu.Lock()
			evicted = append(evicted, id)
			evicted2 = append(evicted2, s)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("NewPerTenantS3Store: %v", err)
	}

	tenantA := uuid.New()
	tenantB := uuid.New()
	tenantC := uuid.New()

	for _, id := range []uuid.UUID{tenantA, tenantB, tenantC} {
		ctx := WithTenant(context.Background(), id)
		if _, err := store.routeFor(ctx); err != nil {
			t.Fatalf("routeFor(%s): %v", id, err)
		}
	}

	if got := store.CachedLen(); got != 2 {
		t.Fatalf("cached len after 3 inserts with cap=2: got %d, want 2", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(evicted) != 1 {
		t.Fatalf("expected exactly one eviction, got %d", len(evicted))
	}
	if evicted[0] != tenantA {
		t.Fatalf("expected oldest tenant (%s) to evict, got %s", tenantA, evicted[0])
	}
	if evicted2[0] == nil {
		t.Fatalf("OnEvict received nil *S3Store")
	}
}

// TestPerTenantStoreOnEvictReceivesCorrectStore verifies the OnEvict
// callback is handed the same *S3Store instance that originally
// served tenant A — i.e. the cache value is not corrupted between
// Set and the eviction callback.
func TestPerTenantStoreOnEvictReceivesCorrectStore(t *testing.T) {
	resolver := newFakeResolver()

	var (
		mu       sync.Mutex
		captured *S3Store
	)
	store, err := NewPerTenantS3Store(PerTenantConfig{
		Resolver:   resolver,
		Fallback:   NewMemoryStore(),
		MaxEntries: 1,
		IdleTTL:    time.Hour,
		OnEvict: func(_ uuid.UUID, s *S3Store) {
			mu.Lock()
			captured = s
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("NewPerTenantS3Store: %v", err)
	}

	tenantA := uuid.New()
	ctx := WithTenant(context.Background(), tenantA)
	got, err := store.routeFor(ctx)
	if err != nil {
		t.Fatalf("routeFor: %v", err)
	}
	storeA, ok := got.(*S3Store)
	if !ok {
		t.Fatalf("routeFor returned %T, want *S3Store", got)
	}

	// Push tenantA out of the LRU.
	tenantB := uuid.New()
	if _, err := store.routeFor(WithTenant(context.Background(), tenantB)); err != nil {
		t.Fatalf("routeFor(B): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if captured == nil {
		t.Fatalf("OnEvict was not called")
	}
	if captured != storeA {
		t.Fatalf("OnEvict received a different *S3Store than the one cached for tenant A")
	}
}

// TestPerTenantStoreInvalidateCallsOnEvict verifies Invalidate drops
// the cached entry and surfaces it through OnEvict so callers can
// close the underlying transport deterministically.
func TestPerTenantStoreInvalidateCallsOnEvict(t *testing.T) {
	resolver := newFakeResolver()

	var (
		mu      sync.Mutex
		evicted []uuid.UUID
	)
	store, err := NewPerTenantS3Store(PerTenantConfig{
		Resolver:   resolver,
		Fallback:   NewMemoryStore(),
		MaxEntries: 8,
		IdleTTL:    time.Hour,
		OnEvict: func(id uuid.UUID, _ *S3Store) {
			mu.Lock()
			evicted = append(evicted, id)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("NewPerTenantS3Store: %v", err)
	}

	tenantA := uuid.New()
	if _, err := store.routeFor(WithTenant(context.Background(), tenantA)); err != nil {
		t.Fatalf("routeFor: %v", err)
	}
	if got := store.CachedLen(); got != 1 {
		t.Fatalf("cached len after first put: got %d, want 1", got)
	}

	store.Invalidate(tenantA)

	if got := store.CachedLen(); got != 0 {
		t.Fatalf("cached len after invalidate: got %d, want 0", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(evicted) != 1 || evicted[0] != tenantA {
		t.Fatalf("Invalidate did not surface the right tenant through OnEvict: got %v", evicted)
	}
}

// TestPerTenantStoreIdleTTLEviction verifies an idle entry is
// evicted (and the OnEvict callback fires) once its TTL passes.
// Uses a controllable clock on the underlying LRU so the test
// runs deterministically without sleeping.
func TestPerTenantStoreIdleTTLEviction(t *testing.T) {
	resolver := newFakeResolver()

	var (
		mu      sync.Mutex
		evicted []uuid.UUID
	)
	store, err := NewPerTenantS3Store(PerTenantConfig{
		Resolver:   resolver,
		Fallback:   NewMemoryStore(),
		MaxEntries: 8,
		IdleTTL:    10 * time.Minute,
		OnEvict: func(id uuid.UUID, _ *S3Store) {
			mu.Lock()
			evicted = append(evicted, id)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("NewPerTenantS3Store: %v", err)
	}

	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	store.stores.SetClock(func() time.Time { return now })

	tenantA := uuid.New()
	if _, err := store.routeFor(WithTenant(context.Background(), tenantA)); err != nil {
		t.Fatalf("routeFor: %v", err)
	}
	if got := store.CachedLen(); got != 1 {
		t.Fatalf("cached len after first put: got %d, want 1", got)
	}

	// Advance past the idle TTL.
	now = now.Add(11 * time.Minute)

	// A subsequent routeFor call must re-resolve credentials because
	// the prior entry expired. The expired entry must surface through
	// OnEvict so transport cleanup runs.
	if _, err := store.routeFor(WithTenant(context.Background(), tenantA)); err != nil {
		t.Fatalf("routeFor (post-TTL): %v", err)
	}
	if got := resolver.resolveCount(tenantA); got != 2 {
		t.Fatalf("expected resolver to be called twice (pre + post TTL), got %d", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(evicted) != 1 || evicted[0] != tenantA {
		t.Fatalf("expected one TTL eviction for tenant A, got %v", evicted)
	}
}
