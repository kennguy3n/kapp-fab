import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import type { KRecord } from "@kapp/client";
import { api } from "../lib/api";

const KTYPE = "sales.price_list";

interface PriceListItem {
  item_id: string;
  price: number | string;
  discount_percent?: number | string;
  min_qty?: number | string;
}

interface PriceListData {
  name?: string;
  currency?: string;
  customer_id?: string;
  valid_from?: string;
  valid_until?: string;
  items?: PriceListItem[];
  active?: boolean;
}

/**
 * PriceListsPage lists `sales.price_list` KRecords and lets the user
 * drill into one to edit its `items` matrix. Editing a single row
 * issues a PATCH against the whole record — price lists are low-cardinality
 * so the naive replace-all update is acceptable.
 */
export function PriceListsPage() {
  const qc = useQueryClient();
  const q = useQuery<KRecord[]>({
    queryKey: ["records", KTYPE],
    queryFn: () => api.listRecords(KTYPE),
  });
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const selected = useMemo(
    () => (q.data ?? []).find((r) => r.id === selectedId) ?? null,
    [q.data, selectedId]
  );

  const updateMutation = useMutation({
    mutationFn: (r: KRecord) => api.updateRecord(KTYPE, r.id, r.data),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["records", KTYPE] }),
  });

  return (
    <section style={{ display: "flex", gap: 16 }}>
      <div style={{ flex: "0 0 300px" }}>
        <h1>Price Lists</h1>
        {q.isLoading && <p>Loading…</p>}
        <ul style={{ listStyle: "none", padding: 0, fontSize: 13 }}>
          {(q.data ?? []).map((r) => {
            const d = r.data as unknown as PriceListData;
            const isSel = selectedId === r.id;
            return (
              <li
                key={r.id}
                onClick={() => setSelectedId(r.id)}
                style={{
                  padding: "6px 8px",
                  cursor: "pointer",
                  background: isSel ? "#eef2ff" : "transparent",
                  borderRadius: 4,
                }}
              >
                <div style={{ fontWeight: 500 }}>{d.name ?? "(unnamed)"}</div>
                <div style={{ color: "#6b7280", fontSize: 12 }}>
                  {d.currency ?? "—"} · {d.customer_id ?? "all customers"}
                </div>
              </li>
            );
          })}
        </ul>
      </div>
      <div style={{ flex: 1 }}>
        {selected ? (
          <PriceListEditor
            record={selected}
            onSave={(r) => updateMutation.mutate(r)}
            saving={updateMutation.isPending}
          />
        ) : (
          <p style={{ color: "#6b7280" }}>
            Select a price list to edit its item matrix.
          </p>
        )}
      </div>
    </section>
  );
}

function PriceListEditor({
  record,
  onSave,
  saving,
}: {
  record: KRecord;
  onSave: (r: KRecord) => void;
  saving: boolean;
}) {
  const initial = record.data as unknown as PriceListData;
  const [items, setItems] = useState<PriceListItem[]>(initial.items ?? []);

  const updateRow = (i: number, patch: Partial<PriceListItem>) => {
    setItems((prev) => prev.map((row, idx) => (idx === i ? { ...row, ...patch } : row)));
  };
  const addRow = () =>
    setItems((prev) => [...prev, { item_id: "", price: 0 }]);
  const removeRow = (i: number) =>
    setItems((prev) => prev.filter((_, idx) => idx !== i));

  const save = () => {
    onSave({
      ...record,
      data: { ...record.data, items },
    });
  };

  return (
    <div>
      <h2>{initial.name}</h2>
      <div style={{ color: "#6b7280", fontSize: 13 }}>
        {initial.currency ?? "—"} · valid {initial.valid_from ?? "—"} to {initial.valid_until ?? "—"}
      </div>

      <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13, marginTop: 12 }}>
        <thead>
          <tr style={{ textAlign: "left", color: "#6b7280" }}>
            <th style={{ padding: 6 }}>Item</th>
            <th style={{ padding: 6 }}>Price</th>
            <th style={{ padding: 6 }}>Discount %</th>
            <th style={{ padding: 6 }}>Min qty</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {items.map((r, i) => (
            <tr key={i} style={{ borderTop: "1px solid #e5e7eb" }}>
              <td style={{ padding: 6 }}>
                <input
                  value={r.item_id}
                  onChange={(e) => updateRow(i, { item_id: e.target.value })}
                />
              </td>
              <td style={{ padding: 6 }}>
                <input
                  type="number"
                  value={String(r.price)}
                  onChange={(e) => updateRow(i, { price: Number(e.target.value) })}
                />
              </td>
              <td style={{ padding: 6 }}>
                <input
                  type="number"
                  value={String(r.discount_percent ?? 0)}
                  onChange={(e) => updateRow(i, { discount_percent: Number(e.target.value) })}
                />
              </td>
              <td style={{ padding: 6 }}>
                <input
                  type="number"
                  value={String(r.min_qty ?? 0)}
                  onChange={(e) => updateRow(i, { min_qty: Number(e.target.value) })}
                />
              </td>
              <td style={{ padding: 6 }}>
                <button onClick={() => removeRow(i)}>Remove</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <div style={{ marginTop: 8, display: "flex", gap: 8 }}>
        <button onClick={addRow}>Add row</button>
        <button onClick={save} disabled={saving}>
          {saving ? "Saving…" : "Save"}
        </button>
      </div>
    </div>
  );
}
