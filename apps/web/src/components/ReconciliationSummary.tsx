import { Badge, StatCard } from "@kapp/ui";
import { useFormatter } from "../lib/i18n/useFormatter";
import { formatMoney, type ReconTotals } from "./reconciliation";
import { rt } from "./ReconciliationStrings";

/**
 * ReconciliationSummary renders the running reconciliation tallies for the
 * selected account — the matched / unmatched / difference signals an
 * operator scans before working the queue, mirroring Xero's reconciliation
 * report header. Counts are the headline figure; the money still in play
 * sits underneath so the operator sees both "how many" and "how much".
 */
export function ReconciliationSummary({
  totals,
  currency,
}: {
  totals: ReconTotals;
  currency?: string;
}) {
  const f = useFormatter();
  const settled = Math.abs(totals.difference) <= 1e-9;
  return (
    <div
      className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5"
      aria-label={rt("reconciliation.summary.label")}
    >
      <StatCard
        label={rt("reconciliation.summary.unmatched")}
        value={f.number(totals.unmatchedCount)}
        sub={`${formatMoney(f, totals.unmatchedAmount, currency)} ${rt(
          "reconciliation.summary.toReview",
        )}`}
      />
      <StatCard
        label={rt("reconciliation.summary.matched")}
        value={f.number(totals.matchedCount)}
        sub={`${formatMoney(f, totals.matchedAmount, currency)} ${rt(
          "reconciliation.summary.reconciled",
        )}`}
      />
      <StatCard
        label={rt("reconciliation.summary.transfers")}
        value={f.number(totals.transferCount)}
      />
      <StatCard
        label={rt("reconciliation.summary.ignored")}
        value={f.number(totals.ignoredCount)}
      />
      <StatCard
        label={rt("reconciliation.summary.difference")}
        value={formatMoney(f, totals.difference, currency)}
        sub={
          <Badge variant={settled ? "success" : "warning"} size="xs">
            {settled
              ? rt("reconciliation.summary.reconciled")
              : rt("reconciliation.summary.netDifference")}
          </Badge>
        }
      />
    </div>
  );
}
