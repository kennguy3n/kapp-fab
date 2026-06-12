package manufacturing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// RunMRP executes a material requirements planning run: it builds an
// in-memory snapshot of the tenant's active BOMs / routings / work
// centers and current supply, nets the supplied (and optionally
// min-stock) demand against it via the pure planMRP kernel, and persists
// the run header, its demand-line snapshot, and the resulting planned
// orders in a single transaction.
func (s *PGStore) RunMRP(ctx context.Context, tenantID, actorID uuid.UUID, in MRPRunInput) (*MRPRun, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id required")
	}
	if in.HorizonStart.IsZero() || in.HorizonEnd.IsZero() {
		return nil, fmt.Errorf("%w: horizon_start and horizon_end are required", ErrInvalidInput)
	}
	horizonStart := truncateToDate(in.HorizonStart)
	horizonEnd := truncateToDate(in.HorizonEnd)
	if horizonEnd.Before(horizonStart) {
		return nil, fmt.Errorf("%w: horizon_end must be on or after horizon_start", ErrInvalidInput)
	}
	if !in.IncludeMinStock && len(in.Demand) == 0 {
		return nil, ErrMRPNoDemand
	}
	buyLeadDays := in.BuyLeadTimeDays
	if buyLeadDays < 0 {
		return nil, fmt.Errorf("%w: buy_lead_time_days must be >= 0", ErrInvalidInput)
	}
	if buyLeadDays == 0 {
		buyLeadDays = defaultBuyLeadTimeDays
	}

	now := s.now()
	run := &MRPRun{
		TenantID:        tenantID,
		ID:              uuid.New(),
		Status:          MRPRunStatusCompleted,
		HorizonStart:    horizonStart,
		HorizonEnd:      horizonEnd,
		IncludeMinStock: in.IncludeMinStock,
		BuyLeadTimeDays: buyLeadDays,
		Notes:           in.Notes,
		CreatedBy:       actorID,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		bomByItem, err := loadActiveBOMs(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		routingByItem, err := loadActiveRoutings(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		wcAvail, err := loadWorkCenterAvailability(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		onHand, err := loadOnHandByItem(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		scheduled, err := loadScheduledReceiptsByItem(ctx, tx, tenantID)
		if err != nil {
			return err
		}

		// Assemble the independent demand snapshot. Explicit demand lines
		// due within the horizon come first; min-stock top-ups (if
		// enabled) are appended for items whose on-hand sits below their
		// reorder level.
		demandLines := make([]MRPDemandLine, 0, len(in.Demand))
		for _, d := range in.Demand {
			if d.ItemID == uuid.Nil {
				return fmt.Errorf("%w: demand line item_id required", ErrInvalidInput)
			}
			if d.Qty.IsZero() || d.Qty.IsNegative() {
				return fmt.Errorf("%w: demand line qty must be > 0", ErrInvalidInput)
			}
			if d.DueDate.IsZero() {
				return fmt.Errorf("%w: demand line due_date required", ErrInvalidInput)
			}
			due := truncateToDate(d.DueDate)
			if due.After(horizonEnd) {
				// Outside the planning horizon — ignored for this run.
				continue
			}
			source := d.Source
			if source == "" {
				source = MRPDemandSourceManual
			}
			demandLines = append(demandLines, MRPDemandLine{
				TenantID:  tenantID,
				ID:        uuid.New(),
				RunID:     run.ID,
				ItemID:    d.ItemID,
				Qty:       d.Qty,
				DueDate:   due,
				Source:    source,
				SourceRef: d.SourceRef,
				CreatedAt: now,
			})
		}
		if in.IncludeMinStock {
			minStock, err := loadMinStockDemand(ctx, tx, tenantID, onHand, horizonStart, run.ID, now)
			if err != nil {
				return err
			}
			demandLines = append(demandLines, minStock...)
		}
		if len(demandLines) == 0 {
			return ErrMRPNoDemand
		}

		// Collapse demand lines into per-item gross demand (sum qty,
		// earliest due) for the planner.
		independent := make(map[uuid.UUID]mrpDemand, len(demandLines))
		for i := range demandLines {
			dl := &demandLines[i]
			cur, ok := independent[dl.ItemID]
			if !ok {
				independent[dl.ItemID] = mrpDemand{qty: dl.Qty, due: dl.DueDate}
				continue
			}
			cur.qty = cur.qty.Add(dl.Qty)
			if dl.DueDate.Before(cur.due) {
				cur.due = dl.DueDate
			}
			independent[dl.ItemID] = cur
		}

		// Build the supply snapshot for every reachable item: the demand
		// items plus, transitively, the components of every make item.
		data := buildItemData(independent, bomByItem, routingByItem, onHand, scheduled)

		planned, err := planMRP(independent, data, wcAvail, buyLeadDays)
		if err != nil {
			return err
		}

		// Persist the run header.
		if _, err := tx.Exec(ctx,
			`INSERT INTO mrp_runs
			     (tenant_id, id, status, horizon_start, horizon_end, include_min_stock,
			      buy_lead_time_days, demand_line_count, planned_order_count,
			      make_order_count, buy_order_count, notes, created_by, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)`,
			run.TenantID, run.ID, run.Status, run.HorizonStart, run.HorizonEnd, run.IncludeMinStock,
			run.BuyLeadTimeDays, len(demandLines), len(planned),
			countOrderType(planned, MRPOrderTypeMake), countOrderType(planned, MRPOrderTypeBuy),
			nullableString(run.Notes), nullableUUID(run.CreatedBy), run.CreatedAt,
		); err != nil {
			return fmt.Errorf("manufacturing: insert mrp run: %w", err)
		}

		for i := range demandLines {
			dl := &demandLines[i]
			if _, err := tx.Exec(ctx,
				`INSERT INTO mrp_demand_lines
				     (tenant_id, id, run_id, item_id, qty, due_date, source, source_ref, created_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				dl.TenantID, dl.ID, dl.RunID, dl.ItemID, dl.Qty, dl.DueDate,
				dl.Source, nullableString(dl.SourceRef), dl.CreatedAt,
			); err != nil {
				return fmt.Errorf("manufacturing: insert mrp demand line: %w", err)
			}
		}

		for i := range planned {
			po := &planned[i]
			po.TenantID = tenantID
			po.ID = uuid.New()
			po.RunID = run.ID
			po.CreatedAt = now
			if _, err := tx.Exec(ctx,
				`INSERT INTO mrp_planned_orders
				     (tenant_id, id, run_id, item_id, order_type, qty, due_date,
				      suggested_start_date, explosion_level, bom_id, routing_id, lead_time_days, created_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
				po.TenantID, po.ID, po.RunID, po.ItemID, po.OrderType, po.Qty, po.DueDate,
				po.SuggestedStartDate, po.ExplosionLevel, po.BOMID, po.RoutingID, po.LeadTimeDays, po.CreatedAt,
			); err != nil {
				return fmt.Errorf("manufacturing: insert mrp planned order: %w", err)
			}
		}

		run.DemandLines = demandLines
		run.PlannedOrders = planned
		run.DemandLineCount = len(demandLines)
		run.PlannedOrderCount = len(planned)
		run.MakeOrderCount = countOrderType(planned, MRPOrderTypeMake)
		run.BuyOrderCount = countOrderType(planned, MRPOrderTypeBuy)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return run, nil
}

// countOrderType tallies planned orders of a given type for the run
// header's denormalized summary counters.
func countOrderType(planned []MRPPlannedOrder, orderType string) int {
	n := 0
	for i := range planned {
		if planned[i].OrderType == orderType {
			n++
		}
	}
	return n
}

// buildItemData assembles the per-item supply snapshot for every item
// reachable from the independent demand through the active-BOM graph.
// Items with an active BOM are make items (and carry their active
// routing when one exists); everything else is a buy item.
func buildItemData(
	independent map[uuid.UUID]mrpDemand,
	bomByItem map[uuid.UUID]*BOM,
	routingByItem map[uuid.UUID]*Routing,
	onHand, scheduled map[uuid.UUID]decimal.Decimal,
) map[uuid.UUID]mrpItemData {
	data := make(map[uuid.UUID]mrpItemData)
	var ensure func(item uuid.UUID)
	ensure = func(item uuid.UUID) {
		if _, ok := data[item]; ok {
			return
		}
		info := mrpItemData{
			OnHand:    onHand[item],
			Scheduled: scheduled[item],
		}
		if bom, ok := bomByItem[item]; ok {
			info.ActiveBOM = bom
			if r, ok := routingByItem[item]; ok {
				info.ActiveRouting = r
			}
		}
		data[item] = info
		if info.ActiveBOM != nil {
			for _, c := range info.ActiveBOM.Components {
				ensure(c.ComponentItemID)
			}
		}
	}
	for item := range independent {
		ensure(item)
	}
	return data
}

// loadActiveBOMs reads every active BOM for the tenant with its
// components attached, keyed by the item the BOM produces.
func loadActiveBOMs(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (map[uuid.UUID]*BOM, error) {
	rows, err := tx.Query(ctx,
		`SELECT tenant_id, id, item_id, version, status, output_qty, uom,
		        COALESCE(notes, ''), COALESCE(created_by, '00000000-0000-0000-0000-000000000000'::uuid),
		        created_at, updated_at
		   FROM boms
		  WHERE tenant_id = $1 AND status = 'active'`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("manufacturing: select active boms: %w", err)
	}
	byItem := make(map[uuid.UUID]*BOM)
	byID := make(map[uuid.UUID]*BOM)
	func() {
		defer rows.Close()
		for rows.Next() {
			var b BOM
			if err = rows.Scan(&b.TenantID, &b.ID, &b.ItemID, &b.Version, &b.Status, &b.OutputQty, &b.UOM,
				&b.Notes, &b.CreatedBy, &b.CreatedAt, &b.UpdatedAt); err != nil {
				return
			}
			bom := b
			byItem[bom.ItemID] = &bom
			byID[bom.ID] = &bom
		}
		err = rows.Err()
	}()
	if err != nil {
		return nil, fmt.Errorf("manufacturing: scan active bom: %w", err)
	}
	if len(byID) == 0 {
		return byItem, nil
	}

	bomIDs := make([]uuid.UUID, 0, len(byID))
	for id := range byID {
		bomIDs = append(bomIDs, id)
	}
	crows, err := tx.Query(ctx,
		`SELECT bom_id, component_item_id, qty, uom, scrap_percent, sort_order
		   FROM bom_components
		  WHERE tenant_id = $1 AND bom_id = ANY($2::uuid[])
		  ORDER BY bom_id, sort_order, component_item_id`,
		tenantID, bomIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("manufacturing: select bom components: %w", err)
	}
	defer crows.Close()
	for crows.Next() {
		var c BOMComponent
		var scrap *decimal.Decimal
		if err := crows.Scan(&c.BOMID, &c.ComponentItemID, &c.Qty, &c.UOM, &scrap, &c.SortOrder); err != nil {
			return nil, fmt.Errorf("manufacturing: scan bom component: %w", err)
		}
		c.ScrapPercent = scrap
		if bom, ok := byID[c.BOMID]; ok {
			bom.Components = append(bom.Components, c)
		}
	}
	return byItem, crows.Err()
}

// loadActiveRoutings reads every active routing for the tenant with its
// operations attached, keyed by the item it produces.
func loadActiveRoutings(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (map[uuid.UUID]*Routing, error) {
	rows, err := tx.Query(ctx, routingSelectColumns+
		` FROM routings WHERE tenant_id = $1 AND status = 'active'`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("manufacturing: select active routings: %w", err)
	}
	var routings []*Routing
	func() {
		defer rows.Close()
		for rows.Next() {
			var r Routing
			if scanErr := scanRouting(rows, &r); scanErr != nil {
				err = scanErr
				return
			}
			rt := r
			routings = append(routings, &rt)
		}
		if err == nil {
			err = rows.Err()
		}
	}()
	if err != nil {
		return nil, err
	}
	byItem := make(map[uuid.UUID]*Routing, len(routings))
	for _, r := range routings {
		ops, err := loadRoutingOperations(ctx, tx, tenantID, r.ID)
		if err != nil {
			return nil, err
		}
		r.Operations = ops
		byItem[r.ItemID] = r
	}
	return byItem, nil
}

// loadWorkCenterAvailability reads each work center's schedulable
// minutes per day, keyed by work-center id, for routing-derived make
// lead times.
func loadWorkCenterAvailability(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (map[uuid.UUID]decimal.Decimal, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, operating_hours_per_day, efficiency_percent, status
		   FROM work_centers
		  WHERE tenant_id = $1`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("manufacturing: select work centers: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID]decimal.Decimal)
	for rows.Next() {
		var wc WorkCenter
		if err := rows.Scan(&wc.ID, &wc.OperatingHoursPerDay, &wc.EfficiencyPercent, &wc.Status); err != nil {
			return nil, fmt.Errorf("manufacturing: scan work center: %w", err)
		}
		out[wc.ID] = wc.AvailableMinutesPerDay()
	}
	return out, rows.Err()
}

// loadOnHandByItem aggregates the signed inventory_moves ledger into the
// current on-hand quantity per item across all warehouses.
func loadOnHandByItem(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (map[uuid.UUID]decimal.Decimal, error) {
	rows, err := tx.Query(ctx,
		`SELECT item_id, COALESCE(SUM(qty), 0)
		   FROM inventory_moves
		  WHERE tenant_id = $1
		  GROUP BY item_id`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("manufacturing: aggregate on-hand: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID]decimal.Decimal)
	for rows.Next() {
		var item uuid.UUID
		var qty decimal.Decimal
		if err := rows.Scan(&item, &qty); err != nil {
			return nil, fmt.Errorf("manufacturing: scan on-hand: %w", err)
		}
		out[item] = qty
	}
	return out, rows.Err()
}

// loadScheduledReceiptsByItem sums the planned output of work orders
// already in flight (released or in progress) per item — the supply the
// planner credits against demand before suggesting new make/buy orders.
func loadScheduledReceiptsByItem(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (map[uuid.UUID]decimal.Decimal, error) {
	rows, err := tx.Query(ctx,
		`SELECT item_id, COALESCE(SUM(planned_qty), 0)
		   FROM work_orders
		  WHERE tenant_id = $1 AND status IN ('released', 'in_progress')
		  GROUP BY item_id`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("manufacturing: aggregate scheduled receipts: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID]decimal.Decimal)
	for rows.Next() {
		var item uuid.UUID
		var qty decimal.Decimal
		if err := rows.Scan(&item, &qty); err != nil {
			return nil, fmt.Errorf("manufacturing: scan scheduled receipt: %w", err)
		}
		out[item] = qty
	}
	return out, rows.Err()
}

// loadMinStockDemand synthesises a demand line for each item whose
// on-hand balance is below its inventory reorder_level, topping it back
// up to the reorder level. The synthesised line carries the reorder
// level as its gross quantity (NOT the shortfall): the planner nets
// on-hand off every item's gross demand exactly once, so emitting the
// target here yields a planned order of reorder_level - on_hand without
// double-counting the on-hand stock. Items already at or above their
// reorder level are skipped entirely, as are inactive (retired) items.
// Due on the horizon start (the shortfall already exists).
func loadMinStockDemand(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	onHand map[uuid.UUID]decimal.Decimal,
	horizonStart time.Time,
	runID uuid.UUID,
	now time.Time,
) ([]MRPDemandLine, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, sku, reorder_level
		   FROM inventory_items
		  WHERE tenant_id = $1 AND active = true AND reorder_level > 0`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("manufacturing: select reorder items: %w", err)
	}
	defer rows.Close()
	var out []MRPDemandLine
	for rows.Next() {
		var item uuid.UUID
		var sku string
		var reorder decimal.Decimal
		if err := rows.Scan(&item, &sku, &reorder); err != nil {
			return nil, fmt.Errorf("manufacturing: scan reorder item: %w", err)
		}
		// Skip items already at/above their reorder level; the planner
		// nets on-hand off the reorder target below.
		if reorder.Sub(onHand[item]).LessThanOrEqual(decimal.Zero) {
			continue
		}
		out = append(out, MRPDemandLine{
			TenantID:  tenantID,
			ID:        uuid.New(),
			RunID:     runID,
			ItemID:    item,
			Qty:       reorder,
			DueDate:   horizonStart,
			Source:    MRPDemandSourceMinStock,
			SourceRef: sku,
			CreatedAt: now,
		})
	}
	return out, rows.Err()
}

// GetMRPRun fetches a run header with its demand-line snapshot and
// planned-order output attached.
func (s *PGStore) GetMRPRun(ctx context.Context, tenantID, runID uuid.UUID) (*MRPRun, error) {
	if tenantID == uuid.Nil || runID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id and run id required")
	}
	var run MRPRun
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := scanMRPRun(tx.QueryRow(ctx, mrpRunSelectColumns+
			` FROM mrp_runs WHERE tenant_id = $1 AND id = $2`, tenantID, runID), &run); err != nil {
			return err
		}
		lines, err := loadMRPDemandLines(ctx, tx, tenantID, runID)
		if err != nil {
			return err
		}
		run.DemandLines = lines
		orders, err := loadMRPPlannedOrders(ctx, tx, tenantID, runID)
		if err != nil {
			return err
		}
		run.PlannedOrders = orders
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// ListMRPRuns returns the tenant's MRP run headers (no demand lines or
// planned orders) newest first.
func (s *PGStore) ListMRPRuns(ctx context.Context, tenantID uuid.UUID) ([]MRPRun, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id required")
	}
	out := make([]MRPRun, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, mrpRunSelectColumns+
			` FROM mrp_runs WHERE tenant_id = $1 ORDER BY created_at DESC`, tenantID)
		if err != nil {
			return fmt.Errorf("manufacturing: list mrp runs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var run MRPRun
			if err := scanMRPRun(rows, &run); err != nil {
				return err
			}
			out = append(out, run)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

const mrpRunSelectColumns = `SELECT tenant_id, id, status, horizon_start, horizon_end, include_min_stock,
        buy_lead_time_days, demand_line_count, planned_order_count, make_order_count, buy_order_count,
        COALESCE(notes, ''), COALESCE(created_by, '00000000-0000-0000-0000-000000000000'::uuid),
        created_at, updated_at`

func scanMRPRun(r pgxScanner, run *MRPRun) error {
	if err := r.Scan(
		&run.TenantID, &run.ID, &run.Status, &run.HorizonStart, &run.HorizonEnd, &run.IncludeMinStock,
		&run.BuyLeadTimeDays, &run.DemandLineCount, &run.PlannedOrderCount, &run.MakeOrderCount, &run.BuyOrderCount,
		&run.Notes, &run.CreatedBy, &run.CreatedAt, &run.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMRPRunNotFound
		}
		return fmt.Errorf("manufacturing: scan mrp run: %w", err)
	}
	return nil
}

func loadMRPDemandLines(ctx context.Context, tx pgx.Tx, tenantID, runID uuid.UUID) ([]MRPDemandLine, error) {
	rows, err := tx.Query(ctx,
		`SELECT tenant_id, id, run_id, item_id, qty, due_date, source, COALESCE(source_ref, ''), created_at
		   FROM mrp_demand_lines
		  WHERE tenant_id = $1 AND run_id = $2
		  ORDER BY created_at, id`,
		tenantID, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("manufacturing: select mrp demand lines: %w", err)
	}
	defer rows.Close()
	out := make([]MRPDemandLine, 0)
	for rows.Next() {
		var dl MRPDemandLine
		if err := rows.Scan(&dl.TenantID, &dl.ID, &dl.RunID, &dl.ItemID, &dl.Qty, &dl.DueDate,
			&dl.Source, &dl.SourceRef, &dl.CreatedAt); err != nil {
			return nil, fmt.Errorf("manufacturing: scan mrp demand line: %w", err)
		}
		out = append(out, dl)
	}
	return out, rows.Err()
}

func loadMRPPlannedOrders(ctx context.Context, tx pgx.Tx, tenantID, runID uuid.UUID) ([]MRPPlannedOrder, error) {
	rows, err := tx.Query(ctx,
		`SELECT tenant_id, id, run_id, item_id, order_type, qty, due_date, suggested_start_date,
		        explosion_level, bom_id, routing_id, lead_time_days, created_at
		   FROM mrp_planned_orders
		  WHERE tenant_id = $1 AND run_id = $2
		  ORDER BY explosion_level, item_id`,
		tenantID, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("manufacturing: select mrp planned orders: %w", err)
	}
	defer rows.Close()
	out := make([]MRPPlannedOrder, 0)
	for rows.Next() {
		var po MRPPlannedOrder
		if err := rows.Scan(&po.TenantID, &po.ID, &po.RunID, &po.ItemID, &po.OrderType, &po.Qty,
			&po.DueDate, &po.SuggestedStartDate, &po.ExplosionLevel, &po.BOMID, &po.RoutingID,
			&po.LeadTimeDays, &po.CreatedAt); err != nil {
			return nil, fmt.Errorf("manufacturing: scan mrp planned order: %w", err)
		}
		out = append(out, po)
	}
	return out, rows.Err()
}
