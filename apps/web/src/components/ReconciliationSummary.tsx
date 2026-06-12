import { formatAmount, type ReconTotals } from "./reconciliation";

interface StatProps {
  label: string;
  value: string;
  hint?: string;
  tone?: "default" | "accent" | "warning";
}

function Stat({ label, value, hint, tone = "default" }: StatProps) {
  const valueTone =
    tone === "warning"
      ? "text-warning"
      : tone === "accent"
        ? "text-accent"
        : "text-fg";
  return (
    <div className="rounded-lg border border-border bg-bg-subtle px-3 py-2">
      <div className="text-xs font-medium uppercase tracking-wide text-fg-muted">
        {label}
      </div>
      <div className={`mt-0.5 text-lg font-semibold tabular-nums ${valueTone}`}>
        {value}
      </div>
      {hint && <div className="text-xs text-fg-muted">{hint}</div>}
    </div>
  );
}

/**
 * ReconciliationSummary renders the running reconciliation tallies for the
 * selected account — the matched / unmatched / difference signals an
 * operator scans before working the queue, mirroring Xero's reconciliation
 * report header.
 */
export function ReconciliationSummary({
  totals,
  currency,
}: {
  totals: ReconTotals;
  currency?: string;
}) {
  return (
    <div
      className="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-5"
      aria-label="Reconciliation summary"
    >
      <Stat
        label="Unmatched"
        value={String(totals.unmatchedCount)}
        hint={formatAmount(totals.unmatchedAmount, currency)}
        tone={totals.unmatchedCount > 0 ? "warning" : "default"}
      />
      <Stat
        label="Matched"
        value={String(totals.matchedCount)}
        hint={formatAmount(totals.matchedAmount, currency)}
      />
      <Stat label="Transfers" value={String(totals.transferCount)} />
      <Stat label="Ignored" value={String(totals.ignoredCount)} />
      <Stat
        label="Difference"
        value={formatAmount(totals.difference, currency)}
        tone={Math.abs(totals.difference) > 1e-9 ? "accent" : "default"}
      />
    </div>
  );
}
