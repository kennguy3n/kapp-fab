import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord, PayslipGenerateResult } from "@kapp/client";
import {
  Badge,
  Button,
  ConfirmDialog,
  EmptyState,
  Eyebrow,
  Skeleton,
  Stepper,
  Table,
  TableBody,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
  Tabs,
  TabsList,
  TabsTrigger,
  toast,
  cn,
} from "@kapp/ui";
import {
  AlertTriangle,
  CheckCircle2,
  Plus,
  Receipt,
  RefreshCw,
} from "lucide-react";
import { api } from "../lib/api";
import { useFormatter } from "../lib/i18n/useFormatter";
import { humanizeToken, recordLabel, statusVariant } from "../lib/ktypeView";

const KTYPE_COMPONENT = "hr.salary_component";
const KTYPE_STRUCTURE = "hr.salary_structure";
const KTYPE_PAYRUN = "hr.pay_run";
const KTYPE_PAYSLIP = "hr.payslip";
const KTYPE_EMPLOYEE = "hr.employee";

interface SalaryComponentData {
  code?: string;
  name?: string;
  type?: string;
  amount_type?: string;
  amount?: number | string;
  currency?: string;
  active?: boolean;
}

interface SalaryStructureData {
  employee_id?: string;
  effective_from?: string;
  base_salary?: number | string;
  currency?: string;
  payment_frequency?: string;
  status?: string;
}

interface PayRunData {
  name?: string;
  pay_period_start?: string;
  pay_period_end?: string;
  department?: string;
  currency?: string;
  payslip_count?: number;
  total_gross?: number | string;
  total_net?: number | string;
  status?: string;
}

interface PayslipData {
  pay_run_id?: string;
  employee_id?: string;
  pay_period_start?: string;
  pay_period_end?: string;
  currency?: string;
  gross_pay?: number | string;
  total_deductions?: number | string;
  net_pay?: number | string;
  status?: string;
}

type Tab = "components" | "structures" | "runs";

const NEW_LABEL: Record<Tab, string> = {
  components: "New Component",
  structures: "New Structure",
  runs: "New Pay Run",
};

const NEW_ROUTE: Record<Tab, string> = {
  components: `/records/${KTYPE_COMPONENT}/new`,
  structures: `/records/${KTYPE_STRUCTURE}/new`,
  runs: `/records/${KTYPE_PAYRUN}/new`,
};

/** Coerce a JSONB numeric field (sometimes serialised as a string) to a number. */
function num(value: number | string | undefined): number {
  if (typeof value === "number") return value;
  if (typeof value === "string" && value.trim() !== "") return Number(value);
  return 0;
}

/** Parse a date field. Date-only strings ("YYYY-MM-DD") are anchored to
 * local midnight so the rendered day doesn't shift in negative offsets. */
function toDate(value: string): Date {
  return value.length <= 10 ? new Date(`${value}T00:00:00`) : new Date(value);
}

/**
 * PayrollPage is the payroll workspace: salary components (earnings /
 * deductions), per-employee salary structures, and pay runs with their
 * payslips. Components and structures are edited through the generic
 * record form; pay runs expose the generate → post lifecycle inline,
 * with a run-state stepper and right-aligned, currency-formatted money.
 */
export function PayrollPage() {
  const nav = useNavigate();
  const [tab, setTab] = useState<Tab>("components");

  return (
    <section className="flex flex-col gap-4">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <Eyebrow>Human Resources</Eyebrow>
          <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
            Payroll
          </h1>
          <p className="mt-1 text-sm text-fg-muted">
            Define how people are paid, then run payroll and post it to
            the ledger.
          </p>
        </div>
        <Button
          size="sm"
          leadingIcon={<Plus className="h-4 w-4" />}
          onClick={() => nav(NEW_ROUTE[tab])}
        >
          {NEW_LABEL[tab]}
        </Button>
      </header>

      <Tabs value={tab} onValueChange={(v) => setTab(v as Tab)}>
        <TabsList>
          <TabsTrigger value="components">Components</TabsTrigger>
          <TabsTrigger value="structures">Structures</TabsTrigger>
          <TabsTrigger value="runs">Pay Runs</TabsTrigger>
        </TabsList>
      </Tabs>

      {tab === "components" && <ComponentsTable />}
      {tab === "structures" && <StructuresTable />}
      {tab === "runs" && <PayRunsTable />}
    </section>
  );
}

/** Resolves employee record ids to display names for the payroll tables. */
function useEmployeeNames(): Map<string, string> {
  const q = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_EMPLOYEE],
    queryFn: () => api.listRecords(KTYPE_EMPLOYEE),
  });
  return useMemo(() => {
    const m = new Map<string, string>();
    (q.data ?? []).forEach((r) => m.set(r.id, recordLabel(r)));
    return m;
  }, [q.data]);
}

/** Skeleton rows shown while a payroll table's query is in flight. */
function TableLoadingRows({ cols, rows = 4 }: { cols: number; rows?: number }) {
  return (
    <TableBody>
      {Array.from({ length: rows }).map((_, r) => (
        <TableRow key={r}>
          {Array.from({ length: cols }).map((__, c) => (
            <TableCell key={c}>
              <Skeleton className="h-4 w-full" />
            </TableCell>
          ))}
        </TableRow>
      ))}
    </TableBody>
  );
}

/** Inline error banner with a retry affordance, used across the tables. */
function LoadError({
  message,
  onRetry,
}: {
  message: string;
  onRetry: () => void;
}) {
  return (
    <div
      role="alert"
      className="flex flex-wrap items-center gap-3 rounded-md border border-danger/30 bg-danger/5 px-3 py-2 text-sm text-fg"
    >
      <AlertTriangle className="h-4 w-4 shrink-0 text-danger" aria-hidden />
      <span className="min-w-0 flex-1">{message}</span>
      <Button
        size="sm"
        variant="outline"
        leadingIcon={<RefreshCw className="h-4 w-4" />}
        onClick={onRetry}
      >
        Retry
      </Button>
    </div>
  );
}

function CountCaption({ count, noun }: { count: number; noun: string }) {
  return (
    <p className="text-xs text-fg-muted">
      {count.toLocaleString()} {count === 1 ? noun : `${noun}s`}
    </p>
  );
}

function ComponentsTable() {
  const nav = useNavigate();
  const fmt = useFormatter();
  const q = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_COMPONENT],
    queryFn: () => api.listRecords(KTYPE_COMPONENT),
  });
  const rows = q.data ?? [];

  if (q.isError) {
    return (
      <LoadError
        message="We couldn't load salary components."
        onRetry={() => q.refetch()}
      />
    );
  }
  if (!q.isLoading && rows.length === 0) {
    return (
      <EmptyState
        icon={<Receipt />}
        title="No salary components yet"
        description="Components are the building blocks of pay — earnings like base salary and bonuses, and deductions like tax."
        action={
          <Button
            size="sm"
            leadingIcon={<Plus className="h-4 w-4" />}
            onClick={() => nav(NEW_ROUTE.components)}
          >
            New Component
          </Button>
        }
      />
    );
  }

  return (
    <div className="flex flex-col gap-2">
      {!q.isLoading && <CountCaption count={rows.length} noun="component" />}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Code</TableHead>
            <TableHead>Name</TableHead>
            <TableHead>Type</TableHead>
            <TableHead className="text-end">Amount</TableHead>
            <TableHead>Active</TableHead>
          </TableRow>
        </TableHeader>
        {q.isLoading ? (
          <TableLoadingRows cols={5} />
        ) : (
          <TableBody>
            {rows.map((r) => {
              const d = r.data as unknown as SalaryComponentData;
              const isPct = d.amount_type === "percentage";
              return (
                <TableRow
                  key={r.id}
                  className="cursor-pointer"
                  onClick={() => nav(`/records/${KTYPE_COMPONENT}/${r.id}`)}
                >
                  <TableCell className="font-mono text-xs text-fg-muted">
                    {d.code ?? "—"}
                  </TableCell>
                  <TableCell className="font-medium">{d.name ?? "—"}</TableCell>
                  <TableCell>
                    <Badge
                      variant={d.type === "deduction" ? "warning" : "success"}
                      size="sm"
                    >
                      {humanizeToken(d.type ?? "earning")}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-end">
                    {isPct
                      ? `${num(d.amount)}%`
                      : fmt.currency(num(d.amount), d.currency ?? "USD")}
                  </TableCell>
                  <TableCell>
                    <Badge
                      variant={d.active === false ? "neutral" : "success"}
                      size="sm"
                    >
                      {d.active === false ? "Inactive" : "Active"}
                    </Badge>
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        )}
      </Table>
    </div>
  );
}

function StructuresTable() {
  const nav = useNavigate();
  const fmt = useFormatter();
  const employeeNames = useEmployeeNames();
  const q = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_STRUCTURE],
    queryFn: () => api.listRecords(KTYPE_STRUCTURE),
  });
  const rows = q.data ?? [];

  if (q.isError) {
    return (
      <LoadError
        message="We couldn't load salary structures."
        onRetry={() => q.refetch()}
      />
    );
  }
  if (!q.isLoading && rows.length === 0) {
    return (
      <EmptyState
        icon={<Receipt />}
        title="No salary structures yet"
        description="A salary structure sets an employee's base pay and how often they're paid."
        action={
          <Button
            size="sm"
            leadingIcon={<Plus className="h-4 w-4" />}
            onClick={() => nav(NEW_ROUTE.structures)}
          >
            New Structure
          </Button>
        }
      />
    );
  }

  return (
    <div className="flex flex-col gap-2">
      {!q.isLoading && <CountCaption count={rows.length} noun="structure" />}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Employee</TableHead>
            <TableHead>Effective from</TableHead>
            <TableHead className="text-end">Base salary</TableHead>
            <TableHead>Frequency</TableHead>
            <TableHead>Status</TableHead>
          </TableRow>
        </TableHeader>
        {q.isLoading ? (
          <TableLoadingRows cols={5} />
        ) : (
          <TableBody>
            {rows.map((r) => {
              const d = r.data as unknown as SalaryStructureData;
              return (
                <TableRow
                  key={r.id}
                  className="cursor-pointer"
                  onClick={() => nav(`/records/${KTYPE_STRUCTURE}/${r.id}`)}
                >
                  <TableCell className="font-medium">
                    {(d.employee_id && employeeNames.get(d.employee_id)) || "—"}
                  </TableCell>
                  <TableCell>
                    {d.effective_from ? fmt.date(toDate(d.effective_from)) : "—"}
                  </TableCell>
                  <TableCell className="text-end">
                    {fmt.currency(num(d.base_salary), d.currency ?? "USD")}
                  </TableCell>
                  <TableCell>
                    {d.payment_frequency
                      ? humanizeToken(d.payment_frequency)
                      : "—"}
                  </TableCell>
                  <TableCell>
                    {d.status ? (
                      <Badge variant={statusVariant(d.status)} size="sm">
                        {humanizeToken(d.status)}
                      </Badge>
                    ) : (
                      "—"
                    )}
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        )}
      </Table>
    </div>
  );
}

// Pay-run lifecycle stages, in order, for the run-state stepper.
const PAY_RUN_STEPS = [
  { label: "Draft" },
  { label: "Calculated" },
  { label: "Reviewed" },
  { label: "Approved" },
  { label: "Paid" },
];

const PAY_RUN_TERMINAL = new Set(["paid", "posted", "completed"]);

function payRunStepIndex(status: string): number {
  switch (status) {
    case "draft":
      return 0;
    case "calculating":
    case "calculated":
    case "processing":
      return 1;
    case "review":
    case "in_review":
    case "reviewed":
    case "pending_approval":
      return 2;
    case "approved":
      return 3;
    case "paid":
    case "posted":
    case "completed":
      return 4;
    default:
      return 0;
  }
}

/**
 * PayRunsTable lists every hr.pay_run with right-aligned, formatted
 * gross/net totals and the generate / post lifecycle actions. Expanding
 * a run reveals its run-state stepper and the payslips that belong to
 * it. Posting is irreversible (it writes a journal entry) so it is
 * gated behind a confirmation dialog.
 */
function PayRunsTable() {
  const nav = useNavigate();
  const qc = useQueryClient();
  const fmt = useFormatter();
  const [selectedRunId, setSelectedRunId] = useState<string | null>(null);
  const [postTarget, setPostTarget] = useState<KRecord | null>(null);

  const runs = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_PAYRUN],
    queryFn: () => api.listRecords(KTYPE_PAYRUN),
  });

  // Both generate and post mutations must invalidate the dedicated
  // ["hr.pay_run.payslips"] key used by PayslipsForRun alongside the
  // generic ["records", KTYPE_PAYSLIP] key. React Query's
  // invalidateQueries prefix-matches, and those two keys no longer share
  // a prefix — missing the new one leaves the open detail panel showing
  // stale data until the user toggles it off/on.
  function invalidatePayrollQueries() {
    qc.invalidateQueries({ queryKey: ["records", KTYPE_PAYRUN] });
    qc.invalidateQueries({ queryKey: ["records", KTYPE_PAYSLIP] });
    qc.invalidateQueries({ queryKey: ["hr.pay_run.payslips"] });
  }

  const generate = useMutation({
    mutationFn: (id: string) => api.generatePayslips(id),
    onSuccess: (result) => {
      invalidatePayrollQueries();
      toast.success("Payslips generated", {
        description: `${result.created_count} created · ${result.skipped_existing} already existed`,
      });
    },
    onError: (e) =>
      toast.error("Couldn't generate payslips", { description: String(e) }),
  });

  const post = useMutation({
    mutationFn: (id: string) => api.postPayRun(id),
    onSuccess: () => {
      invalidatePayrollQueries();
      setPostTarget(null);
      toast.success("Pay run posted to the ledger");
    },
    // The post confirmation overlay can cover the inline error, so also
    // surface failures as a toast that renders above the dialog.
    onError: (e) =>
      toast.error("Couldn't post pay run", { description: String(e) }),
  });

  const rows = runs.data ?? [];
  const totalGross = rows.reduce(
    (s, r) => s + num((r.data as PayRunData).total_gross),
    0,
  );
  const totalNet = rows.reduce(
    (s, r) => s + num((r.data as PayRunData).total_net),
    0,
  );
  // Footer totals are only meaningful within a single currency; each row
  // still shows its own, so the aggregate is suppressed when runs mix them.
  const currencies = new Set(
    rows.map((r) => (r.data as PayRunData).currency ?? "USD"),
  );
  const footerCurrency =
    (rows[0]?.data as PayRunData | undefined)?.currency ?? "USD";
  const mixedCurrency = currencies.size > 1;

  if (runs.isError) {
    return (
      <LoadError
        message="We couldn't load pay runs."
        onRetry={() => runs.refetch()}
      />
    );
  }
  if (!runs.isLoading && rows.length === 0) {
    return (
      <EmptyState
        icon={<Receipt />}
        title="No pay runs yet"
        description="A pay run groups a period's payslips. Create one, generate payslips for eligible employees, then post it to the ledger."
        action={
          <Button
            size="sm"
            leadingIcon={<Plus className="h-4 w-4" />}
            onClick={() => nav(NEW_ROUTE.runs)}
          >
            New Pay Run
          </Button>
        }
      />
    );
  }

  return (
    <div className="flex flex-col gap-3">
      {!runs.isLoading && <CountCaption count={rows.length} noun="pay run" />}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>Period</TableHead>
            <TableHead>Department</TableHead>
            <TableHead className="text-end">Slips</TableHead>
            <TableHead className="text-end">Total gross</TableHead>
            <TableHead className="text-end">Total net</TableHead>
            <TableHead>Status</TableHead>
            <TableHead className="text-end">Actions</TableHead>
          </TableRow>
        </TableHeader>
        {runs.isLoading ? (
          <TableLoadingRows cols={8} />
        ) : (
          <TableBody>
            {rows.map((r) => {
              const d = r.data as unknown as PayRunData;
              const isSelected = selectedRunId === r.id;
              const status = d.status ?? "draft";
              const terminal = PAY_RUN_TERMINAL.has(status);
              const currency = d.currency ?? "USD";
              const busy =
                (generate.isPending && generate.variables === r.id) ||
                (post.isPending && post.variables === r.id);
              return (
                <TableRow key={r.id} className={cn(isSelected && "bg-bg-subtle")}>
                  <TableCell>
                    <Button
                      variant="link"
                      size="sm"
                      className="h-auto p-0"
                      onClick={() => nav(`/records/${KTYPE_PAYRUN}/${r.id}`)}
                    >
                      {d.name ?? "Untitled pay run"}
                    </Button>
                  </TableCell>
                  <TableCell className="whitespace-nowrap">
                    {d.pay_period_start && d.pay_period_end
                      ? `${fmt.date(toDate(d.pay_period_start))} – ${fmt.date(toDate(d.pay_period_end))}`
                      : "—"}
                  </TableCell>
                  <TableCell>{d.department || "—"}</TableCell>
                  <TableCell className="text-end">
                    {fmt.number(d.payslip_count ?? 0)}
                  </TableCell>
                  <TableCell className="text-end">
                    {fmt.currency(num(d.total_gross), currency)}
                  </TableCell>
                  <TableCell className="text-end">
                    {fmt.currency(num(d.total_net), currency)}
                  </TableCell>
                  <TableCell>
                    <Badge variant={statusVariant(status)} size="sm">
                      {humanizeToken(status)}
                    </Badge>
                  </TableCell>
                  <TableCell className="text-end">
                    <div className="flex flex-wrap justify-end gap-1.5">
                      <Button
                        size="sm"
                        variant="secondary"
                        disabled={busy || terminal}
                        onClick={() => generate.mutate(r.id)}
                      >
                        Generate
                      </Button>
                      <Button
                        size="sm"
                        variant="secondary"
                        disabled={busy || terminal}
                        onClick={() => setPostTarget(r)}
                      >
                        Post
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        aria-expanded={isSelected}
                        onClick={() =>
                          setSelectedRunId(isSelected ? null : r.id)
                        }
                      >
                        {isSelected ? "Hide slips" : "View slips"}
                      </Button>
                    </div>
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        )}
        {!runs.isLoading && rows.length > 0 && (
          <TableFooter>
            <TableRow>
              <TableCell className="font-medium" colSpan={4}>
                Total
              </TableCell>
              {mixedCurrency ? (
                <TableCell
                  colSpan={2}
                  className="text-end text-sm text-fg-muted"
                >
                  Mixed currencies — see each row
                </TableCell>
              ) : (
                <>
                  <TableCell className="text-end font-semibold">
                    {fmt.currency(totalGross, footerCurrency)}
                  </TableCell>
                  <TableCell className="text-end font-semibold">
                    {fmt.currency(totalNet, footerCurrency)}
                  </TableCell>
                </>
              )}
              <TableCell colSpan={2} />
            </TableRow>
          </TableFooter>
        )}
      </Table>

      {generate.isError && (
        <p className="text-sm text-danger">
          Generate failed: {String(generate.error)}
        </p>
      )}
      {post.isError && (
        <p className="text-sm text-danger">Post failed: {String(post.error)}</p>
      )}
      {generate.isSuccess && generate.data && (
        <GenerateSummary summary={generate.data} />
      )}

      {selectedRunId &&
        (() => {
          const run = rows.find((r) => r.id === selectedRunId);
          if (!run) return null;
          return <RunDetailPanel run={run} />;
        })()}

      <ConfirmDialog
        open={postTarget !== null}
        onOpenChange={(o) => !o && setPostTarget(null)}
        title="Post this pay run?"
        description="Posting records a journal entry in the ledger and marks payslips as paid. This can't be undone."
        confirmLabel="Post pay run"
        loading={post.isPending}
        onConfirm={() => postTarget && post.mutate(postTarget.id)}
      />
    </div>
  );
}

function GenerateSummary({ summary }: { summary: PayslipGenerateResult }) {
  return (
    <div className="flex items-start gap-2 rounded-md border border-success/30 bg-success/5 px-3 py-2 text-sm text-fg">
      <CheckCircle2 className="mt-0.5 h-4 w-4 shrink-0 text-success" aria-hidden />
      <p>
        Created {summary.created_count} slip(s); skipped{" "}
        {summary.skipped_existing} existing, {summary.skipped_no_structure}{" "}
        without a salary structure.
      </p>
    </div>
  );
}

function RunDetailPanel({ run }: { run: KRecord }) {
  const d = run.data as unknown as PayRunData;
  const status = d.status ?? "draft";
  const current = PAY_RUN_TERMINAL.has(status)
    ? PAY_RUN_STEPS.length
    : payRunStepIndex(status);
  return (
    <section className="flex flex-col gap-4 rounded-lg border border-border bg-bg-subtle p-4">
      <div className="flex flex-col gap-1">
        <h3 className="text-sm font-semibold text-fg">
          {d.name ?? "Pay run"} progress
        </h3>
        <Stepper steps={PAY_RUN_STEPS} current={current} className="max-w-2xl" />
      </div>
      <PayslipsForRun payRunId={run.id} currency={d.currency ?? "USD"} />
    </section>
  );
}

function PayslipsForRun({
  payRunId,
  currency,
}: {
  payRunId: string;
  currency: string;
}) {
  const fmt = useFormatter();
  const employeeNames = useEmployeeNames();
  // Use the dedicated /hr/pay-runs/:id/payslips endpoint rather than
  // listRecords(KTYPE_PAYSLIP) + client-side filter: the generic list
  // route caps at 500 rows and defaults to 50, so on tenants with >50
  // total payslips across all runs the old path would silently drop
  // results for the selected run.
  const slips = useQuery<KRecord[]>({
    queryKey: ["hr.pay_run.payslips", payRunId],
    queryFn: () => api.listPayRunPayslips(payRunId),
  });
  const rows = slips.data ?? [];
  const totalGross = rows.reduce(
    (s, r) => s + num((r.data as PayslipData).gross_pay),
    0,
  );
  const totalDeductions = rows.reduce(
    (s, r) => s + num((r.data as PayslipData).total_deductions),
    0,
  );
  const totalNet = rows.reduce(
    (s, r) => s + num((r.data as PayslipData).net_pay),
    0,
  );

  return (
    <div className="flex flex-col gap-2">
      <h3 className="text-sm font-semibold text-fg">Payslips</h3>
      {slips.isError ? (
        <LoadError
          message="We couldn't load this run's payslips."
          onRetry={() => slips.refetch()}
        />
      ) : !slips.isLoading && rows.length === 0 ? (
        <EmptyState
          className="py-8"
          icon={<Receipt />}
          title="No payslips in this run"
          description="Generate payslips to populate this pay run."
        />
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Employee</TableHead>
              <TableHead className="text-end">Gross</TableHead>
              <TableHead className="text-end">Deductions</TableHead>
              <TableHead className="text-end">Net</TableHead>
              <TableHead>Status</TableHead>
            </TableRow>
          </TableHeader>
          {slips.isLoading ? (
            <TableLoadingRows cols={5} />
          ) : (
            <TableBody>
              {rows.map((r) => {
                const d = r.data as unknown as PayslipData;
                const slipCurrency = d.currency ?? currency;
                return (
                  <TableRow key={r.id}>
                    <TableCell className="font-medium">
                      {(d.employee_id && employeeNames.get(d.employee_id)) ||
                        "—"}
                    </TableCell>
                    <TableCell className="text-end">
                      {fmt.currency(num(d.gross_pay), slipCurrency)}
                    </TableCell>
                    <TableCell className="text-end">
                      {fmt.currency(num(d.total_deductions), slipCurrency)}
                    </TableCell>
                    <TableCell className="text-end font-medium">
                      {fmt.currency(num(d.net_pay), slipCurrency)}
                    </TableCell>
                    <TableCell>
                      <Badge variant={statusVariant(d.status ?? "draft")} size="sm">
                        {humanizeToken(d.status ?? "draft")}
                      </Badge>
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          )}
          {!slips.isLoading && rows.length > 0 && (
            <TableFooter>
              <TableRow>
                <TableCell className="font-medium">Total</TableCell>
                <TableCell className="text-end font-semibold">
                  {fmt.currency(totalGross, currency)}
                </TableCell>
                <TableCell className="text-end font-semibold">
                  {fmt.currency(totalDeductions, currency)}
                </TableCell>
                <TableCell className="text-end font-semibold">
                  {fmt.currency(totalNet, currency)}
                </TableCell>
                <TableCell />
              </TableRow>
            </TableFooter>
          )}
        </Table>
      )}
    </div>
  );
}
