// Package dbutil holds low-level database helpers that both the platform
// package and downstream tenant-scoped packages need, without forming an
// import cycle. Specifically, WithTenantTx and SetTenantContext live here
// so `internal/tenant` can call them directly without importing
// `internal/platform` (which imports `internal/tenant` for the TenantMiddleware
// and tenant lookup types).
package dbutil

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SetTenantContext injects the tenant id into the current transaction via
// `SET LOCAL app.tenant_id`. Row-level security policies on tenant-scoped
// tables read this GUC to filter rows. Must be called once per transaction
// before any tenant-scoped query.
func SetTenantContext(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
		return fmt.Errorf("dbutil: set tenant context: %w", err)
	}
	return nil
}

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
		return errors.New("dbutil: nil pool")
	}
	if tenantID == uuid.Nil {
		return errors.New("dbutil: tenant id required")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("dbutil: begin tx: %w", err)
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
		return fmt.Errorf("dbutil: commit: %w", err)
	}
	return nil
}

// WithReadOnlyTenantTx runs fn inside a read-only transaction with
// the tenant GUC populated, picking the read pool via the supplied
// PoolRouter. The transaction is opened with
// pgx.TxOptions{AccessMode: pgx.ReadOnly} as defense-in-depth so a
// stray INSERT/UPDATE/DELETE inside fn fails at the DB driver
// boundary instead of silently committing — particularly important
// when the router has routed us to a replica that would simply
// reject the write at the protocol level with a less actionable
// error.
//
// The fall-through semantics in PoolRouter.Read() apply: when the
// router has no replica wired, or when the most recent lag sample
// exceeds tolerance, this helper transparently runs against the
// primary. Callers therefore get "as-fresh-as-the-lag-tolerance-
// allows" reads without having to think about the routing layer.
func WithReadOnlyTenantTx(
	ctx context.Context,
	router *PoolRouter,
	tenantID uuid.UUID,
	fn func(ctx context.Context, tx pgx.Tx) error,
) (err error) {
	if router == nil {
		return errors.New("dbutil: nil router")
	}
	pool := router.Read()
	if pool == nil {
		return errors.New("dbutil: router returned nil pool")
	}
	return WithReadOnlyTenantTxOnPool(ctx, pool, tenantID, fn)
}

// WithReadOnlyTenantTxOnPool is the pool-pinned variant of
// WithReadOnlyTenantTx: instead of asking a PoolRouter where to read,
// the caller passes the pool directly. Use this when a sequence of
// reads must all hit the SAME pool — e.g. a keyset walk where the
// snapshot ceiling is captured against one pool and every chunk must
// honour that pool's replication position. Asking the router on every
// chunk would let a router flip mid-walk leave one chunk reading
// from primary and the next from replica, mixing two pools' snapshot
// semantics under one ceiling.
//
// All the read-only / tenant-GUC / explicit-commit semantics of
// WithReadOnlyTenantTx apply.
func WithReadOnlyTenantTxOnPool(
	ctx context.Context,
	pool *pgxpool.Pool,
	tenantID uuid.UUID,
	fn func(ctx context.Context, tx pgx.Tx) error,
) (err error) {
	if pool == nil {
		return errors.New("dbutil: nil pool")
	}
	if tenantID == uuid.Nil {
		return errors.New("dbutil: tenant id required")
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("dbutil: begin read-only tx: %w", err)
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

	// Even read-only tx benefits from an explicit commit so the
	// snapshot the queries ran under is released back to the pool
	// promptly (instead of waiting for the rollback in the defer
	// to fire on err == nil, which would be a no-op).
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("dbutil: commit read-only tx: %w", err)
	}
	return nil
}
