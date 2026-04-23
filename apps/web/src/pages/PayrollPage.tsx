import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { api } from "../lib/api";

const KTYPE_COMPONENT = "hr.salary_component";
const KTYPE_STRUCTURE = "hr.salary_structure";

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

type Tab = "components" | "structures";

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
        <button
          style={{ marginLeft: "auto" }}
          onClick={() =>
            nav(tab === "components" ? `/records/${KTYPE_COMPONENT}/new` : `/records/${KTYPE_STRUCTURE}/new`)
          }
        >
          New {tab === "components" ? "component" : "structure"}
        </button>
      </div>

      {tab === "components" ? <ComponentsTable /> : <StructuresTable />}
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
