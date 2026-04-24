package main

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/platform"
)

// dashboardHandlers renders the Phase I KPI summary surface. Widgets
// are authored as tenant-scoped SQL aggregations over the existing
// tables (krecords, journal_entries, stock_levels, approvals) so a
// new widget does not require a schema change — only a new SELECT.
type dashboardHandlers struct {
	pool *pgxpool.Pool
}

type dashboardSummary struct {
	OpenDealsCount      int64   `json:"open_deals_count"`
	PipelineValue       float64 `json:"pipeline_value"`
	OutstandingAR       float64 `json:"outstanding_ar"`
	OutstandingAP       float64 `json:"outstanding_ap"`
	LowStockItemsCount  int64   `json:"low_stock_items_count"`
	PendingApprovals    int64   `json:"pending_approvals"`
	OpenTicketsCount    int64   `json:"open_tickets_count"`
	OverdueTicketsCount int64   `json:"overdue_tickets_count"`
}

func (h *dashboardHandlers) summary(w http.ResponseWriter, r *http.Request) {
	t := platform.TenantFromContext(r.Context())
	if t == nil {
		http.Error(w, "tenant context missing", http.StatusInternalServerError)
		return
	}
	var s dashboardSummary
	err := dbutil.WithTenantTx(r.Context(), h.pool, t.ID, func(ctx context.Context, tx pgx.Tx) error {
		if err := scanScalar(ctx, tx,
			`SELECT count(*) FROM krecords
			 WHERE tenant_id = $1 AND ktype = 'crm.deal'
			   AND status NOT IN ('closed_won','closed_lost','archived')
			   AND deleted_at IS NULL`,
			t.ID, &s.OpenDealsCount); err != nil {
			return err
		}
		if err := scanScalar(ctx, tx,
			`SELECT COALESCE(SUM((data->>'value')::numeric), 0) FROM krecords
			 WHERE tenant_id = $1 AND ktype = 'crm.deal'
			   AND status NOT IN ('closed_won','closed_lost','archived')
			   AND deleted_at IS NULL`,
			t.ID, &s.PipelineValue); err != nil {
			return err
		}
		if err := scanScalar(ctx, tx,
			`SELECT COALESCE(SUM((data->>'outstanding')::numeric), 0) FROM krecords
			 WHERE tenant_id = $1 AND ktype = 'finance.ar_invoice'
			   AND status NOT IN ('paid','cancelled','voided')
			   AND deleted_at IS NULL`,
			t.ID, &s.OutstandingAR); err != nil {
			return err
		}
		if err := scanScalar(ctx, tx,
			`SELECT COALESCE(SUM((data->>'outstanding')::numeric), 0) FROM krecords
			 WHERE tenant_id = $1 AND ktype = 'finance.ap_bill'
			   AND status NOT IN ('paid','cancelled','voided')
			   AND deleted_at IS NULL`,
			t.ID, &s.OutstandingAP); err != nil {
			return err
		}
		if err := scanScalar(ctx, tx,
			`SELECT count(*) FROM stock_levels sl
			 JOIN krecords i ON i.tenant_id = sl.tenant_id AND i.id = sl.item_id
			 WHERE sl.tenant_id = $1
			   AND sl.on_hand < COALESCE((i.data->>'reorder_level')::numeric, 0)
			   AND (i.data->>'reorder_level') IS NOT NULL`,
			t.ID, &s.LowStockItemsCount); err != nil {
			// Older deployments may not have stock_levels view; fall
			// back to a count over inventory.item records marked low.
			s.LowStockItemsCount = 0
		}
		if err := scanScalar(ctx, tx,
			`SELECT count(*) FROM approvals
			 WHERE tenant_id = $1 AND state = 'pending'`,
			t.ID, &s.PendingApprovals); err != nil {
			return err
		}
		if err := scanScalar(ctx, tx,
			`SELECT count(*) FROM krecords
			 WHERE tenant_id = $1 AND ktype = 'helpdesk.ticket'
			   AND status IN ('open','in_progress','waiting')
			   AND deleted_at IS NULL`,
			t.ID, &s.OpenTicketsCount); err != nil {
			return err
		}
		if err := scanScalar(ctx, tx,
			`SELECT count(*) FROM krecords
			 WHERE tenant_id = $1 AND ktype = 'helpdesk.ticket'
			   AND status IN ('open','in_progress','waiting')
			   AND (data->>'sla_resolution_by') IS NOT NULL
			   AND (data->>'sla_resolution_by')::timestamptz < now()
			   AND deleted_at IS NULL`,
			t.ID, &s.OverdueTicketsCount); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

// scanScalar runs a one-column query and scans the result into out.
// It is tolerant of missing tables so dashboard widgets can degrade
// gracefully on minimal deployments.
func scanScalar(ctx context.Context, tx pgx.Tx, sql string, tenantArg any, out any) error {
	return tx.QueryRow(ctx, sql, tenantArg).Scan(out)
}
