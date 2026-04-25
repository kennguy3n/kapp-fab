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

	// BaseCurrency is the ISO-4217 code the monetary fields above
	// are expressed in. Set by ComputeSummary from the tenants
	// table; defaults to USD for tenants on the pre-000029 schema.
	BaseCurrency string `json:"base_currency"`
}

// Store computes per-tenant dashboard summaries. The pool is the
// regular application pool; every query runs under WithTenantTx so
// RLS + app.tenant_id back up the explicit tenant_id predicate.
type Store struct {
	pool      *pgxpool.Pool
	converter Converter
}

// Converter narrows the dependency on ledger.ExchangeRateStore so
// dashboard tests can swap in a stub rate without spinning up the
// rates table. Returns the input amount unchanged when no rate is
// wired (Convert returns ok=false on error).
type Converter interface {
	Convert(ctx context.Context, tenantID uuid.UUID, amount float64, from, to string) (float64, bool)
}

// NewStore wires a Store from the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	if pool == nil {
		return nil
	}
	return &Store{pool: pool}
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
// the live database in a single WithTenantTx so every query shares a
// consistent snapshot of the tenant's data.
func (s *Store) ComputeSummary(ctx context.Context, tenantID uuid.UUID) (*Summary, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("dashboard: store not wired")
	}
	if tenantID == uuid.Nil {
		return nil, errors.New("dashboard: tenant id required")
	}
	var out Summary
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
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
