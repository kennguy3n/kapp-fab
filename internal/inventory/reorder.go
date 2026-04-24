package inventory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/dbutil"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
)

// ActionTypeReorder is the scheduled_actions.action_type the reorder
// automation registers under. The tenant wizard seeds one row of this
// type per tenant with an hourly cadence.
const ActionTypeReorder = "inventory_reorder"

// DefaultReorderIdempotencyWindow is the lookback the handler uses
// when checking whether a draft PO for the same supplier already
// exists before creating a new one. Prevents a stale reorder-level
// causing duplicate drafts across consecutive sweeps.
const DefaultReorderIdempotencyWindow = 24 * time.Hour

// reorderCandidate captures one low-stock row to feed into the
// per-supplier PO batch. Held in memory per sweep; never persisted.
type reorderCandidate struct {
	ItemID       uuid.UUID
	SKU          string
	Name         string
	UOM          string
	CurrentQty   decimal.Decimal
	ReorderLevel decimal.Decimal
	ReorderQty   decimal.Decimal
	SupplierID   *uuid.UUID
}

// ReorderHandler is the scheduler.ActionHandler that sweeps the
// tenant's inventory.item KRecords whose aggregated stock level is
// below their reorder_level, groups the resulting candidates by
// preferred_supplier_id, and creates one draft procurement.
// purchase_order per supplier. A per-sweep idempotency window guards
// against duplicate drafts when consecutive sweeps find the same
// shortfall.
//
// The handler is stateless — every call re-reads stock_levels and
// krecords — so a missed sweep is fully recovered by the next one.
// Failures per supplier group are log-and-continue so one bad row
// (e.g. a supplier without a valid crm.organization ref) does not
// stall the other groups.
type ReorderHandler struct {
	records           *record.PGStore
	store             *PGStore
	now               func() time.Time
	idempotencyWindow time.Duration
	systemActor       uuid.UUID
}

// NewReorderHandler wires a handler against the shared record store
// and inventory store. Both must be non-nil; the inventory store
// gives the handler access to the tenant-scoped pool (for the
// stock-level + supplier lookup) and the record store is where the
// draft PO is persisted.
func NewReorderHandler(records *record.PGStore, store *PGStore) *ReorderHandler {
	return &ReorderHandler{
		records:           records,
		store:             store,
		now:               func() time.Time { return time.Now().UTC() },
		idempotencyWindow: DefaultReorderIdempotencyWindow,
		systemActor:       uuid.Nil,
	}
}

// WithClock pins the clock for deterministic tests.
func (h *ReorderHandler) WithClock(now func() time.Time) *ReorderHandler {
	if now != nil {
		h.now = now
	}
	return h
}

// WithIdempotencyWindow overrides the default 24h lookback. Tests
// use a short window so they don't need to manipulate the clock.
func (h *ReorderHandler) WithIdempotencyWindow(d time.Duration) *ReorderHandler {
	if d > 0 {
		h.idempotencyWindow = d
	}
	return h
}

// WithSystemActor stamps generated PO drafts with the supplied
// actor UUID. Matches the recurring-invoice engine so audit trails
// attribute generated rows to the same synthetic system user.
func (h *ReorderHandler) WithSystemActor(actor uuid.UUID) *ReorderHandler {
	if actor != uuid.Nil {
		h.systemActor = actor
	}
	return h
}

// Handle implements scheduler.ActionHandler. Called once per due
// scheduled_actions row; the scheduler has already advanced
// next_run_at by the time this executes so a slow handler does not
// prevent other tenants' rows from being picked up.
func (h *ReorderHandler) Handle(ctx context.Context, tenantID uuid.UUID, _ scheduler.ScheduledAction) error {
	if h == nil || h.records == nil || h.store == nil {
		return errors.New("inventory: reorder handler not wired")
	}
	candidates, err := h.findCandidates(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("inventory: find reorder candidates: %w", err)
	}
	if len(candidates) == 0 {
		return nil
	}
	groups := groupBySupplier(candidates)
	for supplier, items := range groups {
		if err := h.createDraftPO(ctx, tenantID, supplier, items); err != nil {
			log.Printf("inventory: reorder tenant=%s supplier=%v: %v",
				tenantID, supplier, err)
			continue
		}
	}
	return nil
}

// findCandidates scans the tenant's stock_levels joined against
// inventory.item krecords, summing per-item quantities across
// warehouses and returning every item whose aggregate stock has
// dropped below its reorder_level. preferred_supplier_id is pulled
// from the item's data JSON.
func (h *ReorderHandler) findCandidates(ctx context.Context, tenantID uuid.UUID) ([]reorderCandidate, error) {
	out := []reorderCandidate{}
	err := dbutil.WithTenantTx(ctx, h.store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Sum stock_levels per item then join the inventory.item
		// KRecord to pick up SKU, name, uom, reorder_level,
		// reorder_qty, and preferred_supplier_id. reorder_level
		// must be present and positive for the row to count;
		// items without one are intentionally excluded so
		// operators can opt individual SKUs in or out.
		rows, err := tx.Query(ctx, `
			WITH totals AS (
				SELECT item_id, SUM(qty) AS qty
				FROM stock_levels
				WHERE tenant_id = $1
				GROUP BY item_id
			)
			SELECT k.id,
			       k.data->>'sku',
			       k.data->>'name',
			       k.data->>'uom',
			       COALESCE(t.qty, 0),
			       NULLIF(k.data->>'reorder_level', '')::numeric,
			       NULLIF(k.data->>'reorder_qty', '')::numeric,
			       k.data->>'preferred_supplier_id'
			FROM krecords k
			LEFT JOIN totals t ON t.item_id = k.id
			WHERE k.tenant_id = $1
			  AND k.ktype = $2
			  AND k.deleted_at IS NULL
			  AND k.status = 'active'
			  AND NULLIF(k.data->>'reorder_level', '') IS NOT NULL
			  AND (k.data->>'active' IS NULL OR (k.data->>'active')::boolean = TRUE)
			  AND COALESCE(t.qty, 0) < NULLIF(k.data->>'reorder_level', '')::numeric`,
			tenantID, KTypeItem,
		)
		if err != nil {
			return fmt.Errorf("query reorder candidates: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				c            reorderCandidate
				sku, name    *string
				uom          *string
				reorderQty   decimal.NullDecimal
				reorderLevel decimal.Decimal
				supplier     *string
			)
			if err := rows.Scan(&c.ItemID, &sku, &name, &uom, &c.CurrentQty, &reorderLevel, &reorderQty, &supplier); err != nil {
				return fmt.Errorf("scan reorder candidate: %w", err)
			}
			if sku != nil {
				c.SKU = *sku
			}
			if name != nil {
				c.Name = *name
			}
			if uom != nil {
				c.UOM = *uom
			}
			c.ReorderLevel = reorderLevel
			if reorderQty.Valid {
				c.ReorderQty = reorderQty.Decimal
			} else {
				// Default to the reorder_level so a missing
				// explicit reorder_qty still produces a
				// non-zero PO line.
				c.ReorderQty = reorderLevel
			}
			if supplier != nil && *supplier != "" {
				id, err := uuid.Parse(*supplier)
				if err == nil {
					c.SupplierID = &id
				}
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// supplierKey is the map key used to bucket candidates by supplier.
// A nil-supplier bucket is represented by the zero UUID; callers
// detect it by checking whether the supplier slice entry sets
// SupplierID to nil.
type supplierKey struct {
	id uuid.UUID
}

func groupBySupplier(candidates []reorderCandidate) map[supplierKey][]reorderCandidate {
	out := map[supplierKey][]reorderCandidate{}
	for _, c := range candidates {
		var k supplierKey
		if c.SupplierID != nil {
			k.id = *c.SupplierID
		}
		out[k] = append(out[k], c)
	}
	return out
}

// createDraftPO materialises one draft procurement.purchase_order
// KRecord for the supplied supplier + item group. Idempotency: if
// a draft PO for the same (tenant, supplier) already exists with
// any of the candidate items in its lines array and was created
// within the idempotency window, the handler skips the write so
// consecutive sweeps do not duplicate drafts.
func (h *ReorderHandler) createDraftPO(
	ctx context.Context,
	tenantID uuid.UUID,
	supplier supplierKey,
	items []reorderCandidate,
) error {
	if len(items) == 0 {
		return nil
	}
	itemIDs := make([]string, 0, len(items))
	for _, it := range items {
		itemIDs = append(itemIDs, it.ItemID.String())
	}
	windowStart := h.now().Add(-h.idempotencyWindow)
	exists, err := h.existingDraft(ctx, tenantID, supplier, itemIDs, windowStart)
	if err != nil {
		return fmt.Errorf("check existing draft: %w", err)
	}
	if exists {
		return nil
	}
	lines := make([]map[string]any, 0, len(items))
	for _, it := range items {
		line := map[string]any{
			"item_id": it.ItemID.String(),
			"sku":     it.SKU,
			"name":    it.Name,
			"uom":     it.UOM,
			"qty":     decimalFloat(it.ReorderQty),
			"memo":    fmt.Sprintf("Reorder: current %s < threshold %s", it.CurrentQty, it.ReorderLevel),
		}
		lines = append(lines, line)
	}
	data := map[string]any{
		"po_number":       fmt.Sprintf("AUTO-%s", h.now().Format("20060102T150405")),
		"order_date":      h.now().Format("2006-01-02"),
		"lines":           lines,
		"status":          "draft",
		"currency":        "USD",
		"auto_reorder":    true,
		"auto_reorder_at": h.now().Format(time.RFC3339),
	}
	if supplier.id != uuid.Nil {
		data["supplier_id"] = supplier.id.String()
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode draft PO: %w", err)
	}
	actor := h.systemActor
	if actor == uuid.Nil {
		// Use the deterministic worker system actor so audit
		// trails attribute to a stable synthetic user. Caller can
		// still override via WithSystemActor.
		actor = uuid.MustParse("00000000-0000-0000-0000-000000000003")
	}
	_, err = h.records.Create(ctx, record.KRecord{
		TenantID:  tenantID,
		KType:     "procurement.purchase_order",
		Data:      payload,
		CreatedBy: actor,
	})
	if err != nil {
		return fmt.Errorf("create draft PO: %w", err)
	}
	return nil
}

// existingDraft returns true when a draft PO for the same supplier
// already references any of the candidate items and was created
// inside the idempotency window. Uses a JSONB containment query so
// the check stays O(1) DB round-trips regardless of item count.
func (h *ReorderHandler) existingDraft(
	ctx context.Context,
	tenantID uuid.UUID,
	supplier supplierKey,
	itemIDs []string,
	since time.Time,
) (bool, error) {
	if len(itemIDs) == 0 {
		return false, nil
	}
	found := false
	err := dbutil.WithTenantTx(ctx, h.store.pool, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Build a small JSONB array of {"item_id": "..."} probes
		// and ask Postgres if ANY of them is contained in the
		// lines array of a recent draft PO from the same
		// supplier. The `@>` operator treats an array on the left
		// as a superset check; wrapping the candidates in a
		// one-element jsonb probe per pass stays inside the
		// JSONB operator surface.
		for _, id := range itemIDs {
			probe := fmt.Sprintf(`{"lines":[{"item_id":%q}]}`, id)
			var supplierExpr string
			args := []any{tenantID, since, probe}
			if supplier.id == uuid.Nil {
				supplierExpr = "(data->>'supplier_id' IS NULL OR data->>'supplier_id' = '')"
			} else {
				supplierExpr = "data->>'supplier_id' = $4"
				args = append(args, supplier.id.String())
			}
			q := fmt.Sprintf(`
				SELECT 1 FROM krecords
				WHERE tenant_id = $1
				  AND ktype = 'procurement.purchase_order'
				  AND deleted_at IS NULL
				  AND data->>'status' = 'draft'
				  AND created_at >= $2
				  AND data @> $3::jsonb
				  AND %s
				LIMIT 1`, supplierExpr)
			var x int
			err := tx.QueryRow(ctx, q, args...).Scan(&x)
			if err == nil {
				found = true
				return nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

// decimalFloat converts a shopspring decimal to the float64 the
// JSON KType layer expects for `number` fields. Matches
// hr/payroll_engine.go::decimalFloat so precision loss is treated
// the same way across the platform.
func decimalFloat(d decimal.Decimal) float64 {
	f, _ := d.Float64()
	return f
}
