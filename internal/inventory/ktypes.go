package inventory

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// itemSchema — SKU master. `sku` is the per-tenant unique identifier;
// `uom` is a free-form unit-of-measure label (e.g. "ea", "kg", "m").
var itemSchema = []byte(`{
  "name": "inventory.item",
  "version": 1,
  "fields": [
    {"name": "sku", "type": "string", "required": true, "max_length": 64},
    {"name": "name", "type": "string", "required": true, "max_length": 200},
    {"name": "uom", "type": "string", "required": true, "max_length": 32},
    {"name": "active", "type": "boolean", "default": true},
    {"name": "reorder_level", "type": "number", "min": 0},
    {"name": "reorder_qty", "type": "number", "min": 0},
    {"name": "preferred_supplier_id", "type": "ref", "ktype": "crm.organization"}
  ],
  "views": {
    "list": {"columns": ["sku", "name", "uom", "reorder_level", "active"]},
    "form": {"sections": [{"title": "Item", "fields": ["sku", "name", "uom", "reorder_level", "reorder_qty", "preferred_supplier_id", "active"]}]}
  },
  "cards": {"summary": "{{sku}} — {{name}} ({{uom}})"},
  "permissions": {"read": ["tenant.member"], "write": ["inventory.admin", "tenant.admin"]}
}`)

// warehouseSchema — stocking location. `code` is the per-tenant
// identifier (e.g. "MAIN", "WH-02").
var warehouseSchema = []byte(`{
  "name": "inventory.warehouse",
  "version": 1,
  "fields": [
    {"name": "code", "type": "string", "required": true, "max_length": 32},
    {"name": "name", "type": "string", "required": true, "max_length": 200}
  ],
  "views": {
    "list": {"columns": ["code", "name"]},
    "form": {"sections": [{"title": "Warehouse", "fields": ["code", "name"]}]}
  },
  "cards": {"summary": "{{code}} — {{name}}"},
  "permissions": {"read": ["tenant.member"], "write": ["inventory.admin", "tenant.admin"]}
}`)

// moveSchema — one stock move. Rows are append-only; corrections are
// expressed as contra-entries. Positive `qty` = receipt, negative =
// delivery. `source_ktype`/`source_id` link the move to the business
// record that triggered it (e.g. a posted sales invoice).
var moveSchema = []byte(`{
  "name": "inventory.move",
  "version": 1,
  "fields": [
    {"name": "item_id", "type": "ref", "ktype": "inventory.item", "required": true},
    {"name": "warehouse_id", "type": "ref", "ktype": "inventory.warehouse", "required": true},
    {"name": "qty", "type": "number", "required": true},
    {"name": "unit_cost", "type": "number", "min": 0},
    {"name": "source_ktype", "type": "string", "max_length": 64},
    {"name": "source_id", "type": "string"},
    {"name": "batch_id", "type": "ref", "ktype": "inventory.batch"},
    {"name": "moved_at", "type": "datetime"}
  ],
  "views": {
    "list": {"columns": ["moved_at", "item_id", "warehouse_id", "qty", "unit_cost", "source_ktype"]},
    "form": {"sections": [
      {"title": "Move", "fields": ["item_id", "warehouse_id", "qty", "unit_cost", "moved_at"]},
      {"title": "Source", "fields": ["source_ktype", "source_id"]}
    ]}
  },
  "cards": {"summary": "Move {{qty}} {{item_id}} @ {{warehouse_id}}"},
  "permissions": {"read": ["tenant.member"], "write": ["inventory.admin", "tenant.admin"]},
  "agent_tools": ["inventory.record_move"]
}`)

// stockLevelSchema — derived snapshot read from the stock_levels view.
// No writes are possible (enforced by `permissions.write = []`); the UI
// and agent tools treat this KType as read-only.
var stockLevelSchema = []byte(`{
  "name": "inventory.stock_level",
  "version": 1,
  "fields": [
    {"name": "item_id", "type": "ref", "ktype": "inventory.item"},
    {"name": "warehouse_id", "type": "ref", "ktype": "inventory.warehouse"},
    {"name": "qty", "type": "number"}
  ],
  "views": {
    "list": {"columns": ["item_id", "warehouse_id", "qty"]}
  },
  "cards": {"summary": "{{item_id}} @ {{warehouse_id}}: {{qty}}"},
  "permissions": {"read": ["tenant.member"], "write": []},
  "agent_tools": ["inventory.check_stock"]
}`)

// batchSchema — per-tenant lot identifier. Tracking a batch unlocks
// expiry / FEFO logic and per-lot stock visibility. Items without a
// batch context continue to post moves with batch_id=NULL.
var batchSchema = []byte(`{
  "name": "inventory.batch",
  "version": 1,
  "fields": [
    {"name": "item_id", "type": "ref", "ktype": "inventory.item", "required": true},
    {"name": "batch_no", "type": "string", "required": true, "max_length": 64},
    {"name": "manufactured_at", "type": "date"},
    {"name": "expires_at", "type": "date"},
    {"name": "qty_on_hand", "type": "number", "min": 0, "default": 0}
  ],
  "views": {
    "list": {"columns": ["item_id", "batch_no", "manufactured_at", "expires_at", "qty_on_hand"]},
    "form": {"sections": [{"title": "Batch", "fields": ["item_id", "batch_no", "manufactured_at", "expires_at", "qty_on_hand"]}]}
  },
  "cards": {"summary": "Batch {{batch_no}} of {{item_id}} (qty {{qty_on_hand}})"},
  "permissions": {"read": ["tenant.member"], "write": ["inventory.admin", "tenant.admin"]},
  "agent_tools": ["inventory.assign_batch"]
}`)

// All returns every Phase D inventory KType as a freshly-constructed slice.
func All() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeItem, Version: 1, Schema: itemSchema},
		{Name: KTypeWarehouse, Version: 1, Schema: warehouseSchema},
		{Name: KTypeMove, Version: 1, Schema: moveSchema},
		{Name: KTypeStockLevel, Version: 1, Schema: stockLevelSchema},
		{Name: KTypeBatch, Version: 1, Schema: batchSchema},
	}
}

func init() {
	for _, kt := range All() {
		if !json.Valid(kt.Schema) {
			panic(fmt.Sprintf("inventory: embedded schema %q is not valid JSON", kt.Name))
		}
	}
}

// RegisterKTypes registers every Phase D inventory KType against the
// supplied registry. Idempotent: the underlying PGRegistry upserts on
// conflict.
func RegisterKTypes(ctx context.Context, registry ktype.Registry) error {
	for _, kt := range All() {
		if err := registry.Register(ctx, kt); err != nil {
			return fmt.Errorf("inventory: register %s: %w", kt.Name, err)
		}
	}
	return nil
}
