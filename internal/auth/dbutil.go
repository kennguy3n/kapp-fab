package auth

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// withTenantTx is a thin wrapper around dbutil.WithTenantTx so the
// auth package keeps its internal imports tidy. Exported helpers
// elsewhere use the same transaction pattern (SET LOCAL app.tenant_id
// for RLS enforcement).
func withTenantTx(
	ctx context.Context,
	pool *pgxpool.Pool,
	tenantID uuid.UUID,
	fn func(ctx context.Context, tx pgx.Tx) error,
) error {
	return dbutil.WithTenantTx(ctx, pool, tenantID, fn)
}
