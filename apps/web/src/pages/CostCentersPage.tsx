import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { Button, Input } from "@kapp/ui";
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
      <p className="text-fg-muted">
        GL posting tag used to partition reports. Hierarchy is flat
        pointer → tree.
      </p>

      <form onSubmit={submit} className="my-3 flex gap-2 text-[13px]">
        <Input
          placeholder="code"
          value={form.code}
          onChange={(e) => setForm({ ...form, code: e.target.value })}
          required
        />
        <Input
          placeholder="name"
          value={form.name}
          onChange={(e) => setForm({ ...form, name: e.target.value })}
          required
        />
        <Input
          placeholder="parent_code (optional)"
          value={form.parent_code ?? ""}
          onChange={(e) => setForm({ ...form, parent_code: e.target.value })}
        />
        <Button type="submit" disabled={createMutation.isPending}>
          {createMutation.isPending ? "Adding…" : "Add"}
        </Button>
      </form>

      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p className="text-danger">
          Failed to load cost centres: {(q.error as Error).message}
        </p>
      )}

      {records.length === 0 && !q.isLoading && (
        <p className="italic text-fg-subtle">
          No cost centres yet.
        </p>
      )}

      <ul className="mt-3 list-none p-0 text-[13px]">
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
    <li className="py-1" style={{ marginLeft: depth * 16 }}>
      <span className={d.active === false ? "text-fg-subtle" : undefined}>
        <code>{d.code}</code> — {d.name}
      </span>
      <Button
        variant="link"
        size="sm"
        className="ml-2 h-auto p-0 text-[11px]"
        onClick={() => onToggle(node)}
      >
        {d.active === false ? "Activate" : "Deactivate"}
      </Button>
      {kids.length > 0 && (
        <ul className="list-none p-0">
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
