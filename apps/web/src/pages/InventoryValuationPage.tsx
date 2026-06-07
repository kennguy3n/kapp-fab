import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  Input,
  Table,
  TableBody,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

const todayLocalISO = (() => {
  const d = new Date();
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${y}-${m}-${day}`;
})();

/**
 * InventoryValuationPage shows the monetary value of on-hand stock
 * as of a user-chosen date. Rows are grouped per item across all
 * warehouses; total_value equals SUM(rows.value_cost).
 */
export function InventoryValuationPage() {
  const [asOf, setAsOf] = useState<string>(todayLocalISO);
  const q = useQuery({
    queryKey: ["inventory", "valuation", asOf],
    queryFn: () => api.getInventoryValuation(asOf),
  });
  const report = q.data;
  return (
    <section>
      <h1>Inventory Valuation</h1>
      <p className="text-fg-muted">
        Qty × unit cost per (item, warehouse) as of the selected date.
      </p>
      <div className="my-3 flex items-center gap-2 text-[13px]">
        <label>As of:</label>
        <Input
          type="date"
          value={asOf}
          onChange={(e) => setAsOf(e.target.value)}
          className="w-auto"
        />
      </div>
      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p className="text-danger">
          Failed to load report: {(q.error as Error).message}
        </p>
      )}
      {report && (
        <Table className="mt-3 text-[13px]">
          <TableHeader>
            <TableRow>
              <TableHead>SKU</TableHead>
              <TableHead>Item</TableHead>
              <TableHead className="text-right">Qty</TableHead>
              <TableHead className="text-right">Value</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {report.rows.map((r) => (
              <TableRow key={r.item_id}>
                <TableCell>{r.sku}</TableCell>
                <TableCell>{r.name}</TableCell>
                <TableCell className="text-right">{r.qty}</TableCell>
                <TableCell className="text-right">{r.value_cost}</TableCell>
              </TableRow>
            ))}
          </TableBody>
          <TableFooter>
            <TableRow className="font-semibold">
              <TableCell colSpan={3} className="text-right">
                Total
              </TableCell>
              <TableCell className="text-right">{report.total_value}</TableCell>
            </TableRow>
          </TableFooter>
        </Table>
      )}
    </section>
  );
}
