package tenant

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// FeatureStore reads and writes the `tenant_features` table under
// tenant-scoped RLS. IsEnabled defaults unmarked features to "true"
// so a fresh tenant sees every feature without the wizard having
// had a chance to run; the wizard seeds explicit rows per plan to
// lock down that default on paid tiers (e.g. free plan → CRM only).
type FeatureStore struct {
	pool *pgxpool.Pool
}

// NewFeatureStore binds a store to the shared pool.
func NewFeatureStore(pool *pgxpool.Pool) *FeatureStore {
	return &FeatureStore{pool: pool}
}

// IsEnabled returns true if the tenant has the feature enabled. A
// missing row is treated as enabled so adding a new feature key
// does not require backfilling every tenant row — new keys ship as
// "on by default" and the tenant can explicitly disable by writing
// an enabled=false row.
func (s *FeatureStore) IsEnabled(ctx context.Context, tenantID uuid.UUID, featureKey string) (bool, error) {
	if tenantID == uuid.Nil {
		return false, errors.New("tenant: tenant id required")
	}
	if featureKey == "" {
		return false, errors.New("tenant: feature key required")
	}
	enabled := true
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var v bool
		err := tx.QueryRow(ctx,
			`SELECT enabled FROM tenant_features
			 WHERE tenant_id = $1 AND feature_key = $2`,
			tenantID, featureKey,
		).Scan(&v)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Unseeded features fall back to the plan-
				// neutral default: enabled. The wizard seed
				// is the authoritative gate for paid-tier
				// lockdown.
				enabled = true
				return nil
			}
			return err
		}
		enabled = v
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("tenant: lookup feature: %w", err)
	}
	return enabled, nil
}

// ListFeatures returns every (feature_key, enabled) row for the
// tenant plus the implicit defaults for any unrecorded canonical
// feature (see AllFeatures). The result always covers the full set
// so the UI can render toggles without having to reconcile missing
// keys on the client.
func (s *FeatureStore) ListFeatures(ctx context.Context, tenantID uuid.UUID) (map[string]bool, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("tenant: tenant id required")
	}
	out := map[string]bool{}
	for _, f := range AllFeatures {
		// Default everything enabled; overridden by rows below.
		out[f] = true
	}
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT feature_key, enabled FROM tenant_features WHERE tenant_id = $1`,
			tenantID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				key     string
				enabled bool
			)
			if err := rows.Scan(&key, &enabled); err != nil {
				return err
			}
			out[key] = enabled
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("tenant: list features: %w", err)
	}
	return out, nil
}

// SetFeatures upserts every (feature_key, enabled) pair in the
// supplied map. Absent keys are left untouched so callers can patch
// a subset without having to read-modify-write the whole set.
func (s *FeatureStore) SetFeatures(ctx context.Context, tenantID uuid.UUID, features map[string]bool) error {
	if tenantID == uuid.Nil {
		return errors.New("tenant: tenant id required")
	}
	if len(features) == 0 {
		return nil
	}
	return dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for key, enabled := range features {
			if key == "" {
				continue
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO tenant_features (tenant_id, feature_key, enabled)
				 VALUES ($1, $2, $3)
				 ON CONFLICT (tenant_id, feature_key)
				 DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = now()`,
				tenantID, key, enabled,
			); err != nil {
				return fmt.Errorf("tenant: upsert feature %q: %w", key, err)
			}
		}
		return nil
	})
}
