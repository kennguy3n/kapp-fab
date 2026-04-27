// Package inventory implements Phase D simple inventory: tenant-scoped
// items, warehouses, and an append-only stock-move ledger. Stock levels
// are projected from the move ledger via a SECURITY INVOKER view so the
// RLS on inventory_moves transparently applies.
//
// The model mirrors the Frappe ERPNext Stock Ledger Entry pattern: each
// move is a signed row — positive qty for receipts, negative for
// deliveries — and the current quantity is SUM(qty) GROUP BY
// (tenant_id, item_id, warehouse_id). Moves are never mutated or
// deleted; corrections are expressed as contra-entries.
package inventory

import (
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// KType identifiers. Kept as constants so the API, agent tools, and
// tests all reference the same strings.
const (
	KTypeItem       = "inventory.item"
	KTypeWarehouse  = "inventory.warehouse"
	KTypeMove       = "inventory.move"
	KTypeStockLevel = "inventory.stock_level"
	KTypeBatch      = "inventory.batch"
)

// Move source KTypes emitted by the ledger hook when a sales invoice or
// purchase bill posts. Kept here so the ledger, inventory store, and
// tests all agree on the label.
const (
	MoveSourceSalesInvoice = "finance.ar_invoice"
	MoveSourcePurchaseBill = "finance.ap_bill"
	MoveSourceAdjustment   = "inventory.adjustment"
	MoveSourceTransfer     = "inventory.transfer"
)

// Item is a stock-keeping unit. One row per (tenant_id, sku).
type Item struct {
	TenantID     uuid.UUID       `json:"tenant_id"`
	ID           uuid.UUID       `json:"id"`
	SKU          string          `json:"sku"`
	Name         string          `json:"name"`
	UOM          string          `json:"uom"`
	Active       bool            `json:"active"`
	ReorderLevel decimal.Decimal `json:"reorder_level"`
}

// Warehouse is a physical or logical stocking location. One row per
// (tenant_id, code).
type Warehouse struct {
	TenantID uuid.UUID `json:"tenant_id"`
	ID       uuid.UUID `json:"id"`
	Code     string    `json:"code"`
	Name     string    `json:"name"`
}

// Move is a single signed quantity adjustment on the append-only
// inventory_moves table. Positive `Qty` = goods in (receipt); negative
// `Qty` = goods out (delivery). SourceKType/SourceID link the move
// back to the business record that triggered it (e.g. a posted sales
// invoice) so retries and audits can correlate them.
type Move struct {
	ID          int64           `json:"id,omitempty"`
	TenantID    uuid.UUID       `json:"tenant_id"`
	ItemID      uuid.UUID       `json:"item_id"`
	WarehouseID uuid.UUID       `json:"warehouse_id"`
	Qty         decimal.Decimal `json:"qty"`
	UnitCost    decimal.Decimal `json:"unit_cost"`
	SourceKType string          `json:"source_ktype,omitempty"`
	SourceID    *uuid.UUID      `json:"source_id,omitempty"`
	MovedAt     time.Time       `json:"moved_at"`
	CreatedBy   uuid.UUID       `json:"created_by,omitempty"`
	// ReversalOf, when non-nil, points back to the inventory_moves.id
	// this row was created to cancel. Set by ReverseMove; remains nil
	// for ordinary receipts / deliveries / transfers.
	ReversalOf *int64 `json:"reversal_of,omitempty"`
	// BatchID, when non-nil, ties the move to a specific
	// inventory_batches row. The DB-level composite FK guarantees the
	// batch belongs to the same tenant; PGStore.RecordMove additionally
	// rejects mismatched item ids before the INSERT.
	BatchID *uuid.UUID `json:"batch_id,omitempty"`
}

// Batch is a per-tenant lot identifier for an inventory item. Batches
// are not strictly required — items without a batch context post moves
// with BatchID = nil and the system behaves identically to the
// pre-Phase-G/L flow. Tracking a batch unlocks expiry / FEFO logic and
// per-lot stock visibility on the StockLevels page.
type Batch struct {
	TenantID       uuid.UUID       `json:"tenant_id"`
	ID             uuid.UUID       `json:"id"`
	ItemID         uuid.UUID       `json:"item_id"`
	BatchNo        string          `json:"batch_no"`
	ManufacturedAt *time.Time      `json:"manufactured_at,omitempty"`
	ExpiresAt      *time.Time      `json:"expires_at,omitempty"`
	QtyOnHand      decimal.Decimal `json:"qty_on_hand"`
	Metadata       []byte          `json:"metadata,omitempty"`
	CreatedBy      uuid.UUID       `json:"created_by,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// StockLevel is a single (item, warehouse) quantity read from the
// stock_levels view. Zero quantities are represented as decimal.Zero
// with a populated (ItemID, WarehouseID) so callers can still detect
// explicit "present but empty" locations from genuinely-absent ones.
type StockLevel struct {
	TenantID    uuid.UUID       `json:"tenant_id"`
	ItemID      uuid.UUID       `json:"item_id"`
	WarehouseID uuid.UUID       `json:"warehouse_id"`
	Qty         decimal.Decimal `json:"qty"`
}

// ItemFilter narrows a ListItems call.
type ItemFilter struct {
	Active *bool
	Limit  int
	Offset int
}

// MoveFilter narrows a ListMoves call.
type MoveFilter struct {
	ItemID      *uuid.UUID
	WarehouseID *uuid.UUID
	SourceKType string
	SourceID    *uuid.UUID
	From        *time.Time
	To          *time.Time
	Limit       int
	Offset      int
}

// Transfer captures a same-tenant transfer of stock from one warehouse
// to another. The store records two balanced moves (one negative on
// the source, one positive on the destination) in a single transaction
// so stock levels remain conserved.
type Transfer struct {
	TenantID      uuid.UUID       `json:"tenant_id"`
	ItemID        uuid.UUID       `json:"item_id"`
	FromWarehouse uuid.UUID       `json:"from_warehouse_id"`
	ToWarehouse   uuid.UUID       `json:"to_warehouse_id"`
	Qty           decimal.Decimal `json:"qty"`
	UnitCost      decimal.Decimal `json:"unit_cost,omitempty"`
	MovedAt       time.Time       `json:"moved_at,omitempty"`
	CreatedBy     uuid.UUID       `json:"created_by"`
	Memo          string          `json:"memo,omitempty"`
}

// Sentinel errors the API layer translates into 4xx.
var (
	ErrItemNotFound        = errors.New("inventory: item not found")
	ErrWarehouseNotFound   = errors.New("inventory: warehouse not found")
	ErrMoveInvalid         = errors.New("inventory: invalid stock move")
	ErrTransferUnbalanced  = errors.New("inventory: transfer warehouses must differ and qty > 0")
	ErrDuplicateSourceMove = errors.New("inventory: stock move already recorded for source record")
	ErrMoveNotFound        = errors.New("inventory: stock move not found")
	ErrAlreadyReversed     = errors.New("inventory: stock move already reversed")
	ErrCannotReverseContra = errors.New("inventory: cannot reverse a contra-entry directly; reverse the original instead")
	ErrBatchNotFound       = errors.New("inventory: batch not found")
	ErrBatchItemMismatch   = errors.New("inventory: batch belongs to a different item")
	ErrDuplicateBatch      = errors.New("inventory: batch number already exists for this item")
)
