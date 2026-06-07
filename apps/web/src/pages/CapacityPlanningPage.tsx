import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { CapacityDayLoad, WorkCenterSchedule } from "@kapp/client";
import {
  Input,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@kapp/ui";
import { api } from "../lib/api";

// isoDate formats a Date as YYYY-MM-DD using its LOCAL calendar date,
// matching the format the capacity endpoint expects for its start / end
// query parameters. Local (not UTC) so the default window opens on the
// user's "today": toISOString() would render the UTC date, which for a
// user east of UTC shortly after local midnight is still yesterday,
// defaulting the picker to the wrong day. The server treats the date
// string as a calendar day (truncated to midnight UTC), so the grid the
// user sees lines up with the date they picked.
function isoDate(d: Date): string {
  const year = d.getFullYear();
  const month = String(d.getMonth() + 1).padStart(2, "0");
  const day = String(d.getDate()).padStart(2, "0");
  return `${year}-${month}-${day}`;
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
  // Advance by calendar days rather than adding 6*24h of milliseconds:
  // setDate normalises across DST transitions, so the default window is
  // always exactly 7 calendar days. Millisecond arithmetic would land an
  // hour off on a DST boundary and, for a user near midnight, isoDate
  // could then read the previous local day — a 6-day window.
  const weekOut = new Date(today);
  weekOut.setDate(weekOut.getDate() + 6);
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
      <p className="text-fg-muted">
        Finite-capacity utilisation from released / in-progress work orders.
        Overloaded cells are flagged; the planner does not auto-reschedule.
      </p>

      <div className="mb-4 flex items-end gap-4">
        <label className="flex flex-col gap-1">
          Start
          <Input
            type="date"
            value={start}
            onChange={(e) => setStart(e.target.value)}
            className="w-auto"
          />
        </label>
        <label className="flex flex-col gap-1">
          End
          <Input
            type="date"
            value={end}
            onChange={(e) => setEnd(e.target.value)}
            className="w-auto"
          />
        </label>
      </div>

      {planQ.isLoading && <p>Loading…</p>}
      {planQ.isError && <p className="text-danger">{String(planQ.error)}</p>}
      {planQ.data && planQ.data.rows.length === 0 && (
        <p>No work centers defined yet.</p>
      )}
      {planQ.data && planQ.data.rows.length > 0 && (
        <div className="overflow-x-auto">
          <Table className="min-w-full">
            <TableHeader>
              <TableRow>
                <TableHead>Work center</TableHead>
                {dayHeaders.map((d) => (
                  <TableHead key={d} className="whitespace-nowrap text-center">
                    {d.slice(5)}
                  </TableHead>
                ))}
              </TableRow>
            </TableHeader>
            <TableBody>
              {planQ.data.rows.map((row: WorkCenterSchedule) => (
                <TableRow key={row.work_center_id}>
                  <TableCell className="whitespace-nowrap">
                    {row.work_center_name}
                    {row.status !== "active" && (
                      <span className="text-warning"> ({row.status})</span>
                    )}
                  </TableCell>
                  {row.days.map((day: CapacityDayLoad) => (
                    <TableCell
                      key={day.date}
                      title={`${day.scheduled_minutes} / ${day.available_minutes} min`}
                      className={`text-center ${
                        day.overloaded
                          ? "bg-danger/15 font-semibold text-danger"
                          : Number(day.utilization_percent) > 0
                            ? "bg-success/15"
                            : ""
                      }`}
                    >
                      {day.utilization_percent}%
                    </TableCell>
                  ))}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </section>
  );
}
