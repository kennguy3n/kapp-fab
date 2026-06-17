/**
 * Local i18n string table for the MRP (Material Requirements Planning)
 * console.
 *
 * All copy is namespaced under `mrp.*` so it is organised and ready to
 * be lifted into the shared catalogues later. It lives here rather than
 * in `src/locales/*.json` on purpose: the Go-side parity test
 * (`internal/i18n/parity_test.go`) requires the keyset of every frontend
 * catalogue to be IDENTICAL to its backend twin under
 * `internal/i18n/locales/`. Adding `mrp.*` keys to the frontend JSON
 * without the matching backend keys would fail that test — and the
 * backend half is out of scope for this frontend-only change. Keeping
 * the strings in a local module gives us the namespace without breaking
 * parity (mirrors components/ConsolidationStrings.ts).
 *
 * `mt(key)` mirrors the shared `t()` signature (loud-but-safe: returns
 * the literal key if it is ever missing) so a future migration to the
 * shared catalogue is a near-mechanical find/replace.
 */

export const MRP_STRINGS = {
  "mrp.title": "MRP Console",
  "mrp.subtitle":
    "Run material requirements planning: net independent demand against on-hand stock and open work orders, explode active BOMs, and review the suggested make / buy planned orders with backward-scheduled release dates.",

  // Run form
  "mrp.run.heading": "Run MRP",
  "mrp.run.horizonStart": "Horizon start",
  "mrp.run.horizonEnd": "Horizon end",
  "mrp.run.includeMinStock": "Top up items below reorder level",
  "mrp.run.buyLeadTime": "Buy lead time (days)",
  "mrp.run.buyLeadTimeHint":
    "Purchasing lead time used to backward-schedule buy orders. Leave blank for the default (7 days).",
  "mrp.run.notes": "Notes",
  "mrp.run.demand": "Independent demand",
  "mrp.run.demandHint":
    "Sales orders, work orders, or manual top-ups due within the horizon. A run needs at least one demand line unless reorder top-up is enabled.",
  "mrp.run.addDemand": "Add demand line",
  "mrp.run.removeDemand": "Remove",
  "mrp.run.item": "Item",
  "mrp.run.selectItem": "Select item…",
  "mrp.run.qty": "Qty",
  "mrp.run.dueDate": "Due date",
  "mrp.run.source": "Source",
  "mrp.run.submit": "Run MRP",
  "mrp.run.submitting": "Running…",
  "mrp.run.needsDemand":
    "Add at least one demand line, or enable reorder-level top-up.",

  // Demand sources
  "mrp.source.sales_order": "Sales order",
  "mrp.source.work_order": "Work order",
  "mrp.source.min_stock": "Min stock",
  "mrp.source.manual": "Manual",

  // Runs list
  "mrp.runs.heading": "Past runs",
  "mrp.runs.empty": "No MRP runs yet. Run the planner to generate planned orders.",
  "mrp.runs.loading": "Loading runs…",
  "mrp.runs.error": "Failed to load MRP runs.",
  "mrp.runs.horizon": "Horizon",
  "mrp.runs.status": "Status",
  "mrp.runs.demandLines": "Demand",
  "mrp.runs.plannedOrders": "Planned",
  "mrp.runs.makeBuy": "Make / Buy",
  "mrp.runs.created": "Created",
  "mrp.runs.view": "View",
  "mrp.runs.minStockBadge": "+reorder",

  // Run statuses
  "mrp.status.completed": "Completed",
  "mrp.status.failed": "Failed",

  // Run detail
  "mrp.detail.heading": "Run detail",
  "mrp.detail.select": "Select a run to see its demand and planned orders.",
  "mrp.detail.loading": "Loading run…",
  "mrp.detail.error": "Failed to load the run.",
  "mrp.detail.demandHeading": "Demand lines",
  "mrp.detail.demandEmpty": "This run was computed against no explicit demand lines.",
  "mrp.detail.plannedHeading": "Planned orders",
  "mrp.detail.plannedEmpty":
    "Net requirements were fully covered by stock — nothing to order.",
  "mrp.detail.sourceRef": "Reference",
  "mrp.detail.orderType": "Type",
  "mrp.detail.suggestedStart": "Suggested start",
  "mrp.detail.dueDate": "Due",
  "mrp.detail.level": "BOM level",
  "mrp.detail.leadTime": "Lead time",
  "mrp.detail.leadTimeDays": "{days}d",
  "mrp.detail.notes": "Notes",

  // Order types
  "mrp.orderType.make": "Make",
  "mrp.orderType.buy": "Buy",

  // Page chrome
  "mrp.eyebrow": "Manufacturing",

  // Run-list async states
  "mrp.runs.retry": "Try again",
  "mrp.runs.errorTitle": "Couldn't load MRP runs",
  "mrp.runs.emptyTitle": "No MRP runs yet",
  "mrp.runs.emptyBody": "Run the planner to generate make and buy suggestions.",

  // Run-detail async states + actionable presentation
  "mrp.detail.errorTitle": "Couldn't load this run",
  "mrp.detail.explainer":
    "Each card below is an order the planner suggests to cover a shortfall — make it in-house or buy it in, in the quantity shown, and start it by the release date so it arrives before it's needed.",
  "mrp.detail.makeHeading": "Make in-house",
  "mrp.detail.buyHeading": "Buy from a supplier",
  "mrp.detail.makeCount": "{count} to make",
  "mrp.detail.buyCount": "{count} to buy",
  "mrp.suggestion.qtyLabel": "Quantity",
  "mrp.suggestion.startByLabel": "Start by",
  "mrp.suggestion.dueByLabel": "Needed by",
  "mrp.suggestion.leadTimeLabel": "Lead time",
  "mrp.suggestion.levelLabel": "BOM level",
  "mrp.suggestion.leadTimeDays": "{days} day(s)",
  "mrp.detail.viewRun": "View {date}",

  // Generic
  "mrp.error": "Something went wrong.",
} as const;

export type MrpStringKey = keyof typeof MRP_STRINGS;

/**
 * mt — namespaced string resolver. Returns the literal key when it is
 * missing so a typo renders loudly rather than as a blank label,
 * matching the three-stage fallback of the shared `t()`.
 */
export function mt(key: MrpStringKey): string {
  return MRP_STRINGS[key] ?? key;
}

/** Interpolate `{name}` placeholders, mirroring the shared `t()`. */
export function mtp(
  key: MrpStringKey,
  params: Record<string, string | number>,
): string {
  return mt(key).replace(/\{(\w+)\}/g, (whole, name: string) =>
    name in params ? String(params[name]) : whole,
  );
}
