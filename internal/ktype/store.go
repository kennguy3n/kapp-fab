package ktype

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// Sentinel errors.
var (
	ErrNotFound = errors.New("ktype: not found")
)

// PGRegistry is the PostgreSQL-backed Registry. KTypes live in the shared,
// non-tenant-scoped `ktypes` table and are therefore looked up against the
// pool directly (no SET LOCAL).
//
// Hot lookups go through an LRU cache keyed by "<name>:<version>" (version
// "latest" resolves to whatever the DB currently has). Registry Register
// invalidates the cached entries for the affected name.
type PGRegistry struct {
	pool  *pgxpool.Pool
	cache *platform.LRUCache
}

// NewPGRegistry wires a PGRegistry with the shared pool and LRU.
func NewPGRegistry(pool *pgxpool.Pool, cache *platform.LRUCache) *PGRegistry {
	return &PGRegistry{pool: pool, cache: cache}
}

// Register inserts a new KType row. The schema is validated as well-formed
// JSON before insert; deeper schema validation (e.g. field-spec coherence)
// lives in the validator package and is invoked by callers that want to
// reject malformed schemas at registration time.
func (r *PGRegistry) Register(ctx context.Context, kt KType) error {
	if kt.Name == "" || kt.Version <= 0 {
		return errors.New("ktype: name and version required")
	}
	if !json.Valid(kt.Schema) {
		return errors.New("ktype: schema is not valid JSON")
	}
	_, err := r.pool.Exec(ctx,
		`INSERT INTO ktypes (name, version, schema) VALUES ($1, $2, $3)
		 ON CONFLICT (name, version) DO UPDATE SET schema = EXCLUDED.schema`,
		kt.Name, kt.Version, kt.Schema,
	)
	if err != nil {
		return fmt.Errorf("ktype: register: %w", err)
	}
	r.invalidate(kt.Name)
	return nil
}

// Get returns the named KType at the requested version. If version is <= 0,
// the latest version is returned.
func (r *PGRegistry) Get(ctx context.Context, name string, version int) (*KType, error) {
	key := cacheKey(name, version)
	if r.cache != nil {
		if v, ok := r.cache.Get(key); ok {
			if kt, ok := v.(*KType); ok {
				return kt, nil
			}
		}
	}

	var (
		kt  KType
		err error
	)
	if version <= 0 {
		err = r.pool.QueryRow(ctx,
			`SELECT name, version, schema, created_at
			 FROM ktypes WHERE name = $1 ORDER BY version DESC LIMIT 1`,
			name,
		).Scan(&kt.Name, &kt.Version, &kt.Schema, &kt.CreatedAt)
	} else {
		err = r.pool.QueryRow(ctx,
			`SELECT name, version, schema, created_at
			 FROM ktypes WHERE name = $1 AND version = $2`,
			name, version,
		).Scan(&kt.Name, &kt.Version, &kt.Schema, &kt.CreatedAt)
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("ktype: get: %w", err)
	}
	if r.cache != nil {
		r.cache.Set(key, &kt)
		if version <= 0 {
			r.cache.Set(cacheKey(name, kt.Version), &kt)
		}
	}
	return &kt, nil
}

// List returns all registered KTypes. Admin-only; uncached.
func (r *PGRegistry) List(ctx context.Context) ([]KType, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT name, version, schema, created_at FROM ktypes
		 ORDER BY name ASC, version DESC`)
	if err != nil {
		return nil, fmt.Errorf("ktype: list: %w", err)
	}
	defer rows.Close()

	var kts []KType
	for rows.Next() {
		var kt KType
		if err := rows.Scan(&kt.Name, &kt.Version, &kt.Schema, &kt.CreatedAt); err != nil {
			return nil, fmt.Errorf("ktype: scan: %w", err)
		}
		kts = append(kts, kt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ktype: rows: %w", err)
	}
	return kts, nil
}

func (r *PGRegistry) invalidate(name string) {
	if r.cache == nil {
		return
	}
	// Register upserts `(name, version)` so any cached version for this name
	// may now be stale. The LRU is small and shared across name spaces; a
	// full purge on a Register call (admin-frequency) is cheaper than
	// maintaining a secondary name→versions index.
	_ = name
	r.cache.Purge()
}

func cacheKey(name string, version int) string {
	if version <= 0 {
		return name + ":latest"
	}
	return fmt.Sprintf("%s:%d", name, version)
}
