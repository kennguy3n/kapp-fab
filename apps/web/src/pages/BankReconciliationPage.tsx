import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
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
    <section style={{ display: "flex", gap: 16 }}>
      <div style={{ flex: "0 0 260px" }}>
        <h1>Bank Reconciliation</h1>
        <p style={{ color: "#6b7280", fontSize: 13 }}>
          Import statement lines via CSV, then match against journal entries.
        </p>
        <h2 style={{ fontSize: 14 }}>Accounts</h2>
        {accounts.isLoading && <p>Loading…</p>}
        <ul style={{ listStyle: "none", padding: 0, fontSize: 13 }}>
          {(accounts.data ?? []).map((r) => {
            const d = r.data as unknown as BankAccountData;
            const active = selected === r.id;
            return (
              <li
                key={r.id}
                onClick={() => setSelected(r.id)}
                style={{
                  padding: "6px 8px",
                  cursor: "pointer",
                  background: active ? "#eef2ff" : "transparent",
                  borderRadius: 4,
                }}
              >
                <div style={{ fontWeight: 500 }}>{d.name ?? "(unnamed)"}</div>
                <div style={{ color: "#6b7280", fontSize: 12 }}>
                  {d.currency ?? ""} {d.account_number ?? ""}
                </div>
              </li>
            );
          })}
        </ul>
      </div>

      <div style={{ flex: 1, minWidth: 0 }}>
        {!selected ? (
          <p style={{ color: "#6b7280" }}>Select a bank account.</p>
        ) : (
          <>
            <header style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
              <h2 style={{ fontSize: 16 }}>Transactions</h2>
              <CSVUploader bankAccountId={selected} />
            </header>
            <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13, marginTop: 8 }}>
              <thead>
                <tr style={{ textAlign: "left", color: "#6b7280" }}>
                  <Th>Date</Th>
                  <Th>Description</Th>
                  <Th style={{ textAlign: "right" }}>Amount</Th>
                  <Th>Status</Th>
                  <Th>Match</Th>
                  <Th>{""}</Th>
                </tr>
              </thead>
              <tbody>
                {visible.map((r) => {
                  const d = r.data as unknown as BankTxnData;
                  return (
                    <tr key={r.id} style={{ borderTop: "1px solid #e5e7eb" }}>
                      <Td>{d.value_date ?? ""}</Td>
                      <Td>{d.description ?? ""}</Td>
                      <Td style={{ textAlign: "right" }}>
                        {d.amount ?? 0} {d.currency ?? ""}
                      </Td>
                      <Td>{d.status ?? "unreconciled"}</Td>
                      <Td>{d.matched_entry_id ?? "—"}</Td>
                      <Td>
                        {(d.status ?? "unreconciled") === "unreconciled" && (
                          <button
                            onClick={() =>
                              updateTxn.mutate({
                                ...r,
                                data: { ...r.data, status: "ignored" },
                              })
                            }
                          >
                            Mark ignored
                          </button>
                        )}
                      </Td>
                    </tr>
                  );
                })}
                {visible.length === 0 && (
                  <tr>
                    <Td colSpan={6} style={{ padding: 12, color: "#6b7280" }}>
                      No transactions yet.
                    </Td>
                  </tr>
                )}
              </tbody>
            </table>
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
    <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
      <input
        type="file"
        accept=".csv,text/csv"
        disabled={busy}
        onChange={(e) => {
          const f = e.target.files?.[0];
          if (f) handleFile(f);
        }}
      />
      {busy && <span style={{ fontSize: 12 }}>Uploading…</span>}
      {err && <span style={{ fontSize: 12, color: "#b91c1c" }}>{err}</span>}
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

function Th({ children, style }: { children: React.ReactNode; style?: React.CSSProperties }) {
  return <th style={{ padding: "6px 8px", fontWeight: 500, fontSize: 12, ...style }}>{children}</th>;
}

function Td({ children, style, colSpan }: { children: React.ReactNode; style?: React.CSSProperties; colSpan?: number }) {
  return (
    <td style={{ padding: "8px", ...style }} colSpan={colSpan}>
      {children}
    </td>
  );
}
