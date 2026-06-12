/**
 * Local i18n string table for the subcontracting workbench.
 *
 * All copy is namespaced under `subcontracting.*` so it is organised and
 * ready to be lifted into the shared catalogues later. It lives here
 * rather than in `src/locales/*.json` on purpose: the Go-side parity
 * test (`internal/i18n/parity_test.go`) requires the keyset of every
 * frontend catalogue to be IDENTICAL to its backend twin under
 * `internal/i18n/locales/`. Adding `subcontracting.*` keys to the
 * frontend JSON without the matching backend keys would fail that test —
 * and the backend half is out of scope for this frontend-only change.
 * Keeping the strings in a local module gives us the namespace without
 * breaking parity (mirrors components/ConsolidationStrings.ts).
 *
 * `st(key)` mirrors the shared `t()` signature (loud-but-safe: returns
 * the literal key if it is ever missing) so a future migration to the
 * shared catalogue is a near-mechanical find/replace.
 */

export const SUBCONTRACTING_STRINGS = {
  "subcontracting.title": "Subcontracting Workbench",
  "subcontracting.subtitle":
    "Out-source an operation to a supplier: issue components out of stock, receive the finished item back valued at the supplier's service charge, then close the order. Each step posts the matching inventory move.",

  // Create form
  "subcontracting.create.heading": "Create subcontract order",
  "subcontracting.create.item": "Finished item",
  "subcontracting.create.selectItem": "Select item…",
  "subcontracting.create.warehouse": "Warehouse",
  "subcontracting.create.selectWarehouse": "Select warehouse…",
  "subcontracting.create.qty": "Qty",
  "subcontracting.create.supplier": "Supplier ID",
  "subcontracting.create.supplierHint":
    "Optional CRM organisation reference (UUID) for the supplier performing the work.",
  "subcontracting.create.charge": "Service charge",
  "subcontracting.create.chargeCurrency": "Currency",
  "subcontracting.create.notes": "Notes",
  "subcontracting.create.components": "Components to issue",
  "subcontracting.create.componentsHint":
    "Raw materials issued out of our stock to the supplier. At least one is required.",
  "subcontracting.create.addComponent": "Add component",
  "subcontracting.create.removeComponent": "Remove",
  "subcontracting.create.submit": "Create order",
  "subcontracting.create.submitting": "Creating…",
  "subcontracting.create.needsComponents": "Add at least one component to issue.",

  // Orders list
  "subcontracting.orders.heading": "Subcontract orders",
  "subcontracting.orders.empty":
    "No subcontract orders yet. Create one to out-source an operation.",
  "subcontracting.orders.loading": "Loading orders…",
  "subcontracting.orders.error": "Failed to load subcontract orders.",
  "subcontracting.orders.filterStatus": "Status",
  "subcontracting.orders.filterAll": "All",
  "subcontracting.orders.item": "Item",
  "subcontracting.orders.qty": "Qty",
  "subcontracting.orders.received": "Received",
  "subcontracting.orders.status": "Status",
  "subcontracting.orders.charge": "Charge",
  "subcontracting.orders.view": "View",

  // Statuses
  "subcontracting.status.draft": "Draft",
  "subcontracting.status.issued": "Issued",
  "subcontracting.status.received": "Received",
  "subcontracting.status.closed": "Closed",
  "subcontracting.status.cancelled": "Cancelled",

  // Detail + lifecycle
  "subcontracting.detail.heading": "Order detail",
  "subcontracting.detail.select": "Select an order to drive its lifecycle.",
  "subcontracting.detail.loading": "Loading order…",
  "subcontracting.detail.error": "Failed to load the order.",
  "subcontracting.detail.supplier": "Supplier",
  "subcontracting.detail.warehouse": "Warehouse",
  "subcontracting.detail.qty": "Quantity",
  "subcontracting.detail.received": "Received qty",
  "subcontracting.detail.charge": "Service charge",
  "subcontracting.detail.workOrder": "Work order",
  "subcontracting.detail.issuedAt": "Issued",
  "subcontracting.detail.receivedAt": "Received",
  "subcontracting.detail.notes": "Notes",
  "subcontracting.detail.componentsHeading": "Components",
  "subcontracting.detail.componentItem": "Item",
  "subcontracting.detail.componentQty": "Planned",
  "subcontracting.detail.componentIssued": "Issued",
  "subcontracting.detail.none": "—",

  // Lifecycle actions
  "subcontracting.action.issue": "Issue components",
  "subcontracting.action.receive": "Receive finished item",
  "subcontracting.action.close": "Close order",
  "subcontracting.action.cancel": "Cancel order",

  // Confirmations
  "subcontracting.confirm.issueTitle": "Issue components to the supplier?",
  "subcontracting.confirm.issueBody":
    "This posts a negative inventory move for every component, moving the order from draft to issued. It cannot be cancelled afterwards — only received or deliberately reversed.",
  "subcontracting.confirm.receiveTitle": "Receive the finished item?",
  "subcontracting.confirm.receiveBody":
    "This posts a positive receipt move for the finished item, valued at the supplier's service charge per unit, and moves the order to received.",
  "subcontracting.confirm.closeTitle": "Close this order?",
  "subcontracting.confirm.closeBody":
    "Closing settles the order. This is the terminal state for a received order.",
  "subcontracting.confirm.cancelTitle": "Cancel this order?",
  "subcontracting.confirm.cancelBody":
    "Only a draft order can be cancelled, before any components have been issued.",
  "subcontracting.confirm.confirm": "Confirm",
  "subcontracting.confirm.cancel": "Back",
  "subcontracting.confirm.working": "Working…",

  // Generic
  "subcontracting.error": "Something went wrong.",
} as const;

export type SubcontractingStringKey = keyof typeof SUBCONTRACTING_STRINGS;

/**
 * st — namespaced string resolver. Returns the literal key when it is
 * missing so a typo renders loudly rather than as a blank label,
 * matching the three-stage fallback of the shared `t()`.
 */
export function st(key: SubcontractingStringKey): string {
  return SUBCONTRACTING_STRINGS[key] ?? key;
}

/** Interpolate `{name}` placeholders, mirroring the shared `t()`. */
export function stp(
  key: SubcontractingStringKey,
  params: Record<string, string | number>,
): string {
  return st(key).replace(/\{(\w+)\}/g, (whole, name: string) =>
    name in params ? String(params[name]) : whole,
  );
}
