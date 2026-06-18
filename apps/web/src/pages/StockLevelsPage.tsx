import { useMemo, useState } from "react";
import { useQueries, useQuery } from "@tanstack/react-query";
import type { InventoryBatch } from "@kapp/client";
import {
  Badge,
  Button,
  EmptyState,
  Eyebrow,
  Field,
  Select,
  Skeleton,
  Table,
  TableBody,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
  toast,
} from "@kapp/ui";
import { AlertTriangle, Boxes, Download } from "lucide-react";
import { api } from "../lib/api";
import { downloadCsv } from "../lib/csv";
import { useFormatter } from "../lib/i18n";

const ALL_WAREHOUSES = "__all__";

/**
 * StockLevelsPage renders one row per (item, warehouse) with the
 * running on-hand quantity. Items and warehouses are resolved to
 * human-readable labels, low-stock rows are flagged against each
 * item's reorder level, and the list can be filtered per warehouse
 * and exported to CSV.
 */
export function StockLevelsPage() {
  const fmt = useFormatter();
  const [warehouse, setWarehouse] = useState<string>(ALL_WAREHOUSES);

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
  const reorderByItem = useMemo(() => {
    const m = new Map<string, number>();
    (itemsQ.data ?? []).forEach((it) => m.set(it.id, Number(it.reorder_level)));
    return m;
  }, [itemsQ.data]);
  const uomByItem = useMemo(() => {
    const m = new Map<string, string>();
    (itemsQ.data ?? []).forEach((it) => m.set(it.id, it.uom));
    return m;
  }, [itemsQ.data]);
  const whLabel = useMemo(() => {
    const m = new Map<string, string>();
    (warehousesQ.data ?? []).forEach((w) => m.set(w.id, `${w.code} — ${w.name}`));
    return m;
  }, [warehousesQ.data]);

  // Total on-hand per item across every warehouse drives the
  // low-stock comparison against the item's reorder level (which is
  // defined per item, not per location).
  const totalByItem = useMemo(() => {
    const m = new Map<string, number>();
    (levelsQ.data ?? []).forEach((r) => {
      m.set(r.item_id, (m.get(r.item_id) ?? 0) + Number(r.qty));
    });
    return m;
  }, [levelsQ.data]);

  // Fan out one batches request per item we have stock for. Items
  // without any batches return [] and render the standard "—" cell.
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

  const rows = useMemo(() => {
    const all = levelsQ.data ?? [];
    return warehouse === ALL_WAREHOUSES
      ? all
      : all.filter((r) => r.warehouse_id === warehouse);
  }, [levelsQ.data, warehouse]);

  const lowStockCount = useMemo(() => {
    const items = new Set<string>();
    rows.forEach((r) => {
      const reorder = reorderByItem.get(r.item_id) ?? 0;
      if (reorder > 0 && (totalByItem.get(r.item_id) ?? 0) <= reorder) {
        items.add(r.item_id);
      }
    });
    return items.size;
  }, [rows, reorderByItem, totalByItem]);

  function handleExport() {
    const header = ["SKU", "Item", "Warehouse", "Quantity", "Unit"];
    const data = rows.map((r) => {
      const label = itemLabel.get(r.item_id) ?? r.item_id;
      const [sku, ...rest] = label.split(" — ");
      return [
        sku,
        rest.join(" — ") || label,
        whLabel.get(r.warehouse_id) ?? r.warehouse_id,
        r.qty,
        uomByItem.get(r.item_id) ?? "",
      ];
    });
    downloadCsv("stock-levels.csv", header, data);
    toast.success("Export complete", {
      description: `stock-levels.csv · ${data.length} row(s)`,
    });
  }

  const ready = !levelsQ.isLoading && !levelsQ.isError;

  return (
    <section className="flex flex-col gap-4">
      <header className="flex flex-col gap-3">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <Eyebrow>Inventory</Eyebrow>
            <h1 className="mt-1 truncate text-2xl font-semibold tracking-tight text-fg">
              Stock Levels
            </h1>
            <p className="mt-1 text-sm text-fg-muted">
              What you have on hand right now, by item and warehouse.
            </p>
          </div>
          <div className="flex flex-wrap items-end gap-2">
            <Field label="Warehouse" className="w-56">
              <Select
                size="sm"
                value={warehouse}
                onChange={(e) => setWarehouse(e.target.value)}
              >
                <option value={ALL_WAREHOUSES}>All warehouses</option>
                {(warehousesQ.data ?? []).map((w) => (
                  <option key={w.id} value={w.id}>
                    {w.code} — {w.name}
                  </option>
                ))}
              </Select>
            </Field>
            <Button
              size="sm"
              variant="outline"
              leadingIcon={<Download className="size-4" />}
              onClick={handleExport}
              disabled={!ready || rows.length === 0}
            >
              Export CSV
            </Button>
          </div>
        </div>
        {ready && rows.length > 0 && (
          <div className="flex flex-wrap items-center gap-2 text-sm text-fg-muted">
            <span>
              {fmt.number(rows.length)} row{rows.length === 1 ? "" : "s"}
            </span>
            {lowStockCount > 0 && (
              <Badge variant="warning">
                {fmt.number(lowStockCount)} low on stock
              </Badge>
            )}
          </div>
        )}
      </header>

      {levelsQ.isLoading ? (
        <StockLevelsSkeleton />
      ) : levelsQ.isError ? (
        <EmptyState
          icon={<AlertTriangle />}
          title="Couldn't load stock levels"
          description={(levelsQ.error as Error).message}
          action={
            <Button
              variant="secondary"
              onClick={() => void levelsQ.refetch()}
              disabled={levelsQ.isFetching}
            >
              Retry
            </Button>
          }
        />
      ) : rows.length === 0 ? (
        <EmptyState
          icon={<Boxes />}
          title={
            warehouse === ALL_WAREHOUSES
              ? "No stock recorded yet"
              : "No stock in this warehouse"
          }
          description={
            warehouse === ALL_WAREHOUSES
              ? "Stock appears here once items are received or produced."
              : "Try another warehouse, or receive stock into this one."
          }
          action={
            warehouse === ALL_WAREHOUSES ? undefined : (
              <Button
                variant="secondary"
                onClick={() => setWarehouse(ALL_WAREHOUSES)}
              >
                Show all warehouses
              </Button>
            )
          }
        />
      ) : (
        <Table>
          <TableHeader className="sticky top-0 z-10">
            <TableRow>
              <TableHead>Item</TableHead>
              <TableHead>Warehouse</TableHead>
              <TableHead className="text-right">On hand</TableHead>
              <TableHead>Batches</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((r) => {
              const batches = batchesByItem.get(r.item_id) ?? [];
              const qty = Number(r.qty);
              const reorder = reorderByItem.get(r.item_id) ?? 0;
              const itemTotal = totalByItem.get(r.item_id) ?? 0;
              const uom = uomByItem.get(r.item_id);
              const outOfStock = qty <= 0;
              // "Low stock" compares the item's org-wide on-hand total
              // (itemTotal) against its reorder level, NOT this row's
              // single-warehouse qty. Reorder level is defined per item,
              // so the badge reflects whether the item needs reordering
              // overall — it stays consistent even when the table is
              // filtered to one warehouse. "Out of stock" is per-row.
              const lowStock = !outOfStock && reorder > 0 && itemTotal <= reorder;
              return (
                <TableRow key={`${r.item_id}:${r.warehouse_id}`}>
                  <TableCell>
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="font-medium text-fg">
                        {itemLabel.get(r.item_id) ?? r.item_id}
                      </span>
                      {outOfStock && <Badge variant="danger">Out of stock</Badge>}
                      {lowStock && <Badge variant="warning">Low stock</Badge>}
                    </div>
                  </TableCell>
                  <TableCell className="text-fg-muted">
                    {whLabel.get(r.warehouse_id) ?? r.warehouse_id}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    <span className="font-medium text-fg">
                      {fmt.number(qty)}
                    </span>
                    {uom && (
                      <span className="ms-1 text-fg-subtle">{uom}</span>
                    )}
                  </TableCell>
                  <TableCell
                    className={batches.length === 0 ? "text-fg-subtle" : undefined}
                  >
                    {batches.length === 0
                      ? "—"
                      : batches
                          .slice(0, 3)
                          .map((b) =>
                            b.expires_at
                              ? `${b.batch_no} (exp ${fmt.date(new Date(b.expires_at))})`
                              : b.batch_no,
                          )
                          .join(", ") +
                        (batches.length > 3 ? `, +${batches.length - 3} more` : "")}
                  </TableCell>
                </TableRow>
              );
            })}
          </TableBody>
          <TableFooter>
            <TableRow>
              <TableCell colSpan={4} className="text-fg-muted">
                {fmt.number(rows.length)} row{rows.length === 1 ? "" : "s"}
                {warehouse === ALL_WAREHOUSES
                  ? " across all warehouses"
                  : ` in ${whLabel.get(warehouse) ?? "this warehouse"}`}
              </TableCell>
            </TableRow>
          </TableFooter>
        </Table>
      )}
    </section>
  );
}

/** Loading placeholder matching the stock-levels table shape. */
function StockLevelsSkeleton() {
  return (
    <div className="flex flex-col gap-2">
      <Skeleton className="h-9 w-full" />
      {Array.from({ length: 6 }).map((_, i) => (
        <Skeleton key={i} className="h-11 w-full" />
      ))}
    </div>
  );
}
