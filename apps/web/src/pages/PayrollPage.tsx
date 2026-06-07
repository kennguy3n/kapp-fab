import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord, PayslipGenerateResult } from "@kapp/client";
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

const KTYPE_COMPONENT = "hr.salary_component";
const KTYPE_STRUCTURE = "hr.salary_structure";
const KTYPE_PAYRUN = "hr.pay_run";
const KTYPE_PAYSLIP = "hr.payslip";

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

/**
 * PayrollPage exposes the two payroll KTypes side-by-side: salary
 * components (earnings / deductions) and salary structures
 * (per-employee compensation bundles). CRUD happens through the
 * generic KType form — this page just lists and links into it.
 */
export function PayrollPage() {
  const nav = useNavigate();
  const [tab, setTab] = useState<Tab>("components");

  return (
    <section className="flex flex-col gap-3">
      <h1 className="text-2xl font-semibold tracking-tight text-fg">Payroll</h1>
      <div className="flex flex-wrap items-center gap-2">
        <Button
          size="sm"
          variant={tab === "components" ? "primary" : "outline"}
          aria-pressed={tab === "components"}
          onClick={() => setTab("components")}
        >
          Components
        </Button>
        <Button
          size="sm"
          variant={tab === "structures" ? "primary" : "outline"}
          aria-pressed={tab === "structures"}
          onClick={() => setTab("structures")}
        >
          Structures
        </Button>
        <Button
          size="sm"
          variant={tab === "runs" ? "primary" : "outline"}
          aria-pressed={tab === "runs"}
          onClick={() => setTab("runs")}
        >
          Pay Runs
        </Button>
        <Button
          size="sm"
          className="ms-auto"
          onClick={() => {
            if (tab === "components") nav(`/records/${KTYPE_COMPONENT}/new`);
            else if (tab === "structures") nav(`/records/${KTYPE_STRUCTURE}/new`);
            else nav(`/records/${KTYPE_PAYRUN}/new`);
          }}
        >
          New{" "}
          {tab === "components"
            ? "component"
            : tab === "structures"
              ? "structure"
              : "pay run"}
        </Button>
      </div>

      {tab === "components" && <ComponentsTable />}
      {tab === "structures" && <StructuresTable />}
      {tab === "runs" && <PayRunsTable />}
    </section>
  );
}

function ComponentsTable() {
  const nav = useNavigate();
  const q = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_COMPONENT],
    queryFn: () => api.listRecords(KTYPE_COMPONENT),
  });
  return (
    <>
      {q.isLoading && <p className="text-sm text-fg-muted">Loading…</p>}
      {q.isError && <p className="text-sm text-danger">Failed to load.</p>}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Code</TableHead>
            <TableHead>Name</TableHead>
            <TableHead>Type</TableHead>
            <TableHead>Amount</TableHead>
            <TableHead>Currency</TableHead>
            <TableHead>Active</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {(q.data ?? []).map((r) => {
            const d = r.data as unknown as SalaryComponentData;
            return (
              <TableRow
                key={r.id}
                className="cursor-pointer"
                onClick={() => nav(`/records/${KTYPE_COMPONENT}/${r.id}`)}
              >
                <TableCell>
                  <code>{d.code ?? ""}</code>
                </TableCell>
                <TableCell>{d.name ?? ""}</TableCell>
                <TableCell>{d.type ?? ""}</TableCell>
                <TableCell>
                  {d.amount ?? 0}
                  {d.amount_type === "percentage" ? " %" : ""}
                </TableCell>
                <TableCell>{d.currency ?? "USD"}</TableCell>
                <TableCell>{d.active === false ? "no" : "yes"}</TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </>
  );
}

function StructuresTable() {
  const nav = useNavigate();
  const q = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_STRUCTURE],
    queryFn: () => api.listRecords(KTYPE_STRUCTURE),
  });
  return (
    <>
      {q.isLoading && <p className="text-sm text-fg-muted">Loading…</p>}
      {q.isError && <p className="text-sm text-danger">Failed to load.</p>}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Employee</TableHead>
            <TableHead>Effective from</TableHead>
            <TableHead>Base salary</TableHead>
            <TableHead>Currency</TableHead>
            <TableHead>Frequency</TableHead>
            <TableHead>Status</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {(q.data ?? []).map((r) => {
            const d = r.data as unknown as SalaryStructureData;
            return (
              <TableRow
                key={r.id}
                className="cursor-pointer"
                onClick={() => nav(`/records/${KTYPE_STRUCTURE}/${r.id}`)}
              >
                <TableCell>{d.employee_id ?? ""}</TableCell>
                <TableCell>{d.effective_from ?? ""}</TableCell>
                <TableCell>{d.base_salary ?? 0}</TableCell>
                <TableCell>{d.currency ?? "USD"}</TableCell>
                <TableCell>{d.payment_frequency ?? ""}</TableCell>
                <TableCell>{d.status ?? ""}</TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </>
  );
}

/**
 * PayRunsTable lists every hr.pay_run row with action buttons for
 * the generate and post endpoints. Status drives which action is
 * available: draft → Generate; approved → Post. Slips belonging to
 * the selected run are listed below the table.
 */
function PayRunsTable() {
  const nav = useNavigate();
  const qc = useQueryClient();
  const [selectedRunId, setSelectedRunId] = useState<string | null>(null);

  const runs = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE_PAYRUN],
    queryFn: () => api.listRecords(KTYPE_PAYRUN),
  });

  // Both generate and post mutations must invalidate the
  // dedicated ["hr.pay_run.payslips"] key used by PayslipsForRun
  // alongside the generic ["records", KTYPE_PAYSLIP] key.
  // React Query's invalidateQueries prefix-matches, and those two
  // keys no longer share a prefix — missing the new one leaves the
  // open "View slips" panel showing stale data until the user
  // toggles it off/on.
  const generate = useMutation({
    mutationFn: (id: string) => api.generatePayslips(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", KTYPE_PAYRUN] });
      qc.invalidateQueries({ queryKey: ["records", KTYPE_PAYSLIP] });
      qc.invalidateQueries({ queryKey: ["hr.pay_run.payslips"] });
    },
  });

  const post = useMutation({
    mutationFn: (id: string) => api.postPayRun(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", KTYPE_PAYRUN] });
      qc.invalidateQueries({ queryKey: ["records", KTYPE_PAYSLIP] });
      qc.invalidateQueries({ queryKey: ["hr.pay_run.payslips"] });
    },
  });

  return (
    <>
      {runs.isLoading && <p className="text-sm text-fg-muted">Loading…</p>}
      {runs.isError && <p className="text-sm text-danger">Failed to load.</p>}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>Period</TableHead>
            <TableHead>Department</TableHead>
            <TableHead>Slips</TableHead>
            <TableHead>Total Gross</TableHead>
            <TableHead>Total Net</TableHead>
            <TableHead>Status</TableHead>
            <TableHead>Actions</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {(runs.data ?? []).map((r) => {
            const d = r.data as unknown as PayRunData;
            const isSelected = selectedRunId === r.id;
            const busy =
              (generate.isPending && generate.variables === r.id) ||
              (post.isPending && post.variables === r.id);
            return (
              <TableRow
                key={r.id}
                className={cn(isSelected && "bg-bg-subtle")}
              >
                <TableCell>
                  <Button
                    variant="link"
                    size="sm"
                    onClick={() => nav(`/records/${KTYPE_PAYRUN}/${r.id}`)}
                  >
                    {d.name ?? r.id}
                  </Button>
                </TableCell>
                <TableCell>
                  {d.pay_period_start ?? "?"} → {d.pay_period_end ?? "?"}
                </TableCell>
                <TableCell>{d.department ?? ""}</TableCell>
                <TableCell>{d.payslip_count ?? 0}</TableCell>
                <TableCell>
                  {d.total_gross ?? 0} {d.currency ?? ""}
                </TableCell>
                <TableCell>
                  {d.total_net ?? 0} {d.currency ?? ""}
                </TableCell>
                <TableCell>{d.status ?? "draft"}</TableCell>
                <TableCell>
                  <div className="flex flex-wrap gap-1.5">
                    <Button
                      size="sm"
                      variant="secondary"
                      disabled={busy || d.status === "paid"}
                      onClick={() => generate.mutate(r.id)}
                      title="Generate draft payslips for eligible employees"
                    >
                      Generate
                    </Button>
                    <Button
                      size="sm"
                      variant="secondary"
                      disabled={busy || d.status === "paid"}
                      onClick={() => post.mutate(r.id)}
                      title="Post approved payslips as a journal entry"
                    >
                      Post
                    </Button>
                    <Button
                      size="sm"
                      variant="ghost"
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
      {selectedRunId && <PayslipsForRun payRunId={selectedRunId} />}
    </>
  );
}

function GenerateSummary({ summary }: { summary: PayslipGenerateResult }) {
  return (
    <p className="mt-3 text-xs text-fg-muted">
      Created {summary.created_count} slip(s); skipped{" "}
      {summary.skipped_existing} existing, {summary.skipped_no_structure} without
      a salary structure.
    </p>
  );
}

function PayslipsForRun({ payRunId }: { payRunId: string }) {
  // Use the dedicated /hr/pay-runs/:id/payslips endpoint rather
  // than listRecords(KTYPE_PAYSLIP) + client-side filter: the
  // generic list route caps at 500 rows and defaults to 50, so
  // on tenants with >50 total payslips across all runs the old
  // path would silently drop results for the selected run.
  const slips = useQuery<KRecord[]>({
    queryKey: ["hr.pay_run.payslips", payRunId],
    queryFn: () => api.listPayRunPayslips(payRunId),
  });
  return (
    <section className="mt-4 flex flex-col gap-2">
      <h3 className="text-sm font-semibold text-fg">Payslips</h3>
      {slips.isLoading && (
        <p className="text-sm text-fg-muted">Loading slips…</p>
      )}
      {slips.isError && (
        <p className="text-sm text-danger">Failed to load slips.</p>
      )}
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Employee</TableHead>
            <TableHead>Gross</TableHead>
            <TableHead>Deductions</TableHead>
            <TableHead>Net</TableHead>
            <TableHead>Currency</TableHead>
            <TableHead>Status</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {(slips.data ?? []).map((r) => {
            const d = r.data as unknown as PayslipData;
            return (
              <TableRow key={r.id}>
                <TableCell>{d.employee_id ?? ""}</TableCell>
                <TableCell>{d.gross_pay ?? 0}</TableCell>
                <TableCell>{d.total_deductions ?? 0}</TableCell>
                <TableCell>{d.net_pay ?? 0}</TableCell>
                <TableCell>{d.currency ?? ""}</TableCell>
                <TableCell>{d.status ?? "draft"}</TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </section>
  );
}
