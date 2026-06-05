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

// workCenterSchema — A machine or workstation with a finite hourly
// capacity. The authoritative store is the `work_centers` table; the
// KType is registered so generic record views and agent tools can
// reference the type by name. status is active/maintenance/retired —
// only active centers contribute available minutes to the capacity
// grid (see capacity.go).
var workCenterSchema = []byte(`{
  "name": "manufacturing.work_center",
  "version": 1,
  "fields": [
    {"name": "name", "type": "string", "required": true, "max_length": 128},
    {"name": "capacity_per_hour", "type": "number", "default": 0, "min": 0},
    {"name": "operating_hours_per_day", "type": "number", "default": 8, "min": 0, "max": 24},
    {"name": "efficiency_percent", "type": "number", "default": 100, "min": 0},
    {"name": "status", "type": "enum", "values": ["active", "maintenance", "retired"], "default": "active"},
    {"name": "notes", "type": "text"}
  ],
  "views": {
    "list": {"columns": ["name", "status", "capacity_per_hour", "operating_hours_per_day", "efficiency_percent"]},
    "form": {"sections": [
      {"title": "Work Center", "fields": ["name", "status"]},
      {"title": "Capacity", "fields": ["capacity_per_hour", "operating_hours_per_day", "efficiency_percent"]},
      {"title": "Notes", "fields": ["notes"]}
    ]}
  },
  "cards": {"summary": "{{name}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["manufacturing.admin", "tenant.admin"]},
  "agent_tools": ["manufacturing.create_work_center"]
}`)

// routingSchema — A versioned, ordered sequence of operations for
// producing an item. The operations live on the `routing_operations`
// table (one row per step); the "operations" array field documents the
// nested shape for generic form rendering and agent-tool payloads. The
// lifecycle (draft → active → obsolete) mirrors the BOM; only one
// active routing per item is snapshotted onto a work order at release.
var routingSchema = []byte(`{
  "name": "manufacturing.routing",
  "version": 1,
  "fields": [
    {"name": "item_id", "type": "ref", "ktype": "inventory.item", "required": true},
    {"name": "version", "type": "string", "required": true, "max_length": 32},
    {"name": "status", "type": "enum", "values": ["draft", "active", "obsolete"], "default": "draft"},
    {"name": "operations", "type": "array", "item_type": "object", "item_fields": [
      {"name": "sequence", "type": "integer", "min": 1},
      {"name": "operation_name", "type": "string", "required": true, "max_length": 128},
      {"name": "work_center_id", "type": "ref", "ktype": "manufacturing.work_center", "required": true},
      {"name": "setup_time_minutes", "type": "number", "default": 0, "min": 0},
      {"name": "cycle_time_minutes", "type": "number", "default": 0, "min": 0},
      {"name": "description", "type": "text"}
    ]},
    {"name": "notes", "type": "text"}
  ],
  "views": {
    "list": {"columns": ["item_id", "version", "status"]},
    "form": {"sections": [
      {"title": "Header", "fields": ["item_id", "version", "status"]},
      {"title": "Operations", "fields": ["operations"]},
      {"title": "Notes", "fields": ["notes"]}
    ]}
  },
  "cards": {"summary": "Routing {{version}} for {{item_id}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["manufacturing.admin", "tenant.admin"]},
  "agent_tools": ["manufacturing.create_routing", "manufacturing.activate_routing"]
}`)

// jobCardSchema — Shop-floor execution record, one per routing
// operation per work order. Created automatically when a work order is
// released; status walks pending → in_progress → completed. The
// authoritative store is the `job_cards` table.
var jobCardSchema = []byte(`{
  "name": "manufacturing.job_card",
  "version": 1,
  "fields": [
    {"name": "work_order_id", "type": "ref", "ktype": "manufacturing.work_order", "required": true},
    {"name": "routing_operation_seq", "type": "integer", "required": true, "min": 1},
    {"name": "work_center_id", "type": "ref", "ktype": "manufacturing.work_center", "required": true},
    {"name": "status", "type": "enum", "values": ["pending", "in_progress", "completed"], "default": "pending"},
    {"name": "planned_start", "type": "datetime"},
    {"name": "planned_end", "type": "datetime"},
    {"name": "actual_start", "type": "datetime"},
    {"name": "actual_end", "type": "datetime"},
    {"name": "operator_id", "type": "ref", "ktype": "tenant.user"},
    {"name": "qty_produced", "type": "number", "default": 0, "min": 0},
    {"name": "qty_rejected", "type": "number", "default": 0, "min": 0},
    {"name": "notes", "type": "text"}
  ],
  "views": {
    "list": {"columns": ["work_order_id", "routing_operation_seq", "work_center_id", "status", "qty_produced"]},
    "form": {"sections": [
      {"title": "Card", "fields": ["work_order_id", "routing_operation_seq", "work_center_id", "status"]},
      {"title": "Schedule", "fields": ["planned_start", "planned_end", "actual_start", "actual_end"]},
      {"title": "Yield", "fields": ["operator_id", "qty_produced", "qty_rejected", "notes"]}
    ]}
  },
  "cards": {"summary": "Job card op {{routing_operation_seq}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["manufacturing.operator", "manufacturing.admin", "tenant.admin"]},
  "agent_tools": ["manufacturing.start_job_card", "manufacturing.complete_job_card"]
}`)

// All returns every manufacturing KType as a freshly-constructed
// slice. Order matches the registration order in RegisterKTypes —
// work_center precedes routing because a routing operation references a
// work center, and routing precedes job_card for the same reason.
func All() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeBOM, Version: 1, Schema: bomSchema},
		{Name: KTypeWorkOrder, Version: 1, Schema: workOrderSchema},
		{Name: KTypeWorkCenter, Version: 1, Schema: workCenterSchema},
		{Name: KTypeRouting, Version: 1, Schema: routingSchema},
		{Name: KTypeJobCard, Version: 1, Schema: jobCardSchema},
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
