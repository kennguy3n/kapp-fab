import { useMemo, useState, type Key } from "react";
import {
  useMutation,
  useQueries,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { Link } from "react-router-dom";
import type { Approval, KRecord } from "@kapp/client";
import {
  Badge,
  Button,
  ConfirmDialog,
  DataGrid,
  EmptyState,
  Eyebrow,
  Skeleton,
  Tabs,
  TabsContent,
  TabsList,
  TabsTrigger,
  Tooltip,
  TooltipContent,
  TooltipTrigger,
  toast,
  type DataGridColumn,
} from "@kapp/ui";
import { AlertTriangle, Check, Inbox, X } from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n";
import { ktypeSingular, recordLabel, statusVariant } from "../lib/ktypeView";

type Formatter = ReturnType<typeof useFormatter>;

// Money is stored under different keys across the record types an
// approval can target (invoices use `total`, expense claims `amount`,
// …). We probe a short, ordered list and surface the first numeric
// match — never fabricating a figure the linked record doesn't carry.
const MONEY_KEYS = [
  "total",
  "total_amount",
  "grand_total",
  "amount",
  "net_amount",
  "subtotal",
  "value",
];

function recordAmount(
  record: KRecord,
): { value: number; currency?: string } | null {
  for (const key of MONEY_KEYS) {
    const raw = record.data[key];
    if (typeof raw === "number" && Number.isFinite(raw)) {
      const currency = record.data["currency"];
      return {
        value: raw,
        currency: typeof currency === "string" ? currency : undefined,
      };
    }
  }
  return null;
}

function ageLabel(iso: string, fmt: Formatter): string {
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "—";
  const diffMs = then - Date.now();
  // Round by magnitude then re-apply the sign: JS's Math.round(-1.5) === -1
  // would otherwise report a 90-minute-old request as "1 hour ago".
  const sign = diffMs < 0 ? -1 : 1;
  const absMs = Math.abs(diffMs);
  const minutes = Math.round(absMs / 60_000);
  if (minutes < 60) return fmt.relativeTime(sign * minutes, "minute");
  const hours = Math.round(absMs / 3_600_000);
  if (hours < 24) return fmt.relativeTime(sign * hours, "hour");
  return fmt.relativeTime(sign * Math.round(absMs / 86_400_000), "day");
}

interface ApprovalRowView {
  id: string;
  subject: string;
  typeLabel: string;
  href: string;
  requester: string;
  amountText: string;
  amountValue: number | null;
  createdAt: string;
  step: string;
  state: Approval["state"];
  isPending: boolean;
}

/**
 * ApprovalsPage is the decision queue: a scannable list of what's
 * waiting, who asked, the subject + amount it concerns, and how long
 * it has been sitting — with one-click Approve / Reject and bulk
 * approve. The linked record and requester are resolved to human
 * labels so the queue never shows a raw ktype id or UUID.
 *
 * Note: the decide endpoint accepts only the decision (approve /
 * reject) — there is no field to carry a rejection reason — so Reject
 * asks for an explicit confirmation rather than collecting a comment
 * the API would silently drop.
 */
export function ApprovalsPage() {
  const qc = useQueryClient();
  const fmt = useFormatter();

  const approvals = useQuery({
    queryKey: ["approvals"],
    queryFn: () => api.listApprovals(),
  });

  // Resolve the distinct record types an approval points at so the
  // subject + amount come from the real linked record.
  const targetKtypes = useMemo(() => {
    const set = new Set<string>();
    for (const a of approvals.data ?? []) set.add(a.record_ktype);
    return [...set].sort();
  }, [approvals.data]);

  const recordQueries = useQueries({
    queries: targetKtypes.map((ktype) => ({
      queryKey: ["records", ktype],
      queryFn: () => api.listRecords(ktype),
      staleTime: 60_000,
    })),
  });

  const recordsByKtype = useMemo(() => {
    const map = new Map<string, Map<string, KRecord>>();
    targetKtypes.forEach((ktype, i) => {
      const inner = new Map<string, KRecord>();
      for (const record of recordQueries[i]?.data ?? []) inner.set(record.id, record);
      map.set(ktype, inner);
    });
    return map;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [targetKtypes, recordQueries.map((r) => r.dataUpdatedAt).join(",")]);

  const employees = useQuery({
    queryKey: ["records", "hr.employee"],
    queryFn: () => api.listRecords("hr.employee"),
    staleTime: 60_000,
  });

  const employeeNames = useMemo(() => {
    const map = new Map<string, string>();
    for (const e of employees.data ?? []) map.set(e.id, recordLabel(e));
    return map;
  }, [employees.data]);

  const rows = useMemo<ApprovalRowView[]>(() => {
    return (approvals.data ?? []).map((a) => {
      const record = recordsByKtype.get(a.record_ktype)?.get(a.record_id);
      const money = record ? recordAmount(record) : null;
      return {
        id: a.id,
        subject: record ? recordLabel(record) : ktypeSingular(a.record_ktype),
        typeLabel: ktypeSingular(a.record_ktype),
        href: `/records/${a.record_ktype}/${a.record_id}`,
        requester: employeeNames.get(a.chain.requested_by) ?? "—",
        amountText: money
          ? money.currency
            ? safeCurrency(fmt, money.value, money.currency)
            : fmt.number(money.value)
          : "—",
        amountValue: money?.value ?? null,
        createdAt: a.created_at,
        step: `${a.chain.current_step + 1} of ${a.chain.steps.length}`,
        state: a.state,
        isPending: a.state === "pending",
      };
    });
  }, [approvals.data, recordsByKtype, employeeNames, fmt]);

  const pendingRows = useMemo(() => rows.filter((r) => r.isPending), [rows]);

  const [selected, setSelected] = useState<Set<Key>>(new Set());
  const [rejectId, setRejectId] = useState<string | null>(null);

  const decide = useMutation({
    mutationFn: ({
      id,
      decision,
    }: {
      id: string;
      decision: "approve" | "reject";
    }) => api.decideApproval(id, decision),
    onSuccess: (_result, vars) => {
      toast.success(
        vars.decision === "approve" ? "Request approved" : "Request rejected",
      );
    },
    onError: (error) =>
      toast.error("Couldn't record your decision", {
        description: (error as Error).message,
      }),
    onSettled: () => qc.invalidateQueries({ queryKey: ["approvals"] }),
  });

  const bulkApprove = useMutation({
    // Promise.allSettled never rejects, so the mutation always resolves and
    // there is no reachable error path — per-item failures are reported via
    // the resolved `failedIds` instead of an onError handler.
    mutationFn: async (ids: string[]) => {
      const results = await Promise.allSettled(
        ids.map((id) => api.decideApproval(id, "approve")),
      );
      const failedIds = ids.filter((_, i) => results[i].status === "rejected");
      return { total: ids.length, failedIds };
    },
    onSuccess: ({ total, failedIds }) => {
      const failed = failedIds.length;
      // Keep the requests that failed selected so the operator can retry them
      // directly; clear only the ones that actually went through.
      setSelected(new Set(failedIds));
      if (failed === 0) {
        toast.success(`Approved ${total} request${total === 1 ? "" : "s"}`);
      } else {
        toast.warning(`Approved ${total - failed} of ${total}`, {
          description: `${failed} couldn't be approved — try again.`,
        });
      }
    },
    onSettled: () => qc.invalidateQueries({ queryKey: ["approvals"] }),
  });

  const busy = decide.isPending || bulkApprove.isPending;
  // `decide` gets a fresh identity every render in TanStack Query v5, but
  // `decide.mutate` is stable — depend on that so the columns memo holds.
  const decideMutate = decide.mutate;

  const columns = useMemo<DataGridColumn<ApprovalRowView>[]>(
    () => [
      {
        key: "subject",
        header: "Subject",
        sortable: true,
        accessor: (r) => r.subject.toLowerCase(),
        cell: (r) => (
          <Link
            to={r.href}
            className="font-medium text-fg hover:text-accent hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-(--focus-ring) focus-visible:ring-offset-2 focus-visible:ring-offset-(--bg)"
          >
            {r.subject}
          </Link>
        ),
      },
      {
        key: "type",
        header: "Type",
        sortable: true,
        accessor: (r) => r.typeLabel.toLowerCase(),
        cell: (r) => <Badge variant="neutral">{r.typeLabel}</Badge>,
      },
      {
        key: "amount",
        header: "Amount",
        sortable: true,
        accessor: (r) => r.amountValue,
        className: "text-end font-tabular tabular-nums",
        headerClassName: "text-end",
        cell: (r) => <span>{r.amountText}</span>,
      },
      {
        key: "requester",
        header: "Requested by",
        sortable: true,
        accessor: (r) => r.requester.toLowerCase(),
        cell: (r) => <span className="text-fg-muted">{r.requester}</span>,
      },
      {
        key: "age",
        header: "Age",
        sortable: true,
        accessor: (r) => new Date(r.createdAt).getTime(),
        cell: (r) => (
          <Tooltip>
            <TooltipTrigger asChild>
              <span className="text-fg-muted">{ageLabel(r.createdAt, fmt)}</span>
            </TooltipTrigger>
            <TooltipContent>
              Requested {fmt.dateTime(new Date(r.createdAt))}
            </TooltipContent>
          </Tooltip>
        ),
      },
      {
        key: "step",
        header: "Step",
        cell: (r) => <span className="text-fg-muted">{r.step}</span>,
      },
      {
        key: "status",
        header: "Status",
        sortable: true,
        accessor: (r) => r.state,
        cell: (r) => (
          <Badge variant={statusVariant(r.state)}>{titleCase(r.state)}</Badge>
        ),
      },
      {
        key: "actions",
        header: <span className="sr-only">Actions</span>,
        className: "text-end",
        headerClassName: "text-end",
        cell: (r) =>
          r.isPending ? (
            <div className="flex justify-end gap-2">
              <Button
                size="sm"
                disabled={busy}
                leadingIcon={<Check className="h-4 w-4" />}
                onClick={() => decideMutate({ id: r.id, decision: "approve" })}
              >
                Approve
              </Button>
              <Button
                size="sm"
                variant="outline"
                disabled={busy}
                leadingIcon={<X className="h-4 w-4" />}
                onClick={() => setRejectId(r.id)}
              >
                Reject
              </Button>
            </div>
          ) : (
            <span className="text-fg-subtle">Decided</span>
          ),
      },
    ],
    [busy, decideMutate, fmt],
  );

  // The "actions" column is meaningless once a row is decided, so the
  // All tab (which can include decided rows) drops it.
  const allColumns = useMemo(
    () => columns.filter((c) => c.key !== "actions"),
    [columns],
  );

  return (
    <section className="flex flex-col gap-6">
      <header className="flex flex-col gap-1">
        <Eyebrow>Work</Eyebrow>
        <h1 className="text-2xl font-semibold tracking-tight text-fg">
          Approvals
        </h1>
        <p className="text-sm text-fg-muted">
          Decisions waiting on you. Approve or reject in one click, or select
          several and approve them together.
        </p>
      </header>

      {approvals.isLoading ? (
        <ApprovalsSkeleton />
      ) : approvals.isError ? (
        <EmptyState
          icon={<AlertTriangle />}
          title="Couldn't load approvals"
          description={(approvals.error as Error).message}
          action={
            <Button
              variant="secondary"
              onClick={() => void approvals.refetch()}
              disabled={approvals.isFetching}
            >
              Retry
            </Button>
          }
        />
      ) : rows.length === 0 ? (
        <EmptyState
          icon={<Inbox />}
          title="No approvals yet"
          description="When a teammate submits something that needs sign-off — an invoice, a purchase order, time off — it shows up here for you to review."
        />
      ) : (
        <Tabs
          defaultValue="pending"
          onValueChange={() => setSelected(new Set())}
        >
          <TabsList>
            <TabsTrigger value="pending">
              Pending ({pendingRows.length})
            </TabsTrigger>
            <TabsTrigger value="all">All ({rows.length})</TabsTrigger>
          </TabsList>

          <TabsContent value="pending" className="flex flex-col gap-3">
            {selected.size > 0 && (
              <div className="flex items-center justify-between rounded-md border border-border bg-bg-subtle px-3 py-2">
                <span className="text-sm text-fg-muted">
                  {selected.size} selected
                </span>
                <Button
                  size="sm"
                  disabled={busy}
                  leadingIcon={<Check className="h-4 w-4" />}
                  onClick={() =>
                    bulkApprove.mutate([...selected].map((k) => String(k)))
                  }
                >
                  {bulkApprove.isPending ? "Approving…" : "Approve selected"}
                </Button>
              </div>
            )}
            <DataGrid
              data={pendingRows}
              columns={columns}
              rowKey={(r) => r.id}
              selectedKeys={selected}
              onSelectionChange={setSelected}
              emptyState={
                <EmptyState
                  icon={<Check />}
                  title="Nothing waiting on you"
                  description="You're all caught up — no approvals need a decision right now."
                />
              }
            />
          </TabsContent>

          <TabsContent value="all">
            <DataGrid
              data={rows}
              columns={allColumns}
              rowKey={(r) => r.id}
              emptyState={
                <EmptyState icon={<Inbox />} title="No approvals to show" />
              }
            />
          </TabsContent>
        </Tabs>
      )}

      <ConfirmDialog
        open={rejectId !== null}
        onOpenChange={(open) => {
          if (!open) setRejectId(null);
        }}
        title="Reject this request?"
        description="The requester will be notified that their request was rejected. This can't be undone from here."
        confirmLabel="Reject request"
        cancelLabel="Keep reviewing"
        destructive
        loading={decide.isPending}
        onConfirm={() => {
          if (rejectId) {
            decide.mutate(
              { id: rejectId, decision: "reject" },
              { onSettled: () => setRejectId(null) },
            );
          }
        }}
      />
    </section>
  );
}

function safeCurrency(fmt: Formatter, value: number, currency: string): string {
  try {
    return fmt.currency(value, currency);
  } catch {
    return fmt.number(value);
  }
}

function titleCase(token: string): string {
  return token.charAt(0).toUpperCase() + token.slice(1);
}

function ApprovalsSkeleton() {
  return (
    <div className="flex flex-col gap-3">
      <Skeleton className="h-9 w-48" />
      <div className="rounded-lg border border-border">
        {Array.from({ length: 5 }).map((_, i) => (
          <div
            key={i}
            className="flex items-center gap-4 border-b border-border px-3 py-3 last:border-b-0"
          >
            <Skeleton variant="text" className="w-40" />
            <Skeleton className="h-5 w-20" />
            <Skeleton variant="text" className="ml-auto w-24" />
            <Skeleton className="h-8 w-40" />
          </div>
        ))}
      </div>
    </div>
  );
}
