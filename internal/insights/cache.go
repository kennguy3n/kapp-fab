package insights

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// CacheStore reads and writes insights_query_cache rows under
// tenant-scoped RLS. Get short-circuits to a miss when the row is
// expired so callers never see stale data.
type CacheStore struct {
	pool *pgxpool.Pool
}

// NewCacheStore wires a CacheStore from the shared pool.
func NewCacheStore(pool *pgxpool.Pool) *CacheStore {
	return &CacheStore{pool: pool}
}

// ErrCacheMiss is the sentinel returned when no live cache row matches.
var ErrCacheMiss = errors.New("insights: cache miss")

// Get returns the cached result for a (queryHash, filterHash) pair if
// it exists and has not expired. Expired rows are deleted in-line so
// the cache never returns stale data even if the retention sweeper
// has not run yet.
func (s *CacheStore) Get(ctx context.Context, tenantID uuid.UUID, queryHash, filterHash string) (*QueryCache, error) {
	if tenantID == uuid.Nil {
		return nil, validationErr("tenant id required")
	}
	if queryHash == "" {
		return nil, validationErr("query hash required")
	}
	out := QueryCache{
		TenantID:   tenantID,
		QueryHash:  queryHash,
		FilterHash: filterHash,
	}
	// expired captures whether the row was found-but-stale so the
	// in-line DELETE can commit alongside the read; returning
	// ErrCacheMiss from inside the transaction would roll the DELETE
	// back (dbutil.WithTenantTx rolls back on any non-nil error) and
	// the stale row would persist until SweepExpired runs.
	var (
		miss    bool
		expired bool
	)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var (
			result    []byte
			queryID   *uuid.UUID
			expiresAt time.Time
		)
		row := tx.QueryRow(ctx,
			`SELECT query_id, result, row_count, created_at, expires_at
			   FROM insights_query_cache
			  WHERE tenant_id = $1 AND query_hash = $2 AND filter_hash = $3`,
			tenantID, queryHash, filterHash,
		)
		if err := row.Scan(&queryID, &result, &out.RowCount, &out.CreatedAt, &expiresAt); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				miss = true
				return nil
			}
			return err
		}
		if !expiresAt.After(timeNow()) {
			// Stale row — purge in-line and let the transaction
			// commit so the caller's next Set has a clean slot.
			if _, err := tx.Exec(ctx,
				`DELETE FROM insights_query_cache
				  WHERE tenant_id = $1 AND query_hash = $2 AND filter_hash = $3`,
				tenantID, queryHash, filterHash,
			); err != nil {
				return err
			}
			expired = true
			return nil
		}
		out.QueryID = queryID
		out.ExpiresAt = expiresAt
		out.Result = json.RawMessage(result)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if miss || expired {
		return nil, ErrCacheMiss
	}
	return &out, nil
}

// Set upserts a cache row. ttl <= 0 short-circuits to no-op so the
// caller can disable caching per query without conditional logic on
// every code path.
func (s *CacheStore) Set(ctx context.Context, tenantID uuid.UUID, queryHash, filterHash string, queryID *uuid.UUID, result json.RawMessage, rowCount int, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	if tenantID == uuid.Nil {
		return validationErr("tenant id required")
	}
	if queryHash == "" {
		return validationErr("query hash required")
	}
	if len(result) == 0 {
		return validationErr("result required")
	}
	expiresAt := timeNow().Add(ttl)
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var qID any
		if queryID != nil {
			qID = *queryID
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO insights_query_cache
			   (tenant_id, query_hash, filter_hash, query_id, result, row_count, expires_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)
			 ON CONFLICT (tenant_id, query_hash, filter_hash) DO UPDATE
			   SET query_id   = EXCLUDED.query_id,
			       result     = EXCLUDED.result,
			       row_count  = EXCLUDED.row_count,
			       created_at = now(),
			       expires_at = EXCLUDED.expires_at`,
			tenantID, queryHash, filterHash, qID, []byte(result), rowCount, expiresAt,
		)
		if err != nil {
			return fmt.Errorf("insights: cache set: %w", err)
		}
		return nil
	})
}

// Invalidate removes a single (queryHash, filterHash) cache row.
func (s *CacheStore) Invalidate(ctx context.Context, tenantID uuid.UUID, queryHash, filterHash string) error {
	if tenantID == uuid.Nil || queryHash == "" {
		return validationErr("tenant id and query hash required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM insights_query_cache
			  WHERE tenant_id = $1 AND query_hash = $2 AND filter_hash = $3`,
			tenantID, queryHash, filterHash,
		)
		return err
	})
}

// InvalidateQuery removes every cache row tied to a saved query id.
// Used on Update / Delete of a saved query so the next run rebuilds
// the cache from scratch.
func (s *CacheStore) InvalidateQuery(ctx context.Context, tenantID, queryID uuid.UUID) error {
	if tenantID == uuid.Nil || queryID == uuid.Nil {
		return validationErr("tenant id and query id required")
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM insights_query_cache
			  WHERE tenant_id = $1 AND query_id = $2`,
			tenantID, queryID,
		)
		return err
	})
}

// SweepExpired removes every expired row for a tenant. Wired into the
// data_retention_sweep scheduled action so a cold query cache cannot
// accumulate forever even when the runner stops being invoked.
func (s *CacheStore) SweepExpired(ctx context.Context, tenantID uuid.UUID) (int64, error) {
	if tenantID == uuid.Nil {
		return 0, validationErr("tenant id required")
	}
	var deleted int64
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM insights_query_cache
			  WHERE tenant_id = $1 AND expires_at <= now()`,
			tenantID,
		)
		if err != nil {
			return err
		}
		deleted = tag.RowsAffected()
		return nil
	})
	return deleted, err
}
