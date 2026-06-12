import { useCallback, useMemo, useRef, useState } from "react";
import type { BankFeedSuggestion, KRecord } from "@kapp/client";
import { Badge, Button, Input, Select } from "@kapp/ui";
import {
  formatAmount,
  isBalanced,
  shortId,
  splitRemaining,
  txnAmount,
} from "./reconciliation";
import { rt } from "./ReconciliationStrings";

export interface SplitAllocation {
  suggestion: BankFeedSuggestion;
  amount: number;
}

interface AllocationRow {
  /** Stable local key for the React list. */
  key: string;
  /** Chosen candidate suggestion id (i.e. its ledger entry). */
  suggestionId: string;
  /** Raw text the operator typed — kept verbatim so a half-entered
   *  value ("", "-", "12.") doesn't get coerced and reset under them. */
  raw: string;
}

/**
 * ReconciliationSplitMatch is the partial / split-match composer: it lets
 * an operator allocate one bank line across several candidate ledger
 * entries, shows the running difference (line amount − allocations), and
 * only enables reconciliation once the split nets to zero (within the
 * half-cent tolerance). It consumes the same accept endpoint as a single
 * match — the parent accepts each chosen suggestion — so no backend change
 * is required; the value here is the balance-to-zero workflow Xero and
 * QuickBooks operators rely on for one-to-many clears.
 */
export function ReconciliationSplitMatch({
  txn,
  suggestions,
  currency,
  pending,
  onReconcile,
}: {
  txn: KRecord;
  suggestions: BankFeedSuggestion[];
  currency?: string;
  pending: boolean;
  onReconcile: (allocations: SplitAllocation[]) => void;
}) {
  const target = txnAmount(txn);
  // Per-instance row-key sequence, so keys are locally unique and stable
  // across test runs (a module-level counter would leak between instances).
  const seqRef = useRef(0);
  const newRow = useCallback((suggestionId = ""): AllocationRow => {
    seqRef.current += 1;
    return { key: `alloc-${seqRef.current}`, suggestionId, raw: "" };
  }, []);
  const [rows, setRows] = useState<AllocationRow[]>(() => [
    newRow(suggestions[0]?.id ?? ""),
  ]);

  const suggestionById = useMemo(() => {
    const m = new Map<string, BankFeedSuggestion>();
    for (const s of suggestions) m.set(s.id, s);
    return m;
  }, [suggestions]);

  const amounts = useMemo(
    () => rows.map((r) => Number(r.raw)),
    [rows],
  );
  const allocated = useMemo(
    () => amounts.reduce((acc, n) => acc + (Number.isFinite(n) ? n : 0), 0),
    [amounts],
  );
  const remaining = splitRemaining(target, amounts);
  const balanced = isBalanced(remaining);

  // A row is complete when it names a candidate and carries a finite,
  // non-zero amount. The reconcile gate needs at least one complete row,
  // a zero remaining, and no two rows pointing at the same ledger entry.
  const completeRows = rows.filter(
    (r) => r.suggestionId !== "" && Number.isFinite(Number(r.raw)) && Number(r.raw) !== 0,
  );
  const distinctEntries =
    new Set(completeRows.map((r) => r.suggestionId)).size ===
    completeRows.length;
  const canReconcile =
    !pending && balanced && completeRows.length >= 1 && distinctEntries;

  const setRow = (key: string, patch: Partial<AllocationRow>) =>
    setRows((prev) =>
      prev.map((r) => (r.key === key ? { ...r, ...patch } : r)),
    );
  const addRow = () => setRows((prev) => [...prev, newRow()]);
  const removeRow = (key: string) =>
    setRows((prev) =>
      prev.length <= 1 ? prev : prev.filter((r) => r.key !== key),
    );

  const handleReconcile = () => {
    if (!canReconcile) return;
    const allocations: SplitAllocation[] = [];
    for (const r of completeRows) {
      const s = suggestionById.get(r.suggestionId);
      if (s) allocations.push({ suggestion: s, amount: Number(r.raw) });
    }
    if (allocations.length > 0) onReconcile(allocations);
  };

  return (
    <div
      className="mt-2 rounded-md border border-border bg-bg p-3"
      aria-label={rt("reconciliation.split.title")}
    >
      <p className="text-xs text-fg-muted">{rt("reconciliation.split.intro")}</p>

      <div className="mt-2 flex flex-col gap-2">
        {rows.map((row) => (
          <div key={row.key} className="flex flex-wrap items-center gap-2">
            <Select
              aria-label="Ledger entry"
              className="min-w-[14rem] flex-1"
              value={row.suggestionId}
              disabled={pending}
              onChange={(e) => setRow(row.key, { suggestionId: e.target.value })}
            >
              <option value="">—</option>
              {suggestions.map((s) => (
                <option key={s.id} value={s.id}>
                  {`Journal entry ${shortId(s.journal_entry_id)}`}
                </option>
              ))}
            </Select>
            <Input
              type="number"
              inputMode="decimal"
              step="0.01"
              size="sm"
              className="w-32 text-right tabular-nums"
              aria-label={rt("reconciliation.split.allocated")}
              placeholder="0.00"
              value={row.raw}
              disabled={pending}
              onChange={(e) => setRow(row.key, { raw: e.target.value })}
            />
            <Button
              size="sm"
              variant="ghost"
              disabled={pending || rows.length <= 1}
              onClick={() => removeRow(row.key)}
            >
              {rt("reconciliation.split.remove")}
            </Button>
          </div>
        ))}
      </div>

      <Button
        size="sm"
        variant="outline"
        className="mt-2"
        disabled={pending}
        onClick={addRow}
      >
        {rt("reconciliation.split.add")}
      </Button>

      <dl className="mt-3 flex flex-wrap gap-x-6 gap-y-1 text-xs">
        <div className="flex gap-1">
          <dt className="text-fg-muted">{rt("reconciliation.split.target")}</dt>
          <dd className="font-semibold tabular-nums text-fg">
            {formatAmount(target, currency)}
          </dd>
        </div>
        <div className="flex gap-1">
          <dt className="text-fg-muted">
            {rt("reconciliation.split.allocated")}
          </dt>
          <dd className="font-semibold tabular-nums text-fg">
            {formatAmount(allocated, currency)}
          </dd>
        </div>
        <div className="flex gap-1">
          <dt className="text-fg-muted">
            {rt("reconciliation.split.remaining")}
          </dt>
          <dd
            className={`font-semibold tabular-nums ${
              balanced ? "text-success" : "text-accent"
            }`}
          >
            {formatAmount(remaining, currency)}
          </dd>
        </div>
      </dl>

      <div className="mt-2 flex flex-wrap items-center gap-2">
        <Button size="sm" disabled={!canReconcile} onClick={handleReconcile}>
          {rt("reconciliation.split.reconcile")}
        </Button>
        {balanced && !distinctEntries ? (
          <span className="text-xs text-danger">
            {rt("reconciliation.split.duplicateEntry")}
          </span>
        ) : balanced ? (
          <Badge variant="success" size="xs">
            {rt("reconciliation.split.balanced")}
          </Badge>
        ) : (
          <span className="text-xs text-fg-muted">
            {completeRows.length === 0
              ? rt("reconciliation.split.needTwo")
              : rt("reconciliation.split.unbalanced")}
          </span>
        )}
      </div>
    </div>
  );
}
