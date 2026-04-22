package platform

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WithTenantTx runs fn inside a database transaction with the tenant context
// configured via SET LOCAL app.tenant_id. The tenant GUC persists for the life
// of the transaction and is cleared automatically on COMMIT or ROLLBACK, so
// there is no risk of context leaking across pooled connections.
//
// The transaction commits iff fn returns nil. Panics inside fn are rolled back
// and re-raised.
func WithTenantTx(
	ctx context.Context,
	pool *pgxpool.Pool,
	tenantID uuid.UUID,
	fn func(ctx context.Context, tx pgx.Tx) error,
) (err error) {
	if pool == nil {
		return errors.New("platform: nil pool")
	}
	if tenantID == uuid.Nil {
		return errors.New("platform: tenant id required")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("platform: begin tx: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.Background())
			panic(p)
		}
		if err != nil {
			if rbErr := tx.Rollback(context.Background()); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				err = fmt.Errorf("%w; rollback: %v", err, rbErr)
			}
		}
	}()

	if err = SetTenantContext(ctx, tx, tenantID); err != nil {
		return err
	}

	if err = fn(ctx, tx); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("platform: commit: %w", err)
	}
	return nil
}
