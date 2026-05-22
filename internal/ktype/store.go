package ktype

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

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

// RegisterIfChanged is the boot-time variant of Register. It computes a
// deterministic content hash (SHA-256) of the KType's schema + name + version
// and compares it against the stored `content_hash` column. If the hash
// matches, the row is unchanged since the last boot → the write is skipped
// entirely, eliminating the ~50 unnecessary UPSERT round-trips that the old
// unconditional Register path performed on every cold-start. If the hash
// differs (or the row doesn't exist yet), a full upsert + hash update is
// performed.
//
// The hash is deterministic because it's derived from the canonical form of
// the KType:
//   - Name + Version (both stable identifiers)
//   - Schema: json.RawMessage bytes as stored (deterministic marshal by
//     construction — all KType schema literals in the codebase are declared
//     as raw string constants / json.RawMessage, not round-tripped through
//     a map, so key order is stable across rebuilds).
//
// This method is suitable for boot-time bulk registration where the caller
// wants to skip writes for unchanged rows. It should NOT be used for the
// POST /api/v1/ktypes endpoint (which always writes to honour the admin's
// explicit intent).
func (r *PGRegistry) RegisterIfChanged(ctx context.Context, kt KType) error {
	if kt.Name == "" || kt.Version <= 0 {
		return errors.New("ktype: name and version required")
	}
	if !json.Valid(kt.Schema) {
		return errors.New("ktype: schema is not valid JSON")
	}
	hash := contentHash(kt)

	var existingHash *string
	err := r.pool.QueryRow(ctx,
		`SELECT content_hash FROM ktypes WHERE name = $1 AND version = $2`,
		kt.Name, kt.Version,
	).Scan(&existingHash)
	if err == nil && existingHash != nil && *existingHash == hash {
		return nil
	}

	_, err = r.pool.Exec(ctx,
		`INSERT INTO ktypes (name, version, schema, content_hash) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (name, version) DO UPDATE SET schema = EXCLUDED.schema, content_hash = EXCLUDED.content_hash`,
		kt.Name, kt.Version, kt.Schema, hash,
	)
	if err != nil {
		return fmt.Errorf("ktype: register if changed: %w", err)
	}
	r.invalidate(kt.Name)
	return nil
}

// contentHash produces a deterministic SHA-256 hex digest from the KType's
// identifying fields. The digest is used by RegisterIfChanged to detect
// unchanged schemas across restarts. The function sorts the JSON keys of the
// schema before hashing to guarantee stability even if a future refactor
// round-trips the schema through a map (which would lose key order). The
// fallback path (used when the schema is not a JSON object — e.g. an array)
// hashes the raw bytes directly, which is stable as long as the source
// literal doesn't change whitespace.
func contentHash(kt KType) string {
	h := sha256.New()
	h.Write([]byte(kt.Name))
	h.Write([]byte(fmt.Sprintf(":%d:", kt.Version)))
	// Canonicalize schema: if it's a JSON object, re-marshal with sorted
	// keys; otherwise hash raw bytes. This guarantees stability across
	// future schema definitions that might be constructed at runtime.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(kt.Schema, &obj); err == nil {
		h.Write(canonicalJSON(obj))
	} else {
		h.Write(kt.Schema)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalJSON produces a stable JSON serialization of a string→RawMessage
// map by sorting keys lexicographically and recursively canonicalizing nested
// objects. This is the same approach used by JCS (RFC 8785) minus the
// number/unicode normalization steps (unnecessary for schema content which is
// always ASCII and uses integer field counts).
func canonicalJSON(obj map[string]json.RawMessage) []byte {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	buf := []byte("{")
	for i, k := range keys {
		if i > 0 {
			buf = append(buf, ',')
		}
		keyBytes, _ := json.Marshal(k)
		buf = append(buf, keyBytes...)
		buf = append(buf, ':')
		// Recursively canonicalize nested objects.
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(obj[k], &nested); err == nil {
			buf = append(buf, canonicalJSON(nested)...)
		} else {
			buf = append(buf, obj[k]...)
		}
	}
	buf = append(buf, '}')
	return buf
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
