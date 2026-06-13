import { useCallback, useEffect, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type {
  BankFeedRule,
  BankFeedSuggestion,
  ExchangeRate,
  KRecord,
} from "@kapp/client";
import {
  Badge,
  Button,
  Input,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  cn,
  toast,
} from "@kapp/ui";
import { api } from "../lib/api";
import {
  buildRateMap,
  computeTotals,
  convertToBase,
  detectAnomalies,
  detectTransferPairs,
  groupSuggestions,
  highConfidenceSuggestions,
  isForeignLine,
  isUnmatched,
  matchesQuery,
  txnData,
  txnStatus,
} from "../components/reconciliation";
import type { SplitAllocation } from "../components/ReconciliationSplitMatch";
import { ReconciliationSummary } from "../components/ReconciliationSummary";
import { ReconciliationMatchQueue } from "../components/ReconciliationMatchQueue";
import { ReconciliationSideBySide } from "../components/ReconciliationSideBySide";
import { ReconciliationTransfers } from "../components/ReconciliationTransfers";
import { ReconciliationRulesPanel } from "../components/ReconciliationRulesPanel";
import { rt, rtp } from "../components/ReconciliationStrings";

const KTYPE_ACCOUNT = "finance.bank_account";
const KTYPE_TXN = "finance.bank_transaction";

interface BankAccountData {
  name?: string;
  currency?: string;
  account_number?: string;
}

const STATUS_BADGE: Record<
  string,
  "default" | "success" | "warning" | "info" | "outline"
> = {
  matched: "success",
  transfer: "info",
  ignored: "default",
  unreconciled: "warning",
};

/**
 * BankReconciliationPage is the operator's reconciliation console. The
 * left rail lists the tenant's bank accounts; selecting one drives the
 * reconciliation surfaces for that account:
 *
 *   - a running matched / unmatched / difference summary;
 *   - the smart-matcher review queue (accept / reject / find-alternative
 *     per line, with the reasons each candidate was suggested) plus the
 *     accept-all-high-confidence bulk action;
 *   - a side-by-side workspace (unmatched bank lines vs candidate ledger
 *     entries);
 *   - the inter-account transfers the backend auto-paired;
 *   - the full transaction table with CSV import and mark-ignored;
 *   - the reconciliation rules that drive auto-matching.
 *
 * All match data comes from the bank-feeds HTTP surface
 * (/api/v1/finance/bank-feeds/*); the page never mutates ledger state
 * directly — accepting a suggestion is what reconciles a line.
 */
export function BankReconciliationPage() {
  const qc = useQueryClient();
  const accounts = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_ACCOUNT],
    queryFn: () => api.listRecords(KTYPE_ACCOUNT),
  });
  const txns = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_TXN],
    queryFn: () => api.listRecords(KTYPE_TXN),
  });
  const [selected, setSelected] = useState<string | null>(null);
  const [activeLine, setActiveLine] = useState<string | null>(null);
  const [pendingIds, setPendingIds] = useState<ReadonlySet<string>>(new Set());
  const [bulkPending, setBulkPending] = useState(false);
  const [query, setQuery] = useState("");
  // Snapshot of the bank lines a bulk accept reconciled, kept only until
  // the operator either undoes it or starts another batch — the basis for
  // the one-click "undo bulk" correction.
  const [lastBulk, setLastBulk] = useState<KRecord[] | null>(null);

  const suggestionsQ = useQuery<BankFeedSuggestion[]>({
    queryKey: ["bankfeed", "suggestions", selected],
    queryFn: () => api.listBankFeedSuggestions(selected as string),
    enabled: !!selected,
  });
  const rulesQ = useQuery<BankFeedRule[]>({
    queryKey: ["bankfeed", "rules"],
    queryFn: () => api.listBankFeedRules(),
    enabled: !!selected,
  });
  // Exchange rates power the foreign-currency base-equivalent display. The
  // list is small and tenant-scoped; we load it once an account is open
  // and reuse it across every foreign line.
  const ratesQ = useQuery<{ rates: ExchangeRate[] }>({
    queryKey: ["exchange-rates"],
    queryFn: () => api.listExchangeRates({ limit: 500 }),
    enabled: !!selected,
  });

  const setPending = useCallback((id: string, on: boolean) => {
    setPendingIds((prev) => {
      const next = new Set(prev);
      if (on) next.add(id);
      else next.delete(id);
      return next;
    });
  }, []);

  const invalidateMatchData = useCallback(() => {
    // Prefix match invalidates every account's suggestion cache, so a
    // mutation that settles after the operator switches accounts still
    // refreshes the account the suggestion belonged to.
    qc.invalidateQueries({ queryKey: ["bankfeed", "suggestions"] });
    qc.invalidateQueries({ queryKey: ["records", KTYPE_TXN] });
  }, [qc]);

  const updateTxn = useMutation({
    mutationFn: (r: KRecord) => api.updateRecord(KTYPE_TXN, r.id, r.data),
    onSuccess: () => invalidateMatchData(),
  });

  const acceptMut = useMutation({
    mutationFn: (s: BankFeedSuggestion) => api.acceptBankFeedSuggestion(s.id),
    onMutate: (s) => setPending(s.id, true),
    onSuccess: () => {
      toast.success("Line matched");
      invalidateMatchData();
    },
    onError: (e) =>
      toast.error("Accept failed", { description: (e as Error).message }),
    onSettled: (_d, _e, s) => setPending(s.id, false),
  });

  const rejectMut = useMutation({
    mutationFn: (s: BankFeedSuggestion) => api.rejectBankFeedSuggestion(s.id),
    onMutate: (s) => setPending(s.id, true),
    onSuccess: () => {
      toast.success("Suggestion rejected");
      invalidateMatchData();
    },
    onError: (e) =>
      toast.error("Reject failed", { description: (e as Error).message }),
    onSettled: (_d, _e, s) => setPending(s.id, false),
  });

  const suggestions = useMemo(
    () => suggestionsQ.data ?? [],
    [suggestionsQ.data],
  );
  const allTxns = useMemo(() => txns.data ?? [], [txns.data]);

  const visible = useMemo(() => {
    if (!selected) return [];
    return allTxns.filter((r) => txnData(r).bank_account_id === selected);
  }, [allTxns, selected]);

  const unmatched = useMemo(
    () => visible.filter((r) => isUnmatched(txnStatus(r))),
    [visible],
  );

  const txnById = useMemo(() => {
    const m = new Map<string, KRecord>();
    for (const r of allTxns) m.set(r.id, r);
    return m;
  }, [allTxns]);

  const groups = useMemo(() => groupSuggestions(suggestions), [suggestions]);

  const suggestionsByTxn = useMemo(() => {
    const m = new Map<string, BankFeedSuggestion[]>();
    for (const g of groups) m.set(g.transactionId, g.suggestions);
    return m;
  }, [groups]);
  const totals = useMemo(() => computeTotals(visible), [visible]);
  const highConfidence = useMemo(
    () => highConfidenceSuggestions(suggestions),
    [suggestions],
  );

  const accountName = useCallback(
    (id: string) => {
      const acct = (accounts.data ?? []).find((a) => a.id === id);
      return (acct?.data as BankAccountData | undefined)?.name ?? id;
    },
    [accounts.data],
  );

  const transferPairs = useMemo(() => {
    if (!selected) return [];
    const transfers = allTxns.filter(
      (r) => txnData(r).status === "transfer",
    );
    return detectTransferPairs(transfers, accountName).filter((p) => {
      const outAcct = p.out ? txnData(p.out.txn).bank_account_id : undefined;
      const inAcct = p.in ? txnData(p.in.txn).bank_account_id : undefined;
      return outAcct === selected || inAcct === selected;
    });
  }, [allTxns, selected, accountName]);

  const selectedCurrency = useMemo(() => {
    const acct = (accounts.data ?? []).find((a) => a.id === selected);
    return (acct?.data as BankAccountData | undefined)?.currency;
  }, [accounts.data, selected]);

  const rateMap = useMemo(
    () => buildRateMap(ratesQ.data?.rates ?? []),
    [ratesQ.data],
  );

  // Duplicate / reversed lines are flagged across the whole account so the
  // badges are stable regardless of the current filter.
  const anomalies = useMemo(() => detectAnomalies(visible), [visible]);

  // The search box narrows the unmatched-line surfaces (match queue +
  // side-by-side). Resolved lines stay in the totals and table so the
  // operator never loses the full picture while filtering work-in-progress.
  const filteredUnmatched = useMemo(
    () => unmatched.filter((r) => matchesQuery(r, query)),
    [unmatched, query],
  );
  const filteredGroups = useMemo(() => {
    if (query.trim() === "") return groups;
    return groups.filter((g) => {
      const txn = txnById.get(g.transactionId);
      return txn ? matchesQuery(txn, query) : false;
    });
  }, [groups, query, txnById]);

  // Clear transient per-account state when the operator switches accounts:
  // a stale filter or an undo snapshot from another account would be
  // confusing (and undoing it would touch the wrong tenant lines).
  useEffect(() => {
    setQuery("");
    setLastBulk(null);
  }, [selected]);

  // unmatchData returns the line's data reset to the open state, dropping
  // the matched ledger reference so a re-matched line starts clean.
  const unmatchData = useCallback((r: KRecord) => {
    const next = { ...(r.data as Record<string, unknown>) };
    delete next.matched_entry_id;
    next.status = "unreconciled";
    return next;
  }, []);

  const unmatchMut = useMutation({
    mutationFn: (r: KRecord) =>
      api.updateRecord(KTYPE_TXN, r.id, unmatchData(r)),
    onMutate: (r) => setPending(r.id, true),
    onSuccess: () => {
      toast.success(rt("reconciliation.unmatch.done"));
      invalidateMatchData();
    },
    onError: (e) =>
      toast.error(rt("reconciliation.unmatch.failed"), {
        description: (e as Error).message,
      }),
    onSettled: (_d, _e, r) => setPending(r.id, false),
  });

  const acceptAllHighConfidence = useCallback(async () => {
    if (highConfidence.length === 0) return;
    setBulkPending(true);
    // Capture each line as its accept actually succeeds, so undo covers
    // exactly what was reconciled — including a partial batch that stops on
    // an error midway (those lines would otherwise be stranded with no undo).
    const accepted: KRecord[] = [];
    let error: Error | null = null;
    try {
      for (const s of highConfidence) {
        await api.acceptBankFeedSuggestion(s.id);
        const rec = txnById.get(s.transaction_id);
        if (rec) accepted.push(rec);
      }
    } catch (e) {
      error = e as Error;
    } finally {
      setLastBulk(accepted.length > 0 ? accepted : null);
      setBulkPending(false);
      invalidateMatchData();
    }
    if (error) {
      toast.error("Bulk accept stopped", {
        description: `${error.message} (accepted ${accepted.length})`,
      });
    } else {
      toast.success(
        `Accepted ${accepted.length} high-confidence match${
          accepted.length === 1 ? "" : "es"
        }`,
      );
    }
  }, [highConfidence, txnById, invalidateMatchData]);

  // undoBulk reverts the most recent bulk accept by returning each matched
  // line to the open state. It re-reads the live record from the cache so
  // it never clobbers an edit the operator made after the batch.
  const undoBulk = useCallback(async () => {
    if (!lastBulk || lastBulk.length === 0) return;
    setBulkPending(true);
    let ok = 0;
    let failed = false;
    try {
      for (const snapshot of lastBulk) {
        const live = txnById.get(snapshot.id) ?? snapshot;
        await api.updateRecord(KTYPE_TXN, live.id, unmatchData(live));
        ok += 1;
      }
    } catch {
      failed = true;
    } finally {
      setBulkPending(false);
      setLastBulk(null);
      invalidateMatchData();
    }
    if (failed) {
      toast.error(rt("reconciliation.bulk.undoFailed"));
    } else {
      toast.success(
        rtp("reconciliation.bulk.undone", {
          count: ok,
          plural: ok === 1 ? "" : "s",
        }),
      );
    }
  }, [lastBulk, txnById, unmatchData, invalidateMatchData]);

  // Reconcile a split in a single accept-with-amount call: the server
  // re-validates the running difference (legs must net to the line amount),
  // persists each partial allocation, and clears the line atomically. The
  // composer's net-to-zero gate is UX-only — the ledger never trusts a
  // client balance. All legs share one transaction id (the line being
  // cleared); amounts go on the wire as exact decimal strings.
  const reconcileSplit = useCallback(
    async (allocations: SplitAllocation[]) => {
      // A split spans >=2 entries; refuse anything smaller here too, so the
      // page-level guard matches the composer, handler, and matcher invariant
      // (defense-in-depth — never put a sub-2-leg "split" on the wire).
      if (allocations.length < 2) return;
      const transactionId = allocations[0].suggestion.transaction_id;
      setBulkPending(true);
      try {
        await api.acceptBankFeedSplit(
          transactionId,
          allocations.map((a) => ({
            journal_entry_id: a.suggestion.journal_entry_id,
            amount: String(a.amount),
            suggestion_id: a.suggestion.id,
          })),
        );
        toast.success(
          rtp("reconciliation.split.done", { count: allocations.length }),
        );
      } catch (e) {
        toast.error(rt("reconciliation.split.failed"), {
          description: (e as Error).message,
        });
      } finally {
        setBulkPending(false);
        invalidateMatchData();
      }
    },
    [invalidateMatchData],
  );

  return (
    <section className="flex gap-4">
      <div className="flex-[0_0_260px]">
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          Bank Reconciliation
        </h1>
        <p className="mt-1 text-sm text-fg-muted">
          Review smart-matcher suggestions, clear lines side-by-side, and
          import statements via CSV.
        </p>
        <h2 className="mt-4 text-sm font-semibold text-fg">Accounts</h2>
        {accounts.isLoading && (
          <p className="text-sm text-fg-muted">{rt("reconciliation.loading")}</p>
        )}
        {accounts.isError && (
          <div className="flex flex-col items-start gap-1">
            <p className="text-sm text-danger">
              {rt("reconciliation.accounts.error")}
            </p>
            <Button size="sm" variant="outline" onClick={() => accounts.refetch()}>
              {rt("reconciliation.retry")}
            </Button>
          </div>
        )}
        {!accounts.isLoading &&
          !accounts.isError &&
          (accounts.data ?? []).length === 0 && (
            <p className="text-sm text-fg-muted">
              {rt("reconciliation.accounts.empty")}
            </p>
          )}
        <ul className="mt-2 flex list-none flex-col gap-1 p-0">
          {(accounts.data ?? []).map((r) => {
            const d = r.data as unknown as BankAccountData;
            const active = selected === r.id;
            return (
              <li key={r.id}>
                <button
                  type="button"
                  onClick={() => {
                    setSelected(r.id);
                    setActiveLine(null);
                  }}
                  className={cn(
                    "w-full rounded-md px-2 py-1.5 text-left text-sm transition-colors hover:bg-bg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring)",
                    active && "bg-accent/10",
                  )}
                >
                  <div className="font-medium text-fg">
                    {d.name ?? "(unnamed)"}
                  </div>
                  <div className="text-xs text-fg-muted">
                    {d.currency ?? ""} {d.account_number ?? ""}
                  </div>
                </button>
              </li>
            );
          })}
        </ul>
      </div>

      <div className="min-w-0 flex-1">
        {!selected ? (
          <p className="text-sm text-fg-muted">
            {rt("reconciliation.selectAccount")}
          </p>
        ) : (
          <div className="flex flex-col gap-6">
            <ReconciliationSummary totals={totals} currency={selectedCurrency} />

            {txns.isError && (
              <div className="flex items-center gap-2">
                <p className="text-sm text-danger">
                  {rt("reconciliation.txns.error")}
                </p>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => txns.refetch()}
                >
                  {rt("reconciliation.retry")}
                </Button>
              </div>
            )}

            {lastBulk && lastBulk.length > 0 && (
              <div
                role="status"
                className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-border bg-bg-subtle px-3 py-2"
              >
                <span className="text-sm text-fg">
                  {rtp("reconciliation.bulk.recent", {
                    count: lastBulk.length,
                    plural: lastBulk.length === 1 ? "" : "s",
                  })}
                </span>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={bulkPending}
                  onClick={undoBulk}
                >
                  {rt("reconciliation.bulk.undo")}
                </Button>
              </div>
            )}

            <div className="flex flex-col gap-1">
              <label
                htmlFor="recon-filter"
                className="text-xs font-medium text-fg-muted"
              >
                {rt("reconciliation.search.label")}
              </label>
              <Input
                id="recon-filter"
                type="search"
                size="sm"
                className="max-w-sm"
                placeholder={rt("reconciliation.search.placeholder")}
                value={query}
                onChange={(e) => setQuery(e.target.value)}
              />
              {query.trim() !== "" &&
                unmatched.length > 0 &&
                filteredUnmatched.length === 0 && (
                  <p className="text-xs text-fg-muted">
                    {rtp("reconciliation.search.noMatches", { query })}
                  </p>
                )}
            </div>

            {suggestionsQ.isError && (
              <div className="flex items-center gap-2">
                <p className="text-sm text-danger">
                  {rt("reconciliation.suggestions.error")}
                </p>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => suggestionsQ.refetch()}
                >
                  {rt("reconciliation.retry")}
                </Button>
              </div>
            )}

            <ReconciliationMatchQueue
              groups={filteredGroups}
              txnById={txnById}
              baseCurrency={selectedCurrency}
              rates={rateMap}
              pendingIds={pendingIds as Set<string>}
              highConfidenceCount={highConfidence.length}
              bulkPending={bulkPending}
              onAccept={(s) => acceptMut.mutate(s)}
              onReject={(s) => rejectMut.mutate(s)}
              onAcceptAllHighConfidence={acceptAllHighConfidence}
            />

            <ReconciliationSideBySide
              unmatched={filteredUnmatched}
              suggestionsByTxn={suggestionsByTxn}
              totals={totals}
              currency={selectedCurrency}
              rates={rateMap}
              selectedTxnId={activeLine}
              pendingIds={pendingIds as Set<string>}
              bulkPending={bulkPending}
              onSelectTxn={setActiveLine}
              onAccept={(s) => acceptMut.mutate(s)}
              onSplitReconcile={reconcileSplit}
            />

            <ReconciliationTransfers pairs={transferPairs} />

            <div>
              <header className="flex items-center justify-between">
                <h2 className="text-base font-semibold text-fg">
                  Transactions
                </h2>
                <CSVUploader bankAccountId={selected} />
              </header>
              <div className="mt-2">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Date</TableHead>
                      <TableHead>Description</TableHead>
                      <TableHead className="text-right">Amount</TableHead>
                      <TableHead>Status</TableHead>
                      <TableHead>Match</TableHead>
                      <TableHead>
                        <span className="sr-only">Actions</span>
                      </TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {visible.map((r) => {
                      const d = txnData(r);
                      const status = txnStatus(r);
                      const amount = Number(d.amount ?? 0);
                      const foreign = isForeignLine(d.currency, selectedCurrency);
                      const conv = foreign
                        ? convertToBase(
                            Number.isFinite(amount) ? amount : 0,
                            d.currency,
                            selectedCurrency,
                            rateMap,
                          )
                        : null;
                      const isDup = anomalies.duplicateIds.has(r.id);
                      const isRev = anomalies.reversalIds.has(r.id);
                      return (
                        <TableRow key={r.id}>
                          <TableCell>{d.value_date ?? ""}</TableCell>
                          <TableCell>{d.description ?? ""}</TableCell>
                          <TableCell className="text-right tabular-nums">
                            <div>
                              {d.amount ?? 0} {d.currency ?? ""}
                            </div>
                            {foreign && (
                              <div className="text-xs text-fg-muted">
                                {conv
                                  ? `≈ ${conv.base.toLocaleString(undefined, {
                                      minimumFractionDigits: 2,
                                      maximumFractionDigits: 2,
                                    })} ${selectedCurrency ?? ""}`
                                  : rt("reconciliation.fx.noRate")}
                              </div>
                            )}
                          </TableCell>
                          <TableCell>
                            <div className="flex flex-wrap items-center gap-1">
                              <Badge
                                variant={STATUS_BADGE[status] ?? "outline"}
                                size="xs"
                              >
                                {status}
                              </Badge>
                              {isDup && (
                                <Badge
                                  variant="warning"
                                  size="xs"
                                  title={rt("reconciliation.flag.duplicate.hint")}
                                >
                                  {rt("reconciliation.flag.duplicate")}
                                </Badge>
                              )}
                              {isRev && (
                                <Badge
                                  variant="warning"
                                  size="xs"
                                  title={rt("reconciliation.flag.reversal.hint")}
                                >
                                  {rt("reconciliation.flag.reversal")}
                                </Badge>
                              )}
                            </div>
                          </TableCell>
                          <TableCell className="font-mono text-xs">
                            {d.matched_entry_id ?? "—"}
                          </TableCell>
                          <TableCell>
                            {isUnmatched(status) && (
                              <Button
                                size="sm"
                                variant="ghost"
                                disabled={pendingIds.has(r.id)}
                                onClick={() =>
                                  updateTxn.mutate({
                                    ...r,
                                    data: { ...r.data, status: "ignored" },
                                  })
                                }
                              >
                                Mark ignored
                              </Button>
                            )}
                            {status === "matched" && (
                              <Button
                                size="sm"
                                variant="ghost"
                                disabled={pendingIds.has(r.id)}
                                onClick={() => unmatchMut.mutate(r)}
                              >
                                {rt("reconciliation.unmatch")}
                              </Button>
                            )}
                          </TableCell>
                        </TableRow>
                      );
                    })}
                    {visible.length === 0 && (
                      <TableRow>
                        <TableCell colSpan={6} className="text-fg-muted">
                          {rt("reconciliation.txns.empty")}
                        </TableCell>
                      </TableRow>
                    )}
                  </TableBody>
                </Table>
              </div>
            </div>

            <ReconciliationRulesPanel
              rules={rulesQ.data ?? []}
              isLoading={rulesQ.isLoading}
              isError={rulesQ.isError}
              error={rulesQ.error}
            />
          </div>
        )}
      </div>
    </section>
  );
}

// CSVUploader parses a simple CSV client-side and creates individual
// bank_transaction KRecords. Simpler than a dedicated backend route —
// the server enforces schema validation per record.
function CSVUploader({ bankAccountId }: { bankAccountId: string }) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const handleFile = async (file: File) => {
    setBusy(true);
    setErr(null);
    try {
      const text = await file.text();
      const rows = parseCSV(text);
      for (const row of rows) {
        await api.createRecord(KTYPE_TXN, {
          bank_account_id: bankAccountId,
          value_date: row.value_date,
          description: row.description,
          amount: Number(row.amount),
          currency: row.currency || "USD",
          status: "unreconciled",
        });
      }
      qc.invalidateQueries({ queryKey: ["records", KTYPE_TXN] });
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex items-center gap-2">
      <input
        type="file"
        accept=".csv,text/csv"
        disabled={busy}
        onChange={(e) => {
          const f = e.target.files?.[0];
          if (f) handleFile(f);
        }}
        className="text-sm text-fg-muted file:mr-2 file:rounded-md file:border file:border-border file:bg-bg-subtle file:px-3 file:py-1.5 file:text-sm file:font-medium file:text-fg hover:file:bg-bg-muted"
      />
      {busy && <span className="text-xs text-fg-muted">Uploading…</span>}
      {err && <span className="text-xs text-danger">{err}</span>}
    </div>
  );
}

interface CSVRow {
  value_date: string;
  description: string;
  amount: string;
  currency: string;
}

// parseCSV handles a header row of [value_date, description, amount,
// currency] (order enforced). Quoting is not supported — this matches
// the minimal statement shape the Go helper accepts.
function parseCSV(text: string): CSVRow[] {
  const lines = text.split(/\r?\n/).filter((l) => l.trim() !== "");
  if (lines.length < 2) return [];
  const header = lines[0].split(",").map((s) => s.trim().toLowerCase());
  const idx = (k: string): number => header.indexOf(k);
  const vi = idx("value_date");
  const di = idx("description");
  const ai = idx("amount");
  const ci = idx("currency");
  if (vi < 0 || di < 0 || ai < 0) {
    throw new Error("CSV must have value_date, description, amount columns");
  }
  const out: CSVRow[] = [];
  for (let i = 1; i < lines.length; i++) {
    const cells = lines[i].split(",");
    out.push({
      value_date: cells[vi]?.trim() ?? "",
      description: cells[di]?.trim() ?? "",
      amount: cells[ai]?.trim() ?? "0",
      currency: (ci >= 0 ? cells[ci]?.trim() : "") || "USD",
    });
  }
  return out;
}
