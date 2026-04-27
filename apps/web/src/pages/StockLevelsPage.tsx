import { useMemo } from "react";
import { useQueries, useQuery } from "@tanstack/react-query";
import type { InventoryBatch } from "@kapp/client";
import { api } from "../lib/api";

/**
 * StockLevelsPage renders the `stock_levels` view — one row per
 * (item, warehouse) with the running SUM(qty) from the append-only
 * `inventory_moves` ledger. Items/warehouses are fetched alongside so
 * the UI can show human-readable SKUs and warehouse codes instead of
 * bare UUIDs.
 */
export function StockLevelsPage() {
  const levelsQ = useQuery({
    queryKey: ["inventory", "stock-levels"],
    queryFn: () => api.listStockLevels(),
  });
  const itemsQ = useQuery({
    queryKey: ["inventory", "items"],
    queryFn: () => api.listInventoryItems(),
  });
  const warehousesQ = useQuery({
    queryKey: ["inventory", "warehouses"],
    queryFn: () => api.listInventoryWarehouses(),
  });

  const itemLabel = useMemo(() => {
    const m = new Map<string, string>();
    (itemsQ.data ?? []).forEach((it) => m.set(it.id, `${it.sku} — ${it.name}`));
    return m;
  }, [itemsQ.data]);
  const whLabel = useMemo(() => {
    const m = new Map<string, string>();
    (warehousesQ.data ?? []).forEach((w) => m.set(w.id, `${w.code} — ${w.name}`));
    return m;
  }, [warehousesQ.data]);

  // Fan out one batches request per item we have stock for. Items
  // without any batches return [] and render the standard "—" cell.
  // Phase G/L: the Batches column unlocks FEFO inspection on the
  // same page operators already use to read live stock.
  const itemIds = useMemo(() => {
    const seen = new Set<string>();
    (levelsQ.data ?? []).forEach((r) => seen.add(r.item_id));
    return Array.from(seen);
  }, [levelsQ.data]);
  const batchQueries = useQueries({
    queries: itemIds.map((id) => ({
      queryKey: ["inventory", "batches", id],
      queryFn: () => api.listInventoryBatchesByItem(id),
      staleTime: 60_000,
    })),
  });
  const batchesByItem = useMemo(() => {
    const m = new Map<string, InventoryBatch[]>();
    itemIds.forEach((id, i) => {
      const data = batchQueries[i]?.data;
      if (data && data.length > 0) m.set(id, data);
    });
    return m;
  }, [itemIds, batchQueries]);

  return (
    <section>
      <h1>Stock Levels</h1>
      <p style={{ color: "#6b7280" }}>
        Live SUM(qty) from the append-only inventory_moves ledger.
      </p>
      {levelsQ.isLoading && <p>Loading…</p>}
      {levelsQ.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load stock levels: {(levelsQ.error as Error).message}
        </p>
      )}
      {levelsQ.data && (
        <table
          style={{ width: "100%", borderCollapse: "collapse", fontSize: 13, marginTop: 12 }}
        >
          <thead>
            <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
              <th style={{ padding: "6px 8px" }}>Item</th>
              <th style={{ padding: "6px 8px" }}>Warehouse</th>
              <th style={{ padding: "6px 8px", textAlign: "right" }}>Qty</th>
              <th style={{ padding: "6px 8px" }}>Batches</th>
            </tr>
          </thead>
          <tbody>
            {levelsQ.data.map((r) => {
              const batches = batchesByItem.get(r.item_id) ?? [];
              return (
                <tr key={`${r.item_id}:${r.warehouse_id}`} style={{ borderBottom: "1px solid #f3f4f6" }}>
                  <td style={{ padding: "6px 8px" }}>
                    {itemLabel.get(r.item_id) ?? r.item_id}
                  </td>
                  <td style={{ padding: "6px 8px" }}>
                    {whLabel.get(r.warehouse_id) ?? r.warehouse_id}
                  </td>
                  <td style={{ padding: "6px 8px", textAlign: "right" }}>{r.qty}</td>
                  <td style={{ padding: "6px 8px", color: batches.length === 0 ? "#9ca3af" : undefined }}>
                    {batches.length === 0
                      ? "—"
                      : batches
                          .slice(0, 3)
                          .map((b) =>
                            b.expires_at
                              ? `${b.batch_no} (exp ${b.expires_at.slice(0, 10)})`
                              : b.batch_no,
                          )
                          .join(", ") + (batches.length > 3 ? `, +${batches.length - 3}` : "")}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </section>
  );
}
