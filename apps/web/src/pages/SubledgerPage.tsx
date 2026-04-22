import { useMemo } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { api } from "../lib/api";

/**
 * SubledgerPage renders the AR or AP subledger as a list of the
 * corresponding finance KRecords (finance.ar_invoice / finance.ap_bill)
 * with outstanding balances, posting status, and a quick-post button
 * for draft rows. It intentionally lives outside the generic
 * RecordListPage because the subledger is a cross-cutting finance view
 * — it filters by status, highlights unpaid balances, and offers a
 * workflow action (post) that RecordListPage doesn't know about.
 */
export function SubledgerPage({ variant }: { variant: "ar" | "ap" }) {
  const qc = useQueryClient();

  const cfg = useMemo(
    () =>
      variant === "ar"
        ? {
            ktype: "finance.ar_invoice",
            title: "AR Subledger",
            numberField: "invoice_number",
            counterpartyField: "customer_id",
            counterpartyLabel: "Customer",
            numberLabel: "Invoice #",
            post: (id: string) => api.postInvoice(id),
          }
        : {
            ktype: "finance.ap_bill",
            title: "AP Subledger",
            numberField: "bill_number",
            counterpartyField: "supplier_id",
            counterpartyLabel: "Supplier",
            numberLabel: "Bill #",
            post: (id: string) => api.postBill(id),
          },
    [variant],
  );

  const records = useQuery({
    queryKey: ["subledger", cfg.ktype],
    queryFn: () => api.listRecords(cfg.ktype),
  });

  const post = useMutation({
    mutationFn: (id: string) => cfg.post(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["subledger", cfg.ktype] });
    },
  });

  const rows = (records.data ?? []).slice().sort((a, b) => {
    const ad = (a.data.due_date as string) ?? "";
    const bd = (b.data.due_date as string) ?? "";
    return ad < bd ? -1 : ad > bd ? 1 : 0;
  });

  const totalOutstanding = rows
    .filter((r) => statusOf(r) === "posted")
    .reduce((sum, r) => sum + Number(r.data.total ?? 0), 0);

  return (
    <section>
      <h1>{cfg.title}</h1>
      <p style={{ color: "#6b7280" }}>
        {variant === "ar"
          ? "Posted sales invoices and drafts awaiting post. Outstanding totals exclude cancelled and paid rows."
          : "Posted purchase bills and drafts awaiting post. Outstanding totals exclude cancelled and paid rows."}
      </p>

      <div
        style={{
          display: "flex",
          gap: 24,
          marginBottom: 12,
          fontSize: 13,
          color: "#374151",
        }}
      >
        <Metric label="Rows" value={String(rows.length)} />
        <Metric
          label="Outstanding"
          value={totalOutstanding.toFixed(2)}
          hint="Sum of posted totals"
        />
      </div>

      {records.isLoading && <p>Loading…</p>}
      {records.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load subledger: {(records.error as Error).message}
        </p>
      )}

      {records.data && rows.length === 0 && (
        <p style={{ color: "#9ca3af", fontStyle: "italic" }}>
          No {variant === "ar" ? "invoices" : "bills"} yet.
        </p>
      )}

      {rows.length > 0 && (
        <table
          style={{
            width: "100%",
            borderCollapse: "collapse",
            marginTop: 12,
            fontSize: 13,
          }}
        >
          <thead>
            <tr style={{ textAlign: "left", color: "#6b7280" }}>
              <Th>{cfg.numberLabel}</Th>
              <Th>{cfg.counterpartyLabel}</Th>
              <Th>Due</Th>
              <Th>Total</Th>
              <Th>Status</Th>
              <Th>Journal Entry</Th>
              <Th>Actions</Th>
            </tr>
          </thead>
          <tbody>
            {rows.map((r) => (
              <SubledgerRow
                key={r.id}
                record={r}
                numberField={cfg.numberField}
                counterpartyField={cfg.counterpartyField}
                pending={post.isPending}
                onPost={() => post.mutate(r.id)}
              />
            ))}
          </tbody>
        </table>
      )}

      {post.isError && (
        <p style={{ color: "#b91c1c" }}>
          Post failed: {(post.error as Error).message}
        </p>
      )}
    </section>
  );
}

function SubledgerRow({
  record,
  numberField,
  counterpartyField,
  pending,
  onPost,
}: {
  record: KRecord;
  numberField: string;
  counterpartyField: string;
  pending: boolean;
  onPost: () => void;
}) {
  const status = statusOf(record);
  const number = (record.data[numberField] as string) ?? record.id.slice(0, 8);
  const counterparty = (record.data[counterpartyField] as string) ?? "—";
  const journalID = (record.data.journal_entry_id as string) ?? "";
  const dueDate = (record.data.due_date as string) ?? "";
  const total = Number(record.data.total ?? 0).toFixed(2);
  const currency = (record.data.currency as string) ?? "USD";
  const canPost = status === "draft" || status === "pending_approval";

  return (
    <tr style={{ borderTop: "1px solid #e5e7eb" }}>
      <Td>{number}</Td>
      <Td>
        <code>{truncateID(counterparty)}</code>
      </Td>
      <Td>{dueDate || "—"}</Td>
      <Td>
        {total} {currency}
      </Td>
      <Td>
        <StatusBadge status={status} />
      </Td>
      <Td>{journalID ? <code>{truncateID(journalID)}</code> : "—"}</Td>
      <Td>
        {canPost && (
          <button disabled={pending} onClick={onPost}>
            Post
          </button>
        )}
      </Td>
    </tr>
  );
}

function statusOf(record: KRecord): string {
  return (record.data.status as string) ?? record.status ?? "draft";
}

function truncateID(id: string): string {
  if (id.length <= 8) return id;
  return id.slice(0, 8);
}

function Metric({
  label,
  value,
  hint,
}: {
  label: string;
  value: string;
  hint?: string;
}) {
  return (
    <div>
      <div style={{ fontSize: 11, textTransform: "uppercase", color: "#6b7280" }}>
        {label}
      </div>
      <div style={{ fontSize: 16, fontWeight: 600 }}>{value}</div>
      {hint && <div style={{ fontSize: 11, color: "#9ca3af" }}>{hint}</div>}
    </div>
  );
}

function StatusBadge({ status }: { status: string }) {
  const color =
    status === "posted"
      ? "#059669"
      : status === "paid"
        ? "#2563eb"
        : status === "cancelled"
          ? "#9ca3af"
          : status === "pending_approval"
            ? "#d97706"
            : "#6b7280";
  return <span style={{ color, fontWeight: 500 }}>{status}</span>;
}

function Th({ children }: { children: React.ReactNode }) {
  return (
    <th style={{ padding: "6px 8px", fontWeight: 500, fontSize: 12 }}>
      {children}
    </th>
  );
}

function Td({ children }: { children: React.ReactNode }) {
  return <td style={{ padding: "8px" }}>{children}</td>;
}
