package inventory

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
)

// This file implements the lot/serial tracking layer that rides on top
// of the append-only inventory_moves ledger:
//
//   * itemTracking / validateMoveTracking enforce the per-item
//     contract (lot-tracked items must reference a batch; serial-tracked
//     items must enumerate one serial per unit moved).
//   * rollBatchQty is the single guarded path that maintains a lot's
//     qty_on_hand; it makes over-issue impossible by refusing any
//     decrement that would drive the lot negative.
//   * applyMoveSerials / reverseMoveSerials thread serial numbers
//     through every move (receipt, issue, transfer, reversal) and
//     record the move↔serial junction that backs traceability.
//   * ListSerials / GetSerial / ListStockLevelsByBatch / TraceSerial /
//     TraceLot are the read surface.

// itemTracking returns an item's (lotTracked, serialTracked) flags. It
// is a single indexed PK lookup on the caller's transaction so the
// tracking contract is evaluated under the same RLS scope as the move
// write itself.
//
// An item with no inventory_items row is treated as untracked
// (lot=false, serial=false) rather than an error: the move ledger
// historically accepts moves for items that only exist as generic
// krecords, and tracking is strictly opt-in via UpsertItem (which is
// the only path that writes the flag columns). Defaulting to untracked
// keeps every pre-existing move path working unchanged.
func (s *PGStore) itemTracking(ctx context.Context, tx pgx.Tx, tenantID, itemID uuid.UUID) (lot, serial bool, err error) {
	err = tx.QueryRow(ctx,
		`SELECT lot_tracked, serial_tracked
		   FROM inventory_items
		  WHERE tenant_id = $1 AND id = $2`,
		tenantID, itemID,
	).Scan(&lot, &serial)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("inventory: lookup item tracking: %w", err)
	}
	return lot, serial, nil
}

// validateMoveTracking checks a move against the item's tracking flags
// before any row is written.
func validateMoveTracking(m Move, lotTracked, serialTracked bool) error {
	if lotTracked && m.BatchID == nil {
		return ErrLotRequired
	}
	if len(m.SerialNos) > 0 && !serialTracked {
		return ErrSerialUnsupported
	}
	if serialTracked {
		if len(m.SerialNos) == 0 {
			return ErrSerialRequired
		}
		// One serial per physical unit. |qty| must match the count so
		// the ledger quantity and the serial registry never diverge.
		if !m.Qty.Abs().Equal(decimal.NewFromInt(int64(len(m.SerialNos)))) {
			return ErrSerialQtyMismatch
		}
	}
	return nil
}

// rollBatchQty adjusts a lot's qty_on_hand by delta inside the caller's
// transaction. The UPDATE is guarded: it only applies when the result
// stays non-negative, so a decrement that would over-issue the lot
// affects zero rows and is rejected with ErrInsufficientLotStock. A
// positive delta (receipt) can never trip the guard. The batch row is
// guaranteed to exist by the composite FK on inventory_moves plus the
// pre-INSERT linkage check, so zero rows on a decrement unambiguously
// means "would go negative".
func rollBatchQty(ctx context.Context, tx pgx.Tx, tenantID, batchID uuid.UUID, delta decimal.Decimal) error {
	tag, err := tx.Exec(ctx,
		`UPDATE inventory_batches
		    SET qty_on_hand = qty_on_hand + $1, updated_at = now()
		  WHERE tenant_id = $2 AND id = $3
		    AND qty_on_hand + $1 >= 0`,
		delta, tenantID, batchID,
	)
	if err != nil {
		return fmt.Errorf("inventory: roll batch qty: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrInsufficientLotStock
	}
	return nil
}

// applyMoveSerials creates/re-stocks serials on a receipt or decrements
// (transitions to a terminal state) serials on an issue, then links
// each to moveID via the inventory_move_serials junction. Callers
// guarantee len(m.SerialNos) > 0.
func (s *PGStore) applyMoveSerials(ctx context.Context, tx pgx.Tx, m Move, moveID int64) error {
	seen := make(map[string]struct{}, len(m.SerialNos))
	for _, sn := range m.SerialNos {
		if sn == "" {
			return fmt.Errorf("%w: empty serial number", ErrMoveInvalid)
		}
		if _, dup := seen[sn]; dup {
			return ErrDuplicateSerialInput
		}
		seen[sn] = struct{}{}
	}

	inbound := m.Qty.IsPositive()
	outStatus := m.SerialOutStatus
	if outStatus == "" {
		outStatus = SerialStatusConsumed
	}
	if !inbound && outStatus != SerialStatusConsumed && outStatus != SerialStatusDelivered {
		return fmt.Errorf("%w: invalid serial out status %q", ErrMoveInvalid, outStatus)
	}

	for _, sn := range m.SerialNos {
		var serialID uuid.UUID
		var err error
		if inbound {
			serialID, err = s.restockSerial(ctx, tx, m, sn)
		} else {
			serialID, err = s.issueSerial(ctx, tx, m, sn, outStatus)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO inventory_move_serials (tenant_id, move_id, serial_id)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (tenant_id, move_id, serial_id) DO NOTHING`,
			m.TenantID, moveID, serialID,
		); err != nil {
			return fmt.Errorf("inventory: link serial to move: %w", err)
		}
	}
	return nil
}

// restockSerial brings a serial into stock at the move's warehouse. A
// brand-new serial is inserted; a serial that previously left stock
// (terminal state, e.g. a returned unit) is re-stocked. A serial that
// is already in stock is a duplicate intake and surfaces
// ErrSerialAlreadyInStock.
func (s *PGStore) restockSerial(ctx context.Context, tx pgx.Tx, m Move, serialNo string) (uuid.UUID, error) {
	var (
		existingID     uuid.UUID
		existingStatus string
	)
	err := tx.QueryRow(ctx,
		`SELECT id, status FROM inventory_serials
		  WHERE tenant_id = $1 AND item_id = $2 AND serial_no = $3`,
		m.TenantID, m.ItemID, serialNo,
	).Scan(&existingID, &existingStatus)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		id := uuid.New()
		var batchArg any
		if m.BatchID != nil {
			batchArg = *m.BatchID
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO inventory_serials
			     (tenant_id, id, item_id, serial_no, status, warehouse_id, batch_id, created_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			m.TenantID, id, m.ItemID, serialNo, SerialStatusInStock, m.WarehouseID, batchArg,
			nullableUUIDValue(m.CreatedBy),
		); err != nil {
			return uuid.Nil, fmt.Errorf("inventory: insert serial: %w", err)
		}
		return id, nil
	case err != nil:
		return uuid.Nil, fmt.Errorf("inventory: lookup serial: %w", err)
	case existingStatus == SerialStatusInStock:
		return uuid.Nil, ErrSerialAlreadyInStock
	default:
		// Re-stock a previously-issued serial (e.g. a customer
		// return). Adopt the move's warehouse and (optionally) lot.
		var batchArg any
		if m.BatchID != nil {
			batchArg = *m.BatchID
		}
		if _, err := tx.Exec(ctx,
			`UPDATE inventory_serials
			    SET status = $1, warehouse_id = $2, batch_id = COALESCE($3, batch_id), updated_at = now()
			  WHERE tenant_id = $4 AND id = $5`,
			SerialStatusInStock, m.WarehouseID, batchArg, m.TenantID, existingID,
		); err != nil {
			return uuid.Nil, fmt.Errorf("inventory: restock serial: %w", err)
		}
		return existingID, nil
	}
}

// issueSerial transitions a serial out of stock. The UPDATE is guarded
// on (status = in_stock AND warehouse = move warehouse) so a serial
// that is missing, already issued, or sitting in a different warehouse
// surfaces ErrSerialNotAvailable rather than silently over-issuing.
func (s *PGStore) issueSerial(ctx context.Context, tx pgx.Tx, m Move, serialNo, outStatus string) (uuid.UUID, error) {
	var serialID uuid.UUID
	err := tx.QueryRow(ctx,
		`UPDATE inventory_serials
		    SET status = $1, warehouse_id = NULL, updated_at = now()
		  WHERE tenant_id = $2 AND item_id = $3 AND serial_no = $4
		    AND status = $5 AND warehouse_id = $6
		 RETURNING id`,
		outStatus, m.TenantID, m.ItemID, serialNo, SerialStatusInStock, m.WarehouseID,
	).Scan(&serialID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrSerialNotAvailable
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("inventory: issue serial: %w", err)
	}
	return serialID, nil
}

// reverseMoveSerials inverts the serial transitions made by the move
// being reversed and links the affected serials to the contra row. An
// inbound original (receipt) had brought serials into stock, so the
// reversal takes them back out; an outbound original (issue) had taken
// them out, so the reversal restores them to the original warehouse.
func (s *PGStore) reverseMoveSerials(
	ctx context.Context, tx pgx.Tx,
	tenantID uuid.UUID, origMoveID, contraMoveID int64,
	origQty decimal.Decimal, origWarehouse uuid.UUID,
) error {
	rows, err := tx.Query(ctx,
		`SELECT serial_id FROM inventory_move_serials
		  WHERE tenant_id = $1 AND move_id = $2`,
		tenantID, origMoveID,
	)
	if err != nil {
		return fmt.Errorf("inventory: load move serials: %w", err)
	}
	var serialIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("inventory: scan move serial: %w", err)
		}
		serialIDs = append(serialIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inventory: iterate move serials: %w", err)
	}

	origInbound := origQty.IsPositive()
	for _, id := range serialIDs {
		if origInbound {
			// Undo a receipt: remove the serial from stock.
			if _, err := tx.Exec(ctx,
				`UPDATE inventory_serials
				    SET status = $1, warehouse_id = NULL, updated_at = now()
				  WHERE tenant_id = $2 AND id = $3`,
				SerialStatusConsumed, tenantID, id,
			); err != nil {
				return fmt.Errorf("inventory: reverse receipt serial: %w", err)
			}
		} else {
			// Undo an issue: restore the serial to its origin
			// warehouse, back in stock.
			if _, err := tx.Exec(ctx,
				`UPDATE inventory_serials
				    SET status = $1, warehouse_id = $2, updated_at = now()
				  WHERE tenant_id = $3 AND id = $4`,
				SerialStatusInStock, origWarehouse, tenantID, id,
			); err != nil {
				return fmt.Errorf("inventory: reverse issue serial: %w", err)
			}
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO inventory_move_serials (tenant_id, move_id, serial_id)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (tenant_id, move_id, serial_id) DO NOTHING`,
			tenantID, contraMoveID, id,
		); err != nil {
			return fmt.Errorf("inventory: link reversed serial: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Read surface
// ---------------------------------------------------------------------------

// ListSerials returns serials for a tenant, narrowed by the supplied
// filter, ordered by (item_id, serial_no).
func (s *PGStore) ListSerials(ctx context.Context, tenantID uuid.UUID, filter SerialFilter) ([]Serial, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("inventory: tenant id required")
	}
	if filter.Limit <= 0 || filter.Limit > 1000 {
		filter.Limit = 200
	}
	out := make([]Serial, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		conds := []string{"tenant_id = $1"}
		args := []any{tenantID}
		n := 2
		if filter.ItemID != nil {
			conds = append(conds, fmt.Sprintf("item_id = $%d", n))
			args = append(args, *filter.ItemID)
			n++
		}
		if filter.WarehouseID != nil {
			conds = append(conds, fmt.Sprintf("warehouse_id = $%d", n))
			args = append(args, *filter.WarehouseID)
			n++
		}
		if filter.BatchID != nil {
			conds = append(conds, fmt.Sprintf("batch_id = $%d", n))
			args = append(args, *filter.BatchID)
			n++
		}
		if filter.Status != "" {
			conds = append(conds, fmt.Sprintf("status = $%d", n))
			args = append(args, filter.Status)
			n++
		}
		args = append(args, filter.Limit, filter.Offset)
		q := fmt.Sprintf(
			`SELECT tenant_id, id, item_id, serial_no, status, warehouse_id, batch_id, created_by, created_at, updated_at
			   FROM inventory_serials
			  WHERE %s
			  ORDER BY item_id, serial_no
			  LIMIT $%d OFFSET $%d`,
			joinAnd(conds), n, n+1,
		)
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("inventory: list serials: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			sr, err := scanSerial(rows)
			if err != nil {
				return err
			}
			out = append(out, sr)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetSerial loads a single serial by (item, serial_no).
func (s *PGStore) GetSerial(ctx context.Context, tenantID, itemID uuid.UUID, serialNo string) (*Serial, error) {
	if tenantID == uuid.Nil || itemID == uuid.Nil || serialNo == "" {
		return nil, errors.New("inventory: tenant id, item id, and serial no required")
	}
	var sr Serial
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT tenant_id, id, item_id, serial_no, status, warehouse_id, batch_id, created_by, created_at, updated_at
			   FROM inventory_serials
			  WHERE tenant_id = $1 AND item_id = $2 AND serial_no = $3`,
			tenantID, itemID, serialNo,
		)
		var err error
		sr, err = scanSerial(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSerialNotFound
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &sr, nil
}

// ListStockLevelsByBatch reads the per-lot balance projection
// (stock_levels_by_batch). When itemID is non-nil the result is scoped
// to that item. Rows are summed straight from the move ledger so they
// reflect committed reality independent of the batch running total.
func (s *PGStore) ListStockLevelsByBatch(ctx context.Context, tenantID uuid.UUID, itemID *uuid.UUID) ([]BatchStockLevel, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("inventory: tenant id required")
	}
	out := make([]BatchStockLevel, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		conds := []string{"tenant_id = $1"}
		args := []any{tenantID}
		if itemID != nil {
			conds = append(conds, "item_id = $2")
			args = append(args, *itemID)
		}
		q := fmt.Sprintf(
			`SELECT tenant_id, item_id, warehouse_id, batch_id, qty
			   FROM stock_levels_by_batch
			  WHERE %s
			  ORDER BY item_id, warehouse_id, batch_id`,
			joinAnd(conds),
		)
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("inventory: list batch stock levels: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var b BatchStockLevel
			if err := rows.Scan(&b.TenantID, &b.ItemID, &b.WarehouseID, &b.BatchID, &b.Qty); err != nil {
				return fmt.Errorf("inventory: scan batch stock level: %w", err)
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TraceSerial answers forward/backward traceability for a single
// serial: its current state plus every move that touched it,
// oldest-first. The first event is the origin (receipt / production);
// the last is where the unit went.
func (s *PGStore) TraceSerial(ctx context.Context, tenantID, itemID uuid.UUID, serialNo string) (*SerialTrace, error) {
	if tenantID == uuid.Nil || itemID == uuid.Nil || serialNo == "" {
		return nil, errors.New("inventory: tenant id, item id, and serial no required")
	}
	var trace SerialTrace
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT tenant_id, id, item_id, serial_no, status, warehouse_id, batch_id, created_by, created_at, updated_at
			   FROM inventory_serials
			  WHERE tenant_id = $1 AND item_id = $2 AND serial_no = $3`,
			tenantID, itemID, serialNo,
		)
		sr, err := scanSerial(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSerialNotFound
		}
		if err != nil {
			return err
		}
		trace.Serial = sr
		events, err := traceEventsBySerial(ctx, tx, tenantID, sr.ID)
		if err != nil {
			return err
		}
		trace.Events = events
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &trace, nil
}

// TraceLot answers traceability for a single lot: the batch plus every
// move that referenced it, oldest-first.
func (s *PGStore) TraceLot(ctx context.Context, tenantID, batchID uuid.UUID) (*LotTrace, error) {
	if tenantID == uuid.Nil || batchID == uuid.Nil {
		return nil, errors.New("inventory: tenant id and batch id required")
	}
	var trace LotTrace
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := getBatchTx(ctx, tx, tenantID, batchID)
		if err != nil {
			return err
		}
		trace.Batch = b
		rows, err := tx.Query(ctx,
			`SELECT id, item_id, warehouse_id, qty, source_ktype, source_id, batch_id, moved_at
			   FROM inventory_moves
			  WHERE tenant_id = $1 AND batch_id = $2
			  ORDER BY id`,
			tenantID, batchID,
		)
		if err != nil {
			return fmt.Errorf("inventory: load lot moves: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			ev, err := scanTraceEvent(rows)
			if err != nil {
				return err
			}
			trace.Events = append(trace.Events, ev)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return &trace, nil
}

// traceEventsBySerial walks every move linked to a serial, oldest-first.
func traceEventsBySerial(ctx context.Context, tx pgx.Tx, tenantID, serialID uuid.UUID) ([]TraceEvent, error) {
	rows, err := tx.Query(ctx,
		`SELECT m.id, m.item_id, m.warehouse_id, m.qty, m.source_ktype, m.source_id, m.batch_id, m.moved_at
		   FROM inventory_move_serials ms
		   JOIN inventory_moves m
		     ON m.tenant_id = ms.tenant_id AND m.id = ms.move_id
		  WHERE ms.tenant_id = $1 AND ms.serial_id = $2
		  ORDER BY m.id`,
		tenantID, serialID,
	)
	if err != nil {
		return nil, fmt.Errorf("inventory: load serial moves: %w", err)
	}
	defer rows.Close()
	var events []TraceEvent
	for rows.Next() {
		ev, err := scanTraceEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

// rowScanner is satisfied by both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanSerial(row rowScanner) (Serial, error) {
	var sr Serial
	if err := row.Scan(
		&sr.TenantID, &sr.ID, &sr.ItemID, &sr.SerialNo, &sr.Status,
		&sr.WarehouseID, &sr.BatchID, &sr.CreatedBy, &sr.CreatedAt, &sr.UpdatedAt,
	); err != nil {
		return Serial{}, err
	}
	return sr, nil
}

func scanTraceEvent(row rowScanner) (TraceEvent, error) {
	var (
		ev    TraceEvent
		srcK  *string
		srcID *uuid.UUID
		batch *uuid.UUID
	)
	if err := row.Scan(&ev.MoveID, &ev.ItemID, &ev.WarehouseID, &ev.Qty, &srcK, &srcID, &batch, &ev.MovedAt); err != nil {
		return TraceEvent{}, fmt.Errorf("inventory: scan trace event: %w", err)
	}
	if srcK != nil {
		ev.SourceKType = *srcK
	}
	ev.SourceID = srcID
	ev.BatchID = batch
	return ev, nil
}

func joinAnd(conds []string) string {
	out := ""
	for i, c := range conds {
		if i > 0 {
			out += " AND "
		}
		out += c
	}
	return out
}
