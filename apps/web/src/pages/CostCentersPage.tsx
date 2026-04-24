import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { api } from "../lib/api";

// KType for cost centres; mirrors the constant in
// internal/ledger/cost_center.go. Hard-coded here because the page
// only ever drives this one KType.
const KTYPE = "finance.cost_center";

interface CostCenterData {
  code: string;
  name: string;
  parent_code?: string;
  active?: boolean;
}

/**
 * CostCentersPage renders the tenant's cost-centre tree and supports
 * inline create / toggle-active. The hierarchy is materialised from
 * the flat `parent_code` pointer on the server rows; we do a single
 * client-side pass to build the children map so no extra round-trip
 * is needed per node.
 */
export function CostCentersPage() {
  const qc = useQueryClient();
  const q = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE],
    queryFn: () => api.listRecords(KTYPE),
  });

  const records = q.data ?? [];
  const tree = useMemo(() => buildTree(records), [records]);

  const createMutation = useMutation({
    mutationFn: (data: CostCenterData) =>
      api.createRecord(KTYPE, data as unknown as Record<string, unknown>),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["records", KTYPE] }),
  });

  const toggleMutation = useMutation({
    mutationFn: (r: KRecord) => {
      const d = r.data as unknown as CostCenterData;
      return api.updateRecord(KTYPE, r.id, { ...d, active: !d.active });
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["records", KTYPE] }),
  });

  const [form, setForm] = useState<CostCenterData>({
    code: "",
    name: "",
    parent_code: "",
    active: true,
  });

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    if (!form.code || !form.name) return;
    createMutation.mutate({
      code: form.code,
      name: form.name,
      parent_code: form.parent_code || undefined,
      active: form.active ?? true,
    });
    setForm({ code: "", name: "", parent_code: "", active: true });
  };

  return (
    <section>
      <h1>Cost Centers</h1>
      <p style={{ color: "#6b7280" }}>
        GL posting tag used to partition reports. Hierarchy is flat
        pointer → tree.
      </p>

      <form onSubmit={submit} style={{ margin: "12px 0", display: "flex", gap: 8, fontSize: 13 }}>
        <input
          placeholder="code"
          value={form.code}
          onChange={(e) => setForm({ ...form, code: e.target.value })}
          required
        />
        <input
          placeholder="name"
          value={form.name}
          onChange={(e) => setForm({ ...form, name: e.target.value })}
          required
        />
        <input
          placeholder="parent_code (optional)"
          value={form.parent_code ?? ""}
          onChange={(e) => setForm({ ...form, parent_code: e.target.value })}
        />
        <button type="submit" disabled={createMutation.isPending}>
          {createMutation.isPending ? "Adding…" : "Add"}
        </button>
      </form>

      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load cost centres: {(q.error as Error).message}
        </p>
      )}

      {records.length === 0 && !q.isLoading && (
        <p style={{ color: "#9ca3af", fontStyle: "italic" }}>
          No cost centres yet.
        </p>
      )}

      <ul style={{ listStyle: "none", padding: 0, marginTop: 12, fontSize: 13 }}>
        {(tree.get("") ?? []).map((r) => (
          <CostCenterNode
            key={r.id}
            node={r}
            children={tree}
            depth={0}
            onToggle={(cc) => toggleMutation.mutate(cc)}
          />
        ))}
      </ul>
    </section>
  );
}

function CostCenterNode({
  node,
  children,
  depth,
  onToggle,
}: {
  node: KRecord;
  children: Map<string, KRecord[]>;
  depth: number;
  onToggle: (r: KRecord) => void;
}) {
  const d = node.data as unknown as CostCenterData;
  const kids = children.get(d.code) ?? [];
  return (
    <li style={{ marginLeft: depth * 16, padding: "4px 0" }}>
      <span style={{ color: d.active === false ? "#9ca3af" : "inherit" }}>
        <code>{d.code}</code> — {d.name}
      </span>
      <button
        style={{ marginLeft: 8, fontSize: 11 }}
        onClick={() => onToggle(node)}
      >
        {d.active === false ? "Activate" : "Deactivate"}
      </button>
      {kids.length > 0 && (
        <ul style={{ listStyle: "none", padding: 0 }}>
          {kids.map((c) => (
            <CostCenterNode
              key={c.id}
              node={c}
              children={children}
              depth={depth + 1}
              onToggle={onToggle}
            />
          ))}
        </ul>
      )}
    </li>
  );
}

// buildTree indexes records by parent_code so the render can walk the
// hierarchy in O(n). Entries with no parent or a parent that isn't in
// the set are treated as roots.
function buildTree(records: KRecord[]): Map<string, KRecord[]> {
  const out = new Map<string, KRecord[]>();
  const codes = new Set(records.map((r) => (r.data as unknown as CostCenterData).code));
  for (const r of records) {
    const d = r.data as unknown as CostCenterData;
    const parent = d.parent_code && codes.has(d.parent_code) ? d.parent_code : "";
    (out.get(parent) ?? out.set(parent, []).get(parent)!).push(r);
  }
  for (const arr of out.values()) {
    arr.sort((a, b) => {
      const ac = (a.data as unknown as CostCenterData).code;
      const bc = (b.data as unknown as CostCenterData).code;
      return ac < bc ? -1 : ac > bc ? 1 : 0;
    });
  }
  return out;
}
