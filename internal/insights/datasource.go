package insights

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// Supported external dialects. Extend this list as new dialect handlers
// land — the migration CHECK constraint is the authoritative gate, the
// constants below mirror it for callers that want to validate before
// the round-trip.
const (
	DialectPostgres = "postgres"
)

// allowedDataSourceDialects is the in-memory whitelist of recognised
// dialects. Mirrors the CHECK in migrations/000043_insights_data_sources.sql.
var allowedDataSourceDialects = map[string]struct{}{
	DialectPostgres: {},
}

// MaxDataSourceNameLen caps the per-tenant data source name length.
// Aligns with the convention of TEXT-typed name columns in this
// schema; the UNIQUE (tenant_id, name) constraint enforces no
// duplicates per tenant, the length cap keeps display layout sane.
const MaxDataSourceNameLen = 128

// DataSource mirrors one row of insights_data_sources.
//
// ConnectionString and SecretBlob are stored encrypted at rest with
// the `kapp:enc:v1:` envelope. The store layer performs encrypt-on-
// write and decrypt-on-read so callers always see plaintext. Never
// log either field — even at debug level — because they typically
// contain credentials.
type DataSource struct {
	TenantID         uuid.UUID  `json:"tenant_id"`
	ID               uuid.UUID  `json:"id"`
	Name             string     `json:"name"`
	Description      string     `json:"description,omitempty"`
	Dialect          string     `json:"dialect"`
	ConnectionString string     `json:"-"`
	SecretBlob       string     `json:"-"`
	Enabled          bool       `json:"enabled"`
	CreatedBy        *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// ValidateDialect rejects unknown dialect identifiers before any
// store round-trip so the DB CHECK only ever rejects bugs.
func ValidateDialect(d string) error {
	if d == "" {
		return validationErr("dialect required")
	}
	if _, ok := allowedDataSourceDialects[d]; !ok {
		return validationErr("dialect %q invalid", d)
	}
	return nil
}

// Encryptor encrypts/decrypts data-source secrets per-tenant. The
// concrete implementation is tenant.KeyManager; the interface lets
// the store unit-test against an in-memory shim and keeps the
// insights package free of a tenant import.
type Encryptor interface {
	EncryptString(tenantID uuid.UUID, plaintext string) (string, error)
	DecryptString(tenantID uuid.UUID, ciphertext string) (string, error)
}

// DataSourceStore persists insights_data_sources rows under tenant
// RLS. ConnectionString / SecretBlob round-trip through Encryptor
// transparently — callers always pass and receive plaintext.
type DataSourceStore struct {
	pool *pgxpool.Pool
	enc  Encryptor
}

// NewDataSourceStore wires the store from the shared pool plus an
// encryptor. Pass nil for enc only in tests where you assert against
// the ciphertext column directly; production callers must always
// supply a real KeyManager so credentials are never stored in the
// clear.
func NewDataSourceStore(pool *pgxpool.Pool, enc Encryptor) *DataSourceStore {
	return &DataSourceStore{pool: pool, enc: enc}
}

// ErrDataSourceNotFound is surfaced when a row lookup misses.
var ErrDataSourceNotFound = errors.New("insights: data source not found")

// Create inserts a new data source. ConnectionString must be a valid
// libpq URI (or DSN) for `dialect`; the store does not validate the
// shape — that's the job of a separate connection-test endpoint.
func (s *DataSourceStore) Create(ctx context.Context, d DataSource) (*DataSource, error) {
	if d.TenantID == uuid.Nil {
		return nil, validationErr("tenant id required")
	}
	if err := s.validate(d); err != nil {
		return nil, err
	}
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	connEncrypted, err := s.encrypt(d.TenantID, d.ConnectionString)
	if err != nil {
		return nil, err
	}
	secretEncrypted, err := s.encrypt(d.TenantID, d.SecretBlob)
	if err != nil {
		return nil, err
	}
	out := d
	err = dbutil.WithTenantTx(ctx, s.pool, d.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var createdBy any
		if d.CreatedBy != nil {
			createdBy = *d.CreatedBy
		}
		return tx.QueryRow(ctx,
			`INSERT INTO insights_data_sources
			   (tenant_id, id, name, description, dialect,
			    connection_string, secret_blob, enabled, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			 RETURNING created_at, updated_at`,
			d.TenantID, d.ID, d.Name, d.Description, d.Dialect,
			connEncrypted, secretEncrypted, d.Enabled, createdBy,
		).Scan(&out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, fmt.Errorf("insights: create data source: %w", err)
	}
	return &out, nil
}

// Update replaces name, description, dialect, connection string,
// secret blob, and enabled. Pass empty ConnectionString / SecretBlob
// to keep the existing value (the store treats empty as "no change"
// so the UI can patch metadata without resending credentials).
func (s *DataSourceStore) Update(ctx context.Context, d DataSource) (*DataSource, error) {
	if d.TenantID == uuid.Nil || d.ID == uuid.Nil {
		return nil, validationErr("tenant id and data source id required")
	}
	existing, err := s.Get(ctx, d.TenantID, d.ID)
	if err != nil {
		return nil, err
	}
	if d.ConnectionString == "" {
		d.ConnectionString = existing.ConnectionString
	}
	if d.SecretBlob == "" {
		d.SecretBlob = existing.SecretBlob
	}
	if err := s.validate(d); err != nil {
		return nil, err
	}
	connEncrypted, err := s.encrypt(d.TenantID, d.ConnectionString)
	if err != nil {
		return nil, err
	}
	secretEncrypted, err := s.encrypt(d.TenantID, d.SecretBlob)
	if err != nil {
		return nil, err
	}
	err = dbutil.WithTenantTx(ctx, s.pool, d.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE insights_data_sources
			    SET name = $3, description = $4, dialect = $5,
			        connection_string = $6, secret_blob = $7,
			        enabled = $8, updated_at = now()
			  WHERE tenant_id = $1 AND id = $2`,
			d.TenantID, d.ID, d.Name, d.Description, d.Dialect,
			connEncrypted, secretEncrypted, d.Enabled,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrDataSourceNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, d.TenantID, d.ID)
}

// Get loads a single data source with decrypted secrets.
func (s *DataSourceStore) Get(ctx context.Context, tenantID, id uuid.UUID) (*DataSource, error) {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return nil, validationErr("tenant id and data source id required")
	}
	var d DataSource
	var (
		connEncrypted   string
		secretEncrypted string
		createdBy       *uuid.UUID
	)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT tenant_id, id, name, description, dialect,
			        connection_string, secret_blob, enabled,
			        created_by, created_at, updated_at
			   FROM insights_data_sources
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		).Scan(
			&d.TenantID, &d.ID, &d.Name, &d.Description, &d.Dialect,
			&connEncrypted, &secretEncrypted, &d.Enabled,
			&createdBy, &d.CreatedAt, &d.UpdatedAt,
		)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrDataSourceNotFound
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	d.CreatedBy = createdBy
	if d.ConnectionString, err = s.decrypt(tenantID, connEncrypted); err != nil {
		return nil, err
	}
	if d.SecretBlob, err = s.decrypt(tenantID, secretEncrypted); err != nil {
		return nil, err
	}
	return &d, nil
}

// List returns every data source for the tenant. Connection strings
// and secret blobs are NOT decrypted on List — the listing surface
// is for inventory only; callers that need the secret call Get.
func (s *DataSourceStore) List(ctx context.Context, tenantID uuid.UUID) ([]DataSource, error) {
	if tenantID == uuid.Nil {
		return nil, validationErr("tenant id required")
	}
	out := []DataSource{}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tenant_id, id, name, description, dialect,
			        enabled, created_by, created_at, updated_at
			   FROM insights_data_sources
			  WHERE tenant_id = $1
			  ORDER BY name`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d DataSource
			var createdBy *uuid.UUID
			if err := rows.Scan(
				&d.TenantID, &d.ID, &d.Name, &d.Description, &d.Dialect,
				&d.Enabled, &createdBy, &d.CreatedAt, &d.UpdatedAt,
			); err != nil {
				return err
			}
			d.CreatedBy = createdBy
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("insights: list data sources: %w", err)
	}
	return out, nil
}

// Delete removes a data source by id. Returns ErrDataSourceNotFound
// when the row doesn't exist.
func (s *DataSourceStore) Delete(ctx context.Context, tenantID, id uuid.UUID) error {
	if tenantID == uuid.Nil || id == uuid.Nil {
		return validationErr("tenant id and data source id required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM insights_data_sources
			  WHERE tenant_id = $1 AND id = $2`,
			tenantID, id,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrDataSourceNotFound
		}
		return nil
	})
}

func (s *DataSourceStore) validate(d DataSource) error {
	if d.Name == "" {
		return validationErr("data source name required")
	}
	if len(d.Name) > MaxDataSourceNameLen {
		return validationErr("data source name too long (max %d)", MaxDataSourceNameLen)
	}
	if err := ValidateDialect(d.Dialect); err != nil {
		return err
	}
	if d.ConnectionString == "" {
		return validationErr("connection string required")
	}
	return nil
}

func (s *DataSourceStore) encrypt(tenantID uuid.UUID, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if s.enc == nil {
		return plaintext, nil
	}
	out, err := s.enc.EncryptString(tenantID, plaintext)
	if err != nil {
		return "", fmt.Errorf("insights: encrypt data source secret: %w", err)
	}
	return out, nil
}

func (s *DataSourceStore) decrypt(tenantID uuid.UUID, ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	if s.enc == nil {
		return ciphertext, nil
	}
	out, err := s.enc.DecryptString(tenantID, ciphertext)
	if err != nil {
		return "", fmt.Errorf("insights: decrypt data source secret: %w", err)
	}
	return out, nil
}

// PoolManager is the per-tenant connection-pool cache for external
// data sources. Pools are keyed by (tenant_id, datasource_id) and
// evicted by an LRU bound + idle TTL so a tenant that points the
// engine at a hundred external DBs does not blow the file-descriptor
// budget.
//
// All access is serialised behind a single mutex. The hot path is
// Get → cache hit → return; the slow path opens a new pool, which
// happens at most once per (tenant, datasource) per TTL window. The
// mutex is held during open so concurrent callers serialise on the
// expected (~milliseconds) handshake time rather than racing N
// pools open against the same remote.
type PoolManager struct {
	mu       sync.Mutex
	entries  map[poolKey]*poolCacheEntry
	maxPools int
	idleTTL  time.Duration
	open     func(ctx context.Context, dsn string) (*pgxpool.Pool, error)
}

type poolKey struct {
	tenantID     uuid.UUID
	dataSourceID uuid.UUID
}

type poolCacheEntry struct {
	pool   *pgxpool.Pool
	last   time.Time
	dsnSig string
}

// DefaultMaxPools caps the per-process external-pool cache. 32 covers
// the realistic ceiling for an SME deployment: each tenant typically
// has 1–3 external sources, and the worker only routes ~10 hot
// tenants through the cache simultaneously.
const DefaultMaxPools = 32

// DefaultPoolIdleTTL is the LRU age-out window. Five minutes balances
// pool reuse against picking up credential rotation in roughly the
// same TTL as the master-key rotation cache.
const DefaultPoolIdleTTL = 5 * time.Minute

// NewPoolManager wires a PoolManager with the standard caps.
func NewPoolManager() *PoolManager {
	return &PoolManager{
		entries:  make(map[poolKey]*poolCacheEntry),
		maxPools: DefaultMaxPools,
		idleTTL:  DefaultPoolIdleTTL,
		open:     openExternalPool,
	}
}

// WithOpen overrides the pool-open function. Tests inject a fake
// opener that returns a connected pool against the local Postgres
// instance.
func (p *PoolManager) WithOpen(open func(ctx context.Context, dsn string) (*pgxpool.Pool, error)) *PoolManager {
	p.open = open
	return p
}

// Get returns the cached pool for the (tenant, datasource), opening
// a fresh one on cache miss / expired. dsnSig is a signature derived
// from the connection string so credential rotation invalidates the
// pool — pass an empty string to disable that check (tests).
func (p *PoolManager) Get(ctx context.Context, tenantID, dataSourceID uuid.UUID, dsn, dsnSig string) (*pgxpool.Pool, error) {
	if p == nil {
		return nil, errors.New("insights: nil pool manager")
	}
	if tenantID == uuid.Nil || dataSourceID == uuid.Nil {
		return nil, validationErr("tenant id and data source id required")
	}
	if dsn == "" {
		return nil, validationErr("dsn required")
	}
	key := poolKey{tenantID: tenantID, dataSourceID: dataSourceID}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if entry, ok := p.entries[key]; ok {
		if (p.idleTTL == 0 || now.Sub(entry.last) < p.idleTTL) && entry.dsnSig == dsnSig {
			entry.last = now
			return entry.pool, nil
		}
		// Stale or rotated — close and re-open.
		entry.pool.Close()
		delete(p.entries, key)
	}
	if len(p.entries) >= p.maxPools {
		p.evictOldestLocked()
	}
	pool, err := p.open(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("insights: open external pool: %w", err)
	}
	p.entries[key] = &poolCacheEntry{pool: pool, last: now, dsnSig: dsnSig}
	return pool, nil
}

// Close releases every cached pool. Call on shutdown.
func (p *PoolManager) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, entry := range p.entries {
		entry.pool.Close()
		delete(p.entries, k)
	}
}

// Len reports the number of pools cached. Test hook only.
func (p *PoolManager) Len() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

func (p *PoolManager) evictOldestLocked() {
	var oldestKey poolKey
	var oldestEntry *poolCacheEntry
	for k, entry := range p.entries {
		if oldestEntry == nil || entry.last.Before(oldestEntry.last) {
			oldestKey = k
			oldestEntry = entry
		}
	}
	if oldestEntry != nil {
		oldestEntry.pool.Close()
		delete(p.entries, oldestKey)
	}
}

func openExternalPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("insights: parse external dsn: %w", err)
	}
	// External pools are SELECT-only by contract; cap connections
	// tightly so a misbehaving tenant cannot exhaust the remote.
	cfg.MaxConns = 4
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 2 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return pool, nil
}
