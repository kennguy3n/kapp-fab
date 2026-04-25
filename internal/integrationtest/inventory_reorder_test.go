//go:build integration
// +build integration

package integrationtest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/kennguy3n/kapp-fab/internal/crm"
	"github.com/kennguy3n/kapp-fab/internal/inventory"
	"github.com/kennguy3n/kapp-fab/internal/record"
	"github.com/kennguy3n/kapp-fab/internal/sales"
	"github.com/kennguy3n/kapp-fab/internal/scheduler"
	"github.com/kennguy3n/kapp-fab/internal/tenant"
)

// TestReorderAutomation verifies the end-to-end auto-reorder flow.
// One inventory.item is seeded with reorder_level=10 and an initial
// stock of 5 (set by a single inventory_moves row); a draft
// procurement.purchase_order should be created on the first sweep.
// A second sweep should be a no-op thanks to the idempotency
// window guard.
func TestReorderAutomation(t *testing.T) {
	h := newHarness(t)
	tn, supplierID, actorID := newTenantForReorder(t, h)
	ctx := context.Background()

	// Seed an item with reorder_level=10, reorder_qty=20 and a
	// preferred_supplier_id pointing at the crm.organization row
	// the helper just created.
	itemPayload := map[string]any{
		"sku":                   "WIDGET-1",
		"name":                  "Widget",
		"uom":                   "ea",
		"reorder_level":         "10",
		"reorder_qty":           "20",
		"preferred_supplier_id": supplierID.String(),
		"active":                true,
	}
	itemBody, err := json.Marshal(itemPayload)
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	item, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     inventory.KTypeItem,
		Status:    "active",
		Data:      itemBody,
		CreatedBy: actorID,
	})
	if err != nil {
		t.Fatalf("seed item: %v", err)
	}

	// Seed a warehouse so the stock_levels view has somewhere to
	// land the move.
	whBody, _ := json.Marshal(map[string]any{
		"code":   "MAIN",
		"name":   "Main Warehouse",
		"active": true,
	})
	wh, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     inventory.KTypeWarehouse,
		Status:    "active",
		Data:      whBody,
		CreatedBy: actorID,
	})
	if err != nil {
		t.Fatalf("seed warehouse: %v", err)
	}

	// Post a +5 move so stock_levels reports qty=5 < reorder_level=10.
	invStore := inventory.NewPGStore(h.pool, h.publisher, h.auditor)
	if _, err := invStore.RecordMove(ctx, inventory.Move{
		TenantID:    tn.ID,
		ItemID:      item.ID,
		WarehouseID: wh.ID,
		Qty:         decimal.NewFromInt(5),
		MovedAt:     time.Now().UTC(),
		CreatedBy:   actorID,
	}); err != nil {
		t.Fatalf("post stock move: %v", err)
	}

	// Run the reorder handler. We pin the idempotency window
	// short so the second sweep has a clean window if needed
	// later, and pin the clock for determinism.
	now := time.Now().UTC()
	handler := inventory.NewReorderHandler(h.records, invStore).
		WithClock(func() time.Time { return now }).
		WithIdempotencyWindow(time.Hour).
		WithSystemActor(actorID)
	if err := handler.Handle(ctx, tn.ID, scheduler.ScheduledAction{}); err != nil {
		t.Fatalf("first reorder sweep: %v", err)
	}

	// Verify exactly one draft purchase order exists, that it
	// references our supplier + item, and that the qty matches
	// reorder_qty (20).
	drafts := listDraftPOs(t, h, tn.ID)
	if len(drafts) != 1 {
		t.Fatalf("expected 1 draft PO after first sweep, got %d", len(drafts))
	}
	po := drafts[0]
	var poData map[string]any
	if err := json.Unmarshal(po.Data, &poData); err != nil {
		t.Fatalf("unmarshal po: %v", err)
	}
	if got, _ := poData["supplier_id"].(string); got != supplierID.String() {
		t.Fatalf("supplier mismatch: got %q want %q", got, supplierID)
	}
	lines, _ := poData["lines"].([]any)
	if len(lines) != 1 {
		t.Fatalf("expected 1 line on draft PO, got %d", len(lines))
	}
	line, _ := lines[0].(map[string]any)
	if got, _ := line["item_id"].(string); got != item.ID.String() {
		t.Fatalf("line item_id mismatch: got %q want %q", got, item.ID)
	}
	if got, _ := line["qty"].(string); !strings.HasPrefix(got, "20") {
		t.Fatalf("line qty mismatch: got %q want 20", got)
	}

	// Second sweep within the idempotency window must be a
	// no-op — the existing draft already covers this item.
	if err := handler.Handle(ctx, tn.ID, scheduler.ScheduledAction{}); err != nil {
		t.Fatalf("second reorder sweep: %v", err)
	}
	drafts = listDraftPOs(t, h, tn.ID)
	if len(drafts) != 1 {
		t.Fatalf("expected idempotency, got %d drafts after second sweep", len(drafts))
	}
}

// newTenantForReorder bootstraps the harness with the inventory,
// procurement, and crm KTypes registered, returns the tenant and a
// crm.organization row that doubles as the preferred supplier on
// the seeded item, and a synthetic actor uuid for CreatedBy.
func newTenantForReorder(t *testing.T, h *harness) (*tenantRow, uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := inventory.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register inventory ktypes: %v", err)
	}
	if err := sales.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register sales/procurement ktypes: %v", err)
	}
	if err := crm.RegisterKTypes(ctx, h.ktypes); err != nil {
		t.Fatalf("register crm ktypes: %v", err)
	}
	tn, err := h.tenants.Create(ctx, tenant.CreateInput{
		Slug: uniqueSlug("reorder"),
		Name: "Reorder Co",
		Cell: "test",
		Plan: "free",
	})
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	actor := uuid.New()
	supplierBody, _ := json.Marshal(map[string]any{
		"name":     "Acme Supplies",
		"kind":     "supplier",
		"currency": "USD",
	})
	supplier, err := h.records.Create(ctx, record.KRecord{
		TenantID:  tn.ID,
		KType:     crm.KTypeOrganization,
		Status:    "active",
		Data:      supplierBody,
		CreatedBy: actor,
	})
	if err != nil {
		t.Fatalf("seed supplier: %v", err)
	}
	return &tenantRow{ID: tn.ID, Slug: tn.Slug}, supplier.ID, actor
}

// tenantRow is the slim shape this test relies on; full Tenant has
// many more fields but the integration test only needs id+slug.
type tenantRow struct {
	ID   uuid.UUID
	Slug string
}

// listDraftPOs returns every procurement.purchase_order KRecord on
// the tenant in draft status, newest first. Used by the assertions
// to count drafts produced by the reorder sweep.
func listDraftPOs(t *testing.T, h *harness, tenantID uuid.UUID) []record.KRecord {
	t.Helper()
	ctx := context.Background()
	out, err := h.records.List(ctx, tenantID, record.ListFilter{
		KType:  sales.KTypePurchaseOrder,
		Status: "draft",
		Limit:  50,
	})
	if err != nil {
		t.Fatalf("list draft POs: %v", err)
	}
	return out
}
