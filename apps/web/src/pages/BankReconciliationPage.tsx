import { useCallback, useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { BankFeedRule, BankFeedSuggestion, KRecord } from "@kapp/client";
import {
  Badge,
  Button,
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
  computeTotals,
  detectTransferPairs,
  groupSuggestions,
  highConfidenceSuggestions,
  isUnmatched,
  txnData,
  txnStatus,
} from "../components/reconciliation";
import { ReconciliationSummary } from "../components/ReconciliationSummary";
import { ReconciliationMatchQueue } from "../components/ReconciliationMatchQueue";
import { ReconciliationSideBySide } from "../components/ReconciliationSideBySide";
import { ReconciliationTransfers } from "../components/ReconciliationTransfers";
import { ReconciliationRulesPanel } from "../components/ReconciliationRulesPanel";

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

  const setPending = useCallback((id: string, on: boolean) => {
    setPendingIds((prev) => {
      const next = new Set(prev);
      if (on) next.add(id);
      else next.delete(id);
      return next;
    });
  }, []);

  const invalidateMatchData = useCallback(() => {
    qc.invalidateQueries({ queryKey: ["bankfeed", "suggestions", selected] });
    qc.invalidateQueries({ queryKey: ["records", KTYPE_TXN] });
  }, [qc, selected]);

  const updateTxn = useMutation({
    mutationFn: (r: KRecord) => api.updateRecord(KTYPE_TXN, r.id, r.data),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["records", KTYPE_TXN] }),
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

  const acceptAllHighConfidence = useCallback(async () => {
    if (highConfidence.length === 0) return;
    setBulkPending(true);
    let ok = 0;
    try {
      for (const s of highConfidence) {
        await api.acceptBankFeedSuggestion(s.id);
        ok += 1;
      }
      toast.success(`Accepted ${ok} high-confidence match${ok === 1 ? "" : "es"}`);
    } catch (e) {
      toast.error("Bulk accept stopped", {
        description: `${(e as Error).message} (accepted ${ok})`,
      });
    } finally {
      setBulkPending(false);
      invalidateMatchData();
    }
  }, [highConfidence, invalidateMatchData]);

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
          <p className="text-sm text-fg-muted">Loading…</p>
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
          <p className="text-sm text-fg-muted">Select a bank account.</p>
        ) : (
          <div className="flex flex-col gap-6">
            <ReconciliationSummary totals={totals} currency={selectedCurrency} />

            {suggestionsQ.isError && (
              <p className="text-sm text-danger">
                Could not load suggestions: {String(suggestionsQ.error)}
              </p>
            )}

            <ReconciliationMatchQueue
              groups={groups}
              txnById={txnById}
              pendingIds={pendingIds as Set<string>}
              highConfidenceCount={highConfidence.length}
              bulkPending={bulkPending}
              onAccept={(s) => acceptMut.mutate(s)}
              onReject={(s) => rejectMut.mutate(s)}
              onAcceptAllHighConfidence={acceptAllHighConfidence}
            />

            <ReconciliationSideBySide
              unmatched={unmatched}
              suggestionsByTxn={suggestionsByTxn}
              totals={totals}
              currency={selectedCurrency}
              selectedTxnId={activeLine}
              pendingIds={pendingIds as Set<string>}
              onSelectTxn={setActiveLine}
              onAccept={(s) => acceptMut.mutate(s)}
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
                      return (
                        <TableRow key={r.id}>
                          <TableCell>{d.value_date ?? ""}</TableCell>
                          <TableCell>{d.description ?? ""}</TableCell>
                          <TableCell className="text-right tabular-nums">
                            {d.amount ?? 0} {d.currency ?? ""}
                          </TableCell>
                          <TableCell>
                            <Badge
                              variant={STATUS_BADGE[status] ?? "outline"}
                              size="xs"
                            >
                              {status}
                            </Badge>
                          </TableCell>
                          <TableCell className="font-mono text-xs">
                            {d.matched_entry_id ?? "—"}
                          </TableCell>
                          <TableCell>
                            {isUnmatched(status) && (
                              <Button
                                size="sm"
                                variant="ghost"
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
                          </TableCell>
                        </TableRow>
                      );
                    })}
                    {visible.length === 0 && (
                      <TableRow>
                        <TableCell colSpan={6} className="text-fg-muted">
                          No transactions yet.
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
