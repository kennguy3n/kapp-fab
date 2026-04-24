import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord, PayslipGenerateResult } from "@kapp/client";
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
    <section>
      <h1>Payroll</h1>
      <div style={{ display: "flex", gap: 8, marginTop: 8, marginBottom: 12 }}>
        <button aria-pressed={tab === "components"} onClick={() => setTab("components")}>
          Components
        </button>
        <button aria-pressed={tab === "structures"} onClick={() => setTab("structures")}>
          Structures
        </button>
        <button aria-pressed={tab === "runs"} onClick={() => setTab("runs")}>
          Pay Runs
        </button>
        <button
          style={{ marginLeft: "auto" }}
          onClick={() => {
            if (tab === "components") nav(`/records/${KTYPE_COMPONENT}/new`);
            else if (tab === "structures") nav(`/records/${KTYPE_STRUCTURE}/new`);
            else nav(`/records/${KTYPE_PAYRUN}/new`);
          }}
        >
          New {tab === "components" ? "component" : tab === "structures" ? "structure" : "pay run"}
        </button>
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
      {q.isLoading && <p>Loading…</p>}
      {q.isError && <p style={{ color: "#b91c1c" }}>Failed to load.</p>}
      <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
        <thead>
          <tr style={{ textAlign: "left", color: "#6b7280" }}>
            <Th>Code</Th>
            <Th>Name</Th>
            <Th>Type</Th>
            <Th>Amount</Th>
            <Th>Currency</Th>
            <Th>Active</Th>
          </tr>
        </thead>
        <tbody>
          {(q.data ?? []).map((r) => {
            const d = r.data as unknown as SalaryComponentData;
            return (
              <tr
                key={r.id}
                style={{ borderTop: "1px solid #e5e7eb", cursor: "pointer" }}
                onClick={() => nav(`/records/${KTYPE_COMPONENT}/${r.id}`)}
              >
                <Td><code>{d.code ?? ""}</code></Td>
                <Td>{d.name ?? ""}</Td>
                <Td>{d.type ?? ""}</Td>
                <Td>
                  {d.amount ?? 0}
                  {d.amount_type === "percentage" ? " %" : ""}
                </Td>
                <Td>{d.currency ?? "USD"}</Td>
                <Td>{d.active === false ? "no" : "yes"}</Td>
              </tr>
            );
          })}
        </tbody>
      </table>
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
      {q.isLoading && <p>Loading…</p>}
      {q.isError && <p style={{ color: "#b91c1c" }}>Failed to load.</p>}
      <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
        <thead>
          <tr style={{ textAlign: "left", color: "#6b7280" }}>
            <Th>Employee</Th>
            <Th>Effective from</Th>
            <Th>Base salary</Th>
            <Th>Currency</Th>
            <Th>Frequency</Th>
            <Th>Status</Th>
          </tr>
        </thead>
        <tbody>
          {(q.data ?? []).map((r) => {
            const d = r.data as unknown as SalaryStructureData;
            return (
              <tr
                key={r.id}
                style={{ borderTop: "1px solid #e5e7eb", cursor: "pointer" }}
                onClick={() => nav(`/records/${KTYPE_STRUCTURE}/${r.id}`)}
              >
                <Td>{d.employee_id ?? ""}</Td>
                <Td>{d.effective_from ?? ""}</Td>
                <Td>{d.base_salary ?? 0}</Td>
                <Td>{d.currency ?? "USD"}</Td>
                <Td>{d.payment_frequency ?? ""}</Td>
                <Td>{d.status ?? ""}</Td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </>
  );
}

function Th({ children }: { children: React.ReactNode }) {
  return <th style={{ padding: "6px 8px", fontWeight: 500, fontSize: 12 }}>{children}</th>;
}

function Td({ children }: { children: React.ReactNode }) {
  return <td style={{ padding: "8px" }}>{children}</td>;
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

  const generate = useMutation({
    mutationFn: (id: string) => api.generatePayslips(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", KTYPE_PAYRUN] });
      qc.invalidateQueries({ queryKey: ["records", KTYPE_PAYSLIP] });
    },
  });

  const post = useMutation({
    mutationFn: (id: string) => api.postPayRun(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records", KTYPE_PAYRUN] });
      qc.invalidateQueries({ queryKey: ["records", KTYPE_PAYSLIP] });
    },
  });

  return (
    <>
      {runs.isLoading && <p>Loading…</p>}
      {runs.isError && <p style={{ color: "#b91c1c" }}>Failed to load.</p>}
      <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
        <thead>
          <tr style={{ textAlign: "left", color: "#6b7280" }}>
            <Th>Name</Th>
            <Th>Period</Th>
            <Th>Department</Th>
            <Th>Slips</Th>
            <Th>Total Gross</Th>
            <Th>Total Net</Th>
            <Th>Status</Th>
            <Th>Actions</Th>
          </tr>
        </thead>
        <tbody>
          {(runs.data ?? []).map((r) => {
            const d = r.data as unknown as PayRunData;
            const isSelected = selectedRunId === r.id;
            const busy =
              (generate.isPending && generate.variables === r.id) ||
              (post.isPending && post.variables === r.id);
            return (
              <tr
                key={r.id}
                style={{
                  borderTop: "1px solid #e5e7eb",
                  background: isSelected ? "#f3f4f6" : undefined,
                }}
              >
                <Td>
                  <button
                    style={{
                      background: "none",
                      border: 0,
                      padding: 0,
                      color: "#2563eb",
                      cursor: "pointer",
                    }}
                    onClick={() => nav(`/records/${KTYPE_PAYRUN}/${r.id}`)}
                  >
                    {d.name ?? r.id}
                  </button>
                </Td>
                <Td>
                  {d.pay_period_start ?? "?"} → {d.pay_period_end ?? "?"}
                </Td>
                <Td>{d.department ?? ""}</Td>
                <Td>{d.payslip_count ?? 0}</Td>
                <Td>
                  {d.total_gross ?? 0} {d.currency ?? ""}
                </Td>
                <Td>
                  {d.total_net ?? 0} {d.currency ?? ""}
                </Td>
                <Td>{d.status ?? "draft"}</Td>
                <Td>
                  <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
                    <button
                      disabled={busy || (d.status === "paid")}
                      onClick={() => generate.mutate(r.id)}
                      title="Generate draft payslips for eligible employees"
                    >
                      Generate
                    </button>
                    <button
                      disabled={busy || (d.status === "paid")}
                      onClick={() => post.mutate(r.id)}
                      title="Post approved payslips as a journal entry"
                    >
                      Post
                    </button>
                    <button onClick={() => setSelectedRunId(isSelected ? null : r.id)}>
                      {isSelected ? "Hide slips" : "View slips"}
                    </button>
                  </div>
                </Td>
              </tr>
            );
          })}
        </tbody>
      </table>
      {generate.isError && (
        <p style={{ color: "#b91c1c" }}>Generate failed: {String(generate.error)}</p>
      )}
      {post.isError && (
        <p style={{ color: "#b91c1c" }}>Post failed: {String(post.error)}</p>
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
    <p style={{ fontSize: 12, color: "#374151", marginTop: 12 }}>
      Created {summary.created_count} slip(s); skipped{" "}
      {summary.skipped_existing} existing, {summary.skipped_no_structure} without a salary
      structure.
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
    <section style={{ marginTop: 16 }}>
      <h3 style={{ fontSize: 14 }}>Payslips</h3>
      {slips.isLoading && <p>Loading slips…</p>}
      {slips.isError && <p style={{ color: "#b91c1c" }}>Failed to load slips.</p>}
      <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
        <thead>
          <tr style={{ textAlign: "left", color: "#6b7280" }}>
            <Th>Employee</Th>
            <Th>Gross</Th>
            <Th>Deductions</Th>
            <Th>Net</Th>
            <Th>Currency</Th>
            <Th>Status</Th>
          </tr>
        </thead>
        <tbody>
          {(slips.data ?? []).map((r) => {
            const d = r.data as unknown as PayslipData;
            return (
              <tr key={r.id} style={{ borderTop: "1px solid #e5e7eb" }}>
                <Td>{d.employee_id ?? ""}</Td>
                <Td>{d.gross_pay ?? 0}</Td>
                <Td>{d.total_deductions ?? 0}</Td>
                <Td>{d.net_pay ?? 0}</Td>
                <Td>{d.currency ?? ""}</Td>
                <Td>{d.status ?? "draft"}</Td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </section>
  );
}
