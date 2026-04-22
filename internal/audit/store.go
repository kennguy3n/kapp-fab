package audit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

	_, err := tx.Exec(ctx,
		`INSERT INTO audit_log
		     (tenant_id, actor_id, actor_kind, action, target_ktype, target_id, before, after, context)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		entry.TenantID,
		entry.ActorID,
		string(entry.ActorKind),
		entry.Action,
		nullIfEmpty(entry.TargetKType),
		entry.TargetID,
		before,
		after,
		c,
	)
	if err != nil {
		return fmt.Errorf("audit: insert: %w", err)
	}
	return nil
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
