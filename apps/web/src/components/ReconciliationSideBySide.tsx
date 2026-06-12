import type { BankFeedSuggestion, KRecord } from "@kapp/client";
import { Badge, Button, cn } from "@kapp/ui";
import {
  confidenceBand,
  formatAmount,
  formatConfidence,
  parseReasons,
  shortId,
  txnData,
  type ReconTotals,
} from "./reconciliation";

function BankLineButton({
  txn,
  active,
  candidateCount,
  onSelect,
}: {
  txn: KRecord;
  active: boolean;
  candidateCount: number;
  onSelect: (id: string) => void;
}) {
  const d = txnData(txn);
  const amount = typeof d.amount === "number" ? d.amount : Number(d.amount ?? 0);
  return (
    <li>
      <button
        type="button"
        onClick={() => onSelect(txn.id)}
        className={cn(
          "w-full rounded-md border border-border px-3 py-2 text-left transition-colors hover:bg-bg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)",
          active && "border-accent bg-accent/10",
        )}
        aria-pressed={active}
      >
        <div className="flex items-baseline justify-between gap-2">
          <span className="min-w-0 truncate font-medium text-fg">
            {d.description || "(no description)"}
          </span>
          <span className="font-semibold tabular-nums text-fg">
            {formatAmount(amount, d.currency)}
          </span>
        </div>
        <div className="flex items-center justify-between gap-2 text-xs text-fg-muted">
          <span>{d.value_date ?? ""}</span>
          {candidateCount > 0 ? (
            <Badge variant="outline" size="xs">
              {candidateCount} candidate{candidateCount === 1 ? "" : "s"}
            </Badge>
          ) : (
            <span>No candidates</span>
          )}
        </div>
      </button>
    </li>
  );
}

function CandidatePanel({
  suggestions,
  pendingIds,
  bulkPending,
  onAccept,
  hasSelection,
}: {
  suggestions: BankFeedSuggestion[];
  pendingIds: Set<string>;
  bulkPending: boolean;
  onAccept: (s: BankFeedSuggestion) => void;
  hasSelection: boolean;
}) {
  if (!hasSelection) {
    return (
      <p className="text-sm text-fg-muted">
        Select a bank line to see candidate ledger entries.
      </p>
    );
  }
  if (suggestions.length === 0) {
    return (
      <p className="text-sm text-fg-muted">
        No candidate ledger entries for this line.
      </p>
    );
  }
  return (
    <ul className="flex list-none flex-col gap-2 p-0">
      {suggestions.map((s) => {
        const band = confidenceBand(s.confidence);
        const reasons = parseReasons(s.match_reason);
        return (
          <li
            key={s.id}
            className="rounded-md border border-border bg-bg px-3 py-2"
          >
            <div className="flex items-center justify-between gap-2">
              <span
                className="font-mono text-xs text-fg-muted"
                title={s.journal_entry_id}
              >
                Journal entry {shortId(s.journal_entry_id)}
              </span>
              <Badge
                variant={
                  band === "high"
                    ? "success"
                    : band === "medium"
                      ? "warning"
                      : "default"
                }
              >
                {formatConfidence(s.confidence)}
              </Badge>
            </div>
            {reasons.length > 0 && (
              <div className="mt-1 flex flex-wrap gap-1">
                {reasons.map((r) => (
                  <Badge key={r} variant="outline" size="xs">
                    {r}
                  </Badge>
                ))}
              </div>
            )}
            <div className="mt-2">
              <Button
                size="sm"
                disabled={bulkPending || pendingIds.has(s.id)}
                onClick={() => onAccept(s)}
              >
                Match
              </Button>
            </div>
          </li>
        );
      })}
    </ul>
  );
}

/**
 * ReconciliationSideBySide is the focused two-pane reconciliation
 * workspace: unmatched bank lines on the left, the candidate ledger
 * entries for the selected line on the right, with the running matched /
 * unmatched / difference totals across the top — the layout Xero and
 * QuickBooks operators expect when clearing a statement.
 */
export function ReconciliationSideBySide({
  unmatched,
  suggestionsByTxn,
  totals,
  currency,
  selectedTxnId,
  pendingIds,
  bulkPending,
  onSelectTxn,
  onAccept,
}: {
  unmatched: KRecord[];
  suggestionsByTxn: Map<string, BankFeedSuggestion[]>;
  totals: ReconTotals;
  currency?: string;
  selectedTxnId: string | null;
  pendingIds: Set<string>;
  bulkPending: boolean;
  onSelectTxn: (id: string) => void;
  onAccept: (s: BankFeedSuggestion) => void;
}) {
  const selected =
    selectedTxnId && unmatched.some((t) => t.id === selectedTxnId)
      ? selectedTxnId
      : null;
  const candidates = selected ? (suggestionsByTxn.get(selected) ?? []) : [];

  return (
    <section className="flex flex-col gap-3" aria-label="Side-by-side reconciliation">
      <header className="flex flex-wrap items-center justify-between gap-2">
        <h2 className="text-base font-semibold text-fg">Side-by-side</h2>
        <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-fg-muted">
          <span>
            Matched{" "}
            <span className="font-semibold tabular-nums text-fg">
              {totals.matchedCount}
            </span>
          </span>
          <span>
            Unmatched{" "}
            <span className="font-semibold tabular-nums text-fg">
              {totals.unmatchedCount}
            </span>
          </span>
          <span>
            Difference{" "}
            <span className="font-semibold tabular-nums text-fg">
              {formatAmount(totals.difference, currency)}
            </span>
          </span>
        </div>
      </header>

      <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
        <div>
          <h3 className="mb-1 text-sm font-semibold text-fg">
            Unmatched bank lines
          </h3>
          {unmatched.length === 0 ? (
            <p className="text-sm text-fg-muted">
              Nothing left to reconcile.
            </p>
          ) : (
            <ul className="flex list-none flex-col gap-1 p-0">
              {unmatched.map((t) => (
                <BankLineButton
                  key={t.id}
                  txn={t}
                  active={selected === t.id}
                  candidateCount={(suggestionsByTxn.get(t.id) ?? []).length}
                  onSelect={onSelectTxn}
                />
              ))}
            </ul>
          )}
        </div>
        <div>
          <h3 className="mb-1 text-sm font-semibold text-fg">
            Candidate ledger entries
          </h3>
          <CandidatePanel
            suggestions={candidates}
            pendingIds={pendingIds}
            bulkPending={bulkPending}
            onAccept={onAccept}
            hasSelection={selected !== null}
          />
        </div>
      </div>
    </section>
  );
}
