/**
 * Local i18n string table for the consolidation / FX-review console.
 *
 * All copy is namespaced under `consolidation.*` so it is organised
 * and ready to be lifted into the shared catalogues later. It lives
 * here rather than in `src/locales/*.json` on purpose: the Go-side
 * parity test (`internal/i18n/parity_test.go`) requires the keyset of
 * every frontend catalogue to be IDENTICAL to its backend twin under
 * `internal/i18n/locales/`. Adding `consolidation.*` keys to the
 * frontend JSON without the matching backend keys would fail that
 * test — and the backend half is out of scope for this frontend-only
 * change. Keeping the strings in a local module gives us the
 * namespace without breaking parity.
 *
 * `ct(key)` mirrors the `t()` signature (loud-but-safe: returns the
 * literal key if it is ever missing) so a future migration to the
 * shared catalogue is a near-mechanical find/replace.
 */

export const CONSOLIDATION_STRINGS = {
  "consolidation.title": "Consolidation & FX Review",
  "consolidation.subtitle":
    "Roll up subsidiary trial balances into one presentation currency, review currency-translation (CTA) and intercompany eliminations, and clear unrealized FX before posting. Admin only.",

  // Tabs
  "consolidation.tab.groups": "Groups & Run",
  "consolidation.tab.trialBalance": "Trial Balance",
  "consolidation.tab.statements": "Statements",
  "consolidation.tab.fx": "FX Review",

  // Groups panel
  "consolidation.groups.heading": "Consolidation groups",
  "consolidation.groups.create": "Create group",
  "consolidation.groups.known": "Known groups",
  "consolidation.groups.empty":
    "No groups tracked in this browser yet. Create one, or add an existing group by ID.",
  "consolidation.groups.name": "Name",
  "consolidation.groups.presentationCurrency": "Presentation currency",
  "consolidation.groups.members": "Member tenant IDs (one per line or comma-separated)",
  "consolidation.groups.ctaAccount": "CTA account code (optional)",
  "consolidation.groups.ctaAccountHint":
    "Equity account that absorbs currency-translation differences. Defaults to 3900.",
  "consolidation.groups.eliminations": "Intercompany elimination pairs",
  "consolidation.groups.addElimination": "Add pair",
  "consolidation.groups.removeElimination": "Remove",
  "consolidation.groups.fromTenant": "From tenant",
  "consolidation.groups.toTenant": "To tenant",
  "consolidation.groups.accountCode": "Account code",
  "consolidation.groups.fromAccount": "From account (optional)",
  "consolidation.groups.toAccount": "To account (optional)",
  "consolidation.groups.creating": "Creating…",
  "consolidation.groups.addExisting": "Track existing group by ID",
  "consolidation.groups.add": "Track",
  "consolidation.groups.select": "Select",
  "consolidation.groups.selected": "Selected",
  "consolidation.groups.activeGroup": "Active group",
  "consolidation.groups.forget": "Forget",
  "consolidation.groups.membersCount": "{count} entities",
  "consolidation.groups.listEndpointNote":
    "The backend has no list-groups endpoint, so this console remembers the groups you create or track here (per browser).",

  // Run controls
  "consolidation.run.heading": "Run consolidation",
  "consolidation.run.asOf": "As of (optional)",
  "consolidation.run.run": "Run consolidation",
  "consolidation.run.running": "Running…",
  "consolidation.run.statements": "Build statement pack",
  "consolidation.run.buildingStatements": "Building…",
  "consolidation.run.averageRates": "Average rates for P&L translation",
  "consolidation.run.averageRatesHint":
    "Period-average rate INTO the presentation currency, keyed by each entity's base currency (e.g. EUR → 1.08). Without these, income-statement accounts translate at the closing rate and CTA collapses to zero.",
  "consolidation.run.addRate": "Add rate",
  "consolidation.run.currency": "Currency",
  "consolidation.run.rate": "Rate",
  "consolidation.run.removeRate": "Remove",
  "consolidation.run.needsGroup": "Select or create a group first.",

  // Trial balance
  "consolidation.tb.heading": "Consolidated trial balance",
  "consolidation.tb.asOf": "As of",
  "consolidation.tb.account": "Account",
  "consolidation.tb.type": "Type",
  "consolidation.tb.debit": "Debit",
  "consolidation.tb.credit": "Credit",
  "consolidation.tb.balance": "Balance",
  "consolidation.tb.total": "Total",
  "consolidation.tb.cta": "Cumulative translation adjustment",
  "consolidation.tb.residual": "Residual (Dr − Cr)",
  "consolidation.tb.totalDebit": "Total debit",
  "consolidation.tb.totalCredit": "Total credit",
  "consolidation.tb.balanced": "Balanced",
  "consolidation.tb.unbalanced": "Unbalanced",
  "consolidation.tb.ctaRow": "CTA",
  "consolidation.tb.drillHint": "Select a row to see per-entity contributions and any eliminations applied.",
  "consolidation.tb.contributions": "Per-entity contributions",
  "consolidation.tb.entity": "Entity",
  "consolidation.tb.eliminationsApplied": "Intercompany eliminations on this account",
  "consolidation.tb.noContributions": "No per-entity breakdown for this row.",
  "consolidation.tb.eliminated": "Eliminated (intercompany)",
  "consolidation.tb.empty": "Run a consolidation to see the combined trial balance.",
  "consolidation.tb.expand": "Show breakdown",
  "consolidation.tb.collapse": "Hide breakdown",

  // Statements
  "consolidation.stmt.heading": "Consolidated statements",
  "consolidation.stmt.empty": "Build the statement pack to see the consolidated P&L and balance sheet.",
  "consolidation.stmt.incomeStatement": "Income statement (P&L)",
  "consolidation.stmt.revenue": "Revenue",
  "consolidation.stmt.expense": "Expense",
  "consolidation.stmt.totalRevenue": "Total revenue",
  "consolidation.stmt.totalExpense": "Total expense",
  "consolidation.stmt.netIncome": "Net income",
  "consolidation.stmt.balanceSheet": "Balance sheet",
  "consolidation.stmt.assets": "Assets",
  "consolidation.stmt.liabilities": "Liabilities",
  "consolidation.stmt.equity": "Equity",
  "consolidation.stmt.totalAssets": "Total assets",
  "consolidation.stmt.totalLiabilities": "Total liabilities",
  "consolidation.stmt.totalEquity": "Total equity",
  "consolidation.stmt.amount": "Amount",

  // FX review
  "consolidation.fx.heading": "FX review",
  "consolidation.fx.rates": "Exchange rates",
  "consolidation.fx.ratesHint": "Per-tenant daily quotes used for current-rate translation and revaluation.",
  "consolidation.fx.from": "From",
  "consolidation.fx.to": "To",
  "consolidation.fx.rate": "Rate",
  "consolidation.fx.date": "Date",
  "consolidation.fx.provider": "Provider",
  "consolidation.fx.saveRate": "Save rate",
  "consolidation.fx.savingRate": "Saving…",
  "consolidation.fx.noRates": "No exchange rates yet.",
  "consolidation.fx.pair": "Pair",

  "consolidation.fx.translate": "Current-rate translation",
  "consolidation.fx.translateHint": "Convert a foreign amount into the functional currency at the as-of rate.",
  "consolidation.fx.amount": "Amount",
  "consolidation.fx.convert": "Translate",
  "consolidation.fx.converting": "Translating…",
  "consolidation.fx.converted": "Converted",
  "consolidation.fx.usingRate": "at rate",

  "consolidation.fx.unrealized": "Unrealized gain/loss (review before posting)",
  "consolidation.fx.unrealizedHint":
    "Compute the unrealized FX delta on an open foreign position at today's rate — review it here before triggering a revaluation run that posts adjustments.",
  "consolidation.fx.foreignAmount": "Foreign amount",
  "consolidation.fx.foreignCurrency": "Foreign currency",
  "consolidation.fx.functionalCurrency": "Functional currency",
  "consolidation.fx.originalRate": "Original rate",
  "consolidation.fx.compute": "Compute delta",
  "consolidation.fx.computing": "Computing…",
  "consolidation.fx.unrealizedResult": "Unrealized gain/loss",
  "consolidation.fx.gainHint": "Positive = gain, negative = loss (functional currency).",

  "consolidation.fx.revaluation": "FX revaluation run",
  "consolidation.fx.revaluationHint":
    "Revalue a tenant's open foreign-currency balances and post the unrealized gain/loss. Review the per-account deltas below; this posts journal entries.",
  "consolidation.fx.tenantId": "Tenant ID",
  "consolidation.fx.gainAccount": "Gain account (optional)",
  "consolidation.fx.lossAccount": "Loss account (optional)",
  "consolidation.fx.runReval": "Run revaluation",
  "consolidation.fx.runningReval": "Revaluing…",
  "consolidation.fx.revalAsOf": "As of (optional)",
  "consolidation.fx.totalGain": "Total gain",
  "consolidation.fx.totalLoss": "Total loss",
  "consolidation.fx.net": "Net",
  "consolidation.fx.revalLines": "Per-account revaluation",
  "consolidation.fx.recordedBase": "Recorded base",
  "consolidation.fx.revaluedBase": "Revalued base",
  "consolidation.fx.delta": "Delta",
  "consolidation.fx.currentRate": "Current rate",
  "consolidation.fx.foreignNet": "Foreign net",
  "consolidation.fx.glAccount": "G/L account",
  "consolidation.fx.skipped": "Skipped (no rate available)",
  "consolidation.fx.reason": "Reason",
  "consolidation.fx.noRevalLines": "No open foreign-currency balances were revalued.",
  "consolidation.fx.revalEmpty": "Run a revaluation to review unrealized FX gain/loss.",

  // Generic
  "consolidation.error": "Something went wrong.",
} as const;

export type ConsolidationStringKey = keyof typeof CONSOLIDATION_STRINGS;

/**
 * ct — namespaced string resolver. Returns the literal key when it is
 * missing so a typo renders loudly rather than as a blank label,
 * matching the three-stage fallback of the shared `t()`.
 */
export function ct(key: ConsolidationStringKey): string {
  return CONSOLIDATION_STRINGS[key] ?? key;
}

/** Interpolate `{name}` placeholders, mirroring the shared `t()`. */
export function ctp(
  key: ConsolidationStringKey,
  params: Record<string, string | number>,
): string {
  return ct(key).replace(/\{(\w+)\}/g, (whole, name: string) =>
    name in params ? String(params[name]) : whole,
  );
}
