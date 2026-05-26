package manufacturing

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
)

// ReleaseWorkOrder transitions a draft work order to released and
// snapshots the currently active BOM. The bom_id is captured on the
// work order row so the consumption math at completion is
// reproducible even if a new BOM version is later activated.
//
// Returns ErrBOMNotActive when the work order's item has no active
// BOM and ErrWorkOrderInvalidTransition when the current status is
// not 'draft'.
func (s *PGStore) ReleaseWorkOrder(ctx context.Context, tenantID, woID uuid.UUID) (*WorkOrder, error) {
	if tenantID == uuid.Nil || woID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id and work order id required")
	}
	var out WorkOrder
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Lock the row so a concurrent release / cancel
		// can't race the state-machine guard below.
		if err := scanWorkOrder(tx.QueryRow(ctx,
			`SELECT tenant_id, id, item_id, bom_id, warehouse_id, planned_qty, actual_qty, status,
			        scheduled_start, scheduled_end, started_at, completed_at,
			        COALESCE(notes, ''), COALESCE(created_by, '00000000-0000-0000-0000-000000000000'::uuid),
			        created_at, updated_at
			   FROM work_orders
			  WHERE tenant_id = $1 AND id = $2
			  FOR UPDATE`,
			tenantID, woID), &out); err != nil {
			return err
		}
		if !out.CanTransitionTo(WorkOrderStatusReleased) {
			return fmt.Errorf("%w: %s → released", ErrWorkOrderInvalidTransition, out.Status)
		}
		if out.Status == WorkOrderStatusReleased {
			// Idempotent re-release is a no-op — the BOM
			// snapshot is already captured.
			return nil
		}

		bom, err := s.activeBOMForItem(ctx, tx, tenantID, out.ItemID)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx,
			`UPDATE work_orders
			    SET status = 'released', bom_id = $1, updated_at = now()
			  WHERE tenant_id = $2 AND id = $3`,
			bom.ID, tenantID, woID,
		); err != nil {
			return fmt.Errorf("manufacturing: release work order: %w", err)
		}
		out.Status = WorkOrderStatusReleased
		out.BOMID = &bom.ID
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// StartWorkOrder transitions a released work order to in_progress
// and stamps started_at.
func (s *PGStore) StartWorkOrder(ctx context.Context, tenantID, woID uuid.UUID) (*WorkOrder, error) {
	return s.transitionStatus(ctx, tenantID, woID, WorkOrderStatusInProgress, true, false, false)
}

// CancelWorkOrder transitions a draft / released / in_progress
// work order to cancelled. Cancelling a completed order is
// rejected — use a corrective work order instead so the inventory
// moves stay auditable.
func (s *PGStore) CancelWorkOrder(ctx context.Context, tenantID, woID uuid.UUID) (*WorkOrder, error) {
	return s.transitionStatus(ctx, tenantID, woID, WorkOrderStatusCancelled, false, false, false)
}

// CloseWorkOrder transitions a completed work order to closed
// (terminal). Used to lock a completed order out of further
// reporting changes; the inventory moves stay intact.
func (s *PGStore) CloseWorkOrder(ctx context.Context, tenantID, woID uuid.UUID) (*WorkOrder, error) {
	return s.transitionStatus(ctx, tenantID, woID, WorkOrderStatusClosed, false, false, false)
}

// transitionStatus is the shared state-machine update path used by
// the simple transitions (those that don't emit inventory moves).
// The completion path lives in CompleteWorkOrder because it has to
// emit moves and stamp actual_qty atomically with the status flip.
func (s *PGStore) transitionStatus(
	ctx context.Context,
	tenantID, woID uuid.UUID,
	target string,
	stampStartedAt, stampCompletedAt, _ bool,
) (*WorkOrder, error) {
	if tenantID == uuid.Nil || woID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id and work order id required")
	}
	var out WorkOrder
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := scanWorkOrder(tx.QueryRow(ctx,
			`SELECT tenant_id, id, item_id, bom_id, warehouse_id, planned_qty, actual_qty, status,
			        scheduled_start, scheduled_end, started_at, completed_at,
			        COALESCE(notes, ''), COALESCE(created_by, '00000000-0000-0000-0000-000000000000'::uuid),
			        created_at, updated_at
			   FROM work_orders
			  WHERE tenant_id = $1 AND id = $2
			  FOR UPDATE`,
			tenantID, woID), &out); err != nil {
			return err
		}
		if !out.CanTransitionTo(target) {
			return fmt.Errorf("%w: %s → %s", ErrWorkOrderInvalidTransition, out.Status, target)
		}
		if out.Status == target {
			return nil
		}

		q := `UPDATE work_orders SET status = $1, updated_at = now()`
		args := []any{target}
		idx := 2
		if stampStartedAt {
			q += fmt.Sprintf(", started_at = COALESCE(started_at, $%d)", idx)
			args = append(args, s.now())
			idx++
		}
		if stampCompletedAt {
			q += fmt.Sprintf(", completed_at = COALESCE(completed_at, $%d)", idx)
			args = append(args, s.now())
			idx++
		}
		q += fmt.Sprintf(" WHERE tenant_id = $%d AND id = $%d", idx, idx+1)
		args = append(args, tenantID, woID)

		if _, err := tx.Exec(ctx, q, args...); err != nil {
			return fmt.Errorf("manufacturing: transition work order: %w", err)
		}
		out.Status = target
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Re-fetch to surface the stamped timestamps.
	return s.GetWorkOrder(ctx, tenantID, woID)
}

// CompleteWorkOrderInput is the canonical input for
// CompleteWorkOrder.
type CompleteWorkOrderInput struct {
	// ActualQty is the realised yield. Must be > 0 and <=
	// planned_qty * (1 + yieldToleranceFactor). Pass
	// decimal.Decimal{} to default to planned_qty (i.e. nominal
	// yield).
	ActualQty decimal.Decimal
}

// yieldToleranceFactor caps the allowed over-yield at 10% above
// planned_qty. Over-yield beyond that almost always indicates a
// data-entry error and would consume more material than the BOM
// reserved, so the safer default is to reject it.
const yieldToleranceFactor = 0.10

// CompleteWorkOrder transitions a released or in_progress work
// order to completed, stamps actual_qty + completed_at, and emits
// the matching inventory moves:
//
//   - One consumption move (negative qty) per BOM component, scaled
//     by (actual_qty / bom.output_qty) and the component's
//     scrap_percent.
//   - One receipt move (positive qty) for the finished good
//     against the work order's warehouse.
//
// All moves are emitted with source_ktype =
// MoveSourceWorkOrderConsume / MoveSourceWorkOrderReceipt and
// source_id = work_order.id, so the existing
// inventory_moves_source_uniq partial unique index makes the whole
// completion idempotent on retry — a half-completed work order can
// be re-completed without double-issuing the components or
// double-receipting the finished good.
//
// Returns ErrWorkOrderInsufficientStock if any component would go
// negative; the work order stays in its current status in that
// case so the caller can pre-receipt components and retry.
func (s *PGStore) CompleteWorkOrder(ctx context.Context, tenantID, woID, actorID uuid.UUID, in CompleteWorkOrderInput) (*WorkOrder, error) {
	if tenantID == uuid.Nil || woID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id and work order id required")
	}
	if s.inventory == nil {
		return nil, errors.New("manufacturing: inventory store not configured; cannot emit completion moves")
	}

	// Phase 1: validate, snapshot the BOM, flip the status,
	// stamp actual_qty + completed_at, and compute the moves to
	// emit. Run inside a tenant tx so the row is locked while
	// we plan the moves.
	type plannedMove struct {
		itemID      uuid.UUID
		qty         decimal.Decimal
		sourceKType string
	}
	var moves []plannedMove
	var wo WorkOrder
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := scanWorkOrder(tx.QueryRow(ctx,
			`SELECT tenant_id, id, item_id, bom_id, warehouse_id, planned_qty, actual_qty, status,
			        scheduled_start, scheduled_end, started_at, completed_at,
			        COALESCE(notes, ''), COALESCE(created_by, '00000000-0000-0000-0000-000000000000'::uuid),
			        created_at, updated_at
			   FROM work_orders
			  WHERE tenant_id = $1 AND id = $2
			  FOR UPDATE`,
			tenantID, woID), &wo); err != nil {
			return err
		}
		if !wo.CanTransitionTo(WorkOrderStatusCompleted) {
			return fmt.Errorf("%w: %s → completed", ErrWorkOrderInvalidTransition, wo.Status)
		}

		// Re-completion is idempotent at the work-order row
		// level but NOT at the inventory-move level — Phase 2
		// commits one move per row, outside the Phase 1 tx, so
		// a crash between move 1 and move 3 leaves the work
		// order stamped `completed` while moves 2-3 never hit
		// inventory_moves. On retry we still need to plan the
		// same moves from the same actual_qty so Phase 2 can
		// replay them (ErrDuplicateSourceMove makes the moves
		// that DID land no-ops, while the missing ones finally
		// post). The early return that used to live here would
		// short-circuit the planning step and silently lose
		// the un-emitted moves forever.
		alreadyCompleted := wo.Status == WorkOrderStatusCompleted

		var actualQty decimal.Decimal
		if alreadyCompleted {
			// The stored value is the source of truth on the
			// replay path: it was already capped, validated,
			// and persisted on the original call. Ignore any
			// new `in.ActualQty` to avoid a retry silently
			// changing the yield from what was journalled.
			if wo.ActualQty == nil {
				return errors.New("manufacturing: completed work order missing actual_qty; data inconsistency")
			}
			actualQty = *wo.ActualQty
		} else {
			actualQty = in.ActualQty
			if actualQty.IsZero() {
				actualQty = wo.PlannedQty
			}
			if actualQty.IsNegative() {
				return errors.New("manufacturing: actual_qty must be >= 0")
			}
			// Cap upside at planned * (1 + tolerance). Anything
			// further almost always indicates a data-entry slip
			// and would consume more components than the BOM
			// reserved.
			maxQty := wo.PlannedQty.Mul(decimal.NewFromFloat(1 + yieldToleranceFactor))
			if actualQty.GreaterThan(maxQty) {
				return fmt.Errorf("manufacturing: actual_qty %s exceeds 110%% of planned %s", actualQty.String(), wo.PlannedQty.String())
			}
		}

		if wo.BOMID == nil {
			// Defensive: a released work order always has a
			// bom_id snapshot. Re-resolve from the active
			// BOM if for any reason the snapshot is missing.
			bom, err := s.activeBOMForItem(ctx, tx, tenantID, wo.ItemID)
			if err != nil {
				return err
			}
			wo.BOMID = &bom.ID
			if _, err := tx.Exec(ctx,
				`UPDATE work_orders SET bom_id = $1, updated_at = now()
				  WHERE tenant_id = $2 AND id = $3`,
				bom.ID, tenantID, woID,
			); err != nil {
				return fmt.Errorf("manufacturing: backfill bom_id: %w", err)
			}
		}
		// Load the snapshot.
		rows, err := tx.Query(ctx,
			`SELECT c.component_item_id, c.qty, c.uom, c.scrap_percent, c.sort_order, b.output_qty
			   FROM bom_components c
			   JOIN boms b ON b.tenant_id = c.tenant_id AND b.id = c.bom_id
			  WHERE c.tenant_id = $1 AND c.bom_id = $2
			  ORDER BY c.sort_order, c.component_item_id`,
			tenantID, *wo.BOMID,
		)
		if err != nil {
			return fmt.Errorf("manufacturing: load bom snapshot: %w", err)
		}
		type planComp struct {
			c         BOMComponent
			outputQty decimal.Decimal
		}
		var components []planComp
		for rows.Next() {
			var c BOMComponent
			var scrap *decimal.Decimal
			var outputQty decimal.Decimal
			if err := rows.Scan(&c.ComponentItemID, &c.Qty, &c.UOM, &scrap, &c.SortOrder, &outputQty); err != nil {
				rows.Close()
				return fmt.Errorf("manufacturing: scan bom snapshot: %w", err)
			}
			c.ScrapPercent = scrap
			components = append(components, planComp{c: c, outputQty: outputQty})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(components) == 0 {
			return ErrBOMHasNoComponents
		}

		// Compute consumption qty per component:
		//   per-batch = component.qty * (1 + scrap/100)
		//   total     = per-batch * actualQty / bom.output_qty
		//
		// The stock guard is skipped on the already-completed
		// replay path: some component moves may have already
		// landed in Phase 2 from the original call, so the
		// current SUM(qty) is the post-consumption balance.
		// Re-checking it against the same `total` would spuri-
		// ously fail (we'd be requiring twice the stock). The
		// original call already proved stock sufficiency before
		// flipping the WO to `completed`; that promise is what
		// we're now honoring.
		//
		// CONCURRENCY: the SELECT SUM below is NOT race-free
		// against concurrent work-order completions consuming
		// the same item/warehouse. Phase 1 holds FOR UPDATE on
		// the work_orders ROW but does not lock inventory_moves
		// or take an advisory lock keyed on the item. Two
		// CompleteWorkOrder calls for different work orders
		// can both see the pre-consumption SUM and both pass
		// their guards, then both emit moves in Phase 2 and
		// drive on-hand below zero. This is a deliberate
		// trade-off for the SME shop-floor target: the
		// alternative (xact-level pg_advisory_xact_lock keyed
		// on every component item) would serialize all
		// completions for any shared raw material across the
		// whole tenant, which on a busy line would hurt
		// throughput more than the negative-stock window costs.
		// The defence is depth-of-coverage:
		//   1. inventory_moves_source_uniq prevents double
		//      emission within a single work order;
		//   2. negative on-hand surfaces in the inventory
		//      dashboard and the next WO's stock guard;
		//   3. the operator pre-receipts the missing material
		//      and the negative window closes.
		// Revisit this if a tenant in the wild is materially
		// hurt by the loophole.
		var insufficient []string
		for _, pc := range components {
			perBatch := pc.c.EffectiveQty()
			total := perBatch.Mul(actualQty).Div(pc.outputQty)

			if !alreadyCompleted {
				// Stock guard: current SUM(qty) at this
				// warehouse must be >= total.
				var onHand decimal.Decimal
				err := tx.QueryRow(ctx,
					`SELECT COALESCE(SUM(qty), 0) FROM inventory_moves
					  WHERE tenant_id = $1 AND item_id = $2 AND warehouse_id = $3`,
					tenantID, pc.c.ComponentItemID, wo.WarehouseID,
				).Scan(&onHand)
				if err != nil {
					return fmt.Errorf("manufacturing: stock guard for %s: %w", pc.c.ComponentItemID, err)
				}
				if onHand.LessThan(total) {
					insufficient = append(insufficient,
						fmt.Sprintf("%s (need %s, have %s)",
							pc.c.ComponentItemID, total.String(), onHand.String()))
					continue
				}
			}

			moves = append(moves, plannedMove{
				itemID:      pc.c.ComponentItemID,
				qty:         total.Neg(),
				sourceKType: MoveSourceWorkOrderConsume,
			})
		}
		if len(insufficient) > 0 {
			return fmt.Errorf("%w: %s", ErrWorkOrderInsufficientStock, strings.Join(insufficient, "; "))
		}
		// Finished-goods receipt.
		moves = append(moves, plannedMove{
			itemID:      wo.ItemID,
			qty:         actualQty,
			sourceKType: MoveSourceWorkOrderReceipt,
		})

		if alreadyCompleted {
			// Phase 1 metadata (status / actual_qty /
			// completed_at) is already persisted from the
			// original call; this path exists solely to
			// re-populate the `moves` slice so Phase 2 can
			// replay any inventory moves that didn't land.
			// Re-running the UPDATE would be harmless but
			// pointlessly bumps updated_at and contends on
			// the locked row.
			return nil
		}

		// Flip status + stamp yield + completed_at.
		completedAt := s.now()
		if _, err := tx.Exec(ctx,
			`UPDATE work_orders
			    SET status = 'completed',
			        actual_qty = $1,
			        completed_at = COALESCE(completed_at, $2),
			        updated_at = now()
			  WHERE tenant_id = $3 AND id = $4`,
			actualQty, completedAt, tenantID, woID,
		); err != nil {
			return fmt.Errorf("manufacturing: flip work order to completed: %w", err)
		}
		wo.Status = WorkOrderStatusCompleted
		wo.ActualQty = &actualQty
		wo.CompletedAt = &completedAt
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Phase 2: emit the planned moves. The inventory store opens
	// its own tx per move; ErrDuplicateSourceMove is treated as
	// idempotent so a retry after a partial failure replays
	// cleanly. The composite source key (source_ktype, source_id,
	// item_id, warehouse_id) lets multiple components AND the
	// finished-good receipt all share the same source_id.
	sourceID := wo.ID
	for _, m := range moves {
		_, err := s.inventory.RecordMove(ctx, inventory.Move{
			TenantID:    tenantID,
			ItemID:      m.itemID,
			WarehouseID: wo.WarehouseID,
			Qty:         m.qty,
			SourceKType: m.sourceKType,
			SourceID:    &sourceID,
			CreatedBy:   actorID,
		})
		if err != nil && !errors.Is(err, inventory.ErrDuplicateSourceMove) {
			return nil, fmt.Errorf("manufacturing: emit move for %s: %w", m.itemID, err)
		}
	}
	// Re-fetch to surface the fresh updated_at (and pick up the
	// completed_at / actual_qty that Phase 1 stamped under now()) so
	// callers using updated_at for optimistic-concurrency checks get
	// the value the database actually persisted, not the in-memory
	// struct read at the start of Phase 1.
	return s.GetWorkOrder(ctx, tenantID, woID)
}
