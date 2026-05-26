// requisition.go — Phase N9b: Purchase Requisitions.
//
// A `procurement.purchase_requisition` KRecord represents an
// internal request to purchase goods (typically raised by an
// operations or department head) that must be approved before a
// `procurement.purchase_order` is opened against a supplier. The
// KType orchestrates a small state machine on top of primitives
// that already exist on the platform:
//
//   - record.PGStore.Patch (status flips, approval stamps,
//     po_id stamp) drives the lifecycle without bypassing
//     KRecord workflow / permission semantics.
//
//   - record.PGStore.Create (procurement.purchase_order) is the
//     escape hatch into the PO surface — Approve does not call it;
//     the explicit Convert step does. The conversion stamps the
//     requisition's `po_id` immediately after Create so a retried
//     Convert reuses the prior PO rather than spawning a duplicate
//     procurement record.
//
// The Requisition therefore owns a state machine (requested →
// approved → ordered → cancelled) where Approve is a pure status
// flip and Convert performs exactly one Create against the PO
// KType, then a Patch on the requisition stamping the linkage.
// Cancellation is permitted from any pre-ordered state and is a
// pure status flip with no posting side-effects.

package sales

import "github.com/kennguy3n/kapp-fab/internal/ktype"

// KType identifier and workflow constants for purchase
// requisitions. Wire-stable strings — agent tools, KChat commands,
// and database `kind` columns all hard-code these.
const (
	// KTypePurchaseRequisition is the public KType name for an
	// internal purchase request. Lives under the procurement.*
	// namespace alongside procurement.purchase_order so dashboards
	// and reports can roll the two up.
	KTypePurchaseRequisition = "procurement.purchase_requisition"

	// WorkflowPurchaseRequisition is the workflow id registered on
	// the KType. Mirrors the platform convention `<ktype>.lifecycle`.
	WorkflowPurchaseRequisition = "procurement.purchase_requisition.lifecycle"
)

// Purchase requisition lifecycle states. Forward-only except for
// cancellation, which is permitted from anywhere pre-ordered and is
// a pure status flip (no posting side-effects).
const (
	RequisitionStatusRequested = "requested"
	RequisitionStatusApproved  = "approved"
	RequisitionStatusOrdered   = "ordered"
	RequisitionStatusCancelled = "cancelled"
)

// purchaseRequisitionSchema — internal request to purchase. Mirrors
// ERPNext's Material Request DocType (purchase type) but stays
// scoped to the supplier-facing purchase flow; stock-transfer and
// production material requests are out of scope for the N9b cut and
// can be added later behind separate `purpose` enum values without
// migrating the existing rows.
//
// Field notes worth calling out:
//
//   - `requested_by` is required because approval routing reads
//     the requesting user's department / cost-centre. Without it
//     the approver has no audit trail of who raised the request.
//   - `supplier_id` is optional at the requested stage — many
//     organisations let department heads raise a requisition for a
//     part with no preferred supplier yet, and procurement picks
//     the vendor at PO time. It becomes required at the conversion
//     step (the PO needs a supplier).
//   - `lines[]` carries {item_id, qty, estimated_unit_price,
//     line_total}; the conversion step transposes these into the
//     PO's `lines[]` 1:1.
//   - `po_id` is stamped by the poster when Convert runs; not
//     user-editable, exists on the schema only so the UI can show
//     a drill-down link to the resulting PO.
//   - `approved_by` / `approved_at` are stamped on the Approve
//     transition; same drill-down rationale.
var purchaseRequisitionSchema = []byte(`{
  "name": "procurement.purchase_requisition",
  "version": 1,
  "fields": [
    {"name": "requisition_number", "type": "string", "max_length": 64},
    {"name": "requested_by", "type": "ref", "ktype": "user", "required": true},
    {"name": "department", "type": "string", "max_length": 64},
    {"name": "cost_center", "type": "string", "max_length": 32},
    {"name": "supplier_id", "type": "ref", "ktype": "crm.organization"},
    {"name": "request_date", "type": "date", "required": true},
    {"name": "needed_by", "type": "date"},
    {"name": "justification", "type": "text"},
    {"name": "lines", "type": "array"},
    {"name": "subtotal", "type": "number", "min": 0},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "status", "type": "enum", "values": ["requested", "approved", "ordered", "cancelled"], "default": "requested"},
    {"name": "approved_by", "type": "ref", "ktype": "user"},
    {"name": "approved_at", "type": "string"},
    {"name": "po_id", "type": "ref", "ktype": "procurement.purchase_order"},
    {"name": "ordered_at", "type": "string"},
    {"name": "owner", "type": "ref", "ktype": "user"}
  ],
  "views": {
    "list": {"columns": ["requisition_number", "requested_by", "request_date", "subtotal", "currency", "status"]},
    "form": {"sections": [
      {"title": "Requisition", "fields": ["requisition_number", "requested_by", "department", "cost_center", "request_date", "needed_by", "justification"]},
      {"title": "Supplier (optional)", "fields": ["supplier_id"]},
      {"title": "Items", "fields": ["lines", "subtotal", "currency"]},
      {"title": "Lifecycle", "fields": ["status", "approved_by", "approved_at", "po_id", "ordered_at"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "requisition_number", "card_subtitle": "subtotal"}
  },
  "cards": {"summary": "PR {{requisition_number}} — {{subtotal}} {{currency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["procurement.admin", "tenant.admin"]},
  "workflow": {
    "name": "procurement.purchase_requisition.lifecycle",
    "initial_state": "requested",
    "states": ["requested", "approved", "ordered", "cancelled"],
    "transitions": [
      {"from": ["requested"], "to": "approved", "action": "approve"},
      {"from": ["approved"], "to": "ordered", "action": "convert"},
      {"from": ["requested", "approved"], "to": "cancelled", "action": "cancel"}
    ]
  },
  "agent_tools": ["procurement.create_requisition", "procurement.approve_requisition", "procurement.convert_requisition_to_po", "procurement.cancel_requisition"]
}`)

// PurchaseRequisitionKTypes returns the requisition KType so it
// can be appended to the procurement registration pass. Kept as a
// dedicated accessor rather than folded into sales.All() so a
// future split of the `sales` package into `sales` + `procurement`
// remains low-friction.
func PurchaseRequisitionKTypes() []ktype.KType {
	return []ktype.KType{
		{Name: KTypePurchaseRequisition, Version: 1, Schema: purchaseRequisitionSchema},
	}
}
