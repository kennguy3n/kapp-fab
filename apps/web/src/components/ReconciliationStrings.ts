/**
 * Local i18n string table for the bank-reconciliation console.
 *
 * All copy is namespaced under `reconciliation.*` so it is organised and
 * ready to be lifted into the shared catalogues later. It lives here
 * rather than in `src/locales/*.json` on purpose: the Go-side parity test
 * (`internal/i18n/parity_test.go`) requires the keyset of every frontend
 * catalogue to be IDENTICAL to its backend twin under
 * `internal/i18n/locales/`. Adding `reconciliation.*` keys to the frontend
 * JSON without the matching backend keys would fail that test — and the
 * backend half is out of scope for this frontend-only change. Keeping the
 * strings in a local module gives us the namespace without breaking
 * parity. (This mirrors the sibling `ConsolidationStrings.ts`.)
 *
 * `rt(key)` mirrors the shared `t()` signature (loud-but-safe: returns the
 * literal key if it is ever missing) so a future migration to the shared
 * catalogue is a near-mechanical find/replace.
 */

export const RECONCILIATION_STRINGS = {
  // Async / edge states
  "reconciliation.loading": "Loading…",
  "reconciliation.retry": "Retry",
  "reconciliation.accounts.empty": "No bank accounts yet.",
  "reconciliation.accounts.error": "Could not load bank accounts.",
  "reconciliation.selectAccount": "Select a bank account.",
  "reconciliation.txns.error": "Could not load transactions.",
  "reconciliation.suggestions.error": "Could not load suggestions.",
  "reconciliation.suggestions.empty": "No match suggestions to review.",
  "reconciliation.txns.empty": "No transactions yet.",
  "reconciliation.nothingLeft": "Nothing left to reconcile.",

  // Search / filter
  "reconciliation.search.label": "Filter unmatched lines",
  "reconciliation.search.placeholder": "Filter by payee, amount or date…",
  "reconciliation.search.noMatches": "No lines match “{query}”.",
  "reconciliation.search.cleared": "Showing all unmatched lines.",

  // Anomalies
  "reconciliation.flag.duplicate": "Duplicate",
  "reconciliation.flag.duplicate.hint":
    "Another line has the same date, amount and description — possibly an imported-twice row.",
  "reconciliation.flag.reversal": "Reversal",
  "reconciliation.flag.reversal.hint":
    "An equal-and-opposite line exists — likely a refund or bounced payment. Reconcile the pair together.",

  // Multi-currency
  "reconciliation.fx.foreign": "Foreign currency",
  "reconciliation.fx.base": "Base equivalent",
  "reconciliation.fx.diff": "FX difference",
  "reconciliation.fx.rateLabel": "Rate",
  "reconciliation.fx.noRate":
    "No exchange rate available — base equivalent can’t be shown.",
  "reconciliation.fx.mismatchWarning":
    "This line is in {lineCurrency} but the account’s base currency is {baseCurrency}. Confirm the currency before matching so an amount isn’t mis-matched across currencies.",
  "reconciliation.fx.confirmMatch": "Match anyway",
  "reconciliation.fx.rateApplied": "Rate {rate} applied",

  // Undo / correction
  "reconciliation.unmatch": "Unmatch",
  "reconciliation.unmatch.done": "Line returned to unmatched",
  "reconciliation.unmatch.failed": "Could not unmatch line",
  "reconciliation.bulk.recent":
    "Bulk-matched {count} line{plural} in the last action.",
  "reconciliation.bulk.undo": "Undo",
  "reconciliation.bulk.undone": "Reverted {count} matched line{plural}",
  "reconciliation.bulk.undoFailed": "Could not undo every line",

  // Split / partial match
  "reconciliation.split.title": "Split match",
  "reconciliation.split.intro":
    "Allocate this bank line across multiple ledger entries. Reconcile when the running difference reaches zero.",
  "reconciliation.split.open": "Split across entries",
  "reconciliation.split.close": "Close split",
  "reconciliation.split.target": "Line amount",
  "reconciliation.split.allocated": "Allocated",
  "reconciliation.split.remaining": "Remaining",
  "reconciliation.split.add": "Add entry",
  "reconciliation.split.remove": "Remove",
  "reconciliation.split.reconcile": "Reconcile split",
  "reconciliation.split.balanced": "Balanced — ready to reconcile.",
  "reconciliation.split.unbalanced": "Allocate the full amount to reconcile.",
  "reconciliation.split.needTwo": "Add at least one entry to split against.",
  "reconciliation.split.duplicateEntry":
    "Each ledger entry can only be used once — pick a different entry for the duplicated row.",
  "reconciliation.split.done": "Split reconciled across {count} entries",
  "reconciliation.split.failed": "Could not reconcile the split",

  // Keyboard throughput
  "reconciliation.kbd.hint":
    "Keyboard: ↑/↓ move · A accept · R reject · S skip",
  "reconciliation.kbd.accept": "Accept",
  "reconciliation.kbd.reject": "Reject",
  "reconciliation.kbd.skip": "Skip",
} as const;

export type ReconciliationStringKey = keyof typeof RECONCILIATION_STRINGS;

/**
 * rt — namespaced string resolver. Returns the literal key when it is
 * missing so a typo renders loudly rather than as a blank label, matching
 * the three-stage fallback of the shared `t()`.
 */
export function rt(key: ReconciliationStringKey): string {
  return RECONCILIATION_STRINGS[key] ?? key;
}

/** Interpolate `{name}` placeholders, mirroring the shared `t()`. */
export function rtp(
  key: ReconciliationStringKey,
  params: Record<string, string | number>,
): string {
  return rt(key).replace(/\{(\w+)\}/g, (whole, name: string) =>
    name in params ? String(params[name]) : whole,
  );
}
