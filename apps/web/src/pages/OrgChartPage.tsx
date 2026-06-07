import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { Badge } from "@kapp/ui";
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
      <p className="text-fg-muted">
        Reporting hierarchy derived from the hr.employee `reporting_to`
        field. Employees with no manager (or whose manager is outside
        this tenant) appear as roots.
      </p>
      {employeesQ.isLoading && <p>Loading…</p>}
      {employeesQ.isError && (
        <p className="text-danger">
          Failed to load employees: {(employeesQ.error as Error).message}
        </p>
      )}
      {employeesQ.data && tree.roots.length === 0 && (
        <p className="text-fg-muted">No employees yet.</p>
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
    <ul className="mt-3 list-none pl-0">
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
    <li className="mb-1.5 ml-1 border-l-2 border-border pl-3">
      <div className="flex items-baseline gap-2">
        <strong>{node.name}</strong>
        {node.designation && (
          <span className="text-[13px] text-fg-muted">
            — {node.designation}
          </span>
        )}
        {node.department && (
          <span className="text-xs text-fg-muted">
            ({node.department})
          </span>
        )}
        {node.status && node.status !== "active" && (
          <Badge variant="warning" size="sm">
            {node.status}
          </Badge>
        )}
      </div>
      {node.email && (
        <div className="text-xs text-fg-muted">{node.email}</div>
      )}
      {kids.length > 0 && (
        <ul className="mt-1.5 list-none pl-3">
          {kids.map((c) => (
            <TreeNode key={c.id} node={c} childrenByParent={childrenByParent} />
          ))}
        </ul>
      )}
    </li>
  );
}
