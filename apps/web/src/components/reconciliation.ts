import type { BankFeedSuggestion, KRecord } from "@kapp/client";

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
