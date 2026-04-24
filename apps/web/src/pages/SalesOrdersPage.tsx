import { useMemo } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { api } from "../lib/api";

const KTYPE = "sales.order";

interface SalesOrderData {
  order_number?: string;
  customer_id?: string;
  order_date?: string;
  total?: number | string;
  currency?: string;
  status?: string;
}

// STAGES mirrors the workflow states in internal/sales/ktypes.go so
// the kanban matches what the engine accepts. Keeping the list here
// avoids a round-trip to the registry for what is a stable constant.
const STAGES: string[] = ["draft", "confirmed", "fulfilled", "cancelled"];

/**
 * SalesOrdersPage is a pipeline-stage kanban over `sales.order`
 * KRecords. Cards show order number, customer ref, and total;
 * clicking a card jumps to the record form for line-item editing.
 */
export function SalesOrdersPage() {
  const nav = useNavigate();
  const qc = useQueryClient();
  const q = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE],
    queryFn: () => api.listRecords(KTYPE),
  });

  const moveMutation = useMutation({
    mutationFn: async ({ r, to }: { r: KRecord; to: string }) => {
      const current = (r.data as unknown as SalesOrderData).status ?? "draft";
      if (current === to) return;
      const action = resolveAction(current, to);
      if (!action) {
        // Fallback to a patch when no workflow edge matches. The
        // server will still enforce the KType constraint.
        await api.updateRecord(KTYPE, r.id, { ...r.data, status: to });
        return;
      }
      await api.runAction(KTYPE, r.id, action);
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ["records", KTYPE] }),
  });

  const columns = useMemo(() => {
    const by = new Map<string, KRecord[]>();
    for (const s of STAGES) by.set(s, []);
    for (const r of q.data ?? []) {
      const s = (r.data as unknown as SalesOrderData).status ?? "draft";
      (by.get(s) ?? by.set(s, []).get(s)!).push(r);
    }
    return by;
  }, [q.data]);

  return (
    <section>
      <header style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
        <h1>Sales Orders</h1>
        <button onClick={() => nav(`/records/${KTYPE}/new`)}>New order</button>
      </header>
      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load orders: {(q.error as Error).message}
        </p>
      )}
      <div style={{ display: "flex", gap: 12, marginTop: 12, overflowX: "auto" }}>
        {STAGES.map((s) => (
          <div
            key={s}
            style={{
              minWidth: 240,
              background: "#f9fafb",
              border: "1px solid #e5e7eb",
              borderRadius: 6,
              padding: 8,
            }}
            onDragOver={(e) => e.preventDefault()}
            onDrop={(e) => {
              const id = e.dataTransfer.getData("text/plain");
              const r = (q.data ?? []).find((x) => x.id === id);
              if (r) moveMutation.mutate({ r, to: s });
            }}
          >
            <div style={{ textTransform: "capitalize", fontSize: 12, color: "#6b7280" }}>
              {s} · {(columns.get(s) ?? []).length}
            </div>
            {(columns.get(s) ?? []).map((r) => {
              const d = r.data as unknown as SalesOrderData;
              return (
                <div
                  key={r.id}
                  draggable
                  onDragStart={(e) => e.dataTransfer.setData("text/plain", r.id)}
                  onClick={() => nav(`/records/${KTYPE}/${r.id}`)}
                  style={{
                    marginTop: 6,
                    padding: 8,
                    background: "white",
                    borderRadius: 4,
                    border: "1px solid #e5e7eb",
                    cursor: "pointer",
                    fontSize: 13,
                  }}
                >
                  <div style={{ fontWeight: 500 }}>
                    {d.order_number ?? r.id.slice(0, 8)}
                  </div>
                  <div style={{ color: "#6b7280", fontSize: 12 }}>
                    {d.customer_id ?? "—"}
                  </div>
                  <div style={{ marginTop: 4, fontSize: 12 }}>
                    {d.total ?? 0} {d.currency ?? "USD"}
                  </div>
                </div>
              );
            })}
          </div>
        ))}
      </div>
    </section>
  );
}

// resolveAction maps (from, to) stage pairs to the workflow action
// names declared in internal/sales/ktypes.go. Invalid transitions
// return undefined so the caller can decide whether to fall back to
// a raw patch or reject the drop.
function resolveAction(from: string, to: string): string | undefined {
  if (from === "draft" && to === "confirmed") return "confirm";
  if (from === "confirmed" && to === "fulfilled") return "fulfil";
  if ((from === "draft" || from === "confirmed") && to === "cancelled") return "cancel";
  return undefined;
}
