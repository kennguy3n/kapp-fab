package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/ledger"
	"github.com/kennguy3n/kapp-fab/internal/record"
)

// PosterHook is the adapter that turns ledger.InvoicePoster post-commit
// callbacks into stock moves. One hook instance serves both the sales
// invoice (delivery / negative qty) and the purchase bill (receipt /
// positive qty) flows; the ledger wires each method up via
// InvoicePoster.WithSalesInvoiceHook / WithPurchaseBillHook.
//
// The hook reads `lines` off the source KRecord data and appends one
// stock move per line that carries `item_id` + `warehouse_id` + `qty`.
// Lines without those fields are silently skipped so the hook is safe
// to enable on tenants that have not yet opted into inventory tracking.
//
// Idempotency is guaranteed by the `inventory_moves_source_uniq`
// partial unique index installed in migrations/000005_inventory.sql:
// retries of a posting that previously succeeded produce
// ErrDuplicateSourceMove on the second attempt, which the hook
// translates into a no-op so callers can replay safely.
type PosterHook struct {
	store *PGStore
}

// NewPosterHook builds a hook bound to the given inventory store.
func NewPosterHook(store *PGStore) *PosterHook {
	return &PosterHook{store: store}
}

// invoiceLine is the subset of fields the hook reads from each entry
// in a posted invoice/bill's `lines` array. All fields are optional
// and missing/empty values skip that line so the hook is a no-op for
// non-inventory lines (services, fees, adjustments, …).
type invoiceLine struct {
	ItemID      string          `json:"item_id"`
	WarehouseID string          `json:"warehouse_id"`
	Qty         decimal.Decimal `json:"qty"`
	UnitPrice   decimal.Decimal `json:"unit_price"`
	UnitCost    decimal.Decimal `json:"unit_cost"`
}

type invoiceLines struct {
	Lines []invoiceLine `json:"lines"`
}

// OnSalesInvoicePosted records one negative-qty move (delivery) per
// inventory line on a posted sales invoice. Matches the
// ledger.PostHook signature.
func (h *PosterHook) OnSalesInvoicePosted(
	ctx context.Context, tenantID uuid.UUID,
	rec *record.KRecord, _ *ledger.JournalEntry, actorID uuid.UUID,
) error {
	return h.applyLines(ctx, tenantID, rec, actorID, MoveSourceSalesInvoice, true)
}

// OnPurchaseBillPosted records one positive-qty move (receipt) per
// inventory line on a posted purchase bill. Matches the
// ledger.PostHook signature.
func (h *PosterHook) OnPurchaseBillPosted(
	ctx context.Context, tenantID uuid.UUID,
	rec *record.KRecord, _ *ledger.JournalEntry, actorID uuid.UUID,
) error {
	return h.applyLines(ctx, tenantID, rec, actorID, MoveSourcePurchaseBill, false)
}

// aggregatedLine accumulates invoice lines that share (item_id,
// warehouse_id) so the hook emits a single Move per pair. This keeps
// the inventory_moves_source_uniq partial unique index — which is
// keyed on (tenant_id, source_ktype, source_id, item_id, warehouse_id)
// — aligned 1:1 with what the hook inserts: retries are still
// idempotent, but two lines for the same (item, warehouse) no longer
// collide and lose the second line's qty.
type aggregatedLine struct {
	itemID      uuid.UUID
	warehouseID uuid.UUID
	qty         decimal.Decimal
	// costTimesQty / qtyAbs compute a qty-weighted average unit cost
	// across the contributing lines, falling back to unit_price when
	// a line has no unit_cost.
	costTimesQty decimal.Decimal
	qtyAbs       decimal.Decimal
}

func (h *PosterHook) applyLines(
	ctx context.Context, tenantID uuid.UUID,
	rec *record.KRecord, actorID uuid.UUID,
	sourceKType string, negate bool,
) error {
	if h == nil || h.store == nil || rec == nil {
		return nil
	}
	var parsed invoiceLines
	if len(rec.Data) > 0 {
		if err := json.Unmarshal(rec.Data, &parsed); err != nil {
			return fmt.Errorf("inventory: decode invoice lines: %w", err)
		}
	}
	type pairKey struct {
		item, warehouse uuid.UUID
	}
	agg := map[pairKey]*aggregatedLine{}
	order := make([]pairKey, 0, len(parsed.Lines))
	for i, line := range parsed.Lines {
		if line.ItemID == "" || line.WarehouseID == "" {
			continue
		}
		if line.Qty.IsZero() {
			continue
		}
		itemID, err := uuid.Parse(line.ItemID)
		if err != nil {
			return fmt.Errorf("inventory: line %d: invalid item_id: %w", i, err)
		}
		warehouseID, err := uuid.Parse(line.WarehouseID)
		if err != nil {
			return fmt.Errorf("inventory: line %d: invalid warehouse_id: %w", i, err)
		}
		qty := line.Qty
		if negate {
			qty = qty.Neg()
		}
		unitCost := line.UnitCost
		if unitCost.IsZero() {
			unitCost = line.UnitPrice
		}
		key := pairKey{item: itemID, warehouse: warehouseID}
		cur, ok := agg[key]
		if !ok {
			cur = &aggregatedLine{itemID: itemID, warehouseID: warehouseID}
			agg[key] = cur
			order = append(order, key)
		}
		cur.qty = cur.qty.Add(qty)
		absQty := line.Qty.Abs()
		cur.costTimesQty = cur.costTimesQty.Add(unitCost.Mul(absQty))
		cur.qtyAbs = cur.qtyAbs.Add(absQty)
	}

	sourceID := rec.ID
	for _, key := range order {
		a := agg[key]
		if a.qty.IsZero() {
			continue
		}
		var unitCost decimal.Decimal
		if a.qtyAbs.IsPositive() {
			unitCost = a.costTimesQty.Div(a.qtyAbs)
		}
		_, err := h.store.RecordMove(ctx, Move{
			TenantID:    tenantID,
			ItemID:      a.itemID,
			WarehouseID: a.warehouseID,
			Qty:         a.qty,
			UnitCost:    unitCost,
			SourceKType: sourceKType,
			SourceID:    &sourceID,
			CreatedBy:   actorID,
		})
		if err != nil && !errors.Is(err, ErrDuplicateSourceMove) {
			return fmt.Errorf("inventory: record move for item %s / warehouse %s: %w",
				a.itemID, a.warehouseID, err)
		}
	}
	return nil
}
