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

// CreateSubcontractOrderInput is the canonical input for
// CreateSubcontractOrder. Components must be non-empty.
type CreateSubcontractOrderInput struct {
	WorkOrderID         *uuid.UUID
	RoutingOperationSeq *int
	SupplierID          *uuid.UUID
	ItemID              uuid.UUID
	WarehouseID         uuid.UUID
	Qty                 decimal.Decimal
	ChargeAmount        decimal.Decimal
	ChargeCurrency      string
	Notes               string
	Components          []SubcontractComponentInput
}

// SubcontractComponentInput is one component line on a create request.
type SubcontractComponentInput struct {
	ItemID uuid.UUID
	Qty    decimal.Decimal
}

// CreateSubcontractOrder inserts a draft subcontract order plus its
// component rows in a single transaction.
func (s *PGStore) CreateSubcontractOrder(ctx context.Context, tenantID, actorID uuid.UUID, in CreateSubcontractOrderInput) (*SubcontractOrder, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id required")
	}
	if in.ItemID == uuid.Nil || in.WarehouseID == uuid.Nil {
		return nil, fmt.Errorf("%w: item_id and warehouse_id required", ErrInvalidInput)
	}
	if in.Qty.IsZero() || in.Qty.IsNegative() {
		return nil, fmt.Errorf("%w: qty must be > 0", ErrInvalidInput)
	}
	if in.ChargeAmount.IsNegative() {
		return nil, fmt.Errorf("%w: charge_amount must be >= 0", ErrInvalidInput)
	}
	if in.RoutingOperationSeq != nil && *in.RoutingOperationSeq <= 0 {
		return nil, fmt.Errorf("%w: routing_operation_seq must be > 0", ErrInvalidInput)
	}
	if len(in.Components) == 0 {
		return nil, ErrSubcontractNoComponents
	}
	seen := make(map[uuid.UUID]struct{}, len(in.Components))
	for _, c := range in.Components {
		if c.ItemID == uuid.Nil {
			return nil, fmt.Errorf("%w: component item_id required", ErrInvalidInput)
		}
		if c.Qty.IsZero() || c.Qty.IsNegative() {
			return nil, fmt.Errorf("%w: component %s qty must be > 0", ErrInvalidInput, c.ItemID)
		}
		if _, dup := seen[c.ItemID]; dup {
			return nil, ErrSubcontractDuplicateComponent
		}
		seen[c.ItemID] = struct{}{}
	}

	now := s.now()
	order := &SubcontractOrder{
		TenantID:            tenantID,
		ID:                  uuid.New(),
		WorkOrderID:         in.WorkOrderID,
		RoutingOperationSeq: in.RoutingOperationSeq,
		SupplierID:          in.SupplierID,
		ItemID:              in.ItemID,
		WarehouseID:         in.WarehouseID,
		Qty:                 in.Qty,
		ReceivedQty:         decimal.Zero,
		Status:              SubcontractStatusDraft,
		ChargeAmount:        in.ChargeAmount,
		ChargeCurrency:      in.ChargeCurrency,
		Notes:               in.Notes,
		CreatedBy:           actorID,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	for _, c := range in.Components {
		order.Components = append(order.Components, SubcontractComponent{
			TenantID:           tenantID,
			ID:                 uuid.New(),
			SubcontractOrderID: order.ID,
			ItemID:             c.ItemID,
			Qty:                c.Qty,
			IssuedQty:          decimal.Zero,
			CreatedAt:          now,
		})
	}

	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO subcontract_orders
			     (tenant_id, id, work_order_id, routing_operation_seq, supplier_id, item_id,
			      warehouse_id, qty, received_qty, status, charge_amount, charge_currency,
			      notes, created_by, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $15)`,
			order.TenantID, order.ID, order.WorkOrderID, order.RoutingOperationSeq, nullableUUIDPtr(order.SupplierID),
			order.ItemID, order.WarehouseID, order.Qty, order.ReceivedQty, order.Status,
			order.ChargeAmount, nullableString(order.ChargeCurrency),
			nullableString(order.Notes), nullableUUID(order.CreatedBy), order.CreatedAt,
		); err != nil {
			return fmt.Errorf("manufacturing: insert subcontract order: %w", err)
		}
		for _, c := range order.Components {
			if _, err := tx.Exec(ctx,
				`INSERT INTO subcontract_components
				     (tenant_id, id, subcontract_order_id, item_id, qty, issued_qty, created_at)
				 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				c.TenantID, c.ID, c.SubcontractOrderID, c.ItemID, c.Qty, c.IssuedQty, c.CreatedAt,
			); err != nil {
				return fmt.Errorf("manufacturing: insert subcontract component: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return order, nil
}

// GetSubcontractOrder fetches an order header with its components.
func (s *PGStore) GetSubcontractOrder(ctx context.Context, tenantID, orderID uuid.UUID) (*SubcontractOrder, error) {
	if tenantID == uuid.Nil || orderID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id and order id required")
	}
	var order *SubcontractOrder
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		o, err := loadSubcontractOrderTx(ctx, tx, tenantID, orderID, false)
		if err != nil {
			return err
		}
		order = o
		return nil
	})
	if err != nil {
		return nil, err
	}
	return order, nil
}

// ListSubcontractOrders returns order headers (no components) newest
// first, optionally filtered by status.
func (s *PGStore) ListSubcontractOrders(ctx context.Context, tenantID uuid.UUID, status string) ([]SubcontractOrder, error) {
	if tenantID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id required")
	}
	out := make([]SubcontractOrder, 0)
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := subcontractOrderSelectColumns + ` FROM subcontract_orders WHERE tenant_id = $1`
		args := []any{tenantID}
		if status != "" {
			q += ` AND status = $2`
			args = append(args, status)
		}
		q += ` ORDER BY created_at DESC`
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("manufacturing: list subcontract orders: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var o SubcontractOrder
			if err := scanSubcontractOrder(rows, &o); err != nil {
				return err
			}
			out = append(out, o)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// IssueSubcontractOrder transitions a draft order to issued, emitting a
// negative inventory move for each component (out of the order's
// warehouse to the supplier). Idempotent: a retry after a partial move
// failure replays the moves without re-flipping the status.
func (s *PGStore) IssueSubcontractOrder(ctx context.Context, tenantID, actorID, orderID uuid.UUID, in IssueSubcontractInput) (*SubcontractOrder, error) {
	if s.inventory == nil {
		return nil, errors.New("manufacturing: inventory store required to issue subcontract components")
	}
	if tenantID == uuid.Nil || orderID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id and order id required")
	}

	var order *SubcontractOrder
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		o, err := loadSubcontractOrderTx(ctx, tx, tenantID, orderID, true)
		if err != nil {
			return err
		}
		order = o

		switch o.Status {
		case SubcontractStatusIssued:
			// Replay path: status already flipped on a prior call;
			// Phase 2 below re-emits any moves that did not land.
			return nil
		case SubcontractStatusDraft:
			// Fall through to validate + flip.
		default:
			return fmt.Errorf("%w: %s -> issued", ErrSubcontractInvalidTransition, o.Status)
		}

		// Validate every component move up front (tracking contract +
		// stock guard) so a bad input fails the whole issue before the
		// status flips.
		var insufficient []string
		for _, c := range o.Components {
			batchID, serials := in.componentPayload(c.ItemID)
			if err := validateTrackedMove(ctx, tx, tenantID, c.ItemID, c.Qty.Neg(), batchID, serials); err != nil {
				return err
			}
			var onHand decimal.Decimal
			if err := tx.QueryRow(ctx,
				`SELECT COALESCE(SUM(qty), 0) FROM inventory_moves
				  WHERE tenant_id = $1 AND item_id = $2 AND warehouse_id = $3`,
				tenantID, c.ItemID, o.WarehouseID,
			).Scan(&onHand); err != nil {
				return fmt.Errorf("manufacturing: read on-hand for %s: %w", c.ItemID, err)
			}
			if onHand.LessThan(c.Qty) {
				insufficient = append(insufficient,
					fmt.Sprintf("%s (need %s, have %s)", c.ItemID, c.Qty.String(), onHand.String()))
			}
		}
		if len(insufficient) > 0 {
			return fmt.Errorf("%w: %s", ErrSubcontractInsufficientStock, strings.Join(insufficient, "; "))
		}

		issuedAt := s.now()
		if _, err := tx.Exec(ctx,
			`UPDATE subcontract_orders
			    SET status = 'issued', issued_at = COALESCE(issued_at, $1), updated_at = now()
			  WHERE tenant_id = $2 AND id = $3`,
			issuedAt, tenantID, orderID,
		); err != nil {
			return fmt.Errorf("manufacturing: flip subcontract order to issued: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE subcontract_components SET issued_qty = qty
			  WHERE tenant_id = $1 AND subcontract_order_id = $2`,
			tenantID, orderID,
		); err != nil {
			return fmt.Errorf("manufacturing: stamp issued quantities: %w", err)
		}
		o.Status = SubcontractStatusIssued
		o.IssuedAt = &issuedAt
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Phase 2: emit the negative component moves. Idempotent via the
	// inventory_moves source uniqueness; a duplicate is treated as a
	// successful replay.
	for _, c := range order.Components {
		batchID, serials := in.componentPayload(c.ItemID)
		if _, err := s.inventory.RecordMove(ctx, inventory.Move{
			TenantID:        tenantID,
			ItemID:          c.ItemID,
			WarehouseID:     order.WarehouseID,
			Qty:             c.Qty.Neg(),
			SourceKType:     MoveSourceSubcontractIssue,
			SourceID:        &order.ID,
			CreatedBy:       actorID,
			BatchID:         batchID,
			SerialNos:       serials,
			SerialOutStatus: inventory.SerialStatusConsumed,
		}); err != nil && !errors.Is(err, inventory.ErrDuplicateSourceMove) {
			return nil, fmt.Errorf("manufacturing: emit subcontract issue move for %s: %w", c.ItemID, err)
		}
	}
	return s.GetSubcontractOrder(ctx, tenantID, orderID)
}

// IssueSubcontractInput carries optional lot/serial payloads for the
// component issue moves, keyed by component item id. Items without a
// tracking requirement may omit an entry.
type IssueSubcontractInput struct {
	ComponentBatches map[uuid.UUID]uuid.UUID
	ComponentSerials map[uuid.UUID][]string
}

func (in IssueSubcontractInput) componentPayload(item uuid.UUID) (batch *uuid.UUID, serials []string) {
	if in.ComponentBatches != nil {
		if b, ok := in.ComponentBatches[item]; ok && b != uuid.Nil {
			bv := b
			batch = &bv
		}
	}
	if in.ComponentSerials != nil {
		serials = in.ComponentSerials[item]
	}
	return batch, serials
}

// ReceiveSubcontractInput carries the received quantity (defaulting to
// the order quantity) and optional lot/serial payload for the finished
// item receipt move.
type ReceiveSubcontractInput struct {
	ActualQty       *decimal.Decimal
	FinishedBatchID *uuid.UUID
	FinishedSerials []string
}

// ReceiveSubcontractOrder transitions an issued order to received,
// emitting a positive receipt move for the finished item valued at the
// supplier's per-unit service charge. Idempotent on retry.
func (s *PGStore) ReceiveSubcontractOrder(ctx context.Context, tenantID, actorID, orderID uuid.UUID, in ReceiveSubcontractInput) (*SubcontractOrder, error) {
	if s.inventory == nil {
		return nil, errors.New("manufacturing: inventory store required to receive subcontract order")
	}
	if tenantID == uuid.Nil || orderID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id and order id required")
	}

	var order *SubcontractOrder
	var receiptQty decimal.Decimal
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		o, err := loadSubcontractOrderTx(ctx, tx, tenantID, orderID, true)
		if err != nil {
			return err
		}
		order = o

		switch o.Status {
		case SubcontractStatusReceived:
			receiptQty = o.ReceivedQty
			return nil
		case SubcontractStatusIssued:
			// Fall through to validate + flip.
		default:
			return fmt.Errorf("%w: %s -> received", ErrSubcontractInvalidTransition, o.Status)
		}

		receiptQty = o.Qty
		if in.ActualQty != nil {
			if in.ActualQty.IsZero() || in.ActualQty.IsNegative() {
				return fmt.Errorf("%w: received qty must be > 0", ErrInvalidInput)
			}
			receiptQty = *in.ActualQty
		}
		if err := validateTrackedMove(ctx, tx, tenantID, o.ItemID, receiptQty, in.FinishedBatchID, in.FinishedSerials); err != nil {
			return err
		}

		receivedAt := s.now()
		if _, err := tx.Exec(ctx,
			`UPDATE subcontract_orders
			    SET status = 'received', received_qty = $1,
			        received_at = COALESCE(received_at, $2), updated_at = now()
			  WHERE tenant_id = $3 AND id = $4`,
			receiptQty, receivedAt, tenantID, orderID,
		); err != nil {
			return fmt.Errorf("manufacturing: flip subcontract order to received: %w", err)
		}
		o.Status = SubcontractStatusReceived
		o.ReceivedQty = receiptQty
		o.ReceivedAt = &receivedAt
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Phase 2: emit the finished-item receipt, valued at the supplier's
	// per-unit service charge so the received stock carries the
	// subcontracting cost.
	unitCost := decimal.Zero
	if order.ChargeAmount.IsPositive() && receiptQty.IsPositive() {
		unitCost = order.ChargeAmount.Div(receiptQty)
	}
	if _, err := s.inventory.RecordMove(ctx, inventory.Move{
		TenantID:    tenantID,
		ItemID:      order.ItemID,
		WarehouseID: order.WarehouseID,
		Qty:         receiptQty,
		UnitCost:    unitCost,
		SourceKType: MoveSourceSubcontractReceipt,
		SourceID:    &order.ID,
		CreatedBy:   actorID,
		BatchID:     in.FinishedBatchID,
		SerialNos:   in.FinishedSerials,
	}); err != nil && !errors.Is(err, inventory.ErrDuplicateSourceMove) {
		return nil, fmt.Errorf("manufacturing: emit subcontract receipt move: %w", err)
	}
	return s.GetSubcontractOrder(ctx, tenantID, orderID)
}

// CloseSubcontractOrder transitions a received order to closed.
func (s *PGStore) CloseSubcontractOrder(ctx context.Context, tenantID, orderID uuid.UUID) (*SubcontractOrder, error) {
	return s.setSubcontractStatus(ctx, tenantID, orderID, SubcontractStatusClosed)
}

// CancelSubcontractOrder transitions a draft order to cancelled.
func (s *PGStore) CancelSubcontractOrder(ctx context.Context, tenantID, orderID uuid.UUID) (*SubcontractOrder, error) {
	return s.setSubcontractStatus(ctx, tenantID, orderID, SubcontractStatusCancelled)
}

// setSubcontractStatus applies a non-stock-moving status transition
// (close / cancel) under the CanTransitionTo matrix.
func (s *PGStore) setSubcontractStatus(ctx context.Context, tenantID, orderID uuid.UUID, target string) (*SubcontractOrder, error) {
	if tenantID == uuid.Nil || orderID == uuid.Nil {
		return nil, errors.New("manufacturing: tenant id and order id required")
	}
	var order *SubcontractOrder
	err := dbutil.WithTenantTx(ctx, s.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		o, err := loadSubcontractOrderTx(ctx, tx, tenantID, orderID, true)
		if err != nil {
			return err
		}
		order = o
		if !o.CanTransitionTo(target) {
			return fmt.Errorf("%w: %s -> %s", ErrSubcontractInvalidTransition, o.Status, target)
		}
		if o.Status == target {
			return nil
		}
		if _, err := tx.Exec(ctx,
			`UPDATE subcontract_orders SET status = $1, updated_at = now()
			  WHERE tenant_id = $2 AND id = $3`,
			target, tenantID, orderID,
		); err != nil {
			return fmt.Errorf("manufacturing: set subcontract order status: %w", err)
		}
		o.Status = target
		return nil
	})
	if err != nil {
		return nil, err
	}
	return order, nil
}

// loadSubcontractOrderTx reads an order header (optionally FOR UPDATE)
// with its components attached.
func loadSubcontractOrderTx(ctx context.Context, tx pgx.Tx, tenantID, orderID uuid.UUID, forUpdate bool) (*SubcontractOrder, error) {
	q := subcontractOrderSelectColumns + ` FROM subcontract_orders WHERE tenant_id = $1 AND id = $2`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	var o SubcontractOrder
	if err := scanSubcontractOrder(tx.QueryRow(ctx, q, tenantID, orderID), &o); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx,
		`SELECT tenant_id, id, subcontract_order_id, item_id, qty, issued_qty, created_at
		   FROM subcontract_components
		  WHERE tenant_id = $1 AND subcontract_order_id = $2
		  ORDER BY created_at, item_id`,
		tenantID, orderID,
	)
	if err != nil {
		return nil, fmt.Errorf("manufacturing: select subcontract components: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c SubcontractComponent
		if err := rows.Scan(&c.TenantID, &c.ID, &c.SubcontractOrderID, &c.ItemID, &c.Qty, &c.IssuedQty, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("manufacturing: scan subcontract component: %w", err)
		}
		o.Components = append(o.Components, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return &o, nil
}

const subcontractOrderSelectColumns = `SELECT tenant_id, id, work_order_id, routing_operation_seq, supplier_id,
        item_id, warehouse_id, qty, received_qty, status, charge_amount, COALESCE(charge_currency, ''),
        issued_at, received_at, COALESCE(notes, ''),
        COALESCE(created_by, '00000000-0000-0000-0000-000000000000'::uuid), created_at, updated_at`

func scanSubcontractOrder(r pgxScanner, o *SubcontractOrder) error {
	if err := r.Scan(
		&o.TenantID, &o.ID, &o.WorkOrderID, &o.RoutingOperationSeq, &o.SupplierID,
		&o.ItemID, &o.WarehouseID, &o.Qty, &o.ReceivedQty, &o.Status, &o.ChargeAmount, &o.ChargeCurrency,
		&o.IssuedAt, &o.ReceivedAt, &o.Notes, &o.CreatedBy, &o.CreatedAt, &o.UpdatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSubcontractOrderNotFound
		}
		return fmt.Errorf("manufacturing: scan subcontract order: %w", err)
	}
	return nil
}

// nullableUUIDPtr returns nil for a nil pointer or the zero UUID so the
// SQL driver writes NULL.
func nullableUUIDPtr(u *uuid.UUID) any {
	if u == nil || *u == uuid.Nil {
		return nil
	}
	return *u
}
