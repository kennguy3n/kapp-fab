import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Download } from "lucide-react";
import type { KRecord } from "@kapp/client";
import {
  Badge,
  type BadgeProps,
  Button,
  Eyebrow,
  Field,
  Select,
  StatCard,
  Table,
  TableBody,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
  toast,
} from "@kapp/ui";
import { api } from "../lib/api";
import { humanizeToken, recordLabel } from "../lib/ktypeView";
import { useFormatter } from "../lib/i18n";
import {
  csvFilename,
  downloadCsv,
  parseAmount,
  useMoney,
} from "../lib/finance/format";
import { FinanceError, TableSkeleton } from "../lib/finance/presentation";

type BadgeVariant = NonNullable<BadgeProps["variant"]>;

// Subledger documents carry their own lifecycle vocabulary (post/pay)
// that the generic status map doesn't cover, so map locally.
const SUBLEDGER_STATUS: Record<string, BadgeVariant> = {
  draft: "neutral",
  pending_approval: "warning",
  posted: "success",
  paid: "info",
  partially_paid: "info",
  cancelled: "outline",
  canceled: "outline",
};

// A document still owes money once it's posted and until it's fully
// paid — partially_paid rows carry a remaining balance, so both count
// toward outstanding. Drafts (awaiting posting), paid, and cancelled
// rows do not.
const OUTSTANDING_STATUSES = new Set(["posted", "partially_paid"]);

// A document has a journal entry to drill into once it's been posted to
// the ledger — posted, partially_paid, and paid all qualify. Drafts,
// pending approvals, and cancelled rows have no entry yet.
const POSTED_STATUSES = new Set(["posted", "partially_paid", "paid"]);

function statusOf(record: KRecord): string {
  return (record.data.status as string) ?? record.status ?? "draft";
}

function SubledgerStatus({ status }: { status: string }) {
  return (
    <Badge variant={SUBLEDGER_STATUS[status.toLowerCase()] ?? "neutral"}>
      {humanizeToken(status)}
    </Badge>
  );
}

/**
 * SubledgerPage renders the AR or AP subledger: the finance KRecords
 * (finance.ar_invoice / finance.ap_bill) with outstanding balances,
 * resolved counterparty names, posting status, a quick-post action for
 * drafts, and drill-through to the posted journal entry.
 */
export function SubledgerPage({ variant }: { variant: "ar" | "ap" }) {
  const qc = useQueryClient();
  const f = useFormatter();
  const money = useMoney();
  const [statusFilter, setStatusFilter] = useState<string>("all");

  const cfg = useMemo(
    () =>
      variant === "ar"
        ? {
            ktype: "finance.ar_invoice",
            area: "Accounts Receivable",
            title: "AR Subledger",
            numberField: "invoice_number",
            counterpartyField: "customer_id",
            counterpartyLabel: "Customer",
            counterpartyKtype: "crm.organization",
            numberLabel: "Invoice #",
            noun: "invoices",
            post: (id: string) => api.postInvoice(id),
          }
        : {
            ktype: "finance.ap_bill",
            area: "Accounts Payable",
            title: "AP Subledger",
            numberField: "bill_number",
            counterpartyField: "supplier_id",
            counterpartyLabel: "Supplier",
            counterpartyKtype: "crm.organization",
            numberLabel: "Bill #",
            noun: "bills",
            post: (id: string) => api.postBill(id),
          },
    [variant],
  );

  const records = useQuery({
    queryKey: ["subledger", cfg.ktype],
    queryFn: () => api.listRecords(cfg.ktype),
  });

  // Counterparties are stored as crm.organization ids; resolve them to
  // human names so the table never shows a raw UUID.
  const orgs = useQuery({
    queryKey: ["records", cfg.counterpartyKtype],
    queryFn: () => api.listRecords(cfg.counterpartyKtype),
  });
  const orgName = useMemo(() => {
    const map = new Map<string, string>();
    for (const o of orgs.data ?? []) map.set(o.id, recordLabel(o));
    return map;
  }, [orgs.data]);
  const resolveCounterparty = (id: string | undefined): string => {
    if (!id) return "—";
    return orgName.get(id) ?? (orgs.isLoading ? "…" : `Unknown ${cfg.counterpartyLabel.toLowerCase()}`);
  };

  const post = useMutation({
    mutationFn: (id: string) => cfg.post(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["subledger", cfg.ktype] });
      toast.success("Posted to the ledger");
    },
    onError: (err) =>
      toast.error("Couldn't post", {
        description: err instanceof Error ? err.message : undefined,
      }),
  });

  const allRows = useMemo(
    () =>
      (records.data ?? []).slice().sort((a, b) => {
        const ad = (a.data.due_date as string) ?? "";
        const bd = (b.data.due_date as string) ?? "";
        return ad < bd ? -1 : ad > bd ? 1 : 0;
      }),
    [records.data],
  );

  const statuses = useMemo(() => {
    const set = new Set<string>();
    for (const r of allRows) set.add(statusOf(r));
    return [...set].sort();
  }, [allRows]);

  const rows =
    statusFilter === "all"
      ? allRows
      : allRows.filter((r) => statusOf(r) === statusFilter);

  const totalOutstanding = allRows
    .filter((r) => OUTSTANDING_STATUSES.has(statusOf(r)))
    .reduce((sum, r) => sum + (parseAmount(r.data.total as string) || 0), 0);
  const draftCount = allRows.filter((r) => {
    const s = statusOf(r);
    return s === "draft" || s === "pending_approval";
  }).length;
  const outstandingCurrency =
    (allRows.find((r) => OUTSTANDING_STATUSES.has(statusOf(r)))?.data
      .currency as string) ??
    (allRows[0]?.data.currency as string) ??
    "USD";

  const exportCsv = () => {
    if (rows.length === 0) return;
    const data = rows.map((r) => [
      (r.data[cfg.numberField] as string) ?? "",
      resolveCounterparty(r.data[cfg.counterpartyField] as string | undefined),
      (r.data.due_date as string)?.slice(0, 10) ?? "",
      String(r.data.total ?? ""),
      (r.data.currency as string) ?? "",
      humanizeToken(statusOf(r)),
    ]);
    downloadCsv(
      csvFilename(`${variant}-subledger`),
      [cfg.numberLabel, cfg.counterpartyLabel, "Due", "Total", "Currency", "Status"],
      data,
    );
    toast.success(`${cfg.title} exported`);
  };

  return (
    <section className="flex flex-col gap-5">
      <header className="flex flex-col gap-4">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <Eyebrow>{cfg.area}</Eyebrow>
            <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
              {cfg.title}
            </h1>
            <p className="mt-1 text-sm text-fg-muted">
              {variant === "ar"
                ? "Sales invoices and drafts awaiting posting. Outstanding excludes paid and cancelled rows."
                : "Purchase bills and drafts awaiting posting. Outstanding excludes paid and cancelled rows."}
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <Button
              variant="outline"
              leadingIcon={<Download aria-hidden />}
              onClick={exportCsv}
              disabled={rows.length === 0}
            >
              Export CSV
            </Button>
          </div>
        </div>

        <div className="grid gap-3 sm:grid-cols-3">
          <StatCard label="Documents" value={f.number(allRows.length)} />
          <StatCard
            label="Outstanding"
            value={money(totalOutstanding, { currency: outstandingCurrency })}
            sub="Posted and partially paid"
          />
          <StatCard
            label="Awaiting posting"
            value={f.number(draftCount)}
            sub="Drafts and pending approval"
          />
        </div>

        {statuses.length > 1 && (
          <div className="max-w-xs">
            <Field label="Filter by status">
              <Select
                value={statusFilter}
                onChange={(e) => setStatusFilter(e.target.value)}
              >
                <option value="all">All statuses</option>
                {statuses.map((s) => (
                  <option key={s} value={s}>
                    {humanizeToken(s)}
                  </option>
                ))}
              </Select>
            </Field>
          </div>
        )}
      </header>

      {records.isLoading && <TableSkeleton columns={7} />}

      {records.isError && (
        <FinanceError
          title={`Couldn't load the ${cfg.title.toLowerCase()}`}
          error={records.error}
          onRetry={() => void records.refetch()}
        />
      )}

      {records.data && allRows.length === 0 && (
        <div className="rounded-lg border border-border p-8">
          <p className="text-center text-sm text-fg-muted">
            No {cfg.noun} yet. New {cfg.noun} will appear here once created.
          </p>
        </div>
      )}

      {records.data && allRows.length > 0 && rows.length === 0 && (
        <div className="rounded-lg border border-border p-8">
          <p className="text-center text-sm text-fg-muted">
            No {cfg.noun} with status “{humanizeToken(statusFilter)}”.
          </p>
        </div>
      )}

      {rows.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-32">{cfg.numberLabel}</TableHead>
              <TableHead>{cfg.counterpartyLabel}</TableHead>
              <TableHead>Due</TableHead>
              <TableHead className="text-right">Total</TableHead>
              <TableHead>Status</TableHead>
              <TableHead>Journal entry</TableHead>
              <TableHead className="text-right">Actions</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((r) => {
              const status = statusOf(r);
              const number = (r.data[cfg.numberField] as string) || "—";
              const dueDate = r.data.due_date as string | undefined;
              const currency = (r.data.currency as string) ?? "USD";
              const canPost = status === "draft" || status === "pending_approval";
              const isPosted = POSTED_STATUSES.has(status);
              return (
                <TableRow key={r.id}>
                  <TableCell className="font-medium text-fg">{number}</TableCell>
                  <TableCell>
                    {resolveCounterparty(
                      r.data[cfg.counterpartyField] as string | undefined,
                    )}
                  </TableCell>
                  <TableCell className="text-fg-muted">
                    {dueDate ? f.date(new Date(dueDate)) : "—"}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {money(r.data.total as string, { currency })}
                  </TableCell>
                  <TableCell>
                    <SubledgerStatus status={status} />
                  </TableCell>
                  <TableCell>
                    {isPosted ? (
                      <Link
                        to={`/finance/journal?source_ktype=${encodeURIComponent(cfg.ktype)}&source_id=${encodeURIComponent(r.id)}`}
                        className="text-xs text-accent hover:underline focus-visible:underline focus-visible:outline-none"
                      >
                        View entry
                      </Link>
                    ) : (
                      <span className="text-fg-subtle">—</span>
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    {canPost && (
                      <Button
                        size="sm"
                        variant="outline"
                        disabled={post.isPending}
                        onClick={() => post.mutate(r.id)}
                      >
                        Post
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
          <TableFooter>
            <TableRow>
              <TableCell colSpan={3} className="font-semibold">
                Outstanding
              </TableCell>
              <TableCell className="text-right font-semibold tabular-nums">
                {money(totalOutstanding, { currency: outstandingCurrency })}
              </TableCell>
              <TableCell colSpan={3} />
            </TableRow>
          </TableFooter>
        </Table>
      )}
    </section>
  );
}
