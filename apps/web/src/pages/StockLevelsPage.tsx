import { useMemo } from "react";
import { useQueries, useQuery } from "@tanstack/react-query";
import type { InventoryBatch } from "@kapp/client";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
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
  // useQueries returns a fresh array reference on every render, so
  // depending on `batchQueries` directly defeats the memo. Hash the
  // stable .data values into a string key the memo can compare.
  const batchData = batchQueries.map((q) => q.data);
  const batchDataKey = batchData
    .map((d) => (d ? d.map((b) => b.id).join(",") : ""))
    .join("|");
  const batchesByItem = useMemo(() => {
    const m = new Map<string, InventoryBatch[]>();
    itemIds.forEach((id, i) => {
      const data = batchData[i];
      if (data && data.length > 0) m.set(id, data);
    });
    return m;
    // batchDataKey captures the meaningful identity of every per-item
    // batch list; itemIds is the stable item-set membership.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [itemIds, batchDataKey]);

  return (
    <section>
      <h1>Stock Levels</h1>
      <p className="text-fg-muted">
        Live SUM(qty) from the append-only inventory_moves ledger.
      </p>
      {levelsQ.isLoading && <p>Loading…</p>}
      {levelsQ.isError && (
        <p className="text-danger">
          Failed to load stock levels: {(levelsQ.error as Error).message}
        </p>
      )}
      {levelsQ.data && (
        <Table className="mt-3 text-[13px]">
          <TableHeader>
            <TableRow>
              <TableHead>Item</TableHead>
              <TableHead>Warehouse</TableHead>
              <TableHead className="text-right">Qty</TableHead>
              <TableHead>Batches</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {levelsQ.data.map((r) => {
              const batches = batchesByItem.get(r.item_id) ?? [];
              return (
                <TableRow key={`${r.item_id}:${r.warehouse_id}`}>
                  <TableCell>
                    {itemLabel.get(r.item_id) ?? r.item_id}
                  </TableCell>
                  <TableCell>
                    {whLabel.get(r.warehouse_id) ?? r.warehouse_id}
                  </TableCell>
                  <TableCell className="text-right">{r.qty}</TableCell>
                  <TableCell className={batches.length === 0 ? "text-fg-subtle" : undefined}>
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
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
        </Table>
      )}
    </section>
  );
}
