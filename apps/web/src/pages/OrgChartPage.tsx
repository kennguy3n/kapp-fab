import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { api } from "../lib/api";

/**
 * OrgChartPage renders a reporting hierarchy from the hr.employee
 * KType's `reporting_to` field. Employees whose `reporting_to` is
 * empty (or whose manager is outside the returned set) are treated as
 * roots so deactivated managers or cross-tenant references surface as
 * top-level nodes rather than disappearing from the tree.
 *
 * MVP shape: a nested <ul> tree with one row per employee. No drag /
 * expand-collapse / reporting-path search — those follow once the HR
 * data model stabilizes.
 */
export function OrgChartPage() {
  const employeesQ = useQuery({
    queryKey: ["records", "hr.employee"],
    queryFn: () => api.listRecords("hr.employee"),
  });

  const tree = useMemo(() => buildTree(employeesQ.data ?? []), [employeesQ.data]);

  return (
    <section>
      <h1>Org Chart</h1>
      <p style={{ color: "#6b7280" }}>
        Reporting hierarchy derived from the hr.employee `reporting_to`
        field. Employees with no manager (or whose manager is outside
        this tenant) appear as roots.
      </p>
      {employeesQ.isLoading && <p>Loading…</p>}
      {employeesQ.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load employees: {(employeesQ.error as Error).message}
        </p>
      )}
      {employeesQ.data && tree.roots.length === 0 && (
        <p style={{ color: "#6b7280" }}>No employees yet.</p>
      )}
      {employeesQ.data && tree.roots.length > 0 && (
        <TreeList nodes={tree.roots} childrenByParent={tree.childrenByParent} />
      )}
    </section>
  );
}

interface EmployeeNode {
  id: string;
  name: string;
  designation?: string;
  department?: string;
  email?: string;
  status?: string;
}

interface TreeShape {
  roots: EmployeeNode[];
  childrenByParent: Map<string, EmployeeNode[]>;
}

function buildTree(records: KRecord[]): TreeShape {
  const nodes: EmployeeNode[] = records.map((r) => {
    const d = r.data as Record<string, unknown>;
    return {
      id: r.id,
      name: stringField(d.name) ?? "(unnamed)",
      designation: stringField(d.designation),
      department: stringField(d.department),
      email: stringField(d.email),
      status: stringField(d.status),
    };
  });
  const byId = new Map<string, EmployeeNode>();
  nodes.forEach((n) => byId.set(n.id, n));
  const childrenByParent = new Map<string, EmployeeNode[]>();
  const roots: EmployeeNode[] = [];
  nodes.forEach((n) => {
    const raw = (records.find((r) => r.id === n.id)?.data ?? {}) as Record<
      string,
      unknown
    >;
    const managerId = stringField(raw.reporting_to);
    if (managerId && byId.has(managerId)) {
      const siblings = childrenByParent.get(managerId) ?? [];
      siblings.push(n);
      childrenByParent.set(managerId, siblings);
    } else {
      roots.push(n);
    }
  });
  const sortByName = (a: EmployeeNode, b: EmployeeNode) =>
    a.name.localeCompare(b.name);
  roots.sort(sortByName);
  childrenByParent.forEach((kids) => kids.sort(sortByName));
  return { roots, childrenByParent };
}

function stringField(v: unknown): string | undefined {
  if (typeof v !== "string") return undefined;
  const s = v.trim();
  return s ? s : undefined;
}

function TreeList({
  nodes,
  childrenByParent,
}: {
  nodes: EmployeeNode[];
  childrenByParent: Map<string, EmployeeNode[]>;
}) {
  return (
    <ul style={{ listStyle: "none", paddingLeft: 0, marginTop: 12 }}>
      {nodes.map((n) => (
        <TreeNode key={n.id} node={n} childrenByParent={childrenByParent} />
      ))}
    </ul>
  );
}

function TreeNode({
  node,
  childrenByParent,
}: {
  node: EmployeeNode;
  childrenByParent: Map<string, EmployeeNode[]>;
}) {
  const kids = childrenByParent.get(node.id) ?? [];
  return (
    <li
      style={{
        borderLeft: "2px solid #e5e7eb",
        paddingLeft: 12,
        marginLeft: 4,
        marginBottom: 6,
      }}
    >
      <div style={{ display: "flex", alignItems: "baseline", gap: 8 }}>
        <strong>{node.name}</strong>
        {node.designation && (
          <span style={{ color: "#4b5563", fontSize: 13 }}>
            — {node.designation}
          </span>
        )}
        {node.department && (
          <span style={{ color: "#6b7280", fontSize: 12 }}>
            ({node.department})
          </span>
        )}
        {node.status && node.status !== "active" && (
          <span
            style={{
              fontSize: 11,
              color: "#92400e",
              background: "#fef3c7",
              padding: "1px 6px",
              borderRadius: 3,
            }}
          >
            {node.status}
          </span>
        )}
      </div>
      {node.email && (
        <div style={{ color: "#6b7280", fontSize: 12 }}>{node.email}</div>
      )}
      {kids.length > 0 && (
        <ul style={{ listStyle: "none", paddingLeft: 12, marginTop: 6 }}>
          {kids.map((c) => (
            <TreeNode key={c.id} node={c} childrenByParent={childrenByParent} />
          ))}
        </ul>
      )}
    </li>
  );
}
