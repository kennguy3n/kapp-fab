package bundle

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/marketplace"
	"github.com/kennguy3n/kapp-fab/internal/marketplace/runtime"
)

// stubResolver counts Resolve calls and returns either a pre-set
// bundle (per version ID) or a pre-set error. Used to verify that
// the cache wrapper actually elides repeated upstream calls.
type stubResolver struct {
	calls   atomic.Int64
	mu      sync.Mutex
	bundles map[string]*runtime.ResolvedBundle
	errs    map[string]error
}

func newStubResolver() *stubResolver {
	return &stubResolver{
		bundles: make(map[string]*runtime.ResolvedBundle),
		errs:    make(map[string]error),
	}
}

func (s *stubResolver) Set(id uuid.UUID, rb *runtime.ResolvedBundle) {
	s.mu.Lock()
	s.bundles[id.String()] = rb
	s.mu.Unlock()
}

func (s *stubResolver) SetError(id uuid.UUID, err error) {
	s.mu.Lock()
	s.errs[id.String()] = err
	s.mu.Unlock()
}

func (s *stubResolver) Resolve(_ context.Context, v *marketplace.ExtensionVersion) (*runtime.ResolvedBundle, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err, ok := s.errs[v.ID.String()]; ok {
		return nil, err
	}
	if rb, ok := s.bundles[v.ID.String()]; ok {
		return rb, nil
	}
	return nil, errors.New("not set")
}

func (s *stubResolver) Calls() int64 { return s.calls.Load() }

func TestCachingResolver_RepeatedFetchHitsCache(t *testing.T) {
	stub := newStubResolver()
	id := uuid.New()
	rb := &runtime.ResolvedBundle{}
	stub.Set(id, rb)

	cr := NewCachingResolver(stub, 16)
	ver := &marketplace.ExtensionVersion{ID: id}

	for i := 0; i < 5; i++ {
		got, err := cr.Resolve(context.Background(), ver)
		if err != nil {
			t.Fatalf("Resolve[%d]: %v", i, err)
		}
		if got != rb {
			t.Fatalf("Resolve[%d]: pointer mismatch — want %p got %p", i, rb, got)
		}
	}
	if got := stub.Calls(); got != 1 {
		t.Fatalf("inner resolver call count = %d, want 1 (cache should elide 4 of 5)", got)
	}
	if got := cr.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1", got)
	}
}

func TestCachingResolver_FetchErrorsNotCached(t *testing.T) {
	stub := newStubResolver()
	id := uuid.New()
	sentinel := errors.New("transient 5xx")
	stub.SetError(id, sentinel)

	cr := NewCachingResolver(stub, 16)
	ver := &marketplace.ExtensionVersion{ID: id}

	for i := 0; i < 3; i++ {
		_, err := cr.Resolve(context.Background(), ver)
		if !errors.Is(err, sentinel) {
			t.Fatalf("Resolve[%d]: err = %v, want %v", i, err, sentinel)
		}
	}
	if got := stub.Calls(); got != 3 {
		t.Fatalf("inner resolver call count = %d, want 3 (errors must not poison cache)", got)
	}
	if got := cr.Len(); got != 0 {
		t.Fatalf("Len = %d, want 0 (no entries should be cached)", got)
	}

	// Recover: switch the upstream to success and verify subsequent
	// calls do cache.
	stub.mu.Lock()
	delete(stub.errs, id.String())
	stub.bundles[id.String()] = &runtime.ResolvedBundle{}
	stub.mu.Unlock()

	if _, err := cr.Resolve(context.Background(), ver); err != nil {
		t.Fatalf("Resolve after recovery: %v", err)
	}
	if _, err := cr.Resolve(context.Background(), ver); err != nil {
		t.Fatalf("Resolve repeat after recovery: %v", err)
	}
	if got := stub.Calls(); got != 4 {
		t.Fatalf("call count after recovery = %d, want 4 (3 errors + 1 successful fetch + 1 cache hit)", got)
	}
}

func TestCachingResolver_LRUEvictsOldest(t *testing.T) {
	stub := newStubResolver()
	capacity := 4
	cr := NewCachingResolver(stub, capacity)

	ids := make([]uuid.UUID, 6)
	bundles := make([]*runtime.ResolvedBundle, 6)
	for i := range ids {
		ids[i] = uuid.New()
		bundles[i] = &runtime.ResolvedBundle{}
		stub.Set(ids[i], bundles[i])
	}

	// Fill the cache to capacity (4 entries).
	for i := 0; i < capacity; i++ {
		if _, err := cr.Resolve(context.Background(), &marketplace.ExtensionVersion{ID: ids[i]}); err != nil {
			t.Fatalf("Resolve fill[%d]: %v", i, err)
		}
	}
	if got := cr.Len(); got != capacity {
		t.Fatalf("Len after fill = %d, want %d", got, capacity)
	}

	// Touch entries 1, 2 — these become MRU.
	for _, i := range []int{1, 2} {
		if _, err := cr.Resolve(context.Background(), &marketplace.ExtensionVersion{ID: ids[i]}); err != nil {
			t.Fatalf("Resolve touch[%d]: %v", i, err)
		}
	}

	// Insert two new entries; entries 0 and 3 should be evicted
	// (entries 1 and 2 just got promoted to MRU).
	for _, i := range []int{4, 5} {
		if _, err := cr.Resolve(context.Background(), &marketplace.ExtensionVersion{ID: ids[i]}); err != nil {
			t.Fatalf("Resolve insert[%d]: %v", i, err)
		}
	}
	if got := cr.Len(); got != capacity {
		t.Fatalf("Len after evictions = %d, want %d", got, capacity)
	}

	// Touching the surviving entries (1, 2, 4, 5) should not refetch.
	priorCalls := stub.Calls()
	for _, i := range []int{1, 2, 4, 5} {
		if _, err := cr.Resolve(context.Background(), &marketplace.ExtensionVersion{ID: ids[i]}); err != nil {
			t.Fatalf("Resolve survivor[%d]: %v", i, err)
		}
	}
	if extra := stub.Calls() - priorCalls; extra != 0 {
		t.Fatalf("survivor lookups triggered %d extra fetches, want 0", extra)
	}

	// Re-resolving the evicted entries (0, 3) MUST refetch.
	priorCalls = stub.Calls()
	for _, i := range []int{0, 3} {
		if _, err := cr.Resolve(context.Background(), &marketplace.ExtensionVersion{ID: ids[i]}); err != nil {
			t.Fatalf("Resolve evicted[%d]: %v", i, err)
		}
	}
	if extra := stub.Calls() - priorCalls; extra != 2 {
		t.Fatalf("evicted-re-resolve triggered %d fetches, want 2", extra)
	}
}

func TestCachingResolver_NilInnerPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from NewCachingResolver(nil, …)")
		}
	}()
	_ = NewCachingResolver(nil, 1)
}

func TestCachingResolver_DefaultCapacity(t *testing.T) {
	stub := newStubResolver()
	cr := NewCachingResolver(stub, 0)
	if cr.capacity != DefaultResolverCacheSize {
		t.Fatalf("capacity = %d, want default %d", cr.capacity, DefaultResolverCacheSize)
	}
}

func TestCachingResolver_NilVersion(t *testing.T) {
	stub := newStubResolver()
	cr := NewCachingResolver(stub, 4)
	if _, err := cr.Resolve(context.Background(), nil); err == nil {
		t.Fatal("Resolve(nil): want error, got nil")
	}
	if got := stub.Calls(); got != 0 {
		t.Fatalf("nil version triggered %d inner calls, want 0", got)
	}
}

func TestCachingResolver_ZeroIDBypassesCache(t *testing.T) {
	stub := newStubResolver()
	cr := NewCachingResolver(stub, 4)
	zero := &marketplace.ExtensionVersion{ID: uuid.Nil}
	for i := 0; i < 3; i++ {
		_, _ = cr.Resolve(context.Background(), zero)
	}
	if got := stub.Calls(); got != 3 {
		t.Fatalf("zero-ID lookups triggered %d inner calls, want 3 (must bypass cache)", got)
	}
	if got := cr.Len(); got != 0 {
		t.Fatalf("Len = %d, want 0", got)
	}
}

func TestCachingResolver_Invalidate(t *testing.T) {
	stub := newStubResolver()
	id := uuid.New()
	rb := &runtime.ResolvedBundle{}
	stub.Set(id, rb)

	cr := NewCachingResolver(stub, 16)
	ver := &marketplace.ExtensionVersion{ID: id}

	// Prime.
	if _, err := cr.Resolve(context.Background(), ver); err != nil {
		t.Fatalf("prime: %v", err)
	}
	if cr.Len() != 1 {
		t.Fatalf("Len after prime = %d, want 1", cr.Len())
	}

	// Invalidate.
	cr.Invalidate(ver)
	if cr.Len() != 0 {
		t.Fatalf("Len after Invalidate = %d, want 0", cr.Len())
	}

	// Subsequent Resolve must hit the inner resolver again.
	priorCalls := stub.Calls()
	if _, err := cr.Resolve(context.Background(), ver); err != nil {
		t.Fatalf("post-invalidate Resolve: %v", err)
	}
	if extra := stub.Calls() - priorCalls; extra != 1 {
		t.Fatalf("post-invalidate Resolve triggered %d inner calls, want 1", extra)
	}
}

func TestCachingResolver_ConcurrentAccess(t *testing.T) {
	stub := newStubResolver()
	cr := NewCachingResolver(stub, 16)

	const workers = 32
	const perWorker = 16

	ids := make([]uuid.UUID, 8)
	for i := range ids {
		ids[i] = uuid.New()
		stub.Set(ids[i], &runtime.ResolvedBundle{})
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				id := ids[(w+j)%len(ids)]
				if _, err := cr.Resolve(context.Background(), &marketplace.ExtensionVersion{ID: id}); err != nil {
					t.Errorf("worker %d iter %d: %v", w, j, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	// Even with a concurrent race the cache must converge to at
	// most len(ids) entries — no per-call inflation.
	if cr.Len() > len(ids) {
		t.Fatalf("Len = %d, want ≤ %d", cr.Len(), len(ids))
	}
	// And inner fetches must be bounded by the number of distinct
	// IDs plus a small race-multiplicity factor (the test's
	// scheduler may briefly let several workers miss for the same
	// ID before the first PUT lands).
	if got := stub.Calls(); got > int64(len(ids)*workers) {
		t.Fatalf("inner calls = %d exceeded the worst-case upper bound", got)
	}
}

func TestCachingResolver_PutUpdatesExistingEntryWithoutGrowth(t *testing.T) {
	stub := newStubResolver()
	id := uuid.New()
	first := &runtime.ResolvedBundle{}
	stub.Set(id, first)

	cr := NewCachingResolver(stub, 4)
	ver := &marketplace.ExtensionVersion{ID: id}

	if _, err := cr.Resolve(context.Background(), ver); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// Force the wrapper to PUT again for the same key (simulates a
	// concurrent racer landing after the entry is already present).
	second := &runtime.ResolvedBundle{}
	cr.put(id.String(), second)

	if cr.Len() != 1 {
		t.Fatalf("Len after duplicate put = %d, want 1 (must update in place, not duplicate)", cr.Len())
	}

	// Subsequent Resolve must return the updated pointer.
	got, err := cr.Resolve(context.Background(), ver)
	if err != nil {
		t.Fatalf("post-update Resolve: %v", err)
	}
	if got != second {
		t.Fatalf("post-update Resolve returned %p, want %p", got, second)
	}
}

func TestCachingResolver_UnknownIDPropagatesInnerError(t *testing.T) {
	stub := newStubResolver()
	cr := NewCachingResolver(stub, 4)
	id := uuid.New()

	_, err := cr.Resolve(context.Background(), &marketplace.ExtensionVersion{ID: id})
	if err == nil {
		t.Fatal("Resolve unknown: want error, got nil")
	}
	if cr.Len() != 0 {
		t.Fatalf("Len = %d, want 0 (errors do not enter the cache)", cr.Len())
	}
}

// Stress: 200 distinct IDs through a capacity-of-32 cache. Verifies
// that the LRU keeps the working set bounded under aggressive churn
// without losing correctness (every Resolve must succeed).
func TestCachingResolver_StressEviction(t *testing.T) {
	stub := newStubResolver()
	cr := NewCachingResolver(stub, 32)

	const n = 200
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.New()
		stub.Set(ids[i], &runtime.ResolvedBundle{})
	}

	for i := 0; i < n; i++ {
		if _, err := cr.Resolve(context.Background(), &marketplace.ExtensionVersion{ID: ids[i]}); err != nil {
			t.Fatalf("stress[%d] (%s): %v", i, strconv.Itoa(i), err)
		}
	}
	if cr.Len() != 32 {
		t.Fatalf("Len after stress = %d, want 32 (LRU capacity)", cr.Len())
	}
}
