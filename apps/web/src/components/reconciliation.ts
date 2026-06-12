import type { BankFeedSuggestion, ExchangeRate, KRecord } from "@kapp/client";

// Statuses a finance.bank_transaction can hold. The first four are the
// KType enum (internal/ledger/bank.go); "transfer" is written by the
// SmartMatcher's inter-account transfer detector (matcher.go) when it
// pairs the equal-and-opposite leg in another of the tenant's accounts.
export type TxnStatus =
  | "unreconciled"
  | "matched"
  | "ignored"
  | "voided"
  | "transfer";

// Suggestions at or above this confidence are eligible for the
// accept-all-high-confidence bulk action and render with the "high" band.
export const HIGH_CONFIDENCE = 0.9;

export interface BankTxnData {
  bank_account_id?: string;
  value_date?: string;
  description?: string;
  amount?: number | string;
  currency?: string;
  status?: string;
  matched_entry_id?: string;
}

export function txnData(r: KRecord): BankTxnData {
  return r.data as unknown as BankTxnData;
}

export function txnStatus(r: KRecord): string {
  return txnData(r).status ?? "unreconciled";
}

// The currency a bank line is denominated in (the statement's own
// currency), independent of the account's base/presentation currency.
export function txnCurrency(r: KRecord): string | undefined {
  return txnData(r).currency;
}

// Half a cent: the tolerance at which a running split difference (or a
// reversal pairing) is treated as netting to zero. Floating-point sums of
// money values accumulate sub-cent error, so an exact === 0 check would
// leave a balanced split looking unbalanced.
export const AMOUNT_TOLERANCE = 0.005;

// Terminal statuses are resolved lines that have left the review queue.
const TERMINAL_STATUSES = new Set(["matched", "ignored", "voided", "transfer"]);

// isUnmatched is true for any line still needing reconciliation. Treating
// it as the complement of the terminal set (rather than an equality check
// on "unreconciled") tolerates the handful of import paths that label an
// open line "unmatched" instead of the canonical "unreconciled".
export function isUnmatched(status: string): boolean {
  return !TERMINAL_STATUSES.has(status);
}

// txnAmount coerces the stored amount (a number in JSON, but a decimal
// string when round-tripped through some import paths) to a number for
// totalling. A non-numeric value contributes zero rather than NaN.
export function txnAmount(r: KRecord): number {
  const a = txnData(r).amount;
  const n = typeof a === "number" ? a : Number(a ?? 0);
  return Number.isFinite(n) ? n : 0;
}

export interface ReconTotals {
  unmatchedCount: number;
  unmatchedAmount: number;
  matchedCount: number;
  matchedAmount: number;
  transferCount: number;
  ignoredCount: number;
  /** Net amount still to reconcile — the running "difference" Xero shows. */
  difference: number;
}

// computeTotals derives the per-account reconciliation tallies the
// summary bar renders. Voided lines are excluded entirely (they are
// soft-deleted statement corrections, not live activity).
export function computeTotals(txns: KRecord[]): ReconTotals {
  const t: ReconTotals = {
    unmatchedCount: 0,
    unmatchedAmount: 0,
    matchedCount: 0,
    matchedAmount: 0,
    transferCount: 0,
    ignoredCount: 0,
    difference: 0,
  };
  for (const r of txns) {
    const status = txnStatus(r);
    const amount = txnAmount(r);
    switch (status) {
      case "matched":
        t.matchedCount += 1;
        t.matchedAmount += amount;
        break;
      case "transfer":
        t.transferCount += 1;
        break;
      case "ignored":
        t.ignoredCount += 1;
        break;
      case "voided":
        break;
      default:
        t.unmatchedCount += 1;
        t.unmatchedAmount += amount;
    }
  }
  t.difference = t.unmatchedAmount;
  return t;
}

export type ConfidenceBand = "high" | "medium" | "low";

export function confidenceBand(confidence: number): ConfidenceBand {
  if (confidence >= HIGH_CONFIDENCE) return "high";
  if (confidence >= 0.6) return "medium";
  return "low";
}

export function formatConfidence(confidence: number): string {
  return `${Math.round(confidence * 100)}%`;
}

// parseReasons splits the matcher's comma-joined reason string (e.g.
// "exact amount, same-day, learned counterparty") into individual chips
// so the operator sees *why* a suggestion was made at a glance.
export function parseReasons(reason: string): string[] {
  return reason
    .split(",")
    .map((s) => s.trim())
    .filter((s) => s.length > 0);
}

export interface SuggestionGroup {
  transactionId: string;
  /** Candidate suggestions for this line, highest confidence first. */
  suggestions: BankFeedSuggestion[];
}

// groupSuggestions buckets the flat suggestion list by bank line so the
// review queue can show the best candidate per line and fold the rest
// behind a "find alternative" affordance. Groups are ordered by their
// best candidate's confidence (highest first), matching the server's
// highest-confidence-first list ordering.
export function groupSuggestions(
  suggestions: BankFeedSuggestion[],
): SuggestionGroup[] {
  const byTxn = new Map<string, BankFeedSuggestion[]>();
  for (const s of suggestions) {
    const list = byTxn.get(s.transaction_id) ?? [];
    list.push(s);
    byTxn.set(s.transaction_id, list);
  }
  const groups: SuggestionGroup[] = [];
  for (const [transactionId, list] of byTxn) {
    list.sort((a, b) => b.confidence - a.confidence);
    groups.push({ transactionId, suggestions: list });
  }
  groups.sort(
    (a, b) => (b.suggestions[0]?.confidence ?? 0) - (a.suggestions[0]?.confidence ?? 0),
  );
  return groups;
}

// highConfidenceSuggestions returns the open suggestions eligible for the
// accept-all bulk action: at or above HIGH_CONFIDENCE and still in the
// suggested state. Only the top (highest-confidence) suggestion per line
// is kept so accepting the batch never tries to reconcile one bank line
// against two different journal entries.
export function highConfidenceSuggestions(
  suggestions: BankFeedSuggestion[],
): BankFeedSuggestion[] {
  const best = new Map<string, BankFeedSuggestion>();
  for (const s of suggestions) {
    if (s.status !== "suggested") continue;
    if (s.confidence < HIGH_CONFIDENCE) continue;
    const cur = best.get(s.transaction_id);
    if (!cur || s.confidence > cur.confidence) {
      best.set(s.transaction_id, s);
    }
  }
  return [...best.values()];
}

export interface TransferLeg {
  txn: KRecord;
  accountName: string;
}

export interface TransferPairRow {
  /** Stable key for React lists — the joined leg ids, or a single id. */
  key: string;
  amount: number;
  currency: string;
  /** Money-out (negative) leg, when present. */
  out?: TransferLeg;
  /** Money-in (positive) leg, when present. */
  in?: TransferLeg;
}

// dateDistance returns the absolute gap in days between two value_date
// strings, or Infinity when either is missing/unparseable so a dated
// candidate is always preferred over an undated one.
function dateDistance(a?: string, b?: string): number {
  if (!a || !b) return Number.POSITIVE_INFINITY;
  const ta = Date.parse(a);
  const tb = Date.parse(b);
  if (Number.isNaN(ta) || Number.isNaN(tb)) return Number.POSITIVE_INFINITY;
  return Math.abs(ta - tb) / 86_400_000;
}

// detectTransferPairs reconstructs the inter-account transfer pairs the
// backend already produced (it marks both legs status="transfer") so the
// operator can see them as a single internal movement rather than two
// orphan lines. The backend owns the authoritative pairing in
// bank_transfer_pairs; that table is not exposed over HTTP, so we pair
// client-side by the same rule the detector uses — equal magnitude,
// opposite sign, same currency — and, when several credits could match a
// debit, prefer the one closest in value_date (the detector pairs within a
// few days) so two unrelated same-amount transfers don't get crossed.
// Lines that cannot be paired (e.g. the counter-leg is out of the loaded
// window) — and zero-amount legs, which have no magnitude to match on — are
// surfaced on their own so nothing is hidden.
export function detectTransferPairs(
  transfers: KRecord[],
  accountName: (id: string) => string,
): TransferPairRow[] {
  const legOf = (r: KRecord): TransferLeg => ({
    txn: r,
    accountName: accountName(txnData(r).bank_account_id ?? ""),
  });
  const credits = transfers.filter((r) => txnAmount(r) > 0);
  const debits = transfers.filter((r) => txnAmount(r) < 0);
  const zeros = transfers.filter((r) => txnAmount(r) === 0);
  const usedCredits = new Set<string>();
  const rows: TransferPairRow[] = [];

  for (const debit of debits) {
    const d = txnData(debit);
    const magnitude = Math.abs(txnAmount(debit));
    let match: KRecord | undefined;
    let bestGap = Number.POSITIVE_INFINITY;
    for (const c of credits) {
      if (usedCredits.has(c.id)) continue;
      const cd = txnData(c);
      if (cd.currency !== d.currency) continue;
      if (Math.abs(txnAmount(c) - magnitude) >= 1e-9) continue;
      const gap = dateDistance(cd.value_date, d.value_date);
      if (gap < bestGap) {
        bestGap = gap;
        match = c;
      }
    }
    if (match) usedCredits.add(match.id);
    rows.push({
      key: match ? `${debit.id}:${match.id}` : debit.id,
      amount: magnitude,
      currency: d.currency ?? "",
      out: legOf(debit),
      in: match ? legOf(match) : undefined,
    });
  }
  for (const credit of credits) {
    if (usedCredits.has(credit.id)) continue;
    const c = txnData(credit);
    rows.push({
      key: credit.id,
      amount: Math.abs(txnAmount(credit)),
      currency: c.currency ?? "",
      in: legOf(credit),
    });
  }
  for (const zero of zeros) {
    const z = txnData(zero);
    rows.push({
      key: zero.id,
      amount: 0,
      currency: z.currency ?? "",
      out: legOf(zero),
    });
  }
  return rows;
}

export function formatAmount(amount: number, currency?: string): string {
  const body = amount.toLocaleString(undefined, {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  });
  return currency ? `${body} ${currency}` : body;
}

// shortId trims a UUID to its first segment for compact display while
// keeping the full value available via the title attribute at call sites.
export function shortId(id: string): string {
  return id.split("-")[0] ?? id;
}

// --- Multi-currency -----------------------------------------------------
//
// Bank lines carry their own statement currency, which can differ from the
// account's base/presentation currency (a USD account receiving a EUR
// wire). The matcher pairs on amount, so a foreign line whose number
// happens to equal a base-currency ledger entry would silently mis-match.
// These helpers convert a foreign amount to its base equivalent (so the
// operator sees the comparable figure and the FX difference) and flag the
// lines that must not be reconciled without an explicit currency check.

function normCur(c: string | undefined): string {
  return (c ?? "").trim().toUpperCase();
}

// isForeignLine is true when a bank line's currency differs from the
// account's base currency. A missing currency on either side is treated as
// same-currency (no FX badge) rather than guessed.
export function isForeignLine(
  lineCurrency: string | undefined,
  baseCurrency: string | undefined,
): boolean {
  const a = normCur(lineCurrency);
  const b = normCur(baseCurrency);
  if (a === "" || b === "") return false;
  return a !== b;
}

// A directional from→to rate lookup, latest quote per pair.
export type RateMap = ReadonlyMap<string, number>;

function rateKey(from: string, to: string): string {
  return `${normCur(from)}>${normCur(to)}`;
}

// buildRateMap reduces the exchange-rate rows the API returns to the most
// recent rate per (from,to) pair, keyed for O(1) lookup. Rows with a
// non-numeric or non-positive rate are dropped — a zero/negative FX rate
// is never a valid conversion factor and would produce nonsense base
// amounts.
export function buildRateMap(rates: ExchangeRate[]): RateMap {
  const latestDate = new Map<string, string>();
  const map = new Map<string, number>();
  for (const r of rates) {
    const rate = Number(r.rate);
    if (!Number.isFinite(rate) || rate <= 0) continue;
    const key = rateKey(r.from_currency, r.to_currency);
    const prev = latestDate.get(key);
    if (prev === undefined || r.rate_date > prev) {
      latestDate.set(key, r.rate_date);
      map.set(key, rate);
    }
  }
  return map;
}

export interface BaseConversion {
  /** The line amount expressed in the account's base currency. */
  base: number;
  /** The applied from→base conversion factor. */
  rate: number;
}

// convertToBase expresses a foreign amount in the account's base currency.
// Same-currency conversion is the identity (rate 1). When only the inverse
// (base→from) rate is published it is reciprocated. Returns null when no
// rate can be found so the caller flags the line for manual FX handling
// rather than presenting a fabricated base figure.
export function convertToBase(
  amount: number,
  from: string | undefined,
  base: string | undefined,
  rates: RateMap,
): BaseConversion | null {
  const b = normCur(base);
  if (b === "") return null;
  const f = normCur(from) || b;
  if (f === b) return { base: amount, rate: 1 };
  const direct = rates.get(rateKey(f, b));
  if (direct !== undefined) return { base: amount * direct, rate: direct };
  const inverse = rates.get(rateKey(b, f));
  if (inverse !== undefined && inverse !== 0) {
    return { base: amount / inverse, rate: 1 / inverse };
  }
  return null;
}

// --- Filtering / search -------------------------------------------------

// matchesQuery is a case-insensitive substring test across a bank line's
// description, amount, date and currency, so an operator can narrow a long
// queue by payee, value or date fragment. An empty query matches every
// line.
export function matchesQuery(r: KRecord, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (q === "") return true;
  const d = txnData(r);
  const haystack = [
    d.description ?? "",
    String(d.amount ?? ""),
    d.value_date ?? "",
    d.currency ?? "",
  ]
    .join(" ")
    .toLowerCase();
  return haystack.includes(q);
}

// --- Split / partial matches -------------------------------------------

// splitRemaining is the running difference a split composer shows: the
// target (the bank line being split, or the ledger total being built up)
// minus the sum of the allocations entered so far. Positive means there is
// still an amount to allocate; negative means the split is over-allocated.
// Non-finite allocations contribute zero so a half-typed input never makes
// the whole figure NaN.
export function splitRemaining(target: number, allocations: number[]): number {
  const sum = allocations.reduce(
    (acc, n) => acc + (Number.isFinite(n) ? n : 0),
    0,
  );
  return target - sum;
}

// isBalanced is true when a running difference has netted to zero within
// the half-cent tolerance — the point at which a split may be reconciled.
export function isBalanced(
  remaining: number,
  tolerance: number = AMOUNT_TOLERANCE,
): boolean {
  return Math.abs(remaining) <= tolerance;
}

// --- Duplicate / reversal detection ------------------------------------

export interface TxnAnomalies {
  /** Lines that are byte-for-byte repeats of another line (date, amount,
   *  description, currency) — typically a statement imported twice. */
  duplicateIds: ReadonlySet<string>;
  /** Lines that have an equal-and-opposite counterpart with the same
   *  description and currency — a refund, bounced payment or correction
   *  the operator should reconcile as a pair, not match individually. */
  reversalIds: ReadonlySet<string>;
}

const EMPTY_ANOMALIES: TxnAnomalies = {
  duplicateIds: new Set(),
  reversalIds: new Set(),
};

// detectAnomalies flags the two error classes a bookkeeper most often hits
// on a freshly imported statement: duplicate rows (a re-imported file) and
// reversed pairs (a charge plus its refund). Both are computed in a single
// O(n) pass keyed on a normalised signature so it stays cheap on the large
// statements a busy account accumulates.
export function detectAnomalies(txns: KRecord[]): TxnAnomalies {
  if (txns.length === 0) return EMPTY_ANOMALIES;
  const duplicateIds = new Set<string>();
  const reversalIds = new Set<string>();

  // Exact-match buckets for duplicates.
  const exact = new Map<string, KRecord[]>();
  // |amount|-keyed buckets split by sign for reversals.
  const bySig = new Map<string, { pos: KRecord[]; neg: KRecord[] }>();

  for (const r of txns) {
    const d = txnData(r);
    const amount = txnAmount(r);
    const desc = (d.description ?? "").trim().toLowerCase();
    const cur = normCur(d.currency);
    const date = d.value_date ?? "";

    const exactKey = `${date}|${desc}|${amount}|${cur}`;
    const exactList = exact.get(exactKey) ?? [];
    exactList.push(r);
    exact.set(exactKey, exactList);

    if (amount !== 0) {
      const sig = `${cur}|${desc}|${Math.abs(amount).toFixed(2)}`;
      const slot = bySig.get(sig) ?? { pos: [], neg: [] };
      (amount > 0 ? slot.pos : slot.neg).push(r);
      bySig.set(sig, slot);
    }
  }

  for (const list of exact.values()) {
    if (list.length > 1) for (const r of list) duplicateIds.add(r.id);
  }
  for (const slot of bySig.values()) {
    if (slot.pos.length > 0 && slot.neg.length > 0) {
      for (const r of slot.pos) reversalIds.add(r.id);
      for (const r of slot.neg) reversalIds.add(r.id);
    }
  }
  return { duplicateIds, reversalIds };
}
