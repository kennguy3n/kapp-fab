package platform

import (
	"context"
	"fmt"

	"github.com/exaring/otelpgx"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool constructs a pgxpool.Pool against the provided database URL. The
// pool uses pgx defaults tuned for the shared multi-tenant gateway pattern:
// one pool per process, transaction-scoped tenant context via SetTenantContext.
//
// The pool installs otelpgx as the connection tracer so every Query /
// Exec / CopyFrom call emits a child span when an OTel context is in
// scope. When the global TracerProvider is the no-op (KAPP_OTEL_ENDPOINT
// unset) the tracer hot-path is a single nil-check per call site —
// otelpgx's tracer is safe to install unconditionally.
func NewPool(ctx context.Context, dbURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		return nil, fmt.Errorf("parse db url: %w", err)
	}
	// Two separate concerns govern the otelpgx options here. Both are
	// deliberately wired; do NOT remove either thinking it's redundant.
	//
	//   PII protection (span attributes):
	//     otelpgx's IncludeQueryParameters option defaults to false,
	//     which means rendered SQL parameter values (email addresses,
	//     tenant names, invoice line-item descriptions) are NEVER
	//     attached to spans. We rely on this default — we MUST NOT
	//     pass otelpgx.WithIncludeQueryParameters() here. The
	//     parameter-less query text is still attached as the
	//     `db.statement` attribute so the span carries enough debug
	//     context for "which statement was slow".
	//
	//   Cardinality protection (span names):
	//     WithTrimSQLInSpanName() trims the SQL text used as the span
	//     name to a short prefix. Without this, every unique query
	//     (different ORDER BY clauses, dynamic SELECT lists, etc.)
	//     produces a distinct span name and explodes the span-name
	//     index in the tracing backend, same failure mode the HTTP
	//     middleware's chi RoutePattern rewrite guards against. Span
	//     attributes still carry the full statement.
	cfg.ConnConfig.Tracer = otelpgx.NewTracer(
		otelpgx.WithTrimSQLInSpanName(),
	)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("new pool: %w", err)
	}
	return pool, nil
}

// SetTenantContext injects the tenant id into the current transaction via
// `SET LOCAL app.tenant_id`. Row-level security policies on tenant-scoped
// tables read this GUC to filter rows. Must be called once per transaction
// before any tenant-scoped query.
func SetTenantContext(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	_, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String())
	if err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	return nil
}
