import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { CapacityDayLoad, WorkCenterSchedule } from "@kapp/client";
import { api } from "../lib/api";

// isoDate formats a Date as YYYY-MM-DD in UTC, matching the format the
// capacity endpoint expects for its start / end query parameters.
function isoDate(d: Date): string {
  return d.toISOString().slice(0, 10);
}

/**
 * CapacityPlanningPage renders the Stream 2 finite-capacity grid. It
 * queries every released / in-progress work order, joins each to its
 * snapshotted routing operations, and shows utilisation (scheduled vs
 * available minutes) per work center per day over the selected window.
 *
 * v1 is a read-only conflict surface: cells where scheduled minutes
 * exceed available capacity are flagged as overloaded (red) but the
 * planner does not auto-reschedule.
 */
export function CapacityPlanningPage() {
  const today = new Date();
  const weekOut = new Date(today.getTime() + 6 * 24 * 60 * 60 * 1000);
  const [start, setStart] = useState(isoDate(today));
  const [end, setEnd] = useState(isoDate(weekOut));

  const planQ = useQuery({
    queryKey: ["mfg", "capacity", start, end],
    queryFn: () => api.capacityPlan({ start, end }),
  });

  // The day axis is identical for every row, so read it off the first
  // row (all rows share the planner's dayKeys ordering).
  const dayHeaders: string[] =
    planQ.data && planQ.data.rows.length > 0
      ? planQ.data.rows[0].days.map((d) => d.date)
      : [];

  return (
    <section>
      <h1>Capacity Planning</h1>
      <p style={{ color: "#6b7280" }}>
        Finite-capacity utilisation from released / in-progress work orders.
        Overloaded cells are flagged; the planner does not auto-reschedule.
      </p>

      <div style={{ display: "flex", gap: 16, alignItems: "end", marginBottom: 16 }}>
        <label>
          Start
          <input
            type="date"
            value={start}
            onChange={(e) => setStart(e.target.value)}
            style={{ display: "block" }}
          />
        </label>
        <label>
          End
          <input
            type="date"
            value={end}
            onChange={(e) => setEnd(e.target.value)}
            style={{ display: "block" }}
          />
        </label>
      </div>

      {planQ.isLoading && <p>Loading…</p>}
      {planQ.isError && <p style={{ color: "#dc2626" }}>{String(planQ.error)}</p>}
      {planQ.data && planQ.data.rows.length === 0 && (
        <p>No work centers defined yet.</p>
      )}
      {planQ.data && planQ.data.rows.length > 0 && (
        <div style={{ overflowX: "auto" }}>
          <table style={{ borderCollapse: "collapse", minWidth: "100%" }}>
            <thead>
              <tr style={{ borderBottom: "1px solid #e5e7eb" }}>
                <th style={{ textAlign: "left", padding: "4px 8px" }}>
                  Work center
                </th>
                {dayHeaders.map((d) => (
                  <th key={d} style={{ padding: "4px 8px", whiteSpace: "nowrap" }}>
                    {d.slice(5)}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {planQ.data.rows.map((row: WorkCenterSchedule) => (
                <tr key={row.work_center_id} style={{ borderBottom: "1px solid #f3f4f6" }}>
                  <td style={{ padding: "4px 8px", whiteSpace: "nowrap" }}>
                    {row.work_center_name}
                    {row.status !== "active" && (
                      <span style={{ color: "#b45309" }}> ({row.status})</span>
                    )}
                  </td>
                  {row.days.map((day: CapacityDayLoad) => (
                    <td
                      key={day.date}
                      title={`${day.scheduled_minutes} / ${day.available_minutes} min`}
                      style={{
                        padding: "4px 8px",
                        textAlign: "center",
                        background: day.overloaded
                          ? "#fee2e2"
                          : Number(day.utilization_percent) > 0
                            ? "#dcfce7"
                            : "transparent",
                        color: day.overloaded ? "#991b1b" : "#111827",
                        fontWeight: day.overloaded ? 600 : 400,
                      }}
                    >
                      {day.utilization_percent}%
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
