package platform

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// WithTenantTx runs fn inside a database transaction with the tenant context
// configured via SET LOCAL app.tenant_id. This is a thin alias over
// dbutil.WithTenantTx, kept so existing call sites in the platform package
// (and downstream callers that already import platform) need not churn. New
// code in leaf packages that cannot import platform — e.g. internal/tenant,
// which platform itself imports for TenantMiddleware — should depend on
// dbutil directly to avoid the import cycle that previously forced a local
// copy of this helper.
//
// See internal/dbutil/txn.go for the canonical implementation.
func WithTenantTx(
	ctx context.Context,
	pool *pgxpool.Pool,
	tenantID uuid.UUID,
	fn func(ctx context.Context, tx pgx.Tx) error,
) error {
	return dbutil.WithTenantTx(ctx, pool, tenantID, fn)
}
