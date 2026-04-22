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
func (q *QuotaEnforcer) CheckRecordCount(ctx context.Context, tenantID uuid.UUID, quota Quota) error {
	if quota.MaxRecords <= 0 {
		return nil
	}
	var count int64
	err := WithTenantTx(ctx, q.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM krecords WHERE tenant_id = $1 AND status != 'deleted'`,
			tenantID,
		).Scan(&count)
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
