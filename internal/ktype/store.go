package ktype

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// pgErrUndefinedColumn is PostgreSQL SQLSTATE for "undefined column". Used
// to detect the case where RegisterIfChanged runs against a database that
// has not yet had migration 000052 applied — in that case we fall back to
// the plain Register path so the API can still boot.
const pgErrUndefinedColumn = "42703"

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
	if err != nil {
		// Defense in depth: if migration 000052 has not been applied yet,
		// the `content_hash` column does not exist and the SELECT fails
		// with SQLSTATE 42703 (undefined_column). Fall back to plain
		// Register so the API can still boot — RegisterIfChanged then
		// degrades to "always upsert" (the pre-Phase-2.4 behavior) which
		// is correct, just slower. Without this fallback, an out-of-order
		// migrate-then-deploy sequence would hard-stop the API at startup
		// with an opaque "column does not exist" error.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrUndefinedColumn {
			return r.Register(ctx, kt)
		}
		// pgx.ErrNoRows is the expected "row not present yet" case — fall
		// through to the UPSERT below. Anything else (connection reset,
		// permission denied, etc.) is a real error and should propagate.
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("ktype: register if changed: select content_hash: %w", err)
		}
	}
	if existingHash != nil && *existingHash == hash {
		return nil
	}

	_, err = r.pool.Exec(ctx,
		`INSERT INTO ktypes (name, version, schema, content_hash) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (name, version) DO UPDATE SET schema = EXCLUDED.schema, content_hash = EXCLUDED.content_hash`,
		kt.Name, kt.Version, kt.Schema, hash,
	)
	if err != nil {
		// Same fallback as above for the UPSERT path: if the column was
		// added between our SELECT and our INSERT (vanishingly unlikely
		// but possible during a rolling deploy where one replica is
		// running the new code and another is still mid-migration), we
		// could see the column on SELECT but not on INSERT, or vice-versa.
		// Treat undefined_column on INSERT the same way and fall back.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgErrUndefinedColumn {
			return r.Register(ctx, kt)
		}
		return fmt.Errorf("ktype: register if changed: %w", err)
	}
	r.invalidate(kt.Name)
	return nil
}

// contentHash produces a deterministic SHA-256 hex digest from the KType's
// identifying fields. The digest is used by RegisterIfChanged to detect
// unchanged schemas across restarts. Canonicalization is done via
// canonicalJSONValue which recursively sorts the keys of every JSON object
// it encounters — including objects nested inside arrays — so the hash is
// stable across any future refactor that constructs schemas via map literals
// (where Go's map iteration order is randomized).
func contentHash(kt KType) string {
	h := sha256.New()
	h.Write([]byte(kt.Name))
	// hash.Hash.Write never returns an error per the io.Writer
	// contract documented on hash.Hash, so the Fprintf result is
	// safely discarded — the errcheck linter still asks for an
	// explicit acknowledgment.
	_, _ = fmt.Fprintf(h, ":%d:", kt.Version)
	h.Write(canonicalJSONValue(kt.Schema))
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalJSONValue produces a stable serialization of any JSON value:
//
//   - Objects: keys are sorted lexicographically; values are recursively
//     canonicalized.
//   - Arrays: element order is preserved (arrays are ordered by definition),
//     but each element is recursively canonicalized so objects nested inside
//     arrays (e.g. the per-field schema objects inside a KType's "fields"
//     array) also get their keys sorted. This was a real Devin Review finding
//     against an earlier version that only recursed into objects — a future
//     map-based schema construction would have produced non-deterministic
//     hashes for the field-definition objects despite the top-level being
//     canonical, causing unnecessary UPSERTs on every boot.
//   - Primitives (strings, numbers, bools, null): returned as-is. We do NOT
//     attempt to normalize number representation (e.g. 1.0 vs 1) because Go's
//     json package emits a stable form and the KType schemas only use
//     integer-valued field counts.
//
// This is the same approach as JCS (RFC 8785) minus number/Unicode
// normalization, both of which are unnecessary for ASCII schema content.
func canonicalJSONValue(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return raw
	}
	// JSON null deserialises as a nil map / nil slice, which would
	// otherwise be mistaken for an empty object or empty array. Detect
	// it explicitly so it round-trips faithfully.
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return raw
	}
	// Try JSON object.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil && obj != nil {
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
			buf = append(buf, canonicalJSONValue(obj[k])...)
		}
		buf = append(buf, '}')
		return buf
	}
	// Try JSON array.
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		buf := []byte("[")
		for i, item := range arr {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = append(buf, canonicalJSONValue(item)...)
		}
		buf = append(buf, ']')
		return buf
	}
	// Primitive: return as-is. json.RawMessage already preserves the
	// source bytes exactly.
	return raw
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
