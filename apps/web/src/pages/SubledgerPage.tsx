import { useMemo } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import {
  Badge,
  Button,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
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
      <p className="text-fg-muted">
        {variant === "ar"
          ? "Posted sales invoices and drafts awaiting post. Outstanding totals exclude cancelled and paid rows."
          : "Posted purchase bills and drafts awaiting post. Outstanding totals exclude cancelled and paid rows."}
      </p>

      <div className="mb-3 flex gap-6 text-[13px] text-fg">
        <Metric label="Rows" value={String(rows.length)} />
        <Metric
          label="Outstanding"
          value={totalOutstanding.toFixed(2)}
          hint="Sum of posted totals"
        />
      </div>

      {records.isLoading && <p>Loading…</p>}
      {records.isError && (
        <p className="text-danger">
          Failed to load subledger: {(records.error as Error).message}
        </p>
      )}

      {records.data && rows.length === 0 && (
        <p className="italic text-fg-subtle">
          No {variant === "ar" ? "invoices" : "bills"} yet.
        </p>
      )}

      {rows.length > 0 && (
        <Table className="mt-3 text-[13px]">
          <TableHeader>
            <TableRow>
              <TableHead>{cfg.numberLabel}</TableHead>
              <TableHead>{cfg.counterpartyLabel}</TableHead>
              <TableHead>Due</TableHead>
              <TableHead>Total</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Journal Entry</TableHead>
              <TableHead>Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
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
          </TableBody>
        </Table>
      )}

      {post.isError && (
        <p className="text-danger">
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
    <TableRow>
      <TableCell>{number}</TableCell>
      <TableCell>
        <code>{truncateID(counterparty)}</code>
      </TableCell>
      <TableCell>{dueDate || "—"}</TableCell>
      <TableCell>
        {total} {currency}
      </TableCell>
      <TableCell>
        <StatusBadge status={status} />
      </TableCell>
      <TableCell>{journalID ? <code>{truncateID(journalID)}</code> : "—"}</TableCell>
      <TableCell>
        {canPost && (
          <Button size="sm" variant="outline" disabled={pending} onClick={onPost}>
            Post
          </Button>
        )}
      </TableCell>
    </TableRow>
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
      <div className="text-[11px] uppercase text-fg-muted">
        {label}
      </div>
      <div className="text-base font-semibold">{value}</div>
      {hint && <div className="text-[11px] text-fg-subtle">{hint}</div>}
    </div>
  );
}

function StatusBadge({ status }: { status: string }) {
  const variant =
    status === "posted"
      ? "success"
      : status === "paid"
        ? "info"
        : status === "cancelled"
          ? "outline"
          : status === "pending_approval"
            ? "warning"
            : "default";
  return <Badge variant={variant}>{status}</Badge>;
}
