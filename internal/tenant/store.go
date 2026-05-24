package tenant

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Cache is the narrow subset of platform.LRUCache that PGStore depends on for
// optional read-through caching. It lives here (not in internal/platform)
// because internal/platform already imports this package for the
// platform.TenantFromContext helper — introducing the reverse import would
// create a cycle. Anything that satisfies these four methods can be wired in
// via WithCache, including *platform.LRUCache and any test fake.
type Cache interface {
	Get(key string) (any, bool)
	Set(key string, value any)
	Delete(key string)
	SetOnEvict(fn func(key string, value any))
}

// Sentinel errors returned by PGStore. Callers map these to HTTP status codes
// in transport layers (e.g. NotFound → 404, SlugTaken → 409, InvalidTransition
// → 409).
var (
	ErrNotFound          = errors.New("tenant: not found")
	ErrSlugTaken         = errors.New("tenant: slug already taken")
	ErrInvalidTransition = errors.New("tenant: invalid status transition")
)

// PGStore is the PostgreSQL-backed implementation of Service. It operates on
// the control-plane `tenants` table, which is NOT tenant-scoped and does not
// have RLS — so these operations run against the shared pool without needing
// `SET LOCAL app.tenant_id`.
//
// Optional read-through caching: callers may attach a *platform.LRUCache via
// WithCache to short-circuit the per-request Get / GetBySlug round-trip. The
// cache is keyed by both "tenant:id:<uuid>" and "tenant:slug:<slug>" with the
// same *Tenant value behind each key so either lookup path warms the other.
// Every mutation method on this store (Suspend, Activate, Archive, Delete,
// UpdatePlan, SetBaseCurrency, SetCountry, SetLocale, SetZKCredentials,
// SetPlacementPolicy)
// invalidates the affected tenant's cache entries before returning, so
// subsequent reads see the new row. With no cache attached the store is a
// plain pass-through with zero extra branches in the hot path beyond a single
// nil check.
type PGStore struct {
	pool  *pgxpool.Pool
	cache Cache
}

const (
	// tenantCachePrefixID / tenantCachePrefixSlug namespace cache entries so
	// the same LRUCache instance can in principle be shared with other
	// tenant-domain lookup paths without collision. Both prefixes are
	// included in the explicit type assertion done by Get / GetBySlug so a
	// value of unexpected type (e.g. left behind by a future caller that
	// reuses the cache) falls through to the DB path rather than panicking.
	tenantCachePrefixID   = "tenant:id:"
	tenantCachePrefixSlug = "tenant:slug:"
)

// NewPGStore returns a PGStore backed by the supplied pool. The returned
// store has no cache attached; call WithCache to opt in to read-through
// caching after construction.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

// WithCache attaches the supplied LRU cache to this store for read-through
// of Get / GetBySlug and write-through invalidation on every mutation method.
// Returns the receiver so the call can be chained at construction.
//
// A nil argument is accepted and disables caching (the same as never having
// called WithCache). Passing a non-nil cache replaces any previously-attached
// cache; the eviction callback on the new cache is installed by this method
// so an id-keyed eviction cleans up the sibling slug-keyed entry, preventing
// stale slug→tenant mappings from outliving their id sibling and surfacing as
// a phantom-tenant read.
func (s *PGStore) WithCache(cache Cache) *PGStore {
	s.cache = cache
	if cache == nil {
		return s
	}
	cache.SetOnEvict(func(key string, value any) {
		if !strings.HasPrefix(key, tenantCachePrefixID) {
			return
		}
		t, ok := value.(*Tenant)
		if !ok || t == nil || t.Slug == "" {
			return
		}
		cache.Delete(tenantCachePrefixSlug + t.Slug)
	})
	return s
}

// warmCache stores the tenant under both id and slug keys. Both entries point
// at the same struct so a single mutation (which goes through
// invalidateCache) reaps both lookup paths at once.
func (s *PGStore) warmCache(t *Tenant) {
	if s.cache == nil || t == nil {
		return
	}
	s.cache.Set(tenantCachePrefixID+t.ID.String(), t)
	if t.Slug != "" {
		s.cache.Set(tenantCachePrefixSlug+t.Slug, t)
	}
}

// invalidateCache removes the tenant's id-keyed entry; the LRUCache's
// OnEvict callback installed by WithCache then drops the sibling slug-keyed
// entry. If the id-keyed entry is already absent (TTL expiry, prior eviction)
// the slug-keyed entry will reach its own TTL within the cache window — the
// short TTL on the tenant cache (single-digit seconds in production) bounds
// the worst-case staleness window.
func (s *PGStore) invalidateCache(id uuid.UUID) {
	if s.cache == nil {
		return
	}
	s.cache.Delete(tenantCachePrefixID + id.String())
}

const (
	pgUniqueViolation = "23505"
)

// Create inserts a new tenant row. Slug uniqueness is enforced by the
// database; a conflict is translated to ErrSlugTaken.
func (s *PGStore) Create(ctx context.Context, input CreateInput) (*Tenant, error) {
	if input.Slug == "" || input.Name == "" || input.Cell == "" || input.Plan == "" {
		return nil, errors.New("tenant: slug, name, cell, and plan are required")
	}
	id := uuid.New()
	quota := input.Quota
	if len(quota) == 0 {
		quota = []byte("{}")
	}

	var t Tenant
	var zkAccess, zkSecret, zkBucket *string
	err := s.pool.QueryRow(ctx,
		`INSERT INTO tenants (id, slug, name, cell, status, plan, quota)
		 VALUES ($1, $2, $3, $4, 'active', $5, $6)
		 RETURNING id, slug, name, cell, status, plan, quota, created_at, updated_at,
		           zk_access_key, zk_secret_key, zk_bucket, COALESCE(base_currency, 'USD'),
		           COALESCE(country, ''), COALESCE(locale, 'en')`,
		id, input.Slug, input.Name, input.Cell, input.Plan, quota,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Cell, &t.Status, &t.Plan, &t.Quota, &t.CreatedAt, &t.UpdatedAt,
		&zkAccess, &zkSecret, &zkBucket, &t.BaseCurrency, &t.Country, &t.Locale)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
			return nil, ErrSlugTaken
		}
		return nil, fmt.Errorf("tenant: insert: %w", err)
	}
	assignZK(&t, zkAccess, zkSecret, zkBucket)
	s.warmCache(&t)
	return &t, nil
}

// Get returns the tenant with the given id or ErrNotFound. When a cache is
// attached (see WithCache) the row is served from the LRU on hit and warmed
// on miss; the slug-keyed sibling entry is warmed alongside so a subsequent
// GetBySlug lookup hits the same cached row.
func (s *PGStore) Get(ctx context.Context, id uuid.UUID) (*Tenant, error) {
	if s.cache != nil {
		if v, ok := s.cache.Get(tenantCachePrefixID + id.String()); ok {
			if t, ok := v.(*Tenant); ok && t != nil {
				return t, nil
			}
		}
	}
	var t Tenant
	var zkAccess, zkSecret, zkBucket *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, slug, name, cell, status, plan, quota, created_at, updated_at,
		        zk_access_key, zk_secret_key, zk_bucket, COALESCE(base_currency, 'USD'),
		        COALESCE(country, ''), COALESCE(locale, 'en')
		 FROM tenants WHERE id = $1`, id,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Cell, &t.Status, &t.Plan, &t.Quota, &t.CreatedAt, &t.UpdatedAt,
		&zkAccess, &zkSecret, &zkBucket, &t.BaseCurrency, &t.Country, &t.Locale)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("tenant: get: %w", err)
	}
	assignZK(&t, zkAccess, zkSecret, zkBucket)
	s.warmCache(&t)
	return &t, nil
}

// Timezone returns the tenant's IANA timezone identifier (e.g.
// "America/New_York") used to interpret wall-clock fields such as
// hr.shift_type.start_time. The column is backfilled to "UTC" by
// migration 000047, so a never-configured tenant always resolves
// to UTC rather than NULL or empty. ErrNotFound is returned when
// the tenant id doesn't exist.
func (s *PGStore) Timezone(ctx context.Context, id uuid.UUID) (string, error) {
	var tz string
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(timezone, 'UTC') FROM tenants WHERE id = $1`, id).Scan(&tz)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("tenant: timezone: %w", err)
	}
	if tz == "" {
		tz = "UTC"
	}
	return tz, nil
}

// GetBySlug returns the tenant with the given slug or ErrNotFound. When a
// cache is attached the row is served from the LRU on hit and warmed on miss;
// the id-keyed sibling entry is warmed alongside so a subsequent Get lookup
// hits the same cached row.
func (s *PGStore) GetBySlug(ctx context.Context, slug string) (*Tenant, error) {
	if s.cache != nil {
		if v, ok := s.cache.Get(tenantCachePrefixSlug + slug); ok {
			if t, ok := v.(*Tenant); ok && t != nil {
				return t, nil
			}
		}
	}
	var t Tenant
	var zkAccess, zkSecret, zkBucket *string
	err := s.pool.QueryRow(ctx,
		`SELECT id, slug, name, cell, status, plan, quota, created_at, updated_at,
		        zk_access_key, zk_secret_key, zk_bucket, COALESCE(base_currency, 'USD'),
		        COALESCE(country, ''), COALESCE(locale, 'en')
		 FROM tenants WHERE slug = $1`, slug,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Cell, &t.Status, &t.Plan, &t.Quota, &t.CreatedAt, &t.UpdatedAt,
		&zkAccess, &zkSecret, &zkBucket, &t.BaseCurrency, &t.Country, &t.Locale)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("tenant: get by slug: %w", err)
	}
	assignZK(&t, zkAccess, zkSecret, zkBucket)
	s.warmCache(&t)
	return &t, nil
}

// List returns all tenants ordered by slug. Intended for control-plane
// admin tooling; no filtering is applied.
func (s *PGStore) List(ctx context.Context) ([]Tenant, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, slug, name, cell, status, plan, quota, created_at, updated_at,
		        zk_access_key, zk_secret_key, zk_bucket, COALESCE(base_currency, 'USD'),
		        COALESCE(country, ''), COALESCE(locale, 'en')
		 FROM tenants
		 ORDER BY slug ASC`)
	if err != nil {
		return nil, fmt.Errorf("tenant: list: %w", err)
	}
	defer rows.Close()

	// Preallocate an empty (non-nil) slice so the JSON response is `[]`
	// rather than `null` when no rows exist.
	out := make([]Tenant, 0)
	for rows.Next() {
		var t Tenant
		var zkAccess, zkSecret, zkBucket *string
		if err := rows.Scan(
			&t.ID, &t.Slug, &t.Name, &t.Cell, &t.Status,
			&t.Plan, &t.Quota, &t.CreatedAt, &t.UpdatedAt,
			&zkAccess, &zkSecret, &zkBucket, &t.BaseCurrency, &t.Country, &t.Locale,
		); err != nil {
			return nil, fmt.Errorf("tenant: list scan: %w", err)
		}
		assignZK(&t, zkAccess, zkSecret, zkBucket)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tenant: list rows: %w", err)
	}
	return out, nil
}

// Suspend transitions active → suspended.
func (s *PGStore) Suspend(ctx context.Context, id uuid.UUID) error {
	return s.transition(ctx, id, StatusActive, StatusSuspended)
}

// Activate transitions suspended → active. Use this to un-suspend a tenant.
func (s *PGStore) Activate(ctx context.Context, id uuid.UUID) error {
	return s.transition(ctx, id, StatusSuspended, StatusActive)
}

// Archive transitions suspended → archived.
func (s *PGStore) Archive(ctx context.Context, id uuid.UUID) error {
	return s.transition(ctx, id, StatusSuspended, StatusArchived)
}

// Delete transitions any non-deleting state → deleting. Actual purge is async.
func (s *PGStore) Delete(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenants SET status = $1, updated_at = now()
		 WHERE id = $2 AND status <> $1`,
		StatusDeleting, id,
	)
	if err != nil {
		return fmt.Errorf("tenant: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either the tenant does not exist or it is already deleting; check
		// which so callers get the right error.
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return gerr
		}
		return ErrInvalidTransition
	}
	s.invalidateCache(id)
	return nil
}

// UpdatePlan atomically updates the tenant's plan name and quota
// JSON. Used by the /tenants/{id}/plan endpoint. Returns ErrNotFound
// when no row matches.
func (s *PGStore) UpdatePlan(ctx context.Context, id uuid.UUID, plan string, quota []byte) error {
	if plan == "" {
		return errors.New("tenant: plan required")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenants SET plan = $1, quota = $2, updated_at = now()
		 WHERE id = $3`,
		plan, quota, id,
	)
	if err != nil {
		return fmt.Errorf("tenant: update plan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.invalidateCache(id)
	return nil
}

// transition is the shared helper behind Suspend / Activate / Archive. The
// cache is invalidated only on a successful row update; if the transition is
// invalid (no row matched the (id, from) predicate) the cache stays warm
// because the tenant's status is unchanged from a reader's perspective.
func (s *PGStore) transition(ctx context.Context, id uuid.UUID, from, to Status) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenants SET status = $1, updated_at = now()
		 WHERE id = $2 AND status = $3`,
		to, id, from,
	)
	if err != nil {
		return fmt.Errorf("tenant: transition: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if _, gerr := s.Get(ctx, id); gerr != nil {
			return gerr
		}
		return ErrInvalidTransition
	}
	s.invalidateCache(id)
	return nil
}

// assignZK copies the nullable ZK columns onto the Tenant struct.
// Pointers come straight off pgx.Scan and may be nil for tenants
// created before migration 000027 ran.
func assignZK(t *Tenant, access, secret, bucket *string) {
	if access != nil {
		t.ZKAccessKey = *access
	}
	if secret != nil {
		t.ZKSecretKey = *secret
	}
	if bucket != nil {
		t.ZKBucket = *bucket
	}
}

// SetBaseCurrency updates the tenant's functional currency. Called by
// the wizard once at setup time and by the admin tenant-edit form.
// The value must be a 3-letter ISO-4217 code; the column has a CHECK
// of length 3 in migration 000029.
func (s *PGStore) SetBaseCurrency(ctx context.Context, id uuid.UUID, code string) error {
	if len(code) != 3 {
		return errors.New("tenant: base_currency must be a 3-letter ISO-4217 code")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenants SET base_currency = $1, updated_at = now() WHERE id = $2`,
		code, id,
	)
	if err != nil {
		return fmt.Errorf("tenant: set base currency: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.invalidateCache(id)
	return nil
}

// LocaleValidator is the contract for the package that decides whether
// a given IETF BCP 47 tag corresponds to a translation bundle the
// runtime can actually serve. internal/i18n.Bundle satisfies this, but
// the tenant package only takes a Validator so the lookup stays
// dependency-free (i18n imports nothing here, and store.go does not
// import i18n).
//
// Tag is passed through as-is; canonicalisation (case folding,
// region/script normalisation) is the validator's responsibility so
// the same logic governs SetLocale + Accept-Language resolution +
// frontend bundle fetches without drift.
type LocaleValidator interface {
	IsSupported(tag string) bool
}

var localeRe = regexp.MustCompile(`^[a-z]{2,3}(-[A-Za-z0-9]{2,4})?$`)

// ValidateLocale returns nil iff `tag` is a syntactically well-formed
// IETF BCP 47 language tag (e.g. "en", "de", "fr-CH", "zh-Hans") that
// the supplied `validator` recognises as a registered translation
// bundle. The format gate runs first regardless of `validator` so an
// obviously broken value ("../../etc/passwd", "en;DROP TABLE", "EN")
// is rejected before any service consults the i18n loader — defence
// in depth alongside the CHECK on migration 000059.
//
// Empty tags are accepted and return nil so callers (PGStore.SetLocale,
// the wizard) can treat empty as "reset to the DB default 'en'"
// without duplicating the empty-handling branch in every site.
// A non-nil `validator` is required for the membership check to fire;
// pass `nil` to skip the bundle-whitelist gate (e.g. unit tests or
// boot-time paths that don't yet have the runtime loader wired in).
func ValidateLocale(tag string, validator LocaleValidator) error {
	if tag == "" {
		return nil
	}
	if !localeRe.MatchString(tag) {
		return fmt.Errorf("tenant: locale %q must match IETF BCP 47 (e.g. 'en', 'de', 'zh-Hans')", tag)
	}
	if validator != nil && !validator.IsSupported(tag) {
		return fmt.Errorf("tenant: locale %q is not a registered translation bundle", tag)
	}
	return nil
}

// SetLocale updates the tenant's IETF BCP 47 locale tag. Called by
// the setup wizard once at provisioning time (deriving a default
// from the country selection) and by the admin tenant-edit form.
// Empty strings are accepted (clears the locale → stored as the DB
// default 'en').
//
// The validator is the same contract ValidateLocale uses; passing
// nil skips the bundle-whitelist gate but keeps the format check.
func (s *PGStore) SetLocale(ctx context.Context, id uuid.UUID, tag string, validator LocaleValidator) error {
	if err := ValidateLocale(tag, validator); err != nil {
		return err
	}
	stored := tag
	if stored == "" {
		stored = "en"
	}
	cmdTag, err := s.pool.Exec(ctx,
		`UPDATE tenants SET locale = $1, updated_at = now() WHERE id = $2`,
		stored, id,
	)
	if err != nil {
		return fmt.Errorf("tenant: set locale: %w", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.invalidateCache(id)
	return nil
}

// SetCountry updates the tenant's ISO 3166-1 alpha-2 country code.
// Called by the setup wizard once at provisioning time so the
// payroll engine can resolve a per-country tax pack at slip
// generation time. Empty strings are accepted (clears the country)
// because the engine treats empty as "no statutory pack" and a
// tenant operator may legitimately want to opt out.
func (s *PGStore) SetCountry(ctx context.Context, id uuid.UUID, code string) error {
	if code != "" && len(code) != 2 {
		return errors.New("tenant: country must be ISO 3166-1 alpha-2 (or empty)")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenants SET country = $1, updated_at = now() WHERE id = $2`,
		code, id,
	)
	if err != nil {
		return fmt.Errorf("tenant: set country: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.invalidateCache(id)
	return nil
}

// SetZKCredentials persists the per-tenant ZK Object Fabric HMAC
// credentials on the tenants row. Called by the setup wizard after
// it provisions the tenant on the ZK fabric console at :8081 (and
// by integration tests that pre-seed credentials directly). Returns
// ErrNotFound when the tenant id does not exist.
func (s *PGStore) SetZKCredentials(ctx context.Context, id uuid.UUID, accessKey, secretKey, bucket string) error {
	if accessKey == "" || secretKey == "" || bucket == "" {
		return errors.New("tenant: zk access_key, secret_key, and bucket are required")
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE tenants
		    SET zk_access_key = $1,
		        zk_secret_key = $2,
		        zk_bucket     = $3,
		        updated_at    = now()
		  WHERE id = $4`,
		accessKey, secretKey, bucket, id,
	)
	if err != nil {
		return fmt.Errorf("tenant: set zk credentials: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.invalidateCache(id)
	return nil
}
