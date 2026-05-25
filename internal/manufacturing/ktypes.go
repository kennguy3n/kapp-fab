package manufacturing

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kennguy3n/kapp-fab/internal/ktype"
)

// bomSchema — Bill of Materials master. The "components" field is
// stored on `bom_components` (one row per component), not on the
// KRecord — the JSON column on the BOM krecord (if any tenant
// chooses to surface it through generic record views) holds only
// the header fields. The KType is registered primarily so list /
// form views and agent tools can reference the type by name; the
// authoritative store is internal/manufacturing/store.go against
// the `boms` table.
var bomSchema = []byte(`{
  "name": "manufacturing.bom",
  "version": 1,
  "fields": [
    {"name": "item_id", "type": "ref", "ktype": "inventory.item", "required": true},
    {"name": "version", "type": "string", "required": true, "max_length": 32},
    {"name": "status", "type": "enum", "values": ["draft", "active", "obsolete"], "default": "draft"},
    {"name": "output_qty", "type": "number", "default": 1, "min": 0},
    {"name": "uom", "type": "string", "max_length": 32, "default": "ea"},
    {"name": "notes", "type": "text"}
  ],
  "views": {
    "list": {"columns": ["item_id", "version", "status", "output_qty", "uom"]},
    "form": {"sections": [
      {"title": "Header", "fields": ["item_id", "version", "status", "output_qty", "uom"]},
      {"title": "Notes", "fields": ["notes"]}
    ]}
  },
  "cards": {"summary": "BOM {{version}} for {{item_id}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["manufacturing.admin", "tenant.admin"]},
  "agent_tools": ["manufacturing.create_bom", "manufacturing.activate_bom"]
}`)

// workOrderSchema — A single production run. The lifecycle (status)
// is the load-bearing field; the engine in work_order.go enforces
// the legal transitions and emits inventory moves on completion.
var workOrderSchema = []byte(`{
  "name": "manufacturing.work_order",
  "version": 1,
  "fields": [
    {"name": "item_id", "type": "ref", "ktype": "inventory.item", "required": true},
    {"name": "bom_id", "type": "ref", "ktype": "manufacturing.bom"},
    {"name": "warehouse_id", "type": "ref", "ktype": "inventory.warehouse", "required": true},
    {"name": "planned_qty", "type": "number", "required": true, "min": 0},
    {"name": "actual_qty", "type": "number", "min": 0},
    {"name": "status", "type": "enum",
     "values": ["draft", "released", "in_progress", "completed", "closed", "cancelled"],
     "default": "draft"},
    {"name": "scheduled_start", "type": "datetime"},
    {"name": "scheduled_end", "type": "datetime"},
    {"name": "started_at", "type": "datetime"},
    {"name": "completed_at", "type": "datetime"},
    {"name": "notes", "type": "text"}
  ],
  "views": {
    "list": {"columns": ["item_id", "status", "planned_qty", "actual_qty", "warehouse_id", "scheduled_start"]},
    "form": {"sections": [
      {"title": "Order", "fields": ["item_id", "bom_id", "warehouse_id", "planned_qty", "status"]},
      {"title": "Schedule", "fields": ["scheduled_start", "scheduled_end", "started_at", "completed_at"]},
      {"title": "Yield", "fields": ["actual_qty", "notes"]}
    ]}
  },
  "cards": {"summary": "WO {{item_id}} x{{planned_qty}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["manufacturing.admin", "tenant.admin"]},
  "agent_tools": ["manufacturing.create_work_order", "manufacturing.complete_work_order"]
}`)

// All returns every Phase N6 manufacturing KType as a freshly-
// constructed slice. Order matches the registration order in
// RegisterKTypes.
func All() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeBOM, Version: 1, Schema: bomSchema},
		{Name: KTypeWorkOrder, Version: 1, Schema: workOrderSchema},
	}
}

func init() {
	for _, kt := range All() {
		if !json.Valid(kt.Schema) {
			panic(fmt.Sprintf("manufacturing: embedded schema %q is not valid JSON", kt.Name))
		}
	}
}

// RegisterKTypes registers every Phase N6 manufacturing KType
// against the supplied registry. Idempotent: the underlying
// PGRegistry upserts on content-hash mismatch.
func RegisterKTypes(ctx context.Context, registry ktype.Registry) error {
	for _, kt := range All() {
		if err := registry.RegisterIfChanged(ctx, kt); err != nil {
			return fmt.Errorf("manufacturing: register %s: %w", kt.Name, err)
		}
	}
	return nil
}
