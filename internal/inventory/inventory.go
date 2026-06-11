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
	KTypeSerial     = "inventory.serial"
)

// Serial lifecycle statuses. A serial is born 'in_stock' at the
// warehouse it was received into and ends in one of two terminal
// states: 'consumed' (issued into a work order / written off) or
// 'delivered' (shipped to a customer). Transfers keep the serial
// 'in_stock' and only move its warehouse. The set is pinned by the
// inventory_serials_status_valid CHECK in migration 000089.
const (
	SerialStatusInStock   = "in_stock"
	SerialStatusConsumed  = "consumed"
	SerialStatusDelivered = "delivered"
)

// Move source KTypes emitted by the ledger hook when a sales invoice or
// purchase bill posts. Kept here so the ledger, inventory store, and
// tests all agree on the label.
const (
	MoveSourceSalesInvoice = "finance.ar_invoice"
	MoveSourcePurchaseBill = "finance.ap_bill"
	MoveSourceAdjustment   = "inventory.adjustment"
	MoveSourceTransfer     = "inventory.transfer"
	// MoveSourceSalesReturn labels the positive-qty receipt moves
	// the Phase N9a ReturnPoster appends when a customer return
	// hits the "received" lifecycle state. Kept here so the source
	// label is stable across the inventory/sales packages.
	MoveSourceSalesReturn = "sales.return"
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
	// LotTracked / SerialTracked configure how strictly the move
	// ledger enforces traceability for this item. When LotTracked,
	// every move must reference an inventory_batches row. When
	// SerialTracked, every move must enumerate the exact serial
	// numbers it touches (one per unit of |qty|). Both default false
	// so untracked items behave exactly as before.
	LotTracked    bool `json:"lot_tracked"`
	SerialTracked bool `json:"serial_tracked"`
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
	// SerialNos enumerates the serial numbers this move affects. It is
	// an input-only field (never populated when reading moves back):
	//   * On a receipt (Qty > 0) each serial is created / re-stocked
	//     at WarehouseID and linked to the move.
	//   * On an issue (Qty < 0) each serial must currently be in stock
	//     at (ItemID, WarehouseID); it is transitioned to a terminal
	//     state and linked to the move.
	// len(SerialNos) must equal |Qty| for serial-tracked items.
	SerialNos []string `json:"serial_nos,omitempty"`
	// SerialOutStatus picks the terminal state for serials on an issue
	// move. Defaults to SerialStatusConsumed; sales deliveries pass
	// SerialStatusDelivered. Ignored on receipts.
	SerialOutStatus string `json:"serial_out_status,omitempty"`
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

// Serial is a single serialised unit of a serial-tracked item. One row
// per (tenant_id, item_id, serial_no). WarehouseID is the current
// location while Status == SerialStatusInStock and NULL once the unit
// leaves stock (consumed / delivered). BatchID optionally ties the
// serial to the lot it was produced/received in so a serialised unit
// is also lot-traceable.
type Serial struct {
	TenantID    uuid.UUID  `json:"tenant_id"`
	ID          uuid.UUID  `json:"id"`
	ItemID      uuid.UUID  `json:"item_id"`
	SerialNo    string     `json:"serial_no"`
	Status      string     `json:"status"`
	WarehouseID *uuid.UUID `json:"warehouse_id,omitempty"`
	BatchID     *uuid.UUID `json:"batch_id,omitempty"`
	CreatedBy   uuid.UUID  `json:"created_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// SerialFilter narrows a ListSerials call. All fields are optional.
type SerialFilter struct {
	ItemID      *uuid.UUID
	WarehouseID *uuid.UUID
	BatchID     *uuid.UUID
	Status      string
	Limit       int
	Offset      int
}

// BatchStockLevel is one (item, warehouse, batch) balance read from the
// stock_levels_by_batch projection — the per-lot analogue of
// StockLevel, summed straight from the move ledger.
type BatchStockLevel struct {
	TenantID    uuid.UUID       `json:"tenant_id"`
	ItemID      uuid.UUID       `json:"item_id"`
	WarehouseID uuid.UUID       `json:"warehouse_id"`
	BatchID     uuid.UUID       `json:"batch_id"`
	Qty         decimal.Decimal `json:"qty"`
}

// TraceEvent is one move in a lot's or serial's history, joined to the
// business document that drove it. The slice returned by the trace
// queries is ordered oldest-first so the first inbound row is the
// origin (receipt / production) and the last outbound row is where the
// unit ultimately went (delivery / consumption).
type TraceEvent struct {
	MoveID      int64           `json:"move_id"`
	ItemID      uuid.UUID       `json:"item_id"`
	WarehouseID uuid.UUID       `json:"warehouse_id"`
	Qty         decimal.Decimal `json:"qty"`
	SourceKType string          `json:"source_ktype,omitempty"`
	SourceID    *uuid.UUID      `json:"source_id,omitempty"`
	BatchID     *uuid.UUID      `json:"batch_id,omitempty"`
	MovedAt     time.Time       `json:"moved_at"`
}

// SerialTrace is the forward/backward traceability answer for a single
// serial: the serial's current state plus every move that touched it,
// oldest-first.
type SerialTrace struct {
	Serial Serial       `json:"serial"`
	Events []TraceEvent `json:"events"`
}

// LotTrace is the traceability answer for a single lot: the batch plus
// every move that referenced it, oldest-first. Inbound moves (qty > 0)
// are the origins; outbound moves (qty < 0) are where the lot went.
type LotTrace struct {
	Batch  Batch        `json:"batch"`
	Events []TraceEvent `json:"events"`
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
	ErrBatchInvalid        = errors.New("inventory: invalid batch")
	// ErrInsufficientLotStock is returned when an issue would drive a
	// lot's qty_on_hand below zero. The decrement is rejected before
	// the UPDATE so no partial state escapes.
	ErrInsufficientLotStock = errors.New("inventory: insufficient lot quantity on hand")
	// ErrLotRequired is returned when a lot-tracked item posts a move
	// without a BatchID.
	ErrLotRequired = errors.New("inventory: item is lot-tracked; batch_id required")
	// ErrSerialRequired is returned when a serial-tracked item posts a
	// move without the matching serial numbers.
	ErrSerialRequired = errors.New("inventory: item is serial-tracked; serial numbers required")
	// ErrSerialQtyMismatch is returned when len(SerialNos) does not
	// equal |Qty| on a move.
	ErrSerialQtyMismatch = errors.New("inventory: serial count must equal move quantity")
	// ErrSerialNotAvailable is returned when an issue references a
	// serial that is not currently in stock at the move's warehouse.
	ErrSerialNotAvailable = errors.New("inventory: serial not available in stock at warehouse")
	// ErrSerialAlreadyInStock is returned when a receipt references a
	// serial that is already in stock (a duplicate intake).
	ErrSerialAlreadyInStock = errors.New("inventory: serial already in stock")
	// ErrSerialNotFound is returned when a serial lookup misses.
	ErrSerialNotFound = errors.New("inventory: serial not found")
	// ErrSerialItemMismatch is returned when a serial belongs to a
	// different item than the move references.
	ErrSerialItemMismatch = errors.New("inventory: serial belongs to a different item")
	// ErrSerialUnsupported is returned when serial numbers are supplied
	// on a move whose item is not serial-tracked.
	ErrSerialUnsupported = errors.New("inventory: item is not serial-tracked")
	// ErrDuplicateSerialInput is returned when the same serial number
	// appears more than once in a single move's SerialNos.
	ErrDuplicateSerialInput = errors.New("inventory: duplicate serial number in move")
)
