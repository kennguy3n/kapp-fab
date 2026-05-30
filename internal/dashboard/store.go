// Package dashboard owns the Phase I KPI summary surface. Widgets are
// authored as tenant-scoped SQL aggregations over the existing tables
// (krecords, journal_entries, approvals, stock_levels) so a new widget
// does not require a schema change — only a new SELECT. The package
// exposes a single Summary type so the HTTP handler and integration
// tests both drive off the same computation.
package dashboard

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// Summary is the set of KPI counters rendered on the dashboard
// landing page. Every monetary field is denominated in BaseCurrency
// (the tenant's functional currency); the runner converts foreign-
// currency krecords on the fly via the wired ExchangeRateStore.
type Summary struct {
	OpenDealsCount      int64   `json:"open_deals_count"`
	PipelineValue       float64 `json:"pipeline_value"`
	OutstandingAR       float64 `json:"outstanding_ar"`
	OutstandingAP       float64 `json:"outstanding_ap"`
	LowStockItemsCount  int64   `json:"low_stock_items_count"`
	PendingApprovals    int64   `json:"pending_approvals"`
	OpenTicketsCount    int64   `json:"open_tickets_count"`
	OverdueTicketsCount int64   `json:"overdue_tickets_count"`
	// PresentToday is the count of distinct hr.attendance KRecords for
	// the tenant whose `date` field is the current UTC date and whose
	// `status` is "present" (or "half_day"). Surfaced as the "Present
	// today" tile on the Phase G/L dashboard. Renders zero when no
	// attendance has been logged yet today, so the tile is always safe
	// to render unconditionally.
	PresentToday int64 `json:"present_today"`

	// PendingReviews counts hr.appraisal KRecords whose status sits
	// in the {submitted, reviewed} band. The combined count keeps
	// the tile relevant for both the reviewer (who acts on
	// "submitted") and the employee (who acks "reviewed") without
	// surfacing two near-identical widgets. Renders zero when the
	// appraisal surface isn't in use yet.
	PendingReviews int64 `json:"pending_reviews"`

	// BaseCurrency is the ISO-4217 code the monetary fields above
	// are expressed in. Set by ComputeSummary from the tenants
	// table; defaults to USD for tenants on the pre-000029 schema.
	BaseCurrency string `json:"base_currency"`
}

// Store computes per-tenant dashboard summaries. router routes the
// read-only aggregation transaction to a replica when one is
// configured + within KAPP_READ_REPLICA_LAG_TOLERANCE, transparently
// falling back to the primary otherwise. Every query runs under
// WithReadOnlyTenantTx so RLS + app.tenant_id back up the explicit
// tenant_id predicate and Postgres's READ ONLY tx mode catches a
// regression that introduces DML on this path.
type Store struct {
	router    *dbutil.PoolRouter
	converter Converter
}

// Converter narrows the dependency on ledger.ExchangeRateStore so
// dashboard tests can swap in a stub rate without spinning up the
// rates table. Returns the input amount unchanged when no rate is
// wired (Convert returns ok=false on error).
type Converter interface {
	Convert(ctx context.Context, tenantID uuid.UUID, amount float64, from, to string) (float64, bool)
}

// NewStore wires a Store from the shared pool. Equivalent to
// NewStoreWithRouter(dbutil.NewPoolRouter(pool)); kept as the
// canonical single-pool constructor so callers that haven't wired a
// replica don't need to reach into dbutil. Returns nil when pool is
// nil so the existing ComputeSummary nil-check still fires.
func NewStore(pool *pgxpool.Pool) *Store {
	if pool == nil {
		return nil
	}
	return &Store{router: dbutil.NewPoolRouter(pool)}
}

// NewStoreWithRouter wires a Store that routes reads through the
// supplied PoolRouter. Use this in the production wiring path where
// a process-wide router with a lag sampler is available.
func NewStoreWithRouter(router *dbutil.PoolRouter) *Store {
	if router == nil {
		return nil
	}
	return &Store{router: router}
}

// WithConverter wires a foreign-currency converter. When set, the
// monetary widgets (pipeline, AR, AP) sum per-currency totals and
// convert each foreign bucket into the tenant's base currency
// before aggregating.
func (s *Store) WithConverter(c Converter) *Store {
	s.converter = c
	return s
}

// ComputeSummary returns the tenant's KPI summary. All counters scan
// the live database in a single WithReadOnlyTenantTx so every query
// shares a consistent MVCC snapshot of the tenant's data (the READ
// ONLY transaction mode is just defense-in-depth — it doesn't relax
// the snapshot guarantee, which comes from running the queries inside
// the same transaction regardless of access mode). When a read
// replica is wired AND within lag tolerance the snapshot is taken
// against the replica; otherwise WithReadOnlyTenantTx falls back to
// the primary. See dbutil.PoolRouter for the routing semantics.
func (s *Store) ComputeSummary(ctx context.Context, tenantID uuid.UUID) (*Summary, error) {
	if s == nil || s.router == nil {
		return nil, errors.New("dashboard: store not wired")
	}
	if tenantID == uuid.Nil {
		return nil, errors.New("dashboard: tenant id required")
	}
	var out Summary
	// Dashboard aggregation is purely read — KPI rollups, no writes.
	// WithReadOnlyTenantTx routes to the replica when configured +
	// within lag tolerance and tags the tx as READ ONLY for
	// defense-in-depth.
	err := dbutil.WithReadOnlyTenantTx(ctx, s.router, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(base_currency, 'USD') FROM tenants WHERE id = $1`,
			tenantID,
		).Scan(&out.BaseCurrency); err != nil {
			return fmt.Errorf("dashboard: read base currency: %w", err)
		}
		// Business status lives in the JSONB `data` column — the
		// top-level krecords.status column is the record-lifecycle
		// flag (active/deleted).
		if err := scanScalar(ctx, tx,
			`SELECT count(*) FROM krecords
			 WHERE tenant_id = $1 AND ktype = 'crm.deal'
			   AND COALESCE(data->>'stage','') NOT IN ('won','lost')
			   AND deleted_at IS NULL`,
			tenantID, &out.OpenDealsCount); err != nil {
			return err
		}
		// Pipeline / AR / AP are summed per-currency so the
		// converter can fold each bucket into base currency below.
		// Currency defaults to the tenant's base when the krecord
		// data omits the field, preserving the legacy single-
		// currency aggregation for upgrades that haven't yet
		// re-saved their records.
		pipeline, err := sumByCurrency(ctx, tx,
			`SELECT COALESCE(data->>'currency', $2) AS cur,
			        COALESCE(SUM((data->>'amount')::numeric), 0) AS total
			   FROM krecords
			  WHERE tenant_id = $1 AND ktype = 'crm.deal'
			    AND COALESCE(data->>'stage','') NOT IN ('won','lost')
			    AND deleted_at IS NULL
			  GROUP BY cur`,
			tenantID, out.BaseCurrency)
		if err != nil {
			return err
		}
		out.PipelineValue = s.foldToBase(ctx, tenantID, pipeline, out.BaseCurrency)

		ar, err := sumByCurrency(ctx, tx,
			`SELECT COALESCE(data->>'currency', $2) AS cur,
			        COALESCE(SUM((data->>'outstanding_amount')::numeric), 0) AS total
			   FROM krecords
			  WHERE tenant_id = $1 AND ktype = 'finance.ar_invoice'
			    AND COALESCE(data->>'status','') NOT IN ('paid','cancelled','voided')
			    AND deleted_at IS NULL
			  GROUP BY cur`,
			tenantID, out.BaseCurrency)
		if err != nil {
			return err
		}
		out.OutstandingAR = s.foldToBase(ctx, tenantID, ar, out.BaseCurrency)

		ap, err := sumByCurrency(ctx, tx,
			`SELECT COALESCE(data->>'currency', $2) AS cur,
			        COALESCE(SUM((data->>'outstanding_amount')::numeric), 0) AS total
			   FROM krecords
			  WHERE tenant_id = $1 AND ktype = 'finance.ap_bill'
			    AND COALESCE(data->>'status','') NOT IN ('paid','cancelled','voided')
			    AND deleted_at IS NULL
			  GROUP BY cur`,
			tenantID, out.BaseCurrency)
		if err != nil {
			return err
		}
		out.OutstandingAP = s.foldToBase(ctx, tenantID, ap, out.BaseCurrency)
		if err := scanScalar(ctx, tx,
			// stock_levels rows are per-warehouse; dedupe by item so
			// the "low-stock items" widget matches its label.
			`SELECT count(DISTINCT sl.item_id) FROM stock_levels sl
			 JOIN krecords i ON i.tenant_id = sl.tenant_id AND i.id = sl.item_id
			 WHERE sl.tenant_id = $1
			   AND (i.data->>'reorder_level') IS NOT NULL
			   AND sl.qty < COALESCE((i.data->>'reorder_level')::numeric, 0)`,
			tenantID, &out.LowStockItemsCount); err != nil {
			return err
		}
		if err := scanScalar(ctx, tx,
			`SELECT count(*) FROM approvals
			 WHERE tenant_id = $1 AND state = 'pending'`,
			tenantID, &out.PendingApprovals); err != nil {
			return err
		}
		if err := scanScalar(ctx, tx,
			`SELECT count(*) FROM krecords
			 WHERE tenant_id = $1 AND ktype = 'helpdesk.ticket'
			   AND COALESCE(data->>'status','open') IN ('open','in_progress','waiting')
			   AND deleted_at IS NULL`,
			tenantID, &out.OpenTicketsCount); err != nil {
			return err
		}
		if err := scanScalar(ctx, tx,
			`SELECT count(*) FROM krecords
			 WHERE tenant_id = $1 AND ktype = 'helpdesk.ticket'
			   AND COALESCE(data->>'status','open') IN ('open','in_progress','waiting')
			   AND (data->>'sla_resolution_by') IS NOT NULL
			   AND (data->>'sla_resolution_by')::timestamptz < now()
			   AND deleted_at IS NULL`,
			tenantID, &out.OverdueTicketsCount); err != nil {
			return err
		}
		// Present-today tile: count distinct employees with an
		// hr.attendance record dated today and a present-leaning
		// status. Honours soft-deletes via deleted_at IS NULL so a
		// retracted attendance row drops off the tile immediately.
		if err := scanScalar(ctx, tx,
			`SELECT count(DISTINCT COALESCE(data->>'employee_id', id::text))
			   FROM krecords
			  WHERE tenant_id = $1 AND ktype = 'hr.attendance'
			    AND COALESCE(data->>'date','') = to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD')
			    AND COALESCE(data->>'status','') IN ('present','half_day')
			    AND deleted_at IS NULL`,
			tenantID, &out.PresentToday); err != nil {
			return err
		}
		// Pending reviews tile — count appraisals that are awaiting
		// either the reviewer's response (submitted) or the
		// employee's acknowledgement (reviewed). Honours
		// soft-deletes so a withdrawn cycle drops off immediately.
		// Renders zero when the appraisal KType isn't registered or
		// no rows exist yet, keeping the tile safe to mount on
		// every tenant.
		if err := scanScalar(ctx, tx,
			`SELECT count(*) FROM krecords
			 WHERE tenant_id = $1 AND ktype = 'hr.appraisal'
			   AND COALESCE(data->>'status','') IN ('submitted','reviewed')
			   AND deleted_at IS NULL`,
			tenantID, &out.PendingReviews); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("dashboard: compute summary: %w", err)
	}
	return &out, nil
}

// scanScalar runs a one-column query and scans the result into out.
func scanScalar(ctx context.Context, tx pgx.Tx, sql string, tenantArg any, out any) error {
	return tx.QueryRow(ctx, sql, tenantArg).Scan(out)
}

// sumByCurrency runs a (currency, total) grouping query and returns
// the result as a map keyed by ISO-4217 code. The empty-string key
// means "no currency on the record" — the caller is expected to have
// COALESCE'd it to the tenant's base currency upstream.
func sumByCurrency(ctx context.Context, tx pgx.Tx, sql string, tenantID uuid.UUID, baseCurrency string) (map[string]float64, error) {
	rows, err := tx.Query(ctx, sql, tenantID, baseCurrency)
	if err != nil {
		return nil, fmt.Errorf("dashboard: sum by currency: %w", err)
	}
	defer rows.Close()
	out := make(map[string]float64)
	for rows.Next() {
		var (
			cur   string
			total float64
		)
		if err := rows.Scan(&cur, &total); err != nil {
			return nil, fmt.Errorf("dashboard: scan currency row: %w", err)
		}
		if cur == "" {
			cur = baseCurrency
		}
		out[cur] += total
	}
	return out, rows.Err()
}

// foldToBase reduces a per-currency map to a single value in the
// tenant's base currency. When the converter is unwired or the
// rate lookup fails for a foreign bucket, that bucket falls back
// to its raw value — matches the legacy single-currency dashboard
// behaviour for tenants without rates configured.
func (s *Store) foldToBase(ctx context.Context, tenantID uuid.UUID, byCurrency map[string]float64, baseCurrency string) float64 {
	var total float64
	for cur, val := range byCurrency {
		if cur == baseCurrency || s.converter == nil {
			total += val
			continue
		}
		converted, ok := s.converter.Convert(ctx, tenantID, val, cur, baseCurrency)
		if !ok {
			total += val
			continue
		}
		total += converted
	}
	return total
}
