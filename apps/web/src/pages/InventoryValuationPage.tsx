import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
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
 * as of a user-chosen date. Rows sum qty * unit_cost per
 * (item, warehouse); the total must equal SUM(rows.value).
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
      <p style={{ color: "#6b7280" }}>
        Qty × unit cost per (item, warehouse) as of the selected date.
      </p>
      <div style={{ margin: "12px 0", fontSize: 13 }}>
        <label style={{ marginRight: 8 }}>As of:</label>
        <input
          type="date"
          value={asOf}
          onChange={(e) => setAsOf(e.target.value)}
        />
      </div>
      {q.isLoading && <p>Loading…</p>}
      {q.isError && (
        <p style={{ color: "#b91c1c" }}>
          Failed to load report: {(q.error as Error).message}
        </p>
      )}
      {report && (
        <>
          <table
            style={{ width: "100%", borderCollapse: "collapse", fontSize: 13, marginTop: 12 }}
          >
            <thead>
              <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
                <th style={{ padding: "6px 8px" }}>Item</th>
                <th style={{ padding: "6px 8px" }}>Warehouse</th>
                <th style={{ padding: "6px 8px", textAlign: "right" }}>Qty</th>
                <th style={{ padding: "6px 8px", textAlign: "right" }}>Unit Cost</th>
                <th style={{ padding: "6px 8px", textAlign: "right" }}>Value</th>
              </tr>
            </thead>
            <tbody>
              {report.rows.map((r) => (
                <tr
                  key={`${r.item_id}:${r.warehouse_id}`}
                  style={{ borderBottom: "1px solid #f3f4f6" }}
                >
                  <td style={{ padding: "6px 8px" }}>{r.item_id}</td>
                  <td style={{ padding: "6px 8px" }}>{r.warehouse_id}</td>
                  <td style={{ padding: "6px 8px", textAlign: "right" }}>{r.qty}</td>
                  <td style={{ padding: "6px 8px", textAlign: "right" }}>{r.unit_cost}</td>
                  <td style={{ padding: "6px 8px", textAlign: "right" }}>{r.value}</td>
                </tr>
              ))}
            </tbody>
            <tfoot>
              <tr style={{ borderTop: "1px solid #e5e7eb", fontWeight: 600 }}>
                <td colSpan={4} style={{ padding: "6px 8px", textAlign: "right" }}>
                  Total
                </td>
                <td style={{ padding: "6px 8px", textAlign: "right" }}>
                  {report.total_value}
                </td>
              </tr>
            </tfoot>
          </table>
        </>
      )}
    </section>
  );
}
