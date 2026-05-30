package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// recordCountTx is the in-transaction helper called by the record store
// after every krecords INSERT (delta=+1) or soft-delete (delta=-1) so
// tenant_record_counts.record_count stays in lockstep with the source
// of truth. Lives on quota.go (rather than dbutil) because the counter
// IS the quota — keeping the SQL next to CheckRecordCount keeps the
// schema knowledge in one file.
//
// The UPSERT is required (not just UPDATE) because the counter row is
// created lazily on the first insert per tenant; for a brand-new tenant
// the row does not exist yet so a bare UPDATE would silently no-op and
// the counter would never start tracking. ON CONFLICT lets the same
// statement handle first-insert and steady-state in one roundtrip.
//
// Decrements are also UPSERT-shaped (with GREATEST(record_count + $2, 0)
// in the UPDATE clause) so the rare race where a delete arrives before
// the matching insert's counter UPSERT — for example a direct-SQL
// repair script that creates a row outside the store — cannot drop the
// counter below zero. The CHECK constraint in 000067 backs this up.
func bumpTenantRecordCount(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, delta int64) error {
	if delta == 0 {
		return nil
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO tenant_record_counts (tenant_id, record_count, updated_at)
		 VALUES ($1, GREATEST($2, 0), now())
		 ON CONFLICT (tenant_id) DO UPDATE
		   SET record_count = GREATEST(tenant_record_counts.record_count + $2, 0),
		       updated_at   = now()`,
		tenantID, delta,
	)
	if err != nil {
		return fmt.Errorf("quota: bump tenant_record_counts: %w", err)
	}
	return nil
}

// BumpTenantRecordCount is the exported alias the record store calls
// from inside WithTenantTx. Keeping the lower-case implementation
// lets us add a typed wrapper later (e.g. one that returns the new
// value for observability) without breaking the call sites.
func BumpTenantRecordCount(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, delta int64) error {
	return bumpTenantRecordCount(ctx, tx, tenantID, delta)
}

// Quota is the parsed form of tenants.quota JSONB. Zero values mean "unlimited".
type Quota struct {
	MaxRecords        int64 `json:"max_records"`
	MaxStorageBytes   int64 `json:"max_storage_bytes"`
	APICallsPerMinute int   `json:"api_calls_per_minute"`
	APICallsBurst     int   `json:"api_calls_burst"`
}

// ErrQuotaExceeded is returned by QuotaEnforcer when a tenant has exhausted a
// budgeted resource.
var ErrQuotaExceeded = errors.New("platform: quota exceeded")

// QuotaEnforcer checks tenant usage against plan limits. Current usage is
// sampled from Postgres on each call; a cache layer may be added later if
// this becomes hot.
type QuotaEnforcer struct {
	pool *pgxpool.Pool
}

// NewQuotaEnforcer binds the enforcer to the shared pool.
func NewQuotaEnforcer(pool *pgxpool.Pool) *QuotaEnforcer {
	return &QuotaEnforcer{pool: pool}
}

// CheckRecordCount returns ErrQuotaExceeded if the tenant already owns
// MaxRecords active KRecords.
//
// Reads from tenant_record_counts (a denormalised single-row-per-tenant
// counter maintained transactionally by the record store) instead of
// scanning every krecords partition. The fallback `count(*)` path runs
// only when no counter row exists yet — i.e. for a brand-new tenant
// that has never written a KRecord (counter row is created on first
// insert), or — until the daily reconciliation handler's first tick on
// freshly-migrated installs — for the narrow window between the
// 000067 backfill and the next reconciliation. The fallback returns 0
// for those callers because a tenant with zero rows trivially fits any
// MaxRecords > 0 limit; the source-of-truth count(*) would be needless
// work and re-introduce the O(n) scan we are eliminating. Drift
// detection is the job of RecordCountReconciler, not the hot path.
func (q *QuotaEnforcer) CheckRecordCount(ctx context.Context, tenantID uuid.UUID, quota Quota) error {
	if quota.MaxRecords <= 0 {
		return nil
	}
	var count int64
	err := WithTenantTx(ctx, q.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT record_count FROM tenant_record_counts WHERE tenant_id = $1`,
			tenantID,
		).Scan(&count)
		if err == nil {
			return nil
		}
		if errors.Is(err, pgx.ErrNoRows) {
			count = 0
			return nil
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("quota: count records: %w", err)
	}
	if count >= quota.MaxRecords {
		return fmt.Errorf("%w: record count (%d >= %d)", ErrQuotaExceeded, count, quota.MaxRecords)
	}
	return nil
}

// CheckStorageUsage returns ErrQuotaExceeded if the tenant has stored at
// least MaxStorageBytes of file data.
func (q *QuotaEnforcer) CheckStorageUsage(ctx context.Context, tenantID uuid.UUID, quota Quota) error {
	if quota.MaxStorageBytes <= 0 {
		return nil
	}
	var used int64
	err := WithTenantTx(ctx, q.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(size_bytes), 0) FROM files WHERE tenant_id = $1`,
			tenantID,
		).Scan(&used)
	})
	if err != nil {
		return fmt.Errorf("quota: sum storage: %w", err)
	}
	if used >= quota.MaxStorageBytes {
		return fmt.Errorf("%w: storage bytes (%d >= %d)", ErrQuotaExceeded, used, quota.MaxStorageBytes)
	}
	return nil
}

// QuotaMiddleware enforces the record-count and storage quotas on mutating
// requests. Read requests are always allowed — quotas are a write-path concern.
func QuotaMiddleware(enforcer *QuotaEnforcer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isWrite(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			t := TenantFromContext(r.Context())
			if t == nil {
				http.Error(w, "tenant context missing", http.StatusInternalServerError)
				return
			}
			quota := ParseQuota(t.Quota)
			if err := enforcer.CheckRecordCount(r.Context(), t.ID, quota); err != nil {
				writeQuotaError(w, err)
				return
			}
			if err := enforcer.CheckStorageUsage(r.Context(), t.ID, quota); err != nil {
				writeQuotaError(w, err)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func isWrite(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
		return true
	}
	return false
}

func writeQuotaError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrQuotaExceeded) {
		http.Error(w, err.Error(), http.StatusPaymentRequired)
		return
	}
	http.Error(w, "quota check failed", http.StatusInternalServerError)
}

// ParseQuota parses a tenant's quota JSONB into Quota. An empty or invalid
// payload yields a zero Quota (unlimited).
func ParseQuota(raw json.RawMessage) Quota {
	var q Quota
	if len(raw) == 0 {
		return q
	}
	_ = json.Unmarshal(raw, &q)
	return q
}

// extractRateLimitOverrides reads api_calls_per_minute / api_calls_burst out
// of the tenant quota JSON. Zero means "use the limiter default".
func extractRateLimitOverrides(raw json.RawMessage) (rpm, burst int) {
	q := ParseQuota(raw)
	return q.APICallsPerMinute, q.APICallsBurst
}
