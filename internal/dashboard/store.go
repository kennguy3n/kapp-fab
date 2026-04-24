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
// landing page. Every field is a single tenant-scoped aggregation.
type Summary struct {
	OpenDealsCount      int64   `json:"open_deals_count"`
	PipelineValue       float64 `json:"pipeline_value"`
	OutstandingAR       float64 `json:"outstanding_ar"`
	OutstandingAP       float64 `json:"outstanding_ap"`
	LowStockItemsCount  int64   `json:"low_stock_items_count"`
	PendingApprovals    int64   `json:"pending_approvals"`
	OpenTicketsCount    int64   `json:"open_tickets_count"`
	OverdueTicketsCount int64   `json:"overdue_tickets_count"`
}

// Store computes per-tenant dashboard summaries. The pool is the
// regular application pool; every query runs under WithTenantTx so
// RLS + app.tenant_id back up the explicit tenant_id predicate.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wires a Store from the shared pool.
func NewStore(pool *pgxpool.Pool) *Store {
	if pool == nil {
		return nil
	}
	return &Store{pool: pool}
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
		if err := scanScalar(ctx, tx,
			`SELECT COALESCE(SUM((data->>'amount')::numeric), 0) FROM krecords
			 WHERE tenant_id = $1 AND ktype = 'crm.deal'
			   AND COALESCE(data->>'stage','') NOT IN ('won','lost')
			   AND deleted_at IS NULL`,
			tenantID, &out.PipelineValue); err != nil {
			return err
		}
		if err := scanScalar(ctx, tx,
			`SELECT COALESCE(SUM((data->>'outstanding_amount')::numeric), 0) FROM krecords
			 WHERE tenant_id = $1 AND ktype = 'finance.ar_invoice'
			   AND COALESCE(data->>'status','') NOT IN ('paid','cancelled','voided')
			   AND deleted_at IS NULL`,
			tenantID, &out.OutstandingAR); err != nil {
			return err
		}
		if err := scanScalar(ctx, tx,
			`SELECT COALESCE(SUM((data->>'outstanding_amount')::numeric), 0) FROM krecords
			 WHERE tenant_id = $1 AND ktype = 'finance.ap_bill'
			   AND COALESCE(data->>'status','') NOT IN ('paid','cancelled','voided')
			   AND deleted_at IS NULL`,
			tenantID, &out.OutstandingAP); err != nil {
			return err
		}
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
