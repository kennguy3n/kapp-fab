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
	cfg.ConnConfig.Tracer = otelpgx.NewTracer(
		// IncludeQueryParameters defaults to false. The pgx tracer
		// would otherwise attach the rendered SQL parameters to
		// every span, which can leak PII (email addresses, names,
		// invoice line items) into the trace store. The parameter-
		// less query text is still attached so the span carries
		// enough debug context for "which statement was slow".
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
