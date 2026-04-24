package audit

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// PGLogger is the PostgreSQL-backed Logger. Writes participate in the
// caller's transaction so audit entries are durable iff the corresponding
// mutation commits.
type PGLogger struct {
	pool *pgxpool.Pool
}

// NewPGLogger returns a PGLogger bound to the shared pool. The pool is kept
// for future non-transactional read APIs (e.g. audit search); LogTx does not
// use it directly.
func NewPGLogger(pool *pgxpool.Pool) *PGLogger {
	return &PGLogger{pool: pool}
}

// Log writes an entry outside the caller's transaction. Use this for
// bookkeeping that must persist regardless of an upstream rollback —
// e.g. agent tool invocation logs, where we want an attributable
// breadcrumb even when the underlying tool call fails. The write runs
// under SET LOCAL app.tenant_id = entry.TenantID so the audit_log RLS
// policy accepts it even though this pathway doesn't participate in
// the caller's own transaction.
func (l *PGLogger) Log(ctx context.Context, entry Entry) error {
	if entry.TenantID == uuid.Nil {
		return fmt.Errorf("audit: tenant id required")
	}
	return dbutil.WithTenantTx(ctx, l.pool, entry.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return l.LogTx(ctx, tx, entry)
	})
}

// LogTx inserts the entry inside the caller's transaction.
func (l *PGLogger) LogTx(ctx context.Context, tx pgx.Tx, entry Entry) error {
	if entry.TenantID == uuid.Nil {
		return fmt.Errorf("audit: tenant id required")
	}
	if entry.Action == "" {
		return fmt.Errorf("audit: action required")
	}
	if entry.ActorKind == "" {
		entry.ActorKind = ActorSystem
	}
	before := normalizeJSON(entry.Before)
	after := normalizeJSON(entry.After)
	c := normalizeJSON(entry.Context)

	// Hash-chain: look up the previous tenant row's row_hash and
	// stamp (prev_hash, row_hash) on this insert. The lookup runs in
	// the same transaction (same SET LOCAL app.tenant_id) so RLS
	// filters out other tenants for free. The (tenant_id, id) PK
	// ordering matches the verifier traversal.
	//
	// Concurrency: two writers in the same tenant must not both read
	// the same latest hash and fork the chain. We serialize the
	// (fetchPrevHash, INSERT) pair with a transaction-scoped
	// advisory lock keyed on the tenant UUID. The lock auto-releases
	// on COMMIT/ROLLBACK, so callers do not need to explicitly
	// unlock. Cross-tenant writes are uncontended because the key
	// derives from the tenant UUID.
	if err := lockTenantChain(ctx, tx, entry.TenantID); err != nil {
		return err
	}
	prevHash, err := fetchPrevHash(ctx, tx, entry.TenantID)
	if err != nil {
		return err
	}
	createdAt := time.Now().UTC()
	rowHash := computeRowHash(prevHash, entry, before, after, c, createdAt)
	_, err = tx.Exec(ctx,
		`INSERT INTO audit_log
		     (tenant_id, actor_id, actor_kind, action, target_ktype, target_id,
		      before, after, context, created_at, prev_hash, row_hash)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		entry.TenantID,
		entry.ActorID,
		string(entry.ActorKind),
		entry.Action,
		nullIfEmpty(entry.TargetKType),
		entry.TargetID,
		before,
		after,
		c,
		createdAt,
		prevHash,
		rowHash,
	)
	if err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
}

// lockTenantChain takes a transaction-scoped advisory lock keyed on
// the tenant UUID so concurrent LogTx calls for the same tenant
// serialize on the (fetchPrevHash, INSERT) critical section. The key
// is derived by hashing the tenant's text form to an int4; collisions
// across tenants just cause spurious serialization and never a
// correctness issue. hashtext's int4 result is implicitly widened to
// bigint by pg_advisory_xact_lock.
func lockTenantChain(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1::text))`,
		tenantID.String(),
	); err != nil {
		return fmt.Errorf("audit: lock tenant chain: %w", err)
	}
	return nil
}

// fetchPrevHash returns the row_hash of the most recent audit row for
// this tenant. Returns nil for the first row per tenant — the verifier
// interprets nil prev_hash as a zero seed.
func fetchPrevHash(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]byte, error) {
	var prev []byte
	err := tx.QueryRow(ctx,
		`SELECT row_hash FROM audit_log
		 WHERE tenant_id = $1
		 ORDER BY id DESC
		 LIMIT 1`,
		tenantID,
	).Scan(&prev)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("audit: fetch prev_hash: %w", err)
	}
	return prev, nil
}

// ComputeRowHash is the exported wrapper used by the verifier (see
// services/api/audit.go) so both the logger and the verifier share
// one canonical field-order definition. Callers that hold normalized
// JSON bytes can pass them directly; jsonBytes handles the common
// shapes (nil, json.RawMessage, []byte, string).
func ComputeRowHash(prev []byte, entry Entry, before, after, contextVal any, createdAt time.Time) []byte {
	return computeRowHash(prev, entry, before, after, contextVal, createdAt)
}

// computeRowHash stitches the canonical field order (see
// migrations/000016_audit_hash_chain.sql) into one SHA-256 digest.
// The normalized JSON slices are passed through unchanged; nil values
// produce empty byte slices so the tuple is deterministic.
func computeRowHash(prev []byte, entry Entry, before, after, contextVal any, createdAt time.Time) []byte {
	h := sha256.New()
	if prev == nil {
		h.Write(make([]byte, sha256.Size))
	} else {
		h.Write(prev)
	}
	h.Write(entry.TenantID[:])
	if entry.TargetID != nil {
		h.Write(entry.TargetID[:])
	}
	h.Write([]byte(entry.Action))
	h.Write(jsonBytes(before))
	h.Write(jsonBytes(after))
	h.Write(jsonBytes(contextVal))
	h.Write([]byte(createdAt.UTC().Format(time.RFC3339Nano)))
	return h.Sum(nil)
}

// jsonBytes is the inverse of normalizeJSON: we get back whatever
// normalizeJSON returned and want a byte slice for hashing.
func jsonBytes(v any) []byte {
	switch t := v.(type) {
	case nil:
		return nil
	case json.RawMessage:
		return t
	case []byte:
		return t
	case string:
		return []byte(t)
	default:
		b, _ := json.Marshal(v)
		return b
	}
}

func normalizeJSON(v json.RawMessage) any {
	if len(v) == 0 || !json.Valid(v) {
		return nil
	}
	return v
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
