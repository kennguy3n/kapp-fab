import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import {
  Button,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  cn,
} from "@kapp/ui";
import { api } from "../lib/api";

const KTYPE_ACCOUNT = "finance.bank_account";
const KTYPE_TXN = "finance.bank_transaction";

interface BankAccountData {
  name?: string;
  currency?: string;
  account_number?: string;
}

interface BankTxnData {
  bank_account_id?: string;
  value_date?: string;
  description?: string;
  amount?: number | string;
  currency?: string;
  status?: string;
  matched_entry_id?: string;
}

/**
 * BankReconciliationPage is the operator console for reconciling
 * imported bank statement lines against ledger journal entries. The
 * left panel is the list of bank accounts (finance.bank_account
 * KRecords); selecting one shows its transactions and lets the user
 * trigger the auto-matcher or mark a line ignored.
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

  const updateTxn = useMutation({
    mutationFn: (r: KRecord) => api.updateRecord(KTYPE_TXN, r.id, r.data),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["records", KTYPE_TXN] }),
  });

  const visible = useMemo(() => {
    if (!selected) return [];
    return (txns.data ?? []).filter(
      (r) => (r.data as unknown as BankTxnData).bank_account_id === selected
    );
  }, [txns.data, selected]);

  return (
    <section className="flex gap-4">
      <div className="flex-[0_0_260px]">
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          Bank Reconciliation
        </h1>
        <p className="mt-1 text-sm text-fg-muted">
          Import statement lines via CSV, then match against journal entries.
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
                  onClick={() => setSelected(r.id)}
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
          <>
            <header className="flex items-center justify-between">
              <h2 className="text-base font-semibold text-fg">Transactions</h2>
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
                    const d = r.data as unknown as BankTxnData;
                    return (
                      <TableRow key={r.id}>
                        <TableCell>{d.value_date ?? ""}</TableCell>
                        <TableCell>{d.description ?? ""}</TableCell>
                        <TableCell className="text-right">
                          {d.amount ?? 0} {d.currency ?? ""}
                        </TableCell>
                        <TableCell>{d.status ?? "unreconciled"}</TableCell>
                        <TableCell>{d.matched_entry_id ?? "—"}</TableCell>
                        <TableCell>
                          {(d.status ?? "unreconciled") ===
                            "unreconciled" && (
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
          </>
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
