// returns.go — Phase N9a: Sales Returns / RMA.
//
// A `sales.return` KRecord represents an authorised customer return
// against a previously posted finance.ar_invoice. The KType is a thin
// orchestrator over primitives that already exist on the platform:
//
//   - inventory.RecordMove (positive qty, source_ktype=sales.return)
//     puts the returned units back into the warehouse the moment the
//     goods physically come back. The partial unique index
//     inventory_moves_source_uniq (tenant_id, source_ktype, source_id,
//     item_id, warehouse_id) guarantees the receipt is recorded
//     exactly once per (return, line) pair, so a retried Receive call
//     does not double-stock the inventory.
//
//   - InvoicePoster.PostCreditNote builds the contra-JE that reverses
//     AR + revenue on the original invoice (Dr Revenue, Cr AR). We
//     model the financial side as a real finance.credit_note KRecord
//     so it shows up alongside any other credit-note traffic the
//     tenant has authored manually — the only Returns-specific bit is
//     that it carries `created_from_return_id` pointing back to the
//     return that fathered it.
//
// The Return therefore owns a small state machine (requested →
// approved → received → refunded) where each transition fires
// exactly one primitive against the underlying engines.
// Cancellation is permitted from any pre-refund state and behaves
// differently depending on what side-effects have already been
// posted:
//
//   - Cancel from requested or approved: pure status flip. Neither
//     state has posted anything to inventory or finance.
//   - Cancel from received: ReturnPoster.Cancel auto-reverses the
//     positive-qty inventory moves Receive emitted, by posting
//     contra-rows via inventory.ReverseMove (guarded by
//     inventory_moves_reversal_of_uniq for idempotency). Without
//     this, a cancelled-after-received return would leave the
//     stocked units permanently credited to the warehouse — a
//     silent inflation that operators would have to manually
//     unwind. This mirrors ERPNext's Stock Entry cancellation
//     (auto-posts reverse Stock Ledger Entries with
//     is_cancelled=1).
//   - Cancel from refunded: REJECTED with ErrInvalidReturnState.
//     The credit-note JE has already reversed AR + revenue; the
//     financial side cannot be undone by a status flip and a bare
//     reverse would not give operators an audit trail of the
//     intent. To recover from a mistaken refund, issue a fresh
//     sales invoice for the same amount — same approach as
//     ERPNext.

package sales

import "github.com/kennguy3n/kapp-fab/internal/ktype"

// KType identifier and workflow constants. Kept as exported
// constants so handlers, tests, and agent tools all reference the
// same strings without risk of typo drift.
const (
	// KTypeSalesReturn is the public KType name for a customer
	// return. The string is wire-stable: agent tools, KChat
	// commands, and the source_ktype column on inventory moves
	// all hard-code it.
	KTypeSalesReturn = "sales.return"

	// WorkflowSalesReturn is the workflow id registered on the
	// KType. Mirrors the convention used by sales.order and
	// sales.pos_invoice — `<ktype>.lifecycle`.
	WorkflowSalesReturn = "sales.return.lifecycle"
)

// Sales return lifecycle states. Forward-only except for
// cancellation, which is permitted from any pre-refund state. Cancel
// from `received` auto-reverses the inventory moves Receive posted
// (see ReturnPoster.Cancel doc and the module header) so the
// stock_levels view is not silently inflated; cancel from `refunded`
// is rejected because the credit-note JE cannot be undone by a
// status flip.
const (
	ReturnStatusRequested = "requested"
	ReturnStatusApproved  = "approved"
	ReturnStatusReceived  = "received"
	ReturnStatusRefunded  = "refunded"
	ReturnStatusCancelled = "cancelled"
)

// salesReturnSchema — customer return / RMA KType. Mirrors ERPNext's
// Sales Return DocType shape (a credit-note-shaped header + a per-line
// detail array) but keeps the financial reversal off to a dedicated
// finance.credit_note KRecord so AP/AR reports treat the credit
// uniformly regardless of whether it originated as a return or as a
// standalone goodwill credit.
//
// Field notes worth calling out:
//
//   - `original_invoice_id` is required because the credit-note
//     posting reads AR + revenue account codes off the original
//     invoice. Without it the refund step would have no defensible
//     source for those codes.
//   - `warehouse_id` is required because the receive step needs to
//     know which warehouse the units come back into. Picking a
//     fallback would silently misplace stock.
//   - `lines[]` carries {item_id, qty, unit_price, line_total}; the
//     receive step posts one inventory_moves row per line with
//     qty=+line.qty.
//   - `credit_note_id` and `journal_entry_id` are stamped by the
//     poster as the return advances; they are not user-editable and
//     exist on the schema only so the UI can show drill-down links.
var salesReturnSchema = []byte(`{
  "name": "sales.return",
  "version": 1,
  "fields": [
    {"name": "return_number", "type": "string", "max_length": 64},
    {"name": "original_invoice_id", "type": "ref", "ktype": "finance.ar_invoice", "required": true},
    {"name": "customer_id", "type": "ref", "ktype": "crm.organization", "required": true},
    {"name": "warehouse_id", "type": "ref", "ktype": "inventory.warehouse", "required": true},
    {"name": "return_date", "type": "date", "required": true},
    {"name": "reason", "type": "text"},
    {"name": "lines", "type": "array"},
    {"name": "subtotal", "type": "number", "min": 0},
    {"name": "tax_amount", "type": "number", "min": 0},
    {"name": "total", "type": "number", "min": 0, "required": true},
    {"name": "currency", "type": "string", "pattern": "^[A-Z]{3}$", "default": "USD"},
    {"name": "status", "type": "enum", "values": ["requested", "approved", "received", "refunded", "cancelled"], "default": "requested"},
    {"name": "credit_note_id", "type": "string"},
    {"name": "journal_entry_id", "type": "string"},
    {"name": "received_at", "type": "datetime"},
    {"name": "refunded_at", "type": "datetime"},
    {"name": "owner", "type": "ref", "ktype": "user"}
  ],
  "views": {
    "list": {"columns": ["return_number", "customer_id", "return_date", "total", "currency", "status"]},
    "form": {"sections": [
      {"title": "Return", "fields": ["return_number", "original_invoice_id", "customer_id", "warehouse_id", "return_date", "owner", "reason"]},
      {"title": "Items", "fields": ["lines", "subtotal", "tax_amount", "total", "currency"]},
      {"title": "Lifecycle", "fields": ["status", "received_at", "refunded_at", "credit_note_id", "journal_entry_id"]}
    ]},
    "kanban": {"group_by": "status", "card_title": "return_number", "card_subtitle": "total"}
  },
  "cards": {"summary": "Return {{return_number}} — {{total}} {{currency}} ({{status}})"},
  "permissions": {"read": ["tenant.member"], "write": ["sales.admin", "tenant.admin"]},
  "workflow": {
    "name": "sales.return.lifecycle",
    "initial_state": "requested",
    "states": ["requested", "approved", "received", "refunded", "cancelled"],
    "transitions": [
      {"from": ["requested"], "to": "approved", "action": "approve"},
      {"from": ["approved"], "to": "received", "action": "receive"},
      {"from": ["received"], "to": "refunded", "action": "refund"},
      {"from": ["requested", "approved", "received"], "to": "cancelled", "action": "cancel"}
    ]
  },
  "agent_tools": ["sales.create_return", "sales.approve_return", "sales.receive_return", "sales.refund_return", "sales.cancel_return"]
}`)

// ReturnKTypes returns every KType the Returns/RMA module
// advertises. Wired into services/api/ktype_boot.go alongside the
// rest of the sales catalog. Kept as its own helper (rather than
// stuffed into sales.All()) so a deployment that doesn't want to
// expose returns yet can opt out by dropping a single call site.
func ReturnKTypes() []ktype.KType {
	return []ktype.KType{
		{Name: KTypeSalesReturn, Version: 1, Schema: salesReturnSchema},
	}
}
