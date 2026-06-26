package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// TenantResolver resolves a tenant id to its ZK Object Fabric
// HMAC credentials + bucket. The PerTenantS3Store calls it once per
// tenant id and caches the resulting *S3Store. Implementations are
// expected to be concurrency-safe.
//
// Returning ok=false means the tenant has not been provisioned on
// the ZK fabric — the caller falls back to the platform-default
// store (legacy MinIO bucket) so existing tenants keep working
// across the rollout window.
type TenantResolver interface {
	ResolveZKCredentials(ctx context.Context, tenantID uuid.UUID) (cfg S3StoreConfig, ok bool, err error)
}

// PerTenantConfig configures the routing store. Endpoint and
// Region are merged into the resolved per-tenant config so the
// resolver only needs to surface bucket + HMAC credentials —
// matching the shape of the row on the tenants table.
//
// MaxEntries / IdleTTL bound the per-tenant *S3Store cache so a
// burst of one-shot tenants (e.g. a load test) cannot grow the
// store map without limit. Defaults: 1000 entries, 10-minute idle
// TTL — matching the same primitive the rate limiter uses
// (`platform.LRUCache`). On eviction the cached *S3Store has its
// idle HTTP connections closed via S3Store.Close so a stale store
// does not retain pooled sockets.
type PerTenantConfig struct {
	Resolver   TenantResolver
	Fallback   ObjectStore
	Endpoint   string
	Region     string
	MaxEntries int
	IdleTTL    time.Duration

	// RequireZKFabric, when true, makes the store fail closed if a
	// tenant has not been provisioned on the ZK Object Fabric instead
	// of silently falling back to the platform-default store. This is
	// the production posture: a shared global bucket weakens the
	// per-tenant isolation guarantee that ZK fabric provides, so a
	// production tenant setup that cannot provision a dedicated bucket
	// must fail loudly rather than drop tenant data into the shared
	// pool. Default false preserves the gradual-rollout behaviour for
	// dev / staging and for the migration window.
	RequireZKFabric bool

	// OnEvict, if non-nil, is called after the cached *S3Store has
	// had Close() invoked. Useful for metrics ("per-tenant store
	// evictions") and for tests that need to observe the eviction
	// pipeline without poking at internals.
	OnEvict func(tenantID uuid.UUID, store *S3Store)
}

// PerTenantS3Store routes Put/Get to a per-tenant *S3Store keyed
// by tenant id. The first request for a tenant triggers a
// resolver lookup + S3Store construction; subsequent requests
// reuse the cached client until LRU eviction or TTL expiry. A
// platform-default ObjectStore covers the path where the tenant
// has no ZK credentials yet.
//
// The store implements the same ObjectStore interface as the
// existing MemoryStore / S3Store so callers (Store.Upload,
// Store.Read) do not need to change. Routing happens via context:
// the Upload/Read methods call WithTenant(ctx, id) which threads
// the tenant id through ContextValue, and PerTenantS3Store reads
// it back here.
type PerTenantS3Store struct {
	resolver        TenantResolver
	fallback        ObjectStore
	endpoint        string
	region          string
	requireZKFabric bool

	// stores holds *S3Store values keyed by tenant id (string form).
	// Bounded with a hard cap + idle TTL so the cache cannot grow
	// without limit on cells with thousands of one-shot tenants.
	stores *platform.LRUCache
}

// Default LRU bounds. These match the values the rate limiter uses
// for the same reason (per-tenant resource cache that must stay
// small enough for a 5000-tenant cell).
const (
	defaultPerTenantStoreMax     = 1000
	defaultPerTenantStoreIdleTTL = 10 * time.Minute
)

// NewPerTenantS3Store constructs a per-tenant routing store.
// Fallback must be non-nil; resolver is required when ZK
// per-tenant credentials are expected.
func NewPerTenantS3Store(cfg PerTenantConfig) (*PerTenantS3Store, error) {
	if cfg.Fallback == nil {
		return nil, errors.New("files: per-tenant store: fallback object store required")
	}
	if cfg.Resolver == nil {
		return nil, errors.New("files: per-tenant store: tenant resolver required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = defaultPerTenantStoreMax
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = defaultPerTenantStoreIdleTTL
	}
	cache := platform.NewLRUCache(cfg.MaxEntries, cfg.IdleTTL)
	cache.SetOnEvict(func(key string, v any) {
		s, ok := v.(*S3Store)
		if !ok {
			return
		}
		_ = s.Close()
		if cfg.OnEvict != nil {
			if id, err := uuid.Parse(key); err == nil {
				cfg.OnEvict(id, s)
			}
		}
	})
	return &PerTenantS3Store{
		resolver:        cfg.Resolver,
		fallback:        cfg.Fallback,
		endpoint:        cfg.Endpoint,
		region:          cfg.Region,
		requireZKFabric: cfg.RequireZKFabric,
		stores:          cache,
	}, nil
}

// tenantContextKey carries the tenant id through to the per-tenant
// routing layer. It is kept package-private so callers must use
// WithTenant.
type tenantContextKey struct{}

// WithTenant returns a new context with the tenant id attached. The
// PerTenantS3Store reads it back to look up the per-tenant
// credentials. Callers that don't want per-tenant routing can omit
// it — the store falls back to the platform default in that case.
func WithTenant(ctx context.Context, tenantID uuid.UUID) context.Context {
	return context.WithValue(ctx, tenantContextKey{}, tenantID)
}

// TenantFromContext returns the tenant id previously attached via
// WithTenant, or uuid.Nil if absent.
func TenantFromContext(ctx context.Context) uuid.UUID {
	if v, ok := ctx.Value(tenantContextKey{}).(uuid.UUID); ok {
		return v
	}
	return uuid.Nil
}

// Put routes the put through the per-tenant store when a tenant id
// is on the context and the resolver has credentials for it.
// Falls back to the platform-default store otherwise.
func (s *PerTenantS3Store) Put(ctx context.Context, key, contentType string, data []byte) error {
	store, err := s.routeFor(ctx)
	if err != nil {
		return err
	}
	return store.Put(ctx, key, contentType, data)
}

// Get is the read-side mirror of Put.
func (s *PerTenantS3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	store, err := s.routeFor(ctx)
	if err != nil {
		return nil, err
	}
	return store.Get(ctx, key)
}

func (s *PerTenantS3Store) routeFor(ctx context.Context) (ObjectStore, error) {
	tenantID := TenantFromContext(ctx)
	if tenantID == uuid.Nil {
		return s.fallback, nil
	}
	key := tenantID.String()
	if v, ok := s.stores.Get(key); ok {
		if cached, ok := v.(*S3Store); ok {
			return cached, nil
		}
	}
	cfg, ok, err := s.resolver.ResolveZKCredentials(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("files: resolve zk credentials: %w", err)
	}
	if !ok {
		// Fail closed in production: a tenant without a dedicated ZK
		// fabric bucket must not silently drop its objects into the
		// shared fallback bucket, which would weaken the per-tenant
		// isolation guarantee. Dev / staging keep the fallback so the
		// gradual rollout and local boots stay frictionless.
		if s.requireZKFabric {
			return nil, fmt.Errorf("files: tenant %s not provisioned on ZK fabric and KAPP_FILES_REQUIRE_ZK_FABRIC is enabled; refusing shared fallback", tenantID)
		}
		return s.fallback, nil
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = s.endpoint
	}
	if cfg.Region == "" {
		cfg.Region = s.region
	}
	cfg.ForcePathStyle = true
	store, err := NewS3Store(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("files: build per-tenant s3 store: %w", err)
	}
	// Race check: another goroutine may have just cached a store
	// for the same tenant. Prefer the cached value to avoid two
	// distinct *S3Store instances pinning two HTTP transports.
	if v, ok := s.stores.Get(key); ok {
		if cached, ok := v.(*S3Store); ok {
			_ = store.Close()
			return cached, nil
		}
	}
	s.stores.Set(key, store)
	return store, nil
}

// Invalidate drops the cached *S3Store for the tenant so the next
// request re-resolves credentials. Called when the tenant rotates
// its ZK fabric HMAC pair via the console. Idempotent: missing
// entries are a no-op. The LRU OnEvict callback closes the
// outgoing store's idle connections.
func (s *PerTenantS3Store) Invalidate(tenantID uuid.UUID) {
	if tenantID == uuid.Nil {
		return
	}
	s.stores.Delete(tenantID.String())
}

// CachedLen reports the current number of cached per-tenant stores.
// Intended for the load test + metrics, NOT for normal operation.
func (s *PerTenantS3Store) CachedLen() int {
	return s.stores.Len()
}
